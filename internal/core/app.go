package core

import (
	"context"
	"fmt"
	"net"
	"net/url"
	"os/exec"
	"path/filepath"
	"strings"
	"time"

	"github.com/juliocesar/movcaster/internal/discovery"
	"github.com/juliocesar/movcaster/internal/renderer"
)

// App is the orchestration entry point. It holds cross-cast dependencies and is
// cheap to construct; no I/O happens until a method is called.
type App struct {
	finder      DeviceFinder
	newServer   func(deviceHost string) (MediaServer, error)
	newRenderer func(discovery.Device) RendererControl
	store       Store
	resume      resumeStore
	onEvent     func(Event)
}

// Options configures an App. All fields are optional; zero values wire the
// production implementations. Tests override finder/newServer/newRenderer/store
// with fakes, and onEvent to capture progress.
type Options struct {
	Finder      DeviceFinder
	NewServer   func(deviceHost string) (MediaServer, error)
	NewRenderer func(discovery.Device) RendererControl
	Store       Store
	// Resume persists playback positions. The CLI supplies *resume.Store; when
	// left nil, the resume feature is disabled (e.g. in tests).
	Resume  resumeStore
	OnEvent func(Event)
}

// EventLevel distinguishes user-facing progress from warnings.
type EventLevel int

const (
	// Info is normal progress (printed to stdout by the CLI).
	Info EventLevel = iota
	// Warn is a non-fatal warning (printed to stderr by the CLI).
	Warn
)

// Event is a structured progress/log message. Core emits these instead of
// printing; the front-end renders them. Message carries no "movcaster:" prefix.
type Event struct {
	Level   EventLevel
	Message string
}

// New builds an App, defaulting any unset dependency to its production impl.
func New(opts Options) *App {
	a := &App{
		finder:      opts.Finder,
		newServer:   opts.NewServer,
		newRenderer: opts.NewRenderer,
		store:       opts.Store,
		resume:      opts.Resume,
		onEvent:     opts.OnEvent,
	}
	if a.finder == nil {
		a.finder = realFinder{}
	}
	if a.newServer == nil {
		a.newServer = func(host string) (MediaServer, error) { return realServer(host) }
	}
	if a.newRenderer == nil {
		a.newRenderer = func(d discovery.Device) RendererControl { return renderer.New(d) }
	}
	if a.store == nil {
		a.store = configStore{}
	}
	return a
}

func (a *App) emit(level EventLevel, format string, args ...any) {
	if a.onEvent == nil {
		return
	}
	a.onEvent(Event{Level: level, Message: fmt.Sprintf(format, args...)})
}

// Doctor verifies external dependencies (ffmpeg + ffprobe on PATH).
func (a *App) Doctor() error {
	for _, bin := range []string{"ffmpeg", "ffprobe"} {
		if _, err := exec.LookPath(bin); err != nil {
			return fmt.Errorf("%s not found on PATH; install ffmpeg (e.g. `brew install ffmpeg`)", bin)
		}
	}
	return nil
}

// ListDevices discovers DLNA renderers on the LAN.
func (a *App) ListDevices(ctx context.Context) ([]discovery.Device, error) {
	return a.finder.Discover(ctx)
}

// Discovery timeouts. The first pass is short (TVs that are awake answer the
// initial SSDP burst fast); if nothing turns up we search harder for a while,
// since renderers routinely miss the first M-SEARCH.
const (
	quickDiscover = 5 * time.Second
	deepDiscover  = 10 * time.Second
)

// selectDevice resolves the target to a device. Empty target picks the saved or
// sole device, and when none answer the quick pass it falls back to a longer
// search (with user feedback) before giving up. A target matches by host IP
// (robust to the TV's dynamic control port), falling back to a direct
// description-URL load.
func (a *App) selectDevice(ctx context.Context, target string) (*discovery.Device, error) {
	if target != "" {
		return a.selectTarget(ctx, target)
	}

	// Quick pass: prefer the saved or sole device if anything answers promptly.
	qctx, cancel := context.WithTimeout(ctx, quickDiscover)
	devices, derr := a.finder.Discover(qctx)
	cancel()
	if derr == nil {
		if d := a.pickDevice(devices); d != nil {
			return d, nil
		}
		if len(devices) > 1 {
			return nil, fmt.Errorf("%d renderers found; pick one with -t (run -l to list)", len(devices))
		}
	}

	// Nothing yet: search harder before giving up, telling the user why we wait.
	a.emit(Info, "Looking for a TV...")
	return a.waitForDevice(ctx, deepDiscover)
}

// pickDevice returns the device to use from a discovery result: the saved
// device if present, else the sole device. Returns nil when the choice is
// ambiguous (multiple, none saved) or empty.
func (a *App) pickDevice(devices []discovery.Device) *discovery.Device {
	if saved := a.store.Load().LastDeviceHost; saved != "" {
		for i := range devices {
			if devices[i].Location != nil && hostOf(devices[i].Location.Host) == saved {
				return &devices[i]
			}
		}
	}
	if len(devices) == 1 {
		return &devices[0]
	}
	return nil
}

// waitForDevice repeatedly discovers until a usable renderer appears or budget
// elapses. Each pass blocks on its own SSDP search; we retry until the deadline.
func (a *App) waitForDevice(ctx context.Context, budget time.Duration) (*discovery.Device, error) {
	dctx, cancel := context.WithTimeout(ctx, budget)
	defer cancel()
	for {
		devices, err := a.finder.Discover(dctx)
		if err == nil {
			if d := a.pickDevice(devices); d != nil {
				return d, nil
			}
			if len(devices) > 1 {
				return nil, fmt.Errorf("%d renderers found; pick one with -t (run -l to list)", len(devices))
			}
		}
		if !sleepCtx(dctx, 500*time.Millisecond) {
			return nil, fmt.Errorf("no renderers found after %s; pass -t with a device IP/URL", budget)
		}
	}
}

// selectTarget resolves an explicit target: match a discovered device by host
// IP, else load the device directly from its description URL.
func (a *App) selectTarget(ctx context.Context, target string) (*discovery.Device, error) {
	qctx, cancel := context.WithTimeout(ctx, quickDiscover)
	devices, _ := a.finder.Discover(qctx)
	cancel()

	wantHost := hostOf(target)
	for i := range devices {
		if devices[i].Location != nil && hostOf(devices[i].Location.Host) == wantHost {
			return &devices[i], nil
		}
	}
	if u, err := url.Parse(ensureScheme(target)); err == nil && u.Host != "" {
		uctx, ucancel := context.WithTimeout(ctx, quickDiscover)
		defer ucancel()
		if d, err := a.finder.FindByURL(uctx, u); err == nil {
			return d, nil
		}
	}
	return nil, fmt.Errorf("target %q not found among %d discovered renderer(s)", target, len(devices))
}

func ensureScheme(s string) string {
	if !strings.Contains(s, "://") {
		return "http://" + s
	}
	return s
}

// hostOf extracts the bare host/IP from an IP, IP:port, or URL.
func hostOf(s string) string {
	u, err := url.Parse(ensureScheme(s))
	if err == nil && u.Hostname() != "" {
		return u.Hostname()
	}
	if h, _, err := net.SplitHostPort(s); err == nil {
		return h
	}
	return s
}

func mimeForExt(path string) string {
	switch strings.ToLower(filepath.Ext(path)) {
	case ".mp4", ".m4v":
		return "video/mp4"
	case ".mkv":
		return "video/x-matroska"
	case ".avi":
		return "video/x-msvideo"
	case ".mov":
		return "video/quicktime"
	case ".webm":
		return "video/webm"
	case ".ts":
		return "video/mp2t"
	case ".wmv":
		return "video/x-ms-wmv"
	default:
		return "video/mp4"
	}
}

// subKind returns the (HTTP/protocolInfo mime, sec:type) for a subtitle file.
func subKind(path string) (mime, secType string) {
	switch strings.ToLower(filepath.Ext(path)) {
	case ".vtt":
		return "text/vtt", "vtt"
	default:
		return "text/srt", "srt"
	}
}

// resumeOffset decides where to resume a file: the saved position, unless it is
// negligible (<5s) or within 30s of the known end (treat as finished -> start over).
func resumeOffset(saved, knownDur time.Duration) time.Duration {
	if saved <= 5*time.Second {
		return 0
	}
	if knownDur > 0 && saved >= knownDur-30*time.Second {
		return 0
	}
	return saved
}

// clock renders a duration as M:SS or H:MM:SS for user-facing messages.
func clock(d time.Duration) string {
	if d < 0 {
		d = 0
	}
	t := int(d / time.Second)
	h, m, s := t/3600, (t%3600)/60, t%60
	if h > 0 {
		return fmt.Sprintf("%d:%02d:%02d", h, m, s)
	}
	return fmt.Sprintf("%d:%02d", m, s)
}
