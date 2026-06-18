package resume

import (
	"os"
	"reflect"
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

// writeIndex writes a raw playback_index so a test can control UpdatedAt values
// (Set stamps time.Now()).
func writeIndex(t *testing.T, s *Store, json string) {
	t.Helper()
	if err := os.WriteFile(s.path, []byte(json), 0o644); err != nil {
		t.Fatal(err)
	}
}

func TestRecentNewestFirst(t *testing.T) {
	s := newTestStore(t)
	writeIndex(t, s, `{
	  "/old.mkv":  {"position_seconds": 10, "updated_at": "2026-06-10T00:00:00Z"},
	  "/new.mkv":  {"position_seconds": 20, "updated_at": "2026-06-18T00:00:00Z"},
	  "/mid.mkv":  {"position_seconds": 30, "updated_at": "2026-06-14T00:00:00Z"}
	}`)
	got := s.Recent()
	want := []string{"/new.mkv", "/mid.mkv", "/old.mkv"}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("Recent() = %v, want %v", got, want)
	}
}

func TestRecentEmpty(t *testing.T) {
	s := newTestStore(t)
	writeIndex(t, s, "{}\n")
	if got := s.Recent(); len(got) != 0 {
		t.Fatalf("Recent() on empty index = %v, want empty", got)
	}
}

func TestRecentUnparseableSortsLast(t *testing.T) {
	s := newTestStore(t)
	writeIndex(t, s, `{
	  "/good.mkv": {"position_seconds": 10, "updated_at": "2026-06-10T00:00:00Z"},
	  "/bad.mkv":  {"position_seconds": 20, "updated_at": "not-a-time"}
	}`)
	got := s.Recent()
	want := []string{"/good.mkv", "/bad.mkv"}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("Recent() = %v, want %v (unparseable should sort last)", got, want)
	}
}
