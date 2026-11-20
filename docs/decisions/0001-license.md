# 0001 — License: AGPL-3.0

**Status:** accepted by the maintainer · **Date:** 2026-11-20

## Context
ServerDock is open source. Competitors: Coolify Apache-2.0 (no gating), Dokploy Apache-2.0 + source-available proprietary folders, Komodo GPL-3.0, Portainer Zlib with a paid Business Edition. A hosted offering may come later.

## Decision
**AGPL-3.0** for the project. Contributions use a DCO sign-off (`git commit -s`), not a CLA.

## Why
AGPL keeps it fully free for self-hosters, but anyone who runs a modified ServerDock as a service for others must publish their changes. That stops a cloud provider from taking it closed and reselling it.

## Consequences
- Some companies' legal teams avoid AGPL dependencies. That matters little for a tool you run rather than embed.
- Relicensing later needs agreement from every contributor.
- Open question: whether templates and client SDKs should be MIT so others can reuse them freely.
