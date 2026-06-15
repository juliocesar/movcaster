# movcaster — codebase skeleton (for LLM consumption)

Terminal-only (CLI + bubbletea TUI) tool that streams a local video file to a DLNA
MediaRenderer (target: LG webOS TV) with soft *and* burned-in subtitles. Single Go
binary; shells out to `ffmpeg`/`ffprobe`. No GUI/web. See `plans/MOVCASTER_PLAN.md`
for rationale and `README.md` for user-facing usage.

Module: `github.com/juliocesar/movcaster` · Go 1.26 · deps: `huin/goupnp` (DLNA SOAP),
`charmbracelet/bubbletea|lipgloss|bubbles` (TUI).

---

## Data flow (one cast)

```
main.runCast
  ├─ ensureFFmpeg()                          verify ffmpeg+ffprobe on PATH
  ├─ selectDevice(target)  ── discovery ──►  SSDP find renderer (or saved/--t)
  ├─ mediaserver.New(devHost)                bind HTTP on LAN IP routable to TV
  │    └─ SetDirectPlay(file)                default: serve raw file (range-seek)
  ├─ probe.Probe(file)                       ffprobe → MediaInfo (codecs, sub tracks, duration)
  ├─ media.Duration = info.Duration          DIDL res@duration (fixes transcode seek bar)
  ├─ setupSubtitles(...)  ── subs.Decide ──► strategy; may switch server to transcode pipe
  ├─ transcode.Needs(info)                   codec fallback if not already transcoding
  ├─ renderer.SetMedia(media) + Play()       SOAP SetAVTransportURI(+DIDL) then Play
  └─ cast.New(...) → tui.Run(session)        TUI drives the Session controller until quit
```

The TV pulls media from our HTTP server; we push control via SOAP to the TV. Two
independent channels.

---

## Packages

### `main` (main.go)
Flag parsing, wiring, lifecycle. Subcommands by flag.
- `main()` — flags: `-l -t -sub -no-subs -burn -soft -sub-track -mux-soft -transcode -info`.
- `runList()` — `-l`: print discovered renderers.
- `runInfo(path, opts)` — `--info`: print streams + chosen subtitle strategy, no cast.
- `runCast(path, opts)` — the main path (see flow above).
- `setupSubtitles(srv, &media, abs, tmpDir, info, opts) → subResult` — runs `subs.Decide`
  and APPLIES it: soft→`srv.SetSubtitle`+set `media.Sub*`; bitmap burn→`applyTranscode`
  (returns `buildTranscode` closure); mux-soft→remux+`SetDirectPlay`. `subResult{label, buildTranscode}`.
- `applyTranscode(srv,&media,build)` — switch server+media to transcode-from-0.
- `selectDevice(ctx,target)` — resolve target: explicit `-t` (match by host IP, then
  `FindByURL`) → saved config host → sole device → error.
- helpers: `resolveSubtitle` (sidecar `.srt/.vtt` or `--sub`), `subKind` (mime/sec:type),
  `mimeForExt`, `hostOf`/`ensureScheme`, `ensureFFmpeg`.
- `castOpts{target,sub,noSubs,burn,soft,muxSoft,transcode,subTrack}`.

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

### `internal/cast` — Session controller (the seek brain)
- `Session` implements `tui.Controller`. Wraps renderer+server. `transcoding()` = `buildArgs != nil`.
- Play/Pause/Stop/volume/state delegate to renderer.
- `Position` — transcode: `ssOffset + TVpos` (TV pos is segment-relative), dur=`knownDuration`.
  Direct-play: passthrough.
- `Seek(ctx,pos)` — direct-play: native `r.Seek`. Transcode = **seek-restart**, guarded by
  `seekMu` (serialized): `Stop`(4s) → settle 500ms → `SetTranscode(buildArgs(pos))` → new URL →
  `retrySOAP(SetMedia)` → `retrySOAP(Play)` → set `ssOffset=pos`.
- `retrySOAP(parent,attempts,perCall,fn)` — per-call timeout + backoff; aborts on parent ctx.
- `sleepCtx` — ctx-cancellable sleep.

### `internal/tui` — bubbletea view (thin)
- `Controller` interface = the playback surface (cast.Session implements it).
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

- Unit tests: `subs` (decision tree), `renderer` (DIDL/duration), `tui` (view + seek debounce).
  `go test ./...`.
- Live behaviors (against the real TV) were verified with throwaway harnesses under a
  temporary `cmd/` dir, then deleted — recreate similarly to re-verify discovery, direct-play
  seek, controls, soft-sub fetch, burn-in, seek-restart. TUI needs a TTY (drive via `script`).
- `--info <file>` is the no-cast way to inspect probe + strategy.
- `MOVCASTER_VERBOSE=1` logs media-server requests + ffmpeg stderr.
