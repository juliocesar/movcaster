package resume

import (
	"path/filepath"
	"sort"
	"strings"
)

// matchThreshold is the minimum score a candidate must reach to be considered a
// match. It is high enough that a completely unrelated pattern yields nothing
// (so --resume-last errors rather than resuming a random video) but low enough
// to tolerate a typo or a partial word.
const matchThreshold = 0.6

// Rank orders paths by how well their base names match pattern, best first,
// dropping anything below matchThreshold. The match is fuzzy (not a regex):
// an exact name, a substring ("hannibal" → "Hannibal (2013) ...mkv"), the words
// in any order, and small typos all score. Ties keep the input order, so when
// paths is Recent() (newest-first) the most recently played of equally good
// matches wins.
func Rank(pattern string, paths []string) []string {
	type scored struct {
		path  string
		score float64
	}
	ranked := make([]scored, 0, len(paths))
	for _, p := range paths {
		if s := matchScore(pattern, filepath.Base(p)); s >= matchThreshold {
			ranked = append(ranked, scored{p, s})
		}
	}
	sort.SliceStable(ranked, func(i, j int) bool {
		return ranked[i].score > ranked[j].score
	})
	out := make([]string, len(ranked))
	for i, r := range ranked {
		out[i] = r.path
	}
	return out
}

// matchScore rates how well pattern matches name in [0,1]. Both are normalized
// to lowercase alphanumeric words so separators (dots, dashes, parens) don't
// matter. Scoring, high to low: exact match, whole-pattern substring, then the
// mean over each pattern word of its best per-word similarity (substring or
// edit-distance ratio) against the name's words.
func matchScore(pattern, name string) float64 {
	q := normalize(pattern)
	n := normalize(name)
	if q == "" || n == "" {
		return 0
	}
	if q == n {
		return 1.0
	}
	if strings.Contains(n, q) {
		return 0.95
	}
	qToks := strings.Fields(q)
	nToks := strings.Fields(n)
	if len(qToks) == 0 || len(nToks) == 0 {
		return 0
	}
	total := 0.0
	for _, qt := range qToks {
		best := 0.0
		for _, nt := range nToks {
			if s := tokenSim(qt, nt); s > best {
				best = s
			}
		}
		total += best
	}
	return total / float64(len(qToks))
}

// tokenSim scores two words in [0,1]: 1 for equal, 0.9 when the pattern word is
// contained in the name word (prefix/partial), otherwise an edit-distance ratio
// that stays high for a one- or two-character typo.
func tokenSim(a, b string) float64 {
	if a == b {
		return 1.0
	}
	if strings.Contains(b, a) {
		return 0.9
	}
	longest := len(a)
	if len(b) > longest {
		longest = len(b)
	}
	if longest == 0 {
		return 0
	}
	return 1 - float64(levenshtein(a, b))/float64(longest)
}

// normalize lowercases s and collapses every run of non-alphanumeric characters
// into a single space, so "Naka-Choko.mkv" and "naka choko" compare equal.
func normalize(s string) string {
	var b strings.Builder
	prevSpace := true // trims leading separators
	for _, r := range strings.ToLower(s) {
		if (r >= 'a' && r <= 'z') || (r >= '0' && r <= '9') {
			b.WriteRune(r)
			prevSpace = false
		} else if !prevSpace {
			b.WriteByte(' ')
			prevSpace = true
		}
	}
	return strings.TrimRight(b.String(), " ")
}

// levenshtein returns the edit distance between a and b (single-row DP).
func levenshtein(a, b string) int {
	ra, rb := []rune(a), []rune(b)
	prev := make([]int, len(rb)+1)
	for j := range prev {
		prev[j] = j
	}
	for i := 1; i <= len(ra); i++ {
		cur := make([]int, len(rb)+1)
		cur[0] = i
		for j := 1; j <= len(rb); j++ {
			cost := 1
			if ra[i-1] == rb[j-1] {
				cost = 0
			}
			cur[j] = min3(prev[j]+1, cur[j-1]+1, prev[j-1]+cost)
		}
		prev = cur
	}
	return prev[len(rb)]
}

func min3(a, b, c int) int {
	if b < a {
		a = b
	}
	if c < a {
		a = c
	}
	return a
}
