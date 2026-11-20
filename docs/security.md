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
