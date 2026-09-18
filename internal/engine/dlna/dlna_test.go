package dlna

import (
	"strings"
	"testing"
	"time"

	"github.com/juliocesar/movcaster/internal/engine"
)

func TestFormatDuration(t *testing.T) {
	cases := map[time.Duration]string{
		0:                "0:00:00",
		90 * time.Second: "0:01:30",
		time.Hour + 2*time.Minute + 3*time.Second: "1:02:03",
		-time.Second: "0:00:00",
	}
	for in, want := range cases {
		if got := formatDuration(in); got != want {
			t.Errorf("formatDuration(%v) = %q, want %q", in, got, want)
		}
	}
}

func TestParseDuration(t *testing.T) {
	cases := map[string]time.Duration{
		"0:01:30":         90 * time.Second,
		"1:02:03":         time.Hour + 2*time.Minute + 3*time.Second,
		"00:00:10.500":    10 * time.Second,
		"NOT_IMPLEMENTED": 0,
		"":                0,
		"garbage":         0,
	}
	for in, want := range cases {
		if got := parseDuration(in); got != want {
			t.Errorf("parseDuration(%q) = %v, want %v", in, got, want)
		}
	}
}

func TestBuildDIDLWithCaption(t *testing.T) {
	m := engine.Media{
		URL: "http://h/media.mp4", Title: "Movie & Co", MIME: "video/mp4",
		Duration: 90 * time.Second, Seekable: true,
		Subtitle: &engine.Subtitle{
			URL: "http://h/subs.srt", Format: engine.SubFormatSRT, MIME: "text/srt",
		},
	}
	d := buildDIDL(m)
	for _, want := range []string{
		"sec:CaptionInfoEx sec:type=\"srt\"",
		"http://h/subs.srt",
		"DLNA.ORG_OP=01", // seekable
		"Movie &amp; Co", // xml-escaped title
		"object.item.videoItem",
	} {
		if !strings.Contains(d, want) {
			t.Errorf("buildDIDL missing %q\n%s", want, d)
		}
	}
}

func TestBuildDIDLNoCaption(t *testing.T) {
	d := buildDIDL(engine.Media{URL: "http://h/m.mp4", MIME: "video/mp4", Seekable: false})
	if strings.Contains(d, "CaptionInfo") {
		t.Errorf("expected no caption element:\n%s", d)
	}
	if !strings.Contains(d, "DLNA.ORG_OP=00") {
		t.Errorf("non-seekable should advertise OP=00:\n%s", d)
	}
}

// The sec:type attribute derives from the subtitle Format, not a file extension.
func TestBuildDIDLVTTType(t *testing.T) {
	m := engine.Media{
		URL: "http://h/m.mp4", MIME: "video/mp4",
		Subtitle: &engine.Subtitle{URL: "http://h/subs.vtt", Format: engine.SubFormatVTT, MIME: "text/vtt"},
	}
	d := buildDIDL(m)
	if !strings.Contains(d, `sec:type="vtt"`) {
		t.Errorf("VTT subtitle must carry sec:type=vtt:\n%s", d)
	}
}

func TestMapState(t *testing.T) {
	cases := map[string]engine.PlayerState{
		"PLAYING":          engine.StatePlaying,
		"PAUSED_PLAYBACK":  engine.StatePaused,
		"STOPPED":          engine.StateStopped,
		"NO_MEDIA_PRESENT": engine.StateIdle,
		"TRANSITIONING":    engine.StateBuffering,
		"LG_TRANSITIONING": engine.StateBuffering,
		"SOMETHING_ELSE":   engine.StateUnknown,
	}
	for in, want := range cases {
		if got := mapState(in); got != want {
			t.Errorf("mapState(%q) = %v, want %v", in, got, want)
		}
	}
}

func TestDeviceIdentity(t *testing.T) {
	e := New()
	if e.Name() != "dlna" {
		t.Fatalf("engine name = %q", e.Name())
	}
	p := e.DefaultProfile()
	if p.SoftSubFormat() != engine.SubFormatSRT {
		t.Fatal("webOS profile must prefer SRT soft subs")
	}
	if p.Containers != nil {
		t.Fatal("webOS profile must not restrict containers")
	}
	if !p.VideoCodecs["hevc"] || !p.AudioCodecs["eac3"] {
		t.Fatal("webOS codec sets regressed")
	}
	if p.HonorsStartAt {
		t.Fatal("DLNA has no load-time start offset")
	}
}

// Seek on a session whose loaded media was not seekable must return
// ErrNotSeekable without touching the network.
func TestSeekNotSeekable(t *testing.T) {
	s := &session{}
	if err := s.Seek(t.Context(), 10*time.Second); err != engine.ErrNotSeekable {
		t.Fatalf("err = %v, want ErrNotSeekable", err)
	}
}
