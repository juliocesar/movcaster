package resume

import (
	"testing"
	"time"
)

// newTestStore points a Store at a temp file so tests never touch ~/.movcaster.
func newTestStore(t *testing.T) *Store {
	t.Helper()
	return &Store{path: t.TempDir() + "/playback_index"}
}

func TestSetGetRoundTrip(t *testing.T) {
	s := newTestStore(t)
	if got := s.Get("/x/movie.mkv"); got != 0 {
		t.Fatalf("empty store should return 0, got %v", got)
	}
	if err := s.Set("/x/movie.mkv", 42*time.Second); err != nil {
		t.Fatal(err)
	}
	if got := s.Get("/x/movie.mkv"); got != 42*time.Second {
		t.Fatalf("got %v, want 42s", got)
	}
}

func TestEntriesAreIndependent(t *testing.T) {
	s := newTestStore(t)
	_ = s.Set("/a.mkv", 10*time.Second)
	_ = s.Set("/b.mkv", 20*time.Second)
	if err := s.Clear("/a.mkv"); err != nil {
		t.Fatal(err)
	}
	if s.Get("/a.mkv") != 0 {
		t.Fatal("cleared entry should be gone")
	}
	if s.Get("/b.mkv") != 20*time.Second {
		t.Fatal("unrelated entry should survive a Clear")
	}
}

func TestClearMissingIsNoError(t *testing.T) {
	s := newTestStore(t)
	if err := s.Clear("/nope.mkv"); err != nil {
		t.Fatalf("clearing a missing key should be a no-op, got %v", err)
	}
}
