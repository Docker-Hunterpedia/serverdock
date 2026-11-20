# Research: Coolify and Dokploy

*Researched 2026-11-20 by an AI research agent; not yet confirmed by the maintainer. GitHub-issue measurements are the most reliable numbers here; blog "benchmarks" state no method. Some high-numbered 2026 CVE IDs come from aggregators and were not checked against NVD. Reddit was not reachable.*

## Coolify ([coollabsio/coolify](https://github.com/coollabsio/coolify))

66.3k stars, Apache-2.0, v4.0.0 stable 2026-04-27, current v4.7.6 (2026-11-19). v5 still unreleased — its tracking issue [#5685](https://github.com/coollabsio/coolify/issues/5685) is the most-upvoted in the repo. Mostly one developer.

**Stack.** PHP/Laravel, Livewire + Alpine (React/Inertia UI starting). Control server runs `coolify` (app + Horizon workers + scheduler + Reverb websockets + terminal), `coolify-db` (Postgres), `coolify-redis`, `coolify-proxy` (Traefik v3 or Caddy) ([docs](https://coolify.io/docs/core/how-coolify-works)). Remote servers driven by **shell commands over SSH as root**; optional Rust `coolify-sentinel` for metrics. Builds: Nixpacks, Railpack (beta), Dockerfile, Compose, static. Image ~159 MB compressed.

**Footprint.** Official minimum 2 CPU / 2 GB ([docs](https://coolify.io/docs/get-started/installation)).

| Source | Finding |
|---|---|
| [#9489](https://github.com/coollabsio/coolify/issues/9489) | 43–58% idle CPU, ~310 MiB on the app container; `docker exec` SSH checks |
| [#9198](https://github.com/coollabsio/coolify/issues/9198) | idle CPU 4% → 40% after update; maintainer: monitoring tool "uses this much cpu in idle" |
| [#12145](https://github.com/coollabsio/coolify/issues/12145) | 362% CPU, 668 MiB from Horizon workers |
| [#7394](https://github.com/coollabsio/coolify/issues/7394) | `sessions` table grew to 58 GB; app 382% + DB 224% CPU |
| [#11111](https://github.com/coollabsio/coolify/issues/11111) | Sentinel reports constant 100% CPU when idle |

~35 CPU-titled and ~16 memory-titled issues; PHP memory-limit exhaustion ([#6289](https://github.com/coollabsio/coolify/issues/6289)); standard reply is "check CPU steal".

**Features (v4.7).** GitHub App/GitLab/Bitbucket/Gitea/any git/image/Compose; auto-deploy, auto-cancel superseded deploys; ~10 database engines with S3/SFTP backups and volume backups; pre-update self-backup; 280–360+ templates; Let's Encrypt, wildcards, Cloudflare DNS + Tunnels, error pages, maintenance mode, basic auth, redirects; unlimited SSH servers, build servers, cloud provisioning (Hetzner, DO, Vultr, Hostinger); multi-server apps limited (image only, no volumes/Compose); metrics, traffic analytics, image vulnerability scanning, OS patching (beta); logs, log drains, web terminal, file browser; email/Discord/Telegram/Slack/Pushover/webhook notifications; teams, roles, OIDC, passkeys, 2FA, audit log, external secret managers; REST API with scoped tokens, CLI, read/write MCP; PR previews, rolling updates, rollbacks, cron, self-hosted GitHub runners.

**Top complaints.**
1. CPU/RAM regressions after updates; heavy idle footprint.
2. Updates breaking instances — [#7193](https://github.com/coollabsio/coolify/issues/7193) (73 reactions), [#7599](https://github.com/coollabsio/coolify/issues/7599), [#5288](https://github.com/coollabsio/coolify/issues/5288) (rollback also failed); downgrade only by terminal.
3. Security — 11 critical flaws disclosed Jan 2026 ([The Hacker News](https://thehackernews.com/2026/01/coolify-discloses-11-critical-flaws.html)); CVE-2025-66209 backup command injection, CVE-2026-34038, CVE-2026-34597 Nixpacks RCE, CVE-2026-27955; PHP2Shell affecting all v4 ([#12266](https://github.com/coollabsio/coolify/issues/12266)). Root cause: shell strings over SSH as root.
4. Template and proxy breakage ([#2549](https://github.com/coollabsio/coolify/issues/2549), [#7942](https://github.com/coollabsio/coolify/issues/7942)).
5. Thin multi-server story; frequent breaking changes and UI redesigns.

**Loved.** Nothing paywalled; huge catalog; Heroku-style git push with SSL and previews; any VPS over SSH; S3 database backups; active community.

**Business.** Apache-2.0, no gating; Cloud $5/mo for 2 servers + $3/server; sponsors.

## Dokploy ([Dokploy/dokploy](https://github.com/Dokploy/dokploy))

38.6k stars, v0.31.4 (2026-11-17), 570 open issues. Company-backed.

**Stack.** TypeScript: Next.js 16, React 19, tRPC, Drizzle, better-auth, dockerode, ssh2, node-pty; small Go monitoring agent. Redis removed in v0.29.9 (in-memory per-server queues). Installer runs **`docker swarm init` (mandatory even on one node)**, services `dokploy` + `dokploy-postgres`, plus `dokploy-traefik`. Remote servers over SSH, get Traefik + optional metrics agent ([docs](https://docs.dokploy.com/docs/core/remote-servers)). Builds: Nixpacks, Railpack, Heroku/Paketo, Dockerfile, static, Compose/Stack. Image **~831 MB** compressed.

**Footprint.** 2 GB minimum ([docs](https://docs.dokploy.com/docs/core/installation)).

| Source | Finding |
|---|---|
| [#3909](https://github.com/Dokploy/dokploy/issues/3909) | app at 800–850 MiB; ~1 GB on a fresh install with no apps; >1.25 GB crashing every 12–16 h. Maintainer: ~400 MB at launch, "now usually 700 MB", "difficult to optimise a Next.js app" |
| [#4661](https://github.com/Dokploy/dokploy/issues/4661) | 40–50% CPU spikes every ~20 s: `docker stats --no-stream` every 1.3 s with no overlap guard → 6–12 concurrent processes |
| [#5872](https://github.com/Dokploy/dokploy/issues/5872) | orphaned `docker logs` children kept dockerd at 200–290% CPU for ~5 days |
| [#4835](https://github.com/Dokploy/dokploy/issues/4835) | opening Logs during redeploy left dockerd at ~314% CPU |

**Features (v0.31).** GitHub/GitLab/Bitbucket/Gitea/git/image/upload; Postgres/MySQL/MariaDB/Mongo/Redis/libSQL with S3 backups, restore, self-backup; 600+ templates; Traefik + Let's Encrypt, Cloudflare/Route53 DNS; remote servers, build servers, Swarm multi-node, move services between servers; metrics with alerts, export via Vector; terminal, container & network management; many notification channels; orgs/roles (SSO, SCIM, custom roles, audit logs are Enterprise); OpenAPI REST, CLI (449 commands), MCP (508 tools); **AI log analysis** with BYO provider; Sandboxes; app previews (Compose previews highly requested, [#2028](https://github.com/Dokploy/dokploy/issues/2028)); rollbacks, cron, environments, Vault; project export/import.

**Top complaints.**
1. Next.js monolith RAM growth; CPU storms from Docker polling.
2. Swarm/Traefik fragility — Traefik not starting after reboot ([#5131](https://github.com/Dokploy/dokploy/issues/5131)), overlay 502s ([#3375](https://github.com/Dokploy/dokploy/issues/3375)), log floods filling disk ([#5112](https://github.com/Dokploy/dokploy/issues/5112)), labels lost ([#5453](https://github.com/Dokploy/dokploy/issues/5453)); Caddy requested ([#1246](https://github.com/Dokploy/dokploy/issues/1246)).
3. Security — CVE-2025-53825 unauthenticated RCE via PR on public repo; injection through terminal/logs WebSockets, registry creds, file mounts, Traefik config; v0.31.1 "dozens of authorization and injection fixes".
4. Paid tiers for team features.
5. Poor mobile dashboard ([#6053](https://github.com/Dokploy/dokploy/issues/6053)); partial metrics returned as HTTP 200.

**Loved.** Clean UI, native Compose, big catalog, build servers, rollbacks, API/CLI/MCP, AI debugging. Most requested: move apps between nodes ([#812](https://github.com/Dokploy/dokploy/issues/812)), Compose previews, Caddy, OIDC, secret managers, self-backup, pre/post-deploy commands.

**Business.** Apache-2.0 core + source-available `/proprietary` since 2026-01-21 ([post](https://dokploy.com/blog/we-are-updating-dokploys-open-source-license)); Cloud from $4.50/server; Enterprise sales-led.

## Takeaways for ServerDock

1. **Real idle cost of both is ~0.7–1.2 GB RAM on the control plane** — more than a 1 GB VPS can spare. Blog claims that Dokploy is "lighter" don't hold in 2026.
2. **The worse problem is unbounded CPU** from polling (`docker stats`, `docker logs`, SSH checks) and queue workers. Event-driven Docker APIs, cancellable streams, and no polling are the fix.
3. **Security is a selling point**: both shipped ~a dozen CVSS 9+ command injections in 2026 from shell strings run as root.
4. **Upgrade safety**: atomic upgrades with automatic rollback and pre-upgrade snapshots.
5. **Avoid mandatory Swarm** and Traefik label magic.
6. **Feature floor to match**: git deploys + webhooks, Railpack/Dockerfile/Compose, auto SSL + wildcards, databases with S3 backups and restore, templates, PR previews (incl. Compose), rollbacks, logs, terminal, notifications, multi-server + build servers, free RBAC, REST/CLI/MCP, metrics.
