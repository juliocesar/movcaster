# movcaster — codebase skeleton (for LLM consumption)

Terminal-only (CLI + bubbletea TUI) tool that streams a local video file to a TV
with soft *and* burned-in subtitles. Single Go binary; shells out to
`ffmpeg`/`ffprobe`. No GUI/web. TVs are driven through **engines** — protocol
families behind one contract: `dlna` (UPnP AVTransport, grown against an LG
webOS TV) and `googlecast` (CASTV2, for Chromecast-built-in / Google TV panels).
See `docs/ADDING_AN_ENGINE.md` for the recipe and `README.md` for usage.

Module: `github.com/juliocesar/movcaster` · Go 1.26 · deps: `huin/goupnp` (DLNA
SOAP), `grandcat/zeroconf` (Cast mDNS), `charmbracelet/bubbletea|lipgloss|bubbles`
(TUI), `razsteinmetz/go-ptn` (filename episode parsing for auto-advance).

---

## Data flow (one cast)

All orchestration lives in `internal/core`. `main` is a thin client: parse flags →
build a `core.CastRequest` → call core → render `core.Event`s and drive the TUI.
`main` blank-imports `internal/engine/all`, which registers every engine; `core`
talks only to the `internal/engine` contract and never imports a concrete engine
(enforced by `enginetest.TestCoreDoesNotImportEngines`).

```
main.runCast → tui.Start(title, startFn)    TUI grabs the terminal FIRST (connecting screen);
  │                                         startFn runs in a goroutine, emits → statusMsg lines
  └─ startFn: SetEventSink(→TUI) + core.App.Start(ctx, CastRequest)
       ├─ Doctor(ctx)                             ffmpeg+ffprobe on PATH *and* launch `-version`
       ├─ selectDevice(target, engine) ─ DeviceFinder ─► engine.DiscoverAll / ResolveTarget
       │    └─ emit Event "Casting to …"          + Store.Save(host + engine)
       ├─ prof = DeviceProfile(dev)               engine default merged with config override
       ├─ PrepareWithProfile(): probe + subs.Decide  MediaInfo + Decision against prof (no TV I/O)
       ├─ newServer(dev.Host())                   bind HTTP on LAN IP routable to TV
       ├─ sess = dev.Open(ctx)                    live engine.Session (Cast: TLS+handshake; DLNA: no I/O)
       ├─ buildDelivery(...)                       baseline direct-play → applySubtitles + codec/remux
       ├─ media.StartAt = resume offset           only when prof.HonorsStartAt and direct-play
       ├─ sess.Load(media) sync → return *Cast    the fast half; TUI flips to live view
       └─ background: sess.Play (+ post-Play Seek when !HonorsStartAt) — the slow half
```

The TV pulls media from our HTTP server; we push control through the engine
session. Two independent channels.

Planning (`--info`) reuses `core.Prepare` (default profile = first registered
engine's) or, with `-t`, `PrepareWithProfile` against the resolved device.

---

## Packages

### `internal/engine` — the contract + registry (zero protocol code)
- `Engine` — `Name`, `Discover(ctx)`, `Resolve(ctx, target)` (returns
  `ErrNotThisEngine` for foreign targets), `DefaultProfile()`.
- `Device` — cheap handle: `Name/Host/Addr/Engine/Profile/Open`.
- `Session` — the live control channel. **`Load` is the fast half** (returns
  once the device ACCEPTS the media, never waits for buffering); **`Play` is the
  slow half** (idempotent, may block while buffering). Plus
  `Pause/Stop/Seek/Status/HasVolume/Volume/SetVolume/Mute/Close`. `Seek` on
  non-seekable media returns `ErrNotSeekable` → callers seek-restart.
- `PlayerState` — the normalized transport vocabulary
  (`Unknown/Idle/Buffering/Playing/Paused/Stopped/Finished`). Nothing outside an
  engine string-matches a protocol state. `State{Player, Pos, Dur, Native}` —
  `Native` is for MOVCASTER_VERBOSE only.
- `Media{URL,Title,MIME,Duration,Seekable,StartAt,Subtitle}` — neutral;
  `StartAt` is honored at Load by engines with `Profile.HonorsStartAt` (Cast),
  ignored by DLNA (caller seeks after Play). `Subtitle{URL,Format,MIME,...}`,
  `SubFormat` = SRT | VTT (`.Ext()`, `.MIME()` — SRT keeps the verified
  `text/srt`, not the IANA type).
- `Profile` — what a device actually plays: `Containers/VideoCodecs/AudioCodecs`
  sets (nil = anything), `MaxVideoHeight/MaxAudioChannels`, `SubFormats` in
  preference order (empty = no soft path, burn everything),
  `HonorsStartAt`, `LoadStartsPlayback`, transcode targets.
- `registry.go` — `Register` (from `engine/all` inits), `Engines`, `Lookup`,
  `DiscoverAll` (concurrent, one engine's failure never blocks another),
  `ResolveTarget` (bare IP → try engines in order; `engine:host` prefix or
  `only` restricts), `SplitTarget`.

### `internal/engine/dlna` — the DLNA/webOS engine
- `discover.go` — SSDP. `searchTargets` merging (AVTransport URN + MediaRenderer
  URN + ssdp:all; a just-woke webOS TV answers one and misses another) and the
  multicast-only `searchHost`/`findByHost` (webOS ignores unicast M-SEARCH; the
  probe learns the dynamic control port). **Do not "simplify" either.**
- `session.go` — AVTransport + RenderingControl over small `avTransport`/
  `renderingControl` interfaces (testable against `fakeAV` without SOAP).
  `Load` = Stop → settle → retried SetAVTransportURI (returns on acceptance;
  webOS then auto-plays). `Play` = settle (20s budget) → retried Play. `Seek`
  retries REL_TIME; `Status` = GetTransportInfo + GetPositionInfo.
  `mapState`: `*TRANSITIONING*`→Buffering, `STOPPED`→Stopped,
  `NO_MEDIA_PRESENT`→Idle; DLNA has no finished signal, so never `Finished`.
- `didl.go` — DIDL-Lite builder: `DLNA.ORG_OP`/FLAGS (empirical — keep),
  `res@duration`, `sec:CaptionInfo(Ex)` with `sec:type` from `Subtitle.Format`.
- `quirks.go` — `waitTransportSettled` (poll, never a fixed sleep) + `retrySOAP`.
  The 701 "Transition not available" story lives here now, not in core.
- `profile.go` — `webOSProfile`: no container limit, the observed codec sets,
  SRT-only soft subs, `HonorsStartAt:false`.
- `Resolve`: URL with port → load description directly; bare host → the
  multicast-filtered probe; no SSDP answer → `ErrNotThisEngine`.

### `internal/engine/googlecast` — the Google Cast engine
- `castmsg.go` — the CASTV2 envelope: one flat protobuf message, hand-rolled
  codec (7 scalar fields; generated code + google.golang.org/protobuf would be
  the largest dependency in the tree). 4-byte big-endian length framing.
- `conn.go` — TLS to port 8009 with `InsecureSkipVerify` (the receiver presents
  a self-signed device cert; there is no identity to verify — do not "fix").
  Read loop dispatches by namespace; requestId waiters (removed on timeout);
  write mutex; **heartbeat**: PING every 5s + prompt PONG to inbound PINGs — the
  receiver drops a quiet sender in ~10s and it looks like the TV freezing.
- `channel.go` — receiver namespace: `CONNECT receiver-0` → `GET_STATUS` →
  reuse a running CC1AD845 app or `LAUNCH` → **`CONNECT` to the
  `transportId`** (skipping this makes every media message silently vanish —
  the classic mistake) → media messages go to the transportId. Volume =
  receiver `SET_VOLUME` `{level: 0.0-1.0}` / `{muted}`.
- `session.go` — `Load` sends LOAD with `autoplay:true`,
  `currentTime = StartAt` (collapses the resume dance), `streamType` BUFFERED
  when `Seekable` else **LIVE** (a pipe honestly refuses native SEEK; supplying
  `duration` still lets the TUI draw its bar), WebVTT `tracks` +
  `activeTrackIds:[1]` (omit both entirely with no subtitle — an empty array is
  not absence). LOAD_FAILED surfaces `detailedErrorCode` (100 unknown / 101
  network-or-CORS / 104 unsupported / 110 decode / 905 generic). `Play` is
  idempotent (PLAY only when PAUSED). `Status` serves the last pushed
  MEDIA_STATUS, extrapolating position by wall time while PLAYING, refreshing
  with GET_STATUS past 5s. `IDLE + idleReason FINISHED` → `StateFinished`
  (clean auto-advance). `Close` CLOSEs the virtual connections but **never**
  sends receiver STOP (that blacks the TV to its home screen).
- `discover.go` — zeroconf browse of `_googlecast._tcp` (TXT: `fn` name, `id`
  UUID, `md` model). macOS risk: UDP 5353 is shared with mDNSResponder; if
  browsing is unreliable, `-t <ip>` works with no mDNS (Resolve dials directly),
  or shell out to `dns-sd` as a last resort.
- `profile.go` — `defaultReceiverProfile`, deliberately conservative
  (mp4/webm only, h264/vp8/vp9, no AC-3/DTS, VTT subs, `HonorsStartAt:true`).
  Widen per device via `--probe-device`, never from a spec sheet.
- `fakereceiver_test.go` — an in-process TLS receiver speaking the framing;
  the whole engine is unit-tested against it (handshake order, heartbeat,
  exact LOAD shapes, state mapping, extrapolation, volume, LOAD_FAILED).

### `internal/engine/all` — registration
One `init()` with one `engine.Register` line per engine. `main` imports it
blank. Adding a TV family = new package + one line here.

### `internal/engine/enginetest` — conformance + architecture
- `Run(t, e, dev)` — the shared contract suite (cancelled-ctx discovery,
  `ErrNotThisEngine`, fast Load, idempotent Play, `ErrNotSeekable`, no
  `StateUnknown` when loaded, volume round-trip, double Close). Every engine's
  package test calls it (googlecast with its fake receiver).
- `TestCoreDoesNotImportEngines` — `go list -deps` over core/tui/subs/
  transcode/mediaserver/probe must never reach a concrete engine.

### `main` (main.go)
Thin CLI client. Flags: `-l -t --engine -sub -no-subs -burn -soft -sub-track
-mux-soft -transcode -info -probe-device -no-next -playlist -resume
-resume-last`. `-t` accepts IP, IP:port, `engine:host`, or a device URL;
`--engine` restricts discovery. `-l` prints `name [addr] (engine)`.
`--info [-t]` plans against the default or the resolved device's profile and
prints a `-> profile:` line. `--probe-device [-t]` runs `core.ProbeDevice` and
prints the JSON report + override. Everything else as before (resume matching,
playlists, next-episode, event reporting).

### `internal/core` — UI-agnostic orchestration (the reusable API)
- `App` + `New(Options)` — injectable deps: `DeviceFinder` (Discover/Resolve
  over `engine.Device`), `NewServer`, `Store`, `Resume`, `OnEvent`. Production
  defaults wire the engine registry + real impls.
- `Doctor(ctx)` — ffmpeg/ffprobe present *and* runnable.
- `selectDevice(target, only)` — quick discovery pass → saved (host + engine)
  or sole device; else "Looking for a TV..." + `waitForDevice` (retries + a
  direct Resolve of the saved host, which survives dropped multicast).
  `selectTarget`: match discovered by host IP first, else `finder.Resolve`.
- `DeviceProfile(dev)` (deviceprofile.go) — engine default merged with the
  config `device_profiles["engine:host"]` override.
- `Prepare` / `PrepareWithProfile` — pure planning: probe + `subs.Decide` +
  `codecPlan` against a profile. `Preparation{..., Profile, ProfileName}` +
  `Describe{Streams,Profile,Strategy,Codec}`.
- `Start(ctx, req)` — resolve device, save config, open the session, bind the
  server, `buildDelivery`, set `media.StartAt` (resume on a `HonorsStartAt`
  engine), `sess.Load` **synchronously** (fails Start cleanly on rejection),
  return the `Cast`; the slow tail (`sess.Play`, then the post-Play resume seek
  only when `!HonorsStartAt`) runs in a background goroutine with its own 45s
  ctx. While it runs, `starting` (atomic) short-circuits the read/control
  methods so the TUI renders instantly and shows buffering.
- `Cast` — the live handle. `Position`/`PlayerState` read one cached
  `sess.Status` per poll beat (500ms cache — on DLNA a Status is two SOAP
  calls, so per-second SOAP volume is unchanged vs the pre-engine code);
  control actions invalidate the cache. Transcode mode adds `ssOffset` to the
  reported position and substitutes the probed duration. Seek: direct-play →
  `sess.Seek` (engine owns retries; `ErrNotSeekable` falls through), transcode →
  seek-restart = `SetTranscode(build(pos))` + `resoft` + `sess.Load` +
  `sess.Play` (the engine owns the settle/retry). `seekMu` serializes restarts
  and subtitle switches.
- Live subtitle switching: `SubtitleChoices/ActiveSubtitle/SetSubtitle` —
  rebuild delivery at the current position, `Load`+`Play`, then `StartAt` or
  post-Play `Seek` per the profile.
- `buildDelivery(srv, media, abs, tmpDir, info, subOpts, force, ss, prof)` —
  baseline direct-play → `subs.Decide(prof)` + `codecPlan(prof)` (computed
  before subtitles: any transcode/remux rebases the stream, so `subOffset = ss`
  when `Kind != TranscodeNone`) → `applySubtitles` → codec/remux fallback via
  `transcode.Args(abs, ss, prof, plan)`. Labels: "transcode" / "remux" /
  "soft: …" / "burn-in: …".
- `TranscodeKind` = None | Burn | Codec | **Remux** (container-only; both
  streams copied — a remux costs no encoding). The remux streams through the
  same fragmented-MP4 pipe and seek-restarts. Considered alternative (rejected):
  remux to a temp file with `+faststart` for real byte ranges — costs a full
  upfront wait and disk equal to the source; the copy-only relaunch is fast.
- `ProbeDevice(ctx, target, only)` (probedevice.go) — generates 4s lavfi clips
  over a curated (container, video, audio) matrix + 4K + 5.1 limit probes,
  casts each, watches for `StatePlaying`/`StateFinished` (~8s), derives a
  `config.ProfileOverride` (axis supported when ANY tuple carrying it played;
  a skipped clip — missing local encoder — says nothing). Honest about limits:
  a test tone is not a real track.
- `inhibitSleep()` (power_darwin.go) — `caffeinate -i` for the cast's lifetime.
- `resumeOffset`, `hostOf`, `mimeForExt`, `clock`, `sleepCtx` helpers.

### `internal/mediaserver` — local HTTP server
Serves `/media*` and `/subs*` (prefix routing — URLs carry ext + `?t=token`).
- `setCommonHeaders` — **CORS on both /media and /subs** (`Allow-Headers:
  Content-Type, Accept-Encoding, Range`) + Content-Type: a Cast LOAD carrying
  subtitle tracks requires CORS on the media *and* the track or it fails
  opaquely (code 101). `/media` and `/subs` answer OPTIONS preflight.
- `setDLNAHeaders` — transferMode/realTimeInfo/contentFeatures; sent
  unconditionally (inert on Cast) rather than threading engine identity in.
- `SetDirectPlay` / `SetTranscode` / `SetSubtitle` — swap `Source` + bump the
  cache-buster tokens. `MediaURL()` (ext `.mp4` for a transcode), `SubURL()`
  (extension from the produced file: `.srt` or `.vtt`).
- `serveTranscode` — headers + 200 FIRST (so control calls don't block), then
  ffmpeg on `r.Context()`; HTTP back-pressure throttles ffmpeg.

### `internal/probe` — ffprobe wrapper + sub classification
- `MediaInfo{Duration, Container, VideoCodec, AudioCodec, Height,
  AudioChannels, Subtitles[]}` — `Container` normalized from
  `format.format_name` (`"mov,mp4,..."`→`mp4`; `"matroska,webm"` split by
  extension so true .webm files stay `webm`). `Height`/`AudioChannels` feed the
  profile limits without a second probe pass.
- `SubTrack{Index, SubIndex, Codec, Kind, Language, Title, Default, Forced}`;
  `SubKind` text | bitmap | unknown.

### `internal/subs` — strategy + ffmpeg builders
- `Decide(Request{Info, Profile, ...}) Decision{Kind, Format, ...}` — the tree:
  NoSubs→None; sidecar→SoftSidecar in `Profile.SubFormats[0]` (None if the
  device has no soft path); no tracks→None; ForceSoft + bitmap→error;
  ForceSoft + no soft path→error; ForceBurn→Burn; text→SoftExtract in the
  profile's format (Burn when no soft path); bitmap→Burn (MuxSoft behind
  `--mux-soft`). `selectTrack` prefers text, skips Forced when a non-forced
  peer exists.
- `ExtractText(..., format)` / `ShiftText(..., format)` — emit SRT
  (`-c:s subrip -f srt`) or WebVTT (`-c:s webvtt -f webvtt`); ffmpeg does the
  conversion (timestamp separators, WEBVTT header), never hand-rolled.
  `noCues`: an empty SRT is zero bytes but a cue-less VTT still has its header,
  so VTT is sniffed for `-->`. `NoCuesAfterOffset` = "no soft subs for this
  segment", not a failure. ShiftText also converts an ss=0 sidecar whose
  extension doesn't match the profile's format.
- `BurnArgs` — unchanged (bitmap overlay / subtitles filter + `-copyts` dance).

### `internal/transcode` — codec/container compatibility
- `Needs(mi, profile) Plan{Video, Audio, Remux}` — per-axis check against the
  profile's sets + height/channel limits; `Remux` = container mismatch only.
- `Args(input, ss, profile, plan)` — fragmented MP4 on pipe with the profile's
  transcode targets; a remux-only plan copies both streams. Keeps
  `+delay_moov` + `-use_editlist 0` (load-bearing; see gotchas) on every path.

### `internal/tui` — bubbletea view (thin)
- `Controller` — `Play/Pause/Stop/Seek/Position/PlayerState/HasVolume/Volume/
  SetVolume/Mute`. Deals only in `engine.PlayerState`; zero protocol strings.
- End-of-media: `StateFinished` ends immediately (Cast's clean signal); the
  `everPlayed` + `maxProgress`-within-`endGuard` inference is kept for
  `Stopped`/`Idle` (DLNA has to guess).
- Everything else (connecting screen, seek debounce, windowed subtitle picker,
  polling gates while seeking/switching) as before.

### `internal/config` — persistence
- `Config{LastDeviceHost, LastDeviceEngine, DeviceProfiles}` at
  `os.UserConfigDir()/movcaster/config.json`. `LastDeviceEngine` empty (old
  config) = try every engine against the host — never fail on an old config.
  `DeviceProfiles` maps `"engine:host"` → `ProfileOverride{containers,
  video_codecs, audio_codecs, max_video_height, max_audio_channels}` (non-empty
  axes replace the engine default).

### `internal/resume`, `internal/nextep`, `internal/playlist`
Unchanged: playback-position persistence + fuzzy `--resume-last` ranking;
same-show next-episode detection; plain-text playlist parsing.

---

## Key invariants / gotchas (don't regress)

Engine-neutral:
- **Media URLs carry ext + `?t=token`** → server uses prefix routing, NOT exact
  mux patterns.
- **Transcode = `empty_moov` fragmented MP4 → no in-stream duration.** Advertise
  the probed duration (DIDL `res@duration` on DLNA, LOAD `duration` on Cast) or
  the TV's seek bar races.
- **Transcode/remux streams are NOT byte-seekable** → seeking = kill+relaunch
  ffmpeg at `-ss` (seek-restart). Direct-play keeps native range seeking.
- **A transcoded stream is rebased to 0, so soft cue times must be rebased too**
  (`buildDelivery` computes the codec plan before subtitles; `resoft` re-cuts on
  every seek-restart; `/subs` gets a fresh `?t=` so the TV re-fetches).
- **`softSubFn` takes the `*engine.Media` as a parameter, never a captured one**
  (Start's local media is copied into the Cast; a captured pointer updates a
  dead value).
- **The `subtitles` filter ignores `-ss`** → `-copyts` + `setpts` dance in
  BurnArgs; the bitmap overlay path honors `-ss` and must be left alone.
- **A copied audio stream needs `+delay_moov`**, and **`delay_moov` needs
  `-use_editlist 0`** (webOS freezes on the edit lists) — both kept on every
  Args path, harmless to Cast.
- **`Load` must not block on buffering and `Play` must be idempotent** — the
  contract encodes the TUI's startup responsiveness; every engine inherits it.
- **macOS idle sleep throttles a cast** → a live `Cast` holds `caffeinate -i`.
- **serveTranscode sends headers before launching ffmpeg** so control calls
  don't block.

DLNA-engine (webOS) specific — all live in `internal/engine/dlna`:
- **The TV's DLNA control port is dynamic** → always re-discover; match by host
  IP; `searchTargets` merging and the multicast-only host probe exist because a
  just-woke webOS TV drops M-SEARCHes.
- **webOS rejects SetURI and Play with 701 while `LG_TRANSITIONING`** → poll the
  transport state (never a fixed sleep) before both, and again between them;
  the pre-Play settle matters for deep-offset transcode resumes.
- **webOS does NOT demux embedded subs over DLNA** → bitmap subs burn in;
  `--mux-soft` stays the experiment.
- **webOS honors `sec:CaptionInfoEx` for TEXT subs, but only SRT** (VTT parses
  but renders nothing) → the webOS profile lists SRT only.
- **Forced subtitle tracks are a trap default** → `selectTrack` skips them.

Cast-engine specific — all live in `internal/engine/googlecast`:
- **The second CONNECT (to the app transportId) is mandatory** — without it
  every media message silently vanishes.
- **The heartbeat is not optional** — a sender that misses PING/PONG for ~10s
  is dropped, which looks like the TV freezing.
- **CORS on `/media` too, not just `/subs`** — a LOAD carrying tracks fails
  opaquely (101) without it.
- **No Matroska, no AC-3/E-AC-3/DTS in the default profile** — a typical
  release remuxes (cheap, copy-only) and often re-encodes audio; widen only
  from `--probe-device` evidence.
- **`streamType` LIVE for any pipe, BUFFERED for direct-play** — LIVE makes the
  receiver hide its seek bar and refuse SEEK, which is honest (we seek-restart).
- **Never send receiver STOP on Close** — it tears the app down to a black home
  screen; stop the media session only.
- **Omit `activeTrackIds` entirely when there is no subtitle** — an empty array
  is not absence on some receivers.

## Verification notes

- Unit tests: `engine` (registry), `dlna` (DIDL, state mapping, quirks against
  `fakeAV`), `googlecast` (the full engine against the in-process fake
  receiver), `enginetest` (conformance + architecture), `subs`, `transcode`
  (profile matrix), `mediaserver` (CORS/range over real HTTP), `core`
  (resolution, seek-restart, offsets, overrides, prober derivation), `tui`.
  `go test ./...`.
- Live behaviors are verified with throwaway harnesses under a temporary
  `cmd/` dir (deleted afterwards); the TUI needs a TTY (drive via `script`).
- `--info [-t target]` is the no-cast way to inspect probe + strategy + profile.
- `--probe-device [-t target]` measures a device's real capabilities.
- `MOVCASTER_VERBOSE=1` logs media-server requests, ffmpeg stderr, and the full
  Cast message traffic.
