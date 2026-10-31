package core

import (
	"fmt"
	"strings"

	"github.com/juliocesar/movcaster/internal/probe"
)

// Audio track selection. Tracks are addressed by a:N (index among audio streams,
// ffmpeg's -map 0:a:N). Given a raw multi-track file, the TV picks the track
// itself (webOS applies its own language preference: a:0 Spanish direct-played
// as English), so a multi-track cast always streams a copy-only remux carrying
// only the chosen track (see codecPlan / TranscodeAudio).

// defaultAudio is the file's own preferred track: the first default-flagged one,
// else a:0. Returns 0 when there are no tracks.
func defaultAudio(info *probe.MediaInfo) int {
	if info == nil {
		return 0
	}
	for _, t := range info.Audio {
		if t.Default {
			return t.AudioIndex
		}
	}
	return 0
}

// directPlaysAudio reports whether a direct-play cast reliably plays the chosen
// track: only when there is nothing else the TV could pick instead.
func directPlaysAudio(info *probe.MediaInfo) bool {
	return info == nil || len(info.Audio) <= 1
}

// selectAudio resolves the a:N track to cast. An explicit request (>= 0) must
// exist. Auto picks the first English track (a default-flagged one first, and not
// a commentary when a peer exists), else the file's default track. A file with no
// (probed) audio resolves to 0, which the "?" in the -map selector tolerates.
func selectAudio(info *probe.MediaInfo, requested int) (int, error) {
	var tracks []probe.AudioTrack
	if info != nil {
		tracks = info.Audio
	}
	if requested >= 0 {
		if requested < len(tracks) {
			return requested, nil
		}
		return 0, fmt.Errorf("audio track %d not found (%d tracks available)", requested, len(tracks))
	}

	var english []probe.AudioTrack
	for _, t := range tracks {
		if isEnglish(t.Language) {
			english = append(english, t)
		}
	}
	if len(english) == 0 {
		return defaultAudio(info), nil
	}
	// Commentary tracks are English more often than not; never prefer one when
	// the film's own English track is there.
	var main []probe.AudioTrack
	for _, t := range english {
		if !strings.Contains(strings.ToLower(t.Title), "commentary") {
			main = append(main, t)
		}
	}
	if len(main) == 0 {
		main = english
	}
	for _, t := range main {
		if t.Default {
			return t.AudioIndex, nil
		}
	}
	return main[0].AudioIndex, nil
}

func isEnglish(lang string) bool {
	switch strings.ToLower(lang) {
	case "eng", "en":
		return true
	}
	return false
}

// audioLabel formats one audio track for the picker, the TUI header, and events,
// e.g. `a:1 — English "Director's Commentary" (eac3, 5.1)`.
func audioLabel(t probe.AudioTrack) string {
	var b strings.Builder
	fmt.Fprintf(&b, "a:%d", t.AudioIndex)
	if t.Language != "" {
		fmt.Fprintf(&b, " — %s", languageName(t.Language))
	}
	if t.Title != "" {
		fmt.Fprintf(&b, " %q", t.Title)
	}
	fmt.Fprintf(&b, " (%s", t.Codec)
	if ch := channelLayout(t.Channels); ch != "" {
		fmt.Fprintf(&b, ", %s", ch)
	}
	b.WriteString(")")
	if t.Default {
		b.WriteString(" (default)")
	}
	return b.String()
}

// channelLayout names the common channel counts ("5.1" reads better than "6ch").
func channelLayout(n int) string {
	switch n {
	case 0:
		return ""
	case 1:
		return "mono"
	case 2:
		return "stereo"
	case 6:
		return "5.1"
	case 8:
		return "7.1"
	default:
		return fmt.Sprintf("%dch", n)
	}
}

// audioTrackLabel is audioLabel for a:N of info, or "" when the file has fewer
// than two tracks (nothing to choose, so nothing worth showing).
func audioTrackLabel(info *probe.MediaInfo, audio int) string {
	if info == nil || len(info.Audio) < 2 || audio < 0 || audio >= len(info.Audio) {
		return ""
	}
	return audioLabel(info.Audio[audio])
}

// buildAudioChoices lists the picker entries, one per track, indexed by a:N.
func buildAudioChoices(info *probe.MediaInfo) []string {
	if info == nil {
		return nil
	}
	out := make([]string, len(info.Audio))
	for i, t := range info.Audio {
		out[i] = audioLabel(t)
	}
	return out
}
