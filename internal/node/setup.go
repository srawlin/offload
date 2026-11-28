package node

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"strings"
)

// setupFile records, per lockfile, its hash at the last successful install in
// the workspace. It lives under .git, so checkouts and git clean leave it be,
// and a cloned workspace carries it along with the dependencies it describes.
const setupFile = ".git/offload-setup.json"

type setupStep struct {
	lockfile string
	argv     []string
}

// setupSteps lists the install steps for the lockfiles in dir, picked the way
// offload test picks the test command: one JavaScript package manager (pnpm,
// then yarn, then npm) and uv.
func setupSteps(dir string) []setupStep {
	exists := func(f string) bool { _, err := os.Stat(filepath.Join(dir, f)); return err == nil }
	var steps []setupStep
	switch {
	case exists("pnpm-lock.yaml"):
		steps = append(steps, setupStep{"pnpm-lock.yaml", []string{"pnpm", "install", "--frozen-lockfile"}})
	case exists("yarn.lock"):
		steps = append(steps, setupStep{"yarn.lock", []string{"yarn", "install", "--frozen-lockfile"}})
	case exists("package-lock.json"):
		steps = append(steps, setupStep{"package-lock.json", []string{"npm", "ci"}})
	}
	if exists("uv.lock") {
		steps = append(steps, setupStep{"uv.lock", []string{"uv", "sync", "--frozen"}})
	}
	return steps
}

// setup installs dependencies before a run: for each lockfile from the
// workspace root down to dir whose hash changed since the last successful
// install, it runs the install step there. It's silent on success; when a
// step fails it passes the step's output to out and returns an error, which
// the client treats as a node failure. ran reports whether any step ran.
func (s *Server) setup(ctx context.Context, wsPath, dir string, env map[string]string, out func(string)) (ran bool, err error) {
	rec := filepath.Join(wsPath, setupFile)
	done := map[string]string{}
	if b, err := os.ReadFile(rec); err == nil {
		json.Unmarshal(b, &done)
	}
	save := func() error {
		b, _ := json.Marshal(done)
		return os.WriteFile(rec, b, 0o644)
	}
	for _, d := range dirsDown(wsPath, dir) {
		for _, step := range setupSteps(d) {
			lock := filepath.Join(d, step.lockfile)
			b, err := os.ReadFile(lock)
			if err != nil {
				return ran, fmt.Errorf("setup: %v", err)
			}
			sum := sha256.Sum256(b)
			hash := hex.EncodeToString(sum[:])
			rel, _ := filepath.Rel(wsPath, lock)
			if done[rel] == hash {
				continue
			}
			if _, ok := done[rel]; ok {
				delete(done, rel) // a failed or interrupted install must run again
				if err := save(); err != nil {
					return ran, fmt.Errorf("setup: %v", err)
				}
			}
			var buf strings.Builder
			if code := s.exec(ctx, wsPath, d, step.argv, env, func(o string) { buf.WriteString(o) }); code != 0 {
				out(buf.String())
				return ran, fmt.Errorf("setup: %s for %s exited with %d", strings.Join(step.argv, " "), rel, code)
			}
			ran = true
			done[rel] = hash
			if err := save(); err != nil {
				return ran, fmt.Errorf("setup: %v", err)
			}
		}
	}
	return ran, nil
}

// dirsDown lists root and each directory below it down to dir.
func dirsDown(root, dir string) []string {
	dirs := []string{dir}
	for d := dir; d != root && len(d) > len(root); {
		d = filepath.Dir(d)
		dirs = append(dirs, d)
	}
	slices.Reverse(dirs)
	return dirs
}
