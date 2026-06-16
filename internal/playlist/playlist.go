// Package playlist reads a playlist file: a plain text file with one video path
// per line. Blank lines and comment lines (starting with '#', which also covers
// m3u directives like #EXTM3U / #EXTINF) are ignored. Absolute paths are used
// as-is; relative paths resolve against the current working directory (where the
// app was run from), per the spec.
package playlist

import (
	"bufio"
	"fmt"
	"os"
	"path/filepath"
	"strings"
)

// Load parses the playlist at path and returns its entries as absolute paths, in
// order. It does not check that the referenced files exist (the caller does, so
// it can skip a missing entry rather than abort the whole list). An error is
// returned only when the playlist file itself can't be read or has no entries.
func Load(path string) ([]string, error) {
	f, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	defer f.Close()

	var out []string
	scanner := bufio.NewScanner(f)
	for scanner.Scan() {
		line := strings.TrimSpace(scanner.Text())
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		abs, err := filepath.Abs(line) // already-absolute paths pass through cleaned
		if err != nil {
			return nil, err
		}
		out = append(out, abs)
	}
	if err := scanner.Err(); err != nil {
		return nil, err
	}
	if len(out) == 0 {
		return nil, fmt.Errorf("playlist %q has no entries", path)
	}
	return out, nil
}
