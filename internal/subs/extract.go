package subs

import (
	"context"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"time"

	"github.com/juliocesar/movcaster/internal/engine"
)

// codecAndMuxer maps a soft-subtitle format to ffmpeg's codec and muxer names.
// The conversion (cue timestamp separators and the WEBVTT header included) is
// ffmpeg's job — never hand-rolled here.
func codecAndMuxer(format engine.SubFormat) (codec, muxer string) {
	if format == engine.SubFormatVTT {
		return "webvtt", "webvtt"
	}
	return "subrip", "srt"
}

// ExtractText extracts an embedded text subtitle track (by subtitle-stream index,
// i.e. the s:N selector) to a file in destDir, returning its path. The output
// format is the device profile's choice (webOS renders SRT but not WebVTT over
// DLNA; a Cast receiver takes WebVTT and rejects SRT). Non-matching text sources
// (ass/mov_text/...) are converted, dropping styling the TV wouldn't render anyway.
//
// ss must be the offset the *stream the TV plays* starts at: 0 for direct-play
// (which keeps the file's own absolute timeline), or the transcode's -ss for a
// transcoded stream, whose timestamps ffmpeg rebases to 0. Input-seeking the
// extraction rebases the cue times the same way, keeping the two in step.
func ExtractText(ctx context.Context, input string, subIndex int, ss time.Duration, destDir string, format engine.SubFormat) (string, error) {
	codec, muxer := codecAndMuxer(format)
	out := filepath.Join(destDir, fmt.Sprintf("subs.%d.%d%s", subIndex, int64(ss.Seconds()), format.Ext()))
	args := []string{"-y"}
	if ss > 0 {
		args = append(args, "-ss", fmt.Sprintf("%.3f", ss.Seconds()))
	}
	args = append(args,
		"-i", input,
		"-map", fmt.Sprintf("0:s:%d", subIndex),
		"-c:s", codec,
		"-f", muxer, out,
	)
	cmd := exec.CommandContext(ctx, "ffmpeg", args...)
	if b, err := cmd.CombinedOutput(); err != nil {
		return "", fmt.Errorf("extract subtitle track %d: %w: %s", subIndex, err, lastLine(b))
	}
	if noCues(out, format) {
		// Seeking past the last cue legitimately yields a cue-less file; only the
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
// ss and its format matches the profile's choice, returning the new path (the
// original is left untouched). Same reason as ExtractText's ss: a transcoded
// stream starts at 0, so an absolutely-timed sidecar would run ss too late.
// ss == 0 with a sidecar already in the right format returns the input unchanged.
func ShiftText(ctx context.Context, path string, ss time.Duration, destDir string, format engine.SubFormat) (string, error) {
	if ss <= 0 && filepath.Ext(path) == format.Ext() {
		return path, nil
	}
	codec, muxer := codecAndMuxer(format)
	out := filepath.Join(destDir, fmt.Sprintf("sidecar.%d%s", int64(ss.Seconds()), format.Ext()))
	args := []string{"-y"}
	if ss > 0 {
		args = append(args, "-ss", fmt.Sprintf("%.3f", ss.Seconds()))
	}
	args = append(args, "-i", path, "-c:s", codec, "-f", muxer, out)
	cmd := exec.CommandContext(ctx, "ffmpeg", args...)
	if b, err := cmd.CombinedOutput(); err != nil {
		return "", fmt.Errorf("shift subtitle %s: %w: %s", filepath.Base(path), err, lastLine(b))
	}
	if noCues(out, format) {
		if ss > 0 {
			return "", errNoCuesAfterOffset
		}
		return "", fmt.Errorf("subtitle %s converted empty", filepath.Base(path))
	}
	return out, nil
}

// noCues reports whether an emitted subtitle file carries no cues. An empty SRT
// is zero bytes, but ffmpeg's WebVTT muxer always writes the mandatory "WEBVTT"
// header, so a cue-less VTT is non-empty and must be sniffed for a cue arrow.
func noCues(path string, format engine.SubFormat) bool {
	fi, err := os.Stat(path)
	if err != nil || fi.Size() == 0 {
		return true
	}
	if format != engine.SubFormatVTT {
		return false
	}
	b, err := os.ReadFile(path)
	if err != nil {
		return true
	}
	return !containsCueArrow(b)
}

func containsCueArrow(b []byte) bool {
	for i := 0; i+3 <= len(b); i++ {
		if b[i] == '-' && b[i+1] == '-' && b[i+2] == '>' {
			return true
		}
	}
	return false
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
