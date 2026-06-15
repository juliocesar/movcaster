package mediaserver

import (
	"net/http"
	"os"
)

// handleMedia serves /media. Direct-play uses http.ServeContent (range-capable);
// transcode streams an ffmpeg pipe (not range-capable).
func (s *Server) handleMedia(w http.ResponseWriter, r *http.Request) {
	s.mu.RLock()
	src := s.src
	s.mu.RUnlock()

	if src.Transcode != nil {
		s.serveTranscode(w, r, src.Transcode)
		return
	}

	if src.FilePath == "" {
		http.NotFound(w, r)
		return
	}

	f, err := os.Open(src.FilePath)
	if err != nil {
		http.Error(w, "media unavailable", http.StatusInternalServerError)
		return
	}
	defer f.Close()
	fi, err := f.Stat()
	if err != nil {
		http.Error(w, "media unavailable", http.StatusInternalServerError)
		return
	}

	setDLNAHeaders(w, src.MIME, true)
	// ServeContent honors Range headers via the ReadSeeker -> native TV seeking.
	http.ServeContent(w, r, fi.Name(), fi.ModTime(), f)
}
