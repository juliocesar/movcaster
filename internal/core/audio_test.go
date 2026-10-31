package core

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/juliocesar/movcaster/internal/probe"
	"github.com/juliocesar/movcaster/internal/renderer"
)

// dualAudio mirrors the release that prompted the feature: Spanish first,
// English second, neither flagged default.
func dualAudio() *probe.MediaInfo {
	return &probe.MediaInfo{VideoCodec: "hevc", AudioCodec: "eac3", Audio: []probe.AudioTrack{
		{AudioIndex: 0, Codec: "eac3", Language: "spa", Channels: 6},
		{AudioIndex: 1, Codec: "eac3", Language: "eng", Channels: 6},
	}}
}

func TestSelectAudio(t *testing.T) {
	tr := func(lang, title string, def bool) probe.AudioTrack {
		return probe.AudioTrack{Codec: "aac", Language: lang, Title: title, Default: def}
	}
	info := func(ts ...probe.AudioTrack) *probe.MediaInfo {
		for i := range ts {
			ts[i].AudioIndex = i
		}
		return &probe.MediaInfo{Audio: ts}
	}
	cases := []struct {
		name string
		info *probe.MediaInfo
		req  int
		want int
	}{
		{"english second", dualAudio(), -1, 1},
		{"explicit wins over english", dualAudio(), 0, 0},
		{"no english: native default", info(tr("spa", "", false), tr("fre", "", true)), -1, 1},
		{"no english, no default: first", info(tr("spa", "", false), tr("fre", "", false)), -1, 0},
		{"default english preferred", info(tr("eng", "", false), tr("eng", "", true)), -1, 1},
		{"commentary skipped", info(tr("eng", "Commentary", false), tr("eng", "", false)), -1, 1},
		{"only commentary is english", info(tr("spa", "", false), tr("eng", "Director's Commentary", false)), -1, 1},
		{"two-letter code", info(tr("spa", "", false), tr("en", "", false)), -1, 1},
		{"untagged", info(tr("", "", false), tr("", "", false)), -1, 0},
		{"no audio", &probe.MediaInfo{}, -1, 0},
		{"no probe", nil, -1, 0},
	}
	for _, c := range cases {
		got, err := selectAudio(c.info, c.req)
		if err != nil || got != c.want {
			t.Errorf("%s: selectAudio = (%d, %v), want %d", c.name, got, err, c.want)
		}
	}
	if _, err := selectAudio(dualAudio(), 2); err == nil {
		t.Error("explicit out-of-range track must error")
	}
}

// Given a raw multi-track file the TV picks the track itself (webOS played a:0
// Spanish as English), so every choice on a multi-track file streams a copy-only
// remux; only a single-track file direct-plays.
func TestCodecPlanAudioTrack(t *testing.T) {
	for _, a := range []int{0, 1} {
		if p := codecPlan(dualAudio(), false, a); p.Kind != TranscodeAudio || p.Video || p.Audio || p.AudioTrack != a {
			t.Fatalf("a:%d: %+v, want copy-only remux of a:%d", a, p, a)
		}
	}
	single := &probe.MediaInfo{VideoCodec: "hevc", Audio: dualAudio().Audio[:1]}
	if p := codecPlan(single, false, 0); p.Kind != TranscodeNone {
		t.Fatalf("single track: %+v, want direct play", p)
	}
	// A codec need still wins, carrying the chosen track.
	if p := codecPlan(&probe.MediaInfo{VideoCodec: "av1", Audio: dualAudio().Audio}, false, 1); p.Kind != TranscodeCodec || !p.Video || p.AudioTrack != 1 {
		t.Fatalf("av1 a:1: %+v", p)
	}
}

func TestBuildDeliveryAudioTrack(t *testing.T) {
	a := New(Options{})
	off := SubtitleOptions{NoSubs: true, TrackIndex: -1}

	s := &fakeServer{}
	media := &renderer.Media{}
	label, build, _, err := a.buildDelivery(s, media, "/x/movie.mkv", t.TempDir(), dualAudio(), off, 1, false, 30*time.Second)
	if err != nil {
		t.Fatal(err)
	}
	if build == nil || media.Seekable || media.MIME != "video/mp4" {
		t.Fatalf("a:1 must stream a remux: build=%v seekable=%v mime=%q", build != nil, media.Seekable, media.MIME)
	}
	if label != "" {
		t.Errorf("an audio remux is not a subtitle; label = %q", label)
	}
	args := strings.Join(s.transcodeArgs[len(s.transcodeArgs)-1], " ")
	if !strings.Contains(args, "-map 0:a:1?") || !strings.Contains(args, "-c:v copy") || !strings.Contains(args, "-c:a copy") || !strings.Contains(args, "-ss 30.000") {
		t.Errorf("remux args: %s", args)
	}

	s = &fakeServer{}
	media = &renderer.Media{}
	_, build, _, err = a.buildDelivery(s, media, "/x/movie.mkv", t.TempDir(), dualAudio(), off, 0, false, 0)
	if err != nil {
		t.Fatal(err)
	}
	if build == nil || !strings.Contains(strings.Join(build(0), " "), "-map 0:a:0?") {
		t.Fatal("a:0 of a multi-track file must remux too: direct play lets the TV pick")
	}
}

// A burn-in carries the chosen audio track instead of always a:0.
func TestBuildDeliveryBurnCarriesAudioTrack(t *testing.T) {
	a := New(Options{})
	info := dualAudio()
	info.Subtitles = []probe.SubTrack{{SubIndex: 0, Codec: "hdmv_pgs_subtitle", Kind: probe.SubBitmap}}
	s := &fakeServer{}
	_, build, _, err := a.buildDelivery(s, &renderer.Media{}, "/x/movie.mkv", t.TempDir(), info, SubtitleOptions{TrackIndex: 0}, 1, false, 0)
	if err != nil || build == nil {
		t.Fatalf("burn: build=%v err=%v", build != nil, err)
	}
	if args := strings.Join(build(0), " "); !strings.Contains(args, "-map 0:a:1?") || !strings.Contains(args, "overlay") {
		t.Errorf("burn args: %s", args)
	}
}

// SetAudio restarts at the current position (as a remux in both directions) and
// keeps the subtitle delivery.
func TestSetAudio(t *testing.T) {
	r := &fakeRenderer{}
	s := &fakeServer{}
	info := dualAudio()
	c := &Cast{
		r: r, srv: s, app: New(Options{}), abs: "/x/movie.mkv", info: info,
		audioChoices: buildAudioChoices(info), lastPos: 40 * time.Second,
		subOpts: SubtitleOptions{NoSubs: true, TrackIndex: -1},
	}

	if err := c.SetAudio(context.Background(), 1); err != nil {
		t.Fatal(err)
	}
	if want := "Stop,SetMedia,Play"; strings.Join(r.calls, ",") != want {
		t.Fatalf("calls = %v, want %s", r.calls, want)
	}
	if c.ActiveAudio() != 1 || c.buildArgs == nil || c.ssOffset != 40*time.Second {
		t.Fatalf("after a:1: audio=%d build=%v ss=%v", c.ActiveAudio(), c.buildArgs != nil, c.ssOffset)
	}
	if !strings.Contains(c.AudioInfo(), "English") {
		t.Errorf("AudioInfo = %q", c.AudioInfo())
	}
	if !c.subOpts.NoSubs {
		t.Error("audio switch must keep the subtitle options")
	}

	r.calls = nil
	if err := c.SetAudio(context.Background(), 0); err != nil {
		t.Fatal(err)
	}
	if want := "Stop,SetMedia,Play"; strings.Join(r.calls, ",") != want {
		t.Fatalf("calls = %v, want %s", r.calls, want)
	}
	if c.buildArgs == nil || c.ssOffset != 40*time.Second {
		t.Fatalf("a:0 must remux from 40s: build=%v ss=%v", c.buildArgs != nil, c.ssOffset)
	}
	if args := strings.Join(c.buildArgs(40*time.Second), " "); !strings.Contains(args, "-map 0:a:0?") {
		t.Errorf("a:0 remux args: %s", args)
	}
	if err := c.SetAudio(context.Background(), 2); err == nil {
		t.Error("out-of-range track must error")
	}
}

// A subtitle switch keeps the chosen audio track.
func TestSetSubtitleKeepsAudioTrack(t *testing.T) {
	info := dualAudio()
	c := &Cast{
		r: &fakeRenderer{}, srv: &fakeServer{}, app: New(Options{}), abs: "/x/movie.mkv",
		info: info, subChoices: buildSubChoices(info, ""), audio: 1,
	}
	if err := c.SetSubtitle(context.Background(), 0); err != nil { // Off
		t.Fatal(err)
	}
	if c.ActiveAudio() != 1 || c.buildArgs == nil {
		t.Fatalf("audio=%d build=%v, want a:1 still remuxed", c.ActiveAudio(), c.buildArgs != nil)
	}
}

func TestAudioLabels(t *testing.T) {
	if got := audioLabel(dualAudio().Audio[1]); got != "a:1 — English (eac3, 5.1)" {
		t.Errorf("audioLabel = %q", got)
	}
	single := &probe.MediaInfo{Audio: dualAudio().Audio[:1]}
	if got := audioTrackLabel(single, 0); got != "" {
		t.Errorf("single-track file should have no audio label, got %q", got)
	}
}

// webOS auto-plays a freshly set URI and can reject every Play with 701 while it
// buffers. A switch the TV is playing anyway is a success, and the direct-play
// resume seek still runs.
func TestSwitchToleratesRejectedPlayWhenPlaying(t *testing.T) {
	r := &fakeRenderer{playErr: errors.New("701 Transition not available"), state: "PLAYING"}
	info := &probe.MediaInfo{Subtitles: []probe.SubTrack{{SubIndex: 0, Codec: "hdmv_pgs_subtitle", Kind: probe.SubBitmap}}}
	c := &Cast{
		r: r, srv: &fakeServer{}, app: New(Options{}), abs: "/x/movie.mkv", info: info,
		subChoices: buildSubChoices(info, ""), lastPos: 50 * time.Second,
		buildArgs: func(time.Duration) []string { return nil },
	}
	if err := c.SetSubtitle(context.Background(), 1); err != nil { // Off => direct play
		t.Fatalf("switch failed although the TV is playing: %v", err)
	}
	if r.seekedTo != 50*time.Second {
		t.Fatalf("resume seek = %v, want 50s", r.seekedTo)
	}
}

// When the handshake really fails, the cast's state must still describe what the
// server now streams, or later seeks restart the wrong stream.
func TestFailedSwitchKeepsStateInStepWithServer(t *testing.T) {
	r := &fakeRenderer{playErr: errors.New("701 Transition not available"), state: "STOPPED"}
	info := dualAudio()
	c := &Cast{
		r: r, srv: &fakeServer{}, app: New(Options{}), abs: "/x/movie.mkv", info: info,
		audioChoices: buildAudioChoices(info), audio: 1, lastPos: 40 * time.Second,
		subOpts: SubtitleOptions{NoSubs: true, TrackIndex: -1},
	}
	if err := c.SetAudio(context.Background(), 0); err == nil {
		t.Fatal("want the Play error when the TV isn't playing")
	}
	if c.ActiveAudio() != 0 || c.buildArgs == nil || !strings.Contains(strings.Join(c.buildArgs(0), " "), "0:a:0?") {
		t.Fatalf("state out of step: audio=%d build=%v", c.ActiveAudio(), c.buildArgs != nil)
	}
}
