# ephemeris

Disposable, locked-down sandboxes for code you don't fully trust. One command spins one up with your repo inside, and when you exit it's incinerated, with a signed receipt to prove it.

```console
$ cd ~/code/some-repo
$ ephemeris shell
level=INFO msg="sandbox ready" session=7e6ecaea9d31 backend=docker took=464ms
/workspace # ls        # your repo, including uncommitted edits
/workspace # exit
level=INFO msg=incinerated session=7e6ecaea9d31 receipt=~/.local/state/ephemeris/receipts/7e6ecaea9d31.json
```

No cloud account, no Kubernetes, no daemon. A single static Go binary that talks to Docker or Podman.

## Why

"Works on my machine" and "I just ran a stranger's `npm install` on my laptop" are the same problem: the code and your machine share too much. Ephemeris gives every run a fresh box that shares as little as possible:

- **No network by default.** Turn on an HTTPS allowlist when you need `proxy.golang.org` or `registry.npmjs.org`, and nothing else gets out. The proxy checks the IP it actually dials, so DNS tricks pointing at `127.0.0.1` or your cloud metadata endpoint don't work.
- **Secrets are files, not env vars.** They land in `/run/secrets/NAME` on a RAM-only tmpfs, mode 0600. They don't show up in `env`, `/proc/*/environ`, crash dumps or child processes.
- **Nothing hits disk.** Read-only root filesystem. `/workspace`, `/tmp` and `/run/secrets` are tmpfs.
- **Least privilege.** All capabilities dropped, `no-new-privileges`, pids/cpu/memory limits, an init process, and rootless Podman when you have it.
- **Guaranteed teardown.** Normal exit, error, Ctrl-C or closed terminal: destroyed. CLI killed with `kill -9`: the next run sees its stale lease and reaps it. Forgot it running: the TTL kills it. Every teardown is checked again with the engine, then written to an ed25519-signed receipt.

## Install

You need Go 1.26+ to build from source, and Docker or Podman to run sandboxes.

```bash
git clone https://github.com/angie-kinya/ephemeris && cd ephemeris
make install                 # static binary into $(go env GOPATH)/bin
ephemeris doctor             # tells you what's missing and how to fix it
```

Or grab a prebuilt binary for Linux or macOS (amd64/arm64) from [Releases](https://github.com/angie-kinya/ephemeris/releases).

> Installing with plain `go install`? Use `CGO_ENABLED=0 go install github.com/angie-kinya/ephemeris/cmd/ephemeris@latest`. Allowlist mode mounts the binary into a Linux container, so it has to be static.

### Linux + Podman (recommended)

```bash
systemctl --user enable --now podman.socket
export DOCKER_HOST=unix://$XDG_RUNTIME_DIR/podman/podman.sock   # add to your shell rc
```

### Linux + Docker

Works out of the box. Rootless Docker is strongly preferred: a rootful Docker socket is root on your machine.

### macOS

Any Docker-compatible engine works (Docker Desktop, OrbStack, Colima, Podman machine). Allowlist mode needs a Linux build of the proxy, published as an image:

```bash
export EPHEMERIS_PROXY_IMAGE=ghcr.io/angie-kinya/ephemeris:latest
```

The `process` backend is Linux-only (it uses namespaces via bubblewrap).

## Usage

```bash
ephemeris shell                          # interactive shell, gone on exit
ephemeris run -- make test               # one command; exit code passes through
cat data.csv | ephemeris run -- wc -l    # stdin works when it's piped
ephemeris ls                             # live sandboxes and time left
ephemeris reap                           # clean up after crashes (runs automatically too)
ephemeris verify ~/.local/state/ephemeris/receipts/<id>.json
ephemeris doctor
```

With no spec file, you get Alpine, no network, 1 CPU, 1 GiB RAM, a 30-minute TTL, and the current git repo shipped in. Drop an `ephemeris.yaml` in your repo to change any of that.

## The spec

```yaml
name: my-service
backend: docker                  # docker | process
image: docker.io/library/golang:1.26-alpine
shell: [/bin/sh]
ttl: 45m                         # hard limit, no renewals
resources: {cpu: 2, memory: 2GiB, pids: 512, disk: 1GiB}
env:
  GOPATH: /tmp/go
  GOCACHE: /tmp/go-build
source: {path: .}                # or {git: https://github.com/org/repo, ref: pull/42/head}
secrets:
  - {name: STRIPE_TEST_KEY, from: env:STRIPE_TEST_KEY}   # -> /run/secrets/STRIPE_TEST_KEY
  - {name: DB_PASSWORD, from: file:/home/me/.secrets/db}
network:
  mode: allowlist                # none (default) | open | allowlist
  allow: [proxy.golang.org, sum.golang.org, "*.githubusercontent.com"]
```

Need to open a dev server in your browser? Use `mode: open` with `ports: [3000]`. Ports are only published on `127.0.0.1`, never your LAN.

Unknown fields are errors, so a typo like `secrests:` fails loudly instead of silently doing nothing.

### What gets shipped

Ephemeris pipes `git archive` into the sandbox, so it ships **tracked files plus your uncommitted edits to them**. Untracked files (`.env`, `node_modules`, `.venv`) stay home. The sandbox doesn't need git or network for this.

## For AI agents (MCP)

Give your coding agent a sandbox it can't escape and can't leave behind:

```bash
claude mcp add ephemeris -- ephemeris mcp
```

Tools: `sandbox_create`, `sandbox_exec`, `sandbox_destroy` (returns the signed receipt), `sandbox_list`. At most 4 sandboxes at once by default (`--max-sessions`), and all of them are destroyed when the server exits. Agents can't choose their own spec file unless you start the server with `--allow-spec-path`.

Other MCP clients (`.mcp.json`):

```json
{"mcpServers": {"ephemeris": {"command": "ephemeris", "args": ["mcp"]}}}
```

## How it works

```
reap leftovers -> take lease -> create -> start -> provision (parallel) -> exec/shell -> destroy -> verify gone -> signed receipt
```

| Package | Job |
|---|---|
| `internal/spec` | parse and validate `ephemeris.yaml` |
| `internal/backend/docker` | Docker Engine API (works with Podman); the egress proxy wiring |
| `internal/backend/process` | bubblewrap namespaces + a PTY we own (`creack/pty`) |
| `internal/orchestrator` | session state machine, parallel provisioning with `errgroup` |
| `internal/provision` | ships source and secrets in over exec stdin |
| `internal/lease`, `reaper`, `receipt` | crash-proof teardown with proof |
| `internal/netpolicy` | CONNECT-only HTTPS proxy, judges dialled IPs |
| `internal/mcpserver` | MCP tools for agents |
| `internal/hub`, `internal/pool` | terminal fan-out and warm pool (groundwork for the daemon) |

## Threat model, honestly

Ephemeris is strong defaults, not a VM. Containers share the host kernel, so a kernel exploit still wins; install gVisor and set `runtime: runsc` for a stronger boundary. Anything the sandbox legitimately reaches through the allowlist, it can also leak to. Disk-backed swap can keep tmpfs pages around (`ephemeris doctor` warns about it). Please report security issues privately; see [SECURITY.md](SECURITY.md).

## Develop

```bash
make test-short   # unit tests, no engine needed
make test         # + integration tests against Docker/Podman
make fuzz         # spec sizes and the egress allowlist
make chaos        # kill -9 the CLI mid-session, prove the reaper cleans up
make lint
```

The repo dogfoods itself: `ephemeris run -- go test ./...` runs the test suite inside a sandbox, using the `ephemeris.yaml` in the root.

## Roadmap

- `ephemerisd`: a daemon that owns leases and keeps a warm pool for near-instant spawns
- SSH access with short-lived certificates, read-only spectators, asciinema recording
- Postgres provisioner: restore a dump and scrub it before the sandbox sees it
- Firecracker microVM backend

## License

[MIT](LICENSE)
