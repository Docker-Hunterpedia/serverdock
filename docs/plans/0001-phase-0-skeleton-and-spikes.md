# Plan 0001 — Phase 0: skeleton, CI, bench, and two spikes

**Status:** draft · **Author:** planner (AI) · **Date:** 2026-11-20
**Roadmap:** Phase 0 — repo skeleton + CI, bench harness, Spike 1, Spike 2. Exit check: single node ≤ 80 MB idle, measured.

## Goal

Prove the footprint promise with real numbers before building features:

1. A Go repo skeleton and CI that every later PR builds on.
2. A repeatable bench that measures idle RSS/CPU in a 1 vCPU / 1 GB Linux container.
3. **Spike 1:** one binary in `server` mode with embedded Caddy, SQLite and an embedded placeholder SPA, idle under budget.
4. **Spike 2:** a separate `agent` process that dials out to the server over WebSocket + yamux, survives reconnects, and streams container logs on demand.

## Out of scope

UI beyond a placeholder page; auth/users; mTLS and enrollment (Spike 2 uses a shared token); deploys; Litestream; Compose; metrics collection; production hardening. Spike code may be rewritten in Phase 1, but the skeleton, CI and bench are meant to last.

## Design

### Repo layout (from architecture.md)

```
cmd/serverdock/main.go       subcommands: server | agent | version  (stdlib flag, no CLI framework)
internal/server/             HTTP API (/healthz, /debug/mem), SPA serving, agent hub endpoint
internal/proxy/              embedded Caddy: build JSON config, load in-process
internal/store/              SQLite open (WAL, busy_timeout, single writer), migrations stub
internal/agent/              dial-out client, reconnect/backoff, stream handlers
internal/hub/                server side of agent connections (yamux sessions)
internal/dockerapi/          minimal Docker Engine API client over the unix socket (logs only, for now)
internal/memstat/            read VmRSS/VmHWM from /proc/self/status; Go runtime stats
web/dist/index.html          placeholder SPA, embedded with embed.FS
bench/                       Dockerfile, run.sh, results/
.github/workflows/ci.yml
```

Module: `github.com/Docker-Hunterpedia/serverdock`. Go 1.27, `CGO_ENABLED=0`.

### Dependencies (keep this list short)

| Dependency | Why |
|---|---|
| `github.com/caddyserver/caddy/v2` (+ `modules/caddyhttp`, `reverseproxy`, `caddytls`) | Embedded proxy (ADR 0004). Import only needed modules, not `standard`. |
| `modernc.org/sqlite` | Pure-Go SQLite (ADR 0002). |
| `github.com/coder/websocket` | Small WebSocket library for the agent channel. |
| `github.com/hashicorp/yamux` | Stream multiplexing over one connection (ADR 0003). |

No Docker SDK: `internal/dockerapi` talks HTTP over `/var/run/docker.sock` for the one call Spike 2 needs (`GET /containers/{id}/logs?follow=1&stdout=1&stderr=1`). Decide on moby client vs. thin client later with measurements.

### Spike 1 — `serverdock server`

- Flags: `--data DIR` (default `/var/lib/serverdock`), `--listen :80`, `--internal 127.0.0.1:7070`, `--proxy=true|false`.
- Starts the internal HTTP server: `/` serves the embedded SPA, `/healthz` → `ok`, `/debug/mem` → JSON `{rss_kb, hwm_kb, heap_alloc, sys, goroutines, num_gc}`.
- Opens SQLite at `DATA/serverdock.db` with WAL, `busy_timeout=5000`, a single write connection and a small read pool; `cache_size` capped (−2000 = ~2 MB); creates a `meta` table.
- With `--proxy`, loads a Caddy JSON config in-process: HTTP server on `--listen` with one route reverse-proxying to `--internal`. No TLS in the spike (Phase 1 adds ACME). Caddy admin endpoint disabled (`"admin": {"disabled": true}`).
- Sets `debug.SetMemoryLimit(64 MiB)` unless `GOMEMLIMIT` is set.
- Graceful shutdown on SIGTERM.

### Spike 2 — `serverdock agent` + hub

- Server: `GET /agent/connect` upgrades to WebSocket; checks `Authorization: Bearer <SD_AGENT_TOKEN>` (spike only); wraps the conn as `net.Conn` and starts a yamux **client** session (the server opens streams; the agent accepts).
- Agent: `serverdock agent --server ws://host:7070/agent/connect --token …` dials out, starts a yamux **server** session, accepts streams. Each stream starts with one JSON line `{"op":"logs","container":"<id>","tail":100}`; the agent streams Docker logs (demuxed) until the stream closes, then cancels the Docker request (no orphaned requests: lesson from Dokploy #5872).
- Reconnect: exponential backoff 1 s → 30 s with full jitter; reset after 60 s connected; heartbeat via yamux keepalive (30 s).
- Server debug endpoint for the spike: `GET /debug/agents` lists connected agents; `GET /debug/agents/{id}/logs/{container}` opens a stream and proxies the log lines (to test end to end).

### Bench — `bench/run.sh`

- Builds a Linux image (multi-stage: `golang:1.27` build → `gcr.io/distroless/static` runtime) for the host arch.
- Scenarios, each in a container with `--memory 1g --cpus 1`:
  1. `server --proxy=false`
  2. `server --proxy=true` (the single-node number that matters)
  3. `agent` connected to scenario 2's server, idle
- For each: start, wait 30 s warm-up, then sample `/debug/mem` (RSS) and `docker stats --no-stream` (CPU %, cgroup memory) every 5 s for 60 s. Report median and max.
- Writes `bench/results/<date>-<arch>.md` and `.json`. Exit code 1 if scenario 2 RSS median > 80 MB or agent > 25 MB.

### CI — `.github/workflows/ci.yml`

- `lint-test`: `go vet ./...`, `go test -race ./...`, `govulncheck ./...`, `gofmt -l` must be empty.
- `build`: matrix `linux/amd64`, `linux/arm64`, upload binaries as artifacts; print binary size.
- `bench`: runs `bench/run.sh` on `ubuntu-latest`, uploads results, fails on budget breach.
- Pin actions by commit SHA; `permissions: contents: read`.

## Footprint budget

| Thing | Budget |
|---|---|
| server, no proxy, idle RSS | ≤ 40 MB |
| server + embedded Caddy, idle RSS | ≤ 80 MB (target 45–70) |
| agent idle RSS | ≤ 20 MB (hard fail at 25) |
| idle CPU (each) | < 1% averaged over 60 s |
| server binary size | ≤ 40 MB (report; not a CI fail in Phase 0) |

If scenario 2 exceeds 80 MB: measure Caddy's share, try trimming modules, then fall back to the Caddy-container option in ADR 0004 and record the decision.

## Security notes

- Spike token is a shared secret from an env var; never logged. Marked clearly as spike-only; replaced by enrollment + mTLS in Phase 2 (ADR 0003).
- `/debug/*` endpoints bind only to the internal listener and are compiled in behind a `debug` build tag or a `--debug` flag that defaults off outside the bench.
- Docker socket access only in the agent; the server in Spike 1 does not touch Docker.
- The Docker log request uses fixed paths and a validated container ID (`^[a-zA-Z0-9][a-zA-Z0-9_.-]{0,127}$`); no shell.
- CI uses least-privilege tokens and SHA-pinned actions.

## Steps (each one PR-sized)

1. **Skeleton:** go.mod, `cmd/serverdock` with `version` subcommand, `internal/memstat` + tests, Makefile (`build`, `test`, `lint`), `.golangci.yml` optional. CI `lint-test` + `build`.
2. **Store:** `internal/store` with WAL settings + tests (open, write, concurrent read).
3. **Server + SPA:** `internal/server`, embedded `web/dist/index.html`, `/healthz`, `/debug/mem`, graceful shutdown + tests (httptest).
4. **Embedded Caddy:** `internal/proxy` JSON config builder + in-process load; integration test that `GET /` via the proxy port returns the SPA.
5. **Bench:** Dockerfile, `run.sh`, scenarios 1–2, results file; CI `bench` job.
6. **Hub + agent:** `internal/hub`, `internal/agent`, backoff with tests (fake clock), loopback integration test (server and agent in one test process, fake log source).
7. **Docker logs:** `internal/dockerapi` logs call with demux + context cancellation; integration test behind `//go:build integration` using a real `busybox` container; bench scenario 3.
8. **Record results:** `docs/research/05-phase0-measurements.md` with the numbers and the ADR 0004 verdict; update roadmap checkboxes.

## Test plan

- Unit: memstat parsing, store pragmas, Caddy config builder, backoff schedule, op-line parsing, container ID validation.
- Integration: proxy → SPA round trip; agent ↔ hub loopback including forced disconnect + reconnect; real Docker logs stream (tagged), verifying cancellation closes the Docker request.
- Bench in CI on every PR to `main` touching Go code.
- Manual: run `bench/run.sh` locally on arm64 (Apple Silicon Docker) and on a real 1 GB VPS before declaring Phase 0 done.

## Reviews required

- `reviewer`: this plan, then each step's diff.
- `systems`: reconnect/backoff and stream cancellation (step 6–7).
- `security`: agent channel and debug endpoints (step 6).
- `footprint`: steps 4, 5, 7 and the final numbers.
