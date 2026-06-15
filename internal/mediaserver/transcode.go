package mediaserver

import (
	"io"
	"net/http"
	"os"
	"os/exec"
	"sync"
)

// TranscodeSpec is a fully-formed ffmpeg invocation that writes a fragmented MP4
// to stdout (pipe:1). The subtitle/codec strategy builds Args; the server runs it.
// Because a pipe is not byte-seekable, seeking is handled by relaunching ffmpeg
// with a new -ss offset (see the seek-restart logic in the controller).
type TranscodeSpec struct {
	Args []string

	mu  sync.Mutex
	cmd *exec.Cmd
}

// stop kills the running ffmpeg, if any.
func (t *TranscodeSpec) stop() {
	t.mu.Lock()
	defer t.mu.Unlock()
	if t.cmd != nil && t.cmd.Process != nil {
		_ = t.cmd.Process.Kill()
		t.cmd = nil
	}
}

// serveTranscode launches ffmpeg and streams its stdout to the TV. HTTP write
// back-pressure naturally throttles ffmpeg (it blocks when the TV reads slowly).
func (s *Server) serveTranscode(w http.ResponseWriter, r *http.Request, spec *TranscodeSpec) {
	setDLNAHeaders(w, "video/mp4", false)
	w.WriteHeader(http.StatusOK)

	cmd := exec.CommandContext(r.Context(), "ffmpeg", spec.Args...)
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		return
	}
	if verbose {
		cmd.Stderr = os.Stderr
	}

	spec.mu.Lock()
	spec.cmd = cmd
	spec.mu.Unlock()

	if err := cmd.Start(); err != nil {
		return
	}
	defer func() {
		_ = cmd.Process.Kill()
		_ = cmd.Wait()
	}()

	_, _ = io.Copy(w, stdout)
}
