package subs

import (
	"context"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"time"

	"github.com/juliocesar/movcaster/internal/probe"
)

// BurnArgs builds the ffmpeg argument list that burns a subtitle track into the
// video and streams a fragmented MP4 on stdout (pipe:1). ss is the input-seek
// offset (0 to start from the beginning); the seek-restart logic relaunches with
// a new ss on scrub. Bitmap tracks use the overlay filter; text tracks use the
// subtitles filter.
func BurnArgs(input string, track probe.SubTrack, ss time.Duration) []string {
	args := []string{"-hide_banner", "-loglevel", "error", "-nostdin"}
	if ss > 0 {
		args = append(args, "-ss", fmt.Sprintf("%.3f", ss.Seconds()))
	}
	args = append(args, "-i", input)

	if track.Kind == probe.SubBitmap {
		// Overlay the decoded bitmap subtitle onto the video.
		fc := fmt.Sprintf("[0:v][0:s:%d]overlay[vout]", track.SubIndex)
		args = append(args, "-filter_complex", fc, "-map", "[vout]")
	} else {
		// Render text subtitles via libass. The filename must be escaped for the
		// filter-graph parser.
		vf := fmt.Sprintf("subtitles='%s':si=%d", escapeFilterPath(input), track.SubIndex)
		args = append(args, "-vf", vf, "-map", "0:v:0")
	}
	args = append(args, "-map", "0:a:0?", "-dn", "-map_chapters", "-1")

	args = append(args,
		"-c:v", "libx264", "-preset", "veryfast", "-crf", "22", "-pix_fmt", "yuv420p",
		"-c:a", "aac", "-b:a", "192k", "-ac", "2",
		"-movflags", "+frag_keyframe+empty_moov+default_base_moof",
		"-f", "mp4", "pipe:1",
	)
	return args
}

// escapeFilterPath escapes a path for use inside an ffmpeg filtergraph option.
func escapeFilterPath(p string) string {
	p = strings.ReplaceAll(p, `\`, `\\`)
	p = strings.ReplaceAll(p, `'`, `\'`)
	p = strings.ReplaceAll(p, `:`, `\:`)
	return p
}

// MuxSoftRemux copies the video, first audio, and the chosen bitmap subtitle into
// a fresh Matroska file in destDir, then returns its path. This is the M6a
// experiment: whether webOS will demux a freshly muxed image-sub track over DLNA.
func MuxSoftRemux(ctx context.Context, input string, track probe.SubTrack, destDir string) (string, error) {
	out := filepath.Join(destDir, "muxed.mkv")
	cmd := exec.CommandContext(ctx, "ffmpeg",
		"-y", "-i", input,
		"-map", "0:v:0", "-map", "0:a:0?", "-map", fmt.Sprintf("0:s:%d", track.SubIndex),
		"-c", "copy",
		"-disposition:s:0", "default",
		out,
	)
	if b, err := cmd.CombinedOutput(); err != nil {
		return "", fmt.Errorf("remux subtitle track %d: %w: %s", track.SubIndex, err, lastLine(b))
	}
	if fi, err := os.Stat(out); err != nil || fi.Size() == 0 {
		return "", fmt.Errorf("remuxed file is empty")
	}
	return out, nil
}
