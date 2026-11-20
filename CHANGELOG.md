# Changelog

All notable changes are listed here. Each release has an honest **Breaking changes** section.

## Unreleased

### Upgrade notes (M2)
- `--internal` is removed: the control plane listens on a unix socket (`--control-socket`, default `DATA/run/cp.sock`), not on TCP.
- `DATA` must be mode 0700 and owned by the user ServerDock runs as; the server refuses to start otherwise (`chmod 700 /var/lib/serverdock`).
- `:80` now only redirects to `:443`, where the panel is served with a self-signed certificate. Docker users must publish port 443 (`-p 443:443`); behind a port mapping such as `-p 8443:443` the redirect still points at 443. A systemd unit needs `AmbientCapabilities=CAP_NET_BIND_SERVICE` to bind 80 and 443 as a non-root user (the M7a unit will carry it).
- If the host's IP changes, name the panel with `--panel-host <name or address>` so the certificate stays valid.

### Added
- Planning docs: vision, features, architecture, roadmap, threat model, UI, research.
- ADRs 0001–0006 (AGPL-3.0, Go, outbound agent, embedded Caddy, SvelteKit, MCP-first AI); ADR 0007 accepted (no telemetry).
- Roadmap phase chart (`docs/diagrams/`).
- Claude Code agent team (`.claude/agents/`).
