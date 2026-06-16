package nextep

import (
	"os"
	"path/filepath"
	"testing"
)

// makeDir creates a temp dir containing empty files with the given names and
// returns the dir path.
func makeDir(t *testing.T, names ...string) string {
	t.Helper()
	dir := t.TempDir()
	for _, n := range names {
		if err := os.WriteFile(filepath.Join(dir, n), nil, 0o644); err != nil {
			t.Fatalf("write %s: %v", n, err)
		}
	}
	return dir
}

func TestFind(t *testing.T) {
	cases := []struct {
		name    string
		files   []string
		current string
		want    string // basename, "" => expect ok=false
	}{
		{
			name:    "next episode same season",
			files:   []string{"Hannibal S01E01.mkv", "Hannibal S01E02.mkv", "Hannibal S01E03.mkv"},
			current: "Hannibal S01E01.mkv",
			want:    "Hannibal S01E02.mkv",
		},
		{
			name:    "season rollover when no higher episode in season",
			files:   []string{"Show S01E10.mkv", "Show S02E01.mkv"},
			current: "Show S01E10.mkv",
			want:    "Show S02E01.mkv",
		},
		{
			name:    "gap skips to next available episode",
			files:   []string{"Show S01E01.mkv", "Show S01E03.mkv"},
			current: "Show S01E01.mkv",
			want:    "Show S01E03.mkv",
		},
		{
			name:    "different show is not picked (guard)",
			files:   []string{"Hannibal S01E01.mkv", "Dexter S01E02.mkv"},
			current: "Hannibal S01E01.mkv",
			want:    "",
		},
		{
			name:    "inexact spacing and case still match",
			files:   []string{"the.wire.s01e01.mkv", "The Wire S01E02.mkv"},
			current: "the.wire.s01e01.mkv",
			want:    "The Wire S01E02.mkv",
		},
		{
			name: "mixed formats and episode-title suffixes",
			files: []string{
				"The.Wire.S01E02.720p.BluRay.x264-GROUP.mkv",
				"The Wire - 1x03 - The Buys.mkv",
			},
			current: "The.Wire.S01E02.720p.BluRay.x264-GROUP.mkv",
			want:    "The Wire - 1x03 - The Buys.mkv",
		},
		{
			name:    "standalone movie has no episode",
			files:   []string{"Inception 2010 1080p BluRay.mkv", "Interstellar 2014 1080p.mkv"},
			current: "Inception 2010 1080p BluRay.mkv",
			want:    "",
		},
		{
			name:    "lone episode in dir",
			files:   []string{"Show S01E01.mkv"},
			current: "Show S01E01.mkv",
			want:    "",
		},
		{
			name:    "last episode has no successor",
			files:   []string{"Show S01E01.mkv", "Show S01E02.mkv"},
			current: "Show S01E02.mkv",
			want:    "",
		},
		{
			name:    "non-video siblings are ignored",
			files:   []string{"Show S01E01.mkv", "Show S01E02.srt", "Show S01E02.mkv"},
			current: "Show S01E01.mkv",
			want:    "Show S01E02.mkv",
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			dir := makeDir(t, tc.files...)
			got, ok, err := Find(filepath.Join(dir, tc.current))
			if err != nil {
				t.Fatalf("Find error: %v", err)
			}
			if tc.want == "" {
				if ok {
					t.Fatalf("want ok=false, got next=%q", got)
				}
				return
			}
			if !ok {
				t.Fatalf("want next=%q, got ok=false", tc.want)
			}
			if base := filepath.Base(got); base != tc.want {
				t.Fatalf("next = %q, want %q", base, tc.want)
			}
			if !filepath.IsAbs(got) {
				t.Fatalf("next path is not absolute: %q", got)
			}
		})
	}
}
