# ServerDock

[![License: AGPL v3](https://img.shields.io/badge/license-AGPL--3.0-blue.svg)](LICENSE)
![Status: planning](https://img.shields.io/badge/status-planning%20(phase%200)-orange.svg)

**A self-hosted PaaS and server manager that runs on a 1 GB VPS beside your apps.** Deploy from git, images or Compose with automatic HTTPS, databases and backups, across many servers from one panel. Connect Claude (or any MCP client) to inspect and operate your servers, with every change waiting for your approval.

> **Status:** planning (Phase 0). No code yet. The docs below describe what we're building and why.

## Why

Self-hosted panels made "your own Heroku" popular, but in 2026 the popular ones idle at **~0.7–1.2 GB RAM**, burn CPU while doing nothing, and keep shipping critical command-injection bugs ([research](docs/research/01-coolify-dokploy.md)).

| | ServerDock target |
|---|---|
| **Light** | ≤ 80 MB RAM and < 1% CPU idle for panel + agent + proxy on one server; ≤ 50 MB per extra server. Published and checked in CI. |
| **Safe** | Typed Docker API operations, never shell strings. Agents connect out, so there are no open ports and no stored SSH keys. 2FA on dangerous actions, audit log, signed releases. |
| **Honest** | Real state, failures with evidence and a next step, automatic rollback when a deploy fails its health check. |
| **Simple** | One Go binary, one install command, SQLite inside. No Redis, no Swarm, no Kubernetes. |
| **AI with guardrails** | MCP endpoint first (read-only, then writes with dry-run + approval); a light built-in assistant later. Secrets never reach the model. |
| **Fully open** | AGPL-3.0, team features included, no upsell banners, no telemetry. |

## Roadmap

![Roadmap phases](docs/diagrams/roadmap-phases.svg)

Details and exit checks: [docs/roadmap.md](docs/roadmap.md).

## Docs

| Doc | What's in it |
|---|---|
| [Vision](docs/vision.md) | Problem, promise, audience, non-goals |
| [Features](docs/features.md) | Full feature list by phase, with where each idea comes from |
| [Architecture](docs/architecture.md) | Design, key decisions, footprint budget |
| [Roadmap](docs/roadmap.md) | Phases 0–5 with exit checks |
| [Security](docs/security.md) | Threat model and rules |
| [UI](docs/ui.md) · [Wireframes](docs/wireframes/README.md) | Screens, 13 wireframe images, UI principles |
| [Research](docs/research/) | Coolify & Dokploy, 10+ other tools, architecture options, final gap check |
| [Decisions](docs/decisions/) | ADRs |

## Contributing

See [CONTRIBUTING.md](CONTRIBUTING.md). Report vulnerabilities privately as described in [SECURITY.md](SECURITY.md). Claude Code agents and workflow: [CLAUDE.md](CLAUDE.md).

## License

[AGPL-3.0](LICENSE)
