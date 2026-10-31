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
		v, a := Needs(&probe.MediaInfo{VideoCodec: c.video, AudioCodec: c.audio}, 0)
		if v != c.wantV || a != c.wantA {
			t.Errorf("Needs(%s/%s) = (%v,%v), want (%v,%v)", c.video, c.audio, v, a, c.wantV, c.wantA)
		}
	}
	if v, a := Needs(nil, 0); v || a {
		t.Errorf("Needs(nil, 0) = (%v,%v), want (false,false)", v, a)
	}
}

// The MP4 muxer refuses to write the moov atom for a copied (E-)AC-3 stream
// until it has parsed a packet, so a bare empty_moov aborts ffmpeg before any
// bytes reach the TV. delay_moov must be present on every invocation.
func TestArgsAlwaysDelaysMoov(t *testing.T) {
	for _, tv := range []bool{true, false} {
		for _, ta := range []bool{true, false} {
			args := strings.Join(Args("in.mkv", 0, tv, ta, 0), " ")
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
			args := Args("in.mkv", 0, tv, ta, 0)
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
	got := strings.Join(Args("in.mkv", 0, true, false, 0), " ")
	if !strings.Contains(got, "-c:v libx264") || !strings.Contains(got, "-c:a copy") {
		t.Errorf("video-only transcode: %s", got)
	}
	got = strings.Join(Args("in.mkv", 0, false, true, 0), " ")
	if !strings.Contains(got, "-c:v copy") || !strings.Contains(got, "-c:a aac") {
		t.Errorf("audio-only transcode: %s", got)
	}
}

func TestArgsSeekOffset(t *testing.T) {
	if got := strings.Join(Args("in.mkv", 0, true, true, 0), " "); strings.Contains(got, "-ss") {
		t.Errorf("ss=0 should not emit -ss: %s", got)
	}
	got := Args("in.mkv", 90*time.Second, true, true, 0)
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

// The chosen track's codec decides the audio need, not the first track's: a file
// whose a:0 is TrueHD but a:1 is E-AC-3 copies a:1 untouched.
func TestNeedsJudgesChosenTrack(t *testing.T) {
	mi := &probe.MediaInfo{VideoCodec: "hevc", AudioCodec: "truehd", Audio: []probe.AudioTrack{
		{AudioIndex: 0, Codec: "truehd"}, {AudioIndex: 1, Codec: "eac3"},
	}}
	if _, a := Needs(mi, 0); !a {
		t.Error("a:0 truehd should need an audio transcode")
	}
	if _, a := Needs(mi, 1); a {
		t.Error("a:1 eac3 should be copied")
	}
}

func TestArgsMapsChosenAudioTrack(t *testing.T) {
	got := strings.Join(Args("in.mkv", 0, false, false, 1), " ")
	if !strings.Contains(got, "-map 0:a:1?") || strings.Contains(got, "0:a:0") {
		t.Errorf("want only -map 0:a:1?, got %s", got)
	}
}
