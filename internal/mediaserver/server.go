// Package mediaserver serves the local media file (and optional subtitle) over
// HTTP for the TV to pull. Direct-play uses http.ServeContent for native byte-range
// seeking; transcode (added later) streams an ffmpeg pipe.
package mediaserver

import (
	"context"
	"fmt"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"
)

// verbose enables request logging when MOVCASTER_VERBOSE is set.
var verbose = os.Getenv("MOVCASTER_VERBOSE") != ""

// Source is what the server currently exposes at /media.
type Source struct {
	FilePath  string // local path for direct-play
	MIME      string
	Seekable  bool
	Transcode *TranscodeSpec // non-nil => stream via ffmpeg instead of the file
}

// Server is the local HTTP media server.
type Server struct {
	ln       net.Listener
	srv      *http.Server
	localIP  string
	port     int
	mediaExt string

	mu    sync.RWMutex
	src   Source
	subs  *subtitle
	token string // cache-buster, bumped on transcode (re)launch
}

type subtitle struct {
	path string
	mime string
}

// New binds a TCP port on the LAN interface that can reach deviceHost.
func New(deviceHost string) (*Server, error) {
	ip, err := localIPFor(deviceHost)
	if err != nil {
		return nil, fmt.Errorf("determine local IP: %w", err)
	}
	ln, err := net.Listen("tcp", ip+":0")
	if err != nil {
		return nil, fmt.Errorf("bind media server: %w", err)
	}
	addr := ln.Addr().(*net.TCPAddr)
	s := &Server{ln: ln, localIP: ip, port: addr.Port, token: "0"}

	// Route by prefix: media URLs carry the file extension (e.g. /media.mp4)
	// and a cache-busting query, so exact-match patterns won't do.
	mux := http.NewServeMux()
	mux.HandleFunc("/", func(w http.ResponseWriter, r *http.Request) {
		if verbose {
			fmt.Fprintf(os.Stderr, "[media] %s %s %s\n", r.RemoteAddr, r.Method, r.URL.RequestURI())
		}
		switch {
		case strings.HasPrefix(r.URL.Path, "/media"):
			s.handleMedia(w, r)
		case strings.HasPrefix(r.URL.Path, "/subs"):
			s.handleSubs(w, r)
		default:
			http.NotFound(w, r)
		}
	})
	s.srv = &http.Server{Handler: mux}
	return s, nil
}

// Start serves in the background until Shutdown.
func (s *Server) Start() {
	go func() { _ = s.srv.Serve(s.ln) }()
}

// Shutdown stops the server and any running transcode.
func (s *Server) Shutdown(ctx context.Context) error {
	s.mu.Lock()
	if s.src.Transcode != nil {
		s.src.Transcode.stop()
	}
	s.mu.Unlock()
	return s.srv.Shutdown(ctx)
}

// SetDirectPlay exposes a local file for byte-range direct play.
func (s *Server) SetDirectPlay(filePath, mime string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.src = Source{FilePath: filePath, MIME: mime, Seekable: true}
	s.mediaExt = strings.ToLower(filepath.Ext(filePath))
	s.bumpTokenLocked()
}

// SetTranscode switches /media to an on-the-fly ffmpeg transcode (e.g. burned-in
// subs or codec fallback). The output is a non-seekable fragmented MP4 stream.
func (s *Server) SetTranscode(args []string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.src.Transcode != nil {
		s.src.Transcode.stop()
	}
	s.src = Source{MIME: "video/mp4", Seekable: false, Transcode: &TranscodeSpec{Args: args}}
	s.mediaExt = ".mp4"
	s.bumpTokenLocked()
}

// SetSubtitle exposes a local subtitle file (already in a TV-friendly text format).
func (s *Server) SetSubtitle(path, mime string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.subs = &subtitle{path: path, mime: mime}
}

func (s *Server) bumpTokenLocked() {
	// monotonic token without time/rand: parse+increment
	var n int
	fmt.Sscanf(s.token, "%d", &n)
	s.token = fmt.Sprintf("%d", n+1)
}

// baseURL is http://ip:port.
func (s *Server) baseURL() string {
	return fmt.Sprintf("http://%s:%d", s.localIP, s.port)
}

// MediaURL is the URL handed to the TV. Includes a cache-busting token and the
// file extension (some renderers content-type by URL).
func (s *Server) MediaURL() string {
	s.mu.RLock()
	defer s.mu.RUnlock()
	ext := s.mediaExt
	if s.src.Transcode != nil {
		ext = ".mp4"
	}
	return fmt.Sprintf("%s/media%s?t=%s", s.baseURL(), ext, s.token)
}

// SubURL is the subtitle URL, or "" if none.
func (s *Server) SubURL() string {
	s.mu.RLock()
	defer s.mu.RUnlock()
	if s.subs == nil {
		return ""
	}
	ext := strings.ToLower(filepath.Ext(s.subs.path))
	return fmt.Sprintf("%s/subs%s", s.baseURL(), ext)
}

func setDLNAHeaders(w http.ResponseWriter, mime string, seekable bool) {
	op := "00"
	flags := "01700000000000000000000000000000"
	if seekable {
		op = "01"
		flags = "01500000000000000000000000000000"
	}
	w.Header().Set("transferMode.dlna.org", "Streaming")
	w.Header().Set("realTimeInfo.dlna.org", "DLNA.ORG_TLAG=*")
	w.Header().Set("contentFeatures.dlna.org",
		fmt.Sprintf("DLNA.ORG_OP=%s;DLNA.ORG_CI=0;DLNA.ORG_FLAGS=%s", op, flags))
	if mime != "" {
		w.Header().Set("Content-Type", mime)
	}
}

func (s *Server) handleSubs(w http.ResponseWriter, r *http.Request) {
	s.mu.RLock()
	sub := s.subs
	s.mu.RUnlock()
	if sub == nil {
		http.NotFound(w, r)
		return
	}
	w.Header().Set("Access-Control-Allow-Origin", "*")
	w.Header().Set("Access-Control-Allow-Methods", "GET, HEAD, OPTIONS")
	if r.Method == http.MethodOptions {
		w.WriteHeader(http.StatusOK)
		return
	}
	if sub.mime != "" {
		w.Header().Set("Content-Type", sub.mime)
	}
	w.Header().Set("CaptionInfo.sec", s.SubURL())
	http.ServeFile(w, r, sub.path)
}

// localIPFor returns the local IP that routes to deviceHost (host or host:port).
func localIPFor(deviceHost string) (string, error) {
	host := deviceHost
	if h, _, err := net.SplitHostPort(deviceHost); err == nil {
		host = h
	}
	conn, err := net.DialTimeout("udp", net.JoinHostPort(host, "1900"), 2*time.Second)
	if err != nil {
		return "", err
	}
	defer conn.Close()
	return conn.LocalAddr().(*net.UDPAddr).IP.String(), nil
}
