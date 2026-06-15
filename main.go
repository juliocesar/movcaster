// movcaster streams local video to a DLNA renderer (e.g. an LG webOS TV) with
// soft and burned-in subtitle support. CLI/TUI only.
package main

import (
	"context"
	"flag"
	"fmt"
	"net"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"time"

	"github.com/juliocesar/movcaster/internal/cast"
	"github.com/juliocesar/movcaster/internal/config"
	"github.com/juliocesar/movcaster/internal/discovery"
	"github.com/juliocesar/movcaster/internal/mediaserver"
	"github.com/juliocesar/movcaster/internal/probe"
	"github.com/juliocesar/movcaster/internal/renderer"
	"github.com/juliocesar/movcaster/internal/subs"
	"github.com/juliocesar/movcaster/internal/transcode"
	"github.com/juliocesar/movcaster/internal/tui"
)

func main() {
	var (
		list     = flag.Bool("l", false, "list DLNA renderers on the LAN and exit")
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

	if *list {
		fail(runList())
		return
	}

	args := flag.Args()
	if len(args) != 1 {
		usage()
		os.Exit(2)
	}
	opts := castOpts{
		target: *target, sub: *sub, noSubs: *noSubs,
		burn: *burn, soft: *soft, muxSoft: *muxSoft, transcode: *tcode, subTrack: *subTrack,
	}
	if *info {
		fail(runInfo(args[0], opts))
		return
	}
	fail(runCast(args[0], opts))
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

func runList() error {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	devices, err := discovery.Discover(ctx)
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
func runInfo(path string, opts castOpts) error {
	abs, err := filepath.Abs(path)
	if err != nil {
		return err
	}
	if _, err := os.Stat(abs); err != nil {
		return fmt.Errorf("file: %w", err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	mi, err := probe.Probe(ctx, abs)
	if err != nil {
		return err
	}
	fmt.Printf("%s\n", filepath.Base(abs))
	fmt.Printf("  duration: %s\n", mi.Duration.Round(time.Second))
	fmt.Printf("  video:    %s\n", mi.VideoCodec)
	fmt.Printf("  audio:    %s\n", mi.AudioCodec)
	if len(mi.Subtitles) == 0 {
		fmt.Println("  subtitles: none embedded")
	} else {
		fmt.Println("  subtitle tracks:")
		for _, s := range mi.Subtitles {
			def := ""
			if s.Default {
				def = " (default)"
			}
			fmt.Printf("    s:%d  %-18s %-7s %s %s%s\n", s.SubIndex, s.Codec, s.Kind, s.Language, s.Title, def)
		}
	}
	if sc := resolveSubtitle(abs, opts.sub); sc != "" {
		fmt.Printf("  sidecar:  %s\n", filepath.Base(sc))
	}

	dec, err := subs.Decide(subs.Request{
		Info: mi, SidecarPath: resolveSubtitle(abs, opts.sub),
		NoSubs: opts.noSubs, ForceBurn: opts.burn, ForceSoft: opts.soft, MuxSoftTry: opts.muxSoft, TrackIndex: opts.subTrack,
	})
	if err != nil {
		return err
	}
	fmt.Printf("  -> strategy: %s  (%s)\n", dec.Kind, dec.Reason)
	return nil
}

type castOpts struct {
	target    string
	sub       string
	noSubs    bool
	burn      bool
	soft      bool
	muxSoft   bool
	transcode bool
	subTrack  int
}

func runCast(path string, opts castOpts) error {
	if err := ensureFFmpeg(); err != nil {
		return err
	}
	if _, err := os.Stat(path); err != nil {
		return fmt.Errorf("file: %w", err)
	}
	abs, err := filepath.Abs(path)
	if err != nil {
		return err
	}

	ctx, cancel := context.WithTimeout(context.Background(), 8*time.Second)
	defer cancel()
	dev, err := selectDevice(ctx, opts.target)
	if err != nil {
		return err
	}
	fmt.Printf("Casting to %s [%s]\n", dev.FriendlyName, dev.Location.Host)

	// Remember this device so a future bare `movcaster <file>` finds it again.
	_ = config.Save(config.Config{LastDeviceHost: hostOf(dev.Location.Host)})

	srv, err := mediaserver.New(dev.Location.Host)
	if err != nil {
		return err
	}
	srv.Start()
	defer srv.Shutdown(context.Background())

	srv.SetDirectPlay(abs, mimeForExt(abs))
	rend := renderer.New(*dev)

	media := renderer.Media{
		URL:      srv.MediaURL(),
		Title:    filepath.Base(abs),
		MIME:     mimeForExt(abs),
		Seekable: true,
	}

	tmpDir, err := os.MkdirTemp("", "movcaster-")
	if err != nil {
		return err
	}
	defer os.RemoveAll(tmpDir)

	// Probe once; drives both subtitle and codec decisions.
	pctx, pcancel := context.WithTimeout(context.Background(), 20*time.Second)
	info, perr := probe.Probe(pctx, abs)
	pcancel()
	if perr != nil {
		fmt.Fprintln(os.Stderr, "movcaster: ffprobe failed, auto-detection limited:", perr)
	}
	// Advertise the real duration in DIDL-Lite. Direct-play files declare it in
	// their container anyway, but a live transcode streams an empty-moov fragmented
	// MP4 with no total duration, so the TV's seek bar races without this hint.
	if info != nil {
		media.Duration = info.Duration
	}

	res, err := setupSubtitles(srv, &media, abs, tmpDir, info, opts)
	if err != nil {
		return err
	}

	// Codec-compatibility fallback (M8): only when not already transcoding for subs.
	if res.buildTranscode == nil {
		needV, needA := transcode.Needs(info)
		if opts.transcode {
			needV, needA = true, true
		}
		if needV || needA {
			fmt.Printf("Transcoding for codec compatibility (video=%v audio=%v)...\n", needV, needA)
			res.buildTranscode = func(ss time.Duration) []string {
				return transcode.Args(abs, ss, needV, needA)
			}
			if res.label == "" {
				res.label = "transcode"
			}
			applyTranscode(srv, &media, res.buildTranscode)
		}
	}

	setCtx, setCancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer setCancel()
	if err := rend.SetMedia(setCtx, media); err != nil {
		return fmt.Errorf("SetAVTransportURI: %w", err)
	}
	if err := rend.Play(setCtx); err != nil {
		return fmt.Errorf("Play: %w", err)
	}

	knownDur := time.Duration(0)
	if info != nil {
		knownDur = info.Duration
	}
	sess := cast.New(cast.Config{
		Renderer: rend, Server: srv, Media: media,
		KnownDuration: knownDur, BuildTranscodeArgs: res.buildTranscode,
	})

	return tui.Run(sess, tui.Options{
		Title:     filepath.Base(abs),
		Device:    dev.FriendlyName,
		SubInfo:   res.label,
		HasVolume: rend.HasVolume(),
	})
}

// subResult is the outcome of subtitle setup. buildTranscode is non-nil when
// subtitles require an on-the-fly transcode (burn-in).
type subResult struct {
	label          string
	buildTranscode func(ss time.Duration) []string
}

// applyTranscode points the server + media at the transcode stream from offset 0.
func applyTranscode(srv *mediaserver.Server, media *renderer.Media, build func(time.Duration) []string) {
	srv.SetTranscode(build(0))
	media.URL = srv.MediaURL()
	media.MIME = "video/mp4"
	media.Seekable = false
}

// setupSubtitles decides a subtitle strategy and applies it to the server + media
// metadata, returning a label and an optional transcode-args builder.
func setupSubtitles(srv *mediaserver.Server, media *renderer.Media, abs, tmpDir string, info *probe.MediaInfo, opts castOpts) (subResult, error) {
	dec, err := subs.Decide(subs.Request{
		Info:        info,
		SidecarPath: resolveSubtitle(abs, opts.sub),
		NoSubs:      opts.noSubs,
		ForceBurn:   opts.burn,
		ForceSoft:   opts.soft,
		MuxSoftTry:  opts.muxSoft,
		TrackIndex:  opts.subTrack,
	})
	if err != nil {
		return subResult{}, err
	}

	switch dec.Kind {
	case subs.None:
		return subResult{}, nil

	case subs.SoftSidecar:
		mime, typ := subKind(dec.SidecarPath)
		srv.SetSubtitle(dec.SidecarPath, mime)
		media.SubURL, media.SubMIME, media.SubType = srv.SubURL(), mime, typ
		label := "soft: " + filepath.Base(dec.SidecarPath)
		fmt.Println("Subtitles:", label)
		return subResult{label: label}, nil

	case subs.SoftExtract:
		fmt.Printf("Subtitles: extracting text track %d (%s)...\n", dec.Track.SubIndex, dec.Track.Codec)
		ectx, ecancel := context.WithTimeout(context.Background(), 60*time.Second)
		defer ecancel()
		vtt, err := subs.ExtractText(ectx, abs, dec.Track.SubIndex, tmpDir)
		if err != nil {
			return subResult{}, err
		}
		srv.SetSubtitle(vtt, "text/vtt")
		media.SubURL, media.SubMIME, media.SubType = srv.SubURL(), "text/vtt", "vtt"
		label := fmt.Sprintf("soft: embedded track %d", dec.Track.SubIndex)
		fmt.Println("Subtitles:", label)
		return subResult{label: label}, nil

	case subs.MuxSoft:
		fmt.Printf("Subtitles: remuxing bitmap track %d (%s) as soft [experimental]...\n", dec.Track.SubIndex, dec.Track.Codec)
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Minute)
		defer cancel()
		muxed, err := subs.MuxSoftRemux(ctx, abs, *dec.Track, tmpDir)
		if err != nil {
			return subResult{}, err
		}
		srv.SetDirectPlay(muxed, "video/x-matroska")
		media.URL, media.MIME, media.Seekable = srv.MediaURL(), "video/x-matroska", true
		label := fmt.Sprintf("mux-soft: track %d (toggle subs on the TV)", dec.Track.SubIndex)
		fmt.Println("Subtitles:", label)
		return subResult{label: label}, nil

	case subs.Burn:
		fmt.Printf("Subtitles: burning in %s track %d (%s) on the fly...\n", dec.Track.Kind, dec.Track.SubIndex, dec.Track.Codec)
		track := *dec.Track
		build := func(ss time.Duration) []string { return subs.BurnArgs(abs, track, ss) }
		applyTranscode(srv, media, build)
		label := fmt.Sprintf("burn-in: track %d (%s)", dec.Track.SubIndex, dec.Track.Codec)
		fmt.Println("Subtitles:", label)
		return subResult{label: label, buildTranscode: build}, nil
	}
	return subResult{}, nil
}

// resolveSubtitle returns the subtitle path to use: the explicit one if given,
// otherwise a sidecar .srt/.vtt sharing the media's basename. "" if none.
func resolveSubtitle(mediaPath, explicit string) string {
	if explicit != "" {
		if _, err := os.Stat(explicit); err == nil {
			return explicit
		}
		fmt.Fprintf(os.Stderr, "movcaster: --sub %q not found, ignoring\n", explicit)
		return ""
	}
	base := strings.TrimSuffix(mediaPath, filepath.Ext(mediaPath))
	for _, ext := range []string{".srt", ".vtt"} {
		cand := base + ext
		if _, err := os.Stat(cand); err == nil {
			return cand
		}
	}
	return ""
}

// subKind returns the (HTTP/protocolInfo mime, sec:type) for a subtitle file.
func subKind(path string) (mime, secType string) {
	switch strings.ToLower(filepath.Ext(path)) {
	case ".vtt":
		return "text/vtt", "vtt"
	default: // .srt and anything else treated as SubRip
		return "text/srt", "srt"
	}
}

// selectDevice resolves the target flag to a device. Empty target picks the sole
// device. A target is matched against discovered devices by host IP (robust to
// the TV's dynamic control port), falling back to a direct description-URL load.
func selectDevice(ctx context.Context, target string) (*discovery.Device, error) {
	devices, derr := discovery.Discover(ctx)

	if target == "" {
		if derr != nil {
			return nil, derr
		}
		// Prefer the last-used device if it's on the network.
		if saved := config.Load().LastDeviceHost; saved != "" {
			for i := range devices {
				if devices[i].Location != nil && hostOf(devices[i].Location.Host) == saved {
					return &devices[i], nil
				}
			}
		}
		switch len(devices) {
		case 0:
			return nil, fmt.Errorf("no renderers found; pass -t with a device IP/URL")
		case 1:
			return &devices[0], nil
		default:
			return nil, fmt.Errorf("%d renderers found; pick one with -t (run -l to list)", len(devices))
		}
	}

	wantHost := hostOf(target)
	for i := range devices {
		if devices[i].Location != nil && hostOf(devices[i].Location.Host) == wantHost {
			return &devices[i], nil
		}
	}
	// Fall back to treating target as a full device-description URL.
	if u, err := url.Parse(ensureScheme(target)); err == nil && u.Host != "" {
		if d, err := discovery.FindByURL(ctx, u); err == nil {
			return d, nil
		}
	}
	return nil, fmt.Errorf("target %q not found among %d discovered renderer(s)", target, len(devices))
}

func ensureScheme(s string) string {
	if !strings.Contains(s, "://") {
		return "http://" + s
	}
	return s
}

// hostOf extracts the bare host/IP from an IP, IP:port, or URL.
func hostOf(s string) string {
	u, err := url.Parse(ensureScheme(s))
	if err == nil && u.Hostname() != "" {
		return u.Hostname()
	}
	if h, _, err := net.SplitHostPort(s); err == nil {
		return h
	}
	return s
}

// ensureFFmpeg verifies ffmpeg and ffprobe are on PATH.
func ensureFFmpeg() error {
	for _, bin := range []string{"ffmpeg", "ffprobe"} {
		if _, err := exec.LookPath(bin); err != nil {
			return fmt.Errorf("%s not found on PATH; install ffmpeg (e.g. `brew install ffmpeg`)", bin)
		}
	}
	return nil
}

func mimeForExt(path string) string {
	switch strings.ToLower(filepath.Ext(path)) {
	case ".mp4", ".m4v":
		return "video/mp4"
	case ".mkv":
		return "video/x-matroska"
	case ".avi":
		return "video/x-msvideo"
	case ".mov":
		return "video/quicktime"
	case ".webm":
		return "video/webm"
	case ".ts":
		return "video/mp2t"
	case ".wmv":
		return "video/x-ms-wmv"
	default:
		return "video/mp4"
	}
}
