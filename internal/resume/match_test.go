package resume

import (
	"reflect"
	"testing"
)

const hannibal = "/tv/Hannibal (2013) - S02E10 - Naka-Choko (1080p BluRay x265 RCVR).mkv"

func TestRankMatches(t *testing.T) {
	paths := []string{
		hannibal,
		"/movies/Breaking Bad - S01E01.mkv",
		"/movies/The Silence of the Lambs (1991).mkv",
	}
	cases := []struct {
		pattern string
		want    string // expected first result, or "" for no match
	}{
		{"hannibal", hannibal},             // substring
		{"Hannibal", hannibal},             // case-insensitive
		{"naka-choko", hannibal},           // separators ignored
		{"naka choko", hannibal},           // words with a space
		{"choko naka", hannibal},           // words out of order
		{"hanibal", hannibal},              // one-char typo
		{"s02e10", hannibal},               // token substring
		{"breaking bad", paths[1]},         // different entry
		{"lambs", paths[2]},                // word inside a title
		{"completely unrelated query", ""}, // no reasonable match
	}
	for _, c := range cases {
		got := Rank(c.pattern, paths)
		if c.want == "" {
			if len(got) != 0 {
				t.Errorf("Rank(%q) = %v, want no match", c.pattern, got)
			}
			continue
		}
		if len(got) == 0 || got[0] != c.want {
			t.Errorf("Rank(%q) top = %v, want %q first", c.pattern, got, c.want)
		}
	}
}

func TestRankTieKeepsInputOrder(t *testing.T) {
	// Two equally-good matches: Rank must keep the input order (newest-first
	// when fed Recent()), so the newer entry wins.
	newer := "/tv/Hannibal - S02E11.mkv"
	older := "/tv/Hannibal - S02E10.mkv"
	got := Rank("hannibal", []string{newer, older})
	want := []string{newer, older}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("Rank tie = %v, want %v (input order preserved)", got, want)
	}
}

func TestRankEmptyPattern(t *testing.T) {
	if got := Rank("", []string{hannibal}); len(got) != 0 {
		t.Fatalf("Rank(\"\") = %v, want no match", got)
	}
}

func TestNormalize(t *testing.T) {
	got := normalize("Hannibal (2013) - S02E10 - Naka-Choko.mkv")
	want := "hannibal 2013 s02e10 naka choko mkv"
	if got != want {
		t.Fatalf("normalize = %q, want %q", got, want)
	}
}
