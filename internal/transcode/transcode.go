// Package transcode builds ffmpeg argument lists for codec-compatibility
// transcoding (no subtitle work) and classifies whether a file's codecs are
// playable by the target renderer.
package transcode

import (
	"fmt"
	"time"

	"github.com/juliocesar/movcaster/internal/probe"
)

// webOS-friendly codecs observed to direct-play on the target LG TV. Anything
// outside these is transcoded.
var goodVideo = map[string]bool{
	"h264": true, "hevc": true, "mpeg4": true, "mpeg2video": true, "vc1": true, "msmpeg4v3": true,
}
var goodAudio = map[string]bool{
	"aac": true, "ac3": true, "eac3": true, "mp3": true, "mp2": true, "dts": true, "flac": true,
}

// Needs reports whether the video and/or audio streams need transcoding.
func Needs(mi *probe.MediaInfo) (video, audio bool) {
	if mi == nil {
		return false, false
	}
	if mi.VideoCodec != "" && !goodVideo[mi.VideoCodec] {
		video = true
	}
	if mi.AudioCodec != "" && !goodAudio[mi.AudioCodec] {
		audio = true
	}
	return video, audio
}

// Args builds ffmpeg args that transcode (or copy) into a fragmented MP4 on
// stdout. ss is the absolute input-seek offset for seek-restart.
func Args(input string, ss time.Duration, transcodeVideo, transcodeAudio bool) []string {
	args := []string{"-hide_banner", "-loglevel", "error", "-nostdin"}
	if ss > 0 {
		args = append(args, "-ss", fmt.Sprintf("%.3f", ss.Seconds()))
	}
	args = append(args, "-i", input, "-map", "0:v:0", "-map", "0:a:0?", "-dn", "-map_chapters", "-1")

	if transcodeVideo {
		args = append(args, "-c:v", "libx264", "-preset", "veryfast", "-crf", "22", "-pix_fmt", "yuv420p")
	} else {
		args = append(args, "-c:v", "copy")
	}
	if transcodeAudio {
		args = append(args, "-c:a", "aac", "-b:a", "192k", "-ac", "2")
	} else {
		args = append(args, "-c:a", "copy")
	}

	args = append(args, "-movflags", "+frag_keyframe+empty_moov+default_base_moof", "-f", "mp4", "pipe:1")
	return args
}
