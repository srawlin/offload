# Offload

**Run your coding agents' builds and tests on a warm remote machine, so your laptop stops being
the bottleneck.**

Coding agents spend much of their time in the edit → build → test loop. When three agents run
`cargo test` at once on a laptop, every run takes three times as long. Offload sends each verify
command to a fast Linux node that keeps a warm copy of your repo: dependencies installed, build
caches hot, one workspace per agent. It ships only the files that changed (usually under 1 KB) and
streams the output back as if the command had run locally.

```
$ offload run -- cargo test
…native cargo output, streamed live…
offload: remote 1.6s (snapshot 130ms, upload 671B/60ms, checkout 3ms, exec 1.4s) · passed · 446 passed
```

Agents don't need to know it exists. With the Claude Code hook installed, Claude's `go test ./...`
runs remotely and the agent sees the usual output.

> **Status: early preview.** The idea is proven (below), and the CLI and node work. There's no hosted
> service: you [bring your own node](docs/backends.md), such as a Linux box, Docker, an
> [E2B](https://e2b.dev) sandbox or [microsandbox](https://github.com/superradcompany/microsandbox).
> Expect rough edges and breaking changes.

## Does it help?

Phase 0 benchmark. Each run is an edit followed by the repo's own test command, after a warm-up
run. The baseline is a 2015 quad-core iMac and the node is a 32-vCPU EC2 VM 18 ms away
([full report](docs/phase0-results.md)):

| Repo | 1 agent: local → Offload | 3 agents at once: local → Offload |
| --- | --- | --- |
| ripgrep (Rust, `cargo test`) | 7.0 s → 1.6 s (**4.3x**) | 21.1 s → 1.7 s (**12x**) |
| prometheus (Go, `go test` after a core edit) | 16.4 s → 3.0 s (**5.5x**) | 25.6 s → 3.6 s (**7.2x**) |
| vue-core (TypeScript, vitest, ~3,900 tests) | 44.8 s → 6.0 s (**7.5x**) | 121.8 s → 12.7 s (**9.6x**) |
| express (JavaScript, a 3 s mocha suite) | 3.3 s → 2.1 s (**1.6x**) | 15.1 s → 2.2 s (**7.0x**) |

Offload adds about 250 ms per run. Much of the single-agent gap is old laptop versus big server.
The multi-agent gap is the point: a laptop's cores are shared by every agent, while a node has
headroom. Timing-sensitive tests that flaked on the overloaded laptop mostly passed remotely.

## Quick start

**1. Install the CLI** (macOS or Linux): download a binary from
[Releases](https://github.com/srawlin/offload/releases), or with Go 1.23+:

```sh
go install github.com/srawlin/offload/cmd/offload@latest
```

**2. Start a node.** On any Linux machine with your toolchains, or in Docker:

```sh
docker build -t offload-node .
docker run -d --name offload-node -p 7070:7070 -v offload-data:/data/offload \
  -e OFFLOAD_NODE_TOKEN=$(openssl rand -hex 24) offload-node
docker logs offload-node     # shows the TLS fingerprint
```

See [docs/backends.md](docs/backends.md) for a plain Linux box, E2B, microsandbox, SSH tunnels and
sizing.

**3. Point the CLI at it and run something:**

```sh
offload init --node https://HOST:7070 --token TOKEN --fingerprint FINGERPRINT
offload doctor
cd your-repo
offload test                       # detects cargo / go / npm / pnpm / yarn / pytest / make
offload run -- go test ./pkg/...   # or any command
```

**4. Hook up your agent** (Claude Code):

```sh
cd your-repo && offload init --claude
```

This adds a `PreToolUse` hook to `.claude/settings.local.json`. The hook rewrites Bash commands
that consist only of test or build steps (`cargo test`, `go test`, `npm test`, `pytest`, `make`, …)
to `offload run -- '<cmd>'`. Compound commands with other side effects stay local. For other agents,
tell them to run tests via `offload run -- <cmd>`, or wrap their test command; native hooks for
Codex, OpenCode and Cursor are on the roadmap.

## How it works

1. **Snapshot (local, ~100 ms).** The worktree is written into a *private* git index
   (`.git/offload-index`), so your own staging area is untouched. That covers tracked and untracked
   files, minus ignored ones and minus untracked secrets (below). It's committed as a snapshot whose
   parent is the last snapshot this node received.
2. **Sync (one request).** A git bundle of `snapshot ^last` carries only the changed objects. If the
   node has lost the base, it answers 409 and the client resends the full tree (never your history).
3. **Warm workspace.** The node keeps workspaces per repo, with dependencies and build outputs
   intact. Each agent's worktree sticks to its own workspace. New workspaces are reflink clones of
   the warmest one, so they start warm too.
4. **Checkout.** `git checkout` rewrites only the files that changed, so unchanged files keep their
   mtimes and incremental builds (cargo, go, tsc) stay incremental. Ignored files such as
   `target/` and `node_modules/` are kept.
5. **Exec and stream.** Output streams back live, and the exit code and Ctrl-C work as they do
   locally. Remote paths are rewritten to local ones, so `file:line` links still open in your
   editor. Each workspace gets a private `TMPDIR`.
6. **Never block the agent.** If the node can't be reached, the command runs locally and the footer
   says why. Set `"mode": "remote"` to make that an error instead.

`offload run --json` prints a token-budgeted summary (pass/fail counts and the first failures)
for go test, cargo, vitest, jest, mocha and pytest, which is useful for agents that would rather
not read 3,000 lines of test output.

## Commands

| Command | What it does |
| --- | --- |
| `offload run [--local] [--json] [--env K=V] -- <cmd>` | Run a command on the node (or locally with `--local`) |
| `offload test` | Detect the repo's test command and run it |
| `offload sync` / `offload watch` | Push the worktree ahead of time, once or continuously |
| `offload pool N` | Keep N warm workspaces for this repo (one per concurrent agent) |
| `offload doctor` | Check the connection and show the repo and worktree ids |
| `offload init …` | Write `~/.offload/config.json`; `--claude` installs the Claude Code hook |
| `offload node …` | Run a node (Linux) |

## Configuration

`~/.offload/config.json`, written by `offload init`. Environment variables
`OFFLOAD_NODE`, `OFFLOAD_TOKEN`, `OFFLOAD_FINGERPRINT` and `OFFLOAD_MODE` override it.

```json
{
  "node": "https://HOST:7070",
  "token": "…",
  "fingerprint": "sha256 of the node's certificate; omit when the node has a real certificate",
  "mode": "auto",
  "headers": { "e2b-traffic-access-token": "…" },
  "sync_exclude": ["**/.env", "**/*.pem"]
}
```

- `mode`: `auto` (remote, falling back to local), `remote` or `local`.
- `headers`: extra request headers, for nodes behind an authenticating proxy.
- `sync_exclude`: replaces the default list of secret patterns; `[]` turns it off.

## Security

- **Secrets stay local.** Untracked files that look like secrets are never uploaded, even if
  they're not gitignored: `.env*`, `.envrc`, `.netrc`, `.npmrc`, private keys (`*.pem`, `*.key`,
  `id_rsa*`, …), `.aws/`, `.ssh/` and Terraform state. Files that git tracks are synced, because
  they're already part of your repo, and tests often need committed fixtures such as
  `testdata/server.key`.
- **Environment variables aren't forwarded.** Pass what tests need with `--env K=V`.
- **The node runs arbitrary code for whoever holds its token.** Use a long random token and either
  pin the node's self-signed certificate (the default) or put it behind HTTPS. Run the node for
  yourself or a trusted team, inside a sandbox if the code isn't trusted. See
  [docs/backends.md](docs/backends.md#bring-your-own-node).
- Please report security issues privately via GitHub security advisories rather than in public
  issues.

## Limitations and roadmap

Known gaps today:

- **No per-run isolation.** Runs on a node share its filesystem and CPUs. Concurrent CPU-hungry
  suites (vitest starts one worker per core) can make timing-sensitive tests flaky.
- **Warm disk, not warm memory.** Long-running services (databases, watchers) aren't snapshotted,
  and there's no service orchestration (Postgres, Redis) yet.
- **Single node.** There's no scheduler, garbage collection of old snapshots or workspace eviction.
- **Submodules** aren't synced (the gitlink is sent, not the content).
- **Linux nodes only.**

Next, roughly in order:

- [ ] Per-run CPU caps and queueing
- [ ] Per-run microVM forks from a warm snapshot (E2B / microsandbox fork APIs)
- [ ] Hooks for Codex, OpenCode and Cursor; an MCP server; a "verify before done" stop hook
- [ ] A local daemon with fsmonitor, to take the ~100 ms snapshot off the critical path
- [ ] Workspace and snapshot garbage collection
- [ ] Services (Postgres, Redis) and test-impact analysis
- [ ] A larger public benchmark (10+ repos, newer laptops)

## Repository layout

| Path | What |
| --- | --- |
| `cmd/offload` | The single binary: CLI and node |
| `internal/client` | Snapshots, incremental git-bundle sync, streaming, local fallback |
| `internal/node` | Bundle ingest, warm workspace pool, streaming exec |
| `internal/report` | Test output parsers and token-budgeted summaries |
| `internal/hook` | The Claude Code `PreToolUse` hook |
| `cmd/offload-bench`, `bench/` | The edit → verify benchmark and its results |
| `Dockerfile`, `deploy/e2b/` | The node image and the E2B template |
| `infra/` | AWS EC2 scripts used for the benchmark node |

## Contributing

Issues and pull requests are welcome. It's early, so open an issue before starting anything large.
Run `go vet ./... && go test ./...` before sending a change. Benchmark results from your own hardware
(`offload-bench`, see [docs/phase0-results.md](docs/phase0-results.md#reproduce)) are especially
useful.

## License

[Apache-2.0](LICENSE)
