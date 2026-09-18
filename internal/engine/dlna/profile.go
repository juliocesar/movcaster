package dlna

import "github.com/juliocesar/movcaster/internal/engine"

// webOSProfile is exactly the behavior the tool grew up with against the LG:
// no container restriction (webOS direct-plays MKV/AVI/TS happily), the codec
// sets observed to direct-play, and SRT-only soft subs (webOS honors
// sec:CaptionInfoEx for SRT but renders nothing for WebVTT over DLNA —
// verified against the TV).
var webOSProfile = engine.Profile{
	Name:        "webOS",
	Containers:  nil,
	VideoCodecs: set("h264", "hevc", "mpeg4", "mpeg2video", "vc1", "msmpeg4v3"),
	AudioCodecs: set("aac", "ac3", "eac3", "mp3", "mp2", "dts", "flac"),
	SubFormats:  []engine.SubFormat{engine.SubFormatSRT},
	// DLNA has no load-time start offset: resume seeks after Play.
	HonorsStartAt: false,
	// webOS auto-plays on SetAVTransportURI; Play is still issued (idempotent).
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
