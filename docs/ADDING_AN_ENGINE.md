# Adding an engine

An engine is one TV protocol family (DLNA, Google Cast, ...). The promise this
codebase makes — and enforces with a test — is that adding the next family is a
new package plus one registration line, touching nothing in `core`, `subs`,
`transcode`, `mediaserver`, `tui` or `main`.

## The recipe

1. Create `internal/engine/<name>/`.
2. Implement the three interfaces from `internal/engine`:
   - `Engine` — `Name()`, `Discover(ctx)`, `Resolve(ctx, target)`,
     `DefaultProfile()`. `Resolve` must return `engine.ErrNotThisEngine` for a
     target it cannot address, so the registry can try the next engine.
   - `Device` — a cheap handle (no live connection). `Host()` is the bare IP:
     it binds the media server and keys the config.
   - `Session` — the live control channel. Two contract rules carry real
     weight: **`Load` is the fast half** (return once the device accepts the
     media, never wait for buffering) and **`Play` is the slow half**
     (idempotent, may block while the device buffers). Map your protocol's
     transport vocabulary onto `engine.PlayerState`; nothing outside your
     package may string-match it.
3. Write a conservative `Profile` — only what the device family is *known* to
   play — then refine it per device with `movcaster --probe-device`. Guessing
   from a spec sheet produces silent black screens.
4. Add `engine.Register(<name>.New())` to `internal/engine/all/all.go`.
5. Add a package test calling `enginetest.Run` (with a faked device if you can
   build one in-process; the Cast engine's `fakereceiver_test.go` is the model).
6. Nothing else. If you find yourself editing `core`, the abstraction has a
   gap: fix the abstraction rather than special-casing —
   `enginetest.TestCoreDoesNotImportEngines` will fail the build otherwise.

## Worked example: a Roku ECP engine

Roku is the likely next family, and it stresses the design differently from
Cast:

- **Discover**: SSDP again, but with `ST: roku:ecp` — reuse the pattern in
  `dlna/discover.go`, not the code (engines share nothing but the contract).
- **Resolve**: `GET http://<ip>:8060/query/device-info` answering identifies a
  Roku; anything else is `ErrNotThisEngine`.
- **Load**: a channel deep-link launch of the Roku Media Player:
  `POST /launch/15985?u=<media-url>&t=v&videoFormat=mp4&subtitleUrl=<vtt-url>`.
  Returns as soon as the POST is accepted — the fast half, for free.
- **Play/Pause**: `POST /keypress/Play` (a toggle: read
  `/query/media-player` first to keep `Play` idempotent).
- **Status**: `GET /query/media-player` returns XML with a state attribute
  (`play`, `pause`, `buffer`, `stop`) and position — map onto `PlayerState`.
- **Seek**: ECP has **no absolute seek**. Return `engine.ErrNotSeekable`
  always; core's seek-restart (relaunch the stream at `-ss`) does the rest.
  This case is exactly why `ErrNotSeekable` exists.
- **Profile**: start with `mp4 + h264 + aac` only and let `--probe-device`
  widen it.
- **Volume**: `/keypress/VolumeUp|VolumeDown|VolumeMute` — relative only, so
  `HasVolume()` should return false until someone wants the stepped mapping.
