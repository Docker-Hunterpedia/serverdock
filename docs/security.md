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

## Open items from the Phase 0 review

Found in the systems/security review of plan 0001 (2026-11-20). Status after Phase 1 M1 ([plan 0002](plans/0002-phase-1-mvp-solo.md)); rows 1-2 and 4-6 are closed in code, tests and the bench.

| # | Item | Phase | Status |
|---|---|---|---|
| 1 | The agent bearer token travelled over plain `ws://` on `:80`. | 1 | **Closed (M1 step 5).** Agents connect over `wss` (TLS 1.3) to the hub's own listener (`--agent-listen`, off by default), pinning the hub CA by fingerprint ([ADR 0009](decisions/0009-agent-channel-tls.md)); `ws://` is loopback-only. The token is 43+ characters or generated into a 0600 file. |
| 2 | Caddy reloads dropped every remote agent after `stream_close_delay`. | 1 | **Closed (M1 step 5).** `/agent/connect` is served by the hub's own listener; the Caddy route and `StreamCloseDelay` are gone and `:80/agent/connect` answers 404. A test shows a Caddy reload keeps the session. |
| 3 | The shared token gives no per-agent identity: any token holder can claim any non-reserved id and replace that agent. Enrollment + mTLS must bind the id to the credential (ADR 0003). The local agent's id is already reserved. | 2 | **Open; Phase 1 mitigation:** remote agents are experimental, off by default and read-only (only `logs` is not `LocalOnly`, enforced in the hub for every op), the token is 256-bit, and a failed-auth limiter bounds guessing. A token holder can still replace another agent's session and read logs. |
| 4 | No session cap, per-IP limit, per-agent stream cap or auth-failure rate limit. | 1 | **Closed (M1 step 6)** except the yamux window bound below. 16 sessions (`--agent-max-sessions`, 503 before the upgrade); 4 TCP connections and 4 in-flight handshakes per IP; 5 auth failures/min per IP with burst 10 (429, checked before the token compare); 32 hub-opened streams per session; agent-opened streams closed at once; 32 streams per agent; header 64 KiB, body 1 MiB, 10 s header timeout; Caddy public server header/idle limits. |
| 5 | Non-streaming Docker API calls (inspect) had no timeout. | 1 | **Closed (M1 step 4).** Per-call deadlines (30 s; `stop?t=N` gets N + 15 s); log follow is bounded by its context only; API negotiation refuses Docker older than Engine 25 (API 1.44). |
| 6 | No goroutine-leak tests. | 1 | **Closed (M1 step 3).** goleak runs in the hub, agent, api and cmd/serverdock test mains (and dockerapi). |
| 7 | The bench mounts `docker.sock` into the agent container (root-equivalent); keep it confined to the bench. | ongoing | Open. |
| 8 | **yamux cannot bound a stream's receive window below 256 KiB** (the library rejects any `MaxStreamWindowSize` under its initial window). The plan's 64 KiB bound is not expressible. | 1 | **Needs a decision.** Worst-case memory an authenticated agent can pin at the hub is 16 sessions x 32 streams x 256 KiB = 128 MiB (the plan assumed ~40 MiB), only if the hub's reader of those streams stalls. Reachable only with the token; remote agents are off by default. Options: fewer streams per session, a lower session cap, or another multiplexer. |

## Debug access

`--debug-listen` and `--debug-allow-remote` are removed. `--debug-socket PATH` serves `/debug/*` (memory, agent list, log proxy) on a unix socket created 0600 inside a directory that must be 0700 and owned by the service user; the server refuses a looser directory. There is no TCP debug listener.
