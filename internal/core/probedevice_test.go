package core

import (
	"context"
	"os"
	"os/exec"
	"testing"
)

// The baseline probe clip must generate with a stock ffmpeg; if this fails, the
// whole prober is dead on arrival.
func TestGenerateBaselineClip(t *testing.T) {
	if _, err := exec.LookPath("ffmpeg"); err != nil {
		t.Skip("ffmpeg not installed")
	}
	path, err := generateClip(context.Background(), t.TempDir(), probeClip{Container: "mp4", Video: "h264", Audio: "aac"})
	if err != nil {
		t.Fatal(err)
	}
	fi, err := os.Stat(path)
	if err != nil || fi.Size() == 0 {
		t.Fatalf("clip missing or empty: %v", err)
	}
}

// A clip whose encoder is missing reports a skip, not a hard failure.
func TestGenerateClipMissingEncoderSkips(t *testing.T) {
	if _, err := exec.LookPath("ffmpeg"); err != nil {
		t.Skip("ffmpeg not installed")
	}
	_, err := generateClip(context.Background(), t.TempDir(), probeClip{Container: "mp4", Video: "h264", Audio: "aac", vEnc: "no_such_encoder"})
	if err == nil {
		t.Fatal("expected an error for a missing encoder")
	}
}
