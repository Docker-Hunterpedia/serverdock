# ServerDock — UI

*Draft 1, 2026-11-20. AI-written; not yet confirmed by the maintainer.*

Wireframes: [PNG gallery in the repo](wireframes/README.md). The interactive canvas is a private claude.ai artifact.

The wireframes show the target product across all phases (e.g. Overview shows 3 servers, which needs Phase 2). Items that land after Phase 1 are labelled with their phase where it matters.

## Screens

| # | Screen | Purpose |
|---|---|---|
| 1 | Overview | All servers and services at a glance; "needs attention" banner; recent activity |
| 2 | Service detail | Live version (deploy #, SHA, image digest), tabs, deploy history with one-click rollback |
| 3 | Failed deploy | Stage-by-stage result, "your app is safe" message, log tail, likely cause, fix button |
| 4 | Phone incident | Alert with cause and 2–3 bounded actions, each confirmed by exact target |
| 5 | New service | Pick source (git, image, Compose, database, template) → form → create & deploy |
| 6 | Servers | One panel for every server; add a server with a one-time join command (agent connects out) |
| 7 | Domains | HTTPS status; diagnostics that say exactly what's wrong (DNS, port, rate limit) |
| 8 | Logs | Search across services + proxy, context lines, request tracing; live tail only while open |
| 9 | Databases & backups | Backup health proven by restore tests; restore with typed confirmation + 2FA + rehearse |
| 10 | AI & MCP | Approval queue with dry-run diff; MCP tokens; AI activity |
| 11 | First run | Setup token → admin + 2FA → server checks → connect GitHub → first deploy |
| 12 | Updates | ServerDock update (channel, release notes, signature, safe handover); Docker engine versions; app updates with policy and safe steps |
| 13 | Settings | General (timezone, builds, cache), users, security & tokens (expiry, key backup), backup storage, notifications, update policy, privacy (no telemetry), danger zone (backup, restore, uninstall) |

Gap-check features shown in the wireframes: local-folder source, registry login, volumes, web + worker processes, MongoDB/ClickHouse (New service); Docker resources view, disk cleanup preview, Cloudflare Tunnel, security checks (Servers); vulnerability scan and project export (Service detail); tunnel exposure (Domains); disk alert (Overview).

## Principles (shown in the wireframes)

- State is the product: show host, version, SHA, time; say `unknown` when unknown.
- State uses a word + symbol (● running, ▲ failed, ◆ needs action, ◌ unknown), never color alone.
- Failures always come with evidence and a next step.
- Destructive actions name the exact target; restore/terminal/secret reveal need fresh 2FA.
- The sidebar footer always shows ServerDock's own RAM/CPU.
- Works on a phone; 44 px touch targets; WCAG 2.2 AA.
