package core

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/juliocesar/movcaster/internal/probe"
	"github.com/juliocesar/movcaster/internal/subs"
	"github.com/juliocesar/movcaster/internal/transcode"
)

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
	Target         string // IP / IP:port / device URL / "" (auto + saved)
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
)

// TranscodePlan summarizes the transcode decision (filled by Start; Prepare
// leaves it None — codec fallback needs no TV but is reported only at Start).
type TranscodePlan struct {
	Kind  TranscodeKind
	Video bool // codec: re-encode video
	Audio bool // codec: re-encode audio
}

// Preparation is the result of planning a cast: probe + subtitle strategy. It
// has no side effects on the TV and is reused by both `--info` and Start.
type Preparation struct {
	AbsPath   string
	Info      *probe.MediaInfo // nil if probe failed (see ProbeErr)
	Sidecar   string           // resolved sidecar/--sub path, or ""
	Strategy  subs.Decision    // valid only if DecideErr == nil
	ProbeErr  error            // non-fatal for casting, fatal for --info
	DecideErr error            // subtitle strategy error (e.g. --soft on bitmap)
}

// Prepare probes the file and decides the subtitle strategy. It performs no
// network or TV I/O. A missing file is a hard error; a probe failure is recorded
// in ProbeErr (callers choose severity); a strategy error in DecideErr.
func (a *App) Prepare(ctx context.Context, req CastRequest) (*Preparation, error) {
	abs, err := filepath.Abs(req.File)
	if err != nil {
		return nil, err
	}
	if _, err := os.Stat(abs); err != nil {
		return nil, fmt.Errorf("file: %w", err)
	}

	p := &Preparation{AbsPath: abs}
	p.Sidecar = a.resolveSubtitle(abs, req.Subtitle.Sidecar)

	pctx, cancel := context.WithTimeout(ctx, 20*time.Second)
	info, perr := probe.Probe(pctx, abs)
	cancel()
	if perr != nil {
		p.ProbeErr = perr
	} else {
		p.Info = info
	}

	dec, derr := subs.Decide(subs.Request{
		Info:        p.Info,
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

// codecPlan computes the codec-compatibility transcode need (no subs involved).
func codecPlan(info *probe.MediaInfo, force bool) TranscodePlan {
	needV, needA := transcode.Needs(info)
	if force {
		needV, needA = true, true
	}
	if needV || needA {
		return TranscodePlan{Kind: TranscodeCodec, Video: needV, Audio: needA}
	}
	return TranscodePlan{Kind: TranscodeNone}
}
