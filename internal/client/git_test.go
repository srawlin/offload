package client

import (
	"os"
	"path/filepath"
	"sort"
	"strings"
	"testing"
)

func TestSnapshotDropsUntrackedSecrets(t *testing.T) {
	dir := t.TempDir()
	run := func(args ...string) string {
		t.Helper()
		out, err := gitOut(dir, snapEnv, args...)
		if err != nil {
			t.Fatalf("git %v: %s", args, out)
		}
		return out
	}
	write := func(name, body string) {
		p := filepath.Join(dir, name)
		os.MkdirAll(filepath.Dir(p), 0o755)
		if err := os.WriteFile(p, []byte(body), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	run("init", "-q")
	write("main.go", "package main\n")
	write("testdata/server.key", "fixture\n") // tracked fixture: must sync
	run("add", ".")
	run("commit", "-q", "-m", "init")
	write(".env", "SECRET=1\n")
	write("sub/.env.local", "SECRET=2\n")
	write("deploy/id_ed25519", "key\n")
	write("new.go", "package main\n") // untracked, not secret: must sync

	r, err := OpenRepo(dir)
	if err != nil {
		t.Fatal(err)
	}
	s, err := r.TakeSnapshot("test")
	if err != nil {
		t.Fatal(err)
	}
	files := strings.Fields(run("ls-tree", "-r", "--name-only", s.Tree))
	sort.Strings(files)
	want := "main.go new.go testdata/server.key"
	if got := strings.Join(files, " "); got != want {
		t.Fatalf("snapshot has %q, want %q", got, want)
	}

	r.Exclude = nil // sync_exclude: [] syncs everything
	s, err = r.TakeSnapshot("test")
	if err != nil {
		t.Fatal(err)
	}
	if out := run("ls-tree", "-r", "--name-only", s.Tree); !strings.Contains(out, ".env") {
		t.Fatalf("with no excludes, .env should sync: %s", out)
	}
}

func init() {
	// Tests must not depend on the developer's git identity or hooks.
	os.Setenv("GIT_CONFIG_GLOBAL", os.DevNull)
}
