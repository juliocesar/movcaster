// Package transcode builds ffmpeg argument lists for codec/container
// compatibility (no subtitle work) and classifies what must change for a
// device's Profile to accept a file.
package transcode

import (
	"fmt"
	"time"

	"github.com/juliocesar/movcaster/internal/engine"
	"github.com/juliocesar/movcaster/internal/probe"
)

// Plan reports what must change for a profile to accept a file.
type Plan struct {
	Video bool // re-encode the video stream
	Audio bool // re-encode the audio stream
	Remux bool // container mismatch only: streams can be copied
}

// Needed reports whether the file cannot be direct-played as is.
func (p Plan) Needed() bool { return p.Video || p.Audio || p.Remux }

// Needs reports what must change for p to accept mi when it carries the chosen
// audio stream (a:audioTrack). A nil map on the profile means "accept anything"
// for that axis. A file probed without per-track detail falls back to the first
// audio stream's codec and channel count.
func Needs(mi *probe.MediaInfo, p engine.Profile, audioTrack int) Plan {
	if mi == nil {
		return Plan{}
	}
	var plan Plan
	if mi.VideoCodec != "" && p.VideoCodecs != nil && !p.VideoCodecs[mi.VideoCodec] {
		plan.Video = true
	}
	if p.MaxVideoHeight > 0 && mi.Height > p.MaxVideoHeight {
		plan.Video = true
	}
	codec, channels := mi.AudioCodec, mi.AudioChannels
	if audioTrack >= 0 && audioTrack < len(mi.Audio) {
		codec, channels = mi.Audio[audioTrack].Codec, mi.Audio[audioTrack].Channels
	}
	if codec != "" && p.AudioCodecs != nil && !p.AudioCodecs[codec] {
		plan.Audio = true
	}
	if p.MaxAudioChannels > 0 && channels > p.MaxAudioChannels {
		plan.Audio = true
	}
	if mi.Container != "" && p.Containers != nil && !p.Containers[mi.Container] {
		plan.Remux = true
	}
	return plan
}

// AudioMap is the -map selector for audio stream a:N. The trailing "?" keeps a
// file with no audio at all castable.
func AudioMap(audioTrack int) string { return fmt.Sprintf("0:a:%d?", audioTrack) }

// Args builds ffmpeg args that transcode, remux, or copy into a fragmented MP4
// on stdout, per the plan and the profile's transcode targets. ss is the
// absolute input-seek offset for seek-restart; audioTrack is the a:N stream to
// carry (the only audio in the output). A remux-only plan copies both streams,
// so it costs no encoding.
func Args(input string, ss time.Duration, p engine.Profile, plan Plan, audioTrack int) []string {
	args := []string{"-hide_banner", "-loglevel", "error", "-nostdin"}
	if ss > 0 {
		args = append(args, "-ss", fmt.Sprintf("%.3f", ss.Seconds()))
	}
	args = append(args, "-i", input, "-map", "0:v:0", "-map", AudioMap(audioTrack), "-dn", "-map_chapters", "-1")

	if plan.Video {
		args = append(args, "-c:v", videoCodec(p), "-preset", "veryfast", "-crf", "22", "-pix_fmt", "yuv420p")
		if p.MaxVideoHeight > 0 {
			// Downscale to the profile's limit, never upscale (-2 keeps the width even).
			args = append(args, "-vf", fmt.Sprintf("scale=-2:'min(ih,%d)'", p.MaxVideoHeight))
		}
	} else {
		args = append(args, "-c:v", "copy")
	}
	if plan.Audio {
		args = append(args, "-c:a", audioCodec(p), "-b:a", "192k", "-ac", "2")
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
		"-f", container(p), "pipe:1")
	return args
}

// videoCodec / audioCodec / container read the profile's transcode targets,
// defaulting to the universal libx264/aac/mp4 when a profile leaves them unset.
func videoCodec(p engine.Profile) string {
	if p.TranscodeVideo != "" {
		return p.TranscodeVideo
	}
	return "libx264"
}

func audioCodec(p engine.Profile) string {
	if p.TranscodeAudio != "" {
		return p.TranscodeAudio
	}
	return "aac"
}

func container(p engine.Profile) string {
	if p.TranscodeContainer != "" {
		return p.TranscodeContainer
	}
	return "mp4"
}
