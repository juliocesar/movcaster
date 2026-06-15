// Package resume persists per-file playback positions so a file reopened later
// picks up where it stopped. The store lives at ~/.movcaster/playback_index — a
// JSON object keyed by absolute file path. The directory and an empty index are
// created on first construction (i.e. on every movcaster run).
package resume

import (
	"encoding/json"
	"os"
	"path/filepath"
	"sync"
	"time"
)

const (
	dirName  = ".movcaster"
	fileName = "playback_index"
)

// entry is one file's saved position.
type entry struct {
	PositionSeconds float64 `json:"position_seconds"`
	UpdatedAt       string  `json:"updated_at"`
}

// Store reads and writes the playback index. Each call loads and rewrites the
// whole (tiny) file, so unrelated entries survive concurrent movcaster runs.
type Store struct {
	path string
	mu   sync.Mutex
}

// New ensures ~/.movcaster and an (at least empty) playback_index exist, then
// returns a Store for it.
func New() (*Store, error) {
	home, err := os.UserHomeDir()
	if err != nil {
		return nil, err
	}
	dir := filepath.Join(home, dirName)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return nil, err
	}
	s := &Store{path: filepath.Join(dir, fileName)}
	if _, err := os.Stat(s.path); os.IsNotExist(err) {
		if err := os.WriteFile(s.path, []byte("{}\n"), 0o644); err != nil {
			return nil, err
		}
	}
	return s, nil
}

func (s *Store) load() map[string]entry {
	m := map[string]entry{}
	b, err := os.ReadFile(s.path)
	if err != nil {
		return m
	}
	_ = json.Unmarshal(b, &m)
	return m
}

func (s *Store) save(m map[string]entry) error {
	b, err := json.MarshalIndent(m, "", "  ")
	if err != nil {
		return err
	}
	return os.WriteFile(s.path, append(b, '\n'), 0o644)
}

// Get returns the saved position for absPath, or 0 if there is none.
func (s *Store) Get(absPath string) time.Duration {
	s.mu.Lock()
	defer s.mu.Unlock()
	e, ok := s.load()[absPath]
	if !ok {
		return 0
	}
	return time.Duration(e.PositionSeconds * float64(time.Second))
}

// Set records pos for absPath.
func (s *Store) Set(absPath string, pos time.Duration) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	m := s.load()
	m[absPath] = entry{
		PositionSeconds: pos.Seconds(),
		UpdatedAt:       time.Now().UTC().Format(time.RFC3339),
	}
	return s.save(m)
}

// Clear removes any record for absPath (e.g. once it has been watched to the end).
func (s *Store) Clear(absPath string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	m := s.load()
	if _, ok := m[absPath]; !ok {
		return nil
	}
	delete(m, absPath)
	return s.save(m)
}
