# Bring your own node

Offload has two halves: the `offload` CLI on your laptop and an **Offload node** that runs the
commands. The node is the same `offload` binary (`offload node`) on any Linux machine that has your
repos' toolchains. There's no hosted service. You choose where the node runs and how it's isolated.

| Where | Isolation | Warm state survives | Good for | Status |
| --- | --- | --- | --- | --- |
| [Linux box](#a-linux-box) (desktop, Hetzner, EC2) | None: commands run as the node's user | Yes, on disk | Your own machine, fastest | Benchmarked |
| [Docker](#docker) | Container | Yes, in a volume | Trying it out, a shared box | Tested |
| [E2B](#e2b) (E2B Cloud or self-hosted E2B Runtime) | Firecracker microVM | While the sandbox runs or is paused | Running untrusted agent code on someone else's hardware | Recipe typechecked, not yet run end to end |
| [microsandbox](#microsandbox) | libkrun microVM | Yes, in a named volume | A spare Linux box or Apple Silicon Mac with VM isolation | Recipe only, not yet run end to end |

> **Security model.** The node runs whatever command the client sends, so anyone with the token can
> run code on it. Treat the token like an SSH key. One node serves one developer or one trusted team.
> All repos on a node share one filesystem, and runs aren't isolated from each other, even inside a
> sandbox: the sandbox protects the *host* from the code under test, not one run from another.

Whatever the backend, size it for your tests. The speed-up comes from cores and memory your laptop
doesn't have: the [Phase 0 benchmark](phase0-results.md) used 32 vCPUs.

## The node image

The repository's `Dockerfile` builds an image with the `offload` binary and Go, Node.js (with pnpm),
Rust and Python 3. The image is the basis for Docker, E2B and microsandbox alike. Edit it to match
your repos (add a JDK, Ruby or system libraries your tests need), or skip toolchains with empty build
args:

```sh
docker build -t offload-node .
docker build -t offload-node --build-arg RUST_VERSION= --build-arg NODE_VERSION= .   # Go only
```

E2B and microsandbox pull images from a registry, so push it somewhere they can reach:

```sh
docker tag offload-node ghcr.io/YOU/offload-node:latest && docker push ghcr.io/YOU/offload-node:latest
```

The node keeps everything under `--root` (default `/data/offload`):

| Path | What |
| --- | --- |
| `repos/<id>.git` | object store per repo |
| `ws/<id>/<n>` | warm workspaces, with `node_modules/`, `target/` and the like |
| `token`, `tls.crt`, `tls.key` | the API token and the self-signed certificate |

On XFS or btrfs, new workspaces are reflink copies and start warm almost instantly. Elsewhere they're
plain copies.

## A Linux box

```sh
# on the node, with your toolchains installed
offload node --listen :7070 --root /data/offload
# logs: "token in /data/offload/token" and "fingerprint <sha256>"

# on the laptop
offload init --node https://HOST:7070 --token "$(ssh HOST cat /data/offload/token)" --fingerprint <sha256>
offload doctor
```

The node generates a self-signed certificate, and the client pins it by fingerprint, so no CA or
domain is needed. Only open port 7070 to your own IP, or keep it closed and use an SSH tunnel:

```sh
offload node --listen 127.0.0.1:7070 --plain-http         # on the node
ssh -N -L 7070:127.0.0.1:7070 HOST &                       # on the laptop
offload init --node http://127.0.0.1:7070 --token …
```

`infra/` has the scripts used for the benchmark node on AWS EC2. They launch an instance, format an
XFS data volume and install a systemd unit. There are also stop/start scripts, because a stopped
instance keeps its warm caches.

## Docker

```sh
docker volume create offload-data
docker run -d --name offload-node --restart unless-stopped \
  -p 7070:7070 -v offload-data:/data/offload \
  -e OFFLOAD_NODE_TOKEN="$(openssl rand -hex 24)" offload-node
docker logs offload-node            # prints the fingerprint
offload init --node https://HOST:7070 --token <the token> --fingerprint <fingerprint>
```

`--cpus` and `--memory` cap the container. Without them it can use the whole host.

## E2B

[E2B](https://e2b.dev) runs each sandbox as a Firecracker microVM, either on E2B Cloud or on your own
infrastructure with the open-source [E2B Runtime](https://github.com/e2b-dev/runtime). Each sandbox
gets an HTTPS URL per port (`https://7070-<sandbox-id>.e2b.app`). E2B terminates TLS, so the node
serves plain HTTP inside the sandbox and the client verifies E2B's certificate normally.

`deploy/e2b/` has a template and a connect script:

```sh
cd deploy/e2b && npm install
export E2B_API_KEY=…                                   # plus E2B_DOMAIN for a self-hosted runtime
OFFLOAD_NODE_IMAGE=ghcr.io/YOU/offload-node:latest OFFLOAD_CPUS=8 OFFLOAD_MEMORY_MB=8192 npx tsx build.ts
npx tsx connect.ts                                     # prints an `offload init …` line
```

`connect.ts` creates a sandbox from the template, starts `offload node --plain-http` with a fresh
random token, waits for it to become healthy and prints the `offload init` command. Run
`npx tsx connect.ts <sandbox-id>` to reconnect to the same sandbox, which keeps its workspaces warm.
Use E2B's pause/resume to keep a node's warm state without paying for a running sandbox.

Notes:
- The node isn't started by a template start command. E2B snapshots the start command at build
  time, so a token generated there would be shared by every sandbox made from the template.
- E2B sandbox URLs are public by default, so the bearer token is what protects the node. If you
  create sandboxes with restricted public access, add E2B's token header on the client:
  `offload init … --header e2b-traffic-access-token=<token>`.
- Sandbox CPU and memory are set when the template is built, and plan limits apply.

## microsandbox

[microsandbox](https://github.com/superradcompany/microsandbox) runs OCI images as libkrun microVMs on
Linux (KVM) and Apple Silicon Macs. This makes a spare Mac mini or Linux desktop a node with VM
isolation:

```sh
TOKEN=$(openssl rand -hex 24)
msb run -d --name offload-node -c 8 -m 16G \
  -p 0.0.0.0:7070:7070 -v offload-data:/data/offload \
  -e OFFLOAD_NODE_TOKEN="$TOKEN" ghcr.io/YOU/offload-node:latest
msb logs offload-node               # prints the fingerprint
offload init --node https://HOST:7070 --token "$TOKEN" --fingerprint <fingerprint>
```

`msb stop offload-node` / `msb start offload-node` keep the volume, so workspaces stay warm. The flags
follow microsandbox's CLI reference. microsandbox is beta software, so check `msb run --tree` if a
flag has changed. Ports bind to `127.0.0.1` unless you give an address, as above.

## Writing another backend

Anything that can run a Linux container image and expose one HTTPS or HTTP port can host a node:
Fly Machines, Modal, a Kubernetes pod, a Firecracker VM you manage yourself. The node needs:

- `git` and your toolchains on `PATH`
- a writable `--root`, which should persist if you want warm workspaces between sessions
- `OFFLOAD_NODE_TOKEN` (or `--token`) set to a secret
- `--plain-http` if the platform terminates TLS. If it doesn't, keep the node's own TLS and pass the
  fingerprint to `offload init`.

Per-run isolation would mean a fresh microVM fork per command, restored from a warm snapshot. It's
the obvious next step, and E2B's and microsandbox's fork/snapshot APIs are designed for it. It isn't
built yet.
