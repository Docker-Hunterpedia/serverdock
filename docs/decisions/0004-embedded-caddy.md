# 0004 — Embedded Caddy as the proxy

**Status:** accepted by the maintainer · **Date:** 2026-11-20

**Context.** Traefik idles ~80–120 MB and its label/config paths caused Dokploy bugs and CVEs; Caddy ~20–40 MB with an atomic JSON config API and automatic HTTPS.

**Decision.** Embed Caddy as a Go library in the agent; generate full JSON config from desired state; on-demand TLS gated by our DB. Fallback: official Caddy container controlled over a Unix-socket admin API.

**Consequences.** Zero extra containers on managed servers; +20–30 MB binary size. Validate idle RSS in Phase 0 Spike 1; switch to the fallback if over budget.
