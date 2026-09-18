package core

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/juliocesar/movcaster/internal/config"
	"github.com/juliocesar/movcaster/internal/engine"
	"github.com/juliocesar/movcaster/internal/probe"
	"github.com/juliocesar/movcaster/internal/subs"
)

// --- fakes ---

type fakeDevice struct {
	name string
	host string
	addr string
	eng  string
	prof engine.Profile
	sess *fakeSession
}

func (d *fakeDevice) Name() string            { return d.name }
func (d *fakeDevice) Host() string            { return d.host }
func (d *fakeDevice) Addr() string            { return d.addr }
func (d *fakeDevice) Engine() string          { return d.eng }
func (d *fakeDevice) Profile() engine.Profile { return d.prof }
func (d *fakeDevice) Open(context.Context) (engine.Session, error) {
	if d.sess == nil {
		d.sess = &fakeSession{}
	}
	return d.sess, nil
}

func device(name, host string) *fakeDevice {
	h, addr := host, host
	if i := strings.IndexByte(host, ':'); i >= 0 {
		h = host[:i]
	}
	return &fakeDevice{name: name, host: h, addr: addr, eng: "dlna", prof: defaultProfile}
}

type fakeFinder struct {
	devices    []engine.Device
	err        error
	resolved   engine.Device
	resolveErr error
}

func (f fakeFinder) Discover(context.Context, string) ([]engine.Device, error) {
	return f.devices, f.err
}
func (f fakeFinder) Resolve(context.Context, string, string) (engine.Device, error) {
	if f.resolved == nil {
		if f.resolveErr != nil {
			return nil, f.resolveErr
		}
		return nil, context.Canceled
	}
	return f.resolved, nil
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

// fakeSession records the control calls a Cast issues. Engines own the settle
// waits and retries now, so the fake is deliberately simple: Load and Play just
// succeed, and the tests assert the call sequence.
type fakeSession struct {
	calls    []string
	media    engine.Media
	seekedTo time.Duration
	st       engine.State
	hasVol   bool
	closed   int
}

func (s *fakeSession) Load(_ context.Context, m engine.Media) error {
	s.calls = append(s.calls, "Load")
	s.media = m
	return nil
}
func (s *fakeSession) Play(context.Context) error  { s.calls = append(s.calls, "Play"); return nil }
func (s *fakeSession) Pause(context.Context) error { s.calls = append(s.calls, "Pause"); return nil }
func (s *fakeSession) Stop(context.Context) error  { s.calls = append(s.calls, "Stop"); return nil }
func (s *fakeSession) Seek(_ context.Context, p time.Duration) error {
	s.calls = append(s.calls, "Seek")
	s.seekedTo = p
	return nil
}
func (s *fakeSession) Status(context.Context) (engine.State, error) { return s.st, nil }
func (s *fakeSession) HasVolume() bool                              { return s.hasVol }
func (s *fakeSession) Volume(context.Context) (int, error)          { return 0, nil }
func (s *fakeSession) SetVolume(context.Context, int) error         { return nil }
func (s *fakeSession) Mute(context.Context, bool) error             { return nil }
func (s *fakeSession) Close() error                                 { s.closed++; return nil }

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

// --- device resolution ---

func TestSelectDeviceSole(t *testing.T) {
	a := New(Options{Finder: fakeFinder{devices: []engine.Device{device("TV", "10.0.0.5:1234")}}, Store: &fakeStore{}})
	d, err := a.selectDevice(context.Background(), "", "")
	if err != nil {
		t.Fatal(err)
	}
	if d.Name() != "TV" {
		t.Fatalf("got %q", d.Name())
	}
}

func TestSelectDevicePrefersSaved(t *testing.T) {
	devs := []engine.Device{device("A", "10.0.0.5:1"), device("B", "10.0.0.9:2")}
	a := New(Options{Finder: fakeFinder{devices: devs}, Store: &fakeStore{cfg: config.Config{LastDeviceHost: "10.0.0.9"}}})
	d, err := a.selectDevice(context.Background(), "", "")
	if err != nil {
		t.Fatal(err)
	}
	if d.Name() != "B" {
		t.Fatalf("expected saved device B, got %q", d.Name())
	}
}

// Two engines can expose the same host (a TV that speaks both). A saved engine
// disambiguates; an old config without one still matches by host alone.
func TestSelectDevicePrefersSavedEngine(t *testing.T) {
	dl := device("TV (dlna)", "10.0.0.5:1")
	gc := device("TV (cast)", "10.0.0.5:8009")
	gc.eng = "googlecast"
	a := New(Options{
		Finder: fakeFinder{devices: []engine.Device{dl, gc}},
		Store:  &fakeStore{cfg: config.Config{LastDeviceHost: "10.0.0.5", LastDeviceEngine: "googlecast"}},
	})
	d, err := a.selectDevice(context.Background(), "", "")
	if err != nil {
		t.Fatal(err)
	}
	if d.Engine() != "googlecast" {
		t.Fatalf("expected the saved engine's device, got %s", d.Engine())
	}
}

func TestSelectDeviceMultipleNoSavedErrors(t *testing.T) {
	devs := []engine.Device{device("A", "10.0.0.5:1"), device("B", "10.0.0.9:2")}
	a := New(Options{Finder: fakeFinder{devices: devs}, Store: &fakeStore{}})
	if _, err := a.selectDevice(context.Background(), "", ""); err == nil {
		t.Fatal("expected ambiguity error")
	}
}

func TestSelectDeviceByHostIgnoresPort(t *testing.T) {
	devs := []engine.Device{device("TV", "10.0.0.5:1570")}
	a := New(Options{Finder: fakeFinder{devices: devs}, Store: &fakeStore{}})
	// Target carries a stale port; match must be by host IP.
	d, err := a.selectDevice(context.Background(), "10.0.0.5:9999", "")
	if err != nil {
		t.Fatal(err)
	}
	if d.Name() != "TV" {
		t.Fatalf("got %q", d.Name())
	}
}

func TestSelectTargetFallsBackToResolve(t *testing.T) {
	// Discovery finds nothing; the engines' Resolve paths (unicast probe, direct
	// connection) must recover the explicit target.
	tv := device("TV", "10.0.0.5:1653")
	a := New(Options{Finder: fakeFinder{resolved: tv}, Store: &fakeStore{}})
	d, err := a.selectDevice(context.Background(), "10.0.0.5", "")
	if err != nil {
		t.Fatal(err)
	}
	if d.Name() != "TV" {
		t.Fatalf("got %q", d.Name())
	}
}

func TestWaitForDeviceRecoversSavedHostViaProbe(t *testing.T) {
	// The --resume case: a saved TV that broadcast discovery keeps missing is
	// recovered by resolving the saved host directly.
	tv := device("TV", "10.0.0.5:1653")
	a := New(Options{
		Finder: fakeFinder{resolved: tv},
		Store:  &fakeStore{cfg: config.Config{LastDeviceHost: "10.0.0.5"}},
	})
	d, err := a.selectDevice(context.Background(), "", "")
	if err != nil {
		t.Fatal(err)
	}
	if d.Name() != "TV" {
		t.Fatalf("got %q", d.Name())
	}
}

// seqFinder returns a different result per Discover call, simulating a renderer
// that misses the first search burst and only answers a later one.
type seqFinder struct {
	results []struct {
		devices []engine.Device
		err     error
	}
	n int
}

func (f *seqFinder) Discover(context.Context, string) ([]engine.Device, error) {
	r := f.results[min(f.n, len(f.results)-1)]
	f.n++
	return r.devices, r.err
}
func (f *seqFinder) Resolve(context.Context, string, string) (engine.Device, error) {
	return nil, context.Canceled
}

func TestSelectDeviceFallbackFindsLateRenderer(t *testing.T) {
	f := &seqFinder{results: []struct {
		devices []engine.Device
		err     error
	}{
		{nil, nil}, // quick pass: nothing
		{[]engine.Device{device("TV", "10.0.0.5:1234")}, nil}, // deep pass: found
	}}
	var events []string
	a := New(Options{Finder: f, Store: &fakeStore{}, OnEvent: func(e Event) { events = append(events, e.Message) }})
	d, err := a.selectDevice(context.Background(), "", "")
	if err != nil {
		t.Fatal(err)
	}
	if d.Name() != "TV" {
		t.Fatalf("got %q", d.Name())
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
	sess := &fakeSession{}
	s := &fakeServer{}
	built := []time.Duration{}
	c := &Cast{
		sess: sess,
		srv:  s,
		buildArgs: func(ss time.Duration) []string {
			built = append(built, ss)
			return []string{"-ss", ss.String()}
		},
	}
	if err := c.Seek(context.Background(), 90*time.Second); err != nil {
		t.Fatal(err)
	}
	// The engine session gets a fresh Load (which stops/settles internally) and
	// a Play; the server got a fresh transcode at the seek offset.
	want := []string{"Load", "Play"}
	if strings.Join(sess.calls, ",") != strings.Join(want, ",") {
		t.Fatalf("session calls = %v, want %v", sess.calls, want)
	}
	if len(s.transcodeArgs) != 1 || len(built) != 1 || built[0] != 90*time.Second {
		t.Fatalf("transcode not relaunched at 90s: built=%v", built)
	}
	if c.ssOffset != 90*time.Second {
		t.Fatalf("ssOffset = %v, want 90s", c.ssOffset)
	}
	// The reloaded media must carry the fresh stream URL.
	if !strings.Contains(sess.media.URL, "http://test/media.mp4") {
		t.Fatalf("loaded media URL = %q", sess.media.URL)
	}
}

// While the async start handshake is in flight (starting == true), the
// read/control methods must short-circuit without touching the session, so the
// TUI can render and poll without contending with the in-flight Load/Play.
func TestStartingGatesControls(t *testing.T) {
	sess := &fakeSession{st: engine.State{Player: engine.StatePlaying, Pos: 42 * time.Second, Dur: 5 * time.Second}}
	c := &Cast{sess: sess, srv: &fakeServer{}, knownDuration: 3600 * time.Second}
	c.starting.Store(true)

	pos, dur, err := c.Position(context.Background())
	if err != nil || pos != 0 || dur != 3600*time.Second {
		t.Fatalf("Position while starting = (%v,%v,%v), want (0, 3600s, nil)", pos, dur, err)
	}
	if st, _ := c.PlayerState(context.Background()); st != engine.StateBuffering {
		t.Fatalf("PlayerState while starting = %v, want StateBuffering", st)
	}
	if err := c.Play(context.Background()); err != nil {
		t.Fatalf("Play while starting: %v", err)
	}
	if err := c.Seek(context.Background(), 30*time.Second); err != nil {
		t.Fatalf("Seek while starting: %v", err)
	}
	if len(sess.calls) != 0 {
		t.Fatalf("expected no session calls while starting, got %v", sess.calls)
	}

	// Once started, the same methods hit the session again.
	c.starting.Store(false)
	if st, _ := c.PlayerState(context.Background()); st != engine.StatePlaying {
		t.Fatalf("PlayerState after start = %v, want StatePlaying", st)
	}
}

func TestSeekDirectPlayNative(t *testing.T) {
	sess := &fakeSession{}
	c := &Cast{sess: sess, srv: &fakeServer{}} // buildArgs nil => direct-play
	if err := c.Seek(context.Background(), 30*time.Second); err != nil {
		t.Fatal(err)
	}
	if len(sess.calls) != 1 || sess.calls[0] != "Seek" || sess.seekedTo != 30*time.Second {
		t.Fatalf("expected native Seek to 30s, got calls=%v to=%v", sess.calls, sess.seekedTo)
	}
}

func TestPositionTranscodeAddsOffset(t *testing.T) {
	sess := &fakeSession{st: engine.State{Pos: 10 * time.Second, Dur: 5 * time.Second}}
	c := &Cast{
		sess:          sess,
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

// Position and PlayerState back to back must cost one session Status, not two:
// the cache is what keeps the per-poll device traffic the same as before.
func TestStatusCacheCoalescesPolls(t *testing.T) {
	sess := &statusCountingSession{fakeSession: fakeSession{st: engine.State{Player: engine.StatePlaying, Pos: time.Second, Dur: time.Minute}}}
	c := &Cast{sess: sess, srv: &fakeServer{}}
	if _, _, err := c.Position(context.Background()); err != nil {
		t.Fatal(err)
	}
	if _, err := c.PlayerState(context.Background()); err != nil {
		t.Fatal(err)
	}
	if sess.statusCalls != 1 {
		t.Fatalf("status calls = %d, want 1 (cached)", sess.statusCalls)
	}
}

type statusCountingSession struct {
	fakeSession
	statusCalls int
}

func (s *statusCountingSession) Status(ctx context.Context) (engine.State, error) {
	s.statusCalls++
	return s.fakeSession.Status(ctx)
}

// --- subtitle application + events ---

func TestApplySubtitlesBurn(t *testing.T) {
	s := &fakeServer{}
	media := &engine.Media{Seekable: true, MIME: "video/x-matroska"}
	track := probe.SubTrack{SubIndex: 1, Codec: "hdmv_pgs_subtitle", Kind: probe.SubBitmap}
	dec := subs.Decision{Kind: subs.Burn, Track: &track}

	label, build, _, err := applySubtitles(s, media, "/x/movie.mkv", t.TempDir(), dec, 0, 0)
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
}

func TestApplySubtitlesSidecar(t *testing.T) {
	s := &fakeServer{}
	media := &engine.Media{}
	dec := subs.Decision{Kind: subs.SoftSidecar, Format: engine.SubFormatSRT, SidecarPath: "/x/movie.srt"}

	label, build, _, err := applySubtitles(s, media, "/x/movie.mkv", t.TempDir(), dec, 0, 0)
	if err != nil {
		t.Fatal(err)
	}
	if build != nil {
		t.Fatal("sidecar must not transcode")
	}
	if media.Subtitle == nil || media.Subtitle.URL == "" || media.Subtitle.Format != engine.SubFormatSRT {
		t.Fatalf("expected soft sub set, got %+v", media.Subtitle)
	}
	if label != "soft: movie.srt" {
		t.Fatalf("label = %q", label)
	}
	if s.subPath != "/x/movie.srt" {
		t.Fatalf("server sub path = %q", s.subPath)
	}
}

func TestApplySubtitlesNone(t *testing.T) {
	s := &fakeServer{}
	media := &engine.Media{Seekable: true}
	label, build, _, err := applySubtitles(s, media, "/x/movie.mkv", t.TempDir(), subs.Decision{Kind: subs.None}, 0, 0)
	if err != nil || label != "" || build != nil {
		t.Fatalf("None: label=%q build!=nil=%v err=%v", label, build != nil, err)
	}
	if len(s.calls) != 0 {
		t.Fatalf("None must not touch the server, got %v", s.calls)
	}
}

// buildDelivery must be idempotent across mode flips: each call re-establishes a
// direct-play baseline before applying, so soft -> burn -> off -> soft leaves the
// server source and media fields consistent every time.
func TestBuildDeliveryIdempotentAcrossModes(t *testing.T) {
	a := New(Options{})
	s := &fakeServer{}
	media := &engine.Media{}
	info := &probe.MediaInfo{
		Subtitles: []probe.SubTrack{{SubIndex: 0, Codec: "hdmv_pgs_subtitle", Kind: probe.SubBitmap}},
	}
	soft := SubtitleOptions{Sidecar: "/x/movie.srt", TrackIndex: -1}
	burn := SubtitleOptions{TrackIndex: 0}
	off := SubtitleOptions{NoSubs: true, TrackIndex: -1}

	assertSoft := func() {
		_, build, _, err := a.buildDelivery(s, media, "/x/movie.mkv", t.TempDir(), info, soft, false, 0, defaultProfile)
		if err != nil {
			t.Fatal(err)
		}
		if build != nil {
			t.Fatal("soft must direct-play (build nil)")
		}
		if media.Subtitle == nil || media.Subtitle.Format != engine.SubFormatSRT {
			t.Fatalf("soft: subtitle = %+v", media.Subtitle)
		}
		if !media.Seekable || media.MIME != "video/x-matroska" {
			t.Fatalf("soft: seekable=%v mime=%q (want original)", media.Seekable, media.MIME)
		}
	}

	assertSoft()

	// soft -> burn: media flips to non-seekable mp4 with subs cleared, build set.
	_, build, _, err := a.buildDelivery(s, media, "/x/movie.mkv", t.TempDir(), info, burn, false, 30*time.Second, defaultProfile)
	if err != nil {
		t.Fatal(err)
	}
	if build == nil {
		t.Fatal("burn must transcode (build set)")
	}
	if media.Subtitle != nil {
		t.Fatalf("burn must clear the soft subtitle, got %+v", media.Subtitle)
	}
	if media.Seekable || media.MIME != "video/mp4" {
		t.Fatalf("burn: seekable=%v mime=%q", media.Seekable, media.MIME)
	}

	// burn -> off: back to a clean direct-play baseline, no subs, no transcode.
	_, build, _, err = a.buildDelivery(s, media, "/x/movie.mkv", t.TempDir(), info, off, false, 0, defaultProfile)
	if err != nil {
		t.Fatal(err)
	}
	if build != nil {
		t.Fatal("off must direct-play (build nil)")
	}
	if media.Subtitle != nil {
		t.Fatalf("off must clear the subtitle, got %+v", media.Subtitle)
	}
	if !media.Seekable || media.MIME != "video/x-matroska" {
		t.Fatalf("off: seekable=%v mime=%q (want original)", media.Seekable, media.MIME)
	}

	// off -> soft: returns to the soft state cleanly.
	assertSoft()
}

func TestSetSubtitleDirectPlayChoice(t *testing.T) {
	sess := &fakeSession{}
	s := &fakeServer{}
	a := New(Options{})
	info := &probe.MediaInfo{
		Subtitles: []probe.SubTrack{{SubIndex: 0, Codec: "hdmv_pgs_subtitle", Kind: probe.SubBitmap}},
	}
	choices := buildSubChoices(info, "/x/movie.srt") // [sidecar, track s:0 (bitmap), off]
	c := &Cast{
		sess: sess, srv: s, app: a, abs: "/x/movie.mkv", profile: defaultProfile,
		info: info, subChoices: choices, lastPos: 50 * time.Second,
		buildArgs: func(time.Duration) []string { return nil }, // start in some transcode mode
		ssOffset:  10 * time.Second, activeSub: 1,
	}

	// Switch to the sidecar (index 0): soft, direct-play.
	if err := c.SetSubtitle(context.Background(), 0); err != nil {
		t.Fatal(err)
	}
	want := []string{"Load", "Play", "Seek"}
	if strings.Join(sess.calls, ",") != strings.Join(want, ",") {
		t.Fatalf("calls = %v, want %v", sess.calls, want)
	}
	if sess.seekedTo != 50*time.Second {
		t.Fatalf("resumed at %v, want 50s", sess.seekedTo)
	}
	if c.buildArgs != nil {
		t.Fatal("direct-play choice must clear buildArgs")
	}
	if c.ssOffset != 0 {
		t.Fatalf("ssOffset = %v, want 0 for direct-play", c.ssOffset)
	}
	if c.activeSub != 0 || c.subInfo != "soft: movie.srt" {
		t.Fatalf("activeSub=%d subInfo=%q", c.activeSub, c.subInfo)
	}
}

// An engine that honors StartAt gets the resume position baked into the Load
// instead of a post-Play seek.
func TestSetSubtitleStartAtEngine(t *testing.T) {
	sess := &fakeSession{}
	s := &fakeServer{}
	a := New(Options{})
	prof := defaultProfile
	prof.HonorsStartAt = true
	choices := buildSubChoices(nil, "/x/movie.srt")
	c := &Cast{
		sess: sess, srv: s, app: a, abs: "/x/movie.mkv", profile: prof,
		subChoices: choices, lastPos: 50 * time.Second,
	}
	if err := c.SetSubtitle(context.Background(), 0); err != nil {
		t.Fatal(err)
	}
	want := []string{"Load", "Play"} // no Seek: StartAt does it
	if strings.Join(sess.calls, ",") != strings.Join(want, ",") {
		t.Fatalf("calls = %v, want %v", sess.calls, want)
	}
	if sess.media.StartAt != 50*time.Second {
		t.Fatalf("StartAt = %v, want 50s", sess.media.StartAt)
	}
}

func TestSetSubtitleTranscodeChoice(t *testing.T) {
	sess := &fakeSession{}
	s := &fakeServer{}
	a := New(Options{})
	info := &probe.MediaInfo{
		Subtitles: []probe.SubTrack{{SubIndex: 0, Codec: "hdmv_pgs_subtitle", Kind: probe.SubBitmap}},
	}
	choices := buildSubChoices(info, "/x/movie.srt") // [sidecar, track s:0 (bitmap), off]
	c := &Cast{
		sess: sess, srv: s, app: a, abs: "/x/movie.mkv", profile: defaultProfile,
		info: info, subChoices: choices, lastPos: 50 * time.Second,
	}

	// Switch to the bitmap track (index 1): burn-in, transcode at the current pos.
	if err := c.SetSubtitle(context.Background(), 1); err != nil {
		t.Fatal(err)
	}
	want := []string{"Load", "Play"} // no native Seek in transcode mode
	if strings.Join(sess.calls, ",") != strings.Join(want, ",") {
		t.Fatalf("calls = %v, want %v", sess.calls, want)
	}
	if c.buildArgs == nil {
		t.Fatal("burn choice must set buildArgs")
	}
	if c.ssOffset != 50*time.Second {
		t.Fatalf("ssOffset = %v, want 50s", c.ssOffset)
	}
	if c.activeSub != 1 || !strings.Contains(c.subInfo, "burn-in: track 0") {
		t.Fatalf("activeSub=%d subInfo=%q", c.activeSub, c.subInfo)
	}
	if len(s.transcodeArgs) != 1 {
		t.Fatalf("expected one SetTranscode, got %d", len(s.transcodeArgs))
	}
}

func TestSubTrackLabelLanguageName(t *testing.T) {
	cases := []struct {
		t    probe.SubTrack
		want string
	}{
		{probe.SubTrack{SubIndex: 0, Language: "eng", Codec: "subrip", Kind: probe.SubText},
			`Track s:0 — English (subrip, text)`},
		{probe.SubTrack{SubIndex: 1, Language: "en", Title: "SDH", Codec: "subrip", Kind: probe.SubText},
			`Track s:1 — English "SDH" (subrip, text)`},
		{probe.SubTrack{SubIndex: 2, Language: "pt", Codec: "ass", Kind: probe.SubText, Default: true},
			`Track s:2 — Portuguese (ass, text) (default)`},
		{probe.SubTrack{SubIndex: 3, Language: "qaa", Codec: "subrip", Kind: probe.SubText}, // unknown code passes through
			`Track s:3 — qaa (subrip, text)`},
		{probe.SubTrack{SubIndex: 4, Codec: "hdmv_pgs_subtitle", Kind: probe.SubBitmap}, // no language
			`Track s:4 (hdmv_pgs_subtitle, bitmap)`},
	}
	for _, c := range cases {
		if got := subTrackLabel(c.t); got != c.want {
			t.Errorf("subTrackLabel = %q, want %q", got, c.want)
		}
	}
}

func TestSetSubtitleOutOfRange(t *testing.T) {
	c := &Cast{subChoices: []subChoice{{label: "Off"}}}
	if err := c.SetSubtitle(context.Background(), 5); err == nil {
		t.Fatal("expected out-of-range error")
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
	sess := &fakeSession{}
	c := &Cast{sess: sess, srv: &fakeServer{}, resume: fr, resumePath: "/x/movie.mkv", knownDuration: 3600 * time.Second, lastPos: 100 * time.Second}
	_ = c.Close(context.Background())
	if fr.got["/x/movie.mkv"] != 100*time.Second {
		t.Fatalf("saved = %v, want 100s", fr.got["/x/movie.mkv"])
	}
	if sess.closed != 1 {
		t.Fatalf("session closed %d times, want 1", sess.closed)
	}
}

func TestCloseClearsWhenFinished(t *testing.T) {
	fr := &fakeResume{}
	c := &Cast{sess: &fakeSession{}, srv: &fakeServer{}, resume: fr, resumePath: "/x/movie.mkv", knownDuration: 3600 * time.Second, lastPos: 3595 * time.Second}
	_ = c.Close(context.Background())
	if len(fr.cleared) != 1 || fr.cleared[0] != "/x/movie.mkv" {
		t.Fatalf("expected clear of finished file, got cleared=%v set=%v", fr.cleared, fr.got)
	}
}

func TestCloseSkipsTinyPosition(t *testing.T) {
	fr := &fakeResume{}
	c := &Cast{sess: &fakeSession{}, srv: &fakeServer{}, resume: fr, resumePath: "/x/movie.mkv", knownDuration: 3600 * time.Second, lastPos: 2 * time.Second}
	_ = c.Close(context.Background())
	if len(fr.got) != 0 || len(fr.cleared) != 0 {
		t.Fatalf("tiny position should neither set nor clear: set=%v cleared=%v", fr.got, fr.cleared)
	}
}

func TestPositionCachesLastPos(t *testing.T) {
	sess := &fakeSession{st: engine.State{Pos: 30 * time.Second, Dur: 100 * time.Second}}
	c := &Cast{sess: sess, srv: &fakeServer{}} // direct-play
	if _, _, err := c.Position(context.Background()); err != nil {
		t.Fatal(err)
	}
	if c.lastPos != 30*time.Second {
		t.Fatalf("lastPos = %v, want 30s", c.lastPos)
	}
}

func TestCodecPlan(t *testing.T) {
	// av1 video is outside the webOS list -> transcode video only.
	p := codecPlan(&probe.MediaInfo{VideoCodec: "av1", AudioCodec: "aac"}, defaultProfile, false)
	if p.Kind != TranscodeCodec || !p.Video || p.Audio {
		t.Fatalf("av1/aac: %+v", p)
	}
	// All good -> none.
	p = codecPlan(&probe.MediaInfo{VideoCodec: "h264", AudioCodec: "aac"}, defaultProfile, false)
	if p.Kind != TranscodeNone {
		t.Fatalf("h264/aac should not transcode: %+v", p)
	}
	// Forced -> both.
	p = codecPlan(&probe.MediaInfo{VideoCodec: "h264", AudioCodec: "aac"}, defaultProfile, true)
	if p.Kind != TranscodeCodec || !p.Video || !p.Audio {
		t.Fatalf("forced: %+v", p)
	}
	// Container mismatch alone (a Cast-style profile) -> remux.
	castish := defaultProfile
	castish.Containers = codecSet("mp4", "webm")
	p = codecPlan(&probe.MediaInfo{Container: "matroska", VideoCodec: "h264", AudioCodec: "aac"}, castish, false)
	if p.Kind != TranscodeRemux || !p.Remux || p.Video || p.Audio {
		t.Fatalf("mkv/h264/aac on a container-limited profile: %+v", p)
	}
}

// Cast satisfies the TUI's control surface (compile-time check mirrors tui.Controller).
var _ interface {
	Play(context.Context) error
	Pause(context.Context) error
	Stop(context.Context) error
	Seek(context.Context, time.Duration) error
	Position(context.Context) (time.Duration, time.Duration, error)
	PlayerState(context.Context) (engine.PlayerState, error)
	HasVolume() bool
	Volume(context.Context) (int, error)
	SetVolume(context.Context, int) error
	Mute(context.Context, bool) error
} = (*Cast)(nil)

func TestSetEventSinkRoutesEvents(t *testing.T) {
	a := New(Options{OnEvent: func(Event) { t.Fatal("event reached the replaced sink") }})
	var got []string
	a.SetEventSink(func(e Event) { got = append(got, e.Message) })
	a.emit(Info, "hello %s", "tv")
	if len(got) != 1 || got[0] != "hello tv" {
		t.Fatalf("routed events = %v, want [hello tv]", got)
	}
}

// A transcoded stream is rebased to start at 0, so soft subs riding along with a
// codec transcode must be re-cut from the same offset — otherwise every cue runs
// `ss` late, which looks exactly like "no subtitles". Direct-play keeps the file's
// absolute timeline and must NOT be shifted.
func TestSoftSubOffsetFollowsTranscode(t *testing.T) {
	sidecar := filepath.Join(t.TempDir(), "movie.srt")
	if err := os.WriteFile(sidecar, []byte("1\n00:02:00,000 --> 00:02:02,000\nhi\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	soft := SubtitleOptions{Sidecar: sidecar, TrackIndex: -1}

	// av1 => codec transcode; the sidecar must be shifted by the 85s start offset.
	a := New(Options{})
	s := &fakeServer{}
	media := &engine.Media{}
	_, build, resoft, err := a.buildDelivery(s, media, "/x/movie.mkv", t.TempDir(),
		&probe.MediaInfo{VideoCodec: "av1", AudioCodec: "eac3"}, soft, false, 85*time.Second, defaultProfile)
	if err != nil {
		t.Fatal(err)
	}
	if build == nil {
		t.Fatal("av1 must transcode")
	}
	if resoft == nil {
		t.Fatal("soft subs on a transcode must expose a rebase hook for seeks")
	}
	if s.subPath == sidecar {
		t.Error("transcode: sidecar served unshifted, cues would run 85s late")
	}

	// h264 => direct-play; the sidecar must be served as-is.
	s2 := &fakeServer{}
	_, build2, _, err := a.buildDelivery(s2, &engine.Media{}, "/x/movie.mkv", t.TempDir(),
		&probe.MediaInfo{VideoCodec: "h264", AudioCodec: "eac3"}, soft, false, 85*time.Second, defaultProfile)
	if err != nil {
		t.Fatal(err)
	}
	if build2 != nil {
		t.Fatal("h264 must direct-play")
	}
	if s2.subPath != sidecar {
		t.Errorf("direct-play must serve the sidecar unshifted, got %q", s2.subPath)
	}
}
