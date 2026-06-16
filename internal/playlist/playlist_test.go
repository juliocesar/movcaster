package playlist

import (
	"os"
	"path/filepath"
	"testing"
)

func writePlaylist(t *testing.T, body string) string {
	t.Helper()
	p := filepath.Join(t.TempDir(), "list.txt")
	if err := os.WriteFile(p, []byte(body), 0o644); err != nil {
		t.Fatal(err)
	}
	return p
}

func TestLoadSkipsBlanksAndComments(t *testing.T) {
	body := "#EXTM3U\n" +
		"/movies/Show S01E01.mkv\n" +
		"\n" +
		"  # a comment\n" +
		"#EXTINF:-1,Episode 2\n" +
		"/movies/Show S01E02.mkv\n" +
		"   \n"
	got, err := Load(writePlaylist(t, body))
	if err != nil {
		t.Fatal(err)
	}
	want := []string{"/movies/Show S01E01.mkv", "/movies/Show S01E02.mkv"}
	if len(got) != len(want) {
		t.Fatalf("got %v, want %v", got, want)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("entry %d = %q, want %q", i, got[i], want[i])
		}
	}
}

func TestLoadResolvesRelativeToCWD(t *testing.T) {
	got, err := Load(writePlaylist(t, "subdir/clip.mkv\n"))
	if err != nil {
		t.Fatal(err)
	}
	want, _ := filepath.Abs("subdir/clip.mkv") // relative to the test's CWD
	if len(got) != 1 || got[0] != want {
		t.Fatalf("got %v, want [%s]", got, want)
	}
	if !filepath.IsAbs(got[0]) {
		t.Fatalf("entry not absolute: %q", got[0])
	}
}

func TestLoadEmptyIsError(t *testing.T) {
	if _, err := Load(writePlaylist(t, "\n# only comments\n\n")); err == nil {
		t.Fatal("want error for empty playlist, got nil")
	}
}

func TestLoadMissingFileIsError(t *testing.T) {
	if _, err := Load(filepath.Join(t.TempDir(), "nope.txt")); err == nil {
		t.Fatal("want error for missing playlist file, got nil")
	}
}
