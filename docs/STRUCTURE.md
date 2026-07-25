# movcaster — codebase skeleton (for LLM consumption)

Terminal-only (CLI + bubbletea TUI) tool that streams a local video file to a DLNA
MediaRenderer (target: LG webOS TV) with soft *and* burned-in subtitles. Single Go
binary; shells out to `ffmpeg`/`ffprobe`. No GUI/web. See `plans/MOVCASTER_PLAN.md`
for rationale and `README.md` for user-facing usage.

Module: `github.com/juliocesar/movcaster` · Go 1.26 · deps: `huin/goupnp` (DLNA SOAP),
`charmbracelet/bubbletea|lipgloss|bubbles` (TUI), `razsteinmetz/go-ptn`
(filename episode parsing for auto-advance).

---

## Data flow (one cast)

All orchestration lives in `internal/core`. `main` is a thin client: parse flags →
build a `core.CastRequest` → call core → render `core.Event`s and drive the TUI.

```
main.runCast → tui.Start(title, startFn)    TUI grabs the terminal FIRST (connecting screen);
  │                                         startFn runs in a goroutine, emits → statusMsg lines
  └─ startFn: SetEventSink(→TUI) + core.App.Start(ctx, CastRequest)
       ├─ Doctor(ctx)                             ffmpeg+ffprobe on PATH *and* launch `-version`
       ├─ selectDevice(target)  ── DeviceFinder ─► SSDP find renderer (or saved/--t)
       │    └─ emit Event "Casting to …"          + Store.Save(LastDeviceHost)
       ├─ Prepare(): probe.Probe + subs.Decide    MediaInfo + subtitle Decision (no TV I/O)
       ├─ newServer(devHost)                      bind HTTP on LAN IP routable to TV
       │    └─ SetDirectPlay(file)                default: serve raw file (range-seek)
       ├─ media.Duration = info.Duration          DIDL res@duration (fixes transcode seek bar)
       ├─ buildDelivery(...)                       baseline direct-play → applySubtitles + codec
       │    ├─ applySubtitles(...)                 apply Decision; may switch server to transcode pipe
       │    └─ codecPlan(info) → transcode.Args    codec fallback if not already transcoding
       ├─ renderer.SetMedia(media) + Play()       SOAP SetAVTransportURI(+DIDL) then Play (async tail)
       └─ returns *core.Cast → readyMsg           TUI flips connecting → live view, drives the Cast
                                                  until quit → main closes it (server+ffmpeg+tmp)
```

The TV pulls media from our HTTP server; we push control via SOAP to the TV. Two
independent channels.

Planning (`--info`) reuses `core.Prepare` alone (no device, no TV): probe + decide,
then `Preparation.DescribeStreams()` / `DescribeStrategy()` / `DescribeCodec()` render the text.

---

## Packages

### `main` (main.go)
Thin CLI client: flag parsing + map to `core.CastRequest` + render events. No
orchestration logic.
- `main()` — flags: `-l -t -sub -no-subs -burn -soft -sub-track -mux-soft -transcode -info`.
  Builds one `core.App` with an `OnEvent` reporter. `-no-next` disables auto-advance.
  `-resume` (no file arg) casts the last played video via `resumeFile` (newest still-existing
  entry from `resume.Store.Recent()`, skipping missing ones), then runs like a normal cast;
  mutually exclusive with a file arg and `--playlist`.
  `-resume-last <pattern>` (no file arg) fuzzy-matches `pattern` against the played videos'
  base names via `resumeMatchFile` (→ `resume.Rank` over `Recent()`, newest-first on ties,
  skipping since-deleted entries) and resumes the best match; mutually exclusive with a file
  arg, `--playlist`, and `--resume`.
- `report(Event)` — `Info`→stdout, `Warn`→stderr with the `movcaster:` prefix. Used
  outside a cast (`-l`, `--info`, "Resuming:", "Up next:"); during a cast the sink is
  swapped to the TUI (below) so nothing prints under bubbletea.
- `runList(app)` — `-l`: `app.ListDevices` + print.
- `runInfo(app, req)` — `--info`: `app.Prepare` then print `DescribeStreams`/`DescribeStrategy`/`DescribeCodec`.
  ProbeErr is fatal (matches old behavior: aborts before printing); DecideErr prints
  streams then errors.
- `runCast(app, req, next, autoNext)` — loop: `tui.Start(base(file), startFn)` where
  startFn does `app.SetEventSink(→ emit)` (Warn events get a `"! "` prefix) then
  `app.Start(ctx, req)`; the TUI owns the terminal from t≈0 and shows the events on a
  connecting screen. After `tui.Start` returns, restore `SetEventSink(report)` and
  `Close` the returned controller (returned even if the user quit while connecting, so
  a late-started cast is never leaked). On `OutcomeEnded` (with `autoNext`) or
  `OutcomeNext` (the `n` key), call the `nextProvider` for the next request and cast
  it; otherwise return. Prints "Up next: <base>" between items (before the next
  `tui.Start` grabs the terminal).
- `nextProvider`/`nextEpisode` — `next(cur) (CastRequest, bool)` abstracts "what plays
  next". `nextEpisode` uses `nextep.Find` (same-dir episode); `-no-next` clears
  `autoNext` in this mode. Both providers carry subtitle/codec opts forward and clear
  the file-specific `Subtitle.Sidecar`.
- `runPlaylist(app, base, path)` — `-playlist`: `playlist.Load` → `existingFiles`
  (skip+warn missing/dir entries) → an index-closure `nextProvider` over the list,
  then `runCast(..., autoNext=true)` (a playlist always advances on end).

### `internal/core` — UI-agnostic orchestration (the reusable API)
One import exposes everything a front-end needs. No UI toolkit, no `fmt.Println`;
progress is reported via `Options.OnEvent`, status via the live `Cast`.
- `App` + `New(Options)` — holds injectable deps (`DeviceFinder`, `NewServer`,
  `NewRenderer`, `Store`, `OnEvent`); zero-value Options wires production impls.
- `SetEventSink(fn)` — swaps the `OnEvent` destination at runtime; the CLI routes
  cast-phase events into the TUI's connecting screen and restores its stdout reporter
  afterward. Single-goroutine use per cast (no lock); not safe for concurrent casts.
- `Doctor(ctx)` — ffmpeg/ffprobe on PATH *and* runnable (launches `-version`; a present-
  but-broken binary, e.g. a dangling Homebrew dylib, fails here with an actionable hint
  instead of silently degrading mid-cast). Was `ensureFFmpeg`.
- `ListDevices(ctx)` — discovery passthrough.
- `buildDelivery(...) → (label, build, resoft, err)` — baseline direct-play, then
  `subs.Decide` + `codecPlan` (computed *before* subtitles, since a transcode dictates the
  sub offset) + `applySubtitles`. `resoft` is the `softSubFn` `Seek`/`SetSubtitle` re-run.
- `Prepare(ctx, CastRequest) → *Preparation` — pure planning: probe + `subs.Decide`, no
  TV/network I/O. `Preparation{AbsPath, Info, Sidecar, Strategy, Codec, ProbeErr, DecideErr}` +
  `DescribeStreams()`/`DescribeStrategy()`/`DescribeCodec()`. `Codec` is the `codecPlan` result
  (needs no TV), so `--info` reports a codec-compat transcode; `Start` recomputes it since a
  burn-in strategy supersedes it. Reused by `--info` and `Start`.
- `Start(ctx, CastRequest) → (*Cast, *Preparation)` — resolve device (emit "Casting to",
  save config), bind server, `applySubtitles`, codec fallback (`codecPlan`), then
  `setTransportURI` (Stop→settle→retry SetURI) *synchronously* and return the `Cast`;
  the slow tail (`beginPlayback`: settle→retry Play, then a direct-play resume seek) runs
  in a **background goroutine**. This is deliberate: `Start` used to also block on the
  pre-Play settle + Play, which for a large *direct-play* file (the TV sits in
  `LG_TRANSITIONING` buffering a high-bitrate stream, and its control endpoint lags) delayed
  the caller by 14–45s — so the TUI never rendered and the terminal stayed cooked (echoed
  keys) while the movie already auto-played. Returning after SetURI (which the TV accepts
  fast, then auto-buffers) lets the TUI render at once and show buffering. Cleans up the
  server/tmp dir on any pre-return error; the goroutine uses its own 45s context, cancelled
  by `Close`/`Stop`.
- `startPlayback` / `setTransportURI` / `beginPlayback` (cast.go) — the fresh-cast control
  sequence, split so the fast half can run on the critical path and the slow half off it.
  `setTransportURI`: best-effort Stop, `waitTransportSettled`, `retrySOAP` SetAVTransportURI.
  `beginPlayback`: `waitTransportSettled` again, then `retrySOAP` Play. `startPlayback` = both
  (still used by `SetSubtitle`, which the TUI already gates via `switching`). webOS rejects
  both a new URI *and* a Play with 701 "Transition not available" while it reports
  `LG_TRANSITIONING`, so we poll its state (not a fixed sleep) at both points. The pre-URI
  wait clears a TV left mid-transition by a previous cast; the pre-Play wait covers the TV
  buffering the freshly-set URI — which lingers for a large direct-play file *and* for a
  transcode resumed deep into the file (e.g. `--resume` burning bitmap subs from 40 min in:
  ffmpeg needs seconds to `-ss`-seek and emit the first fragment, so Play fired immediately
  hits 701 even though small offset-0 casts settle at once). Mirrors Seek's seek-restart tail.
- `Cast` — the live, concurrency-safe handle (folds the former `internal/cast.Session`).
  While `Start`'s background tail runs, `starting` (atomic) makes the AVTransport
  read/control methods (`Position`/`TransportState`/`Play`/`Pause`/`Seek`) short-circuit
  *without SOAP* — `Position`→`(0, knownDur)`, `TransportState`→`TRANSITIONING`, controls→
  no-op — so the TUI's polling can't contend with the in-flight Stop/SetURI/Play handshake;
  it just shows a buffering state. `Stop`/`Close` cancel the handshake (`startCancel`) and
  `Close` drains `startDone` before shutting the server down.
  Implements the TUI control surface: `Play/Pause/Stop/Seek/Position/TransportState/
  HasVolume/Volume/SetVolume/Mute`, plus `Title/Device/SubInfo`, `Status(ctx)`, `Close(ctx)`.
  Direct-play vs transcode seek-restart logic (Stop→settle→retry SetURI/Play, `seekMu`
  serialized) lives here, moved verbatim from the old Session.
- Live subtitle switching: `SubtitleChoices()` / `ActiveSubtitle()` expose the picker
  (sidecar + each embedded track + Off), and `SetSubtitle(ctx, idx)` switches to a choice
  by rebuilding delivery via `buildDelivery` at the current position and restarting through
  `startPlayback` (+ a post-Play `Seek` for direct-play). Serialized against seeks (`seekMu`);
  `buildArgs`/`ssOffset`/`subInfo`/`activeSub` are mutated under `c.mu`, and `Seek`/`Position`
  snapshot `buildArgs` under the lock so a switch can swap modes mid-flight safely.
- `buildDelivery(srv, media, abs, tmpDir, info, SubtitleOptions, forceTranscode, ss)` —
  shared by `Start` and `SetSubtitle`. Resets to a direct-play baseline, then `subs.Decide` +
  `applySubtitles` + codec fallback, returning a label + transcode-args builder. Silent (no
  events) so a live switch can't corrupt the TUI; `Start` emits the label itself. Idempotent
  across mode flips. `applySubtitles` is a silent free function taking the transcode offset `ss`.
- Interfaces (consumer-side, with compile-time assertions): `DeviceFinder`,
  `RendererControl` (`*renderer.Renderer`), `MediaServer` (`*mediaserver.Server`), `Store`
  (`config`). Tests inject fakes; production defaults wire the real impls.
- `CastRequest`/`SubtitleOptions`/`TranscodePlan`/`Event`/`Status`/`Options` — public data.
- `inhibitSleep()` (power_darwin.go; no-op in power_other.go) — while a `Cast` is
  live it runs `caffeinate -i -w <pid>` to hold a PreventUserIdleSystemSleep
  assertion. Started in `Start` (stored as `Cast.releaseAwake`), released in
  `Cast.Close`. The display still sleeps; only the idle stall is prevented.
- internal helpers: `selectDevice` (no target: quick SSDP pass → saved/sole; if none
  answer, emit "Looking for a TV..." and `waitForDevice` retries up to 10s before erroring.
  target: `selectTarget` by host IP → description-URL load). `pickDevice` (saved → sole → nil),
  `resolveSubtitle`, `subKind`, `mimeForExt`, `hostOf`/`ensureScheme`, `applyTranscode`,
  `buildDelivery`/`applySubtitles`, `buildSubChoices`/`subTrackLabel`/`activeSubFor` (picker),
  `codecPlan`, `retrySOAP`, `sleepCtx`.

### `internal/discovery` — SSDP discovery (goupnp)
- `Device{FriendlyName, Location *url.URL, AVTransport *av1.AVTransport1, Rendering *av1.RenderingControl1}`.
  `Rendering` may be nil (no volume control).
- `Discover(ctx) []Device` — M-SEARCHes several targets concurrently (`AVTransport:1`,
  `device:MediaRenderer:1`, `ssdp:all`) via `goupnp.DiscoverDevicesCtx`, dedups roots by
  Location, then builds clients with `av1.New*ClientsFromRootDevice`. Broad on purpose:
  a just-woke webOS TV often answers one target while missing another.
- `FindByURL(ctx, loc) *Device` — load directly from a device-description URL (skips SSDP).
- `FindByHost(ctx, host) *Device` — targeted M-SEARCH to the multicast group filtered to
  one host, on its own longer any-ST window (webOS ignores unicast M-SEARCH). Learns the
  TV's dynamic control port and recovers a specific TV a broad `Discover` keeps missing;
  backs `-t <ip>` and saved-host resume.

### `internal/renderer` — AVTransport + RenderingControl + DIDL
Thin typed wrapper over goupnp `av1` clients. InstanceID always 0, channel "Master".
- `New(discovery.Device) *Renderer`.
- `Media{URL,Title,MIME,Duration,Seekable, SubURL,SubMIME,SubType}`.
- `SetMedia(ctx,Media)` → `SetAVTransportURICtx` with `buildDIDL(m)`.
- `buildDIDL(m) string` — DIDL-Lite. Seekable→`DLNA.ORG_OP=01` else `00`. Emits
  `res@duration` when `Duration>0`. Caption via `<res text/...>` + `sec:CaptionInfo`
  + `sec:CaptionInfoEx` (webOS honors these for TEXT subs). XML-escapes fields.
- `Play/Pause/Stop/Seek(REL_TIME)/Position/TransportState/HasVolume/Volume/SetVolume/Mute`.
- `formatDuration`(H:MM:SS) / `parseDuration` (tolerates `NOT_IMPLEMENTED`, fractions).

### `internal/mediaserver` — local HTTP server
Serves `/media*` and `/subs*` (prefix routing — URLs carry ext + `?t=token`, so exact
mux patterns don't match). `verbose` (`MOVCASTER_VERBOSE`) logs requests.
- `Server` holds current `Source{FilePath,MIME,Seekable,Transcode}`, optional `subtitle`,
  and a monotonic cache-buster `token`.
- `New(deviceHost)` — `localIPFor` dials UDP to TV to learn the routable local IP, binds `:0`.
- `SetDirectPlay(file,mime)` / `SetTranscode(args)` / `SetSubtitle(path,mime)` — each
  swaps `Source` and bumps token (kills prior ffmpeg via `Transcode.stop()`).
- `MediaURL()` → `http://ip:port/media<ext>?t=<token>` (ext=`.mp4` for transcode). `SubURL()`.
- `handleMedia` (directplay.go) — transcode→`serveTranscode`; else `http.ServeContent`
  (native byte ranges) with `setDLNAHeaders`.
- `serveTranscode` (transcode.go) — `setDLNAHeaders`+`WriteHeader(200)` FIRST (so SOAP
  doesn't block), then `exec.CommandContext(r.Context(), ffmpeg, args)`, `io.Copy(w, stdout)`.
  Client disconnect cancels ctx → ffmpeg dies. HTTP write back-pressure throttles ffmpeg.
- `setDLNAHeaders` — transferMode/realTimeInfo/contentFeatures.dlna.org; OP=01 if seekable.

### `internal/probe` — ffprobe wrapper + sub classification
- `Probe(ctx,path) *MediaInfo{Duration,VideoCodec,AudioCodec,Subtitles[]}` via
  `ffprobe -print_format json -show_streams -show_format`. Skips attached_pic (cover art).
- `SubTrack{Index, SubIndex (s:N selector), Codec, Kind, Language, Title, Default, Forced}`.
  `Forced` (disposition) marks a foreign-dialogue-only track that `selectTrack` deprioritizes.
- `SubKind`: `SubText` (subrip/ass/mov_text/webvtt…) | `SubBitmap` (dvd_subtitle/pgs/vobsub/dvbsub…) | `SubUnknown`.

### `internal/subs` — strategy + ffmpeg arg/command builders
- `Decide(Request) Decision` — the decision tree (strategy.go). `Kind`:
  `None | SoftSidecar | SoftExtract | MuxSoft | Burn`. Order: NoSubs→None; sidecar→SoftSidecar;
  no tracks→None; `selectTrack` (explicit `TrackIndex`, else prefer TEXT, else default/first;
  each preference skips `Forced` tracks when a non-forced peer exists via `preferNonForced` —
  a forced track only carries foreign-dialogue lines and is a confusing default on English
  releases); ForceSoft (err if bitmap)→SoftExtract; ForceBurn→Burn; auto text→SoftExtract;
  auto bitmap→Burn (or MuxSoft if `MuxSoftTry`). nil-safe on `Info`.
- `BurnArgs(input,track,ss) []string` (burn.go) — fragmented-mp4-on-pipe ffmpeg. Bitmap:
  `[0:v][0:s:N]overlay`. Text: `subtitles=…:si=N`. `-c:v libx264 -preset veryfast -crf 22`,
  aac, `-movflags +frag_keyframe+empty_moov+default_base_moof`, `-dn -map_chapters -1`. `ss`=input seek.
- `ExtractText(ctx,input,subIndex,ss,destDir) → srtPath` (extract.go) — `-map 0:s:N -c:s subrip -f srt`.
  SRT, not WebVTT: webOS's `sec:CaptionInfoEx` renders SRT reliably but not VTT over DLNA.
  `ss` input-seeks the extraction so the cues are rebased onto a transcode's 0-based
  timeline (0 for direct-play). Seeking past the last cue returns `NoCuesAfterOffset(err)`,
  which callers treat as "no soft subs for this segment", not as a failure.
- `ShiftText(ctx,srtPath,ss,destDir) → srtPath` (extract.go) — same rebasing for an external
  sidecar; `ss == 0` returns the input untouched.
- `MuxSoftRemux(ctx,input,track,destDir) → mkvPath` (burn.go) — `-c copy` remux of v+a+sub (experimental 6a).

### `internal/transcode` — codec-compat transcode (no subs)
- `Needs(info) (video,audio bool)` — true if codec outside `goodVideo`/`goodAudio` allowlists
  (good video: h264/hevc/mpeg4/mpeg2video/vc1/msmpeg4v3; good audio: aac/ac3/eac3/mp3/mp2/dts/flac).
- `Args(input,ss,tV,tA) []string` — like BurnArgs minus subs; copies stream if not transcoding it.
  Adds `+delay_moov` (mandatory whenever audio is copied) and `-use_editlist 0` (webOS chokes
  on the edit lists delay_moov then writes) — see gotchas.

> The seek brain (former `internal/cast.Session`) now lives in `internal/core` as
> `Cast` — see the core section above. The `internal/cast` package was removed.

### `internal/tui` — bubbletea view (thin)
- `Controller` interface = the playback surface (`*core.Cast` implements it; the
  assertion in tui still references `*renderer.Renderer`, which also satisfies it).
- `SubtitleController` = optional capability interface (`SubtitleChoices/ActiveSubtitle/
  SetSubtitle/SubInfo`) the model type-asserts; only `*core.Cast` implements it, so the
  picker never opens for `*renderer.Renderer` (kept off `Controller` so that assertion holds).
- `Run(ctrl, Options{Title,Device,SubInfo,HasVolume}) → (Outcome, error)`. Elm loop:
  `model.Init/Update/View`. `Outcome` = `OutcomeQuit | OutcomeEnded | OutcomeNext`
  (read off the final model) tells `main` whether to advance to the next episode.
- `Start(title, startFn) → (Outcome, Controller, error)` — TUI-first entry point:
  grabs the terminal immediately and shows a connecting screen (title + one line per
  `emit(...)` call; a `"! "` prefix renders as a warning; braille spinner on a 200ms
  `spinMsg` tick) while `startFn(ctx, emit)` runs in a goroutine. `readyMsg` adopts
  the returned `Controller`+`Options` and flips to the live view (starting the 1s
  tick/poll loop; the spinner loop dies); `startErrMsg` quits with the error. While
  `connecting`, `ctrl` is nil: no polling, no volume fetch, and every key except
  q/ctrl+c is swallowed. Quit-while-connecting cancels startFn's ctx (aborting
  `app.Start`), then Start reaps the goroutine's result over a channel and returns
  its Controller (possibly produced after the quit) so the caller can always close
  it — never a leak, never a shared-var race. An abandoned start's error is
  suppressed (self-inflicted by the cancel); a `startErrMsg` error is returned.
  `Run` is unchanged (direct controller use / tests).
- End-of-media: a `posMsg` with a stopped state (`STOPPED`/`NO_MEDIA_PRESENT`), after
  `everPlayed`, with `maxProgress` within `endGuard` (12s) of `dur`, sets `OutcomeEnded`
  and quits. `maxProgress` (furthest pos seen) is used because the TV may zero its
  reported position on a natural stop. The `seeking` gate keeps a mid-seek stop from
  counting. `n` sets `OutcomeNext` (Stop→Quit, like `q`).
- Polling: `tickCmd` every 1s → `pollCmd` (Position+TransportState). **Skipped while `seeking`
  or `switching`** to avoid SOAP contention with a seek-restart / subtitle switch.
- Subtitle picker: `s` opens an overlay listing the choices (cursor on the active one); ↑/↓
  (k/j) move, pgup/pgdn (ctrl+u/ctrl+d) page, home/end (g/G) jump, enter applies
  (`SetSubtitle`, sets `switching` until `subDoneMsg`), esc/`s` cancel.
  **The list is windowed** — a WEB-DL can carry 65 subtitle tracks, far past the terminal.
  `subVisibleRows()` = `height - 11` (min 3) rows; `subScroll` is the first visible index and
  `scrollFor(cursor, scroll, visible, total)` moves it only when the cursor would leave the
  window. The picker opens *centred* on the active track, the header shows `i/N`, and a footer
  shows `↑ n more` / `↓ n more`. `View` re-clamps via `scrollFor` so a frame drawn before any
  keystroke/resize is still correct; `WindowSizeMsg` re-clamps an open picker.
- Seek debounce: arrow → `armSeek` moves displayed target + bumps `seekGen` + arms 1s `seekFireMsg`;
  only the matching `seekGen` fires the real `ctrl.Seek` (60s budget) → `seekDoneMsg`. Position
  polls don't overwrite the target while `seeking`.
- Keys: space/p play-pause, ←→/hl seek 10s, ↑↓/kj volume ±5, m mute, s subtitle picker,
  n next, q/ctrl+c stop+quit. `model.height` (from `WindowSizeMsg`, default 24) exists only to
  size that picker.

### `internal/config` — persistence
- `Config{LastDeviceHost}` at `os.UserConfigDir()/movcaster/config.json`. `Load`(zero on miss)/`Save`.
  Saved after each cast → bare `movcaster <file>` re-finds the TV across its dynamic-port reboots.

### `internal/resume` — playback position persistence (resume feature)
- `Store` over `~/.movcaster/playback_index` (JSON object keyed by absolute file path:
  `{position_seconds, updated_at}`). `New()` creates the dir + an empty `{}` index on
  construction (so they exist on every run); `Get/Set/Clear` load+rewrite the whole tiny file.
  `Recent()` returns the keys newest-first by `updated_at` (unparseable timestamps sort
  last) — backs `main`'s `--resume`; since finished files are `Clear`ed, `Recent()[0]` is
  the last in-progress video.
- `Rank(pattern, paths) []string` (match.go) — orders `paths` by how well their base names
  fuzzy-match `pattern`, best first, dropping anything below `matchThreshold` (0.6); ties keep
  input order (so newest-first Recent() input → newest match wins). Scoring: normalize both to
  lowercase alphanumeric words (separators/case/extension don't matter), then exact →
  whole-pattern substring → mean of per-word best similarity (substring or edit-distance ratio,
  tolerating a small typo). Backs `--resume-last`. Pure (no I/O), unit-tested.
- Wired by `main` and injected via `core.Options.Resume` (nil in tests → resume disabled).
  `core.Start` reads it (see `resumeOffset`: skips <5s or within 30s of the end) and starts
  a transcode at the saved offset / seeks a direct-play file after Play. `core.Cast` caches
  the last polled position and `Close` persists it (or clears it once finished).

### `internal/nextep` — next-episode detection (auto-advance feature)
- `Find(currentPath) (next string, ok bool, err error)` — within the current file's
  directory, returns the next episode of the *same show*. Parses season/episode from
  filenames via `github.com/middelink/go-parse-torrent-name`; picks the smallest
  `(season, episode)` strictly greater than the current's (handles E+1, gaps, and
  season rollover). Parsing via `go-ptn` (port of parse-torrent-title) handles the
  inexact cases: `SxxEyy`, `1x03`, episode-title suffixes, scene tags, varied
  spacing/case. `ok=false` when the current file has no episode number (e.g. a
  standalone movie) or no same-show successor exists. Title guard: `norm` (lowercase,
  alphanumeric-only) of the parsed titles must match → never jumps to an unrelated
  movie, so auto-advance is safe on by default. Only I/O is `os.ReadDir`.

### `internal/playlist` — playlist file parsing
- `Load(path) ([]string, error)` — reads a plain text playlist (one video path per
  line). Skips blank lines and `#` comments (covers m3u `#EXTM3U`/`#EXTINF`). Resolves
  each entry to an absolute path: absolute paths pass through, relative paths resolve
  against the CWD (`filepath.Abs`), per spec. Errors only on unreadable file or zero
  entries; existence of referenced files is the caller's concern (`main.existingFiles`
  skips missing ones so one bad line doesn't abort the list). No deps.

---

## Key invariants / gotchas (don't regress)

- **TV DLNA control port is dynamic** (seen 1574→1570 across reboots). Always re-discover;
  match targets by host IP, never hardcode port.
- **Media URLs carry ext + `?t=token`** → server uses prefix routing, NOT exact mux patterns.
- **Transcode = `empty_moov` fragmented MP4 → no in-stream duration.** Must advertise
  `res@duration` in DIDL or the TV's seek bar races. (Verified: TV then reports full duration.)
- **A transcoded stream is rebased to 0, so soft cue times must be rebased too.**
  `-ss T` before `-i` makes ffmpeg restart output timestamps at 0, but an extracted
  SRT (or a sidecar) keeps the file's absolute times, so every cue runs T late — with a
  resume or seek deep into a film that reads as "no subtitles at all". Input-seeking the
  *extraction* (`ffmpeg -ss T -i in -map 0:s:N -c:s subrip`) rebases the cues identically
  (verified: the cue at 40:01.500 comes out at 00:01.500 with the same text). Hence
  `buildDelivery` computes `codecPlan` **before** applying subtitles, passes `ss` as the
  sub offset only when the delivery is a transcode (direct-play keeps absolute times), and
  hands back a `softSubFn` that `Seek` calls on every seek-restart. `/subs` carries its own
  `?t=` token, bumped per `SetSubtitle`, so the TV re-fetches instead of reusing the
  previous segment's cues (verified: `GET /subs.srt?t=1` then `?t=2` after a seek).
- **`softSubFn` takes the `*renderer.Media` as a parameter, never a captured one.**
  `Start` hands `buildDelivery` a local `renderer.Media` that is then *copied* into the
  `Cast`; a captured pointer updates the dead local, so the live DIDL keeps advertising the
  previous caption URL.
- **The `subtitles` filter ignores `-ss`** — it opens the file with its own demuxer and reads
  absolute cue times, so after an input seek every cue is in the past and the burn renders
  *nothing* (verified: bottom-strip luma identical with and without the filter at `-ss 61`,
  vs a clear delta unseeked). Fix is `-copyts` + `setpts=PTS-STARTPTS`/`-af asetpts=PTS-STARTPTS`.
  The **bitmap overlay path takes its subtitle from the demuxer, which does honor `-ss`**, so
  it must be left alone.
- **A copied audio stream needs `+delay_moov` in the fragmented MP4.** The mp4 muxer cannot
  write the moov atom for (E-)AC-3 before it has parsed a packet, so with a bare `empty_moov`
  ffmpeg aborts on "Cannot write moov atom before EAC3 packets parsed", writes nothing, and
  the TV shows "This file cannot be recognized". This only bites the codec-compat path
  (`transcode.Args`, which copies audio when it's already TV-friendly) — `BurnArgs` always
  re-encodes to aac and never hit it. Verified per codec: ac3/eac3 fail without `delay_moov`;
  aac/mp3/mp2/flac are fine either way; all pass with it, and 5.1 E-AC-3 survives intact.
  Triggered by AV1 releases (AV1 ∉ `goodVideo`, eac3 ∈ `goodAudio` → transcode video, copy audio).
- **`delay_moov` starts emitting edit lists → suppress with `-use_editlist 0`.** webOS
  mishandles them: after a pause the video freezes ~0.5s into the resume while audio keeps
  playing. They encode nothing here (both streams already start at PTS 0; output
  `start_time`/`start_pts` are identical with and without), so dropping them is free.
- **Transcode streams are NOT byte-seekable** → seeking = kill+relaunch ffmpeg at `-ss`
  (seek-restart). Direct-play keeps native range seeking.
- **TVs serialize UPnP control & are flaky mid-transition** → pause polling during a seek;
  Stop→settle→retry the SetURI/Play sequence (both the initial cast via `startPlayback`
  *and* seek-restart — a TV left mid-transition rejects a new URI with 701 "Transition not
  available" until it leaves the transitioning state); serialize seeks (`seekMu`).
  webOS reports `LG_TRANSITIONING` and lingers there both for ~1-2s *after* Stop returns
  *and* while it buffers a freshly-set URI, so `startPlayback` polls TransportState until
  settled before SetURI *and* again before Play rather than sleeping a fixed interval. The
  pre-Play wait matters for a live transcode resumed deep into the file (`--resume`): ffmpeg
  takes seconds to `-ss`-seek + emit the first fragment, so the TV stays transitioning long
  past the Play retries and Play hits 701 — invisible at offset 0, where the stream settles
  at once. (Verified: a fake/unreachable URL yields 716 "Resource not found", not 701 — so
  701 is purely a transport-state problem, fixed by waiting out the transition.)
- **The pre-Play settle + Play must NOT block `Start`** → it can take 14–45s for a large
  direct-play file (TV lingers in `LG_TRANSITIONING` buffering a high-bitrate stream and is
  slow to ACK Play), during which the TUI hasn't rendered and the terminal is still cooked
  (keys echo as `^[[B`) even though the movie already auto-plays. `Start` returns right after
  SetURI and runs `beginPlayback` in a goroutine; the `Cast.starting` flag gates AVTransport
  polling/control (no SOAP) meanwhile so the TUI renders instantly and shows buffering.
  (Verified against the TV: the Play handshake no longer gates the TUI — the bar renders
  after the synchronous SetURI and shows BUFFERING→PLAYING instead of freezing on Play;
  quitting mid-buffer cancels the handshake and exits in ~140ms.) The remaining
  pre-render latency (discovery + probe + subtitle extraction + SetURI, ~8-11s) is
  covered by TUI-first startup: `tui.Start` owns the terminal from t≈0 and narrates
  those steps on a connecting screen, so the terminal is never cooked mid-startup.
- **webOS does NOT demux embedded subs over DLNA** (sub button greys out) → bitmap subs
  default to burn-in, not mux-soft. `--mux-soft` is the opt-in 6a experiment (needs eyes on TV).
- **webOS DOES honor `sec:CaptionInfoEx` for TEXT subs, but only SRT (not WebVTT) over DLNA**
  → both the sidecar and the embedded-extract soft paths serve `.srt` (`sec:type="srt"`) at
  `/subs`. Extracting to VTT parses but renders nothing on the TV (verified). Serve SRT.
- **Forced subtitle tracks are a trap default** → an English release often ships a `forced`
  track first (default-flagged) that only subtitles foreign-dialogue scenes, so it looks like
  "subs don't work". `selectTrack` skips forced tracks in auto-selection (see `preferNonForced`).
- **serveTranscode sends headers before launching ffmpeg** so SetAVTransportURI doesn't block.
- `ss` selectors use `0:s:<SubIndex>` (subtitle-stream index), not the absolute stream index.
- **macOS idle sleep throttles a cast** → when the laptop display sleeps, the system
  goes idle and suspends our HTTP server + ffmpeg, so the TV stalls ("loading") and
  only resumes on display wake. A live `Cast` holds `caffeinate -i` (idle-sleep
  assertion) for its lifetime; the display is allowed to sleep, only the stall isn't.

## Verification notes

- Unit tests: `subs` (decision tree), `renderer` (DIDL/duration), `tui` (view + seek debounce),
  `core` (device resolution, seek-restart call sequence, position offset math, subtitle apply +
  events, codec plan) using fakes for the three interfaces. `go test ./...`.
- Live behaviors (against the real TV) were verified with throwaway harnesses under a
  temporary `cmd/` dir, then deleted — recreate similarly to re-verify discovery, direct-play
  seek, controls, soft-sub fetch, burn-in, seek-restart. TUI needs a TTY (drive via `script`).
- `--info <file>` is the no-cast way to inspect probe + strategy.
- `MOVCASTER_VERBOSE=1` logs media-server requests + ffmpeg stderr.
