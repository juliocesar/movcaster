package core

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"sync"
	"time"

	"github.com/juliocesar/movcaster/internal/config"
	"github.com/juliocesar/movcaster/internal/renderer"
	"github.com/juliocesar/movcaster/internal/subs"
	"github.com/juliocesar/movcaster/internal/transcode"
)

// Cast is a live, controllable cast. It is concurrency-safe and satisfies the
// playback-control interface the TUI drives. It hides the difference between
// direct-play (native byte-range seek) and transcode (seek-restart: relaunch
// ffmpeg at a new -ss offset), reporting a sane position/duration in both modes.
type Cast struct {
	r   RendererControl
	srv MediaServer

	knownDuration time.Duration

	// Transcode mode: buildArgs(ss) yields the ffmpeg args to start playback at
	// absolute offset ss. nil => direct-play.
	buildArgs func(ss time.Duration) []string
	media     renderer.Media

	// UI metadata.
	title   string
	device  string
	subInfo string
	hasVol  bool

	mu       sync.Mutex
	ssOffset time.Duration // absolute offset the current transcode segment starts at

	seekMu sync.Mutex // serializes seek-restarts so two never interleave

	lastPos time.Duration // most recent observed position, for resume on Close

	// resume persistence (nil => disabled)
	resume     resumeStore
	resumePath string // absolute file path, key into the resume store

	// teardown
	closeOnce    sync.Once
	tmpDir       string
	releaseAwake func() // releases the idle-sleep assertion held while casting
}

func (c *Cast) transcoding() bool { return c.buildArgs != nil }

// Title, Device, SubInfo, HasVolume expose UI-facing metadata.
func (c *Cast) Title() string   { return c.title }
func (c *Cast) Device() string  { return c.device }
func (c *Cast) SubInfo() string { return c.subInfo }
func (c *Cast) HasVolume() bool { return c.hasVol }

func (c *Cast) Play(ctx context.Context) error  { return c.r.Play(ctx) }
func (c *Cast) Pause(ctx context.Context) error { return c.r.Pause(ctx) }
func (c *Cast) Stop(ctx context.Context) error  { return c.r.Stop(ctx) }
func (c *Cast) TransportState(ctx context.Context) (string, error) {
	return c.r.TransportState(ctx)
}
func (c *Cast) Volume(ctx context.Context) (int, error)    { return c.r.Volume(ctx) }
func (c *Cast) SetVolume(ctx context.Context, v int) error { return c.r.SetVolume(ctx, v) }
func (c *Cast) Mute(ctx context.Context, on bool) error    { return c.r.Mute(ctx, on) }

// Position reports absolute position and total duration. In transcode mode the
// TV's reported position is segment-relative, so we add the segment offset and
// substitute the probed duration (a fragmented stream has no total duration).
func (c *Cast) Position(ctx context.Context) (pos, dur time.Duration, err error) {
	pos, dur, err = c.r.Position(ctx)
	if c.transcoding() {
		c.mu.Lock()
		off := c.ssOffset
		c.mu.Unlock()
		pos += off
		if c.knownDuration > 0 {
			dur = c.knownDuration
		}
		if pos > dur && dur > 0 {
			pos = dur
		}
	}
	// Cache the latest real position so Close can persist a resume point even
	// after the TV has been stopped (which zeroes its reported position).
	if err == nil && pos > 0 {
		c.mu.Lock()
		c.lastPos = pos
		c.mu.Unlock()
	}
	return pos, dur, err
}

// Status is a synchronous snapshot of the live cast: state, position, duration,
// and the static metadata. Volume is queried only if the device supports it.
// (The TUI polls the granular methods directly; Status serves non-terminal UIs.)
func (c *Cast) Status(ctx context.Context) Status {
	s := Status{
		Title:     c.title,
		Device:    c.device,
		SubInfo:   c.subInfo,
		HasVolume: c.hasVol,
	}
	s.Pos, s.Dur, _ = c.Position(ctx)
	s.State, _ = c.r.TransportState(ctx)
	if c.hasVol {
		s.Volume, _ = c.r.Volume(ctx)
	}
	return s
}

// Status is a snapshot of a live cast.
type Status struct {
	State     string
	Pos, Dur  time.Duration
	Volume    int
	HasVolume bool
	Title     string
	Device    string
	SubInfo   string
}

// Seek jumps to an absolute position. Direct-play uses native AVTransport Seek;
// transcode relaunches ffmpeg at the new offset and re-points the TV at the fresh
// stream (Stop -> settle -> SetAVTransportURI -> Play). The control sequence is
// serialized and each SOAP step is retried, because TVs are flaky mid-transition.
func (c *Cast) Seek(ctx context.Context, pos time.Duration) error {
	if !c.transcoding() {
		return c.r.Seek(ctx, pos)
	}
	if pos < 0 {
		pos = 0
	}

	// One restart at a time; a queued seek runs to the latest target afterward.
	c.seekMu.Lock()
	defer c.seekMu.Unlock()

	// Stop the current stream (best-effort, short timeout) and let the TV settle
	// before we hand it a new URI; swapping while it's still tearing down the old
	// stream is what makes SetAVTransportURI hang.
	stopCtx, cancel := context.WithTimeout(ctx, 4*time.Second)
	_ = c.r.Stop(stopCtx)
	cancel()
	if !sleepCtx(ctx, 500*time.Millisecond) {
		return ctx.Err()
	}

	c.srv.SetTranscode(c.buildArgs(pos))
	c.media.URL = c.srv.MediaURL()

	if err := retrySOAP(ctx, 3, 9*time.Second, func(ic context.Context) error {
		return c.r.SetMedia(ic, c.media)
	}); err != nil {
		return err
	}
	if err := retrySOAP(ctx, 3, 7*time.Second, func(ic context.Context) error {
		return c.r.Play(ic)
	}); err != nil {
		return err
	}

	c.mu.Lock()
	c.ssOffset = pos
	c.mu.Unlock()
	return nil
}

// Close tears the cast down: stops playback, kills ffmpeg + shuts the server
// down, and removes the temp dir. Safe to call more than once.
func (c *Cast) Close(ctx context.Context) error {
	var err error
	c.closeOnce.Do(func() {
		c.persistResume()
		if c.releaseAwake != nil {
			c.releaseAwake()
		}
		err = c.srv.Shutdown(ctx)
		if c.tmpDir != "" {
			_ = os.RemoveAll(c.tmpDir)
		}
	})
	return err
}

// persistResume records the last observed position so the file resumes there
// next time. It clears the record once the file is effectively finished, and
// ignores negligible positions (leaving any earlier record intact).
func (c *Cast) persistResume() {
	if c.resume == nil || c.resumePath == "" {
		return
	}
	c.mu.Lock()
	pos := c.lastPos
	c.mu.Unlock()
	switch {
	case pos < 5*time.Second:
		return
	case c.knownDuration > 0 && pos >= c.knownDuration-30*time.Second:
		_ = c.resume.Clear(c.resumePath)
	default:
		_ = c.resume.Set(c.resumePath, pos)
	}
}

// Start resolves the device, binds the media server, applies the subtitle and
// codec strategy, then SetAVTransportURI + Play. It returns the live Cast and
// the Preparation (whose strategy/label the caller may display). On any error
// after the server is bound, it cleans up before returning.
func (a *App) Start(ctx context.Context, req CastRequest) (*Cast, *Preparation, error) {
	if err := a.Doctor(); err != nil {
		return nil, nil, err
	}
	if _, err := os.Stat(req.File); err != nil {
		return nil, nil, fmt.Errorf("file: %w", err)
	}

	// selectDevice owns its own discovery timeouts (quick pass + deep fallback).
	dev, err := a.selectDevice(ctx, req.Target)
	if err != nil {
		return nil, nil, err
	}
	a.emit(Info, "Casting to %s [%s]", dev.FriendlyName, dev.Location.Host)

	// Remember this device so a future bare `movcaster <file>` finds it again.
	_ = a.store.Save(config.Config{LastDeviceHost: hostOf(dev.Location.Host)})

	prep, err := a.Prepare(ctx, req)
	if err != nil {
		return nil, nil, err
	}
	if prep.DecideErr != nil {
		return nil, nil, prep.DecideErr
	}
	if prep.ProbeErr != nil {
		a.emit(Warn, "ffprobe failed, auto-detection limited: %v", prep.ProbeErr)
	}
	abs := prep.AbsPath

	srv, err := a.newServer(dev.Location.Host)
	if err != nil {
		return nil, nil, err
	}
	srv.Start()

	srv.SetDirectPlay(abs, mimeForExt(abs))
	rend := a.newRenderer(*dev)

	media := renderer.Media{
		URL:      srv.MediaURL(),
		Title:    filepath.Base(abs),
		MIME:     mimeForExt(abs),
		Seekable: true,
	}

	tmpDir, err := os.MkdirTemp("", "movcaster-")
	if err != nil {
		_ = srv.Shutdown(context.Background())
		return nil, nil, err
	}

	// Advertise the real duration in DIDL-Lite. Direct-play files declare it in
	// their container anyway, but a live transcode streams an empty-moov fragmented
	// MP4 with no total duration, so the TV's seek bar races without this hint.
	if prep.Info != nil {
		media.Duration = prep.Info.Duration
	}

	label, build, err := a.applySubtitles(srv, &media, abs, tmpDir, prep.Strategy)
	if err != nil {
		_ = srv.Shutdown(context.Background())
		_ = os.RemoveAll(tmpDir)
		return nil, nil, err
	}

	// Codec-compatibility fallback: only when not already transcoding for subs.
	if build == nil {
		plan := codecPlan(prep.Info, req.ForceTranscode)
		if plan.Kind == TranscodeCodec {
			a.emit(Info, "Transcoding for codec compatibility (video=%v audio=%v)...", plan.Video, plan.Audio)
			build = func(ss time.Duration) []string {
				return transcode.Args(abs, ss, plan.Video, plan.Audio)
			}
			if label == "" {
				label = "transcode"
			}
			applyTranscode(srv, &media, build, 0)
		}
	}

	// Resume: if we have a saved position for this file, start there. A transcode
	// stream begins ffmpeg at the offset directly; a direct-play file seeks after
	// Play. resumeOffset ignores negligible/finished positions.
	knownDur := time.Duration(0)
	if prep.Info != nil {
		knownDur = prep.Info.Duration
	}
	startOffset := time.Duration(0)
	if a.resume != nil {
		startOffset = resumeOffset(a.resume.Get(abs), knownDur)
	}
	if startOffset > 0 {
		a.emit(Info, "Resuming at %s", clock(startOffset))
		if build != nil {
			applyTranscode(srv, &media, build, startOffset)
		}
	}

	setCtx, setCancel := context.WithTimeout(ctx, 10*time.Second)
	defer setCancel()
	if err := rend.SetMedia(setCtx, media); err != nil {
		_ = srv.Shutdown(context.Background())
		_ = os.RemoveAll(tmpDir)
		return nil, nil, fmt.Errorf("SetAVTransportURI: %w", err)
	}
	if err := rend.Play(setCtx); err != nil {
		_ = srv.Shutdown(context.Background())
		_ = os.RemoveAll(tmpDir)
		return nil, nil, fmt.Errorf("Play: %w", err)
	}

	// Direct-play resume: seek after Play (best-effort; the TV may need a moment
	// to leave the TRANSITIONING state, so retry briefly). Transcode resume is
	// already baked into the stream offset above.
	if build == nil && startOffset > 0 {
		_ = retrySOAP(setCtx, 4, 3*time.Second, func(c context.Context) error {
			return rend.Seek(c, startOffset)
		})
	}

	c := &Cast{
		r:             rend,
		srv:           srv,
		media:         media,
		knownDuration: knownDur,
		buildArgs:     build,
		title:         filepath.Base(abs),
		device:        dev.FriendlyName,
		subInfo:       label,
		hasVol:        rend.HasVolume(),
		tmpDir:        tmpDir,
		resume:        a.resume,
		resumePath:    abs,
		// Hold an idle-sleep assertion so a sleeping display doesn't stall the
		// stream; Close releases it. (See inhibitSleep.)
		releaseAwake: inhibitSleep(),
	}
	if build != nil {
		c.ssOffset = startOffset // transcode stream starts at this absolute offset
	}
	return c, prep, nil
}

// applyTranscode points the server + media at the transcode stream starting at
// the given offset (0 for a fresh cast; the resume position when resuming).
func applyTranscode(srv MediaServer, media *renderer.Media, build func(time.Duration) []string, ss time.Duration) {
	srv.SetTranscode(build(ss))
	media.URL = srv.MediaURL()
	media.MIME = "video/mp4"
	media.Seekable = false
}

// applySubtitles executes the chosen subtitle strategy against the server +
// media metadata, returning a UI label and (for burn-in) a transcode-args
// builder. It emits progress events; it never prints.
func (a *App) applySubtitles(srv MediaServer, media *renderer.Media, abs, tmpDir string, dec subs.Decision) (label string, build func(time.Duration) []string, err error) {
	switch dec.Kind {
	case subs.None:
		return "", nil, nil

	case subs.SoftSidecar:
		mime, typ := subKind(dec.SidecarPath)
		srv.SetSubtitle(dec.SidecarPath, mime)
		media.SubURL, media.SubMIME, media.SubType = srv.SubURL(), mime, typ
		label = "soft: " + filepath.Base(dec.SidecarPath)
		a.emit(Info, "Subtitles: %s", label)
		return label, nil, nil

	case subs.SoftExtract:
		a.emit(Info, "Subtitles: extracting text track %d (%s)...", dec.Track.SubIndex, dec.Track.Codec)
		ectx, ecancel := context.WithTimeout(context.Background(), 60*time.Second)
		defer ecancel()
		vtt, err := subs.ExtractText(ectx, abs, dec.Track.SubIndex, tmpDir)
		if err != nil {
			return "", nil, err
		}
		srv.SetSubtitle(vtt, "text/vtt")
		media.SubURL, media.SubMIME, media.SubType = srv.SubURL(), "text/vtt", "vtt"
		label = fmt.Sprintf("soft: embedded track %d", dec.Track.SubIndex)
		a.emit(Info, "Subtitles: %s", label)
		return label, nil, nil

	case subs.MuxSoft:
		a.emit(Info, "Subtitles: remuxing bitmap track %d (%s) as soft [experimental]...", dec.Track.SubIndex, dec.Track.Codec)
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Minute)
		defer cancel()
		muxed, err := subs.MuxSoftRemux(ctx, abs, *dec.Track, tmpDir)
		if err != nil {
			return "", nil, err
		}
		srv.SetDirectPlay(muxed, "video/x-matroska")
		media.URL, media.MIME, media.Seekable = srv.MediaURL(), "video/x-matroska", true
		label = fmt.Sprintf("mux-soft: track %d (toggle subs on the TV)", dec.Track.SubIndex)
		a.emit(Info, "Subtitles: %s", label)
		return label, nil, nil

	case subs.Burn:
		a.emit(Info, "Subtitles: burning in %s track %d (%s) on the fly...", dec.Track.Kind, dec.Track.SubIndex, dec.Track.Codec)
		track := *dec.Track
		build = func(ss time.Duration) []string { return subs.BurnArgs(abs, track, ss) }
		applyTranscode(srv, media, build, 0)
		label = fmt.Sprintf("burn-in: track %d (%s)", dec.Track.SubIndex, dec.Track.Codec)
		a.emit(Info, "Subtitles: %s", label)
		return label, build, nil
	}
	return "", nil, nil
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
