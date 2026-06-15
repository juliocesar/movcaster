# movcaster — codebase skeleton (for LLM consumption)

Terminal-only (CLI + bubbletea TUI) tool that streams a local video file to a DLNA
MediaRenderer (target: LG webOS TV) with soft *and* burned-in subtitles. Single Go
binary; shells out to `ffmpeg`/`ffprobe`. No GUI/web. See `plans/MOVCASTER_PLAN.md`
for rationale and `README.md` for user-facing usage.

Module: `github.com/juliocesar/movcaster` · Go 1.26 · deps: `huin/goupnp` (DLNA SOAP),
`charmbracelet/bubbletea|lipgloss|bubbles` (TUI).

---

## Data flow (one cast)

All orchestration lives in `internal/core`. `main` is a thin client: parse flags →
build a `core.CastRequest` → call core → render `core.Event`s and drive the TUI.

```
main.runCast → core.App.Start(ctx, CastRequest)
  ├─ Doctor()                                verify ffmpeg+ffprobe on PATH
  ├─ selectDevice(target)  ── DeviceFinder ─► SSDP find renderer (or saved/--t)
  │    └─ emit Event "Casting to …"          + Store.Save(LastDeviceHost)
  ├─ Prepare(): probe.Probe + subs.Decide    MediaInfo + subtitle Decision (no TV I/O)
  ├─ newServer(devHost)                      bind HTTP on LAN IP routable to TV
  │    └─ SetDirectPlay(file)                default: serve raw file (range-seek)
  ├─ media.Duration = info.Duration          DIDL res@duration (fixes transcode seek bar)
  ├─ applySubtitles(...)                      apply Decision; may switch server to transcode pipe
  ├─ codecPlan(info) → transcode.Args         codec fallback if not already transcoding
  ├─ renderer.SetMedia(media) + Play()       SOAP SetAVTransportURI(+DIDL) then Play
  └─ returns *core.Cast → tui.Run(cast)      TUI drives the Cast controller until quit
                                             → cast.Close() tears down server+ffmpeg+tmp
```

The TV pulls media from our HTTP server; we push control via SOAP to the TV. Two
independent channels.

Planning (`--info`) reuses `core.Prepare` alone (no device, no TV): probe + decide,
then `Preparation.DescribeStreams()` / `DescribeStrategy()` render the text.

---

## Packages

### `main` (main.go)
Thin CLI client: flag parsing + map to `core.CastRequest` + render events. No
orchestration logic.
- `main()` — flags: `-l -t -sub -no-subs -burn -soft -sub-track -mux-soft -transcode -info`.
  Builds one `core.App` with an `OnEvent` reporter.
- `report(Event)` — `Info`→stdout, `Warn`→stderr with the `movcaster:` prefix. This is
  the one place core's progress lines become terminal output.
- `runList(app)` — `-l`: `app.ListDevices` + print.
- `runInfo(app, req)` — `--info`: `app.Prepare` then print `DescribeStreams`/`DescribeStrategy`.
  ProbeErr is fatal (matches old behavior: aborts before printing); DecideErr prints
  streams then errors.
- `runCast(app, req)` — `app.Start` → `tui.Run(cast, …)` → `cast.Close` on quit.

### `internal/core` — UI-agnostic orchestration (the reusable API)
One import exposes everything a front-end needs. No UI toolkit, no `fmt.Println`;
progress is reported via `Options.OnEvent`, status via the live `Cast`.
- `App` + `New(Options)` — holds injectable deps (`DeviceFinder`, `NewServer`,
  `NewRenderer`, `Store`, `OnEvent`); zero-value Options wires production impls.
- `Doctor()` — ffmpeg/ffprobe on PATH (was `ensureFFmpeg`).
- `ListDevices(ctx)` — discovery passthrough.
- `Prepare(ctx, CastRequest) → *Preparation` — pure planning: probe + `subs.Decide`, no
  TV/network I/O. `Preparation{AbsPath, Info, Sidecar, Strategy, ProbeErr, DecideErr}` +
  `DescribeStreams()`/`DescribeStrategy()`. Reused by `--info` and `Start`.
- `Start(ctx, CastRequest) → (*Cast, *Preparation)` — resolve device (emit "Casting to",
  save config), bind server, `applySubtitles`, codec fallback (`codecPlan`), SetMedia+Play.
  Cleans up the server/tmp dir on any post-bind error.
- `Cast` — the live, concurrency-safe handle (folds the former `internal/cast.Session`).
  Implements the TUI control surface: `Play/Pause/Stop/Seek/Position/TransportState/
  HasVolume/Volume/SetVolume/Mute`, plus `Title/Device/SubInfo`, `Status(ctx)`, `Close(ctx)`.
  Direct-play vs transcode seek-restart logic (Stop→settle→retry SetURI/Play, `seekMu`
  serialized) lives here, moved verbatim from the old Session.
- Interfaces (consumer-side, with compile-time assertions): `DeviceFinder`,
  `RendererControl` (`*renderer.Renderer`), `MediaServer` (`*mediaserver.Server`), `Store`
  (`config`). Tests inject fakes; production defaults wire the real impls.
- `CastRequest`/`SubtitleOptions`/`TranscodePlan`/`Event`/`Status`/`Options` — public data.
- internal helpers: `selectDevice` (no target: quick SSDP pass → saved/sole; if none
  answer, emit "Looking for a TV..." and `waitForDevice` retries up to 10s before erroring.
  target: `selectTarget` by host IP → description-URL load). `pickDevice` (saved → sole → nil),
  `resolveSubtitle`, `subKind`, `mimeForExt`, `hostOf`/`ensureScheme`, `applyTranscode`,
  `applySubtitles`, `codecPlan`, `retrySOAP`, `sleepCtx`.

### `internal/discovery` — SSDP discovery (goupnp)
- `Device{FriendlyName, Location *url.URL, AVTransport *av1.AVTransport1, Rendering *av1.RenderingControl1}`.
  `Rendering` may be nil (no volume control).
- `Discover(ctx) []Device` — `av1.NewAVTransport1ClientsCtx` + match RenderingControl by Location.
- `FindByURL(ctx, loc) *Device` — load directly from a device-description URL (skips SSDP).

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
- `SubTrack{Index, SubIndex (s:N selector), Codec, Kind, Language, Title, Default}`.
- `SubKind`: `SubText` (subrip/ass/mov_text/webvtt…) | `SubBitmap` (dvd_subtitle/pgs/vobsub/dvbsub…) | `SubUnknown`.

### `internal/subs` — strategy + ffmpeg arg/command builders
- `Decide(Request) Decision` — the decision tree (strategy.go). `Kind`:
  `None | SoftSidecar | SoftExtract | MuxSoft | Burn`. Order: NoSubs→None; sidecar→SoftSidecar;
  no tracks→None; `selectTrack` (explicit `TrackIndex`, else prefer TEXT, else default/first);
  ForceSoft (err if bitmap)→SoftExtract; ForceBurn→Burn; auto text→SoftExtract; auto bitmap→
  Burn (or MuxSoft if `MuxSoftTry`). nil-safe on `Info`.
- `BurnArgs(input,track,ss) []string` (burn.go) — fragmented-mp4-on-pipe ffmpeg. Bitmap:
  `[0:v][0:s:N]overlay`. Text: `subtitles=…:si=N`. `-c:v libx264 -preset veryfast -crf 22`,
  aac, `-movflags +frag_keyframe+empty_moov+default_base_moof`, `-dn -map_chapters -1`. `ss`=input seek.
- `ExtractText(ctx,input,subIndex,destDir) → vttPath` (extract.go) — `-map 0:s:N -c:s webvtt`.
- `MuxSoftRemux(ctx,input,track,destDir) → mkvPath` (burn.go) — `-c copy` remux of v+a+sub (experimental 6a).

### `internal/transcode` — codec-compat transcode (no subs)
- `Needs(info) (video,audio bool)` — true if codec outside `goodVideo`/`goodAudio` allowlists
  (good video: h264/hevc/mpeg4/mpeg2video/vc1/msmpeg4v3; good audio: aac/ac3/eac3/mp3/mp2/dts/flac).
- `Args(input,ss,tV,tA) []string` — like BurnArgs minus subs; copies stream if not transcoding it.

> The seek brain (former `internal/cast.Session`) now lives in `internal/core` as
> `Cast` — see the core section above. The `internal/cast` package was removed.

### `internal/tui` — bubbletea view (thin)
- `Controller` interface = the playback surface (`*core.Cast` implements it; the
  assertion in tui still references `*renderer.Renderer`, which also satisfies it).
- `Run(ctrl, Options{Title,Device,SubInfo,HasVolume})`. Elm loop: `model.Init/Update/View`.
- Polling: `tickCmd` every 1s → `pollCmd` (Position+TransportState). **Skipped while `seeking`**
  to avoid SOAP contention with a seek-restart.
- Seek debounce: arrow → `armSeek` moves displayed target + bumps `seekGen` + arms 1s `seekFireMsg`;
  only the matching `seekGen` fires the real `ctrl.Seek` (60s budget) → `seekDoneMsg`. Position
  polls don't overwrite the target while `seeking`.
- Keys: space/p play-pause, ←→/hl seek 10s, ↑↓/kj volume ±5, m mute, q/ctrl+c stop+quit.

### `internal/config` — persistence
- `Config{LastDeviceHost}` at `os.UserConfigDir()/movcaster/config.json`. `Load`(zero on miss)/`Save`.
  Saved after each cast → bare `movcaster <file>` re-finds the TV across its dynamic-port reboots.

### `internal/resume` — playback position persistence (resume feature)
- `Store` over `~/.movcaster/playback_index` (JSON object keyed by absolute file path:
  `{position_seconds, updated_at}`). `New()` creates the dir + an empty `{}` index on
  construction (so they exist on every run); `Get/Set/Clear` load+rewrite the whole tiny file.
- Wired by `main` and injected via `core.Options.Resume` (nil in tests → resume disabled).
  `core.Start` reads it (see `resumeOffset`: skips <5s or within 30s of the end) and starts
  a transcode at the saved offset / seeks a direct-play file after Play. `core.Cast` caches
  the last polled position and `Close` persists it (or clears it once finished).

---

## Key invariants / gotchas (don't regress)

- **TV DLNA control port is dynamic** (seen 1574→1570 across reboots). Always re-discover;
  match targets by host IP, never hardcode port.
- **Media URLs carry ext + `?t=token`** → server uses prefix routing, NOT exact mux patterns.
- **Transcode = `empty_moov` fragmented MP4 → no in-stream duration.** Must advertise
  `res@duration` in DIDL or the TV's seek bar races. (Verified: TV then reports full duration.)
- **Transcode streams are NOT byte-seekable** → seeking = kill+relaunch ffmpeg at `-ss`
  (seek-restart). Direct-play keeps native range seeking.
- **TVs serialize UPnP control & are flaky mid-transition** → pause polling during a seek;
  Stop→settle→retry the SetURI/Play sequence; serialize seeks (`seekMu`).
- **webOS does NOT demux embedded subs over DLNA** (sub button greys out) → bitmap subs
  default to burn-in, not mux-soft. `--mux-soft` is the opt-in 6a experiment (needs eyes on TV).
- **webOS DOES honor `sec:CaptionInfoEx` for TEXT subs** → soft path serves srt/vtt at `/subs`.
- **serveTranscode sends headers before launching ffmpeg** so SetAVTransportURI doesn't block.
- `ss` selectors use `0:s:<SubIndex>` (subtitle-stream index), not the absolute stream index.

## Verification notes

- Unit tests: `subs` (decision tree), `renderer` (DIDL/duration), `tui` (view + seek debounce),
  `core` (device resolution, seek-restart call sequence, position offset math, subtitle apply +
  events, codec plan) using fakes for the three interfaces. `go test ./...`.
- Live behaviors (against the real TV) were verified with throwaway harnesses under a
  temporary `cmd/` dir, then deleted — recreate similarly to re-verify discovery, direct-play
  seek, controls, soft-sub fetch, burn-in, seek-restart. TUI needs a TTY (drive via `script`).
- `--info <file>` is the no-cast way to inspect probe + strategy.
- `MOVCASTER_VERBOSE=1` logs media-server requests + ffmpeg stderr.
