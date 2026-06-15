# movcaster

A terminal-only (CLI + TUI) tool to stream local video to a DLNA renderer
(built for an LG webOS TV) over wifi, with **soft** *and* **burned-in** subtitle
support — including bitmap subtitle tracks (VobSub/PGS/`dvd_subtitle`) that
go2tv cannot show.

## Requirements

- Go 1.26+ (to build)
- `ffmpeg` and `ffprobe` on `PATH` (`brew install ffmpeg`)

## Build

```
go build -o movcaster .
```

## Usage

```
movcaster -l                       list DLNA renderers on the LAN
movcaster <file>                   cast (auto subtitles + auto codec fallback)
movcaster <file> -t TARGET         target a device (IP, IP:port, or device URL)
movcaster <file> --info            print streams + the chosen subtitle strategy
movcaster <file> --sub foo.srt     force an explicit soft subtitle
movcaster <file> --sub-track N     pick embedded subtitle track N (see --info)
movcaster <file> --burn            burn the selected subtitle into the video
movcaster <file> --soft            force soft subs (errors on bitmap tracks)
movcaster <file> --mux-soft        remux a bitmap track as soft (experimental)
movcaster <file> --no-subs         cast without subtitles
movcaster <file> --transcode       force a codec-compatibility transcode
```

The first device you cast to is remembered (`~/.config/movcaster/config.json`),
so later `movcaster <file>` calls find it again even after the TV's DLNA port
changes on reboot.

### TUI controls

`space` play/pause · `←/→` seek 10s · `↑/↓` volume · `m` mute · `q` quit

## How subtitles are chosen

1. A sidecar `.srt`/`.vtt` (or `--sub`) → **soft** subs via the DLNA caption
   mechanism (`sec:CaptionInfoEx`).
2. An embedded **text** track → extracted to WebVTT → **soft**.
3. A **bitmap** track (`dvd_subtitle`/PGS/VobSub) → **burned in** on the fly with
   ffmpeg `overlay` (playback starts in seconds; no full pre-encode).
   `--mux-soft` instead remuxes it as a soft track to try the TV's own renderer.

Run `movcaster <file> --info` to see the decision without casting.

## Seeking

Direct-play files seek natively via HTTP byte ranges. During a transcode
(burn-in or codec fallback) the stream is not byte-seekable, so seeking restarts
ffmpeg at the new offset (`-ss`) and re-points the TV at the fresh stream.

## Layout

```
main.go                  flag parsing, wiring, lifecycle
internal/discovery       SSDP discovery (goupnp) -> Device + service clients
internal/renderer        AVTransport + RenderingControl + DIDL-Lite building
internal/mediaserver     local HTTP server (direct-play range + transcode pipe)
internal/probe           ffprobe wrapper + subtitle classification
internal/subs            subtitle strategy + extract/burn/remux ffmpeg args
internal/transcode       codec-compatibility transcode args
internal/cast            session controller (direct-play vs seek-restart)
internal/tui             bubbletea view layer
internal/config          remembers the last device
```
