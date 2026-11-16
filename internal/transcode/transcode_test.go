package transcode

import (
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/juliocesar/movcaster/internal/engine"
	"github.com/juliocesar/movcaster/internal/probe"
)

func set(names ...string) map[string]bool {
	m := make(map[string]bool, len(names))
	for _, n := range names {
		m[n] = true
	}
	return m
}

// webOS is the LG profile: no container restriction, broad codec support.
var webOS = engine.Profile{
	VideoCodecs: set("h264", "hevc", "mpeg4", "mpeg2video", "vc1", "msmpeg4v3"),
	AudioCodecs: set("aac", "ac3", "eac3", "mp3", "mp2", "dts", "flac"),
}

// cast is the conservative Default Media Receiver profile.
var cast = engine.Profile{
	Containers:  set("mp4", "webm"),
	VideoCodecs: set("h264", "vp8", "vp9"),
	AudioCodecs: set("aac", "mp3", "opus", "vorbis", "flac"),
}

func TestNeeds(t *testing.T) {
	cases := []struct {
		name      string
		info      probe.MediaInfo
		p         engine.Profile
		wantV     bool
		wantA     bool
		wantRemux bool
	}{
		{"webos direct-play", probe.MediaInfo{Container: "matroska", VideoCodec: "h264", AudioCodec: "eac3"}, webOS, false, false, false},
		{"webos av1 video only", probe.MediaInfo{Container: "matroska", VideoCodec: "av1", AudioCodec: "eac3"}, webOS, true, false, false},
		{"webos opus audio only", probe.MediaInfo{Container: "matroska", VideoCodec: "h264", AudioCodec: "opus"}, webOS, false, true, false},
		{"webos both", probe.MediaInfo{VideoCodec: "vp9", AudioCodec: "truehd"}, webOS, true, true, false},
		{"cast mp4 direct-play", probe.MediaInfo{Container: "mp4", VideoCodec: "h264", AudioCodec: "aac"}, cast, false, false, false},
		{"cast mkv remux only", probe.MediaInfo{Container: "matroska", VideoCodec: "h264", AudioCodec: "aac"}, cast, false, false, true},
		{"cast mkv eac3 remux+audio", probe.MediaInfo{Container: "matroska", VideoCodec: "h264", AudioCodec: "eac3"}, cast, false, true, true},
		{"cast av1 opus full", probe.MediaInfo{Container: "matroska", VideoCodec: "av1", AudioCodec: "opus"}, cast, true, false, true},
		{"empty probe", probe.MediaInfo{}, cast, false, false, false},
	}
	for _, c := range cases {
		got := Needs(&c.info, c.p, 0)
		if got.Video != c.wantV || got.Audio != c.wantA || got.Remux != c.wantRemux {
			t.Errorf("%s: Needs = %+v, want video=%v audio=%v remux=%v", c.name, got, c.wantV, c.wantA, c.wantRemux)
		}
	}
	if got := Needs(nil, cast, 0); got.Needed() {
		t.Errorf("Needs(nil) = %+v, want nothing", got)
	}
}

func TestNeedsLimits(t *testing.T) {
	p := engine.Profile{MaxVideoHeight: 1080, MaxAudioChannels: 2}
	got := Needs(&probe.MediaInfo{VideoCodec: "h264", Height: 2160, AudioCodec: "aac", AudioChannels: 6}, p, 0)
	if !got.Video || !got.Audio {
		t.Fatalf("4K 5.1 over 1080p stereo limits must re-encode both, got %+v", got)
	}
	got = Needs(&probe.MediaInfo{VideoCodec: "h264", Height: 1080, AudioCodec: "aac", AudioChannels: 2}, p, 0)
	if got.Needed() {
		t.Fatalf("within limits must direct-play, got %+v", got)
	}
}

// The MP4 muxer refuses to write the moov atom for a copied (E-)AC-3 stream
// until it has parsed a packet, so a bare empty_moov aborts ffmpeg before any
// bytes reach the TV. delay_moov must be present on every invocation.
func TestArgsAlwaysDelaysMoov(t *testing.T) {
	for _, plan := range []Plan{{}, {Video: true}, {Audio: true}, {Video: true, Audio: true}, {Remux: true}} {
		args := strings.Join(Args("in.mkv", 0, webOS, plan, 0), " ")
		if !strings.Contains(args, "+delay_moov") {
			t.Errorf("Args(%+v) missing +delay_moov: %s", plan, args)
		}
	}
}

// delay_moov makes ffmpeg emit edit lists, which webOS mishandles (video freezes
// on resume from pause while audio keeps playing), so they must stay suppressed.
func TestArgsSuppressesEditLists(t *testing.T) {
	for _, plan := range []Plan{{}, {Video: true}, {Audio: true}, {Video: true, Audio: true}, {Remux: true}} {
		args := Args("in.mkv", 0, webOS, plan, 0)
		var found bool
		for i, a := range args {
			if a == "-use_editlist" && i+1 < len(args) && args[i+1] == "0" {
				found = true
			}
		}
		if !found {
			t.Errorf("Args(%+v) missing -use_editlist 0: %s", plan, strings.Join(args, " "))
		}
	}
}

func TestArgsCodecSelection(t *testing.T) {
	got := strings.Join(Args("in.mkv", 0, webOS, Plan{Video: true}, 0), " ")
	if !strings.Contains(got, "-c:v libx264") || !strings.Contains(got, "-c:a copy") {
		t.Errorf("video-only transcode: %s", got)
	}
	got = strings.Join(Args("in.mkv", 0, webOS, Plan{Audio: true}, 0), " ")
	if !strings.Contains(got, "-c:v copy") || !strings.Contains(got, "-c:a aac") {
		t.Errorf("audio-only transcode: %s", got)
	}
	// The targets come from the profile, not hardcoded strings.
	p := webOS
	p.TranscodeVideo, p.TranscodeAudio = "libx265", "libopus"
	got = strings.Join(Args("in.mkv", 0, p, Plan{Video: true, Audio: true}, 0), " ")
	if !strings.Contains(got, "-c:v libx265") || !strings.Contains(got, "-c:a libopus") {
		t.Errorf("profile targets ignored: %s", got)
	}
}

// A remux-only plan must copy both streams: paying an encode for a container
// change would be absurd.
func TestArgsRemuxCopiesBothStreams(t *testing.T) {
	got := strings.Join(Args("in.mkv", 0, cast, Plan{Remux: true}, 0), " ")
	if !strings.Contains(got, "-c:v copy") || !strings.Contains(got, "-c:a copy") {
		t.Errorf("remux must copy both streams: %s", got)
	}
	if !strings.Contains(got, "-f mp4 pipe:1") {
		t.Errorf("remux must retarget the container: %s", got)
	}
}

func TestArgsSeekOffset(t *testing.T) {
	full := Plan{Video: true, Audio: true}
	if got := strings.Join(Args("in.mkv", 0, webOS, full, 0), " "); strings.Contains(got, "-ss") {
		t.Errorf("ss=0 should not emit -ss: %s", got)
	}
	got := Args("in.mkv", 90*time.Second, webOS, full, 0)
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

// The chosen track's codec (and channel count) decides the audio need, not the
// first track's: on webOS a file whose a:0 is TrueHD but a:1 is E-AC-3 copies
// a:1 untouched, while on Cast the same E-AC-3 track still needs re-encoding and
// an AAC track does not.
func TestNeedsJudgesChosenTrack(t *testing.T) {
	mi := &probe.MediaInfo{Container: "mp4", VideoCodec: "h264", AudioCodec: "truehd", AudioChannels: 8, Audio: []probe.AudioTrack{
		{AudioIndex: 0, Codec: "truehd", Channels: 8}, {AudioIndex: 1, Codec: "eac3", Channels: 6}, {AudioIndex: 2, Codec: "aac", Channels: 2},
	}}
	if !Needs(mi, webOS, 0).Audio {
		t.Error("webOS a:0 truehd should need an audio transcode")
	}
	if Needs(mi, webOS, 1).Audio {
		t.Error("webOS a:1 eac3 should be copied")
	}
	if !Needs(mi, cast, 1).Audio {
		t.Error("cast a:1 eac3 should need an audio transcode")
	}
	if got := Needs(mi, cast, 2); got.Needed() {
		t.Errorf("cast a:2 aac in mp4 should direct-play, got %+v", got)
	}
	if !Needs(mi, engine.Profile{MaxAudioChannels: 2}, 1).Audio {
		t.Error("a 5.1 chosen track over a stereo limit should re-encode")
	}
	if Needs(mi, engine.Profile{MaxAudioChannels: 2}, 2).Audio {
		t.Error("a stereo chosen track within a stereo limit should copy")
	}
}

func TestArgsMapsChosenAudioTrack(t *testing.T) {
	got := strings.Join(Args("in.mkv", 0, webOS, Plan{Remux: true}, 1), " ")
	if !strings.Contains(got, "-map 0:a:1?") || strings.Contains(got, "0:a:0") {
		t.Errorf("want only -map 0:a:1?, got %s", got)
	}
}

// A re-encoded track keeps its surround only where the profile allows it:
// stereo by default (the LG's long-standing behavior), up to 5.1 on a device
// whose probe played a 5.1 AAC clip, and never more channels than the source.
func TestReencodeChannels(t *testing.T) {
	eac3 := func(ch int) *probe.MediaInfo {
		return &probe.MediaInfo{Container: "mp4", VideoCodec: "h264", AudioCodec: "eac3", AudioChannels: ch}
	}
	surround := cast
	surround.TranscodeAudioChannels = 6
	cases := []struct {
		name string
		mi   *probe.MediaInfo
		p    engine.Profile
		want int
	}{
		{"default profile downmixes 5.1", eac3(6), cast, 2},
		{"surround profile keeps 5.1", eac3(6), surround, 6},
		{"surround profile caps 7.1 at 5.1", eac3(8), surround, 6},
		{"surround profile never upmixes stereo", eac3(2), surround, 2},
		{"unknown source channels is stereo", eac3(0), surround, 2},
		{"a channel limit wins", eac3(6), engine.Profile{AudioCodecs: set("aac"), TranscodeAudioChannels: 6, MaxAudioChannels: 2}, 2},
	}
	for _, c := range cases {
		plan := Needs(c.mi, c.p, 0)
		if !plan.Audio || plan.AudioChannels != c.want {
			t.Errorf("%s: plan = %+v, want audio re-encode at %d channels", c.name, plan, c.want)
		}
		args := strings.Join(Args("in.mkv", 0, c.p, plan, 0), " ")
		if !strings.Contains(args, fmt.Sprintf("-ac %d", c.want)) {
			t.Errorf("%s: args %s, want -ac %d", c.name, args, c.want)
		}
	}
	if got := strings.Join(Args("in.mkv", 0, surround, Needs(eac3(6), surround, 0), 0), " "); !strings.Contains(got, "-b:a 384k") {
		t.Errorf("5.1 AAC should get a 384k bitrate: %s", got)
	}
	if got := ForceAll(eac3(6), surround, 0); !got.Video || !got.Audio || got.AudioChannels != 6 {
		t.Errorf("ForceAll = %+v, want everything re-encoded at 6 channels", got)
	}
}
