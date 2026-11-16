package core

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"time"

	"github.com/juliocesar/movcaster/internal/config"
	"github.com/juliocesar/movcaster/internal/engine"
)

// probeClip is one generated test clip: a (container, video, audio) tuple the
// device either plays or does not.
type probeClip struct {
	Container string
	Video     string
	Audio     string
	// encoder overrides / extra ffmpeg args
	vEnc, aEnc string
	extra      []string
	// special limits probes
	size     string // "1280x720" default
	channels int    // 1 default (sine); 6 probes the 5.1 limit
}

// probeMatrix is the curated set of valid (container, video, audio) muxings.
// Invalid combinations (DTS in WebM, ...) are simply not listed. A tuple that
// fails may fail on the container OR a codec; the report derives an axis as
// supported when ANY tuple carrying it played.
var probeMatrix = []probeClip{
	{Container: "mp4", Video: "h264", Audio: "aac"},
	{Container: "mp4", Video: "h264", Audio: "ac3"},
	{Container: "mp4", Video: "h264", Audio: "eac3"},
	{Container: "mp4", Video: "hevc", Audio: "aac", vEnc: "libx265", extra: []string{"-tag:v", "hvc1"}},
	{Container: "mp4", Video: "av1", Audio: "aac", vEnc: "libaom-av1", extra: []string{"-cpu-used", "8", "-b:v", "500k"}},
	{Container: "matroska", Video: "h264", Audio: "aac"},
	{Container: "matroska", Video: "h264", Audio: "dts", aEnc: "dca", extra: []string{"-strict", "-2"}},
	{Container: "matroska", Video: "h264", Audio: "flac"},
	{Container: "matroska", Video: "h264", Audio: "opus", aEnc: "libopus"},
	{Container: "matroska", Video: "h264", Audio: "mp3", aEnc: "libmp3lame"},
	{Container: "webm", Video: "vp9", Audio: "opus", vEnc: "libvpx-vp9", aEnc: "libopus"},
	{Container: "webm", Video: "vp8", Audio: "vorbis", vEnc: "libvpx", aEnc: "libvorbis"},
	// Limits: a 4K clip and a 5.1 clip, in the safest container/codec pairing.
	{Container: "mp4", Video: "h264", Audio: "aac", size: "3840x2160"},
	{Container: "mp4", Video: "h264", Audio: "aac", channels: 6},
}

// ProbeOutcome is one tuple's verdict.
type ProbeOutcome struct {
	Clip    string `json:"clip"`
	Played  bool   `json:"played"`
	Skipped string `json:"skipped,omitempty"` // generation failed (missing encoder)
	Detail  string `json:"detail,omitempty"`  // load error / final state
}

// ProbeReport is what --probe-device prints: the raw outcomes plus the derived
// ProfileOverride, which ProbeDevice also saves into the config for the device.
type ProbeReport struct {
	Device   string                 `json:"device"`
	Key      string                 `json:"key"` // "engine:host" config key
	Outcomes []ProbeOutcome         `json:"outcomes"`
	Override config.ProfileOverride `json:"profile_override"`
	Saved    bool                   `json:"-"` // Override was written to the config
}

// ProbeDevice generates small test clips, casts each at the target device, and
// reports what actually played. The result is empirical, not a spec sheet — but
// a 3-second tone is still not a two-hour film: passthrough audio in particular
// can behave differently on real tracks, so treat the output as a starting
// point and verify one real file.
func (a *App) ProbeDevice(ctx context.Context, target, only string) (*ProbeReport, error) {
	if err := a.Doctor(ctx); err != nil {
		return nil, err
	}
	dev, err := a.selectDevice(ctx, target, only)
	if err != nil {
		return nil, err
	}
	a.emit(Info, "Probing %s [%s] (%s)", dev.Name(), dev.Addr(), dev.Engine())

	tmpDir, err := os.MkdirTemp("", "movcaster-probe-")
	if err != nil {
		return nil, err
	}
	defer os.RemoveAll(tmpDir)

	srv, err := a.newServer(dev.Host())
	if err != nil {
		return nil, err
	}
	srv.Start()
	defer srv.Shutdown(context.Background())

	sess, err := dev.Open(ctx)
	if err != nil {
		return nil, err
	}
	defer sess.Close()

	rep := &ProbeReport{Device: dev.Name(), Key: profileKey(dev)}
	for _, clip := range probeMatrix {
		out := a.probeOne(ctx, srv, sess, tmpDir, clip)
		rep.Outcomes = append(rep.Outcomes, out)
		status := "PASS"
		switch {
		case out.Skipped != "":
			status = "SKIP (" + out.Skipped + ")"
		case !out.Played:
			status = "FAIL"
			if out.Detail != "" {
				status += " (" + out.Detail + ")"
			}
		}
		a.emit(Info, "  %-28s %s", out.Clip, status)
	}
	// Leave the device quiet when done.
	stopCtx, cancel := context.WithTimeout(ctx, 5*time.Second)
	_ = sess.Stop(stopCtx)
	cancel()

	rep.Override = deriveOverride(rep.Outcomes)
	// Save it as this device's profile, so later casts use it with no hand
	// editing. A run where nothing played says nothing about the device (the
	// network or the session broke), so it never replaces a saved profile.
	if anyPlayed(rep.Outcomes) {
		a.updateConfig(func(c *config.Config) {
			if c.DeviceProfiles == nil {
				c.DeviceProfiles = map[string]config.ProfileOverride{}
			}
			c.DeviceProfiles[rep.Key] = rep.Override
		})
		rep.Saved = true
	}
	return rep, nil
}

func anyPlayed(outcomes []ProbeOutcome) bool {
	for _, o := range outcomes {
		if o.Played {
			return true
		}
	}
	return false
}

// probeOne generates, serves, and watches one clip.
func (a *App) probeOne(ctx context.Context, srv MediaServer, sess engine.Session, tmpDir string, clip probeClip) ProbeOutcome {
	name := fmt.Sprintf("%s/%s/%s", clip.Container, clip.Video, clip.Audio)
	if clip.size != "" {
		name += " @" + clip.size
	}
	if clip.channels > 1 {
		name += fmt.Sprintf(" %dch", clip.channels)
	}
	out := ProbeOutcome{Clip: name}

	path, err := generateClip(ctx, tmpDir, clip)
	if err != nil {
		out.Skipped = "could not generate: " + firstLine(err.Error())
		return out
	}

	srv.SetDirectPlay(path, containerMIME(clip.Container))
	media := engine.Media{
		URL: srv.MediaURL(), Title: "movcaster probe", MIME: containerMIME(clip.Container),
		Duration: 4 * time.Second, Seekable: true,
	}
	lctx, cancel := context.WithTimeout(ctx, 20*time.Second)
	err = sess.Load(lctx, media)
	cancel()
	if err != nil {
		out.Detail = firstLine(err.Error())
		return out
	}
	pctx, pcancel := context.WithTimeout(ctx, 10*time.Second)
	_ = sess.Play(pctx)
	pcancel()

	// Watch for up to ~8s: reaching PLAYING (or finishing the 4s clip) is a
	// pass; never leaving buffering, or landing idle without finishing, is not.
	deadline := time.Now().Add(8 * time.Second)
	var last engine.State
	for time.Now().Before(deadline) {
		sctx, scancel := context.WithTimeout(ctx, 3*time.Second)
		st, err := sess.Status(sctx)
		scancel()
		if err == nil {
			last = st
			if st.Player == engine.StatePlaying || st.Player == engine.StateFinished {
				out.Played = true
				break
			}
		}
		if !sleepCtx(ctx, 500*time.Millisecond) {
			break
		}
	}
	if !out.Played {
		out.Detail = fmt.Sprintf("stuck in %s", last.Player)
	}
	stopCtx, scancel := context.WithTimeout(ctx, 4*time.Second)
	_ = sess.Stop(stopCtx)
	scancel()
	return out
}

// generateClip renders a 4-second test-pattern clip with ffmpeg lavfi sources.
func generateClip(ctx context.Context, dir string, clip probeClip) (string, error) {
	size := clip.size
	if size == "" {
		size = "1280x720"
	}
	channels := clip.channels
	if channels == 0 {
		channels = 1
	}
	vEnc := clip.vEnc
	if vEnc == "" {
		vEnc = map[string]string{"h264": "libx264"}[clip.Video]
		if vEnc == "" {
			vEnc = clip.Video
		}
	}
	aEnc := clip.aEnc
	if aEnc == "" {
		aEnc = clip.Audio
	}
	ext := map[string]string{"mp4": ".mp4", "matroska": ".mkv", "webm": ".webm"}[clip.Container]
	out := filepath.Join(dir, fmt.Sprintf("probe-%s-%s-%s-%s-%dch%s",
		clip.Container, clip.Video, clip.Audio, size, channels, ext))

	args := []string{"-y", "-hide_banner", "-loglevel", "error",
		"-f", "lavfi", "-i", "testsrc=size=" + size + ":rate=30:duration=4",
		"-f", "lavfi", "-i", "sine=frequency=440:duration=4",
	}
	if channels > 1 {
		args = append(args, "-af", fmt.Sprintf("aformat=channel_layouts=%s", layoutFor(channels)))
	}
	args = append(args, "-c:v", vEnc, "-c:a", aEnc, "-ac", fmt.Sprint(channels))
	args = append(args, clip.extra...)
	args = append(args, "-f", clip.Container, out)

	gctx, cancel := context.WithTimeout(ctx, 2*time.Minute)
	defer cancel()
	cmd := exec.CommandContext(gctx, "ffmpeg", args...)
	if b, err := cmd.CombinedOutput(); err != nil {
		return "", fmt.Errorf("%v: %s", err, lastNonEmptyLine(string(b)))
	}
	return out, nil
}

func layoutFor(channels int) string {
	if channels == 6 {
		return "5.1"
	}
	return "stereo"
}

func containerMIME(container string) string {
	switch container {
	case "matroska":
		return "video/x-matroska"
	case "webm":
		return "video/webm"
	default:
		return "video/mp4"
	}
}

// deriveOverride turns the outcomes into a config ProfileOverride: an axis
// value is supported when any tuple carrying it played. Skipped tuples say
// nothing. The 4K and 5.1 probes set the limits only when they FAIL while the
// baseline passed.
func deriveOverride(outcomes []ProbeOutcome) config.ProfileOverride {
	containers := map[string]bool{}
	vcodecs := map[string]bool{}
	acodecs := map[string]bool{}
	var o config.ProfileOverride
	baselinePlayed := false
	for i, out := range outcomes {
		clip := probeMatrix[i]
		if clip.size == "" && clip.channels == 0 && out.Played {
			containers[clip.Container] = true
			vcodecs[clip.Video] = true
			acodecs[clip.Audio] = true
			if clip.Container == "mp4" && clip.Video == "h264" && clip.Audio == "aac" {
				baselinePlayed = true
			}
		}
		if clip.size == "3840x2160" && !out.Played && out.Skipped == "" && baselinePlayed {
			o.MaxVideoHeight = 1080
		}
		if clip.channels == 6 && out.Skipped == "" && baselinePlayed {
			if out.Played {
				// The clip is 5.1 AAC, exactly what a surround re-encode produces,
				// so re-encodes may keep 5.1 instead of downmixing to stereo.
				o.TranscodeAudioChannels = 6
			} else {
				o.MaxAudioChannels = 2
			}
		}
	}
	o.Containers = sortedKeys(containers)
	o.VideoCodecs = sortedKeys(vcodecs)
	o.AudioCodecs = sortedKeys(acodecs)
	return o
}

func sortedKeys(m map[string]bool) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	sort.Strings(out)
	return out
}

func firstLine(s string) string {
	for i := 0; i < len(s); i++ {
		if s[i] == '\n' {
			return s[:i]
		}
	}
	return s
}

func lastNonEmptyLine(s string) string {
	lines := []string{}
	start := 0
	for i := 0; i <= len(s); i++ {
		if i == len(s) || s[i] == '\n' {
			if l := s[start:i]; len(l) > 0 {
				lines = append(lines, l)
			}
			start = i + 1
		}
	}
	if len(lines) == 0 {
		return ""
	}
	return lines[len(lines)-1]
}

// MarshalReport renders the report as indented JSON for --probe-device output.
func (r *ProbeReport) MarshalReport() string {
	b, err := json.MarshalIndent(r, "", "  ")
	if err != nil {
		return err.Error()
	}
	return string(b)
}
