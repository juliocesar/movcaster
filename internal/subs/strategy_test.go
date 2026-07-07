package subs

import (
	"testing"

	"github.com/juliocesar/movcaster/internal/probe"
)

func info(tracks ...probe.SubTrack) *probe.MediaInfo {
	return &probe.MediaInfo{Subtitles: tracks}
}

func text(i int) probe.SubTrack {
	return probe.SubTrack{SubIndex: i, Codec: "subrip", Kind: probe.SubText}
}
func bitmap(i int) probe.SubTrack {
	return probe.SubTrack{SubIndex: i, Codec: "dvd_subtitle", Kind: probe.SubBitmap}
}
func forcedText(i int) probe.SubTrack {
	t := text(i)
	t.Forced = true
	return t
}

// TestSelectTrackSkipsForced mirrors the common English release: a forced track
// (foreign-dialogue only) sits first and is even flagged default, but auto should
// land on a full non-forced track so subtitles actually show.
func TestSelectTrackSkipsForced(t *testing.T) {
	forced := forcedText(0)
	forced.Default = true
	full := text(1)
	sdh := text(2)

	got, err := selectTrack([]probe.SubTrack{forced, full, sdh}, Request{TrackIndex: -1})
	if err != nil {
		t.Fatalf("selectTrack error: %v", err)
	}
	if got.SubIndex != 1 {
		t.Fatalf("auto picked s:%d, want the non-forced full track s:1", got.SubIndex)
	}

	// An explicit --sub-track still honors a forced choice.
	got, err = selectTrack([]probe.SubTrack{forced, full}, Request{TrackIndex: 0})
	if err != nil || got.SubIndex != 0 {
		t.Fatalf("explicit forced track: got s:%v err=%v, want s:0", got, err)
	}

	// If the only text track is forced, fall back to it rather than nothing.
	got, err = selectTrack([]probe.SubTrack{forcedText(0)}, Request{TrackIndex: -1})
	if err != nil || got.SubIndex != 0 {
		t.Fatalf("sole forced text track: got %v err=%v, want s:0", got, err)
	}
}

func TestDecide(t *testing.T) {
	tests := []struct {
		name string
		req  Request
		want Kind
	}{
		{"no-subs", Request{NoSubs: true, Info: info(text(0)), TrackIndex: -1}, None},
		{"sidecar wins", Request{SidecarPath: "/x.srt", Info: info(bitmap(0)), TrackIndex: -1}, SoftSidecar},
		{"no tracks", Request{Info: info(), TrackIndex: -1}, None},
		{"embedded text -> extract", Request{Info: info(text(0)), TrackIndex: -1}, SoftExtract},
		{"bitmap -> burn by default", Request{Info: info(bitmap(0)), TrackIndex: -1}, Burn},
		{"bitmap + mux-soft", Request{Info: info(bitmap(0)), MuxSoftTry: true, TrackIndex: -1}, MuxSoft},
		{"prefer text over bitmap in auto", Request{Info: info(bitmap(0), text(1)), TrackIndex: -1}, SoftExtract},
		{"force burn on text", Request{Info: info(text(0)), ForceBurn: true, TrackIndex: -1}, Burn},
		{"explicit bitmap track", Request{Info: info(text(0), bitmap(1)), TrackIndex: 1}, Burn},
		{"nil info", Request{Info: nil, TrackIndex: -1}, None},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			got, err := Decide(tc.req)
			if err != nil {
				t.Fatalf("Decide error: %v", err)
			}
			if got.Kind != tc.want {
				t.Errorf("Decide() = %v, want %v (reason: %s)", got.Kind, tc.want, got.Reason)
			}
		})
	}
}

func TestDecideForceSoftOnBitmapErrors(t *testing.T) {
	_, err := Decide(Request{Info: info(bitmap(0)), ForceSoft: true, TrackIndex: -1})
	if err == nil {
		t.Fatal("expected error forcing soft on a bitmap track")
	}
}

func TestDecideMissingExplicitTrack(t *testing.T) {
	_, err := Decide(Request{Info: info(text(0)), TrackIndex: 9})
	if err == nil {
		t.Fatal("expected error for out-of-range --sub-track")
	}
}
