package mediaserver

import (
	"io"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// startServer binds a real server on the loopback interface, pointing at a small
// direct-play file and a subtitle.
func startServer(t *testing.T) (*Server, string, string) {
	t.Helper()
	dir := t.TempDir()
	media := filepath.Join(dir, "movie.mp4")
	if err := os.WriteFile(media, []byte("0123456789abcdef"), 0o644); err != nil {
		t.Fatal(err)
	}
	sub := filepath.Join(dir, "subs.vtt")
	if err := os.WriteFile(sub, []byte("WEBVTT\n\n00:00.000 --> 00:02.000\nhi\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	s, err := New("127.0.0.1")
	if err != nil {
		t.Fatal(err)
	}
	s.Start()
	t.Cleanup(func() { _ = s.Shutdown(t.Context()) })
	s.SetDirectPlay(media, "video/mp4")
	s.SetSubtitle(sub, "text/vtt")
	return s, media, sub
}

// A Cast receiver needs CORS on the media as well as the track, including a
// preflight answer with Content-Type/Accept-Encoding/Range allowed, or a LOAD
// carrying tracks fails opaquely.
func TestMediaCORS(t *testing.T) {
	s, _, _ := startServer(t)

	req, _ := http.NewRequest(http.MethodOptions, s.MediaURL(), nil)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("OPTIONS /media = %d", resp.StatusCode)
	}
	if got := resp.Header.Get("Access-Control-Allow-Origin"); got != "*" {
		t.Fatalf("Allow-Origin = %q", got)
	}
	allow := resp.Header.Get("Access-Control-Allow-Headers")
	for _, h := range []string{"Content-Type", "Accept-Encoding", "Range"} {
		if !strings.Contains(allow, h) {
			t.Fatalf("Allow-Headers %q missing %s", allow, h)
		}
	}
}

// Ranged reads must keep working (native TV seeking) and carry both the CORS
// and the DLNA headers.
func TestMediaRangeRead(t *testing.T) {
	s, _, _ := startServer(t)

	req, _ := http.NewRequest(http.MethodGet, s.MediaURL(), nil)
	req.Header.Set("Range", "bytes=0-3")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusPartialContent {
		t.Fatalf("ranged GET = %d, want 206", resp.StatusCode)
	}
	b, _ := io.ReadAll(resp.Body)
	if string(b) != "0123" {
		t.Fatalf("range body = %q", b)
	}
	if resp.Header.Get("Access-Control-Allow-Origin") != "*" {
		t.Fatal("media GET missing CORS")
	}
	if !strings.Contains(resp.Header.Get("contentFeatures.dlna.org"), "DLNA.ORG_OP=01") {
		t.Fatalf("direct play must advertise OP=01, got %q", resp.Header.Get("contentFeatures.dlna.org"))
	}
	if resp.Header.Get("Content-Type") != "video/mp4" {
		t.Fatalf("Content-Type = %q", resp.Header.Get("Content-Type"))
	}
}

// The subtitle response carries the format-driven MIME (text/vtt for a WebVTT
// file) and CORS, and answers OPTIONS.
func TestSubsHeaders(t *testing.T) {
	s, _, _ := startServer(t)

	resp, err := http.Get(s.SubURL())
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if resp.Header.Get("Content-Type") != "text/vtt" {
		t.Fatalf("subs Content-Type = %q, want text/vtt", resp.Header.Get("Content-Type"))
	}
	if resp.Header.Get("Access-Control-Allow-Origin") != "*" {
		t.Fatal("subs GET missing CORS")
	}
	if !strings.HasSuffix(strings.SplitN(s.SubURL(), "?", 2)[0], ".vtt") {
		t.Fatalf("SubURL must carry the .vtt extension: %s", s.SubURL())
	}

	req, _ := http.NewRequest(http.MethodOptions, s.SubURL(), nil)
	oresp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	oresp.Body.Close()
	if oresp.StatusCode != http.StatusOK {
		t.Fatalf("OPTIONS /subs = %d", oresp.StatusCode)
	}
}
