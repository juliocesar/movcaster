package core

import (
	"testing"

	"github.com/juliocesar/movcaster/internal/config"
	"github.com/juliocesar/movcaster/internal/engine"
)

// A config override for the device's engine:host key replaces the axes it sets
// and leaves the rest of the engine default alone.
func TestDeviceProfileMergesOverride(t *testing.T) {
	dev := device("TV", "10.0.0.20:8009")
	dev.eng = "googlecast"
	dev.prof = engine.Profile{
		Name:        "Default Media Receiver",
		Containers:  codecSet("mp4", "webm"),
		VideoCodecs: codecSet("h264"),
		AudioCodecs: codecSet("aac"),
		SubFormats:  []engine.SubFormat{engine.SubFormatVTT},
	}
	a := New(Options{Store: &fakeStore{cfg: config.Config{
		DeviceProfiles: map[string]config.ProfileOverride{
			"googlecast:10.0.0.20": {
				VideoCodecs:    []string{"h264", "hevc", "vp9"},
				MaxVideoHeight: 1080,
			},
		},
	}}})

	got := a.DeviceProfile(dev)
	if !got.VideoCodecs["hevc"] || !got.VideoCodecs["vp9"] {
		t.Fatalf("override video codecs not applied: %v", got.VideoCodecs)
	}
	if got.MaxVideoHeight != 1080 {
		t.Fatalf("MaxVideoHeight = %d", got.MaxVideoHeight)
	}
	// Untouched axes keep the engine default.
	if !got.Containers["mp4"] || got.Containers["matroska"] {
		t.Fatalf("containers changed without an override: %v", got.Containers)
	}
	if got.SoftSubFormat() != engine.SubFormatVTT {
		t.Fatal("sub formats must stay the engine's")
	}

	// A different device is untouched.
	other := device("Other", "10.0.0.9:8009")
	other.prof = dev.prof
	if p := a.DeviceProfile(other); p.MaxVideoHeight != 0 {
		t.Fatalf("override leaked to another device: %+v", p)
	}
}

// deriveOverride marks an axis supported when any tuple carrying it played, and
// sets the 4K/5.1 limits only when the limit probe failed while the baseline
// passed.
func TestDeriveOverride(t *testing.T) {
	outcomes := make([]ProbeOutcome, len(probeMatrix))
	for i, clip := range probeMatrix {
		out := ProbeOutcome{Clip: clip.Container}
		switch {
		case clip.Container == "matroska":
			out.Played = false // Cast-style device: no matroska at all
		case clip.Video == "av1":
			out.Skipped = "no encoder" // says nothing
		case clip.size == "3840x2160":
			out.Played = false // 1080p panel
		case clip.channels == 6:
			out.Played = true
		default:
			out.Played = true
		}
		outcomes[i] = out
	}
	o := deriveOverride(outcomes)
	if contains(o.Containers, "matroska") {
		t.Fatalf("matroska failed every tuple but is listed: %v", o.Containers)
	}
	if !contains(o.Containers, "mp4") || !contains(o.Containers, "webm") {
		t.Fatalf("containers = %v", o.Containers)
	}
	if !contains(o.VideoCodecs, "h264") || !contains(o.VideoCodecs, "hevc") || !contains(o.VideoCodecs, "vp9") {
		t.Fatalf("video codecs = %v", o.VideoCodecs)
	}
	if contains(o.VideoCodecs, "av1") {
		t.Fatalf("a skipped clip must not mark its codec supported: %v", o.VideoCodecs)
	}
	// dts/flac/opus(mkv)/mp3 rode only matroska tuples, which all failed.
	if contains(o.AudioCodecs, "dts") || contains(o.AudioCodecs, "flac") {
		t.Fatalf("audio codecs = %v", o.AudioCodecs)
	}
	if o.MaxVideoHeight != 1080 {
		t.Fatalf("failed 4K probe must cap height: %d", o.MaxVideoHeight)
	}
	if o.MaxAudioChannels != 0 {
		t.Fatalf("passed 5.1 probe must not cap channels: %d", o.MaxAudioChannels)
	}
}

func contains(list []string, s string) bool {
	for _, v := range list {
		if v == s {
			return true
		}
	}
	return false
}
