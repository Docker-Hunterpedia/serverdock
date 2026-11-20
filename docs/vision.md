# ServerDock — Vision

*Draft 1, 2026-11-20. AI-written from the research in [research/](research/); not yet confirmed by the maintainer.*

## One line

**A self-hosted PaaS and server manager that runs on a 1 GB VPS beside your apps, with a Claude agent you can ask anything about your servers.**

## The problem

Self-hosted panels like Coolify and Dokploy made "your own Heroku" popular, but in 2026:

1. **They are heavy.** Measured idle control-plane use is about **0.7–1.2 GB RAM**, and both need 2 GB minimum ([research/01](research/01-coolify-dokploy.md)). Openship and Easypanel need 2 GB or more too.
2. **They burn CPU while idle.** Polling `docker stats`/`docker logs`, SSH health checks, and queue workers regularly produce 40–300% CPU on idle hosts.
3. **They keep shipping critical security bugs.** Coolify and Dokploy each shipped about a dozen CVSS 9+ command injections in 2026. Running shell strings over SSH as root makes these bugs hard to avoid.
4. **Upgrades break things.** Updates take proxies or dashboards down and are hard to roll back.
5. **The light tools are CLI-only.** Dokku, Kamal and Uncloud are light but have no UI. Portainer is light but isn't a PaaS. Team features (RBAC, SSO) are often paywalled.

## The promise

| Principle | What it means in practice |
|---|---|
| **Light** | Single node (panel + agent + proxy) idles **under 80 MB RAM and <1% CPU**. Remote agent under 25 MB. Numbers are published, reproducible (`serverdock bench`) and tracked in CI. |
| **Safe** | No shell strings, ever: typed operations on the Docker API. Agents dial out, so there are no stored SSH keys and no inbound ports. 2FA on dangerous actions, audit log, signed releases. |
| **Honest** | The UI shows real state, says `unknown` when it doesn't know, and never reports success before the work is done. Failures come with the evidence and a next step. |
| **Simple** | One binary, one command to install, SQLite inside. No Redis, no Swarm, no Kubernetes required. |
| **AI-native, with guardrails** | A built-in Claude assistant and an MCP endpoint share one set of typed tools. Reads run freely; writes need a dry run and your approval. Secrets never reach the model. |
| **Fully open** | Open source with team features included: no paywall on RBAC, SSO or multi-server. |

## Who it's for (staged)

1. **Solo devs and homelabbers** (phase 1). One person, 1–5 small VPS or home servers, some behind NAT. Wants git-push deploys, domains, databases and backups without giving up half the RAM.
2. **Small teams and agencies** (phase 4). Several people, many client projects, roles, previews, notifications.
3. **Companies** (phase 5). Many servers, SSO, audit export, policies, optional HA control plane.

## What success looks like

- A new user installs on a $4 1 GB VPS and deploys their first app from git with HTTPS in **under 5 minutes**.
- Published idle numbers stay **5–10× lower** than Coolify and Dokploy, release after release.
- Zero critical command-injection CVEs, because the design leaves no place for them.
- An operator can ask the assistant "why is my API returning 502s?" and get an evidence-backed answer from logs, metrics and deploy history.

## Non-goals (for now)

- Kubernetes management.
- A hosted cloud offering (we leave room for one later).
- Mail servers, CDN or analytics suites (Openship-style sprawl).
- Windows hosts.
- Translations / Arabic / RTL: the UI is English only (maintainer decision, 2026-11-20).

## Name

**ServerDock**, confirmed by the maintainer. The website will live on a subdomain of oday-bakkour.com (exact subdomain to be chosen).
