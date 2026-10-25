// Command offload-bench is the Phase 0 speed proof: it runs agent-like
// edit → verify loops on real repos, locally and through Offload, with 1 and
// N concurrent agents (one git worktree each), and reports p50/p95 verify time.
//
//	offload-bench -scenarios bench/scenarios.json -agents 1,3 -iters 5 -out bench/results
package main

import (
	"encoding/json"
	"flag"
	"fmt"
	"log"
	"math"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"
)

type Scenario struct {
	Name   string `json:"name"`
	Repo   string `json:"repo"`   // local clone (relative to the current directory)
	URL    string `json:"url"`    // cloned into Repo if it is missing
	Commit string `json:"commit"` // pinned commit to check out after cloning
	Setup  string `json:"setup"`  // once per worktree, local and remote (e.g. dependency install)
	Cmd    string `json:"cmd"`    // the verify command an agent would run
	Edit   string `json:"edit"`   // file (relative) that each iteration touches
	// EditLine is appended to Edit each iteration; {N} becomes a unique number.
	// It must change compiled code (comments alone hit Go's content caches).
	EditLine string `json:"edit_line"`
	Note     string `json:"note"`
}

type Row struct {
	Scenario string           `json:"scenario"`
	Mode     string           `json:"mode"`
	Agents   int              `json:"agents"`
	Agent    int              `json:"agent"`
	Iter     int              `json:"iter"`
	WallMs   int64            `json:"wall_ms"`
	Exit     int              `json:"exit"`
	Phases   map[string]int64 `json:"phases,omitempty"`
	Bundle   int              `json:"bundle_bytes,omitempty"`
	Fallback string           `json:"fallback,omitempty"`
	Summary  string           `json:"summary,omitempty"`
}

var (
	offloadBin = flag.String("offload", "bin/offload", "offload binary")
	scenFile   = flag.String("scenarios", "bench/scenarios.json", "scenario file")
	only       = flag.String("only", "", "comma-separated scenario names")
	agentsFlag = flag.String("agents", "1,3", "concurrent agent counts")
	modesFlag  = flag.String("modes", "local,remote", "modes to measure")
	iters      = flag.Int("iters", 5, "verify runs per agent per cell")
	think      = flag.Duration("think", 0, "pause between an agent's verify runs (LLM think time)")
	outDir     = flag.String("out", "bench/results", "output directory")
	skipWarm   = flag.Bool("skip-warm", false, "skip setup and warm-up runs")
)

func main() {
	flag.Parse()
	bin, _ := filepath.Abs(*offloadBin)
	*offloadBin = bin
	var scens []Scenario
	b, err := os.ReadFile(*scenFile)
	if err != nil {
		log.Fatal(err)
	}
	if err := json.Unmarshal(b, &scens); err != nil {
		log.Fatal(err)
	}
	var agentCounts []int
	maxAgents := 1
	for _, a := range strings.Split(*agentsFlag, ",") {
		n, _ := strconv.Atoi(a)
		agentCounts = append(agentCounts, n)
		if n > maxAgents {
			maxAgents = n
		}
	}
	modes := strings.Split(*modesFlag, ",")
	os.MkdirAll(filepath.Join(*outDir, "raw"), 0o755)
	stamp := time.Now().Format("20060102-150405")
	rowsPath := filepath.Join(*outDir, "raw", "rows-"+stamp+".jsonl")
	rowsF, err := os.Create(rowsPath)
	if err != nil {
		log.Fatal(err)
	}
	defer rowsF.Close()
	var rowsMu sync.Mutex
	var all []Row
	record := func(r Row) {
		rowsMu.Lock()
		defer rowsMu.Unlock()
		json.NewEncoder(rowsF).Encode(r)
		all = append(all, r)
	}
	log.Printf("load average at start: %s", loadAvg())

	for _, sc := range scens {
		sc.Repo, _ = filepath.Abs(sc.Repo)
		if *only != "" && !contains(strings.Split(*only, ","), sc.Name) {
			continue
		}
		wts, err := worktrees(sc, maxAgents)
		if err != nil {
			log.Fatalf("%s: %v", sc.Name, err)
		}
		if !*skipWarm {
			warm(sc, wts, modes)
		}
		for _, n := range agentCounts {
			for _, mode := range modes {
				log.Printf("== %s: %d agent(s), %s", sc.Name, n, mode)
				var wg sync.WaitGroup
				for a := 0; a < n; a++ {
					wg.Add(1)
					go func(a int) {
						defer wg.Done()
						for it := 0; it < *iters; it++ {
							edit(wts[a], sc, a, it)
							r := verify(sc, wts[a], mode)
							r.Agents, r.Agent, r.Iter = n, a, it
							record(r)
							log.Printf("   %s a%d i%d %s %6dms exit=%d %s", sc.Name, a, it, mode, r.WallMs, r.Exit, r.Summary)
							if *think > 0 {
								time.Sleep(*think)
							}
						}
					}(a)
				}
				wg.Wait()
			}
		}
		for _, wt := range wts {
			exec.Command("git", "-C", wt, "checkout", "-q", "--", sc.Edit).Run()
		}
	}
	md := summarize(all)
	mdPath := filepath.Join(*outDir, "summary-"+stamp+".md")
	os.WriteFile(mdPath, []byte(md), 0o644)
	fmt.Print(md)
	log.Printf("rows: %s\nsummary: %s", rowsPath, mdPath)
}

// worktrees returns n agent worktrees: <repo>-wt0..n-1, created on demand.
func worktrees(sc Scenario, n int) ([]string, error) {
	if _, err := os.Stat(sc.Repo); err != nil && sc.URL != "" {
		log.Printf("cloning %s into %s", sc.URL, sc.Repo)
		if out, err := exec.Command("git", "clone", "-q", sc.URL, sc.Repo).CombinedOutput(); err != nil {
			return nil, fmt.Errorf("clone: %s", out)
		}
		if sc.Commit != "" {
			if out, err := exec.Command("git", "-C", sc.Repo, "checkout", "-q", "--detach", sc.Commit).CombinedOutput(); err != nil {
				return nil, fmt.Errorf("checkout %s: %s", sc.Commit, out)
			}
		}
	}
	var wts []string
	for i := 0; i < n; i++ {
		wt := fmt.Sprintf("%s-wt%d", strings.TrimRight(sc.Repo, "/"), i)
		if _, err := os.Stat(wt); err != nil {
			if out, err := exec.Command("git", "-C", sc.Repo, "worktree", "add", "-q", "--detach", wt).CombinedOutput(); err != nil {
				return nil, fmt.Errorf("worktree add: %s", out)
			}
		}
		wts = append(wts, wt)
	}
	return wts, nil
}

// warm brings every worktree to the steady state an agent would be in after
// its first run: dependencies installed and an initial build done, both
// locally and on the node.
func warm(sc Scenario, wts []string, modes []string) {
	for _, mode := range modes {
		for i, wt := range wts {
			if sc.Setup != "" {
				log.Printf("setup %s wt%d %s: %s", sc.Name, i, mode, sc.Setup)
				r := run(wt, mode, sc.Setup)
				log.Printf("   setup %dms exit=%d", r.WallMs, r.Exit)
			}
			r := run(wt, mode, sc.Cmd)
			log.Printf("warm %s wt%d %s: %dms exit=%d %s", sc.Name, i, mode, r.WallMs, r.Exit, r.Summary)
			if mode == "remote" && i == 0 {
				// Clone the warm workspace so all agents start warm on the node.
				c := exec.Command(*offloadBin, "pool", strconv.Itoa(len(wts)))
				c.Dir = wt
				if out, err := c.CombinedOutput(); err != nil {
					log.Printf("pool: %v %s", err, out)
				}
			}
		}
	}
}

func edit(wt string, sc Scenario, agent, iter int) {
	p := filepath.Join(wt, sc.Edit)
	f, err := os.OpenFile(p, os.O_APPEND|os.O_WRONLY, 0)
	if err != nil {
		log.Fatalf("edit %s: %v", p, err)
	}
	line := sc.EditLine
	if line == "" {
		line = "// offload-bench {N}"
	}
	n := strconv.FormatInt(time.Now().UnixNano()/1000, 10) + strconv.Itoa(agent)
	fmt.Fprintf(f, "\n%s\n", strings.ReplaceAll(line, "{N}", n))
	f.Close()
}

func verify(sc Scenario, wt, mode string) Row {
	r := run(wt, mode, sc.Cmd)
	r.Scenario, r.Mode = sc.Name, mode
	return r
}

func run(wt, mode, cmdline string) Row {
	stats, _ := os.CreateTemp("", "offload-bench-*.json")
	stats.Close()
	defer os.Remove(stats.Name())
	args := []string{"run", "--quiet", "--stats", stats.Name()}
	if mode == "local" {
		args = append(args, "--local")
	}
	args = append(args, "--", cmdline)
	c := exec.Command(*offloadBin, args...)
	c.Dir = wt
	c.Env = append(os.Environ(), "OFFLOAD_MODE="+map[string]string{"local": "local", "remote": "remote"}[mode])
	t := time.Now()
	out, _ := c.CombinedOutput()
	row := Row{WallMs: time.Since(t).Milliseconds(), Exit: c.ProcessState.ExitCode()}
	var res struct {
		Phases      map[string]int64 `json:"phases"`
		BundleBytes int              `json:"bundle_bytes"`
		Fallback    string           `json:"fallback"`
		Summary     struct {
			Framework      string `json:"framework"`
			Passed, Failed int
		} `json:"summary"`
	}
	if b, err := os.ReadFile(stats.Name()); err == nil && json.Unmarshal(b, &res) == nil {
		row.Phases, row.Bundle, row.Fallback = res.Phases, res.BundleBytes, res.Fallback
		row.Summary = fmt.Sprintf("%s %dp/%df", res.Summary.Framework, res.Summary.Passed, res.Summary.Failed)
	}
	if row.Exit != 0 {
		tail := string(out)
		if len(tail) > 1500 {
			tail = tail[len(tail)-1500:]
		}
		log.Printf("   non-zero exit (%d) in %s:\n%s", row.Exit, wt, tail)
	}
	return row
}

func summarize(rows []Row) string {
	type key struct {
		sc     string
		agents int
	}
	cells := map[key]map[string][]float64{}
	exits := map[key]map[string]int{}
	var order []key
	for _, r := range rows {
		k := key{r.Scenario, r.Agents}
		if cells[k] == nil {
			cells[k] = map[string][]float64{}
			exits[k] = map[string]int{}
			order = append(order, k)
		}
		cells[k][r.Mode] = append(cells[k][r.Mode], float64(r.WallMs)/1000)
		if r.Exit != 0 {
			exits[k][r.Mode]++
		}
	}
	var b strings.Builder
	fmt.Fprintf(&b, "| Scenario | Agents | Local p50 | Local p95 | Offload p50 | Offload p95 | Speed-up (p50) | Non-zero exits L/R | n |\n")
	fmt.Fprintf(&b, "| --- | ---: | ---: | ---: | ---: | ---: | ---: | ---: | ---: |\n")
	for _, k := range order {
		l, r := cells[k]["local"], cells[k]["remote"]
		sp := "—"
		if len(l) > 0 && len(r) > 0 {
			sp = fmt.Sprintf("**%.1fx**", pct(l, 50)/pct(r, 50))
		}
		fmt.Fprintf(&b, "| %s | %d | %s | %s | %s | %s | %s | %d/%d | %d |\n", k.sc, k.agents,
			secs(l, 50), secs(l, 95), secs(r, 50), secs(r, 95), sp, exits[k]["local"], exits[k]["remote"], max(len(l), len(r)))
	}
	// Where remote time goes.
	fmt.Fprintf(&b, "\nOffload phase medians (ms): snapshot = local git snapshot, upload = request until first byte back (TLS + bundle + node ingest), checkout/exec on the node.\n\n")
	fmt.Fprintf(&b, "| Scenario | Agents | snapshot | upload | node checkout | node exec | wall | bundle bytes |\n| --- | ---: | ---: | ---: | ---: | ---: | ---: | ---: |\n")
	for _, k := range order {
		var sn, up, co, ex, wall, bb []float64
		for _, r := range rows {
			if r.Scenario == k.sc && r.Agents == k.agents && r.Mode == "remote" {
				sn = append(sn, float64(r.Phases["snapshot"]))
				up = append(up, float64(r.Phases["upload"]))
				co = append(co, float64(r.Phases["node_checkout"]))
				ex = append(ex, float64(r.Phases["node_exec"]))
				wall = append(wall, float64(r.WallMs))
				bb = append(bb, float64(r.Bundle))
			}
		}
		if len(wall) == 0 {
			continue
		}
		fmt.Fprintf(&b, "| %s | %d | %.0f | %.0f | %.0f | %.0f | %.0f | %.0f |\n", k.sc, k.agents, pct(sn, 50), pct(up, 50), pct(co, 50), pct(ex, 50), pct(wall, 50), pct(bb, 50))
	}
	return b.String()
}

func pct(xs []float64, p float64) float64 {
	if len(xs) == 0 {
		return math.NaN()
	}
	s := append([]float64(nil), xs...)
	sort.Float64s(s)
	idx := p / 100 * float64(len(s)-1)
	lo, hi := int(math.Floor(idx)), int(math.Ceil(idx))
	return s[lo] + (s[hi]-s[lo])*(idx-float64(lo))
}

func secs(xs []float64, p float64) string {
	if len(xs) == 0 {
		return "—"
	}
	return fmt.Sprintf("%.1fs", pct(xs, p))
}

func contains(xs []string, s string) bool {
	for _, x := range xs {
		if x == s {
			return true
		}
	}
	return false
}

func loadAvg() string {
	out, _ := exec.Command("sysctl", "-n", "vm.loadavg").Output()
	return strings.TrimSpace(string(out))
}
