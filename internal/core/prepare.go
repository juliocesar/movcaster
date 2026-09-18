package core

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/juliocesar/movcaster/internal/engine"
	"github.com/juliocesar/movcaster/internal/probe"
	"github.com/juliocesar/movcaster/internal/subs"
	"github.com/juliocesar/movcaster/internal/transcode"
)

// defaultProfile is what planning uses when no device is known (--info without
// -t). It matches the LG webOS renderer this tool grew up against; the DLNA
// engine ships the same values as its default.
var defaultProfile = engine.Profile{
	VideoCodecs:        codecSet("h264", "hevc", "mpeg4", "mpeg2video", "vc1", "msmpeg4v3"),
	AudioCodecs:        codecSet("aac", "ac3", "eac3", "mp3", "mp2", "dts", "flac"),
	SubFormats:         []engine.SubFormat{engine.SubFormatSRT},
	LoadStartsPlayback: true,
	TranscodeVideo:     "libx264",
	TranscodeAudio:     "aac",
	TranscodeContainer: "mp4",
}

func codecSet(names ...string) map[string]bool {
	m := make(map[string]bool, len(names))
	for _, n := range names {
		m[n] = true
	}
	return m
}

// SubtitleOptions are the subtitle-related inputs to a cast.
type SubtitleOptions struct {
	Sidecar    string // explicit --sub path, or ""
	NoSubs     bool
	Burn       bool
	Soft       bool // force soft (errors if the chosen track is bitmap)
	MuxSoft    bool // experimental: remux a bitmap track as soft instead of burning
	TrackIndex int  // explicit subtitle stream index (s:N); -1 = auto
}

// CastRequest fully describes a desired cast.
type CastRequest struct {
	File           string
	Target         string // IP / IP:port / engine:host / device URL / "" (auto + saved)
	Engine         string // restrict discovery/resolution to one engine ("" = all)
	Subtitle       SubtitleOptions
	ForceTranscode bool
}

// TranscodeKind is why (if at all) the cast streams a transcode.
type TranscodeKind int

const (
	// TranscodeNone direct-plays the file (byte-range seekable).
	TranscodeNone TranscodeKind = iota
	// TranscodeBurn burns a subtitle track into the video.
	TranscodeBurn
	// TranscodeCodec re-encodes for codec compatibility.
	TranscodeCodec
	// TranscodeRemux changes only the container; both streams are copied.
	TranscodeRemux
)

// TranscodePlan summarizes the transcode decision (filled by Start; Prepare
// leaves it None — codec fallback needs no TV but is reported only at Start).
type TranscodePlan struct {
	Kind  TranscodeKind
	Video bool // codec: re-encode video
	Audio bool // codec: re-encode audio
	Remux bool // container mismatch (streams may still be copied)
}

// Preparation is the result of planning a cast: probe + subtitle strategy. It
// has no side effects on the TV and is reused by both `--info` and Start.
type Preparation struct {
	AbsPath     string
	Info        *probe.MediaInfo // nil if probe failed (see ProbeErr)
	Sidecar     string           // resolved sidecar/--sub path, or ""
	Strategy    subs.Decision    // valid only if DecideErr == nil
	Codec       TranscodePlan    // codec/container need (subs may override at Start)
	Profile     engine.Profile   // the profile the plan was made against
	ProfileName string           // e.g. "dlna (webOS)", for --info output
	ProbeErr    error            // non-fatal for casting, fatal for --info
	DecideErr   error            // subtitle strategy error (e.g. --soft on bitmap)
}

// Prepare probes the file and decides the subtitle strategy against a default
// profile (the first registered engine's, so `--info` without -t reports what a
// bare cast would most likely do). It performs no network or TV I/O; use
// PrepareWithProfile when a real device's profile is known.
func (a *App) Prepare(ctx context.Context, req CastRequest) (*Preparation, error) {
	prof, name := planningProfile(req.Engine)
	return a.PrepareWithProfile(ctx, req, prof, name)
}

// planningProfile picks the profile used when no device has been resolved: the
// named engine's default, else the first registered engine's, else the built-in
// webOS-shaped fallback (hermetic tests register no engines).
func planningProfile(engineName string) (engine.Profile, string) {
	if engineName != "" {
		if e, ok := engine.Lookup(engineName); ok {
			return e.DefaultProfile(), e.Name()
		}
	}
	if es := engine.Engines(); len(es) > 0 {
		return es[0].DefaultProfile(), es[0].Name()
	}
	return defaultProfile, ""
}

// PrepareWithProfile is Prepare against a specific device profile. engineName
// is used only for reporting.
func (a *App) PrepareWithProfile(ctx context.Context, req CastRequest, prof engine.Profile, engineName string) (*Preparation, error) {
	abs, err := filepath.Abs(req.File)
	if err != nil {
		return nil, err
	}
	if _, err := os.Stat(abs); err != nil {
		return nil, fmt.Errorf("file: %w", err)
	}

	p := &Preparation{AbsPath: abs, Profile: prof}
	if engineName != "" {
		p.ProfileName = engineName
		if prof.Name != "" {
			p.ProfileName = fmt.Sprintf("%s (%s)", engineName, prof.Name)
		}
	} else if prof.Name != "" {
		p.ProfileName = prof.Name
	}
	p.Sidecar = a.resolveSubtitle(abs, req.Subtitle.Sidecar)

	pctx, cancel := context.WithTimeout(ctx, 20*time.Second)
	info, perr := probe.Probe(pctx, abs)
	cancel()
	if perr != nil {
		p.ProbeErr = perr
	} else {
		p.Info = info
	}

	p.Codec = codecPlan(p.Info, prof, req.ForceTranscode)

	dec, derr := subs.Decide(subs.Request{
		Info:        p.Info,
		Profile:     prof,
		SidecarPath: p.Sidecar,
		NoSubs:      req.Subtitle.NoSubs,
		ForceBurn:   req.Subtitle.Burn,
		ForceSoft:   req.Subtitle.Soft,
		MuxSoftTry:  req.Subtitle.MuxSoft,
		TrackIndex:  req.Subtitle.TrackIndex,
	})
	if derr != nil {
		p.DecideErr = derr
	} else {
		p.Strategy = dec
	}
	return p, nil
}

// DescribeProfile renders the profile line `--info` prints, or "" when the
// profile has no name to show.
func (p *Preparation) DescribeProfile() string {
	if p.ProfileName == "" {
		return ""
	}
	return fmt.Sprintf("  -> profile: %s\n", p.ProfileName)
}

// resolveSubtitle returns the subtitle path to use: the explicit one if given,
// otherwise a sidecar .srt/.vtt sharing the media's basename. "" if none.
func (a *App) resolveSubtitle(mediaPath, explicit string) string {
	if explicit != "" {
		if _, err := os.Stat(explicit); err == nil {
			return explicit
		}
		a.emit(Warn, "--sub %q not found, ignoring", explicit)
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

// DescribeStreams renders the stream block that `--info` prints (no strategy
// line). It expects Info to be set (probe succeeded).
func (p *Preparation) DescribeStreams() string {
	var b strings.Builder
	fmt.Fprintf(&b, "%s\n", filepath.Base(p.AbsPath))
	if p.Info == nil {
		return b.String()
	}
	fmt.Fprintf(&b, "  duration: %s\n", p.Info.Duration.Round(time.Second))
	fmt.Fprintf(&b, "  video:    %s\n", p.Info.VideoCodec)
	fmt.Fprintf(&b, "  audio:    %s\n", p.Info.AudioCodec)
	if len(p.Info.Subtitles) == 0 {
		fmt.Fprintln(&b, "  subtitles: none embedded")
	} else {
		fmt.Fprintln(&b, "  subtitle tracks:")
		for _, s := range p.Info.Subtitles {
			def := ""
			if s.Default {
				def = " (default)"
			}
			if s.Forced {
				def += " (forced)"
			}
			fmt.Fprintf(&b, "    s:%d  %-18s %-7s %s %s%s\n", s.SubIndex, s.Codec, s.Kind, s.Language, s.Title, def)
		}
	}
	if p.Sidecar != "" {
		fmt.Fprintf(&b, "  sidecar:  %s\n", filepath.Base(p.Sidecar))
	}
	return b.String()
}

// DescribeStrategy renders the chosen-subtitle-strategy line `--info` prints.
func (p *Preparation) DescribeStrategy() string {
	return fmt.Sprintf("  -> strategy: %s  (%s)\n", p.Strategy.Kind, p.Strategy.Reason)
}

// DescribeCodec renders the codec/container-compatibility line `--info` prints,
// or "" when the file direct-plays. A burn-in strategy transcodes anyway, so the
// streams listed here are the ones that would be re-encoded on top of the burn.
func (p *Preparation) DescribeCodec() string {
	switch p.Codec.Kind {
	case TranscodeCodec:
		var what []string
		if p.Codec.Video {
			what = append(what, "video "+p.Info.VideoCodec)
		}
		if p.Codec.Audio {
			what = append(what, "audio "+p.Info.AudioCodec)
		}
		return fmt.Sprintf("  -> transcode: %s not supported by the renderer\n", strings.Join(what, " + "))
	case TranscodeRemux:
		return fmt.Sprintf("  -> remux: container %s not supported by the renderer (streams copied)\n", p.Info.Container)
	default:
		return ""
	}
}

// codecPlan computes the codec/container-compatibility transcode need for a
// profile (no subs involved).
func codecPlan(info *probe.MediaInfo, prof engine.Profile, force bool) TranscodePlan {
	plan := transcode.Needs(info, prof)
	if force {
		plan.Video, plan.Audio = true, true
	}
	switch {
	case plan.Video || plan.Audio:
		return TranscodePlan{Kind: TranscodeCodec, Video: plan.Video, Audio: plan.Audio, Remux: plan.Remux}
	case plan.Remux:
		return TranscodePlan{Kind: TranscodeRemux, Remux: true}
	default:
		return TranscodePlan{Kind: TranscodeNone}
	}
}

// plan converts back to the transcode package's shape for Args.
func (p TranscodePlan) plan() transcode.Plan {
	return transcode.Plan{Video: p.Video, Audio: p.Audio, Remux: p.Remux}
}
