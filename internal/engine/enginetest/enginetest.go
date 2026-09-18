// Package enginetest is the shared conformance suite every engine's package
// test runs. A new engine that passes Run is wired correctly, whatever protocol
// it speaks — it asserts the parts of the contract that are easy to get wrong.
package enginetest

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/juliocesar/movcaster/internal/engine"
)

// Run exercises the Engine contract against a live or faked engine. dev may be
// nil when no device (real or fake) is reachable; the session checks are then
// skipped and only the engine-level guarantees are asserted.
func Run(t *testing.T, e engine.Engine, dev engine.Device) {
	t.Helper()

	t.Run("identity", func(t *testing.T) {
		if e.Name() == "" {
			t.Fatal("engine must have a stable Name")
		}
		p := e.DefaultProfile()
		if p.Name == "" {
			t.Error("the default profile should carry a display Name")
		}
	})

	t.Run("discover respects cancellation", func(t *testing.T) {
		ctx, cancel := context.WithCancel(context.Background())
		cancel()
		start := time.Now()
		_, _ = e.Discover(ctx)
		if d := time.Since(start); d > 3*time.Second {
			t.Fatalf("Discover took %v with a cancelled context", d)
		}
	})

	t.Run("resolve foreign target", func(t *testing.T) {
		// A device-description URL on TEST-NET is nobody's valid target; every
		// engine must answer ErrNotThisEngine (not a generic error) promptly so
		// the registry can move on to the next engine.
		ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
		defer cancel()
		_, err := e.Resolve(ctx, "http://192.0.2.1:1/desc.xml")
		if !errors.Is(err, engine.ErrNotThisEngine) {
			t.Fatalf("Resolve(foreign) = %v, want ErrNotThisEngine", err)
		}
	})

	if dev == nil {
		t.Log("no device supplied: session conformance skipped")
		return
	}

	t.Run("device identity", func(t *testing.T) {
		if dev.Engine() != e.Name() {
			t.Fatalf("device Engine() = %q, engine Name() = %q", dev.Engine(), e.Name())
		}
		if dev.Host() == "" {
			t.Fatal("device must expose a bare host")
		}
	})

	t.Run("session", func(t *testing.T) {
		ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
		defer cancel()
		sess, err := dev.Open(ctx)
		if err != nil {
			t.Fatalf("Open: %v", err)
		}

		// Load the fast half: it must return, accepted, without waiting for
		// playback (the fake/live device may not fetch the URL at all).
		media := engine.Media{
			URL: "http://127.0.0.1:1/media.mp4?t=1", Title: "conformance",
			MIME: "video/mp4", Duration: time.Minute, Seekable: false,
		}
		start := time.Now()
		if err := sess.Load(ctx, media); err != nil {
			t.Fatalf("Load: %v", err)
		}
		if d := time.Since(start); d > 10*time.Second {
			t.Fatalf("Load blocked %v; it must return once the device accepts", d)
		}

		// Play must be idempotent: twice in a row cannot fail.
		if err := sess.Play(ctx); err != nil {
			t.Fatalf("Play: %v", err)
		}
		if err := sess.Play(ctx); err != nil {
			t.Fatalf("second Play: %v", err)
		}

		// The loaded media was not Seekable: Seek must refuse with ErrNotSeekable.
		if err := sess.Seek(ctx, 10*time.Second); !errors.Is(err, engine.ErrNotSeekable) {
			t.Fatalf("Seek(non-seekable) = %v, want ErrNotSeekable", err)
		}

		// Status on a loaded device must never be StateUnknown.
		st, err := sess.Status(ctx)
		if err != nil {
			t.Fatalf("Status: %v", err)
		}
		if st.Player == engine.StateUnknown {
			t.Fatal("Status returned StateUnknown for a loaded device")
		}

		// Volume round-trips within 2 at the extremes.
		if sess.HasVolume() {
			for _, v := range []int{0, 100} {
				if err := sess.SetVolume(ctx, v); err != nil {
					t.Fatalf("SetVolume(%d): %v", v, err)
				}
				got, err := sess.Volume(ctx)
				if err != nil {
					t.Fatalf("Volume: %v", err)
				}
				if got < v-2 || got > v+2 {
					t.Fatalf("volume round-trip %d -> %d", v, got)
				}
			}
		}

		// Close twice is safe.
		if err := sess.Close(); err != nil {
			t.Fatalf("Close: %v", err)
		}
		if err := sess.Close(); err != nil {
			t.Fatalf("second Close: %v", err)
		}
	})
}
