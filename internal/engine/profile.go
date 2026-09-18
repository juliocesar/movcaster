package engine

// Profile declares what a device can actually play. The hard part of supporting
// a new TV is rarely the protocol — it is this data. Engines ship a conservative
// default; --probe-device refines it per device into a config override.
type Profile struct {
	// Containers accepted for direct play, by normalized ffprobe format name
	// ("mov,mp4,m4a,3gp,3g2,mj2" normalizes to "mp4"). Empty = accept anything.
	Containers map[string]bool

	// VideoCodecs / AudioCodecs by ffprobe codec_name. Empty = accept anything.
	VideoCodecs map[string]bool
	AudioCodecs map[string]bool

	// MaxVideoHeight is 0 for no limit. Non-zero forces a downscale.
	MaxVideoHeight int

	// MaxAudioChannels is 0 for no limit. Non-zero forces a downmix.
	MaxAudioChannels int

	// SubFormats lists accepted soft-subtitle formats in PREFERENCE ORDER.
	// Empty means the device has no soft-subtitle path at all, so every
	// subtitle burns in.
	SubFormats []SubFormat

	// HonorsStartAt: the engine can begin playback at Media.StartAt.
	HonorsStartAt bool

	// LoadStartsPlayback: Load alone begins playback (webOS does, Cast does
	// when autoplay is set). Advisory: core still calls Play, which every
	// engine must make idempotent.
	LoadStartsPlayback bool

	// Transcode targets used when the profile is not satisfied.
	TranscodeVideo     string // "libx264"
	TranscodeAudio     string // "aac"
	TranscodeContainer string // "mp4"
}

// SoftSubFormat is the preferred soft-subtitle format, or SubFormatNone when the
// device has no soft-subtitle path.
func (p Profile) SoftSubFormat() SubFormat {
	if len(p.SubFormats) == 0 {
		return SubFormatNone
	}
	return p.SubFormats[0]
}
