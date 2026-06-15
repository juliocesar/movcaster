package core

import (
	"context"
	"net/url"
	"strings"
	"testing"
	"time"

	"github.com/juliocesar/movcaster/internal/config"
	"github.com/juliocesar/movcaster/internal/discovery"
	"github.com/juliocesar/movcaster/internal/probe"
	"github.com/juliocesar/movcaster/internal/renderer"
	"github.com/juliocesar/movcaster/internal/subs"
)

// --- fakes ---

type fakeFinder struct {
	devices []discovery.Device
	err     error
	byURL   *discovery.Device
}

func (f fakeFinder) Discover(context.Context) ([]discovery.Device, error) {
	return f.devices, f.err
}
func (f fakeFinder) FindByURL(context.Context, *url.URL) (*discovery.Device, error) {
	if f.byURL == nil {
		return nil, context.Canceled
	}
	return f.byURL, nil
}

type fakeStore struct {
	cfg   config.Config
	saved []config.Config
}

func (s *fakeStore) Load() config.Config { return s.cfg }
func (s *fakeStore) Save(c config.Config) error {
	s.saved = append(s.saved, c)
	return nil
}

type fakeRenderer struct {
	calls    []string
	media    renderer.Media
	seekedTo time.Duration
	pos, dur time.Duration
	state    string
	hasVol   bool
}

func (r *fakeRenderer) SetMedia(_ context.Context, m renderer.Media) error {
	r.calls = append(r.calls, "SetMedia")
	r.media = m
	return nil
}
func (r *fakeRenderer) Play(context.Context) error  { r.calls = append(r.calls, "Play"); return nil }
func (r *fakeRenderer) Pause(context.Context) error { r.calls = append(r.calls, "Pause"); return nil }
func (r *fakeRenderer) Stop(context.Context) error  { r.calls = append(r.calls, "Stop"); return nil }
func (r *fakeRenderer) Seek(_ context.Context, p time.Duration) error {
	r.calls = append(r.calls, "Seek")
	r.seekedTo = p
	return nil
}
func (r *fakeRenderer) Position(context.Context) (time.Duration, time.Duration, error) {
	return r.pos, r.dur, nil
}
func (r *fakeRenderer) TransportState(context.Context) (string, error) { return r.state, nil }
func (r *fakeRenderer) HasVolume() bool                                { return r.hasVol }
func (r *fakeRenderer) Volume(context.Context) (int, error)            { return 0, nil }
func (r *fakeRenderer) SetVolume(context.Context, int) error           { return nil }
func (r *fakeRenderer) Mute(context.Context, bool) error               { return nil }

type fakeServer struct {
	calls         []string
	transcodeArgs [][]string
	directPlay    string
	subPath       string
	token         int
}

func (s *fakeServer) Start() { s.calls = append(s.calls, "Start") }
func (s *fakeServer) Shutdown(context.Context) error {
	s.calls = append(s.calls, "Shutdown")
	return nil
}
func (s *fakeServer) SetDirectPlay(path, _ string) {
	s.calls = append(s.calls, "SetDirectPlay")
	s.directPlay = path
	s.token++
}
func (s *fakeServer) SetTranscode(args []string) {
	s.calls = append(s.calls, "SetTranscode")
	s.transcodeArgs = append(s.transcodeArgs, args)
	s.token++
}
func (s *fakeServer) SetSubtitle(path, _ string) {
	s.calls = append(s.calls, "SetSubtitle")
	s.subPath = path
}
func (s *fakeServer) MediaURL() string { return "http://test/media.mp4?t=" + itoa(s.token) }
func (s *fakeServer) SubURL() string   { return "http://test/subs" }

func itoa(n int) string {
	if n == 0 {
		return "0"
	}
	var b []byte
	for n > 0 {
		b = append([]byte{byte('0' + n%10)}, b...)
		n /= 10
	}
	return string(b)
}

func device(name, host string) discovery.Device {
	u, _ := url.Parse("http://" + host + "/desc.xml")
	return discovery.Device{FriendlyName: name, Location: u}
}

// --- device resolution ---

func TestSelectDeviceSole(t *testing.T) {
	a := New(Options{Finder: fakeFinder{devices: []discovery.Device{device("TV", "10.0.0.5:1234")}}, Store: &fakeStore{}})
	d, err := a.selectDevice(context.Background(), "")
	if err != nil {
		t.Fatal(err)
	}
	if d.FriendlyName != "TV" {
		t.Fatalf("got %q", d.FriendlyName)
	}
}

func TestSelectDevicePrefersSaved(t *testing.T) {
	devs := []discovery.Device{device("A", "10.0.0.5:1"), device("B", "10.0.0.9:2")}
	a := New(Options{Finder: fakeFinder{devices: devs}, Store: &fakeStore{cfg: config.Config{LastDeviceHost: "10.0.0.9"}}})
	d, err := a.selectDevice(context.Background(), "")
	if err != nil {
		t.Fatal(err)
	}
	if d.FriendlyName != "B" {
		t.Fatalf("expected saved device B, got %q", d.FriendlyName)
	}
}

func TestSelectDeviceMultipleNoSavedErrors(t *testing.T) {
	devs := []discovery.Device{device("A", "10.0.0.5:1"), device("B", "10.0.0.9:2")}
	a := New(Options{Finder: fakeFinder{devices: devs}, Store: &fakeStore{}})
	if _, err := a.selectDevice(context.Background(), ""); err == nil {
		t.Fatal("expected ambiguity error")
	}
}

func TestSelectDeviceByHostIgnoresPort(t *testing.T) {
	devs := []discovery.Device{device("TV", "10.0.0.5:1570")}
	a := New(Options{Finder: fakeFinder{devices: devs}, Store: &fakeStore{}})
	// Target carries a stale port; match must be by host IP.
	d, err := a.selectDevice(context.Background(), "10.0.0.5:9999")
	if err != nil {
		t.Fatal(err)
	}
	if d.FriendlyName != "TV" {
		t.Fatalf("got %q", d.FriendlyName)
	}
}

// seqFinder returns a different result per Discover call, simulating a renderer
// that misses the first SSDP burst and only answers a later one.
type seqFinder struct {
	results []struct {
		devices []discovery.Device
		err     error
	}
	n int
}

func (f *seqFinder) Discover(context.Context) ([]discovery.Device, error) {
	r := f.results[min(f.n, len(f.results)-1)]
	f.n++
	return r.devices, r.err
}
func (f *seqFinder) FindByURL(context.Context, *url.URL) (*discovery.Device, error) {
	return nil, context.Canceled
}

func TestSelectDeviceFallbackFindsLateRenderer(t *testing.T) {
	f := &seqFinder{results: []struct {
		devices []discovery.Device
		err     error
	}{
		{nil, nil}, // quick pass: nothing
		{[]discovery.Device{device("TV", "10.0.0.5:1234")}, nil}, // deep pass: found
	}}
	var events []string
	a := New(Options{Finder: f, Store: &fakeStore{}, OnEvent: func(e Event) { events = append(events, e.Message) }})
	d, err := a.selectDevice(context.Background(), "")
	if err != nil {
		t.Fatal(err)
	}
	if d.FriendlyName != "TV" {
		t.Fatalf("got %q", d.FriendlyName)
	}
	found := false
	for _, m := range events {
		if strings.Contains(m, "Looking for a TV") {
			found = true
		}
	}
	if !found {
		t.Fatalf("expected a 'Looking for a TV' event, got %v", events)
	}
}

// --- seek-restart (the hard-won sequence) ---

func TestSeekTranscodeRestartSequence(t *testing.T) {
	r := &fakeRenderer{}
	s := &fakeServer{}
	built := []time.Duration{}
	c := &Cast{
		r:   r,
		srv: s,
		buildArgs: func(ss time.Duration) []string {
			built = append(built, ss)
			return []string{"-ss", ss.String()}
		},
	}
	if err := c.Seek(context.Background(), 90*time.Second); err != nil {
		t.Fatal(err)
	}
	// Renderer: Stop -> SetMedia -> Play.
	want := []string{"Stop", "SetMedia", "Play"}
	if strings.Join(r.calls, ",") != strings.Join(want, ",") {
		t.Fatalf("renderer calls = %v, want %v", r.calls, want)
	}
	// Server got a fresh transcode at the seek offset.
	if len(s.transcodeArgs) != 1 || len(built) != 1 || built[0] != 90*time.Second {
		t.Fatalf("transcode not relaunched at 90s: built=%v", built)
	}
	if c.ssOffset != 90*time.Second {
		t.Fatalf("ssOffset = %v, want 90s", c.ssOffset)
	}
}

func TestSeekDirectPlayNative(t *testing.T) {
	r := &fakeRenderer{}
	c := &Cast{r: r, srv: &fakeServer{}} // buildArgs nil => direct-play
	if err := c.Seek(context.Background(), 30*time.Second); err != nil {
		t.Fatal(err)
	}
	if len(r.calls) != 1 || r.calls[0] != "Seek" || r.seekedTo != 30*time.Second {
		t.Fatalf("expected native Seek to 30s, got calls=%v to=%v", r.calls, r.seekedTo)
	}
}

func TestPositionTranscodeAddsOffset(t *testing.T) {
	r := &fakeRenderer{pos: 10 * time.Second, dur: 5 * time.Second}
	c := &Cast{
		r:             r,
		srv:           &fakeServer{},
		buildArgs:     func(time.Duration) []string { return nil },
		knownDuration: 3600 * time.Second,
		ssOffset:      90 * time.Second,
	}
	pos, dur, err := c.Position(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if pos != 100*time.Second {
		t.Fatalf("pos = %v, want 100s (offset+rel)", pos)
	}
	if dur != 3600*time.Second {
		t.Fatalf("dur = %v, want known 3600s", dur)
	}
}

// --- subtitle application + events ---

func TestApplySubtitlesBurn(t *testing.T) {
	var events []Event
	a := New(Options{OnEvent: func(e Event) { events = append(events, e) }})
	s := &fakeServer{}
	media := &renderer.Media{Seekable: true, MIME: "video/x-matroska"}
	track := probe.SubTrack{SubIndex: 1, Codec: "hdmv_pgs_subtitle", Kind: probe.SubBitmap}
	dec := subs.Decision{Kind: subs.Burn, Track: &track}

	label, build, err := a.applySubtitles(s, media, "/x/movie.mkv", t.TempDir(), dec)
	if err != nil {
		t.Fatal(err)
	}
	if build == nil {
		t.Fatal("burn must return a transcode builder")
	}
	if media.Seekable || media.MIME != "video/mp4" {
		t.Fatalf("burn must switch media to non-seekable mp4, got seekable=%v mime=%q", media.Seekable, media.MIME)
	}
	if !strings.Contains(label, "burn-in: track 1") {
		t.Fatalf("label = %q", label)
	}
	if len(s.transcodeArgs) != 1 {
		t.Fatalf("expected one SetTranscode, got %d", len(s.transcodeArgs))
	}
	if len(events) == 0 || !strings.Contains(events[0].Message, "burning in") {
		t.Fatalf("expected a 'burning in' event, got %v", events)
	}
}

func TestApplySubtitlesSidecar(t *testing.T) {
	var events []Event
	a := New(Options{OnEvent: func(e Event) { events = append(events, e) }})
	s := &fakeServer{}
	media := &renderer.Media{}
	dec := subs.Decision{Kind: subs.SoftSidecar, SidecarPath: "/x/movie.srt"}

	label, build, err := a.applySubtitles(s, media, "/x/movie.mkv", t.TempDir(), dec)
	if err != nil {
		t.Fatal(err)
	}
	if build != nil {
		t.Fatal("sidecar must not transcode")
	}
	if media.SubURL == "" || media.SubType != "srt" {
		t.Fatalf("expected soft sub set, got url=%q type=%q", media.SubURL, media.SubType)
	}
	if label != "soft: movie.srt" {
		t.Fatalf("label = %q", label)
	}
	if s.subPath != "/x/movie.srt" {
		t.Fatalf("server sub path = %q", s.subPath)
	}
}

func TestApplySubtitlesNone(t *testing.T) {
	a := New(Options{})
	s := &fakeServer{}
	media := &renderer.Media{Seekable: true}
	label, build, err := a.applySubtitles(s, media, "/x/movie.mkv", t.TempDir(), subs.Decision{Kind: subs.None})
	if err != nil || label != "" || build != nil {
		t.Fatalf("None: label=%q build!=nil=%v err=%v", label, build != nil, err)
	}
	if len(s.calls) != 0 {
		t.Fatalf("None must not touch the server, got %v", s.calls)
	}
}

// --- resume ---

type fakeResume struct {
	got     map[string]time.Duration
	cleared []string
}

func (f *fakeResume) Get(p string) time.Duration { return f.got[p] }
func (f *fakeResume) Set(p string, d time.Duration) error {
	if f.got == nil {
		f.got = map[string]time.Duration{}
	}
	f.got[p] = d
	return nil
}
func (f *fakeResume) Clear(p string) error { f.cleared = append(f.cleared, p); return nil }

func TestResumeOffset(t *testing.T) {
	cases := []struct {
		saved, dur, want time.Duration
	}{
		{90 * time.Second, 3600 * time.Second, 90 * time.Second}, // normal
		{2 * time.Second, 3600 * time.Second, 0},                 // negligible
		{3595 * time.Second, 3600 * time.Second, 0},              // within 30s of end
		{90 * time.Second, 0, 90 * time.Second},                  // unknown duration still resumes
	}
	for _, c := range cases {
		if got := resumeOffset(c.saved, c.dur); got != c.want {
			t.Errorf("resumeOffset(%v,%v) = %v, want %v", c.saved, c.dur, got, c.want)
		}
	}
}

func TestCloseSavesPosition(t *testing.T) {
	fr := &fakeResume{}
	c := &Cast{srv: &fakeServer{}, resume: fr, resumePath: "/x/movie.mkv", knownDuration: 3600 * time.Second, lastPos: 100 * time.Second}
	_ = c.Close(context.Background())
	if fr.got["/x/movie.mkv"] != 100*time.Second {
		t.Fatalf("saved = %v, want 100s", fr.got["/x/movie.mkv"])
	}
}

func TestCloseClearsWhenFinished(t *testing.T) {
	fr := &fakeResume{}
	c := &Cast{srv: &fakeServer{}, resume: fr, resumePath: "/x/movie.mkv", knownDuration: 3600 * time.Second, lastPos: 3595 * time.Second}
	_ = c.Close(context.Background())
	if len(fr.cleared) != 1 || fr.cleared[0] != "/x/movie.mkv" {
		t.Fatalf("expected clear of finished file, got cleared=%v set=%v", fr.cleared, fr.got)
	}
}

func TestCloseSkipsTinyPosition(t *testing.T) {
	fr := &fakeResume{}
	c := &Cast{srv: &fakeServer{}, resume: fr, resumePath: "/x/movie.mkv", knownDuration: 3600 * time.Second, lastPos: 2 * time.Second}
	_ = c.Close(context.Background())
	if len(fr.got) != 0 || len(fr.cleared) != 0 {
		t.Fatalf("tiny position should neither set nor clear: set=%v cleared=%v", fr.got, fr.cleared)
	}
}

func TestPositionCachesLastPos(t *testing.T) {
	r := &fakeRenderer{pos: 30 * time.Second, dur: 100 * time.Second}
	c := &Cast{r: r, srv: &fakeServer{}} // direct-play
	if _, _, err := c.Position(context.Background()); err != nil {
		t.Fatal(err)
	}
	if c.lastPos != 30*time.Second {
		t.Fatalf("lastPos = %v, want 30s", c.lastPos)
	}
}

func TestCodecPlan(t *testing.T) {
	// av1 video is outside the good list -> transcode video only.
	p := codecPlan(&probe.MediaInfo{VideoCodec: "av1", AudioCodec: "aac"}, false)
	if p.Kind != TranscodeCodec || !p.Video || p.Audio {
		t.Fatalf("av1/aac: %+v", p)
	}
	// All good -> none.
	p = codecPlan(&probe.MediaInfo{VideoCodec: "h264", AudioCodec: "aac"}, false)
	if p.Kind != TranscodeNone {
		t.Fatalf("h264/aac should not transcode: %+v", p)
	}
	// Forced -> both.
	p = codecPlan(&probe.MediaInfo{VideoCodec: "h264", AudioCodec: "aac"}, true)
	if p.Kind != TranscodeCodec || !p.Video || !p.Audio {
		t.Fatalf("forced: %+v", p)
	}
}

// Cast satisfies the TUI's control surface (compile-time check mirrors tui.Controller).
var _ interface {
	Play(context.Context) error
	Pause(context.Context) error
	Stop(context.Context) error
	Seek(context.Context, time.Duration) error
	Position(context.Context) (time.Duration, time.Duration, error)
	TransportState(context.Context) (string, error)
	HasVolume() bool
	Volume(context.Context) (int, error)
	SetVolume(context.Context, int) error
	Mute(context.Context, bool) error
} = (*Cast)(nil)
