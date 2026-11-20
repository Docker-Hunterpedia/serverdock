# Changelog

All notable changes are listed here. Each release has an honest **Breaking changes** section.

## Unreleased

### Upgrade notes (M2)
- `--internal` is removed: the control plane listens on a unix socket (`--control-socket`, default `DATA/run/cp.sock`), not on TCP.
- `DATA` must be mode 0700 and owned by the user ServerDock runs as; the server refuses to start otherwise (`chmod 700 /var/lib/serverdock`).
- `:80` now only redirects to `:443`, where the panel is served with a self-signed certificate. Docker users must publish port 443 (`-p 443:443`); behind a port mapping such as `-p 8443:443` the redirect still points at 443. A systemd unit needs `AmbientCapabilities=CAP_NET_BIND_SERVICE` to bind 80 and 443 as a non-root user (the M7a unit will carry it).
- If the host's IP changes, name the panel with `--panel-host <name or address>` so the certificate stays valid.

- Lost password or authenticator: stop the server and run `serverdock admin reset` as the user that owns the data directory (`sudo -u serverdock serverdock admin reset`). Docker: `docker run --rm -v <volume>:/data <image> admin reset --data /data --yes` with the server container stopped (`/data` is the path the image's `server --data /data` uses). It clears the admin credentials and signs everyone out; the next server start writes a new setup token to `DATA/setup-token`. It runs pending database migrations like a server start does.

### Added
- **M2: the panel, sign-in and the API** (plan 0003, ADR 0010):
  - the panel is served over HTTPS (a self-signed certificate of its own; `serverdock` prints its fingerprint at every start), `:80` only redirects;
  - first run: a one-time setup token written to `DATA/setup-token`, an admin account with a password (argon2id) and an authenticator app (TOTP), ten recovery codes;
  - sign-in with a second factor, sessions (12 h idle, 7 days at most), step-up confirmation for sensitive changes, back-off without lockout;
  - Settings › Security: the certificate fingerprint and dates, sessions (revoke, sign out everywhere else), API tokens (scoped, with an expiry, shown once), change password, replace the authenticator, new recovery codes;
  - scoped API tokens and a REST API over every action (`POST /api/v1/actions/{name}`) with an OpenAPI 3.1 description (`docs/api/openapi.json`);
  - `serverdock admin reset` for a lost password or authenticator;
  - the interface is a single-page app embedded in the binary (`serverdock version` shows `ui=spa`), checked for WCAG 2.2 AA with axe in eight browser configurations;
  - `DATA` is checked (0700, owned by the running user) and the TOTP seed is encrypted under `DATA/keys/master.key`: keep that file with your backups.
- Planning docs: vision, features, architecture, roadmap, threat model, UI, research.
- ADRs 0001–0006 (AGPL-3.0, Go, outbound agent, embedded Caddy, SvelteKit, MCP-first AI); ADR 0007 accepted (no telemetry).
- Roadmap phase chart (`docs/diagrams/`).
- Claude Code agent team (`.claude/agents/`).
