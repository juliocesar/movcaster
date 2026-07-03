// Package tui is a thin bubbletea view over a Controller (a cast.Session). All
// control actions route through the Controller; the model only renders state and
// translates keystrokes into Controller calls.
package tui

import (
	"context"
	"fmt"
	"strings"
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

// SubtitleController is the optional capability of live subtitle switching,
// implemented by *core.Cast. *renderer.Renderer does not implement it, so the
// picker simply never opens for controllers that lack subtitle support. It is
// kept separate from Controller so the renderer assertion above still holds.
type SubtitleController interface {
	SubtitleChoices() []string
	ActiveSubtitle() int
	SetSubtitle(ctx context.Context, idx int) error
	SubInfo() string
}

const seekDebounce = time.Second

type tickMsg time.Time
type posMsg struct {
	pos, dur time.Duration
	state    string
}
type volMsg int
type errMsg struct{ err error }

// Connecting-phase messages (see Start). statusMsg appends a progress line to
// the connecting screen; readyMsg delivers the live controller and flips to the
// normal view; startErrMsg aborts the TUI with the startup error. spinMsg
// drives the connecting spinner (a faster tick than the 1s polling tick; its
// loop dies once connecting is over).
type statusMsg string
type readyMsg struct {
	ctrl Controller
	opts Options
}
type startErrMsg struct{ err error }
type spinMsg time.Time

// seekFireMsg fires seekDebounce after a seek keypress; gen lets a newer press
// supersede an older pending one.
type seekFireMsg struct{ gen int }
type seekDoneMsg struct{ err error }

// subDoneMsg reports the result of a live subtitle switch.
type subDoneMsg struct{ err error }

type model struct {
	ctrl    Controller
	title   string
	device  string
	subInfo string

	pos, dur   time.Duration
	state      string
	volume     int
	hasVol     bool
	muted      bool
	volFetched bool // volume read succeeded once (deferred until the TV is ready)

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

	// Live subtitle picker. subMenuOpen shows the overlay; switching gates polling
	// during the restart (like seeking) so transient transport states aren't
	// sampled mid-switch.
	subMenuOpen bool
	subChoices  []string
	subCursor   int
	switching   bool

	// Connecting phase (Start): ctrl is nil until readyMsg adopts it, so every
	// command that touches the controller is gated on !connecting. statusLines
	// are the emit(...) progress lines; startErr is a failed startup, surfaced
	// to the caller after the loop ends.
	connecting  bool
	statusLines []string
	startErr    error
	spinFrame   int
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

// startResult carries what startFn produced out of its goroutine, so a
// controller that finishes starting after the user has already quit is still
// delivered to the caller for teardown instead of leaking.
type startResult struct {
	ctrl Controller
	opts Options
	err  error
}

// Start launches the TUI immediately, showing a connecting screen (title +
// progress lines) while startFn brings the cast up in a goroutine. startFn
// reports progress via emit — each string becomes a line on the screen; a
// "! "-prefixed line renders as a warning — and returns the live Controller
// plus view Options once playback has begun, at which point the view flips to
// the normal progress UI. Its ctx is cancelled if the user quits while still
// connecting. The adopted Controller is returned so the caller can close it;
// a non-nil Controller must be closed even when the outcome is a quit or an
// error from the run itself.
func Start(title string, startFn func(ctx context.Context, emit func(string)) (Controller, Options, error)) (Outcome, Controller, error) {
	m := model{
		connecting: true,
		title:      title,
		state:      "...",
		width:      60,
		prog:       progress.New(progress.WithDefaultGradient(), progress.WithWidth(50)),
	}
	p := tea.NewProgram(m)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	resCh := make(chan startResult, 1)
	go func() {
		ctrl, opts, err := startFn(ctx, func(s string) { p.Send(statusMsg(s)) })
		resCh <- startResult{ctrl: ctrl, opts: opts, err: err}
		if err != nil {
			p.Send(startErrMsg{err})
			return
		}
		p.Send(readyMsg{ctrl: ctrl, opts: opts})
	}()

	final, err := p.Run()
	fm, _ := final.(model)
	if fm.ctrl != nil {
		return fm.outcome, fm.ctrl, err
	}
	// The model never adopted a controller: startFn failed, or the user quit
	// while connecting. Cancel the start and reap its result — if it produced a
	// controller anyway (it won the race with the quit), hand it back so the
	// caller closes it rather than leaking the server/ffmpeg/tmp dir.
	cancel()
	res := <-resCh
	if err == nil {
		// A quit-while-connecting cancels startFn, so an error from an
		// abandoned start (res.err) is self-inflicted noise; only a startup
		// failure the model saw (startErr) is the user's problem.
		err = fm.startErr
	}
	return fm.outcome, res.ctrl, err
}

func (m model) Init() tea.Cmd {
	// While connecting there is no controller to poll; the spinner tick is the
	// only driver. Volume is fetched lazily once the TV is actually playing (see
	// posMsg): a RenderingControl read issued while the TV is still buffering/
	// transitioning comes back 606 "Action not authorized", which would flash a
	// spurious error.
	if m.connecting {
		return spinCmd()
	}
	return tea.Batch(tickCmd(), m.pollCmd())
}

func tickCmd() tea.Cmd {
	return tea.Tick(time.Second, func(t time.Time) tea.Msg { return tickMsg(t) })
}

func spinCmd() tea.Cmd {
	return tea.Tick(200*time.Millisecond, func(t time.Time) tea.Msg { return spinMsg(t) })
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
			// Swallow: a mid-transition read is expected to fail with 606; posMsg
			// retries on the next poll until it succeeds, so don't alarm the user.
			return nil
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
		// Don't poll mid-seek or mid-switch: concurrent SOAP to the TV's control
		// endpoint while a restart is in flight is what makes the TV choke and time
		// out (a subtitle switch runs the same Stop->settle->SetURI->Play sequence).
		if m.seeking || m.switching {
			return m, tickCmd()
		}
		return m, tea.Batch(tickCmd(), m.pollCmd())

	case spinMsg:
		if !m.connecting {
			return m, nil // spinner loop ends once the live view takes over
		}
		m.spinFrame++
		return m, spinCmd()

	case statusMsg:
		m.statusLines = append(m.statusLines, string(msg))
		return m, nil

	case readyMsg:
		m.ctrl = msg.ctrl
		m.title = msg.opts.Title
		m.device = msg.opts.Device
		m.subInfo = msg.opts.SubInfo
		m.hasVol = msg.opts.HasVolume
		m.connecting = false
		if m.quitting {
			// The user quit as the cast came up; adopt the controller only so
			// Start returns it for teardown — don't begin polling.
			return m, tea.Quit
		}
		// Begin the normal live loop. Volume stays deferred (see posMsg).
		return m, tea.Batch(tickCmd(), m.pollCmd())

	case startErrMsg:
		m.startErr = msg.err
		m.quitting = true
		return m, tea.Quit

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
		// Fetch volume once the TV has settled out of the (buffering) transitioning
		// state; retried each poll until it succeeds. RenderingControl reads issued
		// while transitioning fail with 606, so we defer rather than fetch at Init.
		if m.hasVol && !m.volFetched && msg.state != "" && msg.state != "TRANSITIONING" {
			return m, m.fetchVolCmd()
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

	case subDoneMsg:
		m.switching = false
		if msg.err != nil {
			m.lastErr = msg.err
		} else if sc, ok := m.ctrl.(SubtitleController); ok {
			m.subInfo = sc.SubInfo() // header label reflects the new track
		}
		return m, nil

	case volMsg:
		m.volume = int(msg)
		m.volFetched = true
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
	// While connecting there is no controller yet: only quitting works (Start's
	// caller cancels the in-flight startup); every other key is swallowed.
	if m.connecting {
		switch msg.String() {
		case "q", "ctrl+c":
			m.quitting = true
			m.outcome = OutcomeQuit
			return m, tea.Quit
		}
		return m, nil
	}

	// While the subtitle picker is open it owns the keyboard.
	if m.subMenuOpen {
		return m.handleSubMenuKey(msg)
	}

	switch msg.String() {
	case "q", "ctrl+c":
		m.quitting = true
		m.outcome = OutcomeQuit
		stop := actionCmd(m.ctrl.Stop)
		return m, tea.Sequence(stop, tea.Quit)

	case "s":
		// Open the subtitle picker if this controller supports switching.
		if sc, ok := m.ctrl.(SubtitleController); ok {
			m.subChoices = sc.SubtitleChoices()
			if len(m.subChoices) > 0 {
				m.subCursor = sc.ActiveSubtitle()
				m.subMenuOpen = true
			}
		}
		return m, nil

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

// handleSubMenuKey handles keystrokes while the subtitle picker is open. Up/Down
// (k/j) move the cursor, Enter applies the highlighted choice (kicking off the
// switch), Esc/s close without changing anything, and q/ctrl+c still quit.
func (m model) handleSubMenuKey(msg tea.KeyMsg) (tea.Model, tea.Cmd) {
	switch msg.String() {
	case "up", "k":
		if m.subCursor > 0 {
			m.subCursor--
		}
		return m, nil

	case "down", "j":
		if m.subCursor < len(m.subChoices)-1 {
			m.subCursor++
		}
		return m, nil

	case "esc", "s":
		m.subMenuOpen = false
		return m, nil

	case "enter":
		sc, ok := m.ctrl.(SubtitleController)
		if !ok {
			m.subMenuOpen = false
			return m, nil
		}
		idx := m.subCursor
		m.subMenuOpen = false
		m.switching = true
		return m, func() tea.Msg {
			// Generous budget: the switch does several retried SOAP calls.
			ctx, cancel := withCtx(60 * time.Second)
			defer cancel()
			return subDoneMsg{err: sc.SetSubtitle(ctx, idx)}
		}

	case "q", "ctrl+c":
		m.quitting = true
		m.outcome = OutcomeQuit
		return m, tea.Sequence(actionCmd(m.ctrl.Stop), tea.Quit)
	}
	return m, nil // swallow other keys while the menu is open
}

var (
	titleStyle = lipgloss.NewStyle().Bold(true).Foreground(lipgloss.Color("213"))
	dimStyle   = lipgloss.NewStyle().Foreground(lipgloss.Color("241"))
	stateStyle = lipgloss.NewStyle().Bold(true).Foreground(lipgloss.Color("82"))
	errStyle   = lipgloss.NewStyle().Foreground(lipgloss.Color("196"))
)

func (m model) View() string {
	if m.quitting {
		if m.connecting {
			// Never went live (quit or startup error while connecting): leave the
			// screen clean; main reports a startup error on stderr itself.
			return ""
		}
		return "Stopped.\n"
	}
	if m.connecting {
		return m.viewConnecting()
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
	if m.switching {
		times += "  → switching subtitles…"
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
	hints := dimStyle.Render("space play/pause   ←/→ seek 10s   ↑/↓ volume   m mute   s subtitles   n next   q quit")

	out := fmt.Sprintf("\n %s\n %s\n\n %s\n %s\n", header, sub, bar, status)
	if m.subMenuOpen {
		out += "\n" + m.renderSubMenu()
	}
	out += "\n " + hints + "\n"
	if m.lastErr != nil {
		out += " " + errStyle.Render("! "+m.lastErr.Error()) + "\n"
	}
	return out
}

// spinFrames animate the connecting screen's in-progress line (braille spinner).
var spinFrames = []string{"⠋", "⠙", "⠹", "⠸", "⠼", "⠴", "⠦", "⠧", "⠇", "⠏"}

// viewConnecting renders the startup screen: the title, one row per progress
// line startFn emitted (a "! " prefix marks a warning), and an animated
// connecting indicator. Shown until readyMsg flips to the live view.
func (m model) viewConnecting() string {
	var b strings.Builder
	b.WriteString("\n " + titleStyle.Render(m.title) + "\n\n")
	for _, line := range m.statusLines {
		if strings.HasPrefix(line, "! ") {
			b.WriteString(" " + errStyle.Render(line) + "\n")
			continue
		}
		b.WriteString(" " + stateStyle.Render("✓") + " " + line + "\n")
	}
	spin := spinFrames[m.spinFrame%len(spinFrames)]
	b.WriteString(" " + stateStyle.Render(spin) + " " + dimStyle.Render("Connecting…") + "\n")
	b.WriteString("\n " + dimStyle.Render("q quit") + "\n")
	return b.String()
}

// renderSubMenu renders the subtitle picker overlay: one row per choice, the
// cursor marked with ">", and the currently active track tagged.
func (m model) renderSubMenu() string {
	active := -1
	if sc, ok := m.ctrl.(SubtitleController); ok {
		active = sc.ActiveSubtitle()
	}
	var b strings.Builder
	b.WriteString(" " + dimStyle.Render("Subtitles  (↑/↓ select · enter apply · esc cancel)") + "\n")
	for i, label := range m.subChoices {
		row := "    " + label
		if i == m.subCursor {
			row = "  > " + label
		}
		if i == active {
			row += "   ● active"
		}
		if i == m.subCursor {
			b.WriteString(stateStyle.Render(row))
		} else {
			b.WriteString(dimStyle.Render(row))
		}
		b.WriteString("\n")
	}
	return b.String()
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
