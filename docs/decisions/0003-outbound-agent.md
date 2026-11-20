# 0003 — Agents dial out; no SSH for operations

**Status:** accepted by the maintainer · **Date:** 2026-11-20

**Context.** Coolify/Dokploy run shell commands over SSH as root — the source of most of their 2026 CVEs and SSH key theft; SSH also fails behind NAT. Portainer Edge and Komodo use outbound agents.

**Decision.** Agent opens one outbound TLS WebSocket to the control plane, multiplexed with yamux; single-use enrollment token, then Ed25519/mTLS identity; typed RPCs only. SSH used only for an optional one-time install.

**Consequences.** Works behind NAT with no inbound ports or stored keys. Control plane must be reachable by agents (public address or tunnel). We own reconnect/backoff and version-skew logic.
