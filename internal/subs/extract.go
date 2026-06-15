package subs

import (
	"context"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
)

// ExtractText extracts an embedded text subtitle track (by subtitle-stream index,
// i.e. the s:N selector) to a WebVTT file in destDir, returning its path. webOS
// renders WebVTT soft subs delivered via the DLNA caption mechanism.
func ExtractText(ctx context.Context, input string, subIndex int, destDir string) (string, error) {
	out := filepath.Join(destDir, fmt.Sprintf("subs.%d.vtt", subIndex))
	cmd := exec.CommandContext(ctx, "ffmpeg",
		"-y", "-i", input,
		"-map", fmt.Sprintf("0:s:%d", subIndex),
		"-c:s", "webvtt",
		"-f", "webvtt", out,
	)
	if b, err := cmd.CombinedOutput(); err != nil {
		return "", fmt.Errorf("extract subtitle track %d: %w: %s", subIndex, err, lastLine(b))
	}
	if fi, err := os.Stat(out); err != nil || fi.Size() == 0 {
		return "", fmt.Errorf("extracted subtitle track %d is empty", subIndex)
	}
	return out, nil
}

func lastLine(b []byte) string {
	s := string(b)
	for len(s) > 0 && (s[len(s)-1] == '\n' || s[len(s)-1] == '\r') {
		s = s[:len(s)-1]
	}
	if i := lastIndexByte(s, '\n'); i >= 0 {
		return s[i+1:]
	}
	return s
}

func lastIndexByte(s string, b byte) int {
	for i := len(s) - 1; i >= 0; i-- {
		if s[i] == b {
			return i
		}
	}
	return -1
}
