// Package tui is a thin bubbletea view over a Controller (a cast.Session). All
// control actions route through the Controller; the model only renders state and
// translates keystrokes into Controller calls.
package tui

import (
	"context"
	"fmt"
	"time"

	"github.com/charmbracelet/bubbles/progress"
	tea "github.com/charmbracelet/bubbletea"
	"github.com/charmbracelet/lipgloss"
	"github.com/juliocesar/movcaster/internal/renderer"
)

const seekStep = 10 * time.Second

// endGuard is how close to the duration the last observed position must be for a
// STOPPED state to count as a natural end-of-media (rather than an early or
// mid-transition stop). The poll runs every second and the TV reports the stop a
// tick or two late, so we allow a small margin.
const endGuard = 12 * time.Second

// Outcome reports why the TUI loop ended, so the caller can decide whether to
// advance to the next episode.
type Outcome int

const (
	OutcomeQuit  Outcome = iota // user quit (q/ctrl+c)
	OutcomeEnded                // media played to its end
	OutcomeNext                 // user asked for the next episode (n)
)

// Controller is the playback behavior the TUI drives. cast.Session implements it
// for both direct-play (native seek) and transcode (seek-restart).
type Controller interface {
	Play(context.Context) error
	Pause(context.Context) error
	Stop(context.Context) error
	Seek(context.Context, time.Duration) error
	Position(context.Context) (pos, dur time.Duration, err error)
	TransportState(context.Context) (string, error)
	HasVolume() bool
	Volume(context.Context) (int, error)
	SetVolume(context.Context, int) error
	Mute(context.Context, bool) error
}

// Ensure *renderer.Renderer satisfies Controller.
var _ Controller = (*renderer.Renderer)(nil)

const seekDebounce = time.Second

type tickMsg time.Time
type posMsg struct {
	pos, dur time.Duration
	state    string
}
type volMsg int
type errMsg struct{ err error }

// seekFireMsg fires seekDebounce after a seek keypress; gen lets a newer press
// supersede an older pending one.
type seekFireMsg struct{ gen int }
type seekDoneMsg struct{ err error }

type model struct {
	ctrl    Controller
	title   string
	device  string
	subInfo string

	pos, dur time.Duration
	state    string
	volume   int
	hasVol   bool
	muted    bool

	prog     progress.Model
	width    int
	lastErr  error
	quitting bool
	outcome  Outcome

	// End-of-media detection. everPlayed gates against the STOPPED state the TV
	// reports before playback begins; maxProgress is the furthest position seen
	// (the TV may reset its reported position to 0 on a natural stop).
	everPlayed  bool
	maxProgress time.Duration

	// Debounced seeking: arrow presses move pendingSeek and (re)arm a timer; the
	// actual seek is issued only after seekDebounce of no further presses. While
	// seeking, position polls don't overwrite the displayed (target) position.
	seeking     bool
	pendingSeek time.Duration
	seekGen     int
}

// Options configure the view.
type Options struct {
	Title     string
	Device    string
	SubInfo   string // e.g. "soft: file.srt" or "burn-in" (shown in header)
	HasVolume bool
}

// Run launches the TUI loop. It blocks until the media ends or the user quits,
// then returns why it ended so the caller can decide whether to advance.
func Run(ctrl Controller, opts Options) (Outcome, error) {
	m := model{
		ctrl:    ctrl,
		title:   opts.Title,
		device:  opts.Device,
		subInfo: opts.SubInfo,
		hasVol:  opts.HasVolume,
		state:   "...",
		width:   60,
		prog:    progress.New(progress.WithDefaultGradient(), progress.WithWidth(50)),
	}
	p := tea.NewProgram(m)
	final, err := p.Run()
	if fm, ok := final.(model); ok {
		return fm.outcome, err
	}
	return OutcomeQuit, err
}

func (m model) Init() tea.Cmd {
	return tea.Batch(tickCmd(), m.pollCmd(), m.fetchVolCmd())
}

func tickCmd() tea.Cmd {
	return tea.Tick(time.Second, func(t time.Time) tea.Msg { return tickMsg(t) })
}

func withCtx(d time.Duration) (context.Context, context.CancelFunc) {
	return context.WithTimeout(context.Background(), d)
}

func (m model) pollCmd() tea.Cmd {
	return func() tea.Msg {
		ctx, cancel := withCtx(2 * time.Second)
		defer cancel()
		pos, dur, err := m.ctrl.Position(ctx)
		if err != nil {
			return errMsg{err}
		}
		state, _ := m.ctrl.TransportState(ctx)
		return posMsg{pos: pos, dur: dur, state: state}
	}
}

func (m model) fetchVolCmd() tea.Cmd {
	if !m.hasVol {
		return nil
	}
	return func() tea.Msg {
		ctx, cancel := withCtx(2 * time.Second)
		defer cancel()
		v, err := m.ctrl.Volume(ctx)
		if err != nil {
			return errMsg{err}
		}
		return volMsg(v)
	}
}

func actionCmd(fn func(context.Context) error) tea.Cmd {
	return func() tea.Msg {
		ctx, cancel := withCtx(4 * time.Second)
		defer cancel()
		if err := fn(ctx); err != nil {
			return errMsg{err}
		}
		return nil
	}
}

func (m model) Update(msg tea.Msg) (tea.Model, tea.Cmd) {
	switch msg := msg.(type) {
	case tea.WindowSizeMsg:
		m.width = msg.Width
		w := msg.Width - 10
		if w > 0 {
			m.prog.Width = w
		}
		return m, nil

	case tickMsg:
		// Don't poll mid-seek: concurrent SOAP to the TV's control endpoint while
		// a seek-restart is in flight is what makes the TV choke and time out.
		if m.seeking {
			return m, tickCmd()
		}
		return m, tea.Batch(tickCmd(), m.pollCmd())

	case posMsg:
		m.dur, m.state = msg.dur, msg.state
		if !m.seeking { // don't fight the user's pending target
			m.pos = msg.pos
		}
		if msg.state == "PLAYING" {
			m.everPlayed = true
		}
		if msg.pos > m.maxProgress {
			m.maxProgress = msg.pos
		}
		// Natural end: the TV stops after we've seen it play, with the furthest
		// observed position within endGuard of the duration. The seeking gate on
		// tickMsg means a mid-seek-restart STOPPED is never sampled here.
		if m.everPlayed && !m.seeking && isStopped(msg.state) &&
			m.dur > 0 && m.maxProgress >= m.dur-endGuard {
			m.outcome = OutcomeEnded
			m.quitting = true
			return m, tea.Quit
		}
		return m, nil

	case seekFireMsg:
		if msg.gen != m.seekGen { // a newer keypress superseded this one
			return m, nil
		}
		target := m.pendingSeek
		return m, func() tea.Msg {
			// Generous overall budget: a transcode seek-restart does several SOAP
			// calls with retries. Per-call timeouts live in the controller.
			ctx, cancel := withCtx(60 * time.Second)
			defer cancel()
			return seekDoneMsg{err: m.ctrl.Seek(ctx, target)}
		}

	case seekDoneMsg:
		m.seeking = false
		if msg.err != nil {
			m.lastErr = msg.err
		}
		return m, nil

	case volMsg:
		m.volume = int(msg)
		return m, nil

	case errMsg:
		m.lastErr = msg.err
		return m, nil

	case tea.KeyMsg:
		return m.handleKey(msg)
	}
	return m, nil
}

// armSeek records a new target and (re)starts the debounce timer.
func (m *model) armSeek(target time.Duration) tea.Cmd {
	m.pos = target
	m.pendingSeek = target
	m.seeking = true
	m.seekGen++
	gen := m.seekGen
	return tea.Tick(seekDebounce, func(time.Time) tea.Msg { return seekFireMsg{gen} })
}

func (m model) handleKey(msg tea.KeyMsg) (tea.Model, tea.Cmd) {
	switch msg.String() {
	case "q", "ctrl+c":
		m.quitting = true
		m.outcome = OutcomeQuit
		stop := actionCmd(m.ctrl.Stop)
		return m, tea.Sequence(stop, tea.Quit)

	case "n":
		m.quitting = true
		m.outcome = OutcomeNext
		stop := actionCmd(m.ctrl.Stop)
		return m, tea.Sequence(stop, tea.Quit)

	case " ", "p":
		if m.state == "PLAYING" {
			return m, actionCmd(m.ctrl.Pause)
		}
		return m, actionCmd(m.ctrl.Play)

	case "right", "l":
		target := m.pos + seekStep
		if m.dur > 0 && target > m.dur {
			target = m.dur
		}
		return m, m.armSeek(target)

	case "left", "h":
		target := m.pos - seekStep
		if target < 0 {
			target = 0
		}
		return m, m.armSeek(target)

	case "up", "k":
		if !m.hasVol {
			return m, nil
		}
		m.volume = clamp(m.volume+1, 0, 100)
		v := m.volume
		return m, actionCmd(func(c context.Context) error { return m.ctrl.SetVolume(c, v) })

	case "down", "j":
		if !m.hasVol {
			return m, nil
		}
		m.volume = clamp(m.volume-1, 0, 100)
		v := m.volume
		return m, actionCmd(func(c context.Context) error { return m.ctrl.SetVolume(c, v) })

	case "m":
		if !m.hasVol {
			return m, nil
		}
		m.muted = !m.muted
		on := m.muted
		return m, actionCmd(func(c context.Context) error { return m.ctrl.Mute(c, on) })
	}
	return m, nil
}

var (
	titleStyle = lipgloss.NewStyle().Bold(true).Foreground(lipgloss.Color("213"))
	dimStyle   = lipgloss.NewStyle().Foreground(lipgloss.Color("241"))
	stateStyle = lipgloss.NewStyle().Bold(true).Foreground(lipgloss.Color("82"))
	errStyle   = lipgloss.NewStyle().Foreground(lipgloss.Color("196"))
)

func (m model) View() string {
	if m.quitting {
		return "Stopped.\n"
	}
	var pct float64
	if m.dur > 0 {
		pct = float64(m.pos) / float64(m.dur)
		if pct > 1 {
			pct = 1
		}
	}

	header := titleStyle.Render(m.title)
	sub := dimStyle.Render(fmt.Sprintf("→ %s", m.device))
	if m.subInfo != "" {
		sub += dimStyle.Render("   subs: " + m.subInfo)
	}

	bar := m.prog.ViewAs(pct)
	times := fmt.Sprintf("%s / %s", fmtDur(m.pos), fmtDur(m.dur))
	if m.seeking {
		times += "  → seeking…"
	}

	vol := ""
	if m.hasVol {
		if m.muted {
			vol = "  vol: muted"
		} else {
			vol = fmt.Sprintf("  vol: %d%%", m.volume)
		}
	}

	status := fmt.Sprintf("%s   %s%s", stateStyle.Render(prettyState(m.state)), times, vol)
	hints := dimStyle.Render("space play/pause   ←/→ seek 10s   ↑/↓ volume   m mute   n next   q quit")

	out := fmt.Sprintf("\n %s\n %s\n\n %s\n %s\n\n %s\n", header, sub, bar, status, hints)
	if m.lastErr != nil {
		out += " " + errStyle.Render("! "+m.lastErr.Error()) + "\n"
	}
	return out
}

// isStopped reports whether a transport state means playback has stopped (the TV
// uses STOPPED; some report NO_MEDIA_PRESENT once the stream is fully drained).
func isStopped(state string) bool {
	return state == "STOPPED" || state == "NO_MEDIA_PRESENT"
}

func prettyState(s string) string {
	switch s {
	case "PLAYING":
		return "▶ PLAYING"
	case "PAUSED_PLAYBACK":
		return "⏸ PAUSED"
	case "STOPPED":
		return "⏹ STOPPED"
	case "TRANSITIONING":
		return "… BUFFERING"
	case "":
		return "…"
	default:
		return s
	}
}

func clamp(v, lo, hi int) int {
	if v < lo {
		return lo
	}
	if v > hi {
		return hi
	}
	return v
}

func fmtDur(d time.Duration) string {
	if d < 0 {
		d = 0
	}
	total := int(d / time.Second)
	h := total / 3600
	mn := (total % 3600) / 60
	s := total % 60
	if h > 0 {
		return fmt.Sprintf("%d:%02d:%02d", h, mn, s)
	}
	return fmt.Sprintf("%02d:%02d", mn, s)
}
