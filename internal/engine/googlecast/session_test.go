package googlecast

import (
	"context"
	"net"
	"strings"
	"testing"
	"time"

	"github.com/juliocesar/movcaster/internal/engine"
)

func startSession(t *testing.T) (*fakeReceiver, *session) {
	t.Helper()
	f := newFakeReceiver(t)
	s, err := newSession(context.Background(), f.addr())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = s.Close() })
	return f, s
}

// The establishment handshake: CONNECT to receiver-0, LAUNCH the app, then a
// second CONNECT to its transportId. Media messages before that second CONNECT
// silently vanish, so the ids must be adopted correctly.
func TestHandshakeLaunchesAndConnects(t *testing.T) {
	f, s := startSession(t)
	if s.sessionID != fakeSessionID || s.transportID != fakeTransportID {
		t.Fatalf("ids = %q/%q", s.sessionID, s.transportID)
	}
	// The app CONNECT is fire-and-forget, so give the fake a beat to see it.
	waitFor(t, func() bool {
		f.mu.Lock()
		defer f.mu.Unlock()
		return f.connectedTo[receiverID] && f.connectedTo[fakeTransportID] && f.launched
	}, "CONNECT receiver-0 + LAUNCH + CONNECT transportId")
}

// waitFor polls cond briefly; media CONNECTs are fire-and-forget frames.
func waitFor(t *testing.T, cond func() bool, what string) {
	t.Helper()
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		if cond() {
			return
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatalf("timed out waiting for %s", what)
}

// A second session must adopt the already-running app instead of relaunching
// (a relaunch blanks the TV screen).
func TestHandshakeReusesRunningApp(t *testing.T) {
	f, _ := startSession(t)
	s2, err := newSession(context.Background(), f.addr())
	if err != nil {
		t.Fatal(err)
	}
	defer s2.Close()
	if s2.transportID != fakeTransportID {
		t.Fatalf("second session transportId = %q", s2.transportID)
	}
}

// Media messages sent before the app CONNECT are dropped by a real receiver;
// the fake mimics that, proving the handshake ordering matters.
func TestMediaBeforeConnectIsDropped(t *testing.T) {
	f := newFakeReceiver(t)
	c, err := dial(context.Background(), f.addr())
	if err != nil {
		t.Fatal(err)
	}
	defer c.close(nil)
	_ = c.connectPlatform()
	ctx, cancel := context.WithTimeout(context.Background(), 300*time.Millisecond)
	defer cancel()
	_, err = c.request(ctx, fakeTransportID, nsMedia, map[string]any{"type": "GET_STATUS"})
	if err == nil {
		t.Fatal("expected the un-CONNECTed media request to time out")
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	if len(f.dropped) == 0 {
		t.Fatal("receiver did not drop the message")
	}
}

// The heartbeat: the fake receiver PINGs the client on its first CONNECT, and
// the client's read loop must answer PONG promptly (a sender that goes quiet is
// dropped within roughly 10 seconds).
func TestHeartbeatAnswersPing(t *testing.T) {
	f := newFakeReceiver(t)
	c, err := dial(context.Background(), f.addr())
	if err != nil {
		t.Fatal(err)
	}
	defer c.close(nil)
	if err := c.connectPlatform(); err != nil {
		t.Fatal(err)
	}
	select {
	case <-f.gotPong:
	case <-time.After(2 * time.Second):
		t.Fatal("client did not PONG the receiver's PING")
	}
}

// LOAD payload shape: BUFFERED for seekable direct-play, LIVE for a pipe,
// currentTime for StartAt, and the subtitle track block with activeTrackIds.
func TestLoadPayloadShape(t *testing.T) {
	f, s := startSession(t)

	// Direct-play with a subtitle and a resume offset.
	err := s.Load(context.Background(), engine.Media{
		URL: "http://h/media.mp4?t=3", Title: "Some Movie", MIME: "video/mp4",
		Duration: 7241 * time.Second, Seekable: true, StartAt: 90 * time.Second,
		Subtitle: &engine.Subtitle{URL: "http://h/subs.vtt?t=1", Format: engine.SubFormatVTT, MIME: "text/vtt", Language: "en", Label: "English"},
	})
	if err != nil {
		t.Fatal(err)
	}
	// A LIVE pipe without subs.
	err = s.Load(context.Background(), engine.Media{
		URL: "http://h/media.mp4?t=4", Title: "Pipe", MIME: "video/mp4", Seekable: false,
	})
	if err != nil {
		t.Fatal(err)
	}

	f.mu.Lock()
	defer f.mu.Unlock()
	if len(f.loads) != 2 {
		t.Fatalf("loads = %d", len(f.loads))
	}
	direct := f.loads[0]
	media := direct["media"].(map[string]any)
	if media["streamType"] != "BUFFERED" {
		t.Fatalf("direct-play streamType = %v", media["streamType"])
	}
	if direct["currentTime"].(float64) != 90 {
		t.Fatalf("currentTime = %v", direct["currentTime"])
	}
	if direct["autoplay"] != true || direct["sessionId"] != fakeSessionID {
		t.Fatalf("autoplay/sessionId = %v/%v", direct["autoplay"], direct["sessionId"])
	}
	if media["duration"].(float64) != 7241 {
		t.Fatalf("duration = %v", media["duration"])
	}
	tracks := media["tracks"].([]any)
	track := tracks[0].(map[string]any)
	if track["trackContentId"] != "http://h/subs.vtt?t=1" || track["trackContentType"] != "text/vtt" ||
		track["type"] != "TEXT" || track["subtype"] != "SUBTITLES" || track["language"] != "en" {
		t.Fatalf("track = %v", track)
	}
	if ids := direct["activeTrackIds"].([]any); len(ids) != 1 || ids[0].(float64) != 1 {
		t.Fatalf("activeTrackIds = %v", direct["activeTrackIds"])
	}

	pipe := f.loads[1]
	pmedia := pipe["media"].(map[string]any)
	if pmedia["streamType"] != "LIVE" {
		t.Fatalf("pipe streamType = %v", pmedia["streamType"])
	}
	if _, has := pipe["activeTrackIds"]; has {
		t.Fatal("activeTrackIds must be omitted entirely without a subtitle")
	}
	if _, has := pipe["currentTime"]; has {
		t.Fatal("currentTime must be omitted at offset 0")
	}
}

// LOAD_FAILED surfaces the detailedErrorCode in a readable error.
func TestLoadFailedSurfacesCode(t *testing.T) {
	f, s := startSession(t)
	f.mu.Lock()
	f.loadFailCode = 104
	f.mu.Unlock()
	err := s.Load(context.Background(), engine.Media{URL: "http://h/m.mkv", MIME: "video/x-matroska"})
	if err == nil {
		t.Fatal("expected LOAD_FAILED")
	}
	if !strings.Contains(err.Error(), "104") || !strings.Contains(err.Error(), "source not supported") {
		t.Fatalf("err = %v", err)
	}
}

// MEDIA_STATUS to engine.State mapping, including idleReason FINISHED.
func TestStateMapping(t *testing.T) {
	cases := []struct {
		player, idle string
		want         engine.PlayerState
	}{
		{"IDLE", "FINISHED", engine.StateFinished},
		{"IDLE", "", engine.StateIdle},
		{"IDLE", "CANCELLED", engine.StateIdle},
		{"BUFFERING", "", engine.StateBuffering},
		{"PLAYING", "", engine.StatePlaying},
		{"PAUSED", "", engine.StatePaused},
	}
	for _, c := range cases {
		if got := mapPlayerState(c.player, c.idle); got != c.want {
			t.Errorf("mapPlayerState(%s,%s) = %v, want %v", c.player, c.idle, got, c.want)
		}
	}
}

// Status extrapolates the position from wall time while playing, because the
// receiver pushes on state changes rather than continuously.
func TestStatusExtrapolatesPosition(t *testing.T) {
	_, s := startSession(t)
	s.mu.Lock()
	s.last = mediaStatus{MediaSessionID: 7, PlayerState: "PLAYING", CurrentTime: 100}
	s.lastAt = time.Now().Add(-2 * time.Second)
	s.haveStatus = true
	s.duration = 3600 * time.Second
	s.mu.Unlock()

	st, err := s.Status(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if st.Player != engine.StatePlaying {
		t.Fatalf("state = %v", st.Player)
	}
	if st.Pos < 101*time.Second || st.Pos > 104*time.Second {
		t.Fatalf("pos = %v, want ~102s (100s + 2s wall time)", st.Pos)
	}
	if st.Dur != 3600*time.Second {
		t.Fatalf("dur = %v", st.Dur)
	}

	// Paused: no extrapolation.
	s.mu.Lock()
	s.last.PlayerState = "PAUSED"
	s.lastAt = time.Now().Add(-2 * time.Second)
	s.mu.Unlock()
	st, _ = s.Status(context.Background())
	if st.Pos != 100*time.Second {
		t.Fatalf("paused pos = %v, want exactly 100s", st.Pos)
	}
}

// Play is idempotent: PLAY goes out only when the receiver reports PAUSED.
func TestPlayIdempotent(t *testing.T) {
	f, s := startSession(t)
	s.mu.Lock()
	s.mediaSessionID = 7
	s.last = mediaStatus{MediaSessionID: 7, PlayerState: "PLAYING"}
	s.lastAt = time.Now()
	s.haveStatus = true
	s.mu.Unlock()

	if err := s.Play(context.Background()); err != nil {
		t.Fatal(err)
	}
	f.mu.Lock()
	calls := strings.Join(f.mediaCalls, ",")
	f.mu.Unlock()
	if strings.Contains(calls, "PLAY") {
		t.Fatalf("PLAY sent while already playing: %s", calls)
	}

	s.mu.Lock()
	s.last.PlayerState = "PAUSED"
	s.lastAt = time.Now()
	s.mu.Unlock()
	if err := s.Play(context.Background()); err != nil {
		t.Fatal(err)
	}
	f.mu.Lock()
	calls = strings.Join(f.mediaCalls, ",")
	f.mu.Unlock()
	if !strings.Contains(calls, "PLAY") {
		t.Fatalf("PLAY not sent while paused: %s", calls)
	}
}

// Seek on LIVE (pipe) media returns ErrNotSeekable so core falls back to
// seek-restart; on BUFFERED media it sends SEEK with currentTime.
func TestSeek(t *testing.T) {
	f, s := startSession(t)
	if err := s.Load(context.Background(), engine.Media{URL: "u", MIME: "video/mp4", Seekable: false}); err != nil {
		t.Fatal(err)
	}
	if err := s.Seek(context.Background(), 30*time.Second); err != engine.ErrNotSeekable {
		t.Fatalf("LIVE seek err = %v, want ErrNotSeekable", err)
	}
	if err := s.Load(context.Background(), engine.Media{URL: "u2", MIME: "video/mp4", Seekable: true}); err != nil {
		t.Fatal(err)
	}
	if err := s.Seek(context.Background(), 30*time.Second); err != nil {
		t.Fatal(err)
	}
	f.mu.Lock()
	calls := strings.Join(f.mediaCalls, ",")
	f.mu.Unlock()
	if !strings.Contains(calls, "SEEK") {
		t.Fatalf("no SEEK sent: %s", calls)
	}
}

// Volume scaling round-trips 0-100 through the receiver's 0.0-1.0.
func TestVolumeRoundTrip(t *testing.T) {
	_, s := startSession(t)
	for _, v := range []int{0, 37, 100} {
		if err := s.SetVolume(context.Background(), v); err != nil {
			t.Fatal(err)
		}
		got, err := s.Volume(context.Background())
		if err != nil {
			t.Fatal(err)
		}
		if got != v {
			t.Fatalf("volume round-trip %d -> %d", v, got)
		}
	}
	if err := s.Mute(context.Background(), true); err != nil {
		t.Fatal(err)
	}
	if !s.HasVolume() {
		t.Fatal("Cast devices always have volume")
	}
}

// The wire codec round-trips a message.
func TestCastMessageRoundTrip(t *testing.T) {
	in := &castMessage{
		protocolVersion: castV2_1_0,
		sourceID:        "sender-x",
		destinationID:   "receiver-0",
		namespace:       nsMedia,
		payloadType:     payloadTypeString,
		payloadUTF8:     `{"type":"LOAD","requestId":3}`,
	}
	var buf strings.Builder
	if err := writeFrame(&buf, in); err != nil {
		t.Fatal(err)
	}
	out, err := readFrame(strings.NewReader(buf.String()))
	if err != nil {
		t.Fatal(err)
	}
	if out.protocolVersion != in.protocolVersion || out.sourceID != in.sourceID ||
		out.destinationID != in.destinationID || out.namespace != in.namespace ||
		out.payloadType != in.payloadType || out.payloadUTF8 != in.payloadUTF8 {
		t.Fatalf("round trip mismatch:\n in=%+v\nout=%+v", in, out)
	}
}

// Resolve identifies a Cast device by dialing it directly (no mDNS involved)
// and refuses non-Cast targets with ErrNotThisEngine.
func TestResolve(t *testing.T) {
	f := newFakeReceiver(t)
	e := New()
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()

	d, err := e.Resolve(ctx, f.addr())
	if err != nil {
		t.Fatalf("resolve against the fake receiver: %v", err)
	}
	if d.Engine() != "googlecast" || d.Addr() != f.addr() {
		t.Fatalf("device = %s/%s", d.Engine(), d.Addr())
	}

	host, _, _ := net.SplitHostPort(f.addr())
	if _, err := e.Resolve(ctx, host+":1"); err != engine.ErrNotThisEngine {
		t.Fatalf("dead target err = %v, want ErrNotThisEngine", err)
	}
	if _, err := e.Resolve(ctx, "http://10.0.0.5:1122/desc.xml"); err != engine.ErrNotThisEngine {
		t.Fatalf("URL target err = %v, want ErrNotThisEngine", err)
	}
}
