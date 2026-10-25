// Package node is the remote side of Offload: it receives worktree snapshots
// as git bundles, keeps a pool of warm per-repo workspaces, and streams
// command output back.
//
// The node runs commands directly as its own user, with no isolation between
// runs. For isolation, run the node inside a sandbox (E2B, microsandbox, a VM).
//
// Layout under Root:
//
//	repos/<repo>.git          bare object store per repo (all snapshots)
//	ws/<repo>/<n>/            warm workspaces; each is a git repo whose
//	                          objects come from the store via alternates
//	tls.crt, tls.key, token   generated on first start
package node

import (
	"bufio"
	"context"
	"crypto/rand"
	"crypto/subtle"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"log"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"time"

	"offload/internal/proto"
)

type Server struct {
	Root  string
	Token string

	mu    sync.Mutex
	repos map[string]*repo
}

type repo struct {
	id      string
	store   string
	fetchMu sync.Mutex // serialises bundle ingest into the store
	ws      []*workspace
}

type workspace struct {
	path     string
	busy     bool
	worktree string // last local worktree that used it (affinity)
	lastUsed time.Time
	head     string // snapshot currently checked out
}

var safeID = regexp.MustCompile(`^[A-Za-z0-9._-]{1,128}$`)
var shaRe = regexp.MustCompile(`^[0-9a-f]{40}$`)

// New opens (or creates) the node state under root. An empty token means the
// one stored in root/token, generated on first start.
func New(root, token string) (*Server, error) {
	for _, d := range []string{"repos", "ws", "tmp"} {
		if err := os.MkdirAll(filepath.Join(root, d), 0o755); err != nil {
			return nil, err
		}
	}
	if token != "" {
		if err := os.WriteFile(filepath.Join(root, "token"), []byte(token), 0o600); err != nil {
			return nil, err
		}
	}
	tok, err := readOrCreate(filepath.Join(root, "token"), func() []byte {
		b := make([]byte, 24)
		rand.Read(b)
		return []byte(hex.EncodeToString(b))
	})
	if err != nil {
		return nil, err
	}
	s := &Server{Root: root, Token: strings.TrimSpace(string(tok)), repos: map[string]*repo{}}
	s.rescan()
	return s, nil
}

// rescan rebuilds the in-memory pool from disk after a restart.
func (s *Server) rescan() {
	ents, _ := os.ReadDir(filepath.Join(s.Root, "ws"))
	for _, e := range ents {
		r := s.repo(e.Name())
		wss, _ := os.ReadDir(filepath.Join(s.Root, "ws", e.Name()))
		for _, w := range wss {
			p := filepath.Join(s.Root, "ws", e.Name(), w.Name())
			head, _ := git(p, "rev-parse", "HEAD")
			r.ws = append(r.ws, &workspace{path: p, head: strings.TrimSpace(head)})
		}
	}
}

func (s *Server) repo(id string) *repo {
	s.mu.Lock()
	defer s.mu.Unlock()
	if r, ok := s.repos[id]; ok {
		return r
	}
	r := &repo{id: id, store: filepath.Join(s.Root, "repos", id+".git")}
	s.repos[id] = r
	return r
}

func (s *Server) Handler() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("GET /v1/health", func(w http.ResponseWriter, r *http.Request) {
		fmt.Fprintln(w, "ok")
	})
	mux.HandleFunc("POST /v1/run", s.auth(s.handleRun))
	mux.HandleFunc("POST /v1/sync", s.auth(s.handleSync))
	mux.HandleFunc("POST /v1/pool", s.auth(s.handlePool))
	return mux
}

func (s *Server) auth(h http.HandlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		got := []byte(r.Header.Get("Authorization"))
		if subtle.ConstantTimeCompare(got, []byte("Bearer "+s.Token)) != 1 {
			http.Error(w, "unauthorized", http.StatusUnauthorized)
			return
		}
		h(w, r)
	}
}

// ingest makes sure the snapshot is in the repo's store, fetching it from the
// request body (a git bundle) if needed. It returns false after writing a 409
// when the bundle's base is missing and the client must resend in full.
func (s *Server) ingest(w http.ResponseWriter, r *http.Request, rp *repo) bool {
	snap, base := r.Header.Get(proto.HeaderSnapshot), r.Header.Get(proto.HeaderBase)
	if !shaRe.MatchString(snap) || (base != "" && !shaRe.MatchString(base)) {
		http.Error(w, "bad snapshot id", http.StatusBadRequest)
		return false
	}
	rp.fetchMu.Lock()
	defer rp.fetchMu.Unlock()
	if _, err := os.Stat(rp.store); err != nil {
		if _, err := git("", "init", "-q", "--bare", rp.store); err != nil {
			http.Error(w, err.Error(), http.StatusInternalServerError)
			return false
		}
		git(rp.store, "config", "gc.auto", "0")
	}
	if hasObject(rp.store, snap) {
		io.Copy(io.Discard, r.Body)
		return true
	}
	needFull := func() bool {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusConflict)
		json.NewEncoder(w).Encode(proto.NeedFull{Need: "full"})
		return false
	}
	if r.ContentLength == 0 || (base != "" && !hasObject(rp.store, base)) {
		io.Copy(io.Discard, r.Body)
		return needFull()
	}
	f, err := os.CreateTemp(filepath.Join(s.Root, "tmp"), "bundle-*")
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return false
	}
	defer os.Remove(f.Name())
	if _, err := io.Copy(f, r.Body); err != nil {
		f.Close()
		http.Error(w, err.Error(), http.StatusBadRequest)
		return false
	}
	f.Close()
	if out, err := git(rp.store, "fetch", "-q", f.Name(), "refs/offload/snap/*:refs/offload/snap/*"); err != nil {
		http.Error(w, "fetch bundle: "+out, http.StatusBadRequest)
		return false
	}
	return true
}

func (s *Server) handleSync(w http.ResponseWriter, r *http.Request) {
	rp, ok := s.repoFor(w, r)
	if !ok {
		return
	}
	if s.ingest(w, r, rp) {
		fmt.Fprintln(w, "ok")
	}
}

// handlePool pre-creates workspaces so N concurrent agents all start warm.
func (s *Server) handlePool(w http.ResponseWriter, r *http.Request) {
	rp, ok := s.repoFor(w, r)
	if !ok {
		return
	}
	min, _ := strconv.Atoi(r.URL.Query().Get("min"))
	if min > 32 {
		min = 32
	}
	for {
		s.mu.Lock()
		n := len(rp.ws)
		s.mu.Unlock()
		if n >= min {
			break
		}
		ws, err := s.newWorkspace(rp)
		if err != nil {
			http.Error(w, err.Error(), http.StatusInternalServerError)
			return
		}
		s.release(ws)
	}
	fmt.Fprintf(w, "%d workspaces\n", min)
}

func (s *Server) repoFor(w http.ResponseWriter, r *http.Request) (*repo, bool) {
	id := r.Header.Get(proto.HeaderRepo)
	if !safeID.MatchString(id) {
		http.Error(w, "bad repo id", http.StatusBadRequest)
		return nil, false
	}
	return s.repo(id), true
}

// acquire picks a workspace: the one this worktree used last, else the most
// recently used idle one, else a new clone of the warmest idle workspace.
func (s *Server) acquire(rp *repo, worktree string) (*workspace, error) {
	s.mu.Lock()
	var pick *workspace
	for _, w := range rp.ws {
		if !w.busy && w.worktree == worktree {
			pick = w
			break
		}
	}
	if pick == nil {
		for _, w := range rp.ws {
			if !w.busy && (pick == nil || w.lastUsed.After(pick.lastUsed)) {
				pick = w
			}
		}
	}
	if pick != nil {
		pick.busy, pick.worktree = true, worktree
		s.mu.Unlock()
		return pick, nil
	}
	s.mu.Unlock()
	ws, err := s.newWorkspace(rp)
	if err == nil {
		ws.worktree = worktree
	}
	return ws, err
}

// newWorkspace clones the warmest idle workspace (reflink copy on XFS keeps
// build outputs, so the clone is warm) or creates an empty one. The result is
// returned busy.
func (s *Server) newWorkspace(rp *repo) (*workspace, error) {
	s.mu.Lock()
	var src *workspace
	for _, w := range rp.ws {
		if !w.busy && (src == nil || w.lastUsed.After(src.lastUsed)) {
			src = w
		}
	}
	if src != nil {
		src.busy = true // hold it still while copying
	}
	dir := filepath.Join(s.Root, "ws", rp.id)
	os.MkdirAll(dir, 0o755)
	path := filepath.Join(dir, strconv.Itoa(len(rp.ws)))
	ws := &workspace{path: path, busy: true}
	rp.ws = append(rp.ws, ws)
	s.mu.Unlock()

	var err error
	if src != nil {
		var out []byte
		out, err = exec.Command("cp", "-a", "--reflink=auto", src.path, path).CombinedOutput()
		if err != nil {
			err = fmt.Errorf("clone workspace: %v: %s", err, out)
		}
		ws.head = src.head
		s.release(src)
	} else {
		_, err = git("", "init", "-q", path)
		if err == nil {
			err = os.WriteFile(filepath.Join(path, ".git", "objects", "info", "alternates"),
				[]byte(filepath.Join(rp.store, "objects")+"\n"), 0o644)
		}
		git(path, "config", "gc.auto", "0")
	}
	if err != nil {
		s.mu.Lock()
		for i, w := range rp.ws {
			if w == ws {
				rp.ws = append(rp.ws[:i], rp.ws[i+1:]...)
			}
		}
		s.mu.Unlock()
		os.RemoveAll(path)
	}
	return ws, err
}

func (s *Server) release(ws *workspace) {
	s.mu.Lock()
	ws.busy, ws.lastUsed = false, time.Now()
	s.mu.Unlock()
}

// checkout moves a workspace to the snapshot. git only rewrites files that
// differ, so unchanged files keep their mtimes and incremental builds stay
// warm. Ignored files (build outputs, node_modules) are left alone.
func checkout(ws *workspace, snap string) error {
	if ws.head == snap {
		return nil
	}
	if out, err := git(ws.path, "checkout", "-q", "-f", "--detach", snap); err != nil {
		return fmt.Errorf("checkout: %s", out)
	}
	if out, err := git(ws.path, "clean", "-q", "-fd"); err != nil {
		return fmt.Errorf("clean: %s", out)
	}
	ws.head = snap
	return nil
}

type streamer struct {
	mu  sync.Mutex
	enc *json.Encoder
	fl  http.Flusher
}

func (st *streamer) send(e proto.Event) {
	st.mu.Lock()
	defer st.mu.Unlock()
	st.enc.Encode(e)
	if st.fl != nil {
		st.fl.Flush()
	}
}

func (s *Server) handleRun(w http.ResponseWriter, r *http.Request) {
	t0 := time.Now()
	rp, ok := s.repoFor(w, r)
	if !ok {
		return
	}
	var argv []string
	if err := json.Unmarshal([]byte(r.Header.Get(proto.HeaderCmd)), &argv); err != nil || len(argv) == 0 {
		http.Error(w, "bad command", http.StatusBadRequest)
		return
	}
	env := map[string]string{}
	if e := r.Header.Get(proto.HeaderEnv); e != "" {
		json.Unmarshal([]byte(e), &env)
	}
	dir := filepath.Clean("/" + r.Header.Get(proto.HeaderDir)) // confine to workspace
	if !s.ingest(w, r, rp) {
		return
	}
	tSync := time.Since(t0)

	ws, err := s.acquire(rp, r.Header.Get(proto.HeaderWorktree))
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	defer s.release(ws)
	tAcq := time.Since(t0) - tSync

	w.Header().Set("Content-Type", "application/x-ndjson")
	w.Header().Set(proto.HeaderWorkspace, ws.path)
	w.WriteHeader(http.StatusOK)
	fl, _ := w.(http.Flusher)
	st := &streamer{enc: json.NewEncoder(w), fl: fl}
	st.send(proto.Event{T: proto.EvInfo, D: ws.path})
	st.send(proto.Event{T: proto.EvPhase, Name: "sync", Ms: tSync.Milliseconds()})
	st.send(proto.Event{T: proto.EvPhase, Name: "acquire", Ms: tAcq.Milliseconds()})

	tc := time.Now()
	if err := checkout(ws, r.Header.Get(proto.HeaderSnapshot)); err != nil {
		st.send(proto.Event{T: proto.EvError, D: err.Error()})
		return
	}
	st.send(proto.Event{T: proto.EvPhase, Name: "checkout", Ms: time.Since(tc).Milliseconds()})

	tr := time.Now()
	code := s.exec(r.Context(), ws.path, filepath.Join(ws.path, dir), argv, env, st)
	st.send(proto.Event{T: proto.EvPhase, Name: "exec", Ms: time.Since(tr).Milliseconds()})
	st.send(proto.Event{T: proto.EvExit, Code: code})
	log.Printf("run repo=%s ws=%s cmd=%q exit=%d total=%s", rp.id, filepath.Base(ws.path), argv, code, time.Since(t0).Round(time.Millisecond))
}

func (s *Server) exec(ctx context.Context, wsPath, dir string, argv []string, env map[string]string, st *streamer) int {
	cmd := exec.Command(argv[0], argv[1:]...)
	if len(argv) == 1 && strings.ContainsAny(argv[0], " |&;<>$`'\"") {
		cmd = exec.Command("bash", "-c", argv[0])
	}
	cmd.Dir = dir
	// A private, emptied TMPDIR per workspace: concurrent agents' test suites
	// otherwise collide in fixed paths under /tmp (ripgrep's integration
	// tests do exactly that). A microVM per run gives this for free later.
	tmp := filepath.Join(s.Root, "tmp", "ws", filepath.Base(filepath.Dir(wsPath)), filepath.Base(wsPath))
	os.RemoveAll(tmp)
	os.MkdirAll(tmp, 0o755)
	cmd.Env = append(os.Environ(), "TMPDIR="+tmp, "TMP="+tmp, "TEMP="+tmp)
	for k, v := range env {
		cmd.Env = append(cmd.Env, k+"="+v)
	}
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	pr, pw := io.Pipe()
	cmd.Stdout, cmd.Stderr = pw, pw
	if err := cmd.Start(); err != nil {
		st.send(proto.Event{T: proto.EvOut, D: err.Error() + "\n"})
		return 127
	}
	done := make(chan struct{})
	go func() {
		select {
		case <-ctx.Done(): // client went away (Ctrl-C): kill the whole process group
			syscall.Kill(-cmd.Process.Pid, syscall.SIGKILL)
		case <-done:
		}
	}()
	readDone := make(chan struct{})
	go func() {
		defer close(readDone)
		br := bufio.NewReaderSize(pr, 64<<10)
		buf := make([]byte, 32<<10)
		for {
			n, err := br.Read(buf)
			if n > 0 {
				st.send(proto.Event{T: proto.EvOut, D: string(buf[:n])})
			}
			if err != nil {
				return
			}
		}
	}()
	err := cmd.Wait()
	close(done)
	pw.Close()
	<-readDone
	if ee, ok := err.(*exec.ExitError); ok {
		return ee.ExitCode()
	} else if err != nil {
		return 1
	}
	return 0
}

func git(dir string, args ...string) (string, error) {
	cmd := exec.Command("git", args...)
	if dir != "" {
		cmd.Dir = dir
	}
	out, err := cmd.CombinedOutput()
	return string(out), err
}

func hasObject(gitDir, sha string) bool {
	return exec.Command("git", "--git-dir", gitDir, "cat-file", "-e", sha+"^{commit}").Run() == nil
}

func readOrCreate(path string, gen func() []byte) ([]byte, error) {
	if b, err := os.ReadFile(path); err == nil {
		return b, nil
	}
	b := gen()
	return b, os.WriteFile(path, b, 0o600)
}
