# Offload (Phase 0)

Offload runs a coding agent's build/test commands on a warm remote node and
streams the result back, so the laptop's CPU is left for the agents.
The results are in [docs/phase0-results.md](docs/phase0-results.md).

```
$ offload run -- cargo test
…native cargo output, streamed live…
offload: remote 3.0s (snapshot 40ms, upload 649B/159ms, checkout 17ms, exec 1.4s) · passed · 446 passed
```

## What's here

| Path | What |
| --- | --- |
| `cmd/offload` | One static binary: the CLI (`run`, `test`, `sync`, `watch`, `pool`, `doctor`, `init`, `hook`) and the node server (`node`) |
| `internal/client` | Worktree snapshots, incremental git-bundle sync, streaming, local fallback |
| `internal/node` | Snapshot ingest, warm per-repo workspace pool with worktree affinity, streaming exec |
| `internal/report` | Parsers for go test, cargo test, vitest, jest, mocha and pytest → agent-sized summary |
| `internal/hook` | Claude Code `PreToolUse` hook that rewrites test/build commands to `offload run` |
| `cmd/offload-bench` | The edit → verify benchmark (local vs offload, 1 and N concurrent agents) |
| `bench/scenarios.json` | Benchmark repos and commands |
| `infra/` | EC2 node lifecycle: `ec2-up.sh`, `deploy-node.sh`, `ec2-stop.sh`, `ec2-start.sh`, `ec2-down.sh` (destructive) |

## How a run works

1. **Snapshot (local, ~10–50 ms idle).** The worktree (tracked + untracked, minus ignored
   files) is written into a *private* git index (`.git/offload-index`, so the agent's own index
   is untouched) and committed as a snapshot whose parent is the last snapshot this node received.
2. **Sync (one request).** A git bundle of `snapshot ^last` carries only changed objects
   (typically < 1 KB). If the node lost the base, it answers 409 and the client resends the full
   tree (no history). An unchanged worktree sends nothing.
3. **Workspace.** The node keeps warm workspaces per repo (dependencies installed, build
   outputs present). Each agent's worktree sticks to "its" workspace. New ones are XFS reflink
   clones of the warmest idle workspace, so they start warm.
4. **Checkout.** `git checkout -f` rewrites only files that differ, so mtimes of unchanged files
   survive and cargo/go/tsc incremental builds stay warm. Ignored files (target/, node_modules)
   are kept.
5. **Exec + stream.** Output streams back as NDJSON events. Workspace paths are rewritten to
   local paths so `file:line` references open locally. The exit code is preserved, Ctrl-C kills
   the remote process group, and if the node is unreachable before the command starts, it runs
   locally and says why.

## Using it

```sh
go build -o bin/offload ./cmd/offload

# Node (Linux box with toolchains):  offload node --listen :7070 --root /data/offload
# prints the TLS fingerprint; the token is in <root>/token
bin/offload init --node https://HOST:7070 --token TOKEN --fingerprint FP
bin/offload doctor

cd your-repo
offload test                    # detects cargo/go/npm/pnpm/pytest/make
offload run -- go test ./pkg/...
offload run --json -- cargo test   # token-budgeted JSON summary only
offload watch                   # speculative sync: push edits as they happen
offload pool 3                  # keep 3 warm workspaces (3 parallel agents)
offload init --claude           # install the Claude Code hook in this repo
```

### EC2 node (the Phase 0 test box)

```sh
infra/ec2-up.sh c8i.8xlarge   # launch (ca-central-1); self-stops after 6 h as a cost cap
infra/deploy-node.sh          # install the node service, configure the local CLI
infra/ec2-stop.sh             # stop; disks and warm caches survive
infra/ec2-start.sh            # start again; refreshes IP, security group, CLI config
infra/ec2-down.sh             # DESTRUCTIVE: terminate and delete SG + key pair
```

AWS calls use the `offload` profile (`OFFLOAD_AWS_PROFILE` to override).
Node state lives in `~/.offload/` (key, `ec2.json`, `config.json`).

### Benchmark

```sh
go build -o bin/offload-bench ./cmd/offload-bench
bin/offload-bench -scenarios bench/scenarios.json -agents 1,3 -iters 3
```

Each agent gets its own git worktree (`<repo>-wt<i>`). Each iteration appends a unique,
compiled code change to a central file and runs the verify command, through
`offload run --local` or `offload run`, so both modes pay the same wrapper overhead.

## Phase 0 limitations

- **No isolation:** commands run as the node's `offload` user, not in a microVM. The E2B Runtime / microsandbox backend is Phase 1.
- **Warm workspaces, not memory snapshots:** daemons and services aren't snapshotted. Warmth
  comes from build outputs and dependency caches on disk.
- **No test-impact analysis, sharding, flake re-runs or result cache** beyond what the tool
  itself does (e.g. Go's test cache).
- **Single node,** one repo store per repo, no eviction or GC of old snapshots.
- **Submodules** are not synced (the gitlink is sent, not the content).
- **Environment:** the node doesn't inherit local env vars or secrets, by design. Pass
  what tests need with `--env` (not yet a flag; `Options.Env` exists).
