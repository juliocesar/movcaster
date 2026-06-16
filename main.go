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
	"path/filepath"
	"time"

	"github.com/juliocesar/movcaster/internal/core"
	"github.com/juliocesar/movcaster/internal/nextep"
	"github.com/juliocesar/movcaster/internal/playlist"
	"github.com/juliocesar/movcaster/internal/resume"
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
		noNext   = flag.Bool("no-next", false, "do not auto-play the next episode in the directory when one ends")
		plist    = flag.String("playlist", "", "cast a playlist file (one video path per line; # comments allowed)")
	)
	flag.Usage = usage
	flag.Parse()

	if *showVer {
		fmt.Println("movcaster", version)
		return
	}

	// Ensure ~/.movcaster + playback_index exist on every run, and enable resume.
	opts := core.Options{OnEvent: report}
	if rs, err := resume.New(); err != nil {
		fmt.Fprintln(os.Stderr, "movcaster: resume disabled:", err)
	} else {
		opts.Resume = rs
	}
	app := core.New(opts)

	if *list {
		fail(runList(app))
		return
	}

	// Subtitle/codec options apply to every file we cast (the sidecar is the one
	// exception: it's file-specific, so it's only attached to the first file and
	// cleared when advancing).
	base := core.CastRequest{
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

	if *plist != "" {
		if len(flag.Args()) != 0 {
			fmt.Fprintln(os.Stderr, "movcaster: --playlist takes the file list from the playlist; don't also pass a file argument")
			os.Exit(2)
		}
		fail(runPlaylist(app, base, *plist))
		return
	}

	args := flag.Args()
	if len(args) != 1 {
		usage()
		os.Exit(2)
	}
	req := base
	req.File = args[0]
	if *info {
		fail(runInfo(app, req))
		return
	}
	// Default mode: advance to the next episode in the same directory.
	fail(runCast(app, req, nextEpisode, !*noNext))
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
  movcaster --playlist list.txt      cast a playlist (one video path per line)
  movcaster <file> -t TARGET         target a device (IP, IP:port, or device URL)
  movcaster <file> --info            show streams + chosen subtitle strategy
  movcaster <file> --sub foo.srt     force an explicit soft subtitle
  movcaster <file> --sub-track N     pick embedded subtitle track N (see --info)
  movcaster <file> --burn            burn the selected subtitle into the video
  movcaster <file> --soft            force soft subs (errors on bitmap tracks)
  movcaster <file> --mux-soft        remux a bitmap track as soft (experimental)
  movcaster <file> --no-subs         cast without subtitles
  movcaster <file> --transcode       force a codec-compatibility transcode
  movcaster <file> --no-next         do not auto-play the next episode
  movcaster --playlist list.txt      cast each file listed, in order

Playlists: a plain text file with one video path per line (blank lines and #
comments ignored). Absolute paths are used as-is; relative paths resolve against
the current directory. The list plays in order; n skips to the next entry.

Subtitles: a sidecar .srt/.vtt is auto-detected and sent as soft subs; embedded
text tracks are extracted to soft; bitmap tracks (PGS/VobSub/dvd_subtitle) are
burned in on the fly. The chosen device is remembered for next time.

Next episode: when a file ends, the next episode in the same directory (same
show, next season/episode) is detected and cast automatically. Press n to skip
to it manually, or pass --no-next to disable the automatic advance.

Controls (TUI): space play/pause, left/right seek, up/down volume, m mute,
n next episode, q quit.
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

// nextProvider yields the request to cast after the current one. ok is false when
// the sequence is exhausted. It backs both playback modes: directory episode
// detection and an explicit playlist.
type nextProvider func(current core.CastRequest) (core.CastRequest, bool)

// nextEpisode advances to the next episode of the same show in the current file's
// directory (see internal/nextep), carrying the subtitle/codec options forward.
func nextEpisode(cur core.CastRequest) (core.CastRequest, bool) {
	n, ok, _ := nextep.Find(cur.File)
	if !ok {
		return core.CastRequest{}, false
	}
	cur.File = n
	cur.Subtitle.Sidecar = "" // sidecar is file-specific; let auto-detect re-find it
	return cur, true
}

// runCast casts req.File and, when the file ends (or the user presses "n"),
// advances via next until the sequence is exhausted or the user quits. autoNext
// gates only the automatic on-end advance; an explicit "n" always advances if
// there is a next item.
func runCast(app *core.App, req core.CastRequest, next nextProvider, autoNext bool) error {
	ctx := context.Background()
	for {
		cast, _, err := app.Start(ctx, req)
		if err != nil {
			return err
		}
		outcome, runErr := tui.Run(cast, tui.Options{
			Title:     cast.Title(),
			Device:    cast.Device(),
			SubInfo:   cast.SubInfo(),
			HasVolume: cast.HasVolume(),
		})
		cast.Close(ctx)
		if runErr != nil {
			return runErr
		}

		advance := outcome == tui.OutcomeNext ||
			(outcome == tui.OutcomeEnded && autoNext)
		if !advance {
			return nil
		}

		nreq, ok := next(req)
		if !ok {
			return nil // sequence exhausted
		}
		fmt.Println("Up next:", filepath.Base(nreq.File))
		req = nreq
	}
}

// runPlaylist casts the entries of a playlist file in order. Missing or
// non-video entries are skipped with a warning rather than aborting the list.
// On-end advance is always enabled here (the playlist defines the sequence);
// --no-next only applies to directory episode detection.
func runPlaylist(app *core.App, base core.CastRequest, path string) error {
	entries, err := playlist.Load(path)
	if err != nil {
		return err
	}
	files := existingFiles(entries)
	if len(files) == 0 {
		return fmt.Errorf("playlist %q has no playable files", path)
	}

	i := 0
	next := func(core.CastRequest) (core.CastRequest, bool) {
		if i >= len(files) {
			return core.CastRequest{}, false
		}
		r := base
		r.File = files[i]
		r.Subtitle.Sidecar = "" // sidecar applies only to a single file
		i++
		return r, true
	}

	first, _ := next(core.CastRequest{}) // len(files) > 0, so this is non-empty
	return runCast(app, first, next, true)
}

// existingFiles drops entries that don't exist or aren't regular files, warning
// about each so a typo in the playlist doesn't silently disappear.
func existingFiles(entries []string) []string {
	out := entries[:0]
	for _, e := range entries {
		fi, err := os.Stat(e)
		if err != nil || fi.IsDir() {
			fmt.Fprintln(os.Stderr, "movcaster: skipping playlist entry:", e)
			continue
		}
		out = append(out, e)
	}
	return out
}
