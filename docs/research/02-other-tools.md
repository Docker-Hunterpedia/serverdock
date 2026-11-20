# Research: Alternatives beyond Coolify and Dokploy

*Researched 2026-11-20 by an AI research agent; not yet confirmed by the maintainer. Stars, licenses and releases from the GitHub API on that date. RAM figures are third-party measurements and vary 2–5× by host — treat as rough.*

## Quick table

| Tool | Stack | Control model | Idle RAM (cited) | License | Stars / activity |
|---|---|---|---|---|---|
| Openship | TS/Bun, Postgres + Redis, OpenResty | Central API + SSH | ≥2 GB required; no idle figure | Apache-2.0 | 15.4k, v1.0.1 2026-11-18 |
| CapRover | Node/TS, nginx, Swarm | Swarm leader | ~210 MB | Apache-2.0 | 15.2k, v1.16.1 2026-11-18 |
| Dokku | Bash + Go plugins, nginx | SSH/git push, no daemon | ~95 MB | MIT | 32.2k, active |
| Komodo | Rust, MongoDB/FerretDB | Core + Periphery agent | 146–208 MiB total (Core ~63, Periphery ~12) | GPL-3.0 | 13.0k, active |
| Easypanel | Closed, Swarm + Traefik | Central panel | ≥2 GB | Proprietary | n/a |
| Portainer CE | Go, BoltDB | Server + agent / Edge agent | ~19–24 MiB | Zlib (BE commercial) | 38.9k |
| Kamal | Ruby CLI + kamal-proxy (Go) | Agentless SSH | only proxy resident | MIT | 14.7k |
| Canine | Rails, Postgres, Kubernetes | UI over your k8s | k8s overhead | Apache-2.0 | 3.0k |
| Uncloud | Go, Caddy, WireGuard, P2P state | Daemon per host, no control plane | n/a | Apache-2.0 | 5.6k |
| Cosmos | Go, Mongo, own proxy | Single node | n/a | Apache-2.0 + Commons Clause (not OSI) | 6.2k |
| Runtipi | NestJS/React, Traefik, Postgres | Single node | n/a | GPL-3.0 | 9.7k |
| 1Panel | Go, SQLite, OpenResty | Panel + agent | n/a | GPL-3.0 | 37.5k |
| Temps | Rust + Postgres | Central + workers | n/a | Apache-2.0 | 0.9k |

Excluded as stale/minor: Sidekick (no release since 2024-10), CasaOS (focus moved to ZimaOS), Disco (~180 stars), Ptah (dead), Kubero (k8s-bound).

## Notes per tool

**Openship** ([repo](https://github.com/oblien/openship), created 2026-03). Repo/folder/artifact → build → container → routing + TLS. Desktop app, web, CLI, JS SDK, REST, MCP. Compose mode runs Postgres + Redis + API + dashboard; "bare" mode uses an embedded DB. Remote servers driven over SSH. 2 cores / 2 GB minimum, reviewers call it "not low-overhead" ([Bitdoze](https://www.bitdoze.com/openship-self-hosted-paas/)); idle Bun agent leak to 22 GB ([#265](https://github.com/oblien/openship/issues/265), closed).
- Standouts: control plane as a desktop app over SSH; routing/TLS applied after app start so a cert failure means "needs action", not down; per-deploy config snapshots for exact rollback; off-box builds; Compose diff-review gate; maintenance & custom error pages; copy service to another server; shareable deploy links; Railway import ([v1.0.0](https://github.com/oblien/openship/releases/tag/v1.0.0)).
- Weaknesses: v1.0 fixed an authz bug letting org members run host commands; onboarding errors ([#527](https://github.com/oblien/openship/issues/527)); concurrent builds corrupt containerd store ([#676](https://github.com/oblien/openship/issues/676)); port collisions ([#1570](https://github.com/oblien/openship/issues/1570)); scope sprawl (mail, CDN, analytics).

**CapRover** — Node "captain" on Swarm + nginx, JSON config, one-click catalog. ~210 MB idle ([selfhostable.dev](https://selfhostable-dev.pages.dev/blog/coolify-vs-caprover-vs-dokku)). Swarm deprioritized by Docker, overlay breakage on upgrades, stale templates ([discussion #2016](https://github.com/caprover/caprover/discussions/2016)), dated UI, weak Compose support ([#2390](https://github.com/caprover/caprover/issues/2390)).

**Dokku** — `git push` mini-Heroku, no resident daemon, plugins, buildpacks/CNB/Railpack/Dockerfile. ~95 MB idle. No OSS UI (Pro is paid), single server by default, no built-in backup scheduling ([Sliplane](https://sliplane.io/blog/dokku-self-hosted-heroku-alternative)).

**Komodo** — Rust Core + stateless Periphery agent; Compose stacks, builds, stats; no proxy/TLS. 146–208 MiB total, DB dominates ([DreamHost](https://www.dreamhost.com/guides/docker/portainer-or-a-lighter-docker-dashboard/), [Jacar](https://jacar.es/en/komodo-vs-portainer-vs-dockge-comparison/)). Standouts: Resource Sync (platform state as TOML in git), TypeScript procedures, ephemeral cloud builders, alerters, fine permissions. Issues: Periphery CPU spikes ([#1546](https://github.com/moghtech/komodo/issues/1546)), stale `compose logs` processes saturating dockerd ([#1514](https://github.com/moghtech/komodo/issues/1514)), wrong RAM in VMs ([#125](https://github.com/moghtech/komodo/issues/125)), Mongo dependency.

**Easypanel** — polished closed-source panel on Swarm + Traefik, 750+ templates, CLI + MCP in 2026 ([changelog](https://easypanel.io/changelog)). Free tier 3 projects; ~$11–30/server/month ([pricing](https://easypanel.io/pricing)). Clustering stuck in alpha.

**Portainer CE** — Go + BoltDB, ~20 MiB idle. Edge agent dials out (works behind NAT). Not a PaaS (no git builds, no TLS proxy). RBAC/team features paywalled ([oneuptime](https://oneuptime.com/blog/post/2026-03-20-portainer-ce-vs-business/view)).

**Kamal** — agentless Ruby CLI over SSH, kamal-proxy for zero-downtime + TLS, accessories for DBs, `deploy.yml`, secrets adapters (1Password/Bitwarden/AWS). Needs a registry; opaque "health check failed" ([levelup](https://levelup.gitconnected.com/kamal-said-healthy-my-users-said-down-b18a411e4ecc)); no maintenance mode, limited multi-domain ([Turkovic](https://www.ivanturkovic.com/2026/02/06/honest-take-kamal-rails-deployment/)); no UI or observability.

**Canine** — Heroku-like UI on your k8s ([repo](https://github.com/CanineHQ/canine)); SSO (SAML/OIDC/LDAP), Helm, MCP, custom pod templates. Needs k8s.

**Uncloud** — Go; Docker hosts joined in a WireGuard mesh with P2P state and no control plane; Compose deploys; Caddy with shared certs; service DNS; **Unregistry** pushes only missing layers straight to hosts ([repo](https://github.com/psviderski/uncloud)). CLI only; no auto-rollback yet.

**Cosmos** — Go home-server gateway: proxy + auto-HTTPS, built-in IdP (OIDC/MFA), SmartShield anti-bot/rate limit, Nebula VPN, Restic backups, monitoring, app store. Not OSI open source; single node.

**Runtipi** — home-server app store, easy app-contribution format; not a dev PaaS.

**1Panel** — Go server panel (sites, app store, DBs, files, terminal, cron, backups, firewall, multi-node agent). CVE run: [CVE-2025-54424](https://advisories.gitlab.com/golang/github.com/1panel-dev/1panel/core/CVE-2025-54424/) agent cert RCE, [CVE-2025-66508](https://www.cvelogic.com/cve/CVE-2025-66508) XFF allow-list bypass, CSRF series, [CVE-2026-23525](https://nvd.nist.gov/vuln/detail/CVE-2026-23525) XSS.

**Others:** [Temps](https://github.com/gotempsh/temps) (built-in analytics, Sentry-compatible errors, OTel, uptime; `temps migrate` imports from Coolify/Dokploy/CapRover/Portainer/Kamal/Vercel; agent skills). [Arcane](https://github.com/getarcaneapp/arcane) ~58 MiB, [Dockhand](https://github.com/Finsys/dockhand) ~77 MiB, [Dockge](https://github.com/louislam/dockge) ~150 MiB (Compose-first). Forge/Ploi/RunCloud: hosted SaaS control planes (general knowledge, not re-verified).

## Feature ideas worth stealing

1. Near-zero resident footprint — Dokku (no daemon), Kamal (proxy only), Komodo Periphery (~12 MiB), Portainer (~20 MiB UI).
2. Control plane on your laptop, nothing public — Openship desktop, Kamal, Uncloud.
3. Outbound-only agent for NAT/home servers — Portainer Edge.
4. Registry-less push of missing layers — Uncloud Unregistry.
5. WireGuard mesh multi-server without Swarm/k8s — Uncloud.
6. Platform state as GitOps file with drift sync — Komodo Resource Sync.
7. Scriptable procedures — Komodo.
8. Per-deploy config snapshots for exact rollback — Openship.
9. Routing/TLS decoupled from app health — Openship.
10. Compose diff review gate — Openship.
11. Maintenance mode & custom error pages — Openship.
12. Off-box / ephemeral builders — Komodo, Openship.
13. Security in the proxy (rate limit, geo-block, bot detection, SSO in front of apps) — Cosmos.
14. Plugins — Dokku; easy app-contribution format — Runtipi.
15. Heroku compatibility (Procfile, buildpacks) — Dokku, Canine.
16. Importers from competitors — Temps, Openship.
17. Agent-first interfaces (MCP, CLI, agent skills) — Openship, Temps, Easypanel, Canine.
18. Built-in observability (errors, OTel, uptime, status pages) — Temps.
19. Encrypted incremental backups with restore verification — Cosmos.
20. Template escape hatch — Canine.
21. Shareable deploy links — Openship.
22. Pluggable secrets backends — Kamal.

## Gaps nobody fills well

- **Published, reproducible resource budget** — no tool ships benchmarks; ServerDock can ship `serverdock bench` + CI-tracked idle numbers.
- **Light UI + real PaaS** — light tools are CLI-only or Docker UIs without builds/TLS; full PaaS UIs need 200 MB–2 GB+.
- **Multi-server without Swarm/k8s, with a UI.**
- **Trustworthy templates** — catalogs rot; nobody CI-tests template upgrades.
- **Backups you can trust** — automated restore tests are rare.
- **Security posture as a feature** — minimal surface, no public panel by default, signed updates, audit log.
- **Honest, actionable failure messages** ("port not listening", "DNS not pointing", "cert rate-limited").
- **Safe upgrades / Docker version checks.**
- **Fully OSS with team features** — RBAC/SSO/multi-node paywalled elsewhere.
- **Low-RAM / ARM / no-AVX friendliness** — SQLite-embedded by default is an opening.
