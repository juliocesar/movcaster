package dlna

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/juliocesar/movcaster/internal/engine"
)

// fakeAV models a webOS TV's AVTransport, including the flaky-mid-transition
// behavior the quirk handling exists for.
type fakeAV struct {
	calls    []string
	uri      string
	meta     string
	seekedTo string
	state    string

	// setURIErrs is consumed front-to-back, one entry per SetAVTransportURI
	// call, to simulate a TV that rejects the URI until its state clears.
	setURIErrs []error

	// stateSeq is consumed front-to-back by GetTransportInfo (falling back to
	// state), to simulate a TV that lingers in TRANSITIONING after a Stop.
	stateSeq   []string
	stateCalls int

	// transitioningAfterSet models a TV that, once it has the URI, sits in
	// TRANSITIONING (buffering a slow deep-offset transcode) for this many
	// state polls before settling. While transitioning, Play is rejected —
	// reproducing the 701 on the --resume path.
	transitioningAfterSet int
	setURICount           int
	pollsAfterSet         int
}

func (f *fakeAV) transitioning() bool {
	return f.setURICount > 0 && f.pollsAfterSet < f.transitioningAfterSet
}

func (f *fakeAV) SetAVTransportURICtx(_ context.Context, _ uint32, uri, meta string) error {
	f.calls = append(f.calls, "SetURI")
	f.uri, f.meta = uri, meta
	f.setURICount++
	if len(f.setURIErrs) > 0 {
		err := f.setURIErrs[0]
		f.setURIErrs = f.setURIErrs[1:]
		return err
	}
	return nil
}

func (f *fakeAV) PlayCtx(context.Context, uint32, string) error {
	f.calls = append(f.calls, "Play")
	if f.transitioning() {
		return errors.New("701 Transition not available")
	}
	return nil
}
func (f *fakeAV) PauseCtx(context.Context, uint32) error { f.calls = append(f.calls, "Pause"); return nil }
func (f *fakeAV) StopCtx(context.Context, uint32) error  { f.calls = append(f.calls, "Stop"); return nil }
func (f *fakeAV) SeekCtx(_ context.Context, _ uint32, _, target string) error {
	f.calls = append(f.calls, "Seek")
	f.seekedTo = target
	return nil
}
func (f *fakeAV) GetTransportInfoCtx(context.Context, uint32) (string, string, string, error) {
	f.stateCalls++
	if f.transitioning() {
		f.pollsAfterSet++
		return "LG_TRANSITIONING", "OK", "1", nil
	}
	if len(f.stateSeq) > 0 {
		s := f.stateSeq[0]
		f.stateSeq = f.stateSeq[1:]
		return s, "OK", "1", nil
	}
	return f.state, "OK", "1", nil
}
func (f *fakeAV) GetPositionInfoCtx(context.Context, uint32) (uint32, string, string, string, string, string, int32, int32, error) {
	return 0, "1:00:00", "", "", "0:00:42", "", 0, 0, nil
}

// A fresh Load must clear the TV's transport state (Stop) before pointing it at
// new media — the sequence that avoids 701 "Transition not available".
func TestLoadStopsThenSetsURI(t *testing.T) {
	av := &fakeAV{}
	s := &session{av: av}
	if err := s.Load(context.Background(), engine.Media{URL: "http://test/media.mp4"}); err != nil {
		t.Fatal(err)
	}
	want := []string{"Stop", "SetURI"}
	if strings.Join(av.calls, ",") != strings.Join(want, ",") {
		t.Fatalf("calls = %v, want %v", av.calls, want)
	}
	if av.uri != "http://test/media.mp4" || !strings.Contains(av.meta, "DIDL-Lite") {
		t.Fatalf("uri=%q meta=%q", av.uri, av.meta)
	}
}

// A transient failure on the first SetAVTransportURI must be retried, not
// surfaced as a hard failure.
func TestLoadRetriesTransientSetURI(t *testing.T) {
	av := &fakeAV{setURIErrs: []error{context.DeadlineExceeded}}
	s := &session{av: av}
	if err := s.Load(context.Background(), engine.Media{}); err != nil {
		t.Fatalf("expected retry to recover, got %v", err)
	}
	want := []string{"Stop", "SetURI", "SetURI"}
	if strings.Join(av.calls, ",") != strings.Join(want, ",") {
		t.Fatalf("calls = %v, want %v", av.calls, want)
	}
}

// Load must not issue SetAVTransportURI until the TV has left the transitioning
// state — firing too early is what triggers 701. It should poll the transport
// state past the LG_TRANSITIONING readings before setting the URI.
func TestLoadWaitsForTransitionToSettle(t *testing.T) {
	av := &fakeAV{stateSeq: []string{"LG_TRANSITIONING", "LG_TRANSITIONING", "STOPPED"}}
	s := &session{av: av}
	if err := s.Load(context.Background(), engine.Media{}); err != nil {
		t.Fatal(err)
	}
	if av.stateCalls < 3 {
		t.Fatalf("expected to poll the transport state until settled, polled %d time(s)", av.stateCalls)
	}
}

// Regression for the --resume 701: after SetAVTransportURI the TV buffers the
// freshly-set URI and rejects Play with 701 while it reports LG_TRANSITIONING.
// A live transcode resumed deep into the file lingers there well past bare Play
// retries, so Play must wait for the transport to settle first.
func TestPlayWaitsOutPostURITransition(t *testing.T) {
	av := &fakeAV{transitioningAfterSet: 3}
	s := &session{av: av}
	if err := s.Load(context.Background(), engine.Media{URL: "http://test/media.mp4"}); err != nil {
		t.Fatal(err)
	}
	if err := s.Play(context.Background()); err != nil {
		t.Fatalf("expected Play to wait out the post-URI transition, got %v", err)
	}
	want := []string{"Stop", "SetURI", "Play"}
	if strings.Join(av.calls, ",") != strings.Join(want, ",") {
		t.Fatalf("calls = %v, want %v", av.calls, want)
	}
}

func TestSeekSeekableUsesRelTime(t *testing.T) {
	av := &fakeAV{}
	s := &session{av: av}
	if err := s.Load(context.Background(), engine.Media{URL: "u", Seekable: true}); err != nil {
		t.Fatal(err)
	}
	if err := s.Seek(context.Background(), 90*time.Second); err != nil {
		t.Fatal(err)
	}
	if av.seekedTo != "0:01:30" {
		t.Fatalf("seeked to %q, want 0:01:30", av.seekedTo)
	}
}

func TestStatusSnapshot(t *testing.T) {
	av := &fakeAV{state: "PLAYING"}
	s := &session{av: av}
	st, err := s.Status(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if st.Player != engine.StatePlaying || st.Native != "PLAYING" {
		t.Fatalf("state = %+v", st)
	}
	if st.Pos != 42*time.Second || st.Dur != time.Hour {
		t.Fatalf("pos/dur = %v/%v", st.Pos, st.Dur)
	}
}

// A device without RenderingControl reports no volume and errors cleanly.
func TestNoVolumeControl(t *testing.T) {
	s := &session{av: &fakeAV{}}
	if s.HasVolume() {
		t.Fatal("HasVolume must be false without RenderingControl")
	}
	if _, err := s.Volume(context.Background()); err != engine.ErrNoVolume {
		t.Fatalf("Volume err = %v, want ErrNoVolume", err)
	}
}
