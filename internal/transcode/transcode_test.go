package transcode

import (
	"strings"
	"testing"
	"time"

	"github.com/juliocesar/movcaster/internal/probe"
)

func TestNeeds(t *testing.T) {
	cases := []struct {
		video, audio string
		wantV, wantA bool
	}{
		{"h264", "aac", false, false},
		{"hevc", "eac3", false, false},
		{"av1", "eac3", true, false},  // AV1 WEB-DLs: video only
		{"h264", "opus", false, true}, // audio only
		{"vp9", "truehd", true, true}, // both
		{"", "", false, false},        // probe found nothing
	}
	for _, c := range cases {
		v, a := Needs(&probe.MediaInfo{VideoCodec: c.video, AudioCodec: c.audio})
		if v != c.wantV || a != c.wantA {
			t.Errorf("Needs(%s/%s) = (%v,%v), want (%v,%v)", c.video, c.audio, v, a, c.wantV, c.wantA)
		}
	}
	if v, a := Needs(nil); v || a {
		t.Errorf("Needs(nil) = (%v,%v), want (false,false)", v, a)
	}
}

// The MP4 muxer refuses to write the moov atom for a copied (E-)AC-3 stream
// until it has parsed a packet, so a bare empty_moov aborts ffmpeg before any
// bytes reach the TV. delay_moov must be present on every invocation.
func TestArgsAlwaysDelaysMoov(t *testing.T) {
	for _, tv := range []bool{true, false} {
		for _, ta := range []bool{true, false} {
			args := strings.Join(Args("in.mkv", 0, tv, ta), " ")
			if !strings.Contains(args, "+delay_moov") {
				t.Errorf("Args(video=%v audio=%v) missing +delay_moov: %s", tv, ta, args)
			}
		}
	}
}

// delay_moov makes ffmpeg emit edit lists, which webOS mishandles (video freezes
// on resume from pause while audio keeps playing), so they must stay suppressed.
func TestArgsSuppressesEditLists(t *testing.T) {
	for _, tv := range []bool{true, false} {
		for _, ta := range []bool{true, false} {
			args := Args("in.mkv", 0, tv, ta)
			var found bool
			for i, a := range args {
				if a == "-use_editlist" && i+1 < len(args) && args[i+1] == "0" {
					found = true
				}
			}
			if !found {
				t.Errorf("Args(video=%v audio=%v) missing -use_editlist 0: %s", tv, ta, strings.Join(args, " "))
			}
		}
	}
}

func TestArgsCodecSelection(t *testing.T) {
	got := strings.Join(Args("in.mkv", 0, true, false), " ")
	if !strings.Contains(got, "-c:v libx264") || !strings.Contains(got, "-c:a copy") {
		t.Errorf("video-only transcode: %s", got)
	}
	got = strings.Join(Args("in.mkv", 0, false, true), " ")
	if !strings.Contains(got, "-c:v copy") || !strings.Contains(got, "-c:a aac") {
		t.Errorf("audio-only transcode: %s", got)
	}
}

func TestArgsSeekOffset(t *testing.T) {
	if got := strings.Join(Args("in.mkv", 0, true, true), " "); strings.Contains(got, "-ss") {
		t.Errorf("ss=0 should not emit -ss: %s", got)
	}
	got := Args("in.mkv", 90*time.Second, true, true)
	joined := strings.Join(got, " ")
	if !strings.Contains(joined, "-ss 90.000") {
		t.Errorf("want -ss 90.000, got %s", joined)
	}
	// -ss must precede -i so ffmpeg input-seeks rather than decoding to the point.
	var iss, ii int
	for i, a := range got {
		switch a {
		case "-ss":
			iss = i
		case "-i":
			ii = i
		}
	}
	if iss > ii {
		t.Errorf("-ss (%d) must come before -i (%d): %s", iss, ii, joined)
	}
}
