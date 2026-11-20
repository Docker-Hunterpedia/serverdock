# ServerDock — Threat model

*Draft 2, reviewed by `security` 2026-11-20 (M2 entry gate). AI-written; not confirmed by the maintainer. Draft 1 (Phase 0) is in git history.*

This draft matches the code on branch `phase-1-m1` at the close of M1 (steps 1–10: CI gate, goroutine-leak tests, Docker deadlines, the agent TLS listener, limits, migrations, the audit log, the action registry, and this record). See [M1 close-out status](#m1-close-out-status). It also adds the M2 surface from [plan 0002](plans/0002-phase-1-mvp-solo.md) (M2 section) so that plan 0003 and ADR 0010 start from a reviewed model.

**Status words** used in every table:
- **Built** means it is in the code on `phase-1-m1`, with tests.
- **Planned (Mx)** means it is a binding requirement in plan 0002 for milestone Mx. It is not built.
- **Accepted** means it is a risk we know about and choose to carry, with the reason given.
- **Gap Gn** means the plan does not cover it yet. See [Gaps](#gaps).

## What we protect

| # | Asset | Where it lives | Why it matters |
|---|---|---|---|
| 1 | **Host control** | The server process is in the `docker` group, and the local agent runs in the same process. A remote agent also holds its host's Docker socket. | Docker socket access is root. Code execution in the control plane means root on the host. |
| 2 | **Ingress availability** | The embedded Caddy runs **in the server process** (ADR 0004). | If the server process crashes or is OOM-killed, every app's HTTP(S) goes down, even though the containers keep running. |
| 3 | **Hub CA key** | `DATA/pki/ca.key` (0600, directory 0700) | Today it signs the hub leaf. In Phase 2 it signs agent client certificates. With this key an attacker can impersonate the hub to every pinned agent. |
| 4 | **Shared agent token** | `SD_AGENT_TOKEN`, or `DATA/agent.token` (0600) | It is the only credential on the agent channel until Phase 2. It lets the holder claim any non-reserved agent id (open item #3). |
| 5 | **Admin credentials (M2)** | Password hash (argon2id), TOTP seed, recovery-code hashes, in SQLite | Together they grant full control. |
| 6 | **Bearer secrets (M2)** | Session cookies, API tokens (`sd_…`) and the setup token (`DATA/setup-token`, 0600). Only hashes are stored. | Each one is an authenticated identity while it is valid. |
| 7 | **Secretbox master key (M2)** | `DATA/keys/master.key` (0600) | It decrypts every app secret (from M3b), S3 key and notification secret. |
| 8 | **Panel TLS key (M2)** | Path not named yet (Gap G10) | With it, an attacker can impersonate the panel on the IP. |
| 9 | **Integrity of history** | The hash-chained audit log (built, M1 step 8) and deploy records | Used for incident reconstruction. Tail truncation can only be detected against an older head hash (state backups, M7b). |
| 10 | **Release and CI integrity** | GitHub repository, branch protection, required checks, and the release signing key (Q5, M7a) | A tampered release, or a tampered self-update, gives root on every install. |
| 11 | **App data and state backups** | Docker volumes, `DATA/backups/`, S3 | These hold user data. State backups also contain items 3, 4 and 7, encrypted (M7b). |

## Deployed shape and trust boundaries (as built on `phase-1-m1`)

| Surface | Bound to | Who can reach it | Auth today | Status |
|---|---|---|---|---|
| Public HTTP `:80` (embedded Caddy, `internal/agent/proxy/proxy.go`) | all interfaces | Internet | none. It only answers `308` to `https://{host}[:port]{uri}` (the port of `--https-listen` as seen inside the process; wrong behind Docker port mapping) and never reaches the control plane. | Built (M2 step 3). The same Host allowlist applies on `:80`: any other `Host` gets 421, never a redirect to a name the client chose (`TestPanelRedirectHostAllowlist`, built in M2 step 6a). |
| Panel HTTPS `:443` (`--https-listen`, embedded Caddy) | all interfaces | Internet | TLS 1.2+ with the self-signed panel leaf (`DATA/pki/panel.pem`, never issued by the hub CA, fingerprint logged at every start); every handshake gets that leaf (`default_sni`/`fallback_sni` = `localhost`). Proxied to the control-plane socket. No auth surface until steps 5 to 8. The leaf's SANs include the host's addresses at creation; if the host's IP changes, name the panel with `--panel-host` (a stable name or address), which regenerates the leaf. | Built (M2 step 3). |
| Control-plane HTTP on the unix socket `--control-socket` (default `DATA/run/cp.sock`, `cmd/serverdock/socket.go`) | no TCP listener; socket 0600 in a 0700 directory owned by the service user, at most 256 connections | Caddy, and the service uid | none yet (the M2 auth surface, steps 5 to 8). Every request must carry exactly one `X-Forwarded-For` value, which only Caddy sets. | Built (M2 step 2, G2). |
| Agent listener `--agent-listen` (`server.go:220`, `internal/hub/hub.go`) | the configured address; **off by default** | Internet when enabled | TLS 1.3 with the hub leaf (pinned CA, role URI SAN), then a shared bearer token checked in constant time after the per-IP failure limiter | Built (ADR 0009, M1 steps 5–6) |
| Debug unix socket `--debug-socket` (`cmd/serverdock/socket.go`) | a filesystem path; off by default | the service uid only (0600 socket in a 0700 directory owned by the service user) | filesystem permissions. It can stream **any container's logs** through the hub. | Built |
| Local agent session (`net.Pipe`, `server.go`, `hub.Attach(LocalID)`) | in-process memory | nobody outside the process | `LocalID` is reserved: a remote agent can't claim it in any letter case. Only `runServer` attaches it. | Built |
| Docker Engine socket | `/var/run/docker.sock` | the server process (and a remote agent on its own host) | `docker` group | Built. Root-equivalent by design. |
| Data directory | `/var/lib/serverdock` by default, created **0700**; an existing one must be owned by the running uid with no group or other bits, or the server refuses to start (`internal/datadir`) | the service user only | filesystem permissions | Built (M2 step 1). See G6. |
| CI (`.github/workflows/ci.yml`) | GitHub Actions | PR authors, including forks (read-only token) | Actions pinned by SHA; `permissions: contents: read`; `actions: read` only on `changes`; inputs passed through `env:`; never `pull_request_target`; CODEOWNERS on `.github/`; fail-closed `bench-gate` | Built (M1 step 1) |
| Release pipeline, install script, self-update | — | — | — | Not built (M7a, M7b) |

**M2 adds** these surfaces (planned):
- `:443` on the server IP, served by Caddy with a self-signed ECDSA panel leaf. It is never issued by the hub CA (ADR 0009 §3, ADR 0010).
- `:80` only redirects to HTTPS until M4.
- The setup endpoint, login, TOTP and recovery codes.
- Session-authenticated REST and WebSocket routes, and API-token REST routes.
- The console command `serverdock admin reset`.

**Trust direction on the agent channel.** The agent trusts the hub completely: a compromised control plane controls every server. This is accepted and inherent to the design. The hub trusts nothing an agent sends. An agent may only answer the streams the hub opens, and Phase 1 remote agents get `logs` only.

## Attackers

| ID | Attacker | Example |
|---|---|---|
| A1 | Internet, unauthenticated | Scans `:80`, `:443`, `:7443`; brute-forces login or the setup token; floods connections |
| A2 | On-path network attacker | Sits between the admin's browser and the panel during first run on a self-signed certificate, or between an agent and the hub |
| A3 | Holder of the shared agent token / compromised remote-agent host | Has root on a remote node (or read its environment) and talks to the hub as any agent id |
| A4 | Compromised app container / malicious image | Code running in a user's container, writing hostile log output, probing the host network |
| A5 | Local unprivileged user on the server host | A shell account or another service on the same VPS that is not in the `docker` group |
| A6 | Holder of a stolen session cookie or API token (M2) | XSS, token leaked from a CI system, laptop theft |
| A7 | Hostile web page in the admin's browser | CSRF, DNS rebinding, clickjacking, cross-origin WebSocket |
| A8 | Malicious repo / PR author (M5, M6) | Hostile Dockerfile, Compose file, symlinks, build args, webhook replay |
| A9 | Prompt injection (M9, and Phase 3 AI) | Log lines or README text steering an MCP client that reads ServerDock output |
| A10 | Supply chain and CI | A malicious dependency, a compromised Action, a tampered release, or a stolen signing key |
| A11 | Console operator (trusted) | Root or the service user on the host. Listed because the recovery paths (`admin reset`, `state restore`) exist for them and must not be reachable by anyone else. |

## Threats and mitigations by attacker

### A1 — Internet, unauthenticated

| Threat | Mitigation | Where | Status |
|---|---|---|---|
| Plain-text agent token on `ws://` | wss with TLS 1.3 only; `ws://` accepted only for loopback IP literals | `internal/agent/client.go` `ValidateURL`, `internal/pki` | Built (open item #1) |
| Agent channel dropped by Caddy reloads | Own listener, not behind Caddy | `server.go`, ADR 0009 | Built (open item #2) |
| Guessing the agent token | 256-bit generated token; a hand-made one needs ≥ 43 chars and ≥ 16 distinct; 5 failures/min per IP (burst 10) **checked before the compare**; one log line per IP per minute | `internal/hub/token.go`, `hub.go` `ServeHTTP` | Built |
| Connection and handshake floods on `:7443` | 4 TCP connections per IP and 128 total, closed in `Accept` before TLS; 5 s handshake/header timeout; no keep-alives | `internal/limits`, `server.go` | Built. **Accepted:** a botnet can hold the 128 slots and block legitimate agents. It doesn't touch the panel. |
| Too many sessions | 16 sessions (503 before upgrade) | `hub.go` `reserve` | Built |
| Header and body abuse on HTTP | 64 KiB header limit, 1 MiB body, 10 s header timeout; Caddy `read_header` 10 s, `idle` 2 m | `internal/api/api.go`, `proxy.go` | Built |
| Unauthenticated debug routes over TCP | TCP debug listener and `--debug-allow-remote` removed; unix socket only | `cmd/serverdock/socket.go` | Built |
| Setup takeover: someone else completes first run (Portainer's init lesson) | Setup token: ≥ 128 bits, single use, stored hashed, compared in constant time, 1 h expiry, sent in the POST body only, file 0600, only its path is logged; setup returns 404 once an admin exists | M2 `internal/auth` | Planned (M2). Atomicity and the post-reset state are **Gap G4**. |
| Password brute force | argon2id (19 MiB, t = 2); per-IP back-off (IPv6 keyed by /64) checked **before** hashing; one hash at a time with a 2 s bounded wait, then a fast 503 | M2 | Planned (M2) |
| TOTP brute force once the password is known | ±1 step; last counter stored, so codes are never reused; per-account slowdown across IPs with an admin notice | M2 | Planned (M2). The example rate is too loose: **Gap G3**. |
| Per-IP back-off evaded by rotating IPv6 /64s, or by evicting blocked keys | /64 keying; map capped at 4096 keys | `internal/limits` | Built. **Gap G3**: a /48 holds 65,536 /64s (M2: a second per-/48 bucket). **Built (M1):** `evict` never drops a currently blocked bucket; when every key is blocked a new key is refused and `Blocked` reports it as blocked (fail closed). |
| Login availability: distributed attempts keep the single argon2 slot busy | Per-IP limit before hashing; existing sessions keep working; the console can `admin reset` | M2 | **Accepted.** Login can be denied while an attack runs; no account is taken over. |
| User enumeration | One admin; identical responses and timings | M2 | Planned (M2) |
| Old default credentials (CapRover's lesson) | No default password; first run needs the console token | M2 | Planned (M2) |
| ACME on-demand abuse | On-demand TLS only for domains in our DB (rule 12) | M4 | Planned (M4) |

### A2 — On-path network attacker

| Threat | Mitigation | Where | Status |
|---|---|---|---|
| Impersonating the hub to an agent | The agent pins the CA by SHA-256; verifies the leaf against that CA only, for the dialed host, with EKU `serverAuth`; requires `urn:serverdock:role:hub`; never consults system roots | `internal/pki/client.go` | Built |
| A leaf for another purpose accepted as the hub | Role URI SAN; CA has `MaxPathLenZero`; the panel leaf is never issued by the hub CA | `internal/pki/pki.go` | Built; panel part planned (M2) |
| Agent bearer token moved by an HTTP redirect | Only a party with a leaf from the pinned CA can send one | `client.go` (`CheckRedirect` refuses every redirect) | **Built (M1)** (G12) |
| MITM of the first run on the self-signed panel: relaying the setup token, password and **TOTP seed** | The console prints the leaf fingerprint; onboarding asks the user to compare it with the browser's certificate details before entering the token | M2, ADR 0010 | Planned (M2). It relies on users comparing fingerprints. **Gap G5.** |
| Cookies or tokens over plain HTTP | `__Host-` cookie, `Secure`; `:80` redirects only; API tokens accepted only from the `Authorization` header | M2 | Planned (M2) |
| Users trained to click through certificate warnings forever | — | — | **Gap G15** (M4) |

### A3 — Shared-token holder / compromised remote agent

| Threat | Mitigation | Where | Status |
|---|---|---|---|
| Claiming another agent's id, replacing its session, feeding fake logs | Remote agents are off by default and experimental; `LocalID` is reserved in every case | `hub.go` | **Accepted until Phase 2** (open item #3; per-agent mTLS identity) |
| Receiving write or secret-bearing ops | Op table where the zero value is local-only; `logs` is the only `RemoteOK` op; unknown ops refused; enforced in `Session.open`; a test pins the allowlist | `internal/agent/protocol.go`, `hub.go` `open` | Built (rule 9) |
| Pinning hub memory with agent-opened streams | The first agent-opened stream ends the session; the hub opens at most 8 streams per remote session; window fixed at 256 KiB (32 MiB worst case) | `hub.go` `attach` | Built (open items #4, #8) |
| **Crashing the control plane with a yamux ping flood**: each ping SYN makes yamux start a goroutine that waits up to 10 s to queue its reply. An agent that sends pings and never reads makes these pile up without limit. | none | `hub.go:228` (`yamux.Client` on the raw conn), `internal/agent/session.go:36` | **Gap G1 (High), built in M1**: measured at 640k goroutines / 2 GiB in 1 s over an in-memory pipe (1.88M goroutines with the guard removed in the regression test). **Fix:** `agent.GuardPings` wraps the connection on both ends (`hub.attach`, `ServeSession`), parses yamux headers, and closes the connection past 4 ping SYNs/s (burst 16), which bounds the goroutines yamux starts per ping (`ConnectionWriteTimeout` stays at yamux's 10 s). Regression test: `TestPingFloodEndsTheSessionAndGoroutinesStayBounded`. |
| Oversized WebSocket messages | `websocket.NetConn` disables the read limit but streams the data; yamux bounds each stream's window and closes the session when a window is exceeded | library | Built (library behaviour) |
| Hostile log bytes reaching the operator: terminal escape sequences through the debug route; later the browser and MCP | Only raw bytes, to the service uid | `hub.go` `DebugRoutes` | **Gap G13** (M8, M9) |
| Reading other nodes' data | The hub never forwards data between agents | `hub.go` | Built |

### A4 — Compromised app container / malicious image

| Threat | Mitigation | Where | Status |
|---|---|---|---|
| Reaching the control plane directly | The control plane has no TCP listener (a unix socket in a 0700 directory); containers are in their own network namespace; host network is denied by policy | `socket.go`; M3a policy | Built (M2 step 2: `--internal` is removed, so there is no non-loopback case). Planned (M3a policy). |
| Escaping through Compose or service config (`privileged`, docker.sock, host pid/net/ipc, devices, caps, `serverdock.*` labels) | Default-deny policy on the resolved model; label spoofing rejected | M3a, M3b, M6 | Planned |
| SSRF from ServerDock fetches (git, S3, webhooks, notifications) toward metadata, loopback, Docker gateways or RFC 1918 | `internal/netpolicy` checks the dialed IP in `DialContext`; per-target audited allowlist | rule 10; M5, M6, M8 | Planned. **Accepted** (documented in M5): the BuildKit DNS-rebinding window. |
| Hostile log output (XSS, escape sequences, prompt injection) | — | — | **Gap G13** |
| Reaching the agent listener | Needs TLS and the token, like A1 | — | Built |

### A5 — Local unprivileged user on the host

| Threat | Mitigation | Where | Status |
|---|---|---|---|
| Calling the control plane on `127.0.0.1:7070` directly, skipping Caddy, and **forging `X-Forwarded-For`** to defeat per-IP back-off (1Panel CVE-2025-66508 class) | `ClientIP(r)` reads `X-Forwarded-For` only on a connection marked "from our proxy" (`ConnContext`, set only for the control socket), and only as exactly one value holding one IP; otherwise it is ignored (RemoteAddr). Loopback is trusted for nothing. Caddy has no `trusted_proxies`, so it replaces a client's value. | `internal/limits`, `internal/api` `WithTrustedProxy`, `cmd/serverdock/socket.go` | **Built (M2 step 2, G2):** `TestForgedXFFOnSocketIgnoredByCaddy` (real Caddy), `TestXFFIgnoredWithoutSocketMark`, `TestMultipleXFFValuesRejected`, `TestProxyMarkIsPerListener`, `FuzzParseForwarded`. |
| Reading the DB, keys or tokens | `DATA/pki` 0700; key and token files 0600; the debug socket directory must be 0700 and owned by the service user | `pki.go`, `token.go`, `socket.go` | Built. **Gap G6, partly built (M2 step 1):** `DATA` is 0700 and checked at start (`datadir.Prepare`), and the process umask is 0077 (`serverMain`), so SQLite's `-wal`/`-shm` and Caddy storage are private. The TOTP seed's secretbox use arrives in step 5. |
| Reading `SD_AGENT_TOKEN` from the environment (`systemctl show` prints `Environment=` to any user) | The generated-token file is the default when no env var is set | `server.go:113`, `agent.go:72` | **Gap G11** (M7a) |
| Using the debug socket | 0600 socket, 0700 directory owned by the service user, checked at start | `socket.go` | Built |
| Running `admin reset` or `state restore` | Console only; takes the data-dir flock (refuses while the server runs); needs write access to `DATA` | M2, M7b | Planned |

### A6 — Stolen session or API token (M2)

| Threat | Mitigation | Where | Status |
|---|---|---|---|
| Session theft through XSS | `HttpOnly`; strict hash-based CSP; no inline script; SPA output-encodes (Svelte default) | M2 | Planned |
| Long-lived sessions | Idle and absolute expiry; revoked on password or 2FA change; `admin reset` revokes all | M2 | Planned. **Gap G7**: pruning (Coolify #7394: 58 GB sessions table). |
| Session fixation and the half-logged-in state between password and TOTP | — | — | **Gap G4** |
| Token used for dangerous actions | Tokens are refused for destructive and sensitive actions (Q8 default); `SessionOnly` for token, auth and user management; tokens created or rotated only from a session with fresh 2FA, audited; expiry required (default 90 days); scopes `read` < `write` < `admin` | `internal/actions` (M1 step 9), M2 | **Built (M1 step 9):** the policy table, `SessionOnly`, and tokens refused for destructive and sensitive actions, each cell tested. **Planned (M2):** token minting rules and expiry. |
| Dangerous actions with a stolen session | Fresh 2FA (5 min) for sensitive and destructive actions; destructive actions also need `Confirm == Target` | `internal/actions` (M1 step 9), M2 | **Built (M1 step 9)** in the registry (`Confirm == Target`, a fresh-2FA window checked against the registry clock). **Planned (M2):** the 2FA flow that sets the window. |
| An HTTP or MCP request reaching the `system` actor | One-time `SystemInvoker` handle; import test; route-matrix assertion | `internal/actions` (M1 step 9), M2, M9 | **Built (M1 step 9):** the one-time `SystemInvoker`, `InvokeAs` as the only way to set the executor, and the import test. **Planned (M2, M9):** the route-matrix assertion. |
| Secrets in logs, errors or audit rows | `internal/redact`; sentinel tests | `internal/redact`, `internal/audit` (M1 steps 8–9), M2, M3b | **Built (M1):** redaction reaches secrets in slices, maps, pointers and embedded structs; a type that marshals itself cannot hide a secret (Register refuses it); sentinel tests search the database bytes and the logs. **Planned (M2, M3b):** the M2 secrets and the secret store. **Gap G9**: M2 sentinels named for the setup token only. |

### A7 — Hostile web page in the admin's browser

| Threat | Mitigation | Where | Status |
|---|---|---|---|
| CSRF | `SameSite=Strict` plus an `X-SD-CSRF` header token | M2 | Planned. **Gap G8**: which auth method wins when both a cookie and `Authorization` are present. |
| Cross-site WebSocket hijacking | Session required, plus an exact `Origin` match against the configured panel origins | M2 | Planned |
| Clickjacking | `X-Frame-Options: DENY` (and CSP `frame-ancestors 'none'` in M2) | `internal/api/api.go` | Built (header); CSP planned (M2) |
| DNS rebinding against the panel on the IP | Cookies are host-bound; over HTTPS the certificate name won't match | M2 | **Gap G8**: no `Host` allowlist |

### A8 — Malicious repo or PR author (M5, M6)

Planned, and outside the M2 gate:
- Webhook HMAC verified before parsing, a lazy replay cache, and a 2 MiB cap (M5).
- No fork builds; PR previews are Phase 2 and off by default.
- BuildKit without `security.insecure` or `network.host` (M5).
- Compose path confinement, no remote `include`, interpolation never reads the process environment, and the full deny-list (M6).
- Symlink-safe clones (M5).

### A9 — Prompt injection (M9 and later)

Planned:
- MCP is read-only in Phase 1, scoped `read` tokens only, output redacted and marked untrusted, and audited with the `mcp` actor (M9).
- Writes need approval in the panel, enforced on the server (Phase 2, ADR 0006).

Log text reaching MCP clients is covered by G13.

### A10 — Supply chain and CI

| Threat | Mitigation | Where | Status |
|---|---|---|---|
| Compromised third-party Action, or a mutable tag | Every Action pinned to a commit SHA | `ci.yml` | Built |
| Workflow token abuse from fork PRs | Top-level `contents: read`; no `pull_request_target`; event values through `env:` only | `ci.yml`, rule 11 | Built |
| Gaming the bench gate (reusing a result from another workflow or app) | `gh run list --workflow ci.yml`; pathspec-based docs-only check; fail-closed gate; required check pinned to the GitHub Actions app (maintainer setting) | `.github/scripts/bench-needed.sh` | Built |
| Changing CI without review | CODEOWNERS on `/.github/` plus "require code owner review" (maintainer setting) | `.github/CODEOWNERS` | Built (file); setting per maintainer |
| Vulnerable dependency | `govulncheck` (pinned version) on every PR | `ci.yml` | Built. **Gap G14**: no Dependabot or Renovate config. |
| Bench image with the Docker socket | Base images pinned by digest; the socket is mounted only in bench scenarios 3–4 | `bench/` | Open item #7 (ongoing) |
| Tampered release or self-update | A signed manifest binds version, OS/arch, file name and SHA-256; embedded key set (current + next); install refuses without verification; downgrade refusal; trial boot with rollback | M7a (ADR 0016), M7b (ADR 0017) | Planned. **Gap G14**: SBOM, provenance, and the cosign wording in rule 10. |
| Stolen release signing key | Hardware key held by the maintainer (Q5a) or a protected `release` environment (Q5b); key rotation through the "next" key | M7a | Planned (Q5 open) |
| Tampered `install.sh` | Served over HTTPS from the repository | M7a | **Accepted** (common `curl \| sh` trust model; the binary it fetches is verified) |

### A11 — Console operator (recovery paths)

| Threat | Mitigation | Where | Status |
|---|---|---|---|
| `admin reset` used by a non-root local user | Needs the data-dir flock and write access to `DATA` | M2 | Planned |
| `admin reset` leaving the panel half-configured, or reopening setup to the network without a token | It clears the password and TOTP, revokes sessions and tokens, writes an audit row through the system invoker, and writes a new 0600 setup token | M2 | Planned. **Gap G4**: the state machine for "admin exists". |
| `state restore` from a forged archive | AEAD archive needs the passphrase; newer schemas refused; console only | M7b | Planned |

## M2 surface (summary of the planned design, for plan 0003)

Plan 0003 and ADR 0010 must carry every row below, plus Gaps G2–G10, each with a test.

| Item | Requirement (plan 0002 M2) | Gap to add |
|---|---|---|
| Panel TLS | Self-signed ECDSA leaf of its own, never from the hub CA; leaf SHA-256 printed on the console; users are never told to install a CA; `:80` redirects only | G5, G10 |
| Setup token | ≥ 128 bits from `crypto/rand`, hashed, constant time, single use, 1 h expiry, regenerated on restart while no admin exists, POST body only, `DATA/setup-token` 0600 removed once used, only its path logged, 404 once an admin exists | G4 | **Built (M2 step 5, store level; the HTTP endpoints are steps 6a and 6b):** migration `0003_auth.sql`; setup is read-only checks, then argon2, then one compare-and-delete transaction (`TestSetupChecksBeforeHash`, `TestSetupCompleteIsAtomic` with a fault after every statement, `TestConcurrentSetupOneWinner`, `TestAdminExistsDefinition`, `TestSetupReusesAResetUserRow`); the pending-2FA state is not a session (`TestPendingIsNotASession`, `TestPendingTokenFiveTries`, `TestPendingPrunedAndCapped`); a new session id at sign-in and the old one revoked (`TestLoginMintsNewSessionIDAndRevokesTheOldOne`); `TestTOTPCounterNeverReused`, RFC 6238 vectors, ±1 step. **Endpoints (M2 step 6b):** setup, login, 2FA, step-up and logout over HTTP with audit rows (intent first; a failed intent write is 503 `audit_unavailable` and mints nothing: `TestAuthIntentFailureFailsClosed`; failures are `anonymous:*` denials, and the user name is recorded only if the user exists: `TestFailedLoginAuditRowsAndUsernames`); `TestFirstRunOverHTTP`, `TestLoginTwoStepAndSessionRotation`, `TestOldSessionRevokedAtLoginAndClientChosenIDRefused`, `TestPendingPlusValidSessionOn2FA`, `TestStaleSessionCookieOnPreSessionEndpoints`, `TestCrossSiteOriginIsRefusedOnEveryPreSessionPost`.
| Admin + TOTP | argon2id with m = 19 MiB, t = 2, p = 1; one hash at a time with a 2 s wait then a fast 503; per-IP check first; RFC 6238 ±1 step; last counter stored; 10 hashed recovery codes | G3, G4, G6 |
| Back-off | Per-IP exponential back-off (IPv6 /64) on login, TOTP, setup and recovery; per-account TOTP slowdown with an admin notice; no hard lockout; XFF only from loopback, and Caddy must **replace** client XFF (test required) | G2, G3 |
| Sessions | Opaque 256-bit `__Host-` cookie (`HttpOnly; Secure; SameSite=Strict`), hashed, idle and absolute expiry; fresh 2FA lasts 5 min; WebSockets need a session and an exact `Origin` | G4, G7 |
| CSRF | `SameSite=Strict` plus an `X-SD-CSRF` header | G8 | **Built (M2 step 6a):** one route table (`internal/api/routes.go`, a test fails on any other mux registration), credential resolution by route kind with stale cookies cleared, CSRF (HMAC of the session secret), Origin on every unsafe request, JSON-only POSTs, the Host allowlist (421), security headers and a CSP from the embedded `index.html`: `TestRouteMatrix` (every route x 24 credential cases), `TestEveryRouteIsInTheMatrix`, `TestHostAllowlist`, `TestDebugRoutesAreOnlyOnTheDebugSocket`, `TestNoMuxRegistrationOutsideTheRouteTable`, `TestCSPHashesMatchTheEmbeddedIndex`, `TestStaleCookieOnPublicRouteIsIgnored`.
| API tokens | `sd_…`, hashed, scoped, expiry required, `Authorization` header only, created only from a session with fresh 2FA, `SessionOnly` management, denied for destructive and sensitive actions, revoked on password or 2FA change | G8 |
| `admin reset` | Console only, flock, clears credentials, revokes everything, audit row, new setup token | G4 |
| Route matrix | Every route declares its auth, including the debug socket routes; no route produces a `system` actor | — |
| Secretbox | AES-256-GCM, AAD = table/row/column, key id prefix, `DATA/keys/master.key` 0600 | G6 | **Built (M2 step 5):** the TOTP seed is sealed (`TestTOTPSeedNotInDBPlaintext`, including the WAL), `TestWrongKeyIDClearError`, `TestMissingKeyWithCiphertextsRefusesStart`, `TestSwappedKeyRefusesStart`, `TestFileModesAfterFirstRun` (built binary).
| SPA | Strict hash-based CSP; no Node at runtime | — |
| Secrets in output | Sentinel tests over DB files, logs and bundles | G9 |

## Rules (non-negotiable)

1. **No shell strings.** Use the Docker API and compose-go only. Exec takes argv arrays. Use strict validators for names, image refs, domains, ports, git refs and paths, and fuzz them. *(Built so far: container id, agent id, tail and op-line validation.)*
2. **Compose and Dockerfiles are untrusted code.** A default-deny policy blocks `privileged`, host pid/network/ipc, docker.sock, `/`, `/etc`, devices and extra caps. Overrides are admin-only and audited.
3. **Agents have no listening ports.** They dial out over TLS 1.3 to a pinned hub CA. The control plane treats every agent message as untrusted: an agent may only answer streams the hub opens, and only report on its own server. *Phase 1:* a shared token, remote agents logs-only (rule 9 of plan 0002). *Phase 2:* a single-use enrollment token, then a per-agent mTLS identity (`clientAuth`-only certificates from the hub CA).
4. **No stored SSH keys.** SSH is used once for install, then discarded.
5. **Secrets** are encrypted at rest with a key outside the DB. They never appear in logs, errors, URLs, audit rows or AI/MCP output. Revealing one requires fresh 2FA and is audited intent-first.
6. **Auth is equal everywhere.** REST, WebSocket, terminal and MCP share one authz layer (the action registry). CI runs an RBAC matrix over every route, including the debug socket's. Sessions are revoked on password or 2FA change. CSRF tokens, a strict CSP, SameSite cookies and rate limits apply.
7. **Dangerous actions** (host terminal, delete, restore, secret reveal, destructive AI tools) need fresh 2FA and name the exact target.
8. **PR previews from forks** are off by default and never get secrets.
9. **AI and MCP** act with the caller's permissions or less. Writes need approval enforced on the server. Tool output is untrusted data. Secrets are redacted before anything is sent. There is no raw shell tool by default.
10. **Releases** are signed: a signed manifest per artifact binding version, OS/arch, file name and SHA-256, verified with an embedded key set (ADR 0016, M7a). Self-update verifies the signature before any swap and refuses downgrades. Dependencies are pinned and scanned (govulncheck; dependency update bot, G14). SBOM and provenance: decided in ADR 0016 (G14).
11. **No unauthenticated windows.** Setup needs the console token. Restore is console-only, from an authentic archive.
12. **On-demand TLS** only for domains present in our DB.
13. **Every limiter fails toward refusing**, never toward allowing. A limiter that evicts state must not evict an active block (G3).

## Gaps

These are found in this review and not yet covered by plan 0002. Each has a severity and a recommended owner milestone. The fix belongs in that milestone's plan, with a test.

| # | Severity | Gap | Owner | Recommended fix |
|---|---|---|---|---|
| G1 | **High** (**built in M1**) | **yamux ping flood.** yamux answers each ping SYN in a new goroutine that waits up to `ConnectionWriteTimeout` (10 s). An agent-token holder that sends pings and never reads creates goroutines without limit; a scratch reproduction reached 640k goroutines / 2 GiB in 1 s. The control plane is OOM-killed, and the in-process Caddy, with it all app ingress, goes down. The same applies to any other session built on `YamuxConfig`. | **M1** (before M1 closes; it belongs to open item #4) | Wrap the hub-side `net.Conn` passed to `yamux.Client` (`hub.go:228`) in a small frame reader that tracks yamux headers (12 bytes; skip data payloads by length). Allow e.g. 4 ping SYNs/s, burst 16, and end the session past that (log once). Optionally also bound window-update and go-away frames. Lower `ConnectionWriteTimeout` for remote sessions (e.g. 5 s). Add a regression test: an agent that floods pings without reading leaves the hub's goroutine count bounded and the session closed. |
| G2 | Medium | **The control plane trusts loopback.** `127.0.0.1:7070` is plain HTTP and reachable by any local process. With `ClientIP(r, true)` that process can forge `X-Forwarded-For` and bypass per-IP back-off. `--internal` also accepts a non-loopback address, which would expose the panel over plain HTTP. | M2 | Preferred: serve the control plane to the in-process Caddy over a **unix socket in a 0700 directory** (Caddy dials `unix/…`), and drop the TCP listener. Then trust XFF only on that socket. Otherwise refuse non-loopback `--internal`, and authenticate Caddy with a per-boot random header that Caddy sets and strips from client requests; trust XFF only when it matches. Test: a direct request with forged XFF is keyed by its real peer. **Built (M2 step 2)** as the preferred option: `TestNoTCPControlListener`, `TestControlSocketModes`, `TestListenPrivateSocketRules`, `TestControlSocketConnectionCap`, `TestControlAndDebugPathsMustDiffer`, `TestForgedXFFOnSocketIgnoredByCaddy`, `TestXFFIgnoredWithoutSocketMark`, `TestMultipleXFFValuesRejected`; the remote agent's Caddy config is pinned by `TestAgentBuildConfigGolden`. |
| G3 | Medium (the M1 part, "never evict a blocked bucket", is **built**) | **TOTP and limiter strength.** At the plan's example "10/min per account", an attacker who has the password wins in about 4% of days (14,400 tries × 3 valid codes / 10⁶). Per-IP back-off is defeated by rotating /64s from one /48. `Keyed.evict` (`limits.go:95`) can evict a bucket that is currently blocked. | M2 | Per-account exponential back-off on TOTP and recovery codes: 5 free attempts, then doubling delays up to a 1 h cap, and the password must be entered again after every 5 TOTP failures. Notify the admin on "password correct, TOTP failed" and advise a password change. Add a second per-/48 bucket for IPv6. Never evict a blocked bucket; when the map is full of blocked keys, refuse new auth keys with 429 (an availability trade-off, recorded). | **Built (M2 step 4, the limiter and hasher core; the wiring is step 6b):** `limits.Backoff` (lazy, no timer; oldest-first eviction of unblocked keys, blocked keys never evicted, a full map refuses new keys, `bypassFull` for known IPs) and `limits.Key48`: `TestBackoffDoublingAndCap`, `TestBackoffNeverEvictsBlocked`, `TestBackoffEvictsUnblockedOldestFirst`, `TestBackoffFullOfBlockedRefusesNewKey`, `TestBackoffBypassFullAdmitsAKnownKey`, `TestBackoffForgetsAfter24h`, `TestBackoffInstancesIndependent`, `TestUnknownUsernamesShareOneBucket`, `TestIPv6Slash48Bucket`; `auth.Hasher` (argon2id m=19 MiB t=2 p=1, one hash at a time, four waiters, 2 s, then `ErrBusy`): `TestHasherFast503`, `TestHasherWait503`, `TestFreeOSMemoryNotOnTheResponsePath`, `TestDummyHashWithin20PercentOfARealOne`, PHC parsing with bounds and `FuzzParsePHC`. **Wired (M2 step 6b):** the four instances (`api.NewLimits`: /64, /48, account password, account 2FA) in the stated order, one shared `auth.Hasher`, the `<unknown>` bucket for non-existent and reset users, known-IP bypass: `TestBackoffCheckedBeforeHash`, `TestBackoffInstancesIndependentOverHTTP`, `TestUnknownUsernamesShareOneBucketOverHTTP`, `TestSecondFactorBackoffAcrossIPs`, `TestKnownIPExemptFromAccountBackoff`, `TestKnownIPReachesHashWhenMapsFullOfBlocked`, `TestReauthFailuresRevokeSession`, `TestAdminNoticeAfterTOTPFailures`, `TestNoUserEnumeration`, `TestLoginBusyHasher`.
| G4 | Medium | **Auth state machine not specified.** (a) The half-authenticated state between a correct password and TOTP. (b) Setup atomicity: two concurrent setup requests, or an admin committed before TOTP is confirmed. (c) What "an admin exists" means after `admin reset` (the setup endpoint must reopen only with the new token). (d) Session id rotation at login. | M2 | (a) The password step returns a short-lived (5 min), single-use, hashed "pending-2FA" token with at most 5 TOTP tries. It is not a session. The session cookie is minted only after TOTP. (b) Setup verifies a TOTP code from the new seed, then commits admin, TOTP and recovery codes in one transaction, consuming the token in the same transaction (compare-and-delete). (c) Define "admin exists" as "an admin with a password and confirmed TOTP". (d) Always issue a new session id at login, and never accept a client-chosen id. |
| G5 | Medium | **First-run MITM on the self-signed panel.** Fingerprint comparison is the only defence, and few users do it. An on-path relay captures the setup token, password and **TOTP seed**, which means a lasting takeover. | M2 (ADR 0010) | Document an SSH-tunnel first run as the recommended path. After setup, show "admin created from <IP> at <time>" on the console, in the journal (no secrets) and on the Security page. Recommend re-enrolling TOTP once the panel domain has a real certificate (M4). Record the residual risk as accepted in ADR 0010. | **Built (M2 step 5):** the setup token is 256 bits, hashed, 1 h, written to `DATA/setup-token` (0600) at every start while no admin exists and removed after setup; the start-up line has the path, expiry, panel fingerprint and the SSH-tunnel hint and never the token (`TestStartupLogHasPathFingerprintAndTunnelHint`, `TestServerWritesSetupTokenAndLogsOnlyItsPath`, `TestSetupTokenFileModeAndRemoval`). **Endpoints (M2 step 6b):** `TestSetupStatusNeverReturnsFingerprint`, `TestAdminCreatedLoggedAndStored`, a setup token in the URL is a 400 (`TestSetupRefusesATokenInTheURLAndBadInput`).
| G6 | Medium | **At-rest protection for M2 credentials.** The plan doesn't say the TOTP seed (which must be reversible) is encrypted. The data directory is created 0750 (`server.go:164`), existing directories aren't checked, and SQLite files get the umask default (usually 0644). | M2 | Store the TOTP seed in secretbox (AAD bound to the user row). Enforce `DATA` 0700, refusing a looser existing directory with a clear message as `socket.go` does. Set the process umask to 0077 at start. Test file modes after first run. **Built (M2 step 1):** `internal/datadir` (`TestDataDirCreated0700`, `TestLooserDataDirRefused`, `TestForeignOwnedDataDirRefused`), umask 0077 in `serverMain` (`TestUmaskKeepsNewFilesPrivate`, built binary), `internal/secretbox` with the master-key rules (`TestSecretboxAADBinding`, `TestSecretboxRefusesBadKeyFile`, `TestMissingKeyWithCiphertextsRefusesStart`, `TestSecretboxWrongKeyIDRefused`, `TestKeyIDRecordedAndChecked`: the key id is recorded in `meta.secretbox_key_id`, so a missing or swapped key file is refused at start; `TestSecondServerCannotStealALiveControlSocket`), the startup order with the lock before any write (`TestSecondServerFailsOnLockAndFirstStillAnswers`), `TestAuditVerifyAsRootOnServiceOwnedData`. **Still to do:** step 5 (the seed in secretbox, `TestFileModesAfterFirstRun`), step 10 (sentinel). |
| G7 | Low | **Session table growth.** Coolify #7394 grew a sessions table to 58 GB, and there is no scheduler until M3a. | M2 | Prune expired rows lazily on login and session insert (bounded batch). Cap active sessions per user (e.g. 20, revoking the oldest). Limit last-seen writes to once per minute, as for tokens. | **Built (M2 step 5, store level):** 12 h idle and 7 d absolute, lazy pruning of 100 on insert, a cap of 20 per user, activity written at most once a minute and best-effort (`TestSessionLifetimes`, `TestExpiredSessionsPrunedOnInsert`, `TestSessionCapTwenty`, `TestLastSeenWrittenOncePerMinute`, `TestLastSeenFailureIsBestEffort`, `TestExpiredSessionRefusedClearedAndDeleted`).
| G8 | Low | **Auth precedence, CSRF and `Host`.** The plan doesn't say what happens when a request has both a cookie and `Authorization`, and there is no `Host` allowlist (DNS rebinding hygiene). | M2 | Decide the auth method first. A request with `Authorization` is token-only and its cookie is ignored. Any cookie-authenticated request needs the CSRF header; never skip CSRF just because some header is present. Accept only the configured `Host` values (server IPs, panel domain), else 421. The WebSocket `Origin` allowlist comes from the same list. |
| G9 | Low | **Sentinel coverage for M2 secrets.** Only the setup token is named in the plan's tests. | M2 | Add sentinels for the password, TOTP seed, recovery codes, API token, session id, and the raw `Authorization` and `Cookie` headers. Search the DB bytes, server logs and Caddy logs for them. |
| G10 | Low | **Panel TLS key path** is not named, and not in the `doctor --bundle` exclusions or the state-backup contents. | M2 (ADR 0010), M7a, M7b | Put it under `DATA/pki/panel/` or `DATA/keys/` (0600). Add it to the M7a bundle exclusions and the M7b archive list. Decide whether a lost key is simply regenerated, which changes the fingerprint the console prints. **Built (M2 step 3):** `DATA/pki/panel.pem` (0600) and `panel.fingerprint` (also 0600: readable only by the owner; the value is public, but it lives under the secret `pki/` tree), under `pki/`, which is in `datadir.SecretPaths`; a lost or invalid leaf is regenerated with a WARN showing the new fingerprint (`TestPanelLeafPathAndModes`, `TestPanelLeafRegeneratedWithWarning`, `TestPanelLeafRenewedUnder60Days`, `TestPanelLeafRegeneratedWhenPanelHostsChange`, `TestPanelLeafNotFromHubCA`, `TestPanelFingerprintLoggedEveryStart`). SNI: `TestPanelSNIHandshakes` (no SNI, `localhost`, an unknown name). `:80` only redirects: `TestPanelServedOnHTTPSAndHTTPOnlyRedirects`, `TestPanelConfig`. |
| G11 | Low | **Agent token in the environment.** `SD_AGENT_TOKEN` in a systemd `Environment=` line is readable by any local user through `systemctl show`. The agent has no file option. | M7a | Add `--token-file` to `server` and `agent` (refuse files readable by group or others), and use `LoadCredential=` or a 0600 `EnvironmentFile=` in the unit. Never use an `Environment=` line. |
| G12 | Low (**built in M1**) | **The agent follows HTTP redirects** during the WebSocket handshake (default `CheckRedirect`, `client.go:200`). That can move the bearer token past `ValidateURL`. It is not exploitable today, because only a holder of a pinned-CA leaf can answer. | M1 (one line) or Phase 2 | **Built in M1 (`55b4807`):** the client refuses every redirect, with a test that the target is never contacted. |
| G13 | Low | **Log content is attacker-controlled.** Any container, or an agent-token holder, controls it. The debug route streams raw bytes to the operator's terminal; M8 renders logs in the browser; M9 sends them to MCP clients. | M8, M9 | Render logs as text only (no HTML injection). Neutralise control characters except a whitelisted SGR subset, or use xterm.js with OSC/DCS disabled. Cap line length. In MCP output, frame logs as untrusted data and run them through redaction. Optionally strip escape sequences from the debug route. |
| G14 | Low | **Supply-chain drift.** Rule 10 and the roadmap say cosign + SBOM + provenance + Dependabot. Plan M7a / ADR 0016 use a stdlib-ECDSA signed manifest and don't mention SBOM or provenance. Before M1 step 10 there was no `.github/dependabot.yml`, so pinned Action SHAs and Go modules got no update PRs. | Dependabot: **built in M1 step 10** (`.github/dependabot.yml`, weekly and grouped for `github-actions` and `gomod`). SBOM and provenance: M7a (ADR 0016). | Add a Dependabot config for `github-actions` and `gomod` (weekly, grouped). In ADR 0016, either add an SBOM (`go version -m`, or CycloneDX at build time) and GitHub artifact attestations, or record that they are dropped. Align the roadmap wording. |
| G15 | Low | **The IP panel stays open after M4.** Once a panel domain has a valid certificate, the self-signed IP panel still works, which teaches users to click through warnings. | M4 | Once the domain certificate is valid, redirect IP access to the domain by default (break-glass opt-in to keep the IP panel). Add HSTS on the domain. Re-check `Host` and `Origin` lists (G8). |

## Accepted risks (Phase 1)

- **Shared agent token (open item #3):** a token holder can replace another remote agent's session and read or fake its logs. Remote agents are off by default, experimental and logs-only. This is fixed in Phase 2.
- **Botnet on the agent listener:** many source IPs can hold the 128 connection slots with slow handshakes (open item #4). It doesn't reach the panel.
- **Login availability under a distributed attack:** the single argon2 slot can be kept busy. Existing sessions keep working, and the console can recover.
- **The control plane is root:** the server process is in the `docker` group, so any control-plane RCE means root on the host. systemd hardening (M7a) limits the filesystem but can't limit Docker. This is inherent to a Docker manager; we keep the attack surface small instead.
- **A compromised control plane compromises every agent.** The agent trusts the hub by design.
- **Data directory compromise** gives the secretbox key, the hub CA key and the DB together. The key is "outside the DB" only to protect DB-only copies and bundles.
- **First-run trust on a self-signed certificate** (after G5 is applied): the remaining risk is a user who skips the fingerprint check on a hostile network.
- **BuildKit DNS rebinding** between the URL pre-check and BuildKit's own fetch (M5, documented there).
- **`curl | sh` install**: the script is trusted through HTTPS; the binary it fetches is verified.

## Lessons from competitors (see [research](research/))

- **Coolify and Dokploy, 2026 CVEs:** command injection through backups, build params, Traefik config, logs/terminal WebSockets, registry credentials and file mounts; unauthenticated RCE from public-repo PRs; a cross-org authz bypass. Coolify #7394: a 58 GB sessions table (G7).
- **Portainer:** privileged plugin ops; file read through git symlinks; unauthenticated init through restore; first-admin creation by whoever reaches a fresh instance first (setup token, M2).
- **1Panel:** RCE through agent certificate verification (CVE-2025-54424; our pinned-CA check and role SAN); an X-Forwarded-For allow-list bypass (CVE-2025-66508; G2); a CSRF series; XSS (CVE-2026-23525).
- **CapRover:** a well-known default password (we have none; console setup token).

## Disclosure

`SECURITY.md` with GitHub private vulnerability reporting, from the first public commit. *(Built.)*

## Open items from the Phase 0 review

These were found in the systems/security review of plan 0001 (2026-11-20). The status is as of Phase 1 M1 ([plan 0002](plans/0002-phase-1-mvp-solo.md)). Rows 1–2 and 4–6 are closed in code, tests and the bench. Row 4 has a follow-up, **G1** (yamux ping flood), found in this review.

| # | Item | Phase | Status |
|---|---|---|---|
| 1 | The agent bearer token travelled over plain `ws://` on `:80`. | 1 | **Closed (M1 step 5).** Agents connect over `wss` (TLS 1.3) to the hub's own listener (`--agent-listen`, off by default), pinning the hub CA by fingerprint ([ADR 0009](decisions/0009-agent-channel-tls.md)). `ws://` is loopback-only. The token is 43+ characters or generated into a 0600 file. |
| 2 | Caddy reloads dropped every remote agent after `stream_close_delay`. | 1 | **Closed (M1 step 5).** `/agent/connect` is served by the hub's own listener. The Caddy route and `StreamCloseDelay` are gone, and `:80/agent/connect` answers 404. A test shows that a Caddy reload keeps the session. |
| 3 | The shared token gives no per-agent identity: any token holder can claim any non-reserved id and replace that agent. Enrollment + mTLS must bind the id to the credential (ADR 0003). The local agent's id is already reserved. | 2 | **Open. Phase 1 mitigation:** remote agents are experimental, off by default and read-only. Only `logs` is `RemoteOK`, an op missing from the table is refused, and this is enforced in the hub for every op. The token is at least 43 characters with at least 16 distinct ones (a generated token is 256-bit; a hand-made one is only screened). A failed-auth limiter bounds guessing. A token holder can still replace another agent's session and read logs. |
| 4 | No session cap, per-IP limit, per-agent stream cap or auth-failure rate limit. | 1 | **Closed (M1 step 6)**, except the yamux window bound below. The ping-flood follow-up (G1, High) is **built** too: `agent.GuardPings` ends a session past 4 pings/s. The limits in place: 16 sessions (`--agent-max-sessions`, 503 before the upgrade); 4 TCP connections per IP and 128 in total on the agent listener (closed in `Accept`, before any TLS work); 5 auth failures/min per IP with burst 10 (429, checked before the token compare); 8 hub-opened streams per remote session (32 for the local one); an agent-opened stream ends that agent's session (yamux would keep an accepted-then-closed stream buffered for up to its close timeout); 32 streams per agent, with one error frame when over; header 64 KiB, body 1 MiB, 10 s header timeout (5 s, no keep-alives, on the agent listener); Caddy public server header and idle limits. **Remaining risk:** an attacker with many source IPs (4 connections each, 128 in total) can hold the agent listener's slots with slow handshakes, each dropped after 5 s and replaced by a new one. This blocks legitimate agents while it lasts. It needs a botnet and doesn't touch the control plane or the panel (separate listeners). |
| 5 | Non-streaming Docker API calls (inspect) had no timeout. | 1 | **Closed (M1 step 4).** Per-call deadlines: 30 s, and `stop?t=N` gets N + 15 s. Log follow is bounded by its context only. API negotiation refuses Docker older than Engine 25 (API 1.44). |
| 6 | No goroutine-leak tests. | 1 | **Closed (M1 step 3).** goleak runs in the hub, agent, api and cmd/serverdock test mains (and dockerapi). |
| 7 | The bench mounts `docker.sock` into the agent container (root-equivalent); keep it confined to the bench. | ongoing | Open. |
| 8 | **yamux can't bound a stream's receive window below 256 KiB** (the library rejects any `MaxStreamWindowSize` under its initial window), so the plan's 64 KiB bound can't be expressed. | 1 | **Resolved (maintainer-delegated decision, 2026-11-20).** Keep the 256 KiB window (pinned by a test) and cap the hub at **8 streams per remote session** (the local in-process session keeps 32). Steady-state worst case for hub-opened streams: 16 sessions × 8 × 256 KiB = **32 MiB**, reached only if the hub's reader stalls. The bound holds because an agent-opened stream ends the session (agents never open streams). Closed hub streams can linger up to yamux's 10 s close timeout, bounded by how fast operators open them. A violating agent can briefly pin about 2 MiB per connection (8-stream backlog × 256 KiB) before its session is closed. Remote agents are logs-only in Phase 1, so 8 is enough. Control frames (pings) are not covered by this bound: see G1. |

## Unattended destructive actions

These are destructive actions that the `SystemInvoker` may run (`System != ""`). `TestEveryUnattendedDestructiveActionIsListedInTheThreatModel` (in `internal/actions`) fails when a destructive action with a `System` reason is not listed here by name, in backticks, with its reason, for `security` to review.

*None yet.*

## M1 close-out status

Recorded at the end of M1 (2026-11-20). Evidence is the tests named in [plan 0002](plans/0002-phase-1-mvp-solo.md) step by step and the commits in its M1 section.

**Built in M1** (each with tests, `go test -race ./...` green):
- CI: the fail-closed `bench-gate`, the docs-only reuse rule (pathspec allowlist), CODEOWNERS and Dependabot.
- goroutine-leak tests in every package that starts goroutines.
- Docker: per-call deadlines, API negotiation, Engine 25+ required.
- The agent channel (ADR 0009): its own TLS 1.3 listener, a pinned hub CA, a role marker, lazy renewal, token rules, read-only remote agents (`RemoteOK`), the per-session stream caps, the agent-opened-stream rule, and the yamux ping guard (G1).
- Limits: per-IP auth-failure limiter before the token compare, session cap, per-IP connection caps in `Accept`, header and body limits; limiters never evict a blocked key (G3, M1 part).
- The debug socket (0600 in a 0700 directory) replaces the TCP debug listener.
- Store: forward-only migrations with a pre-migrate backup, foreign-key-safe table rebuilds, the one-server lock (a 0600 file), and a read-only open that works next to a live server as another user.
- The audit log: a hash chain with boot ids, `audit verify`, redacted rows, rate-limited denied rows. `audit verify` runs as root on a service-owned data dir (production, `TestAuditVerifyAsRootOnServiceOwnedData`) and as a non-owner uid on a test-built permissive tree (`TestAuditVerifyAsAnotherUser`); it creates no files.
- The action registry: risk tiers, the per-actor policy table, the one-time `SystemInvoker`, and the three built-in read actions. No HTTP route reaches it yet.

**Still open, with an owner:**
- Open item #3 (no per-agent identity) and #7 (bench mounts `docker.sock`): Phase 2 and ongoing.
- G2, G4–G11, G13 and the rest of G3 (per-account TOTP back-off, a per-/48 bucket): M2 or later, as the gap table says.
- Not detectable by design: truncating the tail of the audit log, or someone who can write the file rebuilding the whole chain. Compare the head hash and row count (logged at start, returned by `system.info`) with an older copy; M7b stores them in state backups.
- In the audit chain, NULL and an empty string hash the same for `executor` and `approver`. `Append` never writes an empty string, so switching between them takes direct file access and changes no meaning.
- A denied-row window dropped while its table is full of in-progress windows loses its "suppressed" count. That needs thousands of distinct actor+action pairs denied inside one minute.
- The `update.json` skip of the pre-migrate backup is M7b, written together with the file.
- A local user who can already read the service user's files can do far more than anything here; the 0600 lock file and the 0700 debug directory only close the cheap local-denial cases.

## Debug access

`--debug-listen` and `--debug-allow-remote` are removed. `--debug-socket PATH` serves `/debug/*` (memory, agent list, log proxy) on a unix socket created 0600 inside a directory that must be 0700 and owned by the service user; the server refuses a looser directory. There is no TCP debug listener. The log proxy can stream any container's logs, local or remote, so the socket counts as service-user-level access. It joins the M2 route matrix.
