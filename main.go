// movcaster streams local video to a DLNA renderer (e.g. an LG webOS TV) with
// soft and burned-in subtitle support. CLI/TUI only.
//
// This file is a thin client over internal/core: it parses flags, maps them to a
// core.CastRequest, renders core events, and drives the TUI. All orchestration
// lives in core.
package main

import (
	"context"
	"flag"
	"fmt"
	"os"
	"time"

	"github.com/juliocesar/movcaster/internal/core"
	"github.com/juliocesar/movcaster/internal/tui"
)

// version is set at build time via -ldflags "-X main.version=...". It stays
// "dev" for plain `go build`/`go install` of an untagged checkout.
var version = "dev"

func main() {
	var (
		list     = flag.Bool("l", false, "list DLNA renderers on the LAN and exit")
		showVer  = flag.Bool("version", false, "print version and exit")
		target   = flag.String("t", "", "target device: IP, IP:port, or device description URL")
		sub      = flag.String("sub", "", "subtitle file to use as soft subs (.srt/.vtt)")
		noSubs   = flag.Bool("no-subs", false, "do not send any subtitles")
		burn     = flag.Bool("burn", false, "burn the selected subtitle track into the video")
		soft     = flag.Bool("soft", false, "force soft subs (errors if the track is bitmap)")
		subTrack = flag.Int("sub-track", -1, "embedded subtitle track index to use (0-based)")
		muxSoft  = flag.Bool("mux-soft", false, "remux a bitmap track as soft instead of burning (experimental; toggle on TV)")
		tcode    = flag.Bool("transcode", false, "force a codec-compatibility transcode")
		info     = flag.Bool("info", false, "probe the file, print streams and the subtitle decision, then exit")
	)
	flag.Usage = usage
	flag.Parse()

	if *showVer {
		fmt.Println("movcaster", version)
		return
	}

	app := core.New(core.Options{OnEvent: report})

	if *list {
		fail(runList(app))
		return
	}

	args := flag.Args()
	if len(args) != 1 {
		usage()
		os.Exit(2)
	}
	req := core.CastRequest{
		File:           args[0],
		Target:         *target,
		ForceTranscode: *tcode,
		Subtitle: core.SubtitleOptions{
			Sidecar:    *sub,
			NoSubs:     *noSubs,
			Burn:       *burn,
			Soft:       *soft,
			MuxSoft:    *muxSoft,
			TrackIndex: *subTrack,
		},
	}
	if *info {
		fail(runInfo(app, req))
		return
	}
	fail(runCast(app, req))
}

// report renders a core.Event to the terminal: Info to stdout, Warn to stderr
// with the standard "movcaster:" prefix.
func report(e core.Event) {
	switch e.Level {
	case core.Warn:
		fmt.Fprintln(os.Stderr, "movcaster:", e.Message)
	default:
		fmt.Println(e.Message)
	}
}

func usage() {
	fmt.Fprint(os.Stderr, `movcaster - stream local video to a DLNA renderer (e.g. an LG webOS TV)

Usage:
  movcaster -l                       list renderers
  movcaster <file>                   cast (auto subs, auto codec fallback)
  movcaster <file> -t TARGET         target a device (IP, IP:port, or device URL)
  movcaster <file> --info            show streams + chosen subtitle strategy
  movcaster <file> --sub foo.srt     force an explicit soft subtitle
  movcaster <file> --sub-track N     pick embedded subtitle track N (see --info)
  movcaster <file> --burn            burn the selected subtitle into the video
  movcaster <file> --soft            force soft subs (errors on bitmap tracks)
  movcaster <file> --mux-soft        remux a bitmap track as soft (experimental)
  movcaster <file> --no-subs         cast without subtitles
  movcaster <file> --transcode       force a codec-compatibility transcode

Subtitles: a sidecar .srt/.vtt is auto-detected and sent as soft subs; embedded
text tracks are extracted to soft; bitmap tracks (PGS/VobSub/dvd_subtitle) are
burned in on the fly. The chosen device is remembered for next time.

Controls (TUI): space play/pause, left/right seek, up/down volume, m mute, q quit.
`)
}

func fail(err error) {
	if err != nil {
		fmt.Fprintln(os.Stderr, "movcaster:", err)
		os.Exit(1)
	}
}

func runList(app *core.App) error {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	devices, err := app.ListDevices(ctx)
	if err != nil {
		return err
	}
	if len(devices) == 0 {
		fmt.Println("No DLNA renderers found.")
		return nil
	}
	fmt.Printf("Found %d renderer(s):\n", len(devices))
	for i, d := range devices {
		host := ""
		if d.Location != nil {
			host = d.Location.Host
		}
		extra := ""
		if d.Rendering == nil {
			extra = "  (no volume control)"
		}
		fmt.Printf("  %d. %s  [%s]%s\n", i+1, d.FriendlyName, host, extra)
	}
	return nil
}

// runInfo probes the file and prints its streams and the subtitle decision.
func runInfo(app *core.App, req core.CastRequest) error {
	ctx, cancel := context.WithTimeout(context.Background(), 25*time.Second)
	defer cancel()
	prep, err := app.Prepare(ctx, req)
	if err != nil {
		return err
	}
	if prep.ProbeErr != nil {
		return prep.ProbeErr
	}
	fmt.Print(prep.DescribeStreams())
	if prep.DecideErr != nil {
		return prep.DecideErr
	}
	fmt.Print(prep.DescribeStrategy())
	return nil
}

func runCast(app *core.App, req core.CastRequest) error {
	cast, _, err := app.Start(context.Background(), req)
	if err != nil {
		return err
	}
	defer cast.Close(context.Background())

	return tui.Run(cast, tui.Options{
		Title:     cast.Title(),
		Device:    cast.Device(),
		SubInfo:   cast.SubInfo(),
		HasVolume: cast.HasVolume(),
	})
}
