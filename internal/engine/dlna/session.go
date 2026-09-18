package dlna

import (
	"context"
	"fmt"
	"strings"
	"sync"
	"time"

	"github.com/huin/goupnp/dcps/av1"
	"github.com/juliocesar/movcaster/internal/engine"
)

// avTransport and renderingControl are the slices of the goupnp av1 clients the
// session uses. They exist so the quirk handling (settle waits, 701 retries) is
// testable against a fake TV without SOAP.
type avTransport interface {
	SetAVTransportURICtx(ctx context.Context, instanceID uint32, uri, meta string) error
	PlayCtx(ctx context.Context, instanceID uint32, speed string) error
	PauseCtx(ctx context.Context, instanceID uint32) error
	StopCtx(ctx context.Context, instanceID uint32) error
	SeekCtx(ctx context.Context, instanceID uint32, unit, target string) error
	GetTransportInfoCtx(ctx context.Context, instanceID uint32) (state, status, speed string, err error)
	GetPositionInfoCtx(ctx context.Context, instanceID uint32) (track uint32, dur, meta, uri, rel, abs string, relCount, absCount int32, err error)
}

type renderingControl interface {
	GetVolumeCtx(ctx context.Context, instanceID uint32, channel string) (uint16, error)
	SetVolumeCtx(ctx context.Context, instanceID uint32, channel string, v uint16) error
	SetMuteCtx(ctx context.Context, instanceID uint32, channel string, on bool) error
}

var (
	_ avTransport      = (*av1.AVTransport1)(nil)
	_ renderingControl = (*av1.RenderingControl1)(nil)
)

// session drives one renderer over AVTransport (transport control) and
// RenderingControl (volume). InstanceID is always 0, channel "Master". SOAP is
// stateless, so the session holds no connection; Close is a no-op.
type session struct {
	av avTransport
	rc renderingControl // nil when the device exposes no RenderingControl

	mu       sync.Mutex
	seekable bool // whether the currently loaded media supports native Seek
}

// Load is the fast half of playback startup: best-effort Stop, wait for the TV
// to leave the transitioning state left by a previous cast, then
// SetAVTransportURI with DIDL-Lite metadata. It returns as soon as the TV has
// accepted the URI — at which point webOS begins fetching/auto-playing the
// stream — so the caller can bring up the UI without waiting on the (slow) Play
// handshake. webOS rejects a new URI with 701 "Transition not available" while
// it reports LG_TRANSITIONING, so the state is polled rather than slept on
// (firing too early is exactly what triggers the 701, and re-wedges the TV).
// Media.StartAt is ignored: DLNA has no load-time offset, the caller seeks
// after Play (Profile.HonorsStartAt is false).
func (s *session) Load(ctx context.Context, m engine.Media) error {
	stopCtx, cancel := context.WithTimeout(ctx, 4*time.Second)
	_ = s.av.StopCtx(stopCtx, 0)
	cancel()
	s.waitTransportSettled(ctx, 6*time.Second)

	meta := buildDIDL(m)
	if err := retrySOAP(ctx, 3, 9*time.Second, func(ic context.Context) error {
		return s.av.SetAVTransportURICtx(ic, 0, m.URL, meta)
	}); err != nil {
		return fmt.Errorf("SetAVTransportURI: %w", err)
	}
	s.mu.Lock()
	s.seekable = m.Seekable
	s.mu.Unlock()
	return nil
}

// Play is the slow half: wait for the TV to finish buffering the freshly-set
// URI (it rejects Play with 701 while TRANSITIONING), then Play. For a large
// direct-play file or a deep-offset transcode this wait runs into the seconds,
// which is why callers run it off the critical path. A fresh/offset-0 stream
// settles almost immediately, but a live transcode resumed deep into the file
// needs several seconds to -ss-seek and emit its first MP4 fragment, so the TV
// lingers in TRANSITIONING long past bare Play retries.
func (s *session) Play(ctx context.Context) error {
	s.waitTransportSettled(ctx, 20*time.Second)
	if err := retrySOAP(ctx, 3, 7*time.Second, func(ic context.Context) error {
		return s.av.PlayCtx(ic, 0, "1")
	}); err != nil {
		return fmt.Errorf("Play: %w", err)
	}
	return nil
}

func (s *session) Pause(ctx context.Context) error { return s.av.PauseCtx(ctx, 0) }
func (s *session) Stop(ctx context.Context) error  { return s.av.StopCtx(ctx, 0) }

// Seek jumps to an absolute position using REL_TIME. The TV may still be
// mid-transition (e.g. right after a resume Play), so the call is retried
// briefly rather than surfacing a transient 701/701-like failure.
func (s *session) Seek(ctx context.Context, pos time.Duration) error {
	s.mu.Lock()
	seekable := s.seekable
	s.mu.Unlock()
	if !seekable {
		return engine.ErrNotSeekable
	}
	return retrySOAP(ctx, 4, 3*time.Second, func(ic context.Context) error {
		return s.av.SeekCtx(ic, 0, "REL_TIME", formatDuration(pos))
	})
}

// Status is one snapshot: transport state plus position/duration. DLNA has no
// push channel, so both reads hit the TV.
func (s *session) Status(ctx context.Context) (engine.State, error) {
	native, _, _, err := s.av.GetTransportInfoCtx(ctx, 0)
	if err != nil {
		return engine.State{}, err
	}
	st := engine.State{Player: mapState(native), Native: native}
	if _, durStr, _, _, relStr, _, _, _, err := s.av.GetPositionInfoCtx(ctx, 0); err == nil {
		st.Pos, st.Dur = parseDuration(relStr), parseDuration(durStr)
	}
	return st, nil
}

// mapState normalizes the UPnP (and LG-flavored) transport vocabulary. DLNA has
// no explicit end-of-media signal: a natural end reports STOPPED (or
// NO_MEDIA_PRESENT once drained), so StateFinished is never produced here and
// the caller keeps its played-to-the-end inference.
func mapState(native string) engine.PlayerState {
	switch {
	case strings.Contains(native, "TRANSITIONING"): // TRANSITIONING, LG_TRANSITIONING
		return engine.StateBuffering
	case native == "PLAYING":
		return engine.StatePlaying
	case native == "PAUSED_PLAYBACK" || native == "PAUSED_RECORDING":
		return engine.StatePaused
	case native == "STOPPED":
		return engine.StateStopped
	case native == "NO_MEDIA_PRESENT":
		return engine.StateIdle
	default:
		return engine.StateUnknown
	}
}

// HasVolume reports whether the device exposes RenderingControl.
func (s *session) HasVolume() bool { return s.rc != nil }

// Volume returns the master volume (0-100).
func (s *session) Volume(ctx context.Context) (int, error) {
	if s.rc == nil {
		return 0, engine.ErrNoVolume
	}
	v, err := s.rc.GetVolumeCtx(ctx, 0, "Master")
	return int(v), err
}

// SetVolume sets the master volume (0-100, clamped).
func (s *session) SetVolume(ctx context.Context, v int) error {
	if s.rc == nil {
		return engine.ErrNoVolume
	}
	if v < 0 {
		v = 0
	}
	if v > 100 {
		v = 100
	}
	return s.rc.SetVolumeCtx(ctx, 0, "Master", uint16(v))
}

// Mute sets the master mute state.
func (s *session) Mute(ctx context.Context, on bool) error {
	if s.rc == nil {
		return engine.ErrNoVolume
	}
	return s.rc.SetMuteCtx(ctx, 0, "Master", on)
}

func (s *session) Close() error { return nil }

// formatDuration renders a duration as H:MM:SS (UPnP REL_TIME / duration format).
func formatDuration(d time.Duration) string {
	if d < 0 {
		d = 0
	}
	total := int(d / time.Second)
	h := total / 3600
	m := (total % 3600) / 60
	s := total % 60
	return fmt.Sprintf("%d:%02d:%02d", h, m, s)
}

// parseDuration parses "H:MM:SS" / "HH:MM:SS(.fff)" into a Duration. Returns 0 on failure.
func parseDuration(s string) time.Duration {
	s = strings.TrimSpace(s)
	if s == "" || s == "NOT_IMPLEMENTED" {
		return 0
	}
	if i := strings.IndexByte(s, '.'); i >= 0 {
		s = s[:i]
	}
	parts := strings.Split(s, ":")
	if len(parts) != 3 {
		return 0
	}
	var h, m, sec int
	if _, err := fmt.Sscanf(parts[0], "%d", &h); err != nil {
		return 0
	}
	if _, err := fmt.Sscanf(parts[1], "%d", &m); err != nil {
		return 0
	}
	if _, err := fmt.Sscanf(parts[2], "%d", &sec); err != nil {
		return 0
	}
	return time.Duration(h)*time.Hour + time.Duration(m)*time.Minute + time.Duration(sec)*time.Second
}
