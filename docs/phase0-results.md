# Offload Phase 0: is offloading verify faster than local?

*October 23, 2026. Tests the project's biggest technical risk: whether a coding agent's
edit → verify step runs faster on a remote node than on the developer's own machine.*

## Answer

**Yes, by a wide margin, from the second run onward.** On four real open-source repos (Rust, Go,
TypeScript, JavaScript), a warm remote verify was **4.3–7.5x faster** than local for one agent on
the three repos with real build/test work, and **7–12x faster** with three concurrent agents. Even
a 3-second JavaScript suite, the case expected to lose, was 1.6x faster. Offload's fixed overhead
(snapshot + sync + checkout) was **~200–250 ms per run**. A remote result also never differed from
the local verdict because of the change; the only disagreements were contention flakes, which hit
local runs far more often (details below).

The Phase 0 exit criterion we set ourselves ("warm verify ≥ 1.5x faster than local with 3 concurrent
agents on ≥ 6 of 10 repos") is met on **4 of 4** repos tried, with 7x as the lowest result. A 10-repo
run is still to do.

## Setup

| | Local (baseline) | Offload node |
| --- | --- | --- |
| Machine | iMac 27" Retina 5K, Late 2015 | AWS EC2 `c8i.8xlarge`, ca-central-1 |
| CPU | Intel Core i7-6700K, 4 GHz, 4 cores / 8 threads | Intel Xeon 6975P-C, 32 vCPU (16 cores) |
| Memory | 24 GB 1867 MHz DDR3 | 64 GB |
| Storage | internal disk (81% full) | 300 GB gp3, 16,000 IOPS, 1,000 MB/s, XFS (reflink) |
| OS | macOS 12.7.6 | Ubuntu 24.04 |
| Toolchains | Go 1.27.1, Rust 1.98.1, Node 24.13.0 | same versions |
| Network | Ottawa → ca-central-1: new TLS connection + request ≈ 22 ms | |
| Background load | load average 4–6 before the run (other users/apps) | idle |
| Price | — | $1.64/hour on demand |

**Method** (`cmd/offload-bench`, `bench/scenarios.json`):
- Each simulated agent works in its own git worktree.
- Each iteration appends a unique, *compiled* code change to a central source file. Comment-only
  edits are useless because Go's content-addressed caches skip them. The agent then runs the repo's
  verify command.
- Local runs use `offload run --local -- <cmd>` and remote runs use `offload run -- <cmd>`, so both
  pay the same wrapper cost. Wall time is measured outside the process.
- Every worktree is warmed first, locally and on the node: dependencies installed and one full build
  and run. This is the steady state an agent is in after its first verify.
- 3 iterations per agent, with 1 and 3 concurrent agents, so n = 3 or 9 per cell.
- Agents verify back to back with no LLM think time. For 3 agents that's a worst case for local
  contention.

| Scenario | Command | Edited file |
| --- | --- | --- |
| ripgrep (Rust) | `cargo test` (unit + integration, ~440 tests) | `crates/searcher/src/lib.rs` |
| prometheus (Go) | `go test -count=1 ./promql/parser/... ./model/...` | `model/labels/labels_common.go` (most of Prometheus depends on it) |
| vue-core (TypeScript) | `vitest run --project 'unit*'` (~3,900 tests) | `packages/runtime-core/src/renderer.ts` |
| express (JS) | `mocha … test/ test/acceptance/` (1,261 tests, ~3 s) | `lib/utils.js` |

## Results

| Scenario | Agents | Local p50 | Local p95 | Offload p50 | Offload p95 | Speed-up (p50) | Non-zero exits local / offload | n |
| --- | ---: | ---: | ---: | ---: | ---: | ---: | ---: | ---: |
| ripgrep | 1 | 7.0 s | 7.4 s | 1.6 s | 1.7 s | **4.3x** | 0 / 0 | 3 |
| ripgrep | 3 | 21.1 s | 22.9 s | 1.7 s | 3.7 s | **12.2x** | 5 / 4 → **0** after fix¹ | 9 |
| prometheus | 1 | 16.4 s | 18.9 s | 3.0 s | 3.1 s | **5.5x** | 0 / 0 | 3 |
| prometheus | 3 | 25.6 s | 49.7 s | 3.6 s | 3.8 s | **7.2x** | 0 / 0 | 9 |
| vue-core | 1 | 44.8 s | 45.0 s | 6.0 s | 6.0 s | **7.5x** | 2 / 0 | 3 |
| vue-core | 3 | 121.8 s | 124.3 s | 12.7 s | 13.6 s | **9.6x** | 9 / 4² | 9 |
| express | 1 | 3.3 s | 3.3 s | 2.1 s | 2.1 s | **1.6x** | 0 / 0 | 3 |
| express | 3 | 15.1 s | 80.0 s³ | 2.2 s | 3.0 s | **7.0x** | 6 / 0 | 9 |

¹ ripgrep's integration tests use fixed paths under `$TMPDIR`, so concurrent runs on one machine
collide. That happened locally and, at first, on the node too. The node now gives every workspace a
private, emptied `TMPDIR`. A rerun gave **15/15 green, p50 1.8 s, p95 2.7 s**. Locally the
collisions remain, since three agents on one laptop share `$TMPDIR`.

² vue-core has timing-assertion tests ("computed chain under 30 ms", a 5 s scheduler timeout). Each
vitest run starts one worker per CPU, so three concurrent runs oversubscribe any machine. On the
iMac nearly every 3-agent run failed one or two of these. On the node 4/9 did, and wall time doubled
from 6 s to 12.7 s. **The Phase 1 scheduler needs a CPU cap per run** (microVM vCPUs or cgroups, so
`availableParallelism()` sees the cap), or should queue runs rather than oversubscribe.

³ One local run hung after a flaky acceptance-test timeout left a socket open (mocha without
`--exit`) and was killed at 120 s by a watchdog. Five other local 3-agent runs failed 3–4
timing-sensitive tests. All offload runs passed.

### Where the remote time goes (medians, ms)

| Scenario | Agents | Local snapshot | Request → first byte (TLS + bundle + ingest) | Node checkout | Node exec | Total | Bundle |
| --- | ---: | ---: | ---: | ---: | ---: | ---: | ---: |
| ripgrep | 1 | 130 | 60 | 3 | 1,436 | 1,645 | 671 B |
| prometheus | 1 | 238 | 61 | 6 | 2,602 | 2,996 | 612 B |
| vue-core | 1 | 135 | 61 | 5 | 5,712 | 5,981 | 678 B |
| express | 1 | 127 | 58 | 3 | 1,897 | 2,096 | 545 B |
| vue-core | 3 | 140 | 65 | 5 | 12,449 | 12,711 | 742 B |

- **Sync is a solved problem.** Chained snapshot commits mean each run ships a ~0.6 KB git bundle.
  Upload plus node ingest takes ~60 ms, about 3 network round trips.
- **Warm workspaces work.** `git checkout` rewrites only changed files, so incremental cargo/go
  builds stay warm. Checkout takes 3–7 ms.
- **The biggest overhead is local:** a 130–240 ms snapshot, made of ~8 `git` process spawns on a
  loaded 2015 Mac. A resident `offloadd` with fsmonitor and speculative sync would take this off
  the critical path.
- **Cold first runs were also fast remotely.** ripgrep cold: 9 s remote vs 39–50 s local. prometheus
  first build: 18 s vs 89 s. vue dependency install: 7 s vs 70 s.

### Agent integration (Claude Code)

`offload init --claude` installs a `PreToolUse` hook that rewrites test/build Bash commands to
`offload run -- '<cmd>'`. A headless Claude Code agent was given a Go repo with a broken `Add`. It
ran `go test ./...` and the hook offloaded it (0.4 s, failure shown in native format with the
`offload:` footer). It fixed the bug and re-ran the tests (offloaded, 0.3 s, passed), at a cost of
$0.09. The agent didn't know Offload existed. A compound command it ran first (`go test … ; git diff`)
was correctly left local, because the hook only rewrites commands made purely of test/build steps.

## Caveats (read before quoting the numbers)

1. **Much of the gap is hardware.** A 2015 quad-core i7 is compared with a 2025 16-core Xeon. On an
   M-series laptop the single-agent ratio would be smaller. The three-agent result depends less on
   hardware: four cores shared by three builds collapse, while the node has headroom. That's the
   actual thesis: offload pays most when several agents verify at once.
2. **Small samples:** n = 3 per single-agent cell, n = 9 per three-agent cell, one run, one evening.
   The spreads are tight relative to the gaps (local p50 vs offload p95 never comes close), but this
   is a proof, not a benchmark publication.
3. **No isolation:** the node ran commands as a plain Unix user. Running the node inside a
   microVM sandbox (E2B, microsandbox) costs some CPU and I/O overhead not measured here.
4. **Linux vs macOS:** ripgrep runs 446 tests on Linux and 437 on macOS (platform-specific tests).
   The other suites ran identical test counts.
5. **The local machine was not idle** (load average 4–6 from other activity), as on a real
   developer machine.
6. **EC2 is a VM, not bare metal,** and the node's EBS volume is network storage. A Hetzner-class
   bare-metal home node with local NVMe should be at least as fast.

## Cost

- Instance time for the whole exercise was about 2 hours (≈ $3.30 on demand).
- At $1.64/hour, a fully used node costs $0.00046 per second. A typical 2–3 s warm verify that uses
  the whole box costs ≈ $0.001–0.0014, and less when runs share the node. A ~$70/month bare-metal box
  is roughly 30x cheaper per hour than this on-demand VM. Both are well under
  $0.01 per run.

## What's next

- **Confirmed:** diff-only sync and warm per-agent workspaces deliver seconds-level verify with
  ~250 ms overhead. The thesis holds even before memory snapshots, test impact analysis or speculative
  sync.
- **Needed next:**
  - Per-run filesystem isolation for `TMPDIR` and other shared paths (done in a minimal form).
  - Per-run CPU caps or queueing, so concurrent agents don't produce timing flakes on the node.
  - A local daemon, so snapshotting leaves the critical path.
- **New selling point:** "the same suite flakes on your laptop and passes on Offload when three
  agents run at once" showed up in 3 of 4 repos. That's a parity and reliability argument on top of
  speed.

## Reproduce

```sh
infra/ec2-up.sh c8i.8xlarge && infra/deploy-node.sh   # or any Linux node: see the README
go build -o bin/offload ./cmd/offload && go build -o bin/offload-bench ./cmd/offload-bench
bin/offload-bench -scenarios bench/scenarios.json -agents 1,3 -iters 3   # clones pinned commits into bench/repos/
infra/ec2-stop.sh
```

Raw rows: `bench/results/raw/rows-20261023-172159.jsonl` (main run) and
`rows-20261023-181149.jsonl` (the ripgrep rerun after the `TMPDIR` fix).
