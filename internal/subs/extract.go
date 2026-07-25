package subs

import (
	"context"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"time"
)

// ExtractText extracts an embedded text subtitle track (by subtitle-stream index,
// i.e. the s:N selector) to a SubRip (.srt) file in destDir, returning its path.
// webOS renders soft subs delivered via the DLNA caption mechanism
// (sec:CaptionInfoEx); it honors SRT reliably but not WebVTT, so we serve SRT to
// match the (verified) sidecar path. Non-subrip text sources (ass/mov_text/webvtt)
// are converted to subrip, dropping styling the TV wouldn't render anyway.
//
// ss must be the offset the *stream the TV plays* starts at: 0 for direct-play
// (which keeps the file's own absolute timeline), or the transcode's -ss for a
// transcoded stream, whose timestamps ffmpeg rebases to 0. Input-seeking the
// extraction rebases the cue times the same way, keeping the two in step — see
// SoftOffsetFor.
func ExtractText(ctx context.Context, input string, subIndex int, ss time.Duration, destDir string) (string, error) {
	out := filepath.Join(destDir, fmt.Sprintf("subs.%d.%d.srt", subIndex, int64(ss.Seconds())))
	args := []string{"-y"}
	if ss > 0 {
		args = append(args, "-ss", fmt.Sprintf("%.3f", ss.Seconds()))
	}
	args = append(args,
		"-i", input,
		"-map", fmt.Sprintf("0:s:%d", subIndex),
		"-c:s", "subrip",
		"-f", "srt", out,
	)
	cmd := exec.CommandContext(ctx, "ffmpeg", args...)
	if b, err := cmd.CombinedOutput(); err != nil {
		return "", fmt.Errorf("extract subtitle track %d: %w: %s", subIndex, err, lastLine(b))
	}
	if fi, err := os.Stat(out); err != nil || fi.Size() == 0 {
		// Seeking past the last cue legitimately yields an empty file; only the
		// unseeked case means the track itself was unusable.
		if ss > 0 {
			return "", errNoCuesAfterOffset
		}
		return "", fmt.Errorf("extracted subtitle track %d is empty", subIndex)
	}
	return out, nil
}

// errNoCuesAfterOffset marks "the extraction succeeded but there are no cues left
// after ss" — e.g. resuming into the closing credits. Callers drop soft subs for
// that segment rather than failing the cast.
var errNoCuesAfterOffset = fmt.Errorf("no subtitle cues after the start offset")

// ShiftText rewrites an external subtitle file so its cue times are relative to
// ss, returning the new path (the original is left untouched). Same reason as
// ExtractText's ss: a transcoded stream starts at 0, so an absolutely-timed
// sidecar would run ss too late. ss == 0 returns the input unchanged.
func ShiftText(ctx context.Context, path string, ss time.Duration, destDir string) (string, error) {
	if ss <= 0 {
		return path, nil
	}
	out := filepath.Join(destDir, fmt.Sprintf("sidecar.%d.srt", int64(ss.Seconds())))
	cmd := exec.CommandContext(ctx, "ffmpeg",
		"-y", "-ss", fmt.Sprintf("%.3f", ss.Seconds()),
		"-i", path, "-c:s", "subrip", "-f", "srt", out,
	)
	if b, err := cmd.CombinedOutput(); err != nil {
		return "", fmt.Errorf("shift subtitle %s: %w: %s", filepath.Base(path), err, lastLine(b))
	}
	if fi, err := os.Stat(out); err != nil || fi.Size() == 0 {
		return "", errNoCuesAfterOffset
	}
	return out, nil
}

// NoCuesAfterOffset reports whether err means "nothing left to show from here",
// which callers treat as "cast without soft subs" rather than as a failure.
func NoCuesAfterOffset(err error) bool { return err == errNoCuesAfterOffset }

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
