package googlecast

import "github.com/juliocesar/movcaster/internal/engine"

// defaultReceiverProfile is deliberately CONSERVATIVE: only what every Default
// Media Receiver plays. Widen it per device from --probe-device results, never
// from a spec sheet — hevc/av1 and (E-)AC-3 passthrough are device-dependent,
// and guessing produces silent black screens.
//
// This profile changes the economics of a cast versus the LG: a typical release
// (MKV, h264, E-AC-3) needs a container remux plus an audio transcode, and even
// the best case (MKV, h264, AAC) needs the remux. The remux copies both
// streams, so it costs no encoding.
var defaultReceiverProfile = engine.Profile{
	Name:        "Default Media Receiver",
	Containers:  set("mp4", "webm"),                          // NOT matroska
	VideoCodecs: set("h264", "vp8", "vp9"),                   // hevc/av1 are device-dependent
	AudioCodecs: set("aac", "mp3", "opus", "vorbis", "flac"), // NOT ac3/eac3/dts
	SubFormats:  []engine.SubFormat{engine.SubFormatVTT},     // SRT is rejected
	// LOAD carries currentTime: a resume starts there in one shot.
	HonorsStartAt: true,
	// autoplay:true makes Load start playback; Play stays idempotent.
	LoadStartsPlayback: true,
	TranscodeVideo:     "libx264",
	TranscodeAudio:     "aac",
	TranscodeContainer: "mp4",
}

func set(names ...string) map[string]bool {
	m := make(map[string]bool, len(names))
	for _, n := range names {
		m[n] = true
	}
	return m
}
