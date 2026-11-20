# ServerDock — Features

*Draft 1, 2026-11-20. AI-written; not yet confirmed by the maintainer. Phases refer to [roadmap.md](roadmap.md). "From" credits where the idea comes from in [research/](research/).*

Legend: **P1** MVP · **P2** multi-server & builds · **P3** light built-in AI + ops · **P4** teams · **P5** company/scale. (MCP: read-only in P1, write actions in P2. See ADR 0006.)

## 1. Install, update, operate ServerDock itself

| Feature | Phase | From / note |
|---|---|---|
| One-command install (`curl … \| sh`), readable script, installs Docker only after asking | P1 | all |
| One-time setup token printed to console (no open setup window) | P1 | Portainer CVE lesson |
| Self-update from the UI: start a candidate, health-check it, then hand over; old version keeps running if the new one fails | P1 | our differentiator |
| Automatic encrypted backup of ServerDock's own state before every update; restore + **"rehearse restore"** dry run | P1 | Coolify (pre-update backup) + ours |
| Signed releases (cosign), verified on self-update | P1 | security baseline |
| `serverdock bench`: reproducible idle RAM/CPU report | P1 | gap nobody fills |
| Pre-flight checks: Docker version/API, ports 80/443, DNS, disk, cgroup v2 | P1 | CapRover lesson |
| Control plane on your laptop (desktop/CLI mode), nothing public | P5 | Openship, Kamal |

## 2. Deploy apps

| Feature | Phase | From / note |
|---|---|---|
| Deploy from Docker image | P1 | all |
| Deploy from Dockerfile in a git repo | P1 | all |
| Deploy Docker Compose stacks (parsed with compose-go, policy-checked) | P1 | Dokploy, Komodo |
| Railpack auto-build (no Dockerfile needed) | P2 | Railway; Nixpacks is deprecated |
| GitHub / GitLab / Gitea / Bitbucket integration, push-to-deploy webhooks, commit status + PR comment | P1 (GitHub, generic webhook) / P2 (others) | Coolify, Dokploy |
| Zero-downtime blue-green handover with **post-deploy health gate and automatic rollback** | P1 | Kamal + ours |
| One-click rollback to any previous deploy using a **per-deploy config snapshot** (image + env + compose + routes) | P1 | Openship |
| Auto-cancel superseded deploys; per-app deploy lock | P1 | Coolify |
| Pre/post-deploy commands (migrations) | P2 | Dokploy request |
| PR preview deployments, **including Compose** | P2 | Coolify, Dokploy #2028 |
| Compose change review: diff before apply | P2 | Openship |
| Environment variables & secrets, encrypted at rest, reveal is explicit and audited; shared variables | P1 | ours |
| Environments (prod / staging) per project | P2 | Dokploy |
| Scheduled jobs / cron per app | P2 | all |
| Move or copy an app to another server | P2 | Dokploy #812 (top request), Openship |
| Registry-less push: send only missing layers to the target server | P5 | Uncloud |
| Importers from Coolify, Dokploy, CapRover | P5 | Temps |

## 3. Builds

| Feature | Phase | From / note |
|---|---|---|
| Local build with cgroup memory/CPU caps so builds can't OOM your apps | P1 | ours |
| Dedicated **builder server** (agent role `builder`, BuildKit started on demand) | P2 | Komodo, Coolify, Dokploy |
| Build in CI (generated GitHub Actions workflow → registry → deploy) | P2 | ours |
| Registry cache; optional built-in registry (Zot) | P3+ | |

## 4. Domains, proxy, TLS

| Feature | Phase | From / note |
|---|---|---|
| Embedded Caddy: automatic HTTPS, HTTP/2/3, WebSockets, atomic JSON config (no labels) | P1 | research/03 |
| Routing/TLS decoupled from app health: a cert/DNS problem means "needs action", not "down" | P1 | Openship |
| Actionable domain diagnostics: "DNS points elsewhere", "port not listening", "rate-limited by Let's Encrypt" | P1 | Kamal pain |
| Wildcard domains via DNS-01 (Cloudflare first, more providers later) | P2 | Coolify |
| Redirects, basic auth, custom error pages, maintenance mode | P2 | Coolify, Openship |
| Cloudflare-aware: real client IP, "is anything bypassing the CDN?" check | P2 | ours |
| Proxy security: rate limiting, IP/geo allow/deny, SSO forward-auth in front of any app | P5 | Cosmos |

## 5. Databases & backups

| Feature | Phase | From / note |
|---|---|---|
| One-click Postgres, MySQL/MariaDB, Redis/Valkey | P1 | all |
| MongoDB, ClickHouse, others | P2 | |
| Scheduled backups to S3-compatible storage (zstd, encrypted) | P1 | all |
| Volume / directory backups (restic-style incremental) | P2 | Cosmos, Coolify |
| **Automated restore tests**: restore into a throwaway container and verify | P2 | gap nobody fills |
| Restore with approval and a pre-restore snapshot | P1 | ours |

## 6. Servers & multi-server

| Feature | Phase | From / note |
|---|---|---|
| Local server managed in-process (single binary) | P1 | Beszel/PocketBase model |
| Remote servers via **outbound agent** (works behind NAT/CGNAT, no inbound ports, no stored SSH keys) | P2 | Portainer Edge, Komodo |
| One-time SSH-assisted agent install (key used once, then discarded) | P2 | ours |
| Server overview: Docker info, disk, cgroup, OS updates available | P2 | |
| Private networking between servers (WireGuard mesh) | P5 | Uncloud |
| Cloud provisioning (Hetzner first) | P5 | Coolify |

## 7. Observability

| Feature | Phase | From / note |
|---|---|---|
| Host + container metrics from cgroup v2 (no `docker stats` polling), SQLite rollups | P1 | research/03 |
| Live logs streamed only while someone watches; streams always cancelled | P1 | Dokploy CPU-storm lesson |
| Cross-service log search (grep on the agent) incl. proxy access logs | P1 | ours |
| Request ID tracing from proxy to app logs | P2 | ours |
| Alerts (container down, restart loop, disk/RAM thresholds, cert expiry, backup failed) | P1 | all |
| Notifications: email, webhook, Telegram, Discord, Slack, ntfy | P1 (email, webhook, Telegram) / P2 (rest) | all |
| Request analytics from the proxy log | P3 | Coolify |
| Prometheus `/metrics` endpoint; log/OTLP export | P3 | Dokploy |
| Uptime checks and public status page | P5 | Temps |

## 8. Web terminal & files

| Feature | Phase | From / note |
|---|---|---|
| Container shell in the browser (xterm.js), authenticated like REST, short-lived tickets | P2 | all |
| Host root shell: off by default; when enabled needs admin + fresh 2FA code + typed hostname confirmation each time | P2 | ours |
| File browser for volumes (read; write behind permission) | P3 | Coolify |

## 9. AI: MCP first, light built-in AI later (ADR 0006)

| Feature | Phase | From / note |
|---|---|---|
| **One typed action registry** behind UI, REST, CLI, MCP and the later built-in AI — AI can never do more than the caller | P1 | research/03 |
| MCP endpoint, **read-only**, scoped `read` tokens, secret redaction, tool output treated as untrusted | P1 | ours, Coolify, Dokploy |
| MCP investigation tools: failing services, explain deploy failure, search logs, logs around a time, what changed, trace request, audit trail | P2 | ours |
| MCP write actions: dry-run → `pending_approval` → approve in the panel → execute; destructive needs typed confirmation | P2 | research/03 |
| Claude Code skills (`/serverdock deploy`) | P2 | Temps |
| Light built-in AI, BYO key (encrypted), model per job: Haiku 5.5 triage, Sonnet 5.5 chat, Opus 5.5 deep diagnosis, Fable 5.2 opt-in | P3 | |
| Automatic alert / failed-deploy explanations ("restarted 5×, OOM at 512 MB limit") | P3 | MCP can't do this |
| "Explain this" button; in-panel chat for users without an AI client | P3 | Dokploy has log-AI only |

## 10. Templates (one-click apps)

| Feature | Phase | From / note |
|---|---|---|
| Template catalog in Compose format with variables | P2 | all |
| **CI-tested templates**: each template is installed, health-checked and upgraded in CI; pinned versions | P2 | gap: catalogs rot |
| Import Coolify/Dokploy template formats | P3 | research/01 |
| Easy community contribution format | P2 | Runtipi |

## 11. Teams, access, API

| Feature | Phase | From / note |
|---|---|---|
| Single admin + 2FA (TOTP), passkeys | P1 | TOTP in M2; passkeys in M4, once the panel has a domain and a trusted certificate |
| REST API (OpenAPI) + CLI | P1 (API) / P2 (CLI) | all |
| Organizations, projects, roles (owner/admin/developer/viewer), per-project access — **free** | P4 | paywalled elsewhere |
| OIDC/SAML SSO | P4 | Dokploy Enterprise |
| Hash-chained audit log, exportable | P1 (log) / P4 (export) | ours |
| GitOps: whole platform state as a file in git with drift detection | P4 | Komodo Resource Sync |
| External secret managers (1Password, Vault, Bitwarden) | P4 | Kamal |
| Plugins / extension points | P5 | Dokku |
| HA control plane (Postgres backend) | P5 | |

## 12. Added after the gap check (2026-11-20)

These are features competitors have that the first draft missed. They were found by checking every feature in [research/](research/) against this file.

| Feature | Phase | From |
|---|---|---|
| Private registry credentials (GHCR, Docker Hub, self-hosted) | P1 | all |
| Persistent volumes & bind mounts (policy-checked), per-project private network with service DNS names | P1 | all, Uncloud |
| Automatic disk cleanup: old images, build cache, stopped containers, with retention and disk alerts | P1 | common pain (disk full) |
| Docker resources view: all containers, images, volumes, networks on a server (incl. ones ServerDock didn't create), prune with preview | P2 | Dokploy, Portainer |
| Multiple processes from one repo (web + worker), replicas on one server | P2 | Dokku (Procfile), Coolify |
| Deploy from a local folder / upload (`serverdock up`) | P2 | Dokploy, Openship |
| `[skip ci]` commit markers | P2 | Coolify |
| Cloudflare Tunnel support (expose apps with no public IP) | P2 | Coolify |
| More DB engines: MongoDB, ClickHouse, KeyDB/Dragonfly, libSQL | P2 | Coolify, Dokploy |
| More notifications: Pushover, Gotify | P2 | Coolify, Dokploy |
| Container image vulnerability scanning (opt-in, on the builder) | P3 | Coolify v4.7 |
| Custom proxy config snippets (advanced escape hatch) | P3 | Coolify |
| Project export/import between ServerDock instances | P3 | Dokploy, Coolify |
| Server hardening checks: pending security updates, SSH password login, open ports, firewall status | P3 | 1Panel, Coolify (OS patching) |
| Scriptable procedures / multi-step actions (built on the action registry) | P5 | Komodo |
| Same app on several servers with load balancing | P5 | Coolify, Dokploy (Swarm) |
| "Deploy to ServerDock" shareable links / buttons | P5 | Openship |
| GitHub Actions self-hosted runners on builder servers | P5 | Coolify |
| Error tracking (Sentry-compatible) as an optional plugin, not core | P5 | Temps |

### Coverage check

| Tool | Their key features | Covered? |
|---|---|---|
| Coolify | git deploys, previews, DB backups, 280+ templates, multi-server, build servers, MCP, teams, OIDC, Cloudflare Tunnels, vuln scan, OS patching | Yes (phases above) |
| Dokploy | Compose, 600+ templates, build servers, move between servers, AI log analysis, CLI/MCP, Vault, export/import, Docker/network UI | Yes |
| Openship | desktop control plane, config snapshots, TLS decoupled from health, Compose diff, maintenance pages, deploy links | Yes; mail server is out of scope |
| Komodo | outbound agent, GitOps sync, procedures, builders | Yes |
| Portainer | edge agent, Docker resource management | Yes; Kubernetes/Swarm are out of scope |
| Dokku / Kamal | git push, Procfile processes, zero-downtime proxy, secrets adapters | Yes |
| Uncloud | WireGuard mesh, registry-less push, service DNS | Yes (P1 DNS, P5 mesh/push) |
| Cosmos | proxy security, SSO in front of apps, restic backups | Yes; VPN and storage pooling are out of scope |
| CapRover / Easypanel | one-click apps, polished UX | Yes |
| Temps | importers, observability, agent skills | Yes; error tracking only as a plugin |

## 13. Added after the final gap check (2026-11-20)

Sources: [research/04-final-gap-check.md](research/04-final-gap-check.md). It covers the most-reacted user requests on GitHub, commercial PaaS features, and what running in production needs.

| Feature | Phase | Why / from |
|---|---|---|
| **Per-app CPU/RAM limits** and reservations, restart policy, shm size | P1 | Protects a 1 GB box; Coolify #4661 |
| **Stop-first deploy strategy**, chosen automatically when a writable volume is attached; blue-green drains connections with a graceful stop timeout | P1 | Avoids two copies writing to one volume; Coolify #4692 |
| **Keep logs of the last N deploys** per service (compressed, disk-capped) | P1 | Blue-green would otherwise delete them |
| Restart and "redeploy without rebuild" (env changes) | P1 | Coolify #3560 |
| Live build and image-pull progress | P1 | Komodo #77, #1198 |
| Deploy context variables (`SD_FQDN`, `SD_COMMIT`, `SD_PR`, `SD_ENV`) | P1 | Coolify #4757 |
| **Update available** notice for app images (rare registry digest check) | P1 | Portainer #3434 (180) |
| Build concurrency setting; **size-capped BuildKit cache** | P1 | Dokploy #2000; Coolify #12542 (20+ GB cache) |
| Readable container names (`sd-<project>-<service>`) | P1 | Coolify #3569 |
| API/MCP token expiry + last-used | P1 | security |
| **IPv6**: Caddy, AAAA checks in diagnostics, IPv6 networks, v6-only agents | P1 | Coolify #2484 |
| `serverdock uninstall` (keep apps or remove all, dry-run first); `start/stop/status` | P1 | trust; Dokploy #1689 |
| `serverdock doctor --bundle` (redacted support archive); docs site; empty-state hints; in-UI docs links | P1 | onboarding complaints |
| Docker API version negotiation, published supported Docker range, warning when the host Docker version changes; Docker RC in CI | P1 | Docker 29 broke Portainer (#12925, 327) |
| Telemetry: none by default (ADR 0007) | P1 | trust |
| **Auto-update policies for apps/templates** (patch/minor, time window) through the health gate + rollback, backup first | P2 | Komodo #314, #979; Cloudron |
| Minimal multi-user (admin + member) before full RBAC | P2 | Dokploy #707 (58) |
| Per-repo config as code (`serverdock.toml`) | P2 | Dokploy #4008 |
| **Adopt existing Compose stacks/containers**; import `.env` files | P2 | Portainer #1857, Komodo #377 |
| Config files edited in UI and mounted; Compose with repo-relative files (policy-checked) | P2 | Portainer #6390 |
| **One-off run commands** (`serverdock run app -- cmd`) in a fresh container, audited | P2 | Heroku |
| **Release phase**: migrations run once on the new image before handover; failure aborts; rollback warns if the schema is ahead | P2 | Heroku |
| **Static sites served by Caddy** (no container), SPA fallback, atomic switch | P2 | CapRover #905; Netlify/Vercel |
| **TCP/UDP exposure**; external DB access with TLS + IP allowlist (off by default) | P2 | Coolify #3976; Railway |
| Promote staging → prod (same image digest), clone environment | P2 | Heroku pipelines |
| Preview lifecycle: auto-delete on PR close/TTL, per-server cap, DB seed per preview, previews from CI images, fork PRs need approval | P2 | Dokploy #2462, #2386 |
| DB major-version upgrade (guided, old volume kept); optional PgBouncer | P2 | pg_upgrade |
| Backup targets: local disk, SFTP | P2 | Dokploy #416 |
| Build for the target server's CPU architecture; templates tested on arm64 | P2 | Hetzner CAX, Oracle free tier |
| Master key re-key; agent cert renewal/revocation; server decommission | P2 | security |
| Bring-your-own TLS certs / internal CA | P2 | companies |
| Per-project/service alert routing | P2 | Dokploy #572 |
| Compose conformance test suite (behave like `docker compose`) | P2 | Coolify #7655, #5921 |
| Postgres point-in-time recovery (WAL-G) | P3 | Coolify #7423 |
| **Scale-to-zero**: sleep idle apps/previews, Caddy wakes them on request | P3 | Dokploy #2154; Fly autostop |
| Usage & cost view per project (from existing metrics) | P3 | Railway/Vercel |
| One-click DB password rotation with redeploy of dependents | P3 | security |
| Azure DevOps, Codeberg git sources | P3 | Dokploy #1949, #2021 |
| Required second-person approval for prod deploys | P4 | reuses the approval flow |
| Terraform/Pulumi provider (generated from OpenAPI) | P4 | Dokploy #1555 (66) |
| Rootless Docker / Podman runtime | P5 | Portainer #2991; keep the runtime abstraction from P1 |
| GPU containers with an explicit `gpu` permission | P5 | self-hosted AI models |
| Air-gapped install bundle | P5 | companies/government |

### Product rules added

- **No upsell banners, ever.** Security policies stay configurable (Portainer #8452, #6904).
- **Updates never overwrite user config**, never report success on failure, retry on registry rate limits, and show release notes before applying. Stable and edge channels.
- **Upgrades are tested from old versions** (N-5 → N), not only N-1 → N.
- Cron jobs have an explicit timezone.

## Explicitly not building

Mandatory Docker Swarm, Kubernetes, Nixpacks, Cloud Native Buildpacks, Traefik label magic, built-in mail server/CDN, Mongo dependency.
