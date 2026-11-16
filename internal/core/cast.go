package core

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/juliocesar/movcaster/internal/config"
	"github.com/juliocesar/movcaster/internal/engine"
	"github.com/juliocesar/movcaster/internal/probe"
	"github.com/juliocesar/movcaster/internal/subs"
	"github.com/juliocesar/movcaster/internal/transcode"
)

// Cast is a live, controllable cast. It is concurrency-safe and satisfies the
// playback-control interface the TUI drives. It hides the difference between
// direct-play (native byte-range seek) and transcode (seek-restart: relaunch
// ffmpeg at a new -ss offset), reporting a sane position/duration in both modes.
type Cast struct {
	sess engine.Session
	srv  MediaServer

	knownDuration time.Duration

	// Transcode mode: buildArgs(ss) yields the ffmpeg args to start playback at
	// absolute offset ss. nil => direct-play.
	buildArgs func(ss time.Duration) []string
	// resoft re-prepares soft subs for a transcode segment starting at ss (nil when
	// the delivery has none to rebase). Seek must call it: a seek-restart rebases
	// the stream to 0, so the cue times have to be rebased to match.
	resoft  softSubFn
	media   engine.Media
	profile engine.Profile // the device's capabilities (drives StartAt vs post-Play seek)

	// UI metadata.
	title   string
	device  string
	subInfo string
	hasVol  bool

	mu       sync.Mutex
	ssOffset time.Duration // absolute offset the current transcode segment starts at

	// Cached device status. The TUI polls Position and PlayerState back to back
	// every second; one engine Status per beat is enough (on DLNA each Status is
	// two SOAP calls, so the cache keeps the per-second SOAP volume unchanged).
	lastStatus   engine.State
	lastStatusAt time.Time

	seekMu sync.Mutex // serializes seek-restarts (and live subtitle switches)

	lastPos time.Duration // most recent observed position, for resume on Close

	// Live subtitle switching. buildDelivery (on app) rebuilds the server+media for
	// a newly chosen track; SetSubtitle drives the restart. info may be nil (probe
	// failed); subChoices is built once at Start and activeSub indexes into it.
	app            *App             // for buildDelivery during a live switch
	abs            string           // absolute media path
	info           *probe.MediaInfo // probed streams (sub tracks), may be nil
	forceTranscode bool             // carry req.ForceTranscode for the codec fallback
	subChoices     []subChoice      // picker entries, built once
	activeSub      int              // index into subChoices (guarded by mu)
	subOpts        SubtitleOptions  // options behind the live delivery (guarded by mu)
	audio          int              // live audio stream a:N (guarded by mu)
	audioChoices   []string         // audio picker labels, indexed by a:N

	// resume persistence (nil => disabled)
	resume     resumeStore
	resumePath string // absolute file path, key into the resume store

	// Async playback start. Start returns as soon as the TV accepts the URI (so
	// the TUI renders at once); the slow settle+Play+resume-seek handshake runs
	// in a goroutine. While starting is true, the AVTransport read/control
	// methods short-circuit (no SOAP) so the TUI's polling can't contend with the
	// in-flight handshake — the TUI just shows a buffering state. startCancel
	// aborts the handshake on Close.
	starting    atomic.Bool
	startCancel context.CancelFunc
	startDone   chan struct{}

	// teardown
	closeOnce    sync.Once
	tmpDir       string
	releaseAwake func() // releases the idle-sleep assertion held while casting
}

// currentBuild snapshots the transcode-args builder under the lock. nil =>
// direct-play. SetSubtitle can swap it concurrently with Position/Seek reads.
func (c *Cast) currentBuild() func(time.Duration) []string {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.buildArgs
}

func (c *Cast) transcoding() bool { return c.currentBuild() != nil }

// Title, Device, SubInfo, HasVolume expose UI-facing metadata.
func (c *Cast) Title() string  { return c.title }
func (c *Cast) Device() string { return c.device }
func (c *Cast) SubInfo() string {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.subInfo
}
func (c *Cast) HasVolume() bool { return c.hasVol }

// Play/Pause are no-ops while the initial start handshake is still running: the
// stream is already buffering (most devices auto-play on Load) and issuing
// competing control there is exactly what makes a TV choke mid-transition.
func (c *Cast) Play(ctx context.Context) error {
	if c.starting.Load() {
		return nil
	}
	c.invalidateStatus()
	return c.sess.Play(ctx)
}
func (c *Cast) Pause(ctx context.Context) error {
	if c.starting.Load() {
		return nil
	}
	c.invalidateStatus()
	return c.sess.Pause(ctx)
}

// Stop cancels an in-flight start handshake (so its background control traffic
// stops racing us) before stopping the TV. Called on quit and by Close.
func (c *Cast) Stop(ctx context.Context) error {
	if c.startCancel != nil {
		c.startCancel()
	}
	c.invalidateStatus()
	return c.sess.Stop(ctx)
}

// PlayerState reports the normalized transport state. While starting it
// answers StateBuffering without hitting the TV, so the TUI's polling can't
// contend with the in-flight start handshake.
func (c *Cast) PlayerState(ctx context.Context) (engine.PlayerState, error) {
	if c.starting.Load() {
		return engine.StateBuffering, nil
	}
	st, err := c.status(ctx)
	return st.Player, err
}

func (c *Cast) Volume(ctx context.Context) (int, error)    { return c.sess.Volume(ctx) }
func (c *Cast) SetVolume(ctx context.Context, v int) error { return c.sess.SetVolume(ctx, v) }
func (c *Cast) Mute(ctx context.Context, on bool) error    { return c.sess.Mute(ctx, on) }

// status serves the device status from a short-lived cache: the TUI reads
// Position and PlayerState back to back each second and one snapshot covers both.
func (c *Cast) status(ctx context.Context) (engine.State, error) {
	c.mu.Lock()
	if time.Since(c.lastStatusAt) < 500*time.Millisecond {
		st := c.lastStatus
		c.mu.Unlock()
		return st, nil
	}
	c.mu.Unlock()
	st, err := c.sess.Status(ctx)
	if err != nil {
		return engine.State{}, err
	}
	c.mu.Lock()
	c.lastStatus, c.lastStatusAt = st, time.Now()
	c.mu.Unlock()
	return st, nil
}

// invalidateStatus drops the cached snapshot after a control action so the next
// poll reflects the new state instead of a stale one.
func (c *Cast) invalidateStatus() {
	c.mu.Lock()
	c.lastStatusAt = time.Time{}
	c.mu.Unlock()
}

// Position reports absolute position and total duration. In transcode mode the
// TV's reported position is segment-relative, so we add the segment offset and
// substitute the probed duration (a fragmented stream has no total duration).
func (c *Cast) Position(ctx context.Context) (pos, dur time.Duration, err error) {
	// While starting, don't poll the TV (avoid contending with the start
	// handshake); report 0 against the known duration so the TUI can draw its bar.
	if c.starting.Load() {
		return 0, c.knownDuration, nil
	}
	st, err := c.status(ctx)
	if err != nil {
		return 0, 0, err
	}
	pos, dur = st.Pos, st.Dur
	c.mu.Lock()
	build, off := c.buildArgs, c.ssOffset
	c.mu.Unlock()
	if build != nil {
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
	s.State, _ = c.PlayerState(ctx)
	if c.hasVol {
		s.Volume, _ = c.sess.Volume(ctx)
	}
	return s
}

// Status is a snapshot of a live cast.
type Status struct {
	State     engine.PlayerState
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
	// Ignore seeks issued before playback has actually started; the position is
	// still 0 and a seek would collide with the start handshake.
	if c.starting.Load() {
		return nil
	}
	build := c.currentBuild()
	if build == nil {
		c.invalidateStatus()
		return c.sess.Seek(ctx, pos)
	}
	if pos < 0 {
		pos = 0
	}

	// One restart at a time; a queued seek runs to the latest target afterward.
	// SetSubtitle takes the same lock, so a switch and a seek never interleave.
	c.seekMu.Lock()
	defer c.seekMu.Unlock()

	// Re-snapshot under seekMu: a switch may have swapped the builder (or flipped
	// us to direct-play) between the early check and acquiring the lock.
	build = c.currentBuild()
	if build == nil {
		c.invalidateStatus()
		return c.sess.Seek(ctx, pos)
	}

	c.srv.SetTranscode(build(pos))
	c.media.URL = c.srv.MediaURL()

	// The restart rebases the stream to 0, so re-cut the soft subtitle from pos;
	// otherwise every cue runs pos late (i.e. effectively no subtitles). Serving a
	// fresh URL is what makes the TV re-fetch instead of reusing the old cues.
	// Best-effort: a subtitle failure must not cost the user the seek itself.
	if c.resoft != nil {
		rctx, rcancel := context.WithTimeout(ctx, 45*time.Second)
		if err := c.resoft(rctx, pos, &c.media); err != nil {
			c.app.emit(Warn, "subtitles after seek: %v", err)
		}
		rcancel()
	}

	// Re-point the device at the fresh stream. The engine owns the flaky-TV
	// handling: Load stops the old stream, waits the transport out, and retries;
	// Play waits and retries the same way.
	if err := c.sess.Load(ctx, c.media); err != nil {
		return err
	}
	if err := c.sess.Play(ctx); err != nil {
		return err
	}

	c.invalidateStatus()
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
		// Abort any in-flight start handshake and let it unwind before we tear the
		// server down, so its background SOAP isn't still talking to the TV.
		if c.startCancel != nil {
			c.startCancel()
		}
		if c.startDone != nil {
			select {
			case <-c.startDone:
			case <-time.After(2 * time.Second):
			}
		}
		c.persistResume()
		if c.releaseAwake != nil {
			c.releaseAwake()
		}
		if c.sess != nil {
			_ = c.sess.Close()
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
	if err := a.Doctor(ctx); err != nil {
		return nil, nil, err
	}
	if _, err := os.Stat(req.File); err != nil {
		return nil, nil, fmt.Errorf("file: %w", err)
	}

	// selectDevice owns its own discovery timeouts (quick pass + deep fallback).
	dev, err := a.selectDevice(ctx, req.Target, req.Engine)
	if err != nil {
		return nil, nil, err
	}
	a.emit(Info, "Casting to %s [%s]", dev.Name(), dev.Addr())

	// Remember this device so a future bare `movcaster <file>` finds it again.
	_ = a.store.Save(config.Config{LastDeviceHost: dev.Host(), LastDeviceEngine: dev.Engine()})

	prof := a.DeviceProfile(dev)
	prep, err := a.PrepareWithProfile(ctx, req, prof, dev.Engine())
	if err != nil {
		return nil, nil, err
	}
	if prep.DecideErr != nil {
		return nil, nil, prep.DecideErr
	}
	if prep.ProbeErr != nil {
		// Doctor already proved ffprobe runs, so a failure here is specific to this
		// file. Say what we lose: no embedded-subtitle detection and no codec check,
		// so we direct-play by container and apply only an explicit/sidecar subtitle.
		a.emit(Warn, "could not probe %s: %v", filepath.Base(prep.AbsPath), prep.ProbeErr)
		a.emit(Warn, "casting without probe data: no embedded-subtitle or codec detection (direct-play only) — pass --sub for subtitles, --transcode to force re-encode")
	}
	abs := prep.AbsPath

	srv, err := a.newServer(dev.Host())
	if err != nil {
		return nil, nil, err
	}
	srv.Start()

	sess, err := dev.Open(ctx)
	if err != nil {
		_ = srv.Shutdown(context.Background())
		return nil, nil, err
	}

	tmpDir, err := os.MkdirTemp("", "movcaster-")
	if err != nil {
		_ = sess.Close()
		_ = srv.Shutdown(context.Background())
		return nil, nil, err
	}

	media := engine.Media{Title: filepath.Base(abs)}
	// Advertise the real duration. Direct-play files declare it in their
	// container anyway, but a live transcode streams an empty-moov fragmented
	// MP4 with no total duration, so the TV's seek bar races without this hint
	// (DIDL res@duration on DLNA, the LOAD duration on Cast).
	if prep.Info != nil {
		media.Duration = prep.Info.Duration
	}

	// Resume: if we have a saved position for this file, start there. We compute it
	// before building delivery so a transcode can begin ffmpeg at the offset in one
	// shot; a direct-play file seeks after Play. resumeOffset ignores negligible/
	// finished positions.
	knownDur := time.Duration(0)
	if prep.Info != nil {
		knownDur = prep.Info.Duration
	}
	startOffset := time.Duration(0)
	if a.resume != nil {
		startOffset = resumeOffset(a.resume.Get(abs), knownDur)
	}

	// buildDelivery applies the subtitle strategy + codec fallback against the
	// server/media. It uses the resolved sidecar (Prepare's auto-detected one),
	// not the raw --sub flag. It is silent, so Start reports the outcome itself.
	subOpts := req.Subtitle
	subOpts.Sidecar = prep.Sidecar
	label, build, resoft, err := a.buildDelivery(srv, &media, abs, tmpDir, prep.Info, subOpts, prep.Audio, req.ForceTranscode, startOffset, prof)
	if err != nil {
		_ = sess.Close()
		_ = srv.Shutdown(context.Background())
		_ = os.RemoveAll(tmpDir)
		return nil, nil, err
	}
	switch label {
	case "transcode":
		a.emit(Info, "Transcoding for codec compatibility...")
	case "remux":
		a.emit(Info, "Remuxing for container compatibility...")
	case "":
	default:
		a.emit(Info, "Subtitles: %s", label)
	}
	if al := audioTrackLabel(prep.Info, prep.Audio); al != "" {
		a.emit(Info, "Audio: %s", al)
	}
	if startOffset > 0 {
		a.emit(Info, "Resuming at %s", clock(startOffset))
	}

	// A resume on a direct-play file: engines that honor a load-time offset (Cast)
	// start there in one shot, which replaces the whole post-Play retry-a-seek
	// dance. Engines that don't (DLNA) get the post-Play seek below. A transcode
	// bakes the offset into the stream itself, so StartAt stays 0 there.
	if build == nil && startOffset > 0 && prof.HonorsStartAt {
		media.StartAt = startOffset
	}

	// Hand the media to the device synchronously so we can still fail Start
	// cleanly if it is rejected. Load returns as soon as the device accepts it, at
	// which point most devices start buffering/auto-playing — the slow bit
	// (waiting out buffering, Play, then a possible resume seek) runs off the
	// critical path below so the TUI renders immediately instead of freezing for
	// the seconds a large direct-play file takes to buffer.
	setCtx, setCancel := context.WithTimeout(ctx, 15*time.Second)
	err = sess.Load(setCtx, media)
	setCancel()
	if err != nil {
		_ = sess.Close()
		_ = srv.Shutdown(context.Background())
		_ = os.RemoveAll(tmpDir)
		return nil, nil, err
	}

	choices := buildSubChoices(prep.Info, prep.Sidecar)
	c := &Cast{
		sess:          sess,
		srv:           srv,
		media:         media,
		profile:       prof,
		knownDuration: knownDur,
		buildArgs:     build,
		resoft:        resoft,
		title:         filepath.Base(abs),
		device:        dev.Name(),
		subInfo:       label,
		hasVol:        sess.HasVolume(),
		tmpDir:        tmpDir,
		resume:        a.resume,
		resumePath:    abs,
		// Live subtitle switching state (see SetSubtitle).
		app:            a,
		abs:            abs,
		info:           prep.Info,
		forceTranscode: req.ForceTranscode,
		subChoices:     choices,
		activeSub:      activeSubFor(choices, prep.Strategy, prep.Sidecar),
		subOpts:        subOpts,
		audio:          prep.Audio,
		audioChoices:   buildAudioChoices(prep.Info),
		// Hold an idle-sleep assertion so a sleeping display doesn't stall the
		// stream; Close releases it. (See inhibitSleep.)
		releaseAwake: inhibitSleep(),
	}
	if build != nil {
		c.ssOffset = startOffset // transcode stream starts at this absolute offset
	}

	// Finish playback startup in the background: Play (which the engine may block
	// on while the device buffers), then, when the engine could not honor
	// StartAt, the direct-play resume seek. While this runs, the Cast's
	// read/control methods short-circuit (see starting) so the TUI can render and
	// poll without contending with the handshake. Uses its own context (Start's
	// ctx may be short-lived); Close cancels it.
	c.starting.Store(true)
	startCtx, startCancel := context.WithTimeout(context.Background(), 45*time.Second)
	c.startCancel = startCancel
	c.startDone = make(chan struct{})
	go func() {
		defer close(c.startDone)
		defer startCancel()
		defer c.starting.Store(false)
		if err := sess.Play(startCtx); err != nil {
			return
		}
		// Direct-play resume without StartAt support: seek after Play
		// (best-effort; the engine retries a mid-transition rejection itself).
		// Transcode resume is already baked into the stream offset.
		if build == nil && startOffset > 0 && !prof.HonorsStartAt {
			_ = sess.Seek(startCtx, startOffset)
		}
	}()

	return c, prep, nil
}

// applyTranscode points the server + media at the transcode stream starting at
// the given offset (0 for a fresh cast; the resume position when resuming).
func applyTranscode(srv MediaServer, media *engine.Media, build func(time.Duration) []string, ss time.Duration) {
	srv.SetTranscode(build(ss))
	media.URL = srv.MediaURL()
	media.MIME = "video/mp4"
	media.Seekable = false
}

// buildDelivery resets the server+media to a direct-play baseline, then applies
// the chosen subtitle strategy and the codec/audio-track fallback, returning a UI
// label and (for transcode modes) the ffmpeg arg builder. audio is the a:N stream
// the cast carries. ss is the absolute
// offset a transcode stream should start at (0 for a fresh cast; the resume/seek
// position otherwise). It performs no TV I/O and never emits, so it is safe to
// call during a live switch without corrupting the TUI. It is idempotent across
// mode flips: each call re-establishes the baseline before applying.
func (a *App) buildDelivery(srv MediaServer, media *engine.Media, abs, tmpDir string,
	info *probe.MediaInfo, sub SubtitleOptions, audio int, forceTranscode bool, ss time.Duration,
	prof engine.Profile,
) (label string, build func(time.Duration) []string, resoft softSubFn, err error) {

	// Baseline: direct-play the original file, no subs.
	srv.SetDirectPlay(abs, mimeForExt(abs))
	media.URL, media.MIME, media.Seekable = srv.MediaURL(), mimeForExt(abs), true
	media.Subtitle, media.StartAt = nil, 0

	dec, derr := subs.Decide(subs.Request{
		Info: info, Profile: prof, SidecarPath: sub.Sidecar,
		NoSubs: sub.NoSubs, ForceBurn: sub.Burn, ForceSoft: sub.Soft,
		MuxSoftTry: sub.MuxSoft, TrackIndex: sub.TrackIndex,
	})
	if derr != nil {
		return "", nil, nil, derr
	}

	// The codec/container plan has to be known *before* the subtitles are
	// prepared: a transcoded (or remuxed) stream is rebased to start at 0, so
	// soft cue times must be rebased by the same ss to stay in step (direct-play
	// keeps the file's own absolute timeline, so it stays at 0).
	plan := codecPlan(info, prof, forceTranscode, audio)
	subOffset := time.Duration(0)
	if plan.Kind != TranscodeNone {
		subOffset = ss
	}

	label, build, resoft, err = applySubtitles(srv, media, abs, tmpDir, dec, audio, ss, subOffset)
	if err != nil {
		return "", nil, nil, err
	}

	// Codec/container/audio-track fallback: only when subs didn't already force a
	// transcode. A mux-soft file already carries just the chosen track, so it
	// needs no audio-track remux (a container remux still applies).
	needed := plan.Kind == TranscodeCodec || plan.Remux ||
		(plan.Kind == TranscodeRemux && dec.Kind != subs.MuxSoft)
	if build == nil && needed {
		tplan := plan.plan()
		build = func(s time.Duration) []string { return transcode.Args(abs, s, prof, tplan, audio) }
		if label == "" {
			switch {
			case plan.Kind == TranscodeCodec:
				label = "transcode"
			case plan.Remux:
				label = "remux"
			}
		}
		applyTranscode(srv, media, build, ss)
	}
	return label, build, resoft, nil
}

// softSubFn re-prepares the soft subtitle for a transcode segment starting at ss
// and re-points the server + the given media at it. Seek calls it on every
// seek-restart: the restart rebases the stream to 0, so the cue times must be
// rebased too or the subtitles run ss late. nil when the delivery serves no
// rebasable soft subs (direct-play, burn-in, or none).
//
// The media is a parameter rather than captured because Start hands buildDelivery
// a local engine.Media that is then *copied* into the Cast: a captured pointer
// would keep updating the dead local, leaving the live metadata pointing at the
// previous segment's caption URL.
type softSubFn func(ctx context.Context, ss time.Duration, media *engine.Media) error

// serveSoft points the server + media at a soft-subtitle file and refreshes the
// caption URL. The MIME/type follow the decision's format, not the extension.
func serveSoft(srv MediaServer, media *engine.Media, path string, format engine.SubFormat) {
	mime := format.MIME()
	srv.SetSubtitle(path, mime)
	media.Subtitle = &engine.Subtitle{URL: srv.SubURL(), Format: format, MIME: mime}
}

// clearSoft drops the caption reference (used when no cues remain after ss).
func clearSoft(media *engine.Media) {
	media.Subtitle = nil
}

// applySubtitles executes the chosen subtitle strategy against the server +
// media metadata, returning a UI label and (for burn-in) a transcode-args
// builder starting at offset ss. It is silent (no events, no prints) so the same
// helper serves both the initial cast and a live switch. audio is the a:N stream
// a burn-in or mux-soft output carries.
// subOffset is the offset the served cue times must be rebased by (0 to keep the
// file's absolute timeline); see softSubFn.
func applySubtitles(srv MediaServer, media *engine.Media, abs, tmpDir string, dec subs.Decision, audio int, ss, subOffset time.Duration) (label string, build func(time.Duration) []string, resoft softSubFn, err error) {
	switch dec.Kind {
	case subs.None:
		return "", nil, nil, nil

	case subs.SoftSidecar:
		sidecar, format := dec.SidecarPath, dec.Format
		resoft = func(ctx context.Context, at time.Duration, m *engine.Media) error {
			shifted, err := subs.ShiftText(ctx, sidecar, at, tmpDir, format)
			if subs.NoCuesAfterOffset(err) {
				clearSoft(m)
				return nil
			} else if err != nil {
				return err
			}
			serveSoft(srv, m, shifted, format)
			return nil
		}
		sctx, scancel := context.WithTimeout(context.Background(), 60*time.Second)
		defer scancel()
		if err := resoft(sctx, subOffset, media); err != nil {
			return "", nil, nil, err
		}
		label = "soft: " + filepath.Base(sidecar)
		return label, nil, resoft, nil

	case subs.SoftExtract:
		idx, format := dec.Track.SubIndex, dec.Format
		resoft = func(ctx context.Context, at time.Duration, m *engine.Media) error {
			extracted, err := subs.ExtractText(ctx, abs, idx, at, tmpDir, format)
			if subs.NoCuesAfterOffset(err) {
				clearSoft(m)
				return nil
			} else if err != nil {
				return err
			}
			serveSoft(srv, m, extracted, format)
			return nil
		}
		ectx, ecancel := context.WithTimeout(context.Background(), 60*time.Second)
		defer ecancel()
		if err := resoft(ectx, subOffset, media); err != nil {
			return "", nil, nil, err
		}
		label = fmt.Sprintf("soft: embedded track %d", idx)
		return label, nil, resoft, nil

	case subs.MuxSoft:
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Minute)
		defer cancel()
		muxed, err := subs.MuxSoftRemux(ctx, abs, *dec.Track, audio, tmpDir)
		if err != nil {
			return "", nil, nil, err
		}
		srv.SetDirectPlay(muxed, "video/x-matroska")
		media.URL, media.MIME, media.Seekable = srv.MediaURL(), "video/x-matroska", true
		label = fmt.Sprintf("mux-soft: track %d (toggle subs on the TV)", dec.Track.SubIndex)
		return label, nil, nil, nil

	case subs.Burn:
		track := *dec.Track
		build = func(s time.Duration) []string { return subs.BurnArgs(abs, track, audio, s) }
		applyTranscode(srv, media, build, ss)
		label = fmt.Sprintf("burn-in: track %d (%s)", dec.Track.SubIndex, dec.Track.Codec)
		return label, build, nil, nil
	}
	return "", nil, nil, nil
}

// subChoice is one entry in the live subtitle picker: a display label plus the
// SubtitleOptions that recreate that delivery via buildDelivery.
type subChoice struct {
	label string
	opts  SubtitleOptions
}

// buildSubChoices enumerates the picker entries: the sidecar (if any), each
// embedded track, and an explicit Off. Selecting a track re-runs the auto
// strategy for that specific SubIndex (text -> soft, bitmap -> burn).
func buildSubChoices(info *probe.MediaInfo, sidecar string) []subChoice {
	var cs []subChoice
	if sidecar != "" {
		cs = append(cs, subChoice{
			label: "Sidecar: " + filepath.Base(sidecar),
			opts:  SubtitleOptions{Sidecar: sidecar, TrackIndex: -1},
		})
	}
	if info != nil {
		for _, t := range info.Subtitles {
			cs = append(cs, subChoice{
				label: subTrackLabel(t),
				opts:  SubtitleOptions{TrackIndex: t.SubIndex},
			})
		}
	}
	cs = append(cs, subChoice{label: "Off", opts: SubtitleOptions{NoSubs: true, TrackIndex: -1}})
	return cs
}

// subTrackLabel formats one embedded subtitle track as a single terminal row.
func subTrackLabel(t probe.SubTrack) string {
	var b strings.Builder
	fmt.Fprintf(&b, "Track s:%d", t.SubIndex)
	if t.Language != "" {
		fmt.Fprintf(&b, " — %s", languageName(t.Language))
	}
	if t.Title != "" {
		fmt.Fprintf(&b, " %q", t.Title)
	}
	fmt.Fprintf(&b, " (%s, %s)", t.Codec, t.Kind)
	if t.Default {
		b.WriteString(" (default)")
	}
	if t.Forced {
		b.WriteString(" (forced)")
	}
	return b.String()
}

// languageName turns an ISO 639 subtitle language tag (as ffprobe reports it,
// usually a 3-letter 639-2/B code like "eng", sometimes a 2-letter 639-1 code
// like "en") into a human-readable name. Unknown or "und"/empty tags fall back
// to the raw value so nothing is lost.
func languageName(code string) string {
	if name, ok := langNames[strings.ToLower(code)]; ok {
		return name
	}
	return code
}

// langNames maps the subtitle language codes commonly seen in media files to
// their English names. Both 639-2/B and 639-1 forms are included.
var langNames = map[string]string{
	"eng": "English", "en": "English",
	"spa": "Spanish", "es": "Spanish",
	"fre": "French", "fra": "French", "fr": "French",
	"ger": "German", "deu": "German", "de": "German",
	"ita": "Italian", "it": "Italian",
	"por": "Portuguese", "pt": "Portuguese",
	"dut": "Dutch", "nld": "Dutch", "nl": "Dutch",
	"rus": "Russian", "ru": "Russian",
	"jpn": "Japanese", "ja": "Japanese",
	"chi": "Chinese", "zho": "Chinese", "zh": "Chinese",
	"kor": "Korean", "ko": "Korean",
	"ara": "Arabic", "ar": "Arabic",
	"hin": "Hindi", "hi": "Hindi",
	"pol": "Polish", "pl": "Polish",
	"tur": "Turkish", "tr": "Turkish",
	"swe": "Swedish", "sv": "Swedish",
	"nor": "Norwegian", "no": "Norwegian",
	"dan": "Danish", "da": "Danish",
	"fin": "Finnish", "fi": "Finnish",
	"gre": "Greek", "ell": "Greek", "el": "Greek",
	"heb": "Hebrew", "he": "Hebrew",
	"cze": "Czech", "ces": "Czech", "cs": "Czech",
	"hun": "Hungarian", "hu": "Hungarian",
	"tha": "Thai", "th": "Thai",
	"vie": "Vietnamese", "vi": "Vietnamese",
	"ind": "Indonesian", "id": "Indonesian",
	"ukr": "Ukrainian", "uk": "Ukrainian",
	"ron": "Romanian", "rum": "Romanian", "ro": "Romanian",
}

// activeSubFor returns the index into choices that matches the strategy chosen at
// Start, defaulting to the Off entry (always last) when nothing matches.
func activeSubFor(choices []subChoice, dec subs.Decision, sidecar string) int {
	off := len(choices) - 1
	switch dec.Kind {
	case subs.None:
		return off
	case subs.SoftSidecar:
		if sidecar != "" {
			for i, c := range choices {
				if c.opts.Sidecar == sidecar {
					return i
				}
			}
		}
	default:
		if dec.Track != nil {
			for i, c := range choices {
				if !c.opts.NoSubs && c.opts.Sidecar == "" && c.opts.TrackIndex == dec.Track.SubIndex {
					return i
				}
			}
		}
	}
	return off
}

// SubtitleChoices returns the picker labels (sidecar, each embedded track, Off).
func (c *Cast) SubtitleChoices() []string {
	out := make([]string, len(c.subChoices))
	for i, ch := range c.subChoices {
		out[i] = ch.label
	}
	return out
}

// ActiveSubtitle returns the index of the currently active picker choice.
func (c *Cast) ActiveSubtitle() int {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.activeSub
}

// SetSubtitle switches to the subtitle choice at idx (into SubtitleChoices()),
// rebuilding the delivery for that track and restarting playback at the current
// position. The audio track is kept. A switch always resumes playback (issues
// Play), even if the cast was paused.
func (c *Cast) SetSubtitle(ctx context.Context, idx int) error {
	if idx < 0 || idx >= len(c.subChoices) {
		return fmt.Errorf("subtitle index %d out of range", idx)
	}
	return c.restartWith(ctx, func(sub *SubtitleOptions, _ *int) {
		*sub = c.subChoices[idx].opts
	}, func() { c.activeSub = idx })
}

// AudioChoices returns the audio picker labels, indexed by a:N.
func (c *Cast) AudioChoices() []string { return c.audioChoices }

// ActiveAudio returns the a:N index of the audio track being cast.
func (c *Cast) ActiveAudio() int {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.audio
}

// AudioInfo labels the live audio track, or "" for a single-track file.
func (c *Cast) AudioInfo() string {
	c.mu.Lock()
	defer c.mu.Unlock()
	return audioTrackLabel(c.info, c.audio)
}

// SetAudio switches to audio track a:idx, keeping the subtitle delivery, and
// restarts playback at the current position (direct play when the TV picks that
// track itself, else a copy-only remux). Like SetSubtitle it always resumes.
func (c *Cast) SetAudio(ctx context.Context, idx int) error {
	if idx < 0 || idx >= len(c.audioChoices) {
		return fmt.Errorf("audio track %d out of range", idx)
	}
	return c.restartWith(ctx, func(_ *SubtitleOptions, audio *int) { *audio = idx }, nil)
}

// restartWith rebuilds the delivery from the live subtitle options and audio
// track, as edited by change, and re-points the TV at it from the current
// position. commit (optional) runs under c.mu alongside the state update. It is
// serialized against seek-restarts via seekMu, since both mutate the server/media
// and buildArgs.
func (c *Cast) restartWith(ctx context.Context, change func(sub *SubtitleOptions, audio *int), commit func()) error {
	c.seekMu.Lock()
	defer c.seekMu.Unlock()

	// Resume where we are now. lastPos is the most recent observed absolute position.
	c.mu.Lock()
	pos, sub, audio := c.lastPos, c.subOpts, c.audio
	c.mu.Unlock()
	if pos < 0 {
		pos = 0
	}
	change(&sub, &audio)

	label, build, resoft, err := c.app.buildDelivery(c.srv, &c.media, c.abs, c.tmpDir,
		c.info, sub, audio, c.forceTranscode, pos, c.profile) // ss=pos => transcode starts at pos in one shot
	if err != nil {
		return err
	}

	// Direct-play resume on an engine with load-time offsets: bake the position
	// into the Load instead of seeking afterward.
	if build == nil && pos > 0 && c.profile.HonorsStartAt {
		c.media.StartAt = pos
	}

	// The server now serves the new delivery, so record it before touching the TV:
	// if the handshake below fails, later seeks must still match what is streamed.
	c.mu.Lock()
	c.buildArgs = build
	c.resoft = resoft
	if build != nil {
		c.ssOffset = pos
	} else {
		c.ssOffset = 0
	}
	c.subInfo = label
	c.subOpts = sub
	c.audio = audio
	if commit != nil {
		commit()
	}
	c.mu.Unlock()

	// Re-point the TV at the fresh stream and resume. The engine owns the settle
	// waits and retries inside Load/Play (and accepts a rejected Play when the TV
	// is playing anyway).
	if err := c.sess.Load(ctx, c.media); err != nil {
		return err
	}
	if err := c.sess.Play(ctx); err != nil {
		return err
	}
	// Direct-play resume without StartAt support: seek after Play (the engine
	// retries a mid-transition rejection itself).
	if build == nil && pos > 0 && !c.profile.HonorsStartAt {
		_ = c.sess.Seek(ctx, pos)
	}
	c.invalidateStatus()
	return nil
}
