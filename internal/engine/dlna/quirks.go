package dlna

import (
	"context"
	"strings"
	"time"
)

// This file holds the webOS quirk handling that used to sit in core. TVs
// serialize UPnP control and are flaky mid-transition: webOS reports
// LG_TRANSITIONING for a beat or two after a Stop *and* while it buffers a
// freshly-set URI, and while there it rejects both SetAVTransportURI and Play
// with 701 "Transition not available" (verified: a fake URL yields 716, not
// 701, so 701 is purely a transport-state problem). The cure is to poll the
// state until it settles — a fixed sleep fires too early and re-wedges the TV.

// waitTransportSettled blocks until the renderer reports a non-transitioning
// transport state (or the budget/ctx elapses). Best-effort: a read error or
// timeout just returns, letting the retrying caller proceed.
func (s *session) waitTransportSettled(ctx context.Context, budget time.Duration) {
	wctx, cancel := context.WithTimeout(ctx, budget)
	defer cancel()
	for {
		sctx, scancel := context.WithTimeout(wctx, 2*time.Second)
		state, _, _, err := s.av.GetTransportInfoCtx(sctx, 0)
		scancel()
		if err == nil && !strings.Contains(state, "TRANSITIONING") {
			return
		}
		if !sleepCtx(wctx, 300*time.Millisecond) {
			return
		}
	}
}

// playingAnyway reports whether the TV ended up playing despite a failed Play.
// webOS auto-plays a freshly set URI, and a large direct-play file can keep it
// buffering long enough that every Play is rejected with 701 while the stream
// starts on its own.
func (s *session) playingAnyway(ctx context.Context) bool {
	s.waitTransportSettled(ctx, 10*time.Second)
	sctx, cancel := context.WithTimeout(ctx, 2*time.Second)
	defer cancel()
	state, _, _, err := s.av.GetTransportInfoCtx(sctx, 0)
	return err == nil && state == "PLAYING"
}

// retrySOAP runs fn up to attempts times, each with its own per-call timeout,
// backing off briefly between tries. It stops early if the parent ctx is done.
// The policy (attempt counts, budgets) is DLNA-specific residual-flakiness
// cover; the settle waits above do the real work.
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
