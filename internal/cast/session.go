// Package cast ties a renderer and media server into a single controller that the
// TUI drives. It hides the difference between direct-play (native byte-range seek)
// and transcode (seek by relaunching ffmpeg at a new -ss offset, the seek-restart
// approach), and reports a sane position/duration in both modes.
package cast

import (
	"context"
	"sync"
	"time"

	"github.com/juliocesar/movcaster/internal/mediaserver"
	"github.com/juliocesar/movcaster/internal/renderer"
)

// Session is a single active cast. It satisfies tui.Controller.
type Session struct {
	r   *renderer.Renderer
	srv *mediaserver.Server

	knownDuration time.Duration

	// Transcode mode: buildArgs(ss) yields the ffmpeg args to start playback at
	// absolute offset ss. nil => direct-play.
	buildArgs func(ss time.Duration) []string
	media     renderer.Media

	mu       sync.Mutex
	ssOffset time.Duration // absolute offset the current transcode segment starts at

	seekMu sync.Mutex // serializes seek-restarts so two never interleave
}

// Config configures a session.
type Config struct {
	Renderer      *renderer.Renderer
	Server        *mediaserver.Server
	Media         renderer.Media
	KnownDuration time.Duration
	// BuildTranscodeArgs is non-nil for transcode casts (burn-in / codec fallback).
	BuildTranscodeArgs func(ss time.Duration) []string
}

// New builds a session.
func New(c Config) *Session {
	return &Session{
		r:             c.Renderer,
		srv:           c.Server,
		media:         c.Media,
		knownDuration: c.KnownDuration,
		buildArgs:     c.BuildTranscodeArgs,
	}
}

func (s *Session) transcoding() bool { return s.buildArgs != nil }

func (s *Session) Play(ctx context.Context) error  { return s.r.Play(ctx) }
func (s *Session) Pause(ctx context.Context) error { return s.r.Pause(ctx) }
func (s *Session) Stop(ctx context.Context) error  { return s.r.Stop(ctx) }
func (s *Session) TransportState(ctx context.Context) (string, error) {
	return s.r.TransportState(ctx)
}
func (s *Session) HasVolume() bool                            { return s.r.HasVolume() }
func (s *Session) Volume(ctx context.Context) (int, error)    { return s.r.Volume(ctx) }
func (s *Session) SetVolume(ctx context.Context, v int) error { return s.r.SetVolume(ctx, v) }
func (s *Session) Mute(ctx context.Context, on bool) error    { return s.r.Mute(ctx, on) }

// Position reports absolute position and total duration. In transcode mode the
// TV's reported position is segment-relative, so we add the segment offset and
// substitute the probed duration (a fragmented stream has no total duration).
func (s *Session) Position(ctx context.Context) (pos, dur time.Duration, err error) {
	pos, dur, err = s.r.Position(ctx)
	if !s.transcoding() {
		return pos, dur, err
	}
	s.mu.Lock()
	off := s.ssOffset
	s.mu.Unlock()
	abs := off + pos
	if s.knownDuration > 0 {
		dur = s.knownDuration
	}
	if abs > dur && dur > 0 {
		abs = dur
	}
	return abs, dur, err
}

// Seek jumps to an absolute position. Direct-play uses native AVTransport Seek;
// transcode relaunches ffmpeg at the new offset and re-points the TV at the fresh
// stream (Stop -> settle -> SetAVTransportURI -> Play). The control sequence is
// serialized and each SOAP step is retried, because TVs are flaky mid-transition.
func (s *Session) Seek(ctx context.Context, pos time.Duration) error {
	if !s.transcoding() {
		return s.r.Seek(ctx, pos)
	}
	if pos < 0 {
		pos = 0
	}

	// One restart at a time; a queued seek runs to the latest target afterward.
	s.seekMu.Lock()
	defer s.seekMu.Unlock()

	// Stop the current stream (best-effort, short timeout) and let the TV settle
	// before we hand it a new URI; swapping while it's still tearing down the old
	// stream is what makes SetAVTransportURI hang.
	stopCtx, cancel := context.WithTimeout(ctx, 4*time.Second)
	_ = s.r.Stop(stopCtx)
	cancel()
	if !sleepCtx(ctx, 500*time.Millisecond) {
		return ctx.Err()
	}

	s.srv.SetTranscode(s.buildArgs(pos))
	s.media.URL = s.srv.MediaURL()

	if err := retrySOAP(ctx, 3, 9*time.Second, func(c context.Context) error {
		return s.r.SetMedia(c, s.media)
	}); err != nil {
		return err
	}
	if err := retrySOAP(ctx, 3, 7*time.Second, func(c context.Context) error {
		return s.r.Play(c)
	}); err != nil {
		return err
	}

	s.mu.Lock()
	s.ssOffset = pos
	s.mu.Unlock()
	return nil
}

// retrySOAP runs fn up to attempts times, each with its own per-call timeout,
// backing off briefly between tries. It stops early if the parent ctx is done.
func retrySOAP(parent context.Context, attempts int, perCall time.Duration, fn func(context.Context) error) error {
	var err error
	for i := 0; i < attempts; i++ {
		if parent.Err() != nil {
			return parent.Err()
		}
		c, cancel := context.WithTimeout(parent, perCall)
		err = fn(c)
		cancel()
		if err == nil {
			return nil
		}
		if !sleepCtx(parent, 400*time.Millisecond) {
			return parent.Err()
		}
	}
	return err
}

// sleepCtx sleeps d, returning false if ctx is cancelled first.
func sleepCtx(ctx context.Context, d time.Duration) bool {
	t := time.NewTimer(d)
	defer t.Stop()
	select {
	case <-ctx.Done():
		return false
	case <-t.C:
		return true
	}
}
