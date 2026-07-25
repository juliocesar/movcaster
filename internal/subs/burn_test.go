package subs

import (
	"strings"
	"testing"
	"time"

	"github.com/juliocesar/movcaster/internal/probe"
)

func joined(args []string) string { return strings.Join(args, " ") }

// The subtitles filter reads absolute cue times through its own demuxer, so it
// ignores -ss: without -copyts the frames are rebased to 0, every cue is in the
// past, and the burn renders nothing at all (verified against the TV's source
// file: identical output with and without the filter). setpts/asetpts then rebase
// both streams for the fragmented-MP4-from-0 stream the TV expects.
func TestBurnArgsTextSeekKeepsSubtitlesAligned(t *testing.T) {
	track := probe.SubTrack{SubIndex: 0, Codec: "subrip", Kind: probe.SubText}
	got := joined(BurnArgs("in.mkv", track, 90*time.Second))

	for _, want := range []string{"-ss 90.000", "-copyts", "setpts=PTS-STARTPTS", "-af asetpts=PTS-STARTPTS"} {
		if !strings.Contains(got, want) {
			t.Errorf("text burn at ss>0 missing %q: %s", want, got)
		}
	}
	// -copyts is an input option: it must precede -i or ffmpeg ignores it.
	if strings.Index(got, "-copyts") > strings.Index(got, "-i in.mkv") {
		t.Errorf("-copyts must come before -i: %s", got)
	}
}

// At ss=0 there is nothing to rebase, so the known-good unseeked path stays
// byte-for-byte as it was.
func TestBurnArgsNoSeekUnchanged(t *testing.T) {
	track := probe.SubTrack{SubIndex: 0, Codec: "subrip", Kind: probe.SubText}
	got := joined(BurnArgs("in.mkv", track, 0))
	for _, unwanted := range []string{"-copyts", "setpts", "-ss"} {
		if strings.Contains(got, unwanted) {
			t.Errorf("ss=0 must not add %q: %s", unwanted, got)
		}
	}
	if !strings.Contains(got, "subtitles='in.mkv':si=0") {
		t.Errorf("missing subtitles filter: %s", got)
	}
}

// Bitmap subs come from the demuxer via overlay, which *does* honor -ss, so the
// timestamp dance must not be applied there (it would only risk the path that
// already works).
func TestBurnArgsBitmapSeekUntouched(t *testing.T) {
	track := probe.SubTrack{SubIndex: 2, Codec: "hdmv_pgs_subtitle", Kind: probe.SubBitmap}
	got := joined(BurnArgs("in.mkv", track, 90*time.Second))
	if !strings.Contains(got, "-ss 90.000") {
		t.Errorf("bitmap burn should still input-seek: %s", got)
	}
	for _, unwanted := range []string{"-copyts", "setpts"} {
		if strings.Contains(got, unwanted) {
			t.Errorf("bitmap burn must not add %q: %s", unwanted, got)
		}
	}
	if !strings.Contains(got, "[0:v][0:s:2]overlay[vout]") {
		t.Errorf("missing overlay filter: %s", got)
	}
}
