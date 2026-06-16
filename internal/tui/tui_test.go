package tui

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/charmbracelet/bubbles/progress"
	tea "github.com/charmbracelet/bubbletea"
)

// fakeCtrl records Seek calls and satisfies Controller.
type fakeCtrl struct {
	seeks []time.Duration
}

func (f *fakeCtrl) Play(context.Context) error  { return nil }
func (f *fakeCtrl) Pause(context.Context) error { return nil }
func (f *fakeCtrl) Stop(context.Context) error  { return nil }
func (f *fakeCtrl) Seek(_ context.Context, d time.Duration) error {
	f.seeks = append(f.seeks, d)
	return nil
}
func (f *fakeCtrl) Position(context.Context) (time.Duration, time.Duration, error) {
	return 0, 0, nil
}
func (f *fakeCtrl) TransportState(context.Context) (string, error) { return "PLAYING", nil }
func (f *fakeCtrl) HasVolume() bool                                { return true }
func (f *fakeCtrl) Volume(context.Context) (int, error)            { return 0, nil }
func (f *fakeCtrl) SetVolume(context.Context, int) error           { return nil }
func (f *fakeCtrl) Mute(context.Context, bool) error               { return nil }

func TestSeekDebounce(t *testing.T) {
	f := &fakeCtrl{}
	m := model{ctrl: f, pos: 10 * time.Second, dur: 100 * time.Second, prog: newTestProgress()}

	// Two quick right presses: target advances 10s each, no seek issued yet.
	right := tea.KeyMsg{Type: tea.KeyRight}
	mi, _ := m.Update(right)
	m = mi.(model)
	mi, _ = m.Update(right)
	m = mi.(model)

	if got := f.seeks; len(got) != 0 {
		t.Fatalf("seek issued during debounce window: %v", got)
	}
	if m.pos != 30*time.Second {
		t.Fatalf("pending target = %v, want 30s", m.pos)
	}
	if !m.seeking || m.seekGen != 2 {
		t.Fatalf("seeking=%v gen=%d, want true/2", m.seeking, m.seekGen)
	}

	// A stale timer (gen 1) must not fire a seek.
	mi, cmd := m.Update(seekFireMsg{gen: 1})
	m = mi.(model)
	if cmd != nil {
		t.Fatal("stale seekFireMsg returned a command")
	}

	// A position poll during seeking must not overwrite the target.
	mi, _ = m.Update(posMsg{pos: 5 * time.Second, dur: 100 * time.Second, state: "PLAYING"})
	m = mi.(model)
	if m.pos != 30*time.Second {
		t.Fatalf("poll overwrote pending target: %v", m.pos)
	}

	// The current timer (gen 2) fires the real seek.
	mi, cmd = m.Update(seekFireMsg{gen: 2})
	m = mi.(model)
	if cmd == nil {
		t.Fatal("current seekFireMsg returned no command")
	}
	done := cmd() // executes the seek
	if len(f.seeks) != 1 || f.seeks[0] != 30*time.Second {
		t.Fatalf("seek calls = %v, want [30s]", f.seeks)
	}

	// seekDoneMsg clears the seeking flag.
	mi, _ = m.Update(done)
	m = mi.(model)
	if m.seeking {
		t.Fatal("seeking flag not cleared after seekDoneMsg")
	}
}

func newTestProgress() progress.Model {
	return progress.New(progress.WithWidth(20))
}

func TestEndOfMediaAdvances(t *testing.T) {
	m := model{ctrl: &fakeCtrl{}, prog: newTestProgress()}

	// Play through to near the end.
	mi, _ := m.Update(posMsg{pos: 90 * time.Second, dur: 100 * time.Second, state: "PLAYING"})
	m = mi.(model)
	if !m.everPlayed || m.maxProgress != 90*time.Second {
		t.Fatalf("everPlayed=%v maxProgress=%v", m.everPlayed, m.maxProgress)
	}

	// The TV stops near the end (and may reset its reported position to 0).
	mi, cmd := m.Update(posMsg{pos: 0, dur: 100 * time.Second, state: "STOPPED"})
	m = mi.(model)
	if cmd == nil {
		t.Fatal("end-of-media did not return a quit command")
	}
	if m.outcome != OutcomeEnded || !m.quitting {
		t.Fatalf("outcome=%v quitting=%v, want Ended/true", m.outcome, m.quitting)
	}
}

func TestStoppedBeforePlayingDoesNotEnd(t *testing.T) {
	m := model{ctrl: &fakeCtrl{}, prog: newTestProgress()}
	// A STOPPED state before anything ever played (startup) must not advance.
	mi, cmd := m.Update(posMsg{pos: 0, dur: 100 * time.Second, state: "STOPPED"})
	m = mi.(model)
	if cmd != nil || m.outcome != OutcomeQuit || m.quitting {
		t.Fatalf("startup STOPPED ended prematurely: outcome=%v quitting=%v", m.outcome, m.quitting)
	}
}

func TestEarlyStopDoesNotEnd(t *testing.T) {
	m := model{ctrl: &fakeCtrl{}, prog: newTestProgress()}
	// Played only a little, then stopped far from the end: not a natural end.
	mi, _ := m.Update(posMsg{pos: 10 * time.Second, dur: 100 * time.Second, state: "PLAYING"})
	m = mi.(model)
	mi, cmd := m.Update(posMsg{pos: 10 * time.Second, dur: 100 * time.Second, state: "STOPPED"})
	m = mi.(model)
	if cmd != nil || m.outcome != OutcomeQuit {
		t.Fatalf("early stop wrongly treated as end: outcome=%v", m.outcome)
	}
}

func TestNextKey(t *testing.T) {
	m := model{ctrl: &fakeCtrl{}, prog: newTestProgress()}
	mi, cmd := m.Update(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune{'n'}})
	m = mi.(model)
	if cmd == nil {
		t.Fatal("n key returned no command")
	}
	if m.outcome != OutcomeNext || !m.quitting {
		t.Fatalf("outcome=%v quitting=%v, want Next/true", m.outcome, m.quitting)
	}
}

func TestViewRendersState(t *testing.T) {
	m := model{
		title:   "Movie.mkv",
		device:  "TV",
		subInfo: "burn-in: track 0",
		state:   "PLAYING",
		pos:     90 * time.Second,
		dur:     2700 * time.Second,
		volume:  42,
		hasVol:  true,
		width:   80,
		prog:    newTestProgress(),
	}
	out := m.View()
	for _, want := range []string{"Movie.mkv", "TV", "PLAYING", "01:30", "45:00", "42%", "burn-in"} {
		if !strings.Contains(out, want) {
			t.Errorf("View() missing %q\n%s", want, out)
		}
	}
}

func TestFmtDur(t *testing.T) {
	cases := map[time.Duration]string{
		0:                  "00:00",
		90 * time.Second:   "01:30",
		3661 * time.Second: "1:01:01",
		-5 * time.Second:   "00:00",
	}
	for in, want := range cases {
		if got := fmtDur(in); got != want {
			t.Errorf("fmtDur(%v) = %q, want %q", in, got, want)
		}
	}
}
