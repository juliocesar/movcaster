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

	// AudioChannels is the re-encoded audio's channel count (with Audio): the
	// source's, capped by the profile. 0 when unknown, which means stereo.
	AudioChannels int
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
	codec, channels := chosenAudio(mi, audioTrack)
	if codec != "" && p.AudioCodecs != nil && !p.AudioCodecs[codec] {
		plan.Audio = true
	}
	if p.MaxAudioChannels > 0 && channels > p.MaxAudioChannels {
		plan.Audio = true
	}
	if mi.Container != "" && p.Containers != nil && !p.Containers[mi.Container] {
		plan.Remux = true
	}
	if plan.Audio {
		plan.AudioChannels = reencodeChannels(channels, p)
	}
	return plan
}

// ForceAll is Needs with every stream re-encoded (--transcode).
func ForceAll(mi *probe.MediaInfo, p engine.Profile, audioTrack int) Plan {
	plan := Needs(mi, p, audioTrack)
	plan.Video, plan.Audio = true, true
	var channels int
	if mi != nil {
		_, channels = chosenAudio(mi, audioTrack)
	}
	plan.AudioChannels = reencodeChannels(channels, p)
	return plan
}

// chosenAudio is the codec and channel count of a:audioTrack, falling back to
// the first audio stream's for a file probed without per-track detail.
func chosenAudio(mi *probe.MediaInfo, audioTrack int) (codec string, channels int) {
	if audioTrack >= 0 && audioTrack < len(mi.Audio) {
		return mi.Audio[audioTrack].Codec, mi.Audio[audioTrack].Channels
	}
	return mi.AudioCodec, mi.AudioChannels
}

// reencodeChannels is how many channels a re-encoded track keeps: the source's,
// capped by the profile's TranscodeAudioChannels (0 = stereo) and its
// MaxAudioChannels limit. An unknown source count yields stereo.
func reencodeChannels(src int, p engine.Profile) int {
	limit := p.TranscodeAudioChannels
	if limit <= 0 {
		limit = 2
	}
	if p.MaxAudioChannels > 0 && p.MaxAudioChannels < limit {
		limit = p.MaxAudioChannels
	}
	if src <= 0 {
		src = 2 // unknown: never guess surround
	}
	return min(src, limit)
}

// audioArgs is the audio re-encode: the profile's codec at the plan's channel
// count, with a bitrate that scales with it (192k is fine for stereo, thin for
// 5.1).
func audioArgs(p engine.Profile, plan Plan) []string {
	ch := plan.AudioChannels
	if ch <= 0 {
		ch = 2
	}
	bitrate := "192k"
	if ch > 2 {
		bitrate = fmt.Sprintf("%dk", 64*ch)
	}
	return []string{"-c:a", audioCodec(p), "-b:a", bitrate, "-ac", fmt.Sprint(ch)}
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
		args = append(args, audioArgs(p, plan)...)
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
