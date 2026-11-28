package node

import (
	"context"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"github.com/srawlin/offload/internal/client"
)

func TestSetupSteps(t *testing.T) {
	for _, tc := range []struct {
		files []string
		want  string
	}{
		{nil, ""},
		{[]string{"package-lock.json"}, "npm ci"},
		{[]string{"package-lock.json", "pnpm-lock.yaml"}, "pnpm install --frozen-lockfile"},
		{[]string{"yarn.lock"}, "yarn install --frozen-lockfile"},
		{[]string{"package-lock.json", "uv.lock"}, "npm ci; uv sync --frozen"},
	} {
		dir := t.TempDir()
		for _, f := range tc.files {
			os.WriteFile(filepath.Join(dir, f), nil, 0o644)
		}
		var got []string
		for _, s := range setupSteps(dir) {
			got = append(got, strings.Join(s.argv, " "))
		}
		if g := strings.Join(got, "; "); g != tc.want {
			t.Errorf("%v: got %q, want %q", tc.files, g, tc.want)
		}
	}
}

// fakeNpm puts an npm on PATH that logs each call to the file it returns and
// fails while $NPM_FAIL is set.
func fakeNpm(t *testing.T) string {
	bin := t.TempDir()
	calls := filepath.Join(bin, "calls")
	script := "#!/bin/sh\necho \"$*\" >> " + calls + "\n[ -z \"$NPM_FAIL\" ] || { echo 'npm ERR! boom'; exit 1; }\n"
	if err := os.WriteFile(filepath.Join(bin, "npm"), []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", bin+string(os.PathListSeparator)+os.Getenv("PATH"))
	return calls
}

func countLines(path string) int {
	b, _ := os.ReadFile(path)
	return strings.Count(string(b), "\n")
}

func TestSetupRunsWhenLockfileChanges(t *testing.T) {
	calls := fakeNpm(t)
	s := &Server{Root: t.TempDir()}
	ws := t.TempDir()
	os.MkdirAll(filepath.Join(ws, ".git"), 0o755)
	os.MkdirAll(filepath.Join(ws, "pkg"), 0o755)
	lock := filepath.Join(ws, "package-lock.json")
	ctx := context.Background()
	var shown strings.Builder
	out := func(d string) { shown.WriteString(d) }
	run := func(dir string, env map[string]string) (bool, error) {
		return s.setup(ctx, ws, filepath.Join(ws, dir), env, out)
	}

	os.WriteFile(lock, []byte("v1"), 0o644)
	if ran, err := run("", nil); !ran || err != nil || countLines(calls) != 1 {
		t.Fatalf("first run: ran=%v err=%v calls=%d", ran, err, countLines(calls))
	}
	// Unchanged lockfile, including from a subdirectory: nothing to do.
	if ran, err := run("pkg", nil); ran || err != nil || countLines(calls) != 1 {
		t.Fatalf("unchanged: ran=%v err=%v calls=%d", ran, err, countLines(calls))
	}
	if shown.Len() != 0 {
		t.Fatalf("output shown on success: %q", shown.String())
	}

	// A failed install shows its output and is reported as an error.
	os.WriteFile(lock, []byte("v2"), 0o644)
	_, err := run("", map[string]string{"NPM_FAIL": "1"})
	if err == nil || !strings.Contains(err.Error(), "npm ci") || !strings.Contains(shown.String(), "npm ERR! boom") {
		t.Fatalf("failed install: err=%v output=%q", err, shown.String())
	}
	// Back on the old lockfile the install runs again: the failed one may
	// have left node_modules half-written.
	os.WriteFile(lock, []byte("v1"), 0o644)
	if ran, err := run("", nil); !ran || err != nil || countLines(calls) != 3 {
		t.Fatalf("after a failure: ran=%v err=%v calls=%d", ran, err, countLines(calls))
	}
}

// A failed install is a node error, so auto mode runs the command locally.
func TestSetupFailureFallsBackToLocal(t *testing.T) {
	calls := fakeNpm(t)
	t.Setenv("GIT_CONFIG_GLOBAL", os.DevNull)
	s, err := New(t.TempDir(), "tok")
	if err != nil {
		t.Fatal(err)
	}
	srv := httptest.NewServer(s.Handler())
	defer srv.Close()

	repo := t.TempDir()
	if out, err := exec.Command("git", "init", "-q", repo).CombinedOutput(); err != nil {
		t.Fatalf("git init: %s", out)
	}
	os.WriteFile(filepath.Join(repo, "package-lock.json"), []byte("{}"), 0o644)
	cfg := client.Config{Node: srv.URL, Token: "tok", Mode: "auto"}
	run := func(env map[string]string) client.Result {
		return client.Run(context.Background(), cfg, client.Options{Argv: []string{"true"}, Dir: repo, Env: env})
	}

	res := run(map[string]string{"NPM_FAIL": "1"})
	if res.Mode != "local" || res.ExitCode != 0 || !strings.Contains(res.Fallback, "npm ci") {
		t.Fatalf("failed install: mode=%s exit=%d fallback=%q", res.Mode, res.ExitCode, res.Fallback)
	}
	res = run(nil)
	if res.Mode != "remote" || res.ExitCode != 0 || countLines(calls) != 2 {
		t.Fatalf("working install: mode=%s exit=%d fallback=%q calls=%d", res.Mode, res.ExitCode, res.Fallback, countLines(calls))
	}
	if _, ok := res.Phases["node_setup"]; !ok {
		t.Fatalf("no setup phase reported: %v", res.Phases)
	}
}
