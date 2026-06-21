package core

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"github.com/juliocesar/movcaster/internal/config"
	"github.com/juliocesar/movcaster/internal/probe"
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

	// resume persistence (nil => disabled)
	resume     resumeStore
	resumePath string // absolute file path, key into the resume store

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
	build := c.currentBuild()
	if build == nil {
		return c.r.Seek(ctx, pos)
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
		return c.r.Seek(ctx, pos)
	}

	// Stop the current stream (best-effort, short timeout) and let the TV settle
	// before we hand it a new URI; swapping while it's still tearing down the old
	// stream is what makes SetAVTransportURI hang.
	stopCtx, cancel := context.WithTimeout(ctx, 4*time.Second)
	_ = c.r.Stop(stopCtx)
	cancel()
	if !sleepCtx(ctx, 500*time.Millisecond) {
		return ctx.Err()
	}

	c.srv.SetTranscode(build(pos))
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
	if err := a.Doctor(ctx); err != nil {
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
		// Doctor already proved ffprobe runs, so a failure here is specific to this
		// file. Say what we lose: no embedded-subtitle detection and no codec check,
		// so we direct-play by container and apply only an explicit/sidecar subtitle.
		a.emit(Warn, "could not probe %s: %v", filepath.Base(prep.AbsPath), prep.ProbeErr)
		a.emit(Warn, "casting without probe data: no embedded-subtitle or codec detection (direct-play only) — pass --sub for subtitles, --transcode to force re-encode")
	}
	abs := prep.AbsPath

	srv, err := a.newServer(dev.Location.Host)
	if err != nil {
		return nil, nil, err
	}
	srv.Start()
	rend := a.newRenderer(*dev)

	tmpDir, err := os.MkdirTemp("", "movcaster-")
	if err != nil {
		_ = srv.Shutdown(context.Background())
		return nil, nil, err
	}

	media := renderer.Media{Title: filepath.Base(abs)}
	// Advertise the real duration in DIDL-Lite. Direct-play files declare it in
	// their container anyway, but a live transcode streams an empty-moov fragmented
	// MP4 with no total duration, so the TV's seek bar races without this hint.
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
	label, build, err := a.buildDelivery(srv, &media, abs, tmpDir, prep.Info, subOpts, req.ForceTranscode, startOffset)
	if err != nil {
		_ = srv.Shutdown(context.Background())
		_ = os.RemoveAll(tmpDir)
		return nil, nil, err
	}
	if label == "transcode" {
		a.emit(Info, "Transcoding for codec compatibility...")
	} else if label != "" {
		a.emit(Info, "Subtitles: %s", label)
	}
	if startOffset > 0 {
		a.emit(Info, "Resuming at %s", clock(startOffset))
	}

	setCtx, setCancel := context.WithTimeout(ctx, 45*time.Second)
	defer setCancel()
	if err := startPlayback(setCtx, rend, media); err != nil {
		_ = srv.Shutdown(context.Background())
		_ = os.RemoveAll(tmpDir)
		return nil, nil, err
	}

	// Direct-play resume: seek after Play (best-effort; the TV may need a moment
	// to leave the TRANSITIONING state, so retry briefly). Transcode resume is
	// already baked into the stream offset above.
	if build == nil && startOffset > 0 {
		_ = retrySOAP(setCtx, 4, 3*time.Second, func(c context.Context) error {
			return rend.Seek(c, startOffset)
		})
	}

	choices := buildSubChoices(prep.Info, prep.Sidecar)
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
		// Live subtitle switching state (see SetSubtitle).
		app:            a,
		abs:            abs,
		info:           prep.Info,
		forceTranscode: req.ForceTranscode,
		subChoices:     choices,
		activeSub:      activeSubFor(choices, prep.Strategy, prep.Sidecar),
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

// buildDelivery resets the server+media to a direct-play baseline, then applies
// the chosen subtitle strategy and codec-compatibility fallback, returning a UI
// label and (for transcode modes) the ffmpeg arg builder. ss is the absolute
// offset a transcode stream should start at (0 for a fresh cast; the resume/seek
// position otherwise). It performs no TV I/O and never emits, so it is safe to
// call during a live switch without corrupting the TUI. It is idempotent across
// mode flips: each call re-establishes the baseline before applying.
func (a *App) buildDelivery(srv MediaServer, media *renderer.Media, abs, tmpDir string,
	info *probe.MediaInfo, sub SubtitleOptions, forceTranscode bool, ss time.Duration,
) (label string, build func(time.Duration) []string, err error) {

	// Baseline: direct-play the original file, no subs.
	srv.SetDirectPlay(abs, mimeForExt(abs))
	media.URL, media.MIME, media.Seekable = srv.MediaURL(), mimeForExt(abs), true
	media.SubURL, media.SubMIME, media.SubType = "", "", ""

	dec, derr := subs.Decide(subs.Request{
		Info: info, SidecarPath: sub.Sidecar,
		NoSubs: sub.NoSubs, ForceBurn: sub.Burn, ForceSoft: sub.Soft,
		MuxSoftTry: sub.MuxSoft, TrackIndex: sub.TrackIndex,
	})
	if derr != nil {
		return "", nil, derr
	}

	label, build, err = applySubtitles(srv, media, abs, tmpDir, dec, ss)
	if err != nil {
		return "", nil, err
	}

	// Codec-compatibility fallback: only when subs didn't already force a transcode.
	if build == nil {
		plan := codecPlan(info, forceTranscode)
		if plan.Kind == TranscodeCodec {
			build = func(s time.Duration) []string { return transcode.Args(abs, s, plan.Video, plan.Audio) }
			if label == "" {
				label = "transcode"
			}
			applyTranscode(srv, media, build, ss)
		}
	}
	return label, build, nil
}

// applySubtitles executes the chosen subtitle strategy against the server +
// media metadata, returning a UI label and (for burn-in) a transcode-args
// builder starting at offset ss. It is silent (no events, no prints) so the same
// helper serves both the initial cast and a live switch.
func applySubtitles(srv MediaServer, media *renderer.Media, abs, tmpDir string, dec subs.Decision, ss time.Duration) (label string, build func(time.Duration) []string, err error) {
	switch dec.Kind {
	case subs.None:
		return "", nil, nil

	case subs.SoftSidecar:
		mime, typ := subKind(dec.SidecarPath)
		srv.SetSubtitle(dec.SidecarPath, mime)
		media.SubURL, media.SubMIME, media.SubType = srv.SubURL(), mime, typ
		label = "soft: " + filepath.Base(dec.SidecarPath)
		return label, nil, nil

	case subs.SoftExtract:
		ectx, ecancel := context.WithTimeout(context.Background(), 60*time.Second)
		defer ecancel()
		vtt, err := subs.ExtractText(ectx, abs, dec.Track.SubIndex, tmpDir)
		if err != nil {
			return "", nil, err
		}
		srv.SetSubtitle(vtt, "text/vtt")
		media.SubURL, media.SubMIME, media.SubType = srv.SubURL(), "text/vtt", "vtt"
		label = fmt.Sprintf("soft: embedded track %d", dec.Track.SubIndex)
		return label, nil, nil

	case subs.MuxSoft:
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Minute)
		defer cancel()
		muxed, err := subs.MuxSoftRemux(ctx, abs, *dec.Track, tmpDir)
		if err != nil {
			return "", nil, err
		}
		srv.SetDirectPlay(muxed, "video/x-matroska")
		media.URL, media.MIME, media.Seekable = srv.MediaURL(), "video/x-matroska", true
		label = fmt.Sprintf("mux-soft: track %d (toggle subs on the TV)", dec.Track.SubIndex)
		return label, nil, nil

	case subs.Burn:
		track := *dec.Track
		build = func(s time.Duration) []string { return subs.BurnArgs(abs, track, s) }
		applyTranscode(srv, media, build, ss)
		label = fmt.Sprintf("burn-in: track %d (%s)", dec.Track.SubIndex, dec.Track.Codec)
		return label, build, nil
	}
	return "", nil, nil
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
// position. It is serialized against seeks via seekMu. A switch always resumes
// playback (issues Play), even if the cast was paused.
func (c *Cast) SetSubtitle(ctx context.Context, idx int) error {
	if idx < 0 || idx >= len(c.subChoices) {
		return fmt.Errorf("subtitle index %d out of range", idx)
	}

	// Serialize against seek-restarts; both mutate the server/media + buildArgs.
	c.seekMu.Lock()
	defer c.seekMu.Unlock()

	// Resume where we are now. lastPos is the most recent observed absolute position.
	c.mu.Lock()
	pos := c.lastPos
	c.mu.Unlock()
	if pos < 0 {
		pos = 0
	}

	choice := c.subChoices[idx]
	label, build, err := c.app.buildDelivery(c.srv, &c.media, c.abs, c.tmpDir,
		c.info, choice.opts, c.forceTranscode, pos) // ss=pos => transcode starts at pos in one shot
	if err != nil {
		return err
	}

	// Re-point the TV at the fresh stream and resume (Stop -> settle -> SetURI -> Play).
	if err := startPlayback(ctx, c.r, c.media); err != nil {
		return err
	}
	// Direct-play resume: seek after Play (transcode resume is baked into ss above).
	if build == nil && pos > 0 {
		_ = retrySOAP(ctx, 4, 3*time.Second, func(ic context.Context) error { return c.r.Seek(ic, pos) })
	}

	c.mu.Lock()
	c.buildArgs = build
	if build != nil {
		c.ssOffset = pos
	} else {
		c.ssOffset = 0
	}
	c.subInfo = label
	c.activeSub = idx
	c.mu.Unlock()
	return nil
}

// startPlayback points the TV at media for a fresh cast: best-effort Stop, wait
// for the TV to leave the transitioning state, SetAVTransportURI, wait again for
// it to settle, then Play. webOS rejects both a new URI and a Play with 701
// "Transition not available" while it reports "LG_TRANSITIONING", so we poll its
// state at both points rather than sleeping a fixed interval (firing too early is
// exactly what triggers the 701). The pre-URI wait covers a TV left mid-transition
// by a previous cast; the pre-Play wait covers the TV buffering the freshly-set
// URI — which lingers for a live transcode resumed deep into the file (its ffmpeg
// needs seconds to -ss-seek and emit the first fragment). The retries cover
// residual flakiness. Mirrors the seek-restart sequence in Seek.
func startPlayback(ctx context.Context, r RendererControl, media renderer.Media) error {
	stopCtx, cancel := context.WithTimeout(ctx, 4*time.Second)
	_ = r.Stop(stopCtx)
	cancel()
	waitTransportSettled(ctx, r, 6*time.Second)

	if err := retrySOAP(ctx, 3, 9*time.Second, func(ic context.Context) error {
		return r.SetMedia(ic, media)
	}); err != nil {
		return fmt.Errorf("SetAVTransportURI: %w", err)
	}

	// After accepting the URI the TV fetches the media URL and sits in
	// TRANSITIONING while it buffers; it rejects Play with 701 until it leaves
	// that state. A fresh/offset-0 stream settles almost immediately, but a live
	// transcode resumed deep into the file (e.g. --resume burning bitmap subs
	// from 40 min in) needs several seconds to -ss-seek and emit its first MP4
	// fragment, so the TV lingers in TRANSITIONING long past the Play retries.
	// Wait it out before Play (returns at once in the common fast case). Budget
	// generously for a slow deep-offset transcode start; both waits fit the
	// caller's 45s setCtx.
	waitTransportSettled(ctx, r, 20*time.Second)

	if err := retrySOAP(ctx, 3, 7*time.Second, func(ic context.Context) error {
		return r.Play(ic)
	}); err != nil {
		return fmt.Errorf("Play: %w", err)
	}
	return nil
}

// waitTransportSettled blocks until the renderer reports a non-transitioning
// transport state (or the budget/ctx elapses). webOS reports "LG_TRANSITIONING"
// for a beat or two after a Stop, and accepts SetAVTransportURI only once it has
// left that state. Best-effort: a read error or timeout just returns, letting the
// retrying caller proceed.
func waitTransportSettled(ctx context.Context, r RendererControl, budget time.Duration) {
	wctx, cancel := context.WithTimeout(ctx, budget)
	defer cancel()
	for {
		sctx, scancel := context.WithTimeout(wctx, 2*time.Second)
		state, err := r.TransportState(sctx)
		scancel()
		if err == nil && !strings.Contains(state, "TRANSITIONING") {
			return
		}
		if !sleepCtx(wctx, 300*time.Millisecond) {
			return
		}
	}
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
