# ServerDock — Threat model (draft)

*Draft 1, 2026-11-20. AI-written; to be reviewed by the `security` agent and the maintainer in Phase 0.*

## What we protect

1. **Host control** — the agent's Docker socket access is root on every managed server.
2. **Secrets** — app env, database passwords, registry/git tokens, S3 keys, AI API keys.
3. **Availability** of user apps during deploys, upgrades and ServerDock failures.
4. **Integrity of history** — deploy records and audit log.

## Attackers

| Attacker | Example |
|---|---|
| Internet, unauthenticated | Scans the panel, webhook endpoint, ACME on-demand TLS |
| Low-privilege user | Developer/viewer role trying admin actions or host escape via Compose |
| Malicious repo / PR author | Public-repo PR preview, hostile Dockerfile, symlinks, build args |
| Compromised app container | Trying to reach the agent or other tenants |
| Compromised remote agent | Trying to attack the control plane or other servers |
| Prompt injection | Log lines or README text steering the AI assistant |
| Supply chain | Malicious dependency or tampered release |

## Rules (non-negotiable)

1. **No shell strings.** Docker API and compose-go only; exec with argv arrays; strict validators for names, image refs, domains, ports, git refs, paths. Fuzz the validators.
2. **Compose/Dockerfile = untrusted code.** Default-deny policy for `privileged`, host pid/network/ipc, docker.sock, `/`, `/etc`, devices, extra caps. Overrides are admin-only and audited.
3. **Agent has no listening ports.** Outbound only, enrollment token single-use, then Ed25519/mTLS identity; control plane treats agent messages as untrusted (an agent can only report on its own server).
4. **No stored SSH keys.** SSH is used once for install, then discarded.
5. **Secrets** encrypted at rest with a key outside the DB; never in logs, errors, URLs, or AI prompts; reveal requires fresh 2FA and is audited.
6. **Auth everywhere equal.** REST, WebSocket, terminal, MCP share one authz layer; CI runs an RBAC matrix over every route. Sessions revoked on password/2FA change. CSRF tokens, strict CSP, SameSite cookies, rate limits.
7. **Dangerous actions** (host terminal, delete, restore, secret reveal, destructive AI tools) need fresh 2FA and name the exact target.
8. **PR previews from forks** are off by default and never get secrets.
9. **AI**: assistant acts with the caller's permissions or less; writes need server-enforced approval; tool output is untrusted data; redact secrets before sending; no raw shell tool by default.
10. **Releases** signed (cosign) with SBOM and provenance; self-update verifies signatures; dependencies pinned and scanned (govulncheck, Dependabot).
11. **No unauthenticated windows**: setup needs the console token; restore needs auth and a signed archive.
12. On-demand TLS only for domains present in our DB.

## Lessons from competitors (see [research](research/))

Coolify and Dokploy 2026 CVEs: command injection through backups, build params, Traefik config, logs/terminal WebSockets, registry credentials, file mounts; unauthenticated RCE from public-repo PRs; cross-org authz bypass. Portainer: privileged plugin ops, git symlink file read, unauthenticated init via restore. 1Panel: agent cert verification RCE, X-Forwarded-For allow-list bypass, CSRF.

## Disclosure

`SECURITY.md` with GitHub private vulnerability reporting from the first public commit.

## Open items from the Phase 0 review (must be resolved before real deployments)

Found in the systems/security review of plan 0001 (2026-11-20). Acceptable for the spike; each is owned by the phase listed.

| # | Item | Phase |
|---|---|---|
| 1 | The agent bearer token travels over plain `ws://` on `:80`. Ship `wss://` (TLS) before any real deployment. | 1 |
| 2 | Caddy reloads drop every remote agent after `stream_close_delay` (5 min). Serve `/agent/connect` from a listener Caddy does not rebuild (hub with its own TLS listener, or a never-rebuilt server block). | 1 |
| 3 | The shared token gives no per-agent identity: any token holder can claim any non-reserved id and replace that agent. Enrollment + mTLS must bind the id to the credential (ADR 0003). The local agent's id is already reserved. | 2 |
| 4 | No session cap, per-IP limit, per-agent stream cap (yamux backlog 256) or auth-failure rate limit. | 1 |
| 5 | Non-streaming Docker API calls (inspect) have no timeout; add `ResponseHeaderTimeout`/per-call timeouts except for log follow. | 1 |
| 6 | No goroutine-leak tests; add `goleak.VerifyTestMain` to `hub`, `agent`, `cmd/serverdock` and join the local session goroutines. | 1 |
| 7 | The bench mounts `docker.sock` into the agent container (root-equivalent); keep it confined to the bench. | ongoing |
