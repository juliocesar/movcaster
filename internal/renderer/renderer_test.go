package renderer

import (
	"strings"
	"testing"
	"time"
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
	m := Media{
		URL: "http://h/media.mp4", Title: "Movie & Co", MIME: "video/mp4",
		Duration: 90 * time.Second, Seekable: true,
		SubURL: "http://h/subs.srt", SubMIME: "text/srt", SubType: "srt",
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
	d := buildDIDL(Media{URL: "http://h/m.mp4", MIME: "video/mp4", Seekable: false})
	if strings.Contains(d, "CaptionInfo") {
		t.Errorf("expected no caption element:\n%s", d)
	}
	if !strings.Contains(d, "DLNA.ORG_OP=00") {
		t.Errorf("non-seekable should advertise OP=00:\n%s", d)
	}
}
