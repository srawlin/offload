// Command offload runs an agent's build/test commands on a warm remote node.
//
//	offload run [--local] [--json] [--stats FILE] -- <cmd> [args…]
//	offload test [--local] [--json]           auto-detect the test command
//	offload sync | watch                      push the worktree ahead of a run
//	offload pool N                            keep N warm workspaces for this repo
//	offload doctor                            check the node connection
//	offload init --node URL --token T --fingerprint F [--claude]
//	offload node [--listen :7070] [--root DIR]  run a node (Linux)
//	offload hook claude                       Claude Code PreToolUse hook
package main

import (
	"context"
	"encoding/json"
	"flag"
	"fmt"
	"log"
	"net/http"
	"os"
	"os/signal"
	"path/filepath"
	"strings"
	"syscall"
	"time"

	"offload/internal/client"
	"offload/internal/hook"
	"offload/internal/node"
)

func main() {
	if len(os.Args) < 2 {
		usage()
	}
	ctx, cancel := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer cancel()
	args := os.Args[2:]
	switch os.Args[1] {
	case "run":
		os.Exit(cmdRun(ctx, args, nil))
	case "test":
		os.Exit(cmdTest(ctx, args))
	case "sync":
		cmdSync(ctx)
	case "watch":
		cmdWatch(ctx)
	case "pool":
		cmdPool(ctx, args)
	case "doctor":
		cmdDoctor(ctx)
	case "init":
		cmdInit(args)
	case "node":
		cmdNode(args)
	case "hook":
		if len(args) > 0 && args[0] == "claude" {
			bin, _ := os.Executable()
			hook.ClaudePreToolUse(os.Stdin, os.Stdout, bin)
			return
		}
		usage()
	default:
		usage()
	}
}

func usage() {
	fmt.Fprintln(os.Stderr, `usage: offload run|test|sync|watch|pool|doctor|init|node|hook  (see source header)`)
	os.Exit(2)
}

func fatal(err error) {
	fmt.Fprintln(os.Stderr, "offload:", err)
	os.Exit(1)
}

func cmdRun(ctx context.Context, args []string, argv []string) int {
	fs := flag.NewFlagSet("run", flag.ExitOnError)
	local := fs.Bool("local", false, "run locally")
	asJSON := fs.Bool("json", false, "print only a JSON summary")
	budget := fs.Int("budget", 2000, "token budget for the JSON summary")
	stats := fs.String("stats", "", "append a JSON result line to this file")
	quiet := fs.Bool("quiet", false, "no footer")
	fs.Parse(args)
	if argv == nil {
		argv = fs.Args()
	}
	if len(argv) == 0 {
		fatal(fmt.Errorf("no command"))
	}
	cwd, _ := os.Getwd()
	o := client.Options{Argv: argv, Dir: cwd, Local: *local}
	if !*asJSON {
		o.Out = os.Stdout
	}
	res := client.Run(ctx, client.LoadConfig(), o)
	if *stats != "" {
		if f, err := os.OpenFile(*stats, os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0o644); err == nil {
			json.NewEncoder(f).Encode(res)
			f.Close()
		}
	}
	if *asJSON {
		res.Summary.Trim(*budget)
		e := json.NewEncoder(os.Stdout)
		e.SetIndent("", "  ")
		e.Encode(res)
	} else if !*quiet {
		fmt.Fprintln(os.Stderr, footer(res))
	}
	return res.ExitCode
}

func footer(r client.Result) string {
	var b strings.Builder
	fmt.Fprintf(&b, "offload: %s %.1fs", r.Mode, float64(r.DurationMs)/1000)
	if r.Mode == "remote" {
		fmt.Fprintf(&b, " (snapshot %dms, upload %dB/%dms, checkout %dms, exec %.1fs)",
			r.Phases["snapshot"], r.BundleBytes, r.Phases["upload"], r.Phases["node_checkout"], float64(r.Phases["node_exec"])/1000)
	}
	fmt.Fprintf(&b, " · %s · %s", r.Status, r.Summary.Line())
	if r.Fallback != "" {
		fmt.Fprintf(&b, " · fallback: %s", r.Fallback)
	}
	return b.String()
}

// detectTest picks the repo's test command: zero config first.
func detectTest(dir string) []string {
	exists := func(f string) bool { _, err := os.Stat(filepath.Join(dir, f)); return err == nil }
	switch {
	case exists("Cargo.toml"):
		return []string{"cargo", "test"}
	case exists("go.mod"):
		return []string{"go", "test", "./..."}
	case exists("package.json"):
		pm := "npm"
		if exists("pnpm-lock.yaml") {
			pm = "pnpm"
		} else if exists("yarn.lock") {
			pm = "yarn"
		}
		return []string{pm, "test"}
	case exists("pyproject.toml"), exists("pytest.ini"), exists("setup.py"):
		return []string{"python", "-m", "pytest", "-q"}
	case exists("Makefile"):
		return []string{"make", "test"}
	}
	return nil
}

func cmdTest(ctx context.Context, args []string) int {
	cwd, _ := os.Getwd()
	root := cwd
	if r, err := client.OpenRepo(cwd); err == nil {
		root = r.Root
	}
	argv := detectTest(cwd)
	if argv == nil {
		argv = detectTest(root)
	}
	if argv == nil {
		fatal(fmt.Errorf("could not detect a test command; use offload run -- <cmd>"))
	}
	return cmdRun(ctx, args, argv)
}

func cmdSync(ctx context.Context) {
	cwd, _ := os.Getwd()
	t := time.Now()
	n, err := client.Sync(ctx, client.LoadConfig(), cwd)
	if err != nil {
		fatal(err)
	}
	fmt.Fprintf(os.Stderr, "offload: synced %d bytes in %dms\n", n, time.Since(t).Milliseconds())
}

// cmdWatch is a simple speculative sync: poll the worktree and push changes.
func cmdWatch(ctx context.Context) {
	cwd, _ := os.Getwd()
	cfg := client.LoadConfig()
	fmt.Fprintln(os.Stderr, "offload: watching", cwd)
	for ctx.Err() == nil {
		t := time.Now()
		n, err := client.Sync(ctx, cfg, cwd)
		if err != nil {
			fmt.Fprintln(os.Stderr, "offload: sync:", err)
		} else if n > 0 {
			fmt.Fprintf(os.Stderr, "offload: pushed %d bytes in %dms\n", n, time.Since(t).Milliseconds())
		}
		select {
		case <-ctx.Done():
		case <-time.After(500 * time.Millisecond):
		}
	}
}

func cmdPool(ctx context.Context, args []string) {
	n := 3
	if len(args) > 0 {
		fmt.Sscan(args[0], &n)
	}
	cwd, _ := os.Getwd()
	if err := client.Pool(ctx, client.LoadConfig(), cwd, n); err != nil {
		fatal(err)
	}
	fmt.Fprintf(os.Stderr, "offload: %d warm workspaces\n", n)
}

func cmdDoctor(ctx context.Context) {
	cfg := client.LoadConfig()
	fmt.Printf("config: %s\nnode: %s\nmode: %s\n", client.ConfigPath(), cfg.Node, cfg.Mode)
	if cfg.Node == "" {
		fatal(fmt.Errorf("no node configured"))
	}
	for i := 0; i < 3; i++ {
		d, err := client.Ping(ctx, cfg)
		if err != nil {
			fatal(err)
		}
		fmt.Printf("health round trip (new TLS connection): %dms\n", d.Milliseconds())
	}
	cwd, _ := os.Getwd()
	if r, err := client.OpenRepo(cwd); err == nil {
		fmt.Printf("repo: %s (id %s, worktree %s)\n", r.Root, r.ID, r.Worktree)
	}
}

func cmdInit(args []string) {
	fs := flag.NewFlagSet("init", flag.ExitOnError)
	cfg := client.LoadConfig()
	fs.StringVar(&cfg.Node, "node", cfg.Node, "node URL (https://host:7070)")
	fs.StringVar(&cfg.Token, "token", cfg.Token, "node token")
	fs.StringVar(&cfg.Fingerprint, "fingerprint", cfg.Fingerprint, "node TLS certificate SHA-256")
	claude := fs.Bool("claude", false, "install the Claude Code hook in this repo (.claude/settings.local.json)")
	fs.Parse(args)
	os.MkdirAll(filepath.Dir(client.ConfigPath()), 0o700)
	b, _ := json.MarshalIndent(cfg, "", "  ")
	if err := os.WriteFile(client.ConfigPath(), b, 0o600); err != nil {
		fatal(err)
	}
	fmt.Println("wrote", client.ConfigPath())
	if *claude {
		if err := installClaudeHook(); err != nil {
			fatal(err)
		}
	}
}

// installClaudeHook merges a PreToolUse hook on Bash into the project's
// local Claude Code settings. offload uninstall would remove the same entry.
func installClaudeHook() error {
	cwd, _ := os.Getwd()
	root := cwd
	if r, err := client.OpenRepo(cwd); err == nil {
		root = r.Root
	}
	bin, _ := os.Executable()
	p := filepath.Join(root, ".claude", "settings.local.json")
	settings := map[string]interface{}{}
	if b, err := os.ReadFile(p); err == nil {
		if err := json.Unmarshal(b, &settings); err != nil {
			return fmt.Errorf("%s: %w", p, err)
		}
	}
	hooks, _ := settings["hooks"].(map[string]interface{})
	if hooks == nil {
		hooks = map[string]interface{}{}
	}
	pre, _ := hooks["PreToolUse"].([]interface{})
	command := bin + " hook claude"
	for _, h := range pre {
		if b, _ := json.Marshal(h); strings.Contains(string(b), "hook claude") {
			fmt.Println("Claude Code hook already installed in", p)
			return nil
		}
	}
	pre = append(pre, map[string]interface{}{
		"matcher": "Bash",
		"hooks":   []interface{}{map[string]interface{}{"type": "command", "command": command}},
	})
	hooks["PreToolUse"] = pre
	settings["hooks"] = hooks
	os.MkdirAll(filepath.Dir(p), 0o755)
	b, _ := json.MarshalIndent(settings, "", "  ")
	if err := os.WriteFile(p, b, 0o644); err != nil {
		return err
	}
	fmt.Println("installed Claude Code hook in", p)
	return nil
}

func cmdNode(args []string) {
	fs := flag.NewFlagSet("node", flag.ExitOnError)
	listen := fs.String("listen", ":7070", "listen address")
	root := fs.String("root", "/data/offload", "state directory")
	fs.Parse(args)
	s, err := node.New(*root)
	if err != nil {
		log.Fatal(err)
	}
	tlsCfg, fp, err := s.TLSConfig()
	if err != nil {
		log.Fatal(err)
	}
	log.Printf("offload node on %s, root %s\nfingerprint %s", *listen, *root, fp)
	srv := &http.Server{Addr: *listen, Handler: s.Handler(), TLSConfig: tlsCfg}
	log.Fatal(srv.ListenAndServeTLS("", ""))
}
