package client

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
)

// Repo is the local worktree an agent is working in.
type Repo struct {
	Root      string // worktree top level
	GitDir    string // per-worktree git dir
	CommonDir string // shared git dir (same as GitDir unless using git worktrees)
	ID        string // stable repo id shared by all worktrees and clones
	Worktree  string // id of this worktree (one per agent)
}

func OpenRepo(dir string) (*Repo, error) {
	out, err := gitOut(dir, nil, "rev-parse", "--path-format=absolute", "--show-toplevel", "--git-dir", "--git-common-dir")
	if err != nil {
		return nil, fmt.Errorf("not a git repository: %s", strings.TrimSpace(out))
	}
	f := strings.Split(strings.TrimSpace(out), "\n")
	if len(f) != 3 {
		return nil, fmt.Errorf("unexpected rev-parse output: %q", out)
	}
	r := &Repo{Root: f[0], GitDir: f[1], CommonDir: f[2]}
	host, _ := os.Hostname()
	r.Worktree = shortHash(host + ":" + r.Root)
	r.ID, err = r.repoID()
	return r, err
}

// repoID is the repo's first root commit, cached; clones and worktrees of the
// same project share it, so they share the node's object store and caches.
func (r *Repo) repoID() (string, error) {
	cache := filepath.Join(r.CommonDir, "offload", "repo-id")
	if b, err := os.ReadFile(cache); err == nil && len(b) > 0 {
		return strings.TrimSpace(string(b)), nil
	}
	id := ""
	if out, err := gitOut(r.Root, nil, "rev-list", "--max-parents=0", "HEAD"); err == nil {
		roots := strings.Fields(out)
		if len(roots) > 0 {
			id = roots[len(roots)-1][:16]
		}
	}
	if id == "" {
		id = "path-" + shortHash(r.Root)
	}
	os.MkdirAll(filepath.Dir(cache), 0o755)
	os.WriteFile(cache, []byte(id+"\n"), 0o644)
	return id, nil
}

// Snapshot is the worktree's full content (tracked + untracked, minus
// ignored files) as a git commit.
type Snapshot struct {
	Commit string
	Tree   string
	Base   string // previous snapshot the node is believed to have ("" = none)
	repo   *Repo
	node   string
}

type syncState struct {
	Last     string `json:"last"`
	LastTree string `json:"last_tree"`
}

func (r *Repo) statePath(node string) string {
	return filepath.Join(r.CommonDir, "offload", "state-"+node+".json")
}

// fixed identity and dates make snapshot commits deterministic.
var snapEnv = []string{
	"GIT_AUTHOR_NAME=offload", "GIT_AUTHOR_EMAIL=offload@localhost", "GIT_AUTHOR_DATE=2000-01-01T00:00:00Z",
	"GIT_COMMITTER_NAME=offload", "GIT_COMMITTER_EMAIL=offload@localhost", "GIT_COMMITTER_DATE=2000-01-01T00:00:00Z",
}

// TakeSnapshot writes the worktree into a private index (so the agent's own
// index is untouched) and makes a commit whose parent is the last snapshot
// sent to this node. The bundle then carries only the changed objects.
func (r *Repo) TakeSnapshot(node string) (*Snapshot, error) {
	idx := filepath.Join(r.GitDir, "offload-index")
	if _, err := os.Stat(idx); err != nil {
		// Seed from the real index so its stat cache avoids rehashing every file.
		copyFile(filepath.Join(r.GitDir, "index"), idx)
	}
	env := append([]string{"GIT_INDEX_FILE=" + idx}, snapEnv...)
	if out, err := gitOut(r.Root, env, "add", "-A", "--ignore-errors", "."); err != nil {
		return nil, fmt.Errorf("snapshot: git add: %s", out)
	}
	tree, err := gitOut(r.Root, env, "write-tree")
	if err != nil {
		return nil, fmt.Errorf("snapshot: write-tree: %s", tree)
	}
	tree = strings.TrimSpace(tree)

	var st syncState
	if b, err := os.ReadFile(r.statePath(node)); err == nil {
		json.Unmarshal(b, &st)
	}
	if st.Last != "" && !hasCommit(r.Root, st.Last) {
		st = syncState{}
	}
	s := &Snapshot{Tree: tree, repo: r, node: node}
	if st.LastTree == tree {
		s.Commit = st.Last
		return s, nil
	}
	args := []string{"commit-tree", tree, "-m", "offload snapshot"}
	if st.Last != "" {
		args = append(args, "-p", st.Last)
		s.Base = st.Last
	}
	c, err := gitOut(r.Root, snapEnv, args...)
	if err != nil {
		return nil, fmt.Errorf("snapshot: commit-tree: %s", c)
	}
	s.Commit = strings.TrimSpace(c)
	return s, nil
}

// Full returns a parentless snapshot of the same tree, for when the node has
// lost the base.
func (s *Snapshot) Full() (*Snapshot, error) {
	c, err := gitOut(s.repo.Root, snapEnv, "commit-tree", s.Tree, "-m", "offload snapshot")
	if err != nil {
		return nil, fmt.Errorf("snapshot: commit-tree: %s", c)
	}
	return &Snapshot{Commit: strings.TrimSpace(c), Tree: s.Tree, repo: s.repo, node: s.node}, nil
}

// Bundle returns the git bundle for this snapshot relative to Base, or nil
// when the node already has it (Base == "" and Commit == last sent).
func (s *Snapshot) Bundle(force bool) ([]byte, error) {
	var st syncState
	if b, err := os.ReadFile(s.repo.statePath(s.node)); err == nil {
		json.Unmarshal(b, &st)
	}
	if !force && s.Commit == st.Last {
		return nil, nil
	}
	ref := "refs/offload/snap/" + s.Commit
	if out, err := gitOut(s.repo.Root, nil, "update-ref", ref, s.Commit); err != nil {
		return nil, fmt.Errorf("bundle: %s", out)
	}
	defer gitOut(s.repo.Root, nil, "update-ref", "-d", ref)
	f, err := os.CreateTemp("", "offload-*.bundle")
	if err != nil {
		return nil, err
	}
	f.Close()
	defer os.Remove(f.Name())
	args := []string{"bundle", "create", "-q", f.Name(), ref}
	if s.Base != "" {
		args = append(args, "^"+s.Base)
	}
	if out, err := gitOut(s.repo.Root, nil, args...); err != nil {
		return nil, fmt.Errorf("bundle: %s", out)
	}
	return os.ReadFile(f.Name())
}

// Synced records that the node now has this snapshot.
func (s *Snapshot) Synced() {
	gitOut(s.repo.Root, nil, "update-ref", "refs/offload/last-"+s.node, s.Commit)
	b, _ := json.Marshal(syncState{Last: s.Commit, LastTree: s.Tree})
	p := s.repo.statePath(s.node)
	tmp := p + fmt.Sprintf(".%d", os.Getpid())
	if os.WriteFile(tmp, b, 0o644) == nil {
		os.Rename(tmp, p)
	}
}

func gitOut(dir string, env []string, args ...string) (string, error) {
	cmd := exec.Command("git", args...)
	cmd.Dir = dir
	cmd.Env = append(os.Environ(), env...)
	var b bytes.Buffer
	cmd.Stdout, cmd.Stderr = &b, &b
	err := cmd.Run()
	return b.String(), err
}

func hasCommit(dir, sha string) bool {
	return exec.Command("git", "-C", dir, "cat-file", "-e", sha+"^{commit}").Run() == nil
}

func shortHash(s string) string {
	h := sha256.Sum256([]byte(s))
	return hex.EncodeToString(h[:8])
}

func copyFile(src, dst string) error {
	in, err := os.Open(src)
	if err != nil {
		return err
	}
	defer in.Close()
	out, err := os.Create(dst)
	if err != nil {
		return err
	}
	defer out.Close()
	_, err = io.Copy(out, in)
	return err
}
