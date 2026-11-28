package node

import (
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"runtime"
	"testing"

	"github.com/srawlin/offload/internal/proto"
)

func TestColdWorkspaceIsAvoidedAndReseeded(t *testing.T) {
	if runtime.GOOS != "linux" {
		t.Skip("workspace clones use GNU cp; nodes run on Linux")
	}
	root := t.TempDir()
	s, err := New(root, "tok")
	if err != nil {
		t.Fatal(err)
	}
	rp := s.repo("r")

	// The repo's first workspace starts empty but isn't cold: it's what the
	// first runs warm up.
	warm, err := s.acquire(rp, "a")
	if err != nil {
		t.Fatal(err)
	}
	if warm.cold {
		t.Fatal("first workspace marked cold")
	}
	dep := filepath.Join("node_modules", "dep")
	os.MkdirAll(filepath.Join(warm.path, "node_modules"), 0o755)
	os.WriteFile(filepath.Join(warm.path, dep), []byte("x"), 0o644)

	// A second agent while the first is still running gets an empty one.
	cold, err := s.acquire(rp, "b")
	if err != nil {
		t.Fatal(err)
	}
	if cold == warm || !cold.cold {
		t.Fatalf("concurrent run got %+v, want a new cold workspace", cold)
	}
	s.release(warm)
	s.release(cold) // most recently used, and b's own

	// b's next run still prefers the warm workspace.
	if got, _ := s.acquire(rp, "b"); got != warm {
		t.Fatalf("acquire picked %s, want the warm %s", got.path, warm.path)
	}
	s.release(warm)

	// The flag survives a restart.
	s2, err := New(root, "tok")
	if err != nil {
		t.Fatal(err)
	}
	for _, w := range s2.repo("r").ws {
		if want := w.path == cold.path; w.cold != want {
			t.Fatalf("after restart %s cold=%v, want %v", w.path, w.cold, want)
		}
	}

	// pool re-seeds it from the warm one.
	req := httptest.NewRequest("POST", "/v1/pool?min=2", nil)
	req.Header.Set("Authorization", "Bearer tok")
	req.Header.Set(proto.HeaderRepo, "r")
	rec := httptest.NewRecorder()
	s.Handler().ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("pool: %d %s", rec.Code, rec.Body)
	}
	if len(rp.ws) != 2 || cold.cold || cold.busy || warm.busy {
		t.Fatalf("after pool: %d workspaces, cold=%v busy=%v/%v", len(rp.ws), cold.cold, cold.busy, warm.busy)
	}
	if _, err := os.Stat(filepath.Join(cold.path, dep)); err != nil {
		t.Fatalf("re-seeded workspace lacks the warm one's files: %v", err)
	}
	if _, err := os.Stat(filepath.Join(cold.path, coldMarker)); err == nil {
		t.Fatal("re-seeded workspace still has the cold marker")
	}
	if ents, _ := os.ReadDir(filepath.Join(root, "tmp")); len(ents) != 0 {
		t.Fatalf("tmp not cleaned up: %v", ents)
	}
}
