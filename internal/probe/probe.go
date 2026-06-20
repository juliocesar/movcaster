// Package probe wraps ffprobe to inspect a media file's streams and classify
// subtitle tracks as text or bitmap, which drives the subtitle strategy.
package probe

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"os/exec"
	"strconv"
	"strings"
	"time"
)

// SubKind classifies a subtitle stream.
type SubKind int

const (
	SubUnknown SubKind = iota
	SubText            // subrip/ass/mov_text/webvtt -> can be soft (extract to vtt)
	SubBitmap          // dvd_subtitle/pgs/vobsub/dvbsub -> must burn or mux
)

func (k SubKind) String() string {
	switch k {
	case SubText:
		return "text"
	case SubBitmap:
		return "bitmap"
	default:
		return "unknown"
	}
}

// SubTrack is one subtitle stream.
type SubTrack struct {
	Index    int // ffmpeg stream index (use with -map 0:<Index>)
	SubIndex int // index among subtitle streams only (use with -map 0:s:<SubIndex>)
	Codec    string
	Kind     SubKind
	Language string
	Title    string
	Default  bool
}

// MediaInfo summarizes a probed file.
type MediaInfo struct {
	Duration   time.Duration
	VideoCodec string
	AudioCodec string
	Subtitles  []SubTrack
}

type ffStream struct {
	Index       int               `json:"index"`
	CodecName   string            `json:"codec_name"`
	CodecType   string            `json:"codec_type"`
	Tags        map[string]string `json:"tags"`
	Disposition map[string]int    `json:"disposition"`
}

type ffOutput struct {
	Streams []ffStream `json:"streams"`
	Format  struct {
		Duration string `json:"duration"`
	} `json:"format"`
}

// Probe runs ffprobe on path and returns parsed media info.
func Probe(ctx context.Context, path string) (*MediaInfo, error) {
	// -v error (not quiet) so genuine ffprobe complaints land in stderr; we
	// capture that buffer ourselves and only surface it when the command fails.
	// This also catches the dynamic loader aborting a broken binary before it
	// runs (e.g. a Homebrew library left dangling by an upgrade), which would
	// otherwise reduce to an opaque "signal: abort trap".
	cmd := exec.CommandContext(ctx, "ffprobe",
		"-v", "error", "-print_format", "json",
		"-show_streams", "-show_format", path)
	var stderr bytes.Buffer
	cmd.Stderr = &stderr
	out, err := cmd.Output()
	if err != nil {
		if msg := firstLines(stderr.String(), 3); msg != "" {
			return nil, fmt.Errorf("ffprobe: %w: %s", err, msg)
		}
		return nil, fmt.Errorf("ffprobe: %w", err)
	}

	var ff ffOutput
	if err := json.Unmarshal(out, &ff); err != nil {
		return nil, fmt.Errorf("parse ffprobe output: %w", err)
	}

	mi := &MediaInfo{}
	if d, err := strconv.ParseFloat(ff.Format.Duration, 64); err == nil {
		mi.Duration = time.Duration(d * float64(time.Second))
	}

	subSeq := 0
	for _, s := range ff.Streams {
		switch s.CodecType {
		case "video":
			// Skip attached cover-art images (mjpeg/png attached_pic).
			if s.Disposition["attached_pic"] == 1 {
				continue
			}
			if mi.VideoCodec == "" {
				mi.VideoCodec = s.CodecName
			}
		case "audio":
			if mi.AudioCodec == "" {
				mi.AudioCodec = s.CodecName
			}
		case "subtitle":
			t := SubTrack{
				Index:    s.Index,
				SubIndex: subSeq,
				Codec:    s.CodecName,
				Kind:     classify(s.CodecName),
				Language: s.Tags["language"],
				Title:    s.Tags["title"],
				Default:  s.Disposition["default"] == 1,
			}
			mi.Subtitles = append(mi.Subtitles, t)
			subSeq++
		}
	}
	return mi, nil
}

func classify(codec string) SubKind {
	switch strings.ToLower(codec) {
	case "subrip", "srt", "ass", "ssa", "mov_text", "webvtt", "text", "subviewer", "subviewer1", "microdvd":
		return SubText
	case "dvd_subtitle", "dvdsub", "hdmv_pgs_subtitle", "pgssub", "dvb_subtitle", "dvbsub", "xsub", "vobsub":
		return SubBitmap
	default:
		return SubUnknown
	}
}

// HasVideo reports whether a usable video stream was found.
func (m *MediaInfo) HasVideo() bool { return m.VideoCodec != "" }

// firstLines returns at most n non-empty, trimmed lines of s joined with "; ",
// for a compact single-line error suffix.
func firstLines(s string, n int) string {
	var kept []string
	for _, l := range strings.Split(s, "\n") {
		if t := strings.TrimSpace(l); t != "" {
			kept = append(kept, t)
			if len(kept) == n {
				break
			}
		}
	}
	return strings.Join(kept, "; ")
}
