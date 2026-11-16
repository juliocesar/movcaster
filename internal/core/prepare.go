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
	AudioTrack     int // audio stream a:N; -1 = auto (English if present)
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
	// TranscodeRemux is a copy-only remux: the container is unsupported, or the
	// file has several audio tracks and the output must carry only the chosen one
	// (a TV handed the raw file picks a track itself).
	TranscodeRemux
)

// TranscodePlan summarizes the transcode decision for the codecs and the chosen
// audio track (a burn-in strategy may supersede it at Start).
type TranscodePlan struct {
	Kind       TranscodeKind
	Video      bool // codec: re-encode video
	Audio      bool // codec: re-encode audio
	Remux      bool // container mismatch (streams may still be copied)
	AudioTrack int  // the a:N stream the output carries
	// AudioChannels is the re-encoded audio's channel count (with Audio).
	AudioChannels int
}

// Preparation is the result of planning a cast: probe + subtitle strategy. It
// has no side effects on the TV and is reused by both `--info` and Start.
type Preparation struct {
	AbsPath     string
	Info        *probe.MediaInfo // nil if probe failed (see ProbeErr)
	Sidecar     string           // resolved sidecar/--sub path, or ""
	Strategy    subs.Decision    // valid only if DecideErr == nil
	Audio       int              // resolved audio stream a:N
	Codec       TranscodePlan    // codec/container/audio-track need (subs may override at Start)
	Profile     engine.Profile   // the profile the plan was made against
	ProfileName string           // e.g. "dlna (webOS)", for --info output
	ProbeErr    error            // non-fatal for casting, fatal for --info
	DecideErr   error            // strategy error (e.g. --soft on bitmap, unknown --audio-track)
}

// Prepare probes the file and decides the subtitle strategy against the
// profile a bare cast would most likely use: the last-used TV's (its engine's
// default plus any probed override from the config), else the first registered
// engine's. It performs no network or TV I/O; use PrepareWithProfile when a
// real device's profile is known.
func (a *App) Prepare(ctx context.Context, req CastRequest) (*Preparation, error) {
	prof, name := a.planningProfile(req.Engine)
	return a.PrepareWithProfile(ctx, req, prof, name)
}

// planningProfile picks the profile used when no device has been resolved: the
// named engine's default, else the saved last device's engine (with its config
// override), else the first registered engine's, else the built-in webOS-shaped
// fallback (hermetic tests register no engines).
func (a *App) planningProfile(engineName string) (engine.Profile, string) {
	cfg := a.store.Load()
	if engineName == "" {
		engineName = cfg.LastDeviceEngine
	}
	if engineName != "" {
		if e, ok := engine.Lookup(engineName); ok {
			prof := e.DefaultProfile()
			if engineName == cfg.LastDeviceEngine {
				if o, ok := cfg.DeviceProfiles[engineName+":"+cfg.LastDeviceHost]; ok {
					prof = applyOverride(prof, o)
				}
			}
			return prof, e.Name()
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

	audio, aerr := selectAudio(p.Info, req.AudioTrack)
	p.Audio = audio
	p.Codec = codecPlan(p.Info, prof, req.ForceTranscode, audio)

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
	if aerr != nil && p.DecideErr == nil {
		p.DecideErr = aerr
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
	switch len(p.Info.Audio) {
	case 0:
		fmt.Fprintln(&b, "  audio:    none")
	case 1:
		fmt.Fprintf(&b, "  audio:    %s\n", strings.TrimPrefix(audioLabel(p.Info.Audio[0]), "a:0 — "))
	default:
		fmt.Fprintln(&b, "  audio tracks:")
		for _, t := range p.Info.Audio {
			fmt.Fprintf(&b, "    %s\n", audioLabel(t))
		}
	}
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

// DescribeAudio renders the chosen-audio line `--info` prints, or "" when the
// file has fewer than two tracks (nothing was chosen).
func (p *Preparation) DescribeAudio() string {
	label := audioTrackLabel(p.Info, p.Audio)
	if label == "" {
		return ""
	}
	// audioTrackLabel is non-empty only for multi-track files, which always remux.
	return fmt.Sprintf("  -> audio: %s  (remux: the TV would otherwise pick a track itself)\n", label)
}

// DescribeCodec renders the codec/container-compatibility line `--info` prints,
// or "" when the file direct-plays (or only remuxes to pick an audio track,
// which DescribeAudio reports). A burn-in strategy transcodes anyway, so the
// streams listed here are the ones that would be re-encoded on top of the burn.
func (p *Preparation) DescribeCodec() string {
	switch p.Codec.Kind {
	case TranscodeCodec:
		var what []string
		if p.Codec.Video {
			what = append(what, "video "+p.Info.VideoCodec)
		}
		if p.Codec.Audio {
			codec := p.Info.AudioCodec
			if p.Codec.AudioTrack < len(p.Info.Audio) {
				codec = p.Info.Audio[p.Codec.AudioTrack].Codec
			}
			what = append(what, fmt.Sprintf("audio %s (-> %s)", codec, channelLayout(p.Codec.AudioChannels)))
		}
		if p.Codec.Remux {
			what = append(what, "container "+p.Info.Container)
		}
		return fmt.Sprintf("  -> transcode: %s not supported by the renderer\n", strings.Join(what, " + "))
	case TranscodeRemux:
		if !p.Codec.Remux {
			return "" // audio-track remux only: DescribeAudio reports it
		}
		return fmt.Sprintf("  -> remux: container %s not supported by the renderer (streams copied)\n", p.Info.Container)
	default:
		return ""
	}
}

// codecPlan computes the transcode need for a profile and the chosen audio
// track a:N (no subs involved): re-encode what the renderer can't play, else
// remux copy-only when the container is unsupported or the file has other
// tracks the TV could pick instead, else direct play.
func codecPlan(info *probe.MediaInfo, prof engine.Profile, force bool, audio int) TranscodePlan {
	plan := transcode.Needs(info, prof, audio)
	if force {
		plan = transcode.ForceAll(info, prof, audio)
	}
	switch {
	case plan.Video || plan.Audio:
		return TranscodePlan{Kind: TranscodeCodec, Video: plan.Video, Audio: plan.Audio, Remux: plan.Remux,
			AudioTrack: audio, AudioChannels: plan.AudioChannels}
	case plan.Remux || !directPlaysAudio(info):
		return TranscodePlan{Kind: TranscodeRemux, Remux: plan.Remux, AudioTrack: audio}
	default:
		return TranscodePlan{Kind: TranscodeNone, AudioTrack: audio}
	}
}

// plan converts back to the transcode package's shape for Args.
func (p TranscodePlan) plan() transcode.Plan {
	return transcode.Plan{Video: p.Video, Audio: p.Audio, Remux: p.Remux, AudioChannels: p.AudioChannels}
}
