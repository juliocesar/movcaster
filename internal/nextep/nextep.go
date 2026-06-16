// Package nextep finds the next episode to play after a given video file.
//
// It parses season/episode numbers out of filenames (via go-parse-torrent-name)
// and, within the same directory, picks the file that is the next episode of the
// same show: the smallest (season, episode) strictly greater than the current
// one. A normalized-title guard keeps it from jumping to an unrelated movie, so
// auto-advance is safe to leave on by default.
package nextep

import (
	"os"
	"path/filepath"
	"strings"

	ptn "github.com/razsteinmetz/go-ptn"
)

// videoExts mirrors the set core.mimeForExt understands. We keep our own copy
// (rather than calling mimeForExt) because that helper defaults unknown
// extensions to video/mp4; here we need a strict membership test.
var videoExts = map[string]bool{
	".mp4": true, ".m4v": true, ".mkv": true, ".avi": true,
	".mov": true, ".webm": true, ".ts": true, ".wmv": true,
}

// epKey identifies a parsed episode's position in a show.
type epKey struct {
	season  int
	episode int
}

// less orders by season then episode.
func (k epKey) less(o epKey) bool {
	if k.season != o.season {
		return k.season < o.season
	}
	return k.episode < o.episode
}

// Find returns the absolute path of the next episode after currentPath, searching
// only the same directory. ok is false when there is nothing sensible to play
// next: the current file has no detectable episode (e.g. a standalone movie), or
// no same-show file with a higher (season, episode) exists. Errors are reserved
// for failing to read the directory.
func Find(currentPath string) (next string, ok bool, err error) {
	abs, err := filepath.Abs(currentPath)
	if err != nil {
		return "", false, err
	}
	dir := filepath.Dir(abs)

	curTitle, curKey, okParse := parseEpisode(filepath.Base(abs))
	if !okParse {
		return "", false, nil // can't sequence a file with no episode number
	}

	entries, err := os.ReadDir(dir)
	if err != nil {
		return "", false, err
	}

	var (
		bestPath string
		bestKey  epKey
		found    bool
	)
	for _, e := range entries {
		if e.IsDir() || !isVideoFile(e.Name()) {
			continue
		}
		cand := filepath.Join(dir, e.Name())
		if cand == abs {
			continue
		}
		title, key, ok := parseEpisode(e.Name())
		if !ok || title != curTitle { // guard: same show only
			continue
		}
		if !curKey.less(key) { // must be a later episode
			continue
		}
		if !found || key.less(bestKey) {
			bestPath, bestKey, found = cand, key, true
		}
	}
	if !found {
		return "", false, nil
	}
	return bestPath, true, nil
}

// parseEpisode extracts a normalized show title and the episode key from a
// filename. ok is false when no episode number is present.
func parseEpisode(name string) (title string, key epKey, ok bool) {
	info, err := ptn.Parse(name)
	if err != nil || info.Episode == 0 {
		return "", epKey{}, false
	}
	return norm(info.Title), epKey{season: info.Season, episode: info.Episode}, true
}

// isVideoFile reports whether name has a known video extension.
func isVideoFile(name string) bool {
	return videoExts[strings.ToLower(filepath.Ext(name))]
}

// norm lowercases a title and strips everything but letters and digits, so minor
// spacing/punctuation differences between sibling filenames still match.
func norm(s string) string {
	var b strings.Builder
	for _, r := range strings.ToLower(s) {
		if (r >= 'a' && r <= 'z') || (r >= '0' && r <= '9') {
			b.WriteRune(r)
		}
	}
	return b.String()
}
