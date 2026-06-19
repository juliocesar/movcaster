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

// fakeSubCtrl adds live subtitle switching to fakeCtrl (satisfies SubtitleController).
type fakeSubCtrl struct {
	fakeCtrl
	choices  []string
	active   int
	setCalls []int
	subInfo  string
}

func (f *fakeSubCtrl) SubtitleChoices() []string { return f.choices }
func (f *fakeSubCtrl) ActiveSubtitle() int       { return f.active }
func (f *fakeSubCtrl) SetSubtitle(_ context.Context, idx int) error {
	f.setCalls = append(f.setCalls, idx)
	return nil
}
func (f *fakeSubCtrl) SubInfo() string { return f.subInfo }

func keyRune(r rune) tea.KeyMsg { return tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune{r}} }

func TestSubMenuOpensOnActive(t *testing.T) {
	f := &fakeSubCtrl{choices: []string{"A", "B", "Off"}, active: 1}
	m := model{ctrl: f, prog: newTestProgress()}
	mi, _ := m.Update(keyRune('s'))
	m = mi.(model)
	if !m.subMenuOpen {
		t.Fatal("s did not open the menu")
	}
	if m.subCursor != 1 {
		t.Fatalf("cursor = %d, want active index 1", m.subCursor)
	}
	if len(m.subChoices) != 3 {
		t.Fatalf("choices = %v", m.subChoices)
	}
}

func TestSubMenuPlainControllerDoesNotOpen(t *testing.T) {
	// fakeCtrl is not a SubtitleController, so s must be a no-op.
	m := model{ctrl: &fakeCtrl{}, prog: newTestProgress()}
	mi, _ := m.Update(keyRune('s'))
	m = mi.(model)
	if m.subMenuOpen {
		t.Fatal("menu opened for a controller without subtitle support")
	}
}

func TestSubMenuNavigationBounds(t *testing.T) {
	f := &fakeSubCtrl{choices: []string{"A", "B", "Off"}, active: 0}
	m := model{ctrl: f, prog: newTestProgress(), subMenuOpen: true, subChoices: f.choices, subCursor: 0}

	// Up at the top stays put.
	mi, _ := m.Update(tea.KeyMsg{Type: tea.KeyUp})
	m = mi.(model)
	if m.subCursor != 0 {
		t.Fatalf("cursor went above 0: %d", m.subCursor)
	}
	// Down moves; down at the bottom clamps.
	for i := 0; i < 5; i++ {
		mi, _ = m.Update(tea.KeyMsg{Type: tea.KeyDown})
		m = mi.(model)
	}
	if m.subCursor != 2 {
		t.Fatalf("cursor = %d, want clamped to 2", m.subCursor)
	}
}

func TestSubMenuEnterSwitches(t *testing.T) {
	f := &fakeSubCtrl{choices: []string{"A", "B", "Off"}, active: 0}
	m := model{ctrl: f, prog: newTestProgress(), subMenuOpen: true, subChoices: f.choices, subCursor: 2}

	mi, cmd := m.Update(tea.KeyMsg{Type: tea.KeyEnter})
	m = mi.(model)
	if m.subMenuOpen {
		t.Fatal("menu should close on enter")
	}
	if !m.switching {
		t.Fatal("switching flag not set")
	}
	if cmd == nil {
		t.Fatal("enter returned no switch command")
	}
	done := cmd()
	if _, ok := done.(subDoneMsg); !ok {
		t.Fatalf("expected subDoneMsg, got %T", done)
	}
	if len(f.setCalls) != 1 || f.setCalls[0] != 2 {
		t.Fatalf("SetSubtitle calls = %v, want [2]", f.setCalls)
	}
}

func TestSubDoneRefreshesLabel(t *testing.T) {
	f := &fakeSubCtrl{subInfo: "soft: movie.srt"}
	m := model{ctrl: f, prog: newTestProgress(), switching: true, subInfo: "old"}
	mi, _ := m.Update(subDoneMsg{})
	m = mi.(model)
	if m.switching {
		t.Fatal("switching not cleared")
	}
	if m.subInfo != "soft: movie.srt" {
		t.Fatalf("subInfo = %q, want refreshed", m.subInfo)
	}
}

func TestSubMenuEscCloses(t *testing.T) {
	f := &fakeSubCtrl{choices: []string{"A", "Off"}}
	m := model{ctrl: f, prog: newTestProgress(), subMenuOpen: true, subChoices: f.choices}
	mi, _ := m.Update(tea.KeyMsg{Type: tea.KeyEsc})
	m = mi.(model)
	if m.subMenuOpen {
		t.Fatal("esc did not close the menu")
	}
	if len(f.setCalls) != 0 {
		t.Fatalf("esc issued a switch: %v", f.setCalls)
	}
}

func TestSwitchingGatesPolling(t *testing.T) {
	m := model{ctrl: &fakeSubCtrl{}, prog: newTestProgress(), switching: true}
	// A tick while switching re-arms the timer but must not also fire a poll.
	_, cmd := m.Update(tickMsg(time.Now()))
	if cmd == nil {
		t.Fatal("tick should re-arm even while switching")
	}
}

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
