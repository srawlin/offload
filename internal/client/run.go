package client

import (
	"bufio"
	"bytes"
	"context"
	"crypto/sha256"
	"crypto/tls"
	"crypto/x509"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"syscall"
	"time"

	"offload/internal/proto"
	"offload/internal/report"
)

// Config says where the node is. Env vars override ~/.offload/config.json.
type Config struct {
	Node        string `json:"node"` // https://host:7070
	Token       string `json:"token"`
	Fingerprint string `json:"fingerprint"` // SHA-256 of the node's TLS cert
	Mode        string `json:"mode"`        // auto (default) | remote | local
}

func ConfigPath() string {
	home, _ := os.UserHomeDir()
	return filepath.Join(home, ".offload", "config.json")
}

func LoadConfig() Config {
	var c Config
	if b, err := os.ReadFile(ConfigPath()); err == nil {
		json.Unmarshal(b, &c)
	}
	for env, p := range map[string]*string{"OFFLOAD_NODE": &c.Node, "OFFLOAD_TOKEN": &c.Token,
		"OFFLOAD_FINGERPRINT": &c.Fingerprint, "OFFLOAD_MODE": &c.Mode} {
		if v := os.Getenv(env); v != "" {
			*p = v
		}
	}
	if c.Mode == "" {
		c.Mode = "auto"
	}
	return c
}

func (c Config) nodeKey() string { return shortHash(c.Node) }

func (c Config) httpClient() *http.Client {
	tlsCfg := &tls.Config{
		InsecureSkipVerify: true, // replaced by fingerprint pinning below
		VerifyPeerCertificate: func(raw [][]byte, _ [][]*x509.Certificate) error {
			if len(raw) == 0 {
				return errors.New("no certificate")
			}
			sum := sha256.Sum256(raw[0])
			if !strings.EqualFold(hex.EncodeToString(sum[:]), c.Fingerprint) {
				return errors.New("node certificate fingerprint mismatch")
			}
			return nil
		},
	}
	return &http.Client{Transport: &http.Transport{
		TLSClientConfig:   tlsCfg,
		ForceAttemptHTTP2: true,
		DialContext:       (&net.Dialer{Timeout: 3 * time.Second}).DialContext,
	}}
}

type Options struct {
	Argv  []string
	Dir   string // local cwd
	Env   map[string]string
	Out   io.Writer // live output (nil = discard)
	Local bool      // force local
}

type Result struct {
	Mode        string           `json:"mode"` // remote | local
	Fallback    string           `json:"fallback,omitempty"`
	Status      string           `json:"status"` // passed | failed | error
	ExitCode    int              `json:"exit_code"`
	DurationMs  int64            `json:"duration_ms"`
	Phases      map[string]int64 `json:"phases,omitempty"`
	BundleBytes int              `json:"bundle_bytes,omitempty"`
	Summary     report.Summary   `json:"summary"`
	Output      string           `json:"-"`
}

const maxCapture = 16 << 20

// Run executes the command remotely when a node is configured, and locally
// otherwise or when the node can't be reached before the command started.
func Run(ctx context.Context, cfg Config, o Options) Result {
	t0 := time.Now()
	var res Result
	switch {
	case o.Local || cfg.Mode == "local":
		res = runLocal(ctx, o)
	case cfg.Node == "":
		res = runLocal(ctx, o)
		res.Fallback = "no node configured (offload init)"
	default:
		var err error
		res, err = runRemote(ctx, cfg, o)
		if err != nil {
			if cfg.Mode == "remote" {
				res = Result{Mode: "remote", Status: "error", ExitCode: 125, Fallback: err.Error()}
			} else {
				res = runLocal(ctx, o)
				res.Fallback = err.Error()
			}
		}
	}
	res.DurationMs = time.Since(t0).Milliseconds()
	res.Summary = report.Parse(res.Output)
	if res.Status == "" {
		res.Status = "passed"
		if res.ExitCode != 0 {
			res.Status = "failed"
		}
	}
	return res
}

func runLocal(ctx context.Context, o Options) Result {
	cmd := exec.Command(o.Argv[0], o.Argv[1:]...)
	if len(o.Argv) == 1 && strings.ContainsAny(o.Argv[0], " |&;<>$`'\"") {
		cmd = exec.Command("bash", "-c", o.Argv[0])
	}
	cmd.Dir = o.Dir
	cmd.Env = os.Environ()
	for k, v := range o.Env {
		cmd.Env = append(cmd.Env, k+"="+v)
	}
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	var capture limitedBuffer
	w := io.Writer(&capture)
	if o.Out != nil {
		w = io.MultiWriter(o.Out, &capture)
	}
	cmd.Stdout, cmd.Stderr = w, w
	t := time.Now()
	res := Result{Mode: "local"}
	if err := cmd.Start(); err != nil {
		res.ExitCode, res.Status = 127, "error"
		res.Output = err.Error()
		return res
	}
	stop := context.AfterFunc(ctx, func() { syscall.Kill(-cmd.Process.Pid, syscall.SIGKILL) })
	err := cmd.Wait()
	stop()
	if ee, ok := err.(*exec.ExitError); ok {
		res.ExitCode = ee.ExitCode()
	} else if err != nil {
		res.ExitCode = 1
	}
	res.Phases = map[string]int64{"exec": time.Since(t).Milliseconds()}
	res.Output = capture.String()
	return res
}

func runRemote(ctx context.Context, cfg Config, o Options) (Result, error) {
	res := Result{Mode: "remote", Phases: map[string]int64{}}
	repo, err := OpenRepo(o.Dir)
	if err != nil {
		return res, err
	}
	rel, _ := filepath.Rel(repo.Root, mustAbs(o.Dir))
	ts := time.Now()
	snap, err := repo.TakeSnapshot(cfg.nodeKey())
	if err != nil {
		return res, err
	}
	bundle, err := snap.Bundle(false)
	if err != nil {
		return res, err
	}
	res.Phases["snapshot"] = time.Since(ts).Milliseconds()
	client := cfg.httpClient()

	tu := time.Now()
	resp, err := postRun(ctx, client, cfg, repo, snap, bundle, rel, o)
	if err == nil && resp.StatusCode == http.StatusConflict {
		resp.Body.Close()
		// The node lost (or never had) our base: resend the whole tree.
		if snap, err = snap.Full(); err != nil {
			return res, err
		}
		if bundle, err = snap.Bundle(true); err != nil {
			return res, err
		}
		resp, err = postRun(ctx, client, cfg, repo, snap, bundle, rel, o)
	}
	if err != nil {
		return res, fmt.Errorf("node unreachable: %w", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		b, _ := io.ReadAll(io.LimitReader(resp.Body, 4096))
		return res, fmt.Errorf("node: %s: %s", resp.Status, strings.TrimSpace(string(b)))
	}
	res.BundleBytes = len(bundle)
	snap.Synced()

	var capture limitedBuffer
	wsPath := ""
	exited := false
	sc := bufio.NewScanner(resp.Body)
	sc.Buffer(make([]byte, 1<<20), 64<<20)
	first := true
	for sc.Scan() {
		var e proto.Event
		if json.Unmarshal(sc.Bytes(), &e) != nil {
			continue
		}
		if first {
			res.Phases["upload"] = time.Since(tu).Milliseconds() // includes node-side sync
			first = false
		}
		switch e.T {
		case proto.EvInfo:
			wsPath = e.D
		case proto.EvPhase:
			res.Phases["node_"+e.Name] = e.Ms
		case proto.EvOut:
			d := e.D
			if wsPath != "" {
				d = strings.ReplaceAll(d, wsPath, repo.Root) // keep file:line links local
			}
			capture.WriteString(d)
			if o.Out != nil {
				io.WriteString(o.Out, d)
			}
		case proto.EvError:
			res.Status, res.ExitCode = "error", 125
			res.Output = capture.String()
			return res, fmt.Errorf("node: %s", e.D)
		case proto.EvExit:
			res.ExitCode, exited = e.Code, true
		}
	}
	res.Output = capture.String()
	if !exited {
		if ctx.Err() != nil {
			res.ExitCode, res.Status = 130, "error"
			return res, nil
		}
		// Output already streamed; don't silently re-run locally.
		res.Status, res.ExitCode = "error", 125
		res.Fallback = "connection to node lost mid-run"
	}
	return res, nil
}

func postRun(ctx context.Context, client *http.Client, cfg Config, repo *Repo, snap *Snapshot, bundle []byte, rel string, o Options) (*http.Response, error) {
	req, err := http.NewRequestWithContext(ctx, "POST", strings.TrimRight(cfg.Node, "/")+"/v1/run", bytes.NewReader(bundle))
	if err != nil {
		return nil, err
	}
	argv, _ := json.Marshal(o.Argv)
	req.Header.Set("Authorization", "Bearer "+cfg.Token)
	req.Header.Set(proto.HeaderRepo, repo.ID)
	req.Header.Set(proto.HeaderWorktree, repo.Worktree)
	req.Header.Set(proto.HeaderSnapshot, snap.Commit)
	if len(bundle) > 0 {
		req.Header.Set(proto.HeaderBase, snap.Base)
	}
	req.Header.Set(proto.HeaderCmd, string(argv))
	req.Header.Set(proto.HeaderDir, filepath.ToSlash(rel))
	if len(o.Env) > 0 {
		e, _ := json.Marshal(o.Env)
		req.Header.Set(proto.HeaderEnv, string(e))
	}
	return client.Do(req)
}

// Sync pushes the current worktree snapshot without running anything, so a
// later run finds nothing to upload (speculative sync).
func Sync(ctx context.Context, cfg Config, dir string) (bytesSent int, err error) {
	repo, err := OpenRepo(dir)
	if err != nil {
		return 0, err
	}
	snap, err := repo.TakeSnapshot(cfg.nodeKey())
	if err != nil {
		return 0, err
	}
	bundle, err := snap.Bundle(false)
	if err != nil || bundle == nil {
		return 0, err
	}
	post := func(b []byte, s *Snapshot) (int, error) {
		req, _ := http.NewRequestWithContext(ctx, "POST", strings.TrimRight(cfg.Node, "/")+"/v1/sync", bytes.NewReader(b))
		req.Header.Set("Authorization", "Bearer "+cfg.Token)
		req.Header.Set(proto.HeaderRepo, repo.ID)
		req.Header.Set(proto.HeaderSnapshot, s.Commit)
		req.Header.Set(proto.HeaderBase, s.Base)
		resp, err := cfg.httpClient().Do(req)
		if err != nil {
			return 0, err
		}
		io.Copy(io.Discard, resp.Body)
		resp.Body.Close()
		return resp.StatusCode, nil
	}
	code, err := post(bundle, snap)
	if err == nil && code == http.StatusConflict {
		if snap, err = snap.Full(); err != nil {
			return 0, err
		}
		if bundle, err = snap.Bundle(true); err != nil {
			return 0, err
		}
		code, err = post(bundle, snap)
	}
	if err != nil {
		return 0, err
	}
	if code != http.StatusOK {
		return 0, fmt.Errorf("node: HTTP %d", code)
	}
	snap.Synced()
	return len(bundle), nil
}

// Pool asks the node to keep at least n warm workspaces for this repo.
func Pool(ctx context.Context, cfg Config, dir string, n int) error {
	repo, err := OpenRepo(dir)
	if err != nil {
		return err
	}
	req, _ := http.NewRequestWithContext(ctx, "POST", fmt.Sprintf("%s/v1/pool?min=%d", strings.TrimRight(cfg.Node, "/"), n), nil)
	req.Header.Set("Authorization", "Bearer "+cfg.Token)
	req.Header.Set(proto.HeaderRepo, repo.ID)
	resp, err := cfg.httpClient().Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	b, _ := io.ReadAll(resp.Body)
	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("node: %s: %s", resp.Status, b)
	}
	return nil
}

// Ping measures a round trip to the node (including TLS setup).
func Ping(ctx context.Context, cfg Config) (time.Duration, error) {
	t := time.Now()
	req, _ := http.NewRequestWithContext(ctx, "GET", strings.TrimRight(cfg.Node, "/")+"/v1/health", nil)
	resp, err := cfg.httpClient().Do(req)
	if err != nil {
		return 0, err
	}
	resp.Body.Close()
	return time.Since(t), nil
}

func mustAbs(p string) string {
	a, err := filepath.Abs(p)
	if err != nil {
		return p
	}
	if r, err := filepath.EvalSymlinks(a); err == nil {
		return r
	}
	return a
}

type limitedBuffer struct{ bytes.Buffer }

func (b *limitedBuffer) Write(p []byte) (int, error) {
	if b.Len() < maxCapture {
		b.Buffer.Write(p)
	}
	return len(p), nil
}

func (b *limitedBuffer) WriteString(s string) (int, error) { return b.Write([]byte(s)) }
