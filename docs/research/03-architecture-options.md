# Research: Low-footprint architecture options

*Researched 2026-11-20 by an AI research agent; not yet confirmed by the maintainer. Many RAM numbers for this class of tool come from blogs, not controlled benchmarks — validate on a fresh 1 GB VPS before treating them as fact.*

## Baseline we compete against

| Tool | Stack | Reported idle footprint |
|---|---|---|
| Coolify v4 | Laravel + Postgres + Redis + Soketi (+ Traefik/Caddy) | Official minimum 2 vCPU / 2 GB. Reports range 300 MB–1.2 GB, most often 600–800 MB ([docs](https://coolify.io/docs/installation), [massivegrid](https://massivegrid.com/blog/coolify-resource-planning-multiple-apps), [itechguides](https://www.itechguides.com/coolify-vps-requirements-cpu-ram-and-disk-you-need/)) |
| Dokploy | Next.js + Postgres + Redis + Traefik | 2 GB recommended; idle ~350–600 MB ([massivegrid](https://massivegrid.com/blog/dokploy-server-requirements-ram-cpu-disk/), [use-apify](https://use-apify.com/docs/self-hosted/devops-paas/dokploy)) |

**Target:** single-node install (control plane + agent + proxy) under **80 MB** idle; remote agent under **25 MB** (plus proxy).

## 1. Language: Go vs Rust → **Go**

- Comparable daemons: Beszel agent (Go) ~14 MB RSS measured ([computingforgeeks](https://computingforgeeks.com/install-beszel-server-monitoring/), [beszel.dev](https://beszel.dev/guide/what-is-beszel)); Netdata 100–200 MB idle ([docs](https://learn.netdata.cloud/docs/welcome-to-netdata/complete-product-description)); Komodo Periphery (Rust) publishes no RSS figure ([komo.do](https://komo.do/docs/setup/connect-servers)).
- Docker SDK: Go moby client is the reference; Rust `bollard` 0.21 is mature ([docs.rs](https://docs.rs/crate/bollard/0.21.0/source/README.md)).
- The deciding gap is the ecosystem around the SDK: **compose-go, BuildKit/LLB, Railpack, Caddy/certmagic** are embeddable Go libraries. Railway rewrote Railpack from Rust to Go for BuildKit access ([blog](https://blog.railway.com/p/introducing-railpack)).
- Go cross-compiles statically with no extra toolchain; the infra contributor pool is mostly Go.
- Rust would save ~5–10 MB per process — small against a 1 GB box.
- Use pure-Go SQLite (`modernc.org/sqlite` or `ncruces/go-sqlite3`) to keep `CGO_ENABLED=0`.

## 2. Control plane shape

- **One binary, two modes** (`serverdock server`, `serverdock agent`), SPA embedded via `embed.FS`. On a single node the agent runs in-process. No separate DB/Redis/WebSocket server.
- **SQLite (WAL)**: few MB vs Postgres 30–80 MB + Redis. Single writer behind a mutex, read pool, `busy_timeout`. Jobs table instead of Redis queues. Optional **Litestream v0.5** (embeddable) for off-site replication ([Fly.io](https://fly.io/blog/litestream-v050-is-here/), [litestream.io](https://litestream.io/how-it-works/)). Keep a DB interface for a future Postgres/HA backend.
- **Transport: agent dials out**, one TLS connection (WebSocket or gRPC) with multiplexed streams (yamux). Works behind NAT/CGNAT, no inbound ports, no SSH keys stored on the control plane. Enrollment via one-time token, then Ed25519/mTLS identity (Komodo model). Every RPC is a typed operation, never a shell string. SSH only as an optional one-time installer.

| Option | Verdict |
|---|---|
| SSH-only (Coolify) | Breaks behind NAT; control plane holds root keys (Coolify CVE-2025-64419/64420/64424 led to key theft, [Censys](https://censys.com/?p=4046)); shell-command injection risk |
| Inbound agent port | NAT problem, open port per host |
| **Agent dials out** | **Chosen** |

## 3. Reverse proxy & TLS → **embedded Caddy**

- Traefik ~80–120 MB idle; Caddy ~20–40 MB ([selfhosting.sh](https://selfhosting-sh.pages.dev/compare/caddy-vs-traefik/), [Traefik forum](https://community.traefik.io/t/traefik-performance-lags-behind-nginx-and-caddy/28919)). Dokploy's Traefik config path produced several command-injection CVEs.
- Embed Caddy as a Go module in the agent; build the full JSON route table from desired state and load it in-process (atomic reload, no labels, no templating, no shell). On-demand TLS gated by an `ask` check against our DB. Fallback: official Caddy container driven over a Unix-socket admin API.
- Custom proxy on certmagic + `httputil.ReverseProxy` is leaner but means owning HTTP/2/3, WebSocket and timeout edge cases — not worth it in v1.

## 4. Builds → **Dockerfile + Compose + Railpack on BuildKit, off the app node**

- Nixpacks is in maintenance / deprecated ([GitHub](https://github.com/railwayapp/nixpacks)); Coolify CVE-2026-34597 was RCE via Nixpacks params ([fieldguide](https://fieldguide.lutrasecurity.com/CVE-2026-34597/)).
- Railpack (Go): analyze → JSON plan → BuildKit LLB; smaller images ([blog](https://blog.railway.com/p/introducing-railpack)).
- Cloud Native Buildpacks: builder images hundreds of MB to 1 GB+, slow — skip.
- Build location, in order: dedicated **builder server** (agent role `builder`, on-demand `buildkitd`, pushes to registry) → **CI** (generated GitHub Actions workflow → GHCR → webhook) → local build only by opt-in, capped with cgroup `memory.max`/`cpu.max`. Registry cache for speed. Optional built-in registry (Zot).

## 5. Metrics & logs

- Docker stats API polling is expensive (~1 core for ~200 containers, [Scalingo](https://scalingo.com/blog/docker-containers-monitoring)); cAdvisor burns CPU ([cadvisor#2523](https://github.com/google/cadvisor/issues/2523)).
- **Read cgroup v2 files directly** (`cpu.stat`, `memory.current`, `io.stat`) and `/proc` for host and network ([Docker runmetrics](https://docs.docker.com/engine/articles/runmetrics/)).
- Store in SQLite with rollups: 15 s raw for 24 h → 1 min/7 d → 10 min/30 d → 1 h/1 y. Agent ring-buffers during disconnects. No embedded TSDB (30–100+ MB). Expose `/metrics` for Prometheus users.
- Logs: Docker `local` driver with rotation as source of truth; stream only while someone is watching; grep on the agent; deploy/build logs in SQLite or compressed files. Optional shipping to syslog/OTLP/Loki.

## 6. Frontend → **SvelteKit static SPA**

| Option | Gzipped runtime | Notes |
|---|---|---|
| **SvelteKit (static)** | ~15–30 KB | Small, good DX, routing built in |
| SolidJS | ~7–15 KB | Smallest; smaller ecosystem |
| React + Vite | ~45–60 KB | Largest contributor pool; acceptable second choice |
| HTMX | ~15 KB | Awkward for terminal, live logs, charts |

Frontend has no server RAM cost (static files in the binary). Use uPlot for charts, xterm.js for the terminal, one WebSocket for events/logs/terminal/AI streaming, REST typed from OpenAPI.

## 7. Built-in AI agent

- **One typed action registry, three consumers**: UI/REST, built-in Claude agent, MCP endpoint. The AI can never do more than the same user/token could.
- Risk tiers: `read` (auto), `mutate` (plan → dry-run diff → approval, or project policy), `destructive` (always approval with typed confirmation). No raw shell by default; opt-in break-glass shell with per-session approval.
- Hash-chained append-only audit log. Logs/files/READMEs are untrusted data; approvals enforced server-side; secrets redacted before reaching the model.
- BYO API key, encrypted at rest, used only by the control plane. Anthropic Go SDK tool runner; zero idle cost.
- MCP spec revision **2026-07-28** ([spec](https://modelcontextprotocol.io/specification/2026-07-28.md)). Set annotations (`readOnlyHint`, `destructiveHint`) but enforce risk server-side. OAuth 2.1 + scoped API tokens. Mutating calls return `pending_approval` unless the token is scoped for auto-approve.

| Job | Model |
|---|---|
| Log triage, alert summaries, cheap checks | Haiku 5.5 (`claude-haiku-5-5`) |
| Interactive "chat with your server", routine ops | Sonnet 5.5 (`claude-sonnet-5-5`) |
| Hard diagnosis, multi-server incidents, migration plans | Opus 5.5 (`claude-opus-5-5`) |
| Opt-in deep investigation | Fable 5.2 (`claude-fable-5-2`) |

## 8. Security lessons from past CVEs

- **Coolify** (Nov 2025–2026): many authenticated command injections → root (CVE-2025-66209…66213 CVSS 10, CVE-2025-59156 Compose injection, CVE-2025-34159/34161, CVE-2025-64419/64420/64424 SSH key theft, CVE-2026-34038, CVE-2026-34597) ([The Hacker News](https://thehackernews.com/2026/01/coolify-discloses-11-critical-flaws.html), [Censys](https://censys.com/?p=4046)).
- **Dokploy**: CVE-2025-53825 preview-deploy RCE; 2026 command injections via Traefik config, logs endpoint, DB deploy, registry creds, file mounts; terminal WebSocket auth bypass; cross-org authz bypass ([vuln.today](https://vuln.today/cve/CVE-2025-53825), [halosecurity](https://cve.halosecurity.com/cve-advisory/cve-2026-45630-dokploy-os-command-injection-in-updatetraefikconfig)).
- **Portainer** (2026): CVE-2026-44848 plugin ops → host RCE, CVE-2026-44849, CVE-2026-44881 git symlink file read, CVE-2026-44882, CVE-2026-55761 unauthenticated init via restore.

**Design rules:** never build shell strings (Docker API/compose-go; argv only); treat user Compose/Dockerfiles as code with a default-deny policy (`privileged`, host pid/network, docker.sock and `/` mounts, devices); explicit RBAC matrix tested per route incl. WebSocket/terminal/MCP; no stored SSH keys; terminal/WebSocket auth equal to REST, revoke sessions on password/2FA change; sandboxed git clones refusing escaping symlinks; no unauthenticated setup/restore windows; signed releases and verified self-update; 2FA, CSRF, CSP, rate limits, disclosure policy from day one.

## 9. Footprint budget (estimates — make them CI-tracked)

| Component | Idle RSS target |
|---|---|
| Control plane (API, hub, scheduler, SQLite, SPA) | 25–40 MB |
| In-process local agent | +3–8 MB |
| Remote agent | 10–20 MB |
| Embedded Caddy | +15–30 MB |
| Metrics (cgroup reads/15 s) | ~0 MB, <0.5% CPU |
| Logs | 0 MB extra |
| AI agent | 0 at idle |
| BuildKit | 0 at idle (on demand / offloaded) |
| **Single node total** | **~45–80 MB** |
| **Remote node total** | **~25–50 MB** |
