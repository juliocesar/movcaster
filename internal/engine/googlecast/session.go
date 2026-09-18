package googlecast

import (
	"context"
	"encoding/json"
	"fmt"
	"sync"
	"time"

	"github.com/juliocesar/movcaster/internal/engine"
)

// mediaStatus is the interesting slice of one MEDIA_STATUS entry.
type mediaStatus struct {
	MediaSessionID int     `json:"mediaSessionId"`
	PlayerState    string  `json:"playerState"` // IDLE/BUFFERING/PLAYING/PAUSED
	IdleReason     string  `json:"idleReason"`  // FINISHED/CANCELLED/ERROR/INTERRUPTED
	CurrentTime    float64 `json:"currentTime"`
	Media          *struct {
		Duration float64 `json:"duration"`
	} `json:"media"`
}

// session drives the Default Media Receiver over one TLS connection. Status is
// served from the receiver's pushed MEDIA_STATUS messages (with position
// extrapolation), refreshed with a GET_STATUS when the cache goes stale.
type session struct {
	c           *conn
	sessionID   string // receiver app session, for LOAD
	transportID string // destination for media-namespace messages

	mu             sync.Mutex
	mediaSessionID int
	seekable       bool          // the currently loaded media supports SEEK
	duration       time.Duration // from Load (a LIVE pipe reports none itself)
	last           mediaStatus   // most recent status
	lastAt         time.Time     // when it arrived (drives extrapolation)
	haveStatus     bool
}

// newSession dials the device and performs the establishment handshake:
// CONNECT to the platform receiver, launch (or adopt) the media app, then
// CONNECT to its transportId so media messages are routed to it.
func newSession(ctx context.Context, host string) (*session, error) {
	c, err := dial(ctx, host)
	if err != nil {
		return nil, err
	}
	s := &session{c: c}
	// Media status is pushed on state changes; keep the cache current whether or
	// not a request was waiting on it.
	c.handle(nsMedia, s.onMediaMessage)

	if err := c.connectPlatform(); err != nil {
		c.close(err)
		return nil, err
	}
	hctx, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()
	sessionID, transportID, err := c.ensureApp(hctx, defaultMediaAppID)
	if err != nil {
		c.close(err)
		return nil, err
	}
	if err := c.connectApp(transportID); err != nil {
		c.close(err)
		return nil, err
	}
	s.sessionID, s.transportID = sessionID, transportID
	return s, nil
}

// onMediaMessage keeps the status cache current from pushed MEDIA_STATUS.
func (s *session) onMediaMessage(_ string, payload json.RawMessage) {
	var msg struct {
		Type   string        `json:"type"`
		Status []mediaStatus `json:"status"`
	}
	if err := json.Unmarshal(payload, &msg); err != nil || msg.Type != "MEDIA_STATUS" || len(msg.Status) == 0 {
		return
	}
	s.mu.Lock()
	s.last = msg.Status[0]
	s.lastAt = time.Now()
	s.haveStatus = true
	if msg.Status[0].MediaSessionID != 0 {
		s.mediaSessionID = msg.Status[0].MediaSessionID
	}
	s.mu.Unlock()
}

// loadErrorText maps the receiver's LOAD failure codes to something readable.
// detailedErrorCode is the only useful debugging signal a failed LOAD gives.
func loadErrorText(code int) string {
	switch code {
	case 100:
		return "media unknown"
	case 101:
		return "media load failed (usually network or CORS)"
	case 104:
		return "source not supported"
	case 110:
		return "media decode error"
	case 905:
		return "load failed"
	default:
		return "load failed"
	}
}

// Load points the receiver at the media. It returns once the receiver has
// accepted the LOAD (the fast half); autoplay makes Play a near no-op.
func (s *session) Load(ctx context.Context, m engine.Media) error {
	media := map[string]any{
		"contentId":   m.URL,
		"contentType": m.MIME,
		// A fragmented-MP4 pipe is not range-fetchable: declaring it LIVE makes
		// the receiver hide its seek bar and refuse SEEK, which is honest —
		// seeking is driven from our side via seek-restart.
		"streamType": "BUFFERED",
		"metadata":   map[string]any{"metadataType": 1, "title": m.Title},
	}
	if !m.Seekable {
		media["streamType"] = "LIVE"
	}
	if m.Duration > 0 {
		media["duration"] = m.Duration.Seconds()
	}

	payload := map[string]any{
		"type":      "LOAD",
		"sessionId": s.sessionID,
		"media":     media,
		"autoplay":  true,
	}
	if m.StartAt > 0 {
		payload["currentTime"] = m.StartAt.Seconds()
	}
	if m.Subtitle != nil && m.Subtitle.URL != "" {
		media["tracks"] = []map[string]any{{
			"trackId":          1,
			"type":             "TEXT",
			"subtype":          "SUBTITLES",
			"trackContentId":   m.Subtitle.URL,
			"trackContentType": m.Subtitle.MIME,
			"language":         subLanguage(m.Subtitle),
			"name":             subName(m.Subtitle),
		}}
		// Omit activeTrackIds entirely when there is no subtitle: an empty array
		// is not the same as absence on some receiver versions.
		payload["activeTrackIds"] = []int{1}
	}

	lctx, cancel := context.WithTimeout(ctx, 15*time.Second)
	defer cancel()
	raw, err := s.c.request(lctx, s.transportID, nsMedia, payload)
	if err != nil {
		return fmt.Errorf("LOAD: %w", err)
	}
	var resp struct {
		Type              string        `json:"type"`
		DetailedErrorCode int           `json:"detailedErrorCode"`
		Status            []mediaStatus `json:"status"`
	}
	if err := json.Unmarshal(raw, &resp); err != nil {
		return fmt.Errorf("LOAD reply: %w", err)
	}
	switch resp.Type {
	case "MEDIA_STATUS":
		// Accepted; the push handler has already cached the status.
	case "LOAD_FAILED", "LOAD_CANCELLED", "INVALID_REQUEST":
		logf("LOAD failed: %s detailedErrorCode=%d", resp.Type, resp.DetailedErrorCode)
		return fmt.Errorf("receiver rejected the media: %s (code %d)", loadErrorText(resp.DetailedErrorCode), resp.DetailedErrorCode)
	default:
		return fmt.Errorf("unexpected LOAD reply %q", resp.Type)
	}

	s.mu.Lock()
	s.seekable = m.Seekable
	s.duration = m.Duration
	s.mu.Unlock()
	return nil
}

// subLanguage returns a usable track language (the receiver requires one).
func subLanguage(sub *engine.Subtitle) string {
	if sub.Language != "" {
		return sub.Language
	}
	return "en"
}

func subName(sub *engine.Subtitle) string {
	if sub.Label != "" {
		return sub.Label
	}
	return "Subtitles"
}

// mediaRequest sends a media-namespace request carrying the current media
// session id.
func (s *session) mediaRequest(ctx context.Context, typ string, extra map[string]any) error {
	s.mu.Lock()
	id := s.mediaSessionID
	s.mu.Unlock()
	if id == 0 {
		return nil // nothing loaded; every transport op is a no-op
	}
	payload := map[string]any{"type": typ, "mediaSessionId": id}
	for k, v := range extra {
		payload[k] = v
	}
	_, err := s.c.request(ctx, s.transportID, nsMedia, payload)
	return err
}

// Play is idempotent: autoplay already runs the media, so PLAY is sent only
// when the receiver reports PAUSED.
func (s *session) Play(ctx context.Context) error {
	st, err := s.Status(ctx)
	if err != nil {
		return err
	}
	if st.Player != engine.StatePaused {
		return nil
	}
	return s.mediaRequest(ctx, "PLAY", nil)
}

func (s *session) Pause(ctx context.Context) error { return s.mediaRequest(ctx, "PAUSE", nil) }

// Stop stops the media session. The app stays up (a receiver-namespace STOP
// would tear it down to the home screen); Close decides about the app.
func (s *session) Stop(ctx context.Context) error { return s.mediaRequest(ctx, "STOP", nil) }

// Seek jumps to an absolute position. LIVE (pipe) media refuses SEEK on the
// receiver, so the caller gets ErrNotSeekable and falls back to seek-restart.
func (s *session) Seek(ctx context.Context, pos time.Duration) error {
	s.mu.Lock()
	seekable := s.seekable
	s.mu.Unlock()
	if !seekable {
		return engine.ErrNotSeekable
	}
	return s.mediaRequest(ctx, "SEEK", map[string]any{"currentTime": pos.Seconds()})
}

// Status serves the last pushed MEDIA_STATUS, extrapolating the position from
// wall time while playing (the receiver pushes on state changes, not
// continuously). A cache older than 5s is refreshed with a GET_STATUS.
func (s *session) Status(ctx context.Context) (engine.State, error) {
	s.mu.Lock()
	stale := !s.haveStatus || time.Since(s.lastAt) > 5*time.Second
	s.mu.Unlock()
	if stale {
		gctx, cancel := context.WithTimeout(ctx, 5*time.Second)
		err := s.mediaRequest(gctx, "GET_STATUS", nil)
		cancel()
		if err != nil {
			return engine.State{}, err
		}
	}

	s.mu.Lock()
	defer s.mu.Unlock()
	if !s.haveStatus {
		return engine.State{Player: engine.StateIdle}, nil
	}
	st := engine.State{
		Player: mapPlayerState(s.last.PlayerState, s.last.IdleReason),
		Pos:    time.Duration(s.last.CurrentTime * float64(time.Second)),
		Native: nativeLabel(s.last.PlayerState, s.last.IdleReason),
	}
	if s.last.Media != nil && s.last.Media.Duration > 0 {
		st.Dur = time.Duration(s.last.Media.Duration * float64(time.Second))
	} else {
		st.Dur = s.duration
	}
	if st.Player == engine.StatePlaying {
		st.Pos += time.Since(s.lastAt)
	}
	if st.Dur > 0 && st.Pos > st.Dur {
		st.Pos = st.Dur
	}
	return st, nil
}

// mapPlayerState normalizes the receiver vocabulary. idleReason FINISHED is a
// clean end-of-media signal — better than DLNA's STOPPED inference — and drives
// auto-advance directly.
func mapPlayerState(playerState, idleReason string) engine.PlayerState {
	switch playerState {
	case "PLAYING":
		return engine.StatePlaying
	case "PAUSED":
		return engine.StatePaused
	case "BUFFERING", "LOADING":
		return engine.StateBuffering
	case "IDLE":
		if idleReason == "FINISHED" {
			return engine.StateFinished
		}
		return engine.StateIdle
	default:
		return engine.StateUnknown
	}
}

func nativeLabel(playerState, idleReason string) string {
	if idleReason != "" {
		return playerState + "/" + idleReason
	}
	return playerState
}

// HasVolume is always true: every Cast device exposes receiver volume.
func (s *session) HasVolume() bool { return true }

func (s *session) Volume(ctx context.Context) (int, error) {
	st, err := s.c.getReceiverStatus(ctx)
	if err != nil {
		return 0, err
	}
	return volumeToPercent(st.Status.Volume.Level), nil
}

func (s *session) SetVolume(ctx context.Context, v int) error {
	return s.c.setVolume(ctx, percentToVolume(v))
}

func (s *session) Mute(ctx context.Context, on bool) error { return s.c.setMuted(ctx, on) }

// Close tears down the virtual connections and the TLS link. It does NOT stop
// the receiver app: that would black the TV to its home screen, and core stops
// playback separately before closing.
func (s *session) Close() error {
	if s.transportID != "" {
		_ = s.c.send(s.transportID, nsConnection, map[string]any{"type": "CLOSE"})
	}
	_ = s.c.send(receiverID, nsConnection, map[string]any{"type": "CLOSE"})
	s.c.close(nil)
	return nil
}
