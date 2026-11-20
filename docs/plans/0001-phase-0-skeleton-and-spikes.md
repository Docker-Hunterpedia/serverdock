# Plan 0001 — Phase 0: skeleton, CI, bench, and two spikes

**Status:** APPROVED by reviewer (round 2) · **Author:** planner (AI) · **Date:** 2026-11-20

**Implementation notes from review:**
- The agent image runs as `nonroot` (uid 65532) and cannot open `/var/run/docker.sock` by default. Bench scenarios 3–4 and the busybox integration test must run the agent with `--group-add <docker.sock gid>` (or as root in tests). Document this in `bench/run.sh`.
- `--debug-listen` is used only during the burst phase. No debug requests are sent during the 5-minute idle window.
- Measurement must also work on Docker Desktop (macOS), where host `/proc` can't see container PIDs. In that case `measure.sh` reads `/proc/<pid>` and cgroup files from a helper container (`docker run --rm --pid=host --privileged alpine …`). Results from this path are informational only.
**Roadmap:** Phase 0 — repo skeleton + CI, bench harness, Spike 1, Spike 2. Exit check: single node ≤ 80 MB idle, measured.

## Goal

Prove the footprint promise with real numbers before building features:

1. A Go repo skeleton and CI that every later PR builds on.
2. A repeatable bench that measures idle RSS/CPU **from outside the processes** in a 1 vCPU / 1 GB Linux container.
3. **Spike 1:** one binary in `server` mode = control plane + **in-process local agent** + embedded Caddy (owned by the agent runtime) + SQLite + embedded placeholder SPA, idle under budget.
4. **Spike 2:** a separate `agent` process (no listening ports) that dials out to the server through the proxy over WebSocket + yamux, survives reconnects, and streams container logs on demand with clean cancellation.

## Out of scope

UI beyond a placeholder page; auth/users; mTLS and enrollment (Spike 2 uses a shared token); deploys; Litestream; Compose; metrics collection; TLS/ACME (Phase 1). Spike code may be rewritten in Phase 1; the skeleton, CI and bench are meant to last.

## Design

### Repo layout

```
cmd/serverdock/main.go       subcommands: server | agent | version  (stdlib flag)
internal/api/                control-plane HTTP: /healthz, SPA serving, debug handlers
internal/hub/                server side of agent connections (WebSocket + yamux)
internal/agent/              agent runtime: dial-out client, backoff, stream handlers;
                             also used in-process by `server` as the local agent
internal/agent/proxy/        embedded Caddy, owned by the agent runtime (ADR 0004)
internal/agent/dockerapi/    minimal Docker Engine API client over the unix socket (logs only)
internal/store/              SQLite (WAL) open + migrations stub
internal/memstat/            /proc/self/status + Go runtime stats (convenience only)
web/dist/index.html          placeholder SPA (embed.FS)
bench/                       Dockerfile, run.sh, measure.sh, results/
.github/workflows/ci.yml
```

`docs/architecture.md` "Repository layout" is updated in the same PR to match this.

Module `github.com/Docker-Hunterpedia/serverdock`, Go 1.27, `CGO_ENABLED=0`.

### Dependencies

| Dependency | Why |
|---|---|
| `github.com/caddyserver/caddy/v2` (`caddyhttp`, `reverseproxy` only; not `standard`) | Embedded proxy (ADR 0004) |
| `modernc.org/sqlite` | Pure-Go SQLite (ADR 0002) |
| `github.com/coder/websocket` | Agent channel |
| `github.com/hashicorp/yamux` | Stream multiplexing (ADR 0003) |

No Docker SDK in Phase 0; `dockerapi` speaks HTTP over `/var/run/docker.sock`.

### Process and network shape

- **Public listener** `--listen :80` is Caddy (inside the local agent runtime). Routes:
  - `/agent/connect` → reverse proxy (WebSocket upgrade passes through) to the control plane;
  - everything else → control plane.
- **Control plane** listens on `--internal 127.0.0.1:7070` (loopback only; reached via Caddy).
- **Debug listener** `--debug-listen ADDR` (default **off**) serves `/debug/mem` and the spike's `/debug/agents…` endpoints. Runtime flag, not a build tag, so the bench measures the binary that ships.
- **Remote agent** has **no listening ports**. It dials `ws://<server>/agent/connect`. Plain `ws://` is acceptable only on loopback or the bench's Docker network; Phase 1 adds TLS, Phase 2 mTLS.

### Spike 1 — `serverdock server`

- Flags: `--data DIR` (default `/var/lib/serverdock`), `--listen :80`, `--internal 127.0.0.1:7070`, `--proxy=true|false`, `--debug-listen` (off).
- Starts the control plane (`internal/api`), then the **in-process local agent** (`internal/agent`, without dial-out: it registers with the hub through an in-memory pipe so the same code path is exercised), and the local agent starts Caddy when `--proxy`.
- **SQLite** via DSN pragmas so every pooled connection gets them:
  `file:DATA/serverdock.db?_pragma=journal_mode(WAL)&_pragma=busy_timeout(5000)&_pragma=synchronous(NORMAL)&_pragma=foreign_keys(1)&_txlock=immediate`.
  Writer `*sql.DB` with `MaxOpenConns(1)`; reader `*sql.DB` with a small pool (2) and without `_txlock`. `cache_size` left at the default (−2000 ≈ 2 MB per connection; noted, not tuned). Creates a `meta` table.
- **Caddy** JSON config, loaded in-process with `caddy.Load`:
  - `"admin": {"disabled": true}`;
  - `"storage": {"module": "file_system", "root": "DATA/caddy"}` (distroless has no usable HOME);
  - HTTP server on `--listen` with `"automatic_https": {"disable": true}` (no redirects; TLS comes in Phase 1);
  - test that a **second `caddy.Load`** with a changed route works in-process with admin disabled. If it does not, record it and switch to keeping a private admin endpoint on a unix socket under `DATA/` (Phase 1 depends on live reloads).
- `debug.SetMemoryLimit(64 MiB)` unless `GOMEMLIMIT` is set. Graceful shutdown on SIGTERM.

### Spike 2 — `serverdock agent` + hub

- **Hub** (`GET /agent/connect`): checks `Authorization: Bearer <SD_AGENT_TOKEN>` with `subtle.ConstantTimeCompare` (spike only; never logged); accepts the WebSocket; wraps it with `websocket.NetConn(sessionCtx, conn, websocket.MessageBinary)`. **The handler blocks for the whole session** (the NetConn's ctx bounds its lifetime). It starts a yamux **client** session (the server opens streams). Note: `NetConn` disables the read limit; yamux framing bounds message sizes.
- **Agent** dials with a dial-timeout ctx only for the handshake, then wraps the conn with a separate **session ctx** that lives until the connection ends. It starts a yamux **server** session and accepts streams.
- **Stream protocol:** first line JSON `{"op":"logs","container":"<id>","tail":100}`. Container ID validated `^[a-zA-Z0-9][a-zA-Z0-9_.-]{0,127}$`; `tail` bounded to 1..5000.
- **Cancellation (yamux half-close):** server `stream.Close()` only sends FIN. The agent runs a **reader goroutine** on every stream; on EOF/error it cancels the Docker request's context. Every write uses a **write deadline** (10 s); a failed or timed-out write also cancels. Test: a server-side close cancels the Docker request within 2 s.
- **Docker logs:** `GET /containers/{id}/json` → if `Config.Tty` is false demux the 8-byte-header stream, else copy raw. Then `GET /containers/{id}/logs?follow=1&stdout=1&stderr=1&tail=N`. `http.Client` with no Timeout; cancellation only via context.
- **Reconnect:** exponential backoff 1 s → 30 s with full jitter; reset after 60 s connected; yamux keepalive 30 s.
- `debug.SetMemoryLimit(20 MiB)` for the agent unless `GOMEMLIMIT` is set.
- Debug listener (bench only): `GET /debug/agents`, `GET /debug/agents/{id}/logs/{container}` opens a stream and proxies lines.

### Bench — `bench/run.sh` + `bench/measure.sh`

Runs on a Linux host (CI runner or VPS); macOS Docker Desktop results are informational only.

- Image: multi-stage `golang:1.27` → `gcr.io/distroless/static:nonroot` (binding :80 works with Docker's default `ip_unprivileged_port_start=0`).
- Every scenario container: `--memory 1g --cpus 1` (Go ≥ 1.25 honours the cgroup CPU quota for GOMAXPROCS, so per-P memory matches a 1 vCPU VM; kernel overhead and CPU steal are not modelled — hence the VPS run below).
- **Scenarios:**
  1. `server --proxy=false`
  2. `server --proxy=true` (control plane + in-process local agent + Caddy) — the single-node number
  3. remote `agent` connected to scenario 2's server through the proxy
  4. *(report only)* remote `agent` + its own Caddy, to preview the remote-node ≤ 50 MB budget
- **Measured from outside** (no polling of the measured process):
  - PID via `docker inspect -f '{{.State.Pid}}'`; read `/proc/<pid>/status` → **VmRSS (gate)**, VmHWM, RssAnon, RssFile.
  - Container cgroup: `memory.current`, `memory.stat` (anon, file, inactive_file) → report `memory.current − inactive_file`.
  - CPU: delta of cgroup `cpu.stat usage_usec` over the whole idle window ÷ wall time.
- **Procedure per run:** start → 2 min warm-up → **"used" burst** (500 requests through the proxy, plus one log stream opened and closed for scenarios 3/4) → **5 min idle window**, sampling memory every 10 s → report median and max VmRSS, VmHWM, idle CPU %.
- **Noise:** each scenario runs **3 times**; gate on the **median of the run medians**, tolerance +5% on memory, +0.2 points on CPU.
- **Gates** (fail the job):

| Scenario | VmRSS (median) | Idle CPU |
|---|---|---|
| 1 server, no proxy | ≤ 40 MB | < 0.5% |
| 2 single node | ≤ 80 MB | < 1% |
| 3 remote agent | ≤ 20 MB (hard fail 25) | < 0.3% |
| 4 agent + Caddy | report only (target ≤ 50 MB) | report only |
| server binary, gzip -9 | ≤ 40 MB | — |

- Output: `bench/results/<date>-<arch>.md` and `.json`.

### CI — `.github/workflows/ci.yml`

- `lint-test`: `gofmt -l` empty, `go vet ./...`, `go test -race ./...`, `govulncheck` (pinned version).
- `build`: matrix linux/amd64 + linux/arm64; artifacts; report raw and gzip sizes.
- `bench`: `ubuntu-latest` (gate) and `ubuntu-24.04-arm` (report); upload results.
- Actions pinned by commit SHA; `permissions: contents: read`. Integration tests use random free ports.

## Footprint budget

As in the gates table above. If scenario 2 exceeds 80 MB: measure Caddy's share (scenario 2 − scenario 1), try trimming modules, then fall back to the Caddy-container option in ADR 0004 and record the decision.

## Security notes

- Spike token: env var, constant-time compare, never logged; replaced by enrollment + mTLS in Phase 2 (ADR 0003).
- Debug listener off by default; bench enables it on the bench network only.
- Agent has no listening ports; plain `ws://` only on loopback/bench network.
- Docker socket used only by the agent runtime; validated IDs, bounded `tail`, fixed API paths, no shell.
- CI: least-privilege token, SHA-pinned actions.

## Steps (each one PR-sized)

1. **Skeleton:** go.mod, `cmd/serverdock` (`version`), `internal/memstat` + tests, Makefile, CI `lint-test` + `build`; update architecture.md layout.
2. **Store:** DSN pragmas, writer/reader pools, tests (pragmas present on every pooled conn, concurrent reads during a write).
3. **Control plane:** `internal/api`, embedded SPA, `/healthz`, debug listener flag, graceful shutdown; httptest tests.
4. **Local agent + Caddy:** `internal/agent` in-process mode, `internal/agent/proxy` config builder, `caddy.Load`; tests: SPA via proxy, WebSocket passes through `/agent/connect`, **second `caddy.Load` reload**.
5. **Bench:** Dockerfile, `run.sh`, `measure.sh`, scenarios 1–2, gates, results; CI `bench` job.
6. **Hub + remote agent:** WebSocket/NetConn session handling, yamux roles, backoff (fake clock), loopback integration test including forced disconnect and reconnect.
7. **Docker logs:** `dockerapi` (Tty check, demux, ctx cancel), per-stream reader + write deadlines; tests: demux unit test, **server close cancels the Docker request within 2 s**, real `busybox` integration test (`//go:build integration`); bench scenarios 3–4.
8. **Record results and exit:** `docs/research/05-phase0-measurements.md` with CI numbers (amd64 + arm64), plus **one run on a real 1 GB VPS (required exit criterion)**; ADR 0004 verdict; roadmap checkboxes.

## Test plan

- Unit: memstat parsing, DSN/pragmas, Caddy config builder, backoff schedule, op-line parsing, ID/tail validation, log demux.
- Integration: proxy → SPA; WebSocket through proxy; Caddy reload; agent ↔ hub reconnect; stream cancellation; real Docker logs (tagged).
- Bench gates in CI on every PR touching Go code.
- Exit: a real 1 GB VPS bench run recorded in step 8.

## Reviews required

- `reviewer`: this plan, then each step's diff.
- `systems`: steps 6–7 (sessions, reconnect, cancellation).
- `security`: steps 3, 6, 7 (debug listener, agent channel, Docker access).
- `footprint`: steps 4, 5, 7, 8.
