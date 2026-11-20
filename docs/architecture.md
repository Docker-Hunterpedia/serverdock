# ServerDock — Architecture

*Draft 1, 2026-11-20. AI-written from [research/03-architecture-options.md](research/03-architecture-options.md); not yet confirmed by the maintainer. Decisions are recorded in [decisions/](decisions/) (0001–0007 accepted 2026-11-20).*

## Overview

```mermaid
flowchart LR
  subgraph Clients
    B[Browser SPA]
    CLI[serverdock CLI]
    MC[Claude Code / MCP clients]
  end
  subgraph CP["serverdock server — one Go binary"]
    API[REST + WebSocket API · auth · RBAC]
    MCP[MCP endpoint /mcp]
    AI[AI assistant loop · BYO key]
    REG[Action registry · typed ops · risk tiers · dry-run]
    AUD[(Audit log · hash-chained)]
    DB[(SQLite WAL · state · jobs · metrics rollups)]
    HUB[Agent hub · mTLS · multiplexed streams]
    LOC[In-process local agent]
  end
  subgraph Node["App server (1 GB VPS, NAT ok)"]
    AG[serverdock agent · Go]
    CAD[Embedded Caddy · ACME]
    DK[(Docker Engine)]
    APPS[User apps & DBs]
  end
  subgraph Builder["Builder server or CI (optional)"]
    BA[agent · role=builder]
    BK[BuildKit + Railpack]
    RG[(Registry)]
  end
  B --> API
  CLI --> API
  MC --> MCP
  API --> REG
  MCP --> REG
  AI --> REG
  REG --> AUD
  REG --> DB
  REG --> HUB
  REG --> LOC
  AG -- outbound WSS --> HUB
  BA -- outbound WSS --> HUB
  AG --> DK --> APPS
  CAD --> APPS
  BK --> RG
  AG -- pull --> RG
  AI -. HTTPS .-> CL[(Claude API)]
```

## Key decisions

| Area | Decision | Why |
|---|---|---|
| Language | **Go** for control plane, agent and CLI (ADR 0002) | Embeddable compose-go, BuildKit, Railpack, Caddy/certmagic; static cross-compile; infra contributor pool. Rust saves only ~5–10 MB. |
| Shape | **One binary, modes** `server`, `agent`, CLI. Single node runs the agent in-process. | No separate DB/Redis/websocket containers — the biggest RAM win. |
| Database | **SQLite (WAL)**, pure-Go driver, single writer + read pool; jobs table instead of Redis; optional Litestream to S3. Keep a storage interface for a future Postgres backend. | Few MB vs 30–80 MB for Postgres + Redis. Write volume is low. |
| Transport | **Agent dials out** over TLS (WebSocket), multiplexed streams (yamux). One-time enrollment token → Ed25519/mTLS identity. | Works behind NAT/CGNAT, no inbound ports, no SSH keys stored (ADR 0003). |
| Operations | Every agent RPC is a **typed operation** (`ComposeUp{stack,rev}`, `ContainerRestart{id}`), executed via the Docker API. Exec only with argv arrays, never `sh -c`. | Removes the command-injection class behind most Coolify/Dokploy CVEs. |
| Proxy | **Caddy embedded** in the agent, configured with full JSON from desired state; on-demand TLS gated by our DB. Fallback: Caddy container via Unix-socket admin API. | ~20–40 MB vs Traefik ~80–120 MB; atomic reloads; no labels (ADR 0004). |
| Builds | Dockerfile, Compose, **Railpack** on BuildKit. Default: builder server or CI; local builds capped by cgroup. BuildKit runs only during builds. | Builds must never OOM a small app server. Nixpacks deprecated; CNB too heavy. |
| Metrics | Read **cgroup v2 + /proc** directly every 15 s; rollups in SQLite (15 s/24 h → 1 min/7 d → 10 min/30 d → 1 h/1 y). | `docker stats` polling caused Dokploy's CPU storms. |
| Logs | Docker `local` log driver with rotation as the source of truth; stream only while subscribed, always cancel; search with grep on the agent. | No Loki; no orphaned `docker logs` processes. |
| Events | Subscribe to the **Docker events API** for state changes instead of polling. | Idle CPU ≈ 0. |
| Frontend | **SvelteKit static SPA** embedded with `embed.FS`; uPlot charts; xterm.js terminal; REST typed from OpenAPI + one WebSocket. English only. | Small bundle, no server RAM cost (ADR 0005). |
| AI | One action registry shared by UI/REST/CLI/assistant/MCP. Risk tiers `read` / `mutate` / `destructive`; server-enforced approvals; secret redaction; Anthropic Go SDK; zero idle cost. | AI can never exceed the caller's permissions. |

## Core concepts (data model sketch)

- **Server** — a host running an agent (local or remote); roles: `app`, `builder`, `edge`.
- **Project** → **Environment** → **Service** (app from image/git, Compose stack, or database).
- **Deploy** — immutable record: source revision, built image digest, resolved env (encrypted), compose, routes, health result, actor. Rollback = redeploy an earlier Deploy.
- **Domain** — host name → service port, TLS state, diagnostics.
- **Backup** / **BackupRun** — schedule, target, result, restore-test result.
- **Secret** — encrypted value; reveal is an audited action.
- **Action** — registry entry: name, JSON schema, risk tier, required scope, `dryRun()`, `execute()`.
- **AuditEntry** — actor (user, token, AI session), action, args (redacted), dry-run, approver, result, prev-hash.
- **Job** — durable queued work with lock key (per service/server), attempts, cancellation.

## Deploy flow (single node, git source)

1. Webhook arrives → signature verified → Job queued (superseded jobs for the same service are cancelled).
2. Clone into a sandbox dir (refuse symlinks escaping the repo) → build with Railpack/Dockerfile under cgroup caps, or hand off to a builder.
3. Compose/service spec passes **policy check** (no `privileged`, host pid/network, docker.sock or `/` mounts, devices — unless the admin allows it, audited).
4. Start the new container(s) beside the old ones → health gate (HTTP/TCP/command) with timeout.
5. Healthy → switch Caddy upstream atomically → stop old containers after a drain period. Unhealthy → remove the candidate, keep the old version running, report with the log tail.
6. Record Deploy, update commit status, notify.

## Deploy strategies

- **Blue-green** (default for stateless): start beside, health gate, switch, drain connections, graceful stop timeout.
- **Stop-first** (chosen automatically when a writable volume is attached, e.g. databases, SQLite apps, uploads): show a maintenance page, stop old, start new, health gate; on failure restart the old one. Never two writers on one volume.
- **Release phase** (optional): a one-off container on the new image runs migrations before either strategy; failure aborts the deploy.
- Logs of the last N deploys are kept as compressed files on the agent (disk-capped) so removing an old container never loses its logs.

## Runtime compatibility

- Negotiate the Docker API version; publish the supported Docker range; warn when the host's Docker version changes; Docker release candidates run in CI.
- Keep a container-runtime interface so rootless Docker / Podman can be added later (Phase 5).
- Build for the target server's architecture (amd64/arm64).
- IPv6 everywhere: proxy, networks, agent dial-out, DNS diagnostics.

## Failure-handling rules

- Every long operation is a Job with a lock key, timeout and cancellation; restart-safe (resume or clean rollback on boot via reconciler).
- A reconciler compares desired state (DB) with actual state (Docker labels `serverdock.*`) and reports drift; it only removes resources it owns (labelled by deploy attempt).
- Agent disconnect: operations in flight are marked `unknown` until the agent reports back; agent buffers metrics in a ring buffer.
- Version skew: control plane supports agent N-1; agents self-update from the control plane, signature-checked.

## Footprint budget

These are the budgets, measured by `bench/run.sh` in CI on a 1 vCPU / 1 GB VM. The two **totals in bold are hard gates**: a PR that exceeds them fails CI and needs a written reason and `footprint` agent review. Component rows are targets the bench reports but does not enforce yet. The remote-agent row becomes a gate again when the slim agent ships in Phase 2 ([ADR 0008](decisions/0008-footprint-gates.md)).

| Component | Idle RSS | Idle CPU |
|---|---|---|
| Control plane (API, hub, scheduler, SQLite, SPA) | ≤ 40 MB | < 0.5% |
| In-process local agent | + ≤ 8 MB | — |
| Remote agent (slim build, Phase 2) | ≤ 20 MB | < 0.3% |
| Embedded Caddy | + ≤ 30 MB | — |
| **Single node total** | **≤ 80 MB** | **< 1%** |
| **Remote node total (agent + proxy)** | **≤ 50 MB** | **< 0.5%** |
| Binary size (server, compressed) | ≤ 40 MB | |
| SPA first load (gzip) | ≤ 250 KB JS | |

Extra containers ServerDock adds to a managed server: **zero** (agent and proxy in one process), or one if the user chooses the Caddy-container fallback.

## Background work

Every goroutine that wakes on a timer, and the goroutines that exist per session. Anything that adds a timer adds a row (`footprint` checks this table). Lazy work (done inside a request or a handshake, with no timer and no goroutine) is listed so nobody adds a timer for it later.

| What | Where | Interval | When it runs |
|---|---|---|---|
| yamux keepalive, hub side | `internal/hub/hub.go` `attach` | 30 s | per **remote** agent session (off on the in-memory local pipe) |
| yamux keepalive, agent side | `internal/agent/session.go` `ServeSession` | 30 s | per remote session, in the agent process |
| Reconnect backoff | `internal/agent/client.go` `sleepCtx` | 1 s to 30 s, jittered | only while a remote agent is disconnected |

Per-session goroutines (they exist because a session exists; none wakes on a timer of ours):
- The hub's **drain goroutine** per agent session (`attach`): blocked in `AcceptStream`. Agents never open streams, so the first one ends the session. It exits with the session.
- yamux's own send, receive and keepalive goroutines per session (the keepalive is the 30 s row above).
- The agent side serves each accepted stream in its own goroutine, at most 32 at once.
- The in-process local agent's two session goroutines in `runServer`, joined before it returns.

Lazy, no timer and no goroutine: the hub certificate renewal and its 1 minute back-off (`internal/pki`, checked in the TLS handshake); the rate limiters' eviction (`internal/limits`); the audit log's denied-row "suppressed" summaries (written on the next denied attempt or at shutdown); the migration runner and its backup (startup only); the startup wait on the data-directory lock (at most about 1 s).

## Security model (summary)

Full threat model: [security.md](security.md).

- Agent socket access to Docker is root-equivalent, so the agent has **no listening ports**; only outbound to the control plane.
- The hub's **agent listener** (`--agent-listen`, off by default) is the only inbound agent surface: its own TLS 1.3 listener (not behind Caddy), a private hub CA the agent pins by fingerprint, a shared token checked after a per-IP failure limiter, session and connection caps, and a yamux ping guard. Remote agents are read-only (`logs` only) until Phase 2. See [ADR 0009](decisions/0009-agent-channel-tls.md).
- Every operation is a registered action with a risk tier, a per-actor policy and an audit trail (intent row first, outcome row after); the audit log is hash-chained and `serverdock audit verify` checks it read-only.
- Panel binds to localhost behind the embedded proxy; first run needs the console setup token.
- Secrets encrypted with a key kept outside the DB file; the key/data pair is shown to the user to back up.
- Auth: sessions + scoped API tokens; TOTP/passkeys; fresh 2FA for terminal, secret reveal, destructive actions; sessions revoked on password/2FA change.
- WebSocket/terminal/MCP routes use the same authz as REST; an RBAC route-matrix test runs in CI.
- Signed releases; SBOM; private vulnerability reporting.

## Repository layout

`*` = existed after Phase 0; `+` = added in Phase 1 M1; the rest are planned.

```
cmd/serverdock/           * main: server | agent | version; + audit verify (cli later)
internal/api/             * control-plane HTTP: /healthz, SPA serving, debug handlers; + header/body limits, /agent/* answers 404, the "from our proxy" connection mark (`WithTrustedProxy`)
internal/hub/             * server side of agent connections (WebSocket + yamux); + sessions cap, auth-failure limiter, stream caps, token rules
internal/agent/           * agent runtime: dial-out client, backoff, stream handlers; also used in-process by `server`; + op-spec table (RemoteOK), ping guard
internal/agent/proxy/     * embedded Caddy, owned by the agent runtime (ADR 0004)
internal/agent/dockerapi/ * Docker Engine API client over the unix socket (logs; + per-call deadlines, API negotiation, Stop)
internal/pki/             + the hub CA, the hub leaf with lazy renewal, the agent-side pin check (ADR 0009)
internal/limits/          + keyed token buckets, client-IP keys (X-Forwarded-For only from the marked control socket), a connection-capping listener
internal/store/           * SQLite (WAL); + forward-only migrations (migrations/NNNN_name.sql), pre-migrate backup, one-server lock, read-only open, testdata/ fixtures
internal/audit/           + hash-chained audit log, verify, denied-row rate limit, the intent/outcome flow
internal/redact/          + removes secrets from audit rows, errors and logs
internal/datadir/         + creates and checks DATA (0700, owned by the running uid), the secret-path list, atomic file writes
internal/secretbox/       + AES-256-GCM sealing under DATA/keys/master.key, with the missing-key and wrong-key refusals
internal/testbin/         + test-only harness: builds and runs the real binary (umask, file modes, exit codes, other uids)
internal/actions/         + action registry: risk tiers, policy, SystemInvoker, built-in read actions (shared by API, MCP, AI)
internal/memstat/         * /proc/self/status + Go runtime stats (convenience only)
internal/mcp/             MCP server
internal/ai/              assistant loop, redaction, approvals
internal/deploy/          pipeline, health gate, rollback, reconciler
internal/build/           railpack/buildkit, builder selection
web/                      SvelteKit SPA (embedded at build time); web/dist/index.html is a Phase 0 placeholder *
bench/                    * Dockerfile, run.sh, measure.sh, results/
templates/                one-click app catalog + CI tests
.github/workflows/        * ci.yml; + .github/scripts/ (bench trigger), CODEOWNERS, dependabot.yml
docs/                     vision, features, architecture, roadmap, research, plans, decisions
```
