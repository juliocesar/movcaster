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

// Needs reports whether the video and/or the chosen audio stream (a:audioTrack)
// need transcoding. A file probed without per-track detail falls back to the
// first audio stream's codec.
func Needs(mi *probe.MediaInfo, audioTrack int) (video, audio bool) {
	if mi == nil {
		return false, false
	}
	if mi.VideoCodec != "" && !goodVideo[mi.VideoCodec] {
		video = true
	}
	codec := mi.AudioCodec
	if audioTrack >= 0 && audioTrack < len(mi.Audio) {
		codec = mi.Audio[audioTrack].Codec
	}
	if codec != "" && !goodAudio[codec] {
		audio = true
	}
	return video, audio
}

// AudioMap is the -map selector for audio stream a:N. The trailing "?" keeps a
// file with no audio at all castable.
func AudioMap(audioTrack int) string { return fmt.Sprintf("0:a:%d?", audioTrack) }

// Args builds ffmpeg args that transcode (or copy) into a fragmented MP4 on
// stdout. ss is the absolute input-seek offset for seek-restart; audioTrack is
// the a:N stream to carry (the only audio in the output).
func Args(input string, ss time.Duration, transcodeVideo, transcodeAudio bool, audioTrack int) []string {
	args := []string{"-hide_banner", "-loglevel", "error", "-nostdin"}
	if ss > 0 {
		args = append(args, "-ss", fmt.Sprintf("%.3f", ss.Seconds()))
	}
	args = append(args, "-i", input, "-map", "0:v:0", "-map", AudioMap(audioTrack), "-dn", "-map_chapters", "-1")

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

	// delay_moov is required whenever audio is copied: the MP4 muxer cannot write
	// the moov atom for (E-)AC-3 until it has parsed a packet, so with a bare
	// empty_moov ffmpeg dies on "Cannot write moov atom before EAC3 packets
	// parsed" and the TV gets an empty stream ("file cannot be recognized").
	// delay_moov also starts emitting edit lists, which webOS mishandles (video
	// freezes on resume while audio keeps going), so suppress them — they are
	// no-ops here anyway: both streams already start at PTS 0, and the output's
	// start_time/start_pts are identical with and without them.
	args = append(args,
		"-movflags", "+frag_keyframe+empty_moov+default_base_moof+delay_moov",
		"-use_editlist", "0",
		"-f", "mp4", "pipe:1")
	return args
}
