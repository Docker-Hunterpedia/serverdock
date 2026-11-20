# Plan 0003 — Phase 1 M2: auth, first run, SPA shell

**Status:** APPROVED (reviewer, security, systems, accessibility — round 2, 2026-11-20) · **Author:** planner (AI) · **Date:** 2026-11-20

**Roadmap:** Phase 1, milestone M2 of [plan 0002](0002-phase-1-mvp-solo.md). It serves these roadmap items: "setup token, admin + TOTP, first-run onboarding" (the M2 part of item 1), "token expiry" (item 18), "REST API (OpenAPI)" (item 21, the skeleton) and "accessible, phone-usable UI" (item 23, the baseline). This plan is AI-written and not yet confirmed by the maintainer.

**Binding inputs:**
- plan 0002: phase-wide rules 1–11, the M2 section, the footprint allocation, and the maintainer answers Q2 and Q8 (2026-11-20);
- [`docs/security.md`](../security.md) Draft 2: the "M2 surface" table and gaps **G2–G10**. `security` made covering each gap with a design item and a test the condition for starting M2. The [coverage table](#gap-coverage-g2g10) maps each gap to its design section, its tests and its step;
- ADR 0004 (embedded Caddy), ADR 0005 (SvelteKit SPA), ADR 0008 (footprint gates), ADR 0009 (agent channel, hub CA);
- the M1 code on `main`: `internal/actions`, `audit`, `redact`, `store`, `limits`, `pki`, `api`, `hub`, `cmd/serverdock`;
- the [maintainer answers](#maintainer-answers-2026-11-20) to QM2-1 to QM2-3.

**How to read this plan.** It has the same parts as plan 0002 Part B: goal, out of scope, design, footprint, security notes, steps, test plan and reviews. Then come the maintainer answers, the [review round 1 changes](#review-round-1-changes), and **Appendix A, ADR 0010**, ready for review. Step 1 copies Appendix A to `docs/decisions/0010-panel-tls-and-auth.md`.

## Goal

1. **The panel is reachable over HTTPS on the server IP** with a self-signed panel leaf of its own (Q2). It is never issued by the hub CA, and the console prints its fingerprint. `:80` only redirects.
2. **The control plane no longer listens on TCP.** Caddy reaches it over a unix socket in a 0700 directory, and only that socket's requests may carry a trusted `X-Forwarded-For` (G2).
3. **The first run is protected by the console setup token.** The admin is created with a password and a confirmed TOTP in one atomic step. The admin signs in with password + TOTP, and recovers from the console with `serverdock admin reset`.
4. **Sessions, API tokens and the authz layer are in place**, with one route table that every route must be declared in (the start of the route-matrix test), CSRF, credential resolution by route kind, and a Host allowlist.
5. **REST and OpenAPI 3.1 are generated from the action registry**, which needs the JSON Schema reflector deferred from M1.
6. **The real SvelteKit SPA ships embedded**: setup, sign-in, a shell with ServerDock's own RAM/CPU in the sidebar, an empty Overview, and Settings › Security. It stays within 250 KB of gzip JS and passes Playwright + axe on desktop, phone and WebKit.
7. **The footprint stays inside the allocation:** S2 ≤ 53 MB and S4 ≤ 45 MB, with no new timers.

## Out of scope

- **The panel domain, end to end** (M4): the setting, its validation, ACME, the IP → domain redirect, HSTS, the break-glass option (G15) and the Settings › General field. Q2 is still honoured: the domain is added after setup, once M4 lands. M2 keeps only `--panel-host` (extra names accepted by the Host allowlist).
- **Passkeys** (Phase 2), **multi-user and roles** (Phase 2). M2 has one admin, but the schema and API carry a username so that Phase 2 doesn't break them.
- **Email or other notifications** of auth events (M8). M2 shows them in the panel only.
- **WebSocket routes.** M2 ships none. The route table's WebSocket rule (session plus exact `Origin`) is built and tested with a test-only route; the first real one comes in M3a or M8.
- **Onboarding steps 3–5** (server checks: M7a; connect GitHub: M5; first deploy: M3a).
- **Settings sections other than Security** (QM2-3).
- **A general CLI.** The only new subcommand is `admin reset` (plan 0002 exception).
- **Secret-store values in redaction** (M3b), `doctor --bundle` itself (M7a), state backups (M7b). M2 provides the list of secret paths those later steps must use.
- **Key rotation** for the secretbox master key.

## Design

### Overview of the request path

```
browser ──HTTPS :443──▶ embedded Caddy (panel leaf, self-signed; default/fallback SNI = localhost)
                         │  replaces client X-Forwarded-For with the real peer
                         ▼
              unix socket DATA/run/cp.sock (dir 0700, socket 0600, 256-connection cap)
                         ▼
   control plane: Host allowlist → route table → credential resolution by the route's AuthKind
                  → Origin + content type (unsafe methods) → CSRF (session, unsafe methods)
                  → handler (auth endpoints | REST action handler | SPA files)
                         ▼
                 action registry (policy, audit) → store

browser ──HTTP :80──▶ Caddy: 308 to https://{host}[:port of --https-listen]{uri}
```

The remote agent's own Caddy (bench S4) is untouched: `proxy.Config` gets an optional panel part that only `server` mode sets.

### Startup order (one rule for `server` and `admin reset`)

1. umask 0077;
2. `datadir.Prepare(DATA)`;
3. `store.Open` (takes the data-directory flock, runs pending migrations);
4. the secretbox master key;
5. the panel leaf (server with proxy only), the hub token and hub pki (moved here from before the store, where M1 calls them today);
6. the setup token (server, no admin only);
7. every listener and socket.

Nothing that writes under `DATA` happens before the lock. A second server started on the same `DATA` fails at step 3, before it can replace the first server's socket, token file or keys. Test: `TestSecondServerFailsOnLockAndFirstStillAnswers` (the first server's control socket still answers `/healthz`, its setup token file is unchanged).

### Data directory and file modes (G6)

New small package **`internal/datadir`**, used by `server`, `admin reset` and the tests:

- **`Prepare(dir)`** creates `DATA` with mode 0700. If it already exists, it must be a directory owned by the running uid with no group or other bits (`mode & 0o077 == 0`). Otherwise the server **refuses to start** with a message like `debug.go`'s ("chmod 700 /var/lib/serverdock"). It never loosens or silently tightens a directory (G6).
- **The process umask is set to 0077** at the top of `serverMain` and `adminResetMain` (`syscall.Umask`), before any file is created. SQLite's `-wal` and `-shm` files and Caddy's storage are then 0600/0700 too. Because the umask is process-wide and set in `serverMain`, file-mode tests run the **built binary** through the subprocess harness added in step 1 (`internal/testbin`: builds `serverdock` once per test package in `TestMain`, runs it with a temp `DATA` and short socket paths), not `runServer` in-process.
- **`SecretPaths`** is one exported list of paths, relative to `DATA`, that hold secrets: `keys/`, `pki/` (hub CA, hub leaf, panel leaf), `setup-token`, `agent.token`, `backups/`. M7a's `doctor --bundle` must exclude exactly this list, and M7b's archive must include `keys/` and `pki/`. A test fails when a file the M2 code creates under `DATA` holds a secret and is not under one of these paths.
- **`audit verify` tests.**
  - `TestAuditVerifyAsAnotherUser` (M1) **stays**. It proves the property M1 needs: a reader that does not own the files creates nothing in `DATA`. It builds its own 0755 tree through `store.Open` and never calls `datadir.Prepare`, so it runs unchanged; no test-only switch that skips the mode check is added.
  - **New** `TestAuditVerifyAsRootOnServiceOwnedData`: the server runs as uid 65534 on a real 0700 `DATA`, and root runs `audit verify`. It checks that verify succeeds and leaves no root-owned file (no `-shm`). This is the production case (an operator running verify with `sudo`).
  - `docs/security.md` wording changes from "audit verify as a different uid" to "as root on a service-owned data dir (production), and as a non-owner uid on a test-built permissive tree (creates no files)".
- **Bench and container images:** `bench/Dockerfile` creates `/out/data` with `--chmod=0700`. M7a's unit uses `StateDirectoryMode=0700` (noted for plan 0009).

### Secretbox (`internal/secretbox`, G6)

- **Master key:** `DATA/keys/master.key`, 32 bytes from `crypto/rand`, file 0600 in a 0700 directory, written atomically (temp file, fsync, rename, directory fsync).
- **When it is created, and when the server refuses:**
  - the file is missing **and no secretbox ciphertext exists in the DB** (M2 checks `users.totp_seed`, `users.totp_pending_seed` and `setup_state.pending_seed`; later milestones add their columns to the same check) → create it;
  - the file is missing **but a key id is recorded in `meta.secretbox_key_id`, even when no ciphertexts exist** → refuse to start with the same kind of message (the id is written in the same startup step that creates the key; a key that is present with a different id is refused too; a key present with no recorded id, as step 1 left it, is adopted);
  - the file is missing **but ciphertexts exist** → refuse to start: "DATA/keys/master.key is missing but encrypted data exists. Restore the key from a backup, or run `serverdock admin reset` to clear the encrypted admin data";
  - the file has the wrong size or can't be read → refuse to start with the same kind of message. It is never replaced, because replacing it would orphan every ciphertext;
  - a ciphertext's key id doesn't match the key → `Open` returns `ErrWrongKey`, and login answers "the server's encryption key doesn't match its data; see the docs (admin reset)" with a log line naming the table and column, never the bytes.
- **`admin reset` creates the key if it is absent**, after clearing the M2 ciphertexts **and `meta.secretbox_key_id` in the same transaction** (it clears exactly the columns above, so the check passes afterwards, and a server started with the key file removed after a reset gets a new key and a new id).
- **Corrupt key, manual steps** (documented on the Security page's docs link and in ADR 0010): stop the server; move `DATA/keys/master.key` aside; run `serverdock admin reset` (it clears the TOTP data and writes a new key); start the server and complete setup with the new token. From M3b on, app secrets encrypted with the old key are lost unless the old key is restored from a state backup (M7b).
- **Format:** `version (1 byte, 0x01) ‖ key id (4 bytes, first 4 bytes of SHA-256(key)) ‖ nonce (12 bytes) ‖ AES-256-GCM ciphertext and tag`.
- **AAD** is `"<table>/<row id>/<column>"`, for example `users/1/totp_seed`. A ciphertext copied to another row or column fails to open.
- **API:** `Seal(aad string, plaintext []byte) ([]byte, error)`, `Open(aad string, ct []byte) ([]byte, error)`. Errors never include plaintext or key bytes.

### Control plane on a unix socket (G2)

- **`--internal` is removed** and replaced by **`--control-socket PATH`**, default `DATA/run/cp.sock`. The directory is created 0700 and must be owned by the service user; the socket is 0600. `listenDebugSocket` becomes a shared `listenPrivateSocket(path)` used by both sockets (absolute path, 0700 directory owned by the running uid, stale socket replaced, anything else refused). The control socket and the debug socket must be different paths.
- **The control socket keeps M1's 256-connection cap** (`limits.NewListener(…, 256, 0, …)`). There is no per-IP cap: every connection comes from our Caddy.
- **Path length:** unix socket paths are limited (108 bytes on Linux, 104 on macOS). `listenPrivateSocket` refuses a longer path with a clear message. Tests create socket directories with `os.MkdirTemp("", "sd")`, not `t.TempDir()`.
- **There is no TCP listener for the control plane.** `--proxy=false` (dev, bench scenario 1) serves the control plane on the socket only.
- **Caddy dials it** with `"dial": "unix//<path>"`.
- **Trusted forwarding header only from the control socket.**
  - `api.Serve` gets an option that marks each connection from a listener as "from our proxy" through `http.Server.ConnContext`. Only the control-socket listener sets it.
  - `limits.ClientIP(r, trustLoopbackProxy bool)` is replaced by **`limits.ClientIP(r)`**, which reads that mark. With the mark it requires **exactly one** `X-Forwarded-For` value holding exactly one IP. Zero values, several values, a list or an unparsable value → 400, logged once per minute (our proxy is misconfigured). Without the mark, `X-Forwarded-For` is ignored and the key is `RemoteAddr`.
  - Loopback is no longer trusted for anything. The hub's agent listener keeps using `RemoteAddr` only.
- **Caddy must replace, never append to, a client's `X-Forwarded-For`.** Caddy keeps incoming `X-Forwarded-*` values only from configured `trusted_proxies`, and we configure none. A test pins this, so a Caddy upgrade that changes it fails CI.
- **Debug socket:** unchanged (0600, 0700 dir, off by default). Its routes are in the route table with the `DebugSocket` auth kind and are mounted only on that listener.

### Panel TLS (Q2, ADR 0010, G10)

- **Leaf.** `pki.EnsurePanelLeaf(dir)` (its own code path, no access to `ca.key`) creates a **self-signed ECDSA P-256 leaf**. It is **never issued by the hub CA**. Properties:
  - validity **825 days** (the longest Apple platforms accept for a TLS server certificate). At every start, if fewer than **60 days** remain, the leaf is regenerated and a WARN line shows the new fingerprint. There is no running renewal (no timer); Settings › Security shows the expiry date and, under 60 days, "restart ServerDock to renew the panel certificate" (a server that runs over 60 days without a restart would otherwise serve an expired self-signed leaf);
  - SANs: DNS `localhost`, IPs `127.0.0.1` and `::1`, the host's global-unicast interface addresses found at creation, and every `--panel-host` name. A changed `--panel-host` set regenerates the leaf at start (new fingerprint, WARN);
  - EKU `serverAuth` only; `BasicConstraints` CA false; no role URI (it is not the hub).
- **Certificate selection (SNI).** certmagic picks the certificate by SNI. The panel's TLS connection policy sets **`default_sni: "localhost"`** (used when the client sends no SNI, which is what browsers do for IP literals) and **`fallback_sni: "localhost"`** (used when the SNI matches no loaded certificate). The one panel leaf, which has `localhost` in its SANs, is therefore served for every handshake on `:443`.
- **Path (G10):** `DATA/pki/panel.pem` (key and certificate in one file, 0600, replaced atomically) and `DATA/pki/panel.fingerprint` (public). `DATA/pki/` is created 0700 whether or not `--agent-listen` is set. Both are under `pki/`, which is in `datadir.SecretPaths`.
- **A lost or unreadable leaf is regenerated** (WARN with the new fingerprint). A half-written file (bad PEM, key and certificate don't match) is treated as missing, because nothing else depends on this key.
- **Fingerprint.** SHA-256 over the leaf's DER, colon-separated hex (the form browsers display). It is logged at **every start** and shown on Settings › Security after sign-in. **It is never shown on an unauthenticated page**: an on-path attacker could show any value there. Onboarding tells the user to compare the browser's certificate details with the **server log**.
- **Caddy config** (`proxy.Config.Panel`, set only in `server` mode):
  - server `panel` on `--https-listen` (default `:443`): TLS 1.2+, certificate from `tls.certificates.load_files` (`panel.pem` as both certificate and key), the SNI defaults above, `automatic_https.disable: true`, OCSP stapling off, every route reverse-proxied to the control socket, and the same limits as `:80` (`read_header_timeout` 10 s, `idle_timeout` 2 m, `max_header_bytes` 64 KiB);
  - server `main` on `--listen` (default `:80`): one `static_response` route, status 308, `Location: https://{http.request.host}{port}{http.request.uri}`, where `{port}` is empty when `--https-listen`'s port is 443 and `:<port>` otherwise. **The port comes from `--https-listen` as seen inside the process.** Behind Docker port mapping (for example `-p 8443:443`) the redirect points at 443, which is wrong for that client; the `--help` text and the docs say so. Nothing on `:80` reaches the control plane.
  - `logging`: level WARN. The sentinel test raises it to DEBUG through the environment variable `SERVERDOCK_TEST_PROXY_LOG_LEVEL`, which only that test sets (no flag, not in `--help`).
- **Background work.** certmagic's cache maintenance goroutine already runs in M1 in **both** `server` and `agent` mode, because `caddyhttp` provisions the `tls` app. Step 3 confirms it with a goroutine dump, adds it to the Background work table as "running since M1 (S2 and S4)", and moves plan 0002's rule-3 row from M4 to that status.

### Host allowlist, Origin and security headers (G8)

Built in step 6a, with the route table.

- **Host allowlist** (first middleware; mismatches get **421 Misdirected Request**). The `Host` header, port stripped, must be:
  - an IP literal (IPv4, or IPv6 in brackets). DNS rebinding needs an attacker-controlled name, so IP literals are safe to accept;
  - `localhost` exactly (the SSH-tunnel path);
  - a name given with the repeatable flag `--panel-host NAME`.
- **Origin.** On every non-GET/HEAD request — pre-session ones (setup, login, 2FA) included — an `Origin` header, **when present**, must equal `https://` + the request's `Host` (with port), or the request gets 403 `origin`. A cookie-authenticated unsafe request **must** carry `Origin` (browsers always send it on POST). A WebSocket upgrade must carry a matching `Origin` and be session-authenticated.
- **Content type.** Every `POST` under `/api/` must have `Content-Type: application/json`. A cross-origin page can't send that without a CORS preflight, and we never answer preflights with CORS headers.
- **Headers** on every response: `nosniff`, `X-Frame-Options: DENY`, `Referrer-Policy: same-origin`, `Cross-Origin-Opener-Policy: same-origin`, `Cross-Origin-Resource-Policy: same-origin`. `/api/*` gets `Cache-Control: no-store`. HTML gets the CSP below. HSTS is M4.
- **CSP** for the SPA (hash mode): `default-src 'none'; script-src 'self' 'sha256-…'; style-src 'self'; img-src 'self' data:; font-src 'self'; connect-src 'self'; base-uri 'none'; form-action 'self'; frame-ancestors 'none'`. The hashes of the inline `<script>` blocks in the embedded `index.html` are computed once on first use (`sync.OnceValue`), so they always match the embedded build.

### Credential resolution (G8)

Credentials are resolved **according to the matched route's `AuthKind`**, the same way for every route:

| Route kind | Credentials read | Notes |
|---|---|---|
| `Public`, `SetupOpen` | **none.** A session or pending cookie is not used to authenticate. | The login handler and setup completion **revoke an old session** whose cookie is still valid before minting anything. |
| `Pending2FA` (`auth/2fa`) | **only** `__Host-sd_pending` | A valid session cookie on the same request is ignored for authentication and revoked when the new session is minted. |
| `Session` | only `__Host-sd_session` | an `Authorization` header on a `Session` route → 400 |
| `SessionOrToken` | `Authorization` if present (cookies then **ignored entirely**), otherwise `__Host-sd_session` | |
| `DebugSocket` | none (filesystem permissions) | |

- **A session cookie that is unknown, expired or revoked counts as no credential**, and the response clears it (`Set-Cookie: __Host-sd_session=; Max-Age=0; …`). On a `Session` or `SessionOrToken` route that gives 401 with the clearing header.
- **The session cookie is cleared** by: login (when it revokes an old session), password change, TOTP re-enrolment, logout, revoking "this device", and any 401 for an invalid session.
- **Token requests:** `Authorization` must be `Bearer sd_…`. A bad, expired or revoked token → 401 (after the per-IP bad-token limiter). No CSRF check, because no ambient credential is used.
- **Session requests:** every non-GET/HEAD request needs a valid `X-SD-CSRF` header **and** a matching `Origin`. CSRF is never skipped because some other header is present.
- **Never accepted:** a token or session id in a URL query or form field (`?token=`, `?access_token=`, `?session=` with valid values → 401 / ignored), a session id chosen by the client, a token in a cookie.
- **CSRF token:** `base64url(HMAC-SHA256(key = the raw session secret, msg = "sd-csrf-v1"))`, recomputed from the cookie and compared in constant time. The server stores only SHA-256 of the session secret, so a stolen DB doesn't yield CSRF tokens. The SPA gets it from the login response and `GET /api/v1/auth/session`.

### Auth state machine (G4)

**"An admin exists"** means `EXISTS (SELECT 1 FROM users WHERE password_hash IS NOT NULL AND totp_confirmed_at IS NOT NULL)`. Setup endpoints answer **404** when an admin exists.

```
                  ┌────────────── server start, no admin ──────────────┐
                  ▼                                                    │
   [SETUP_OPEN] new setup token (hash in DB, plaintext in              │
                DATA/setup-token 0600), pending enrolment cleared      │
        │ POST setup/enroll {token} → token checked, pending seed      │
        ▼                                                              │
   [ENROLLING] ── POST setup/complete {token, enrollment_id, username, │
                  password, code}: read-only checks → argon2 → one tx: │
                  compare-and-delete token row, write user, recovery   │
                  codes, first session ──▶ [ADMIN_EXISTS]              │
                                                    │                  │
   [ADMIN_EXISTS] setup endpoints 404               │                  │
        │ POST auth/login {username, password} ok   │                  │
        ▼                                           │                  │
   [PENDING_2FA] pending token: hashed, single-use, 5 min, 5 tries,    │
        │        at most 5 per user                                    │
        │ POST auth/2fa {code | recovery_code} ok → pending deleted,   │
        │ NEW session id minted, any old session revoked               │
        │ 5 failures → pending deleted, password again                 │
        ▼                                                              │
   [SESSION] idle 12 h / absolute 7 d; fresh 2FA for 5 min after login │
             or auth/reauth; 5 failed reauth codes → session revoked   │
                                                                       │
   `serverdock admin reset` (console, owner uid, server stopped) ──────┘
```

**(a) Between password and TOTP (G4a).**
- A correct password returns a **pending-2FA token**, not a session: 32 random bytes in `__Host-sd_pending` (`HttpOnly; Secure; SameSite=Strict; Path=/; Max-Age=300`). Only its SHA-256 is stored, in `auth_pending` (`user_id`, `expires_at` 5 min, `tries_left` 5, IP key).
- **Pruning and cap (SYS3):** every pending insert first deletes up to 100 expired pending rows, then, after inserting, keeps at most **5 pending rows per user** (oldest deleted).
- `POST /api/v1/auth/2fa` decrements `tries_left` atomically (`UPDATE … SET tries_left = tries_left - 1 WHERE id_hash = ? AND tries_left > 0 AND expires_at > ?`, one row affected) **before** checking the code. At 0 the row is deleted and the password is needed again (G3).
- On success the pending row is deleted in the transaction that creates the session (single use).

**(b) Setup atomicity (G4b).**
- `POST /api/v1/setup/enroll {token}` checks the token, creates a pending TOTP seed (20 random bytes, sealed with AAD `setup_state/1/pending_seed`) and an `enrollment_id` (16 random bytes, stored hashed), and returns the `otpauth://` URI and the base32 secret. A second enrol replaces the first; the first client's completion then fails with "this enrolment was replaced; scan the new code". There is no separate verify endpoint: the SPA's step 1 calls `enroll`.
- `POST /api/v1/setup/complete`, in this order (SYS9):
  1. **read-only checks, before any hashing:** read the `setup_state` row; compare the token hash (constant time), the expiry and the enrollment hash; open the pending seed and check the TOTP code (±1 step), noting its counter; check the password rules. Any failure → the error, with nothing hashed;
  2. argon2 hash of the password (outside any transaction);
  3. **one `BEGIN IMMEDIATE` transaction:** `DELETE FROM setup_state WHERE id = 1 AND token_hash = ? AND enrollment_hash = ? AND expires_at > ?`, which must affect exactly one row (**read first, then compare-and-delete by rows affected**; no `RETURNING`). Then insert the user (or update the existing row after `admin reset`, keeping its id), the TOTP counter from step 1, 10 recovery-code hashes and the first session. Commit.
- If anything fails, nothing is written. After commit, `DATA/setup-token` is removed (a failed removal is logged at WARN and harmless).
- **An admin can never exist without a confirmed TOTP.**
- Twenty concurrent completions with the same token produce exactly one admin and one success; the others get 404 (admin exists) or 409 (token consumed).

**(c) "Admin exists" after `admin reset` (G4c).** `admin reset` nulls `password_hash`, `totp_seed`, `totp_confirmed_at`, `totp_pending_seed`, sets `totp_last_counter` to 0, and keeps the row. No admin exists, so the next start writes a new setup token and setup reopens **only with that token**. A login for the reset user runs the dummy hash and fails like a wrong password.

**(d) Session id rotation (G4d).** A session id is created only by the server, at setup completion and at `auth/2fa` success. An old session presented at login, 2FA or setup is revoked. Password change and TOTP re-enrolment revoke **every** session, including the current one; the SPA then shows the Signed-out screen with the reason. Step-up (`auth/reauth`) changes only `mfa_fresh_until`.

### Setup token

- Format `sd_setup_` + 43 base64url characters (256 bits). Stored as SHA-256 in `setup_state.token_hash`, compared in constant time. Expiry 1 hour.
- **Written at every start while no admin exists**: generate, commit the hash and expiry, write `DATA/setup-token` atomically (0600). A failed file write fails startup. When an admin exists at start, the row and any stale file are deleted.
- **Console and journal print only the path, the expiry time and the panel fingerprint**, plus the SSH-tunnel hint (G5):
  `INFO no admin yet: setup token written to /var/lib/serverdock/setup-token (expires 14:05 UTC); panel certificate SHA-256 AB:CD:…; safest first run: ssh -L 8443:127.0.0.1:443 you@this-server, then open https://localhost:8443/setup`
- **Sent only in POST bodies.** A token in the URL → 400.
- **After expiry**, setup endpoints answer `403 {"code":"setup_token_expired"}`; the page says "Restart ServerDock to get a new token". No on-demand regeneration.
- **`setup/status`** returns `{setup_required, token_expires_at, token_file, server_time}` (UTC). The SPA shows the expiry time and, when a code is rejected, the server's time ("Server time is 14:02:31 UTC; check your device's clock") (SYS16).
- **Deviation from plan 0002:** `admin reset` doesn't write a token itself; the next server start does. Step 7 updates the rows that say otherwise (listed there).

### Passwords, the hash semaphore and timing

- **argon2id** (`golang.org/x/crypto/argon2`): m = 19 MiB, t = 2, p = 1, 16-byte salt, 32-byte key, stored as a PHC string, so parameters can be raised later.
- **Rules:** at least 12 characters and at most 256 bytes of UTF-8, compared as bytes with no normalisation. No composition rules. Must differ from the username.
- **Semaphore (`auth.Hasher`):** one hash at a time, at most **4 waiters**. A 5th caller gets **503 at once** (`Retry-After: 2`, code `busy`); a waiter that doesn't get the slot within **2 s** gets the same. Back-off checks run **before** the semaphore.
- **Unknown or reset user:** the hash runs against a **constant dummy PHC string compiled into the binary**, so timing and response match a wrong password.
- **Memory after a hash (SYS18).** When the hasher releases the slot and no waiter is queued, the goroutine that releases it starts `debug.FreeOSMemory()` in a new goroutine, so it never delays the response. Step 4 measures its cost and effect.

### TOTP and recovery codes

- **TOTP** (RFC 6238) in-repo, HMAC-SHA-1, 6 digits, 30 s, 20-byte seed. Accepts **±1 step**.
- **Never reuse a code:** accepted only if its counter is greater than `users.totp_last_counter`, updated with `UPDATE … WHERE id = ? AND totp_last_counter < ?` (one row affected). A reused code gets the distinct message "That code was already used. Wait for the next one."
- **Seed at rest** in secretbox, AAD `users/<id>/totp_seed`. The URI and secret appear only in the enrolment response.
- **QR code:** rendered in the browser by `uqr` (lazy chunk, enrolment screens only), drawn as SVG `<rect>` elements from its boolean matrix, never `{@html}` (accessibility rules in [A3](#accessibility-wcag-22-aa-plan-0002-rule-6)).
- **Recovery codes:** 10 × 16 base32 characters (80 bits), shown as `xxxx-xxxx-xxxx-xxxx`, stored as SHA-256, single use, accepted at `auth/2fa`, never for step-up, counted against the second-factor back-off. Regenerating replaces all ten.
- **`users.totp_enrolled_host`** (`ip` | `localhost` | `name`) lets M4 recommend re-enrolment once the panel domain has a valid certificate (G5).

### Login protection (G3)

New lazy type **`limits.Backoff`**: per key, a failure count, the last failure time and the next allowed time. No goroutine, no timer. `Check(key) (wait)`, `Fail(key)`, `Reset(key)`.
- At most 4096 keys. **Making room:** unblocked keys are evicted oldest-first (by last failure); entries with no failure for 24 h go first. **A blocked key is never evicted.** When the map is full of blocked keys, a new key is treated as blocked (fail closed, rule 13) — **except** that the caller can pass `bypassFull` for a known IP (below).
- Blocked → `429` with `Retry-After` and code `rate_limited`; the body is the same whether or not the username exists.

**Four separate instances (SEC3), each with its own cap:**

| Instance | Key | Applies to | Free failures | Then | Cap | Reset |
|---|---|---|---|---|---|---|
| `ip64` | IPv4 address, or IPv6 /64 | setup token, password, 2FA code, recovery code | 5 | 1 s, doubling | 15 min | success from that key |
| `ip48` | IPv6 /48 (checked alongside `ip64`) | same | 20 | 1 s, doubling | 15 min | success from that key |
| `accountPassword` | normalised username for existing users; **one shared key `<unknown>` for every name that doesn't exist** | login | 10 | 1 s, doubling | 15 min | successful login |
| `account2FA` | user id | `auth/2fa`, `auth/reauth`, re-enrolment confirm | 5 | 2 s, doubling | **1 h** | successful login |

Plus: the pending token (5 tries, then password again); reauth (5 failures revoke the session, SEC4); the bad-bearer-token bucket (5/min per IP key, burst 10, like the hub).

- **Known IPs.** IP keys that completed a full login in the last 30 days (at most 10 per user, `auth_known_ips`) **skip `accountPassword`**, and **bypass the "map full" refusal on `ip64` and `ip48`** (their own entries still count). An attacker who floods the maps or the admin's account can't keep the admin out from their usual network. Test: `TestKnownIPReachesHashWhenMapsFullOfBlocked`.
- **Unknown names share one bucket**, so spraying random usernames can't fill `accountPassword`, and the shared bucket is the same for every non-existent name (**identical bodies, statuses and timing for a single client, but not undetectable to a multi-IP attacker**: after saturating `<unknown>` from many IPs, a probe for a name that does not exist is refused 429 while a probe for an existing name is answered 401, because the existing name has its own bucket. **Accepted risk**, recorded in ADR 0010 decision 8 and `docs/security.md`: the user name is not a secret, the password and the TOTP still guard the account, and per-name buckets would undo the map-filling protection of SEC3. `TestUnknownBucketRevealsExistenceToAMultiIPAttacker` pins the behaviour so a change is deliberate).
- **Order of checks** on login: Host → content type → Origin → `ip64` and `ip48` → `accountPassword` (unless known IP) → hash semaphore → argon2. On `auth/2fa`: pending decrement → `ip64`/`ip48` → `account2FA` → code.
- **Strength.** With the password known, `account2FA` allows about 40 guesses a day; every 5 need the password again. At 3 valid codes in 10⁶ that is about 1.2 × 10⁻⁴ a day; the admin is told at each sign-in.
- **Admin notice.** Each "password correct, second factor wrong" — and each failed reauth code (SEC4) — increments `users.totp_fail_since_login` and records time and IP key. On the next successful login the count moves into the session's notice and the counter resets. The SPA shows a banner and Settings › Security shows it.
- **Remaining risk (ADR 0010):** back-off state is in memory and resets on restart (needs host access); a distributed attacker can keep the hash slot busy; a password holder can delay the admin's second factor by up to 1 h; an attacker on a new network can be blocked by a flood that a known IP bypasses.

### Sessions (G7)

- **Cookie:** `__Host-sd_session`, 32 random bytes base64url, `HttpOnly; Secure; SameSite=Strict; Path=/`, `Max-Age` = absolute expiry. Only SHA-256 stored.
- **Table `sessions`:** `id_hash`, `public_id` (8 random bytes hex, used in the sessions list), `user_id`, `created_at`, `last_seen_at`, `idle_expires_at`, `abs_expires_at`, `mfa_fresh_until`, `reauth_failures`, `ip_key`, `user_agent` (≤ 200 bytes), `notice_json`.
- **Expiry (QM2-2):** idle 12 h, absolute 7 days. Expired sessions are refused (and the cookie cleared) and deleted on lookup. The 401 carries a reason (`idle_expired`, `absolute_expired`, `revoked`) so the Signed-out screen can say why.
- **`last_seen_at` / idle expiry** are written at most once a minute, **best-effort**: a failed write is logged (rate-limited) and the request continues (SYS17).
- **Lazy pruning:** every session insert first deletes up to 100 expired sessions and up to 100 expired pending rows.
- **Cap:** at most **20 sessions per user**; the oldest `last_seen_at` beyond 20 are deleted after an insert.
- **Fresh 2FA (QM2-2):** `mfa_fresh_until` = login + 5 min, extended to now + 5 min by `POST /api/v1/auth/reauth {code}` (TOTP only). **5 failed reauth codes on one session revoke it** (`reauth_failures`) and clear the cookie (SEC4). The session becomes `actions.UserActor(id, ScopeAdmin, mfa_fresh_until)`.
- **Accepted risk (SEC6):** cookies are scoped to the host, not the port. On IP access and over the SSH tunnel (`localhost`), any other HTTPS service on the same host name and a different port could receive the panel cookie if it were reachable by the browser. `SameSite=Strict`, `HttpOnly` and fresh 2FA for sensitive and destructive actions limit what such a service could do. **Plan 0004 (M3a) must** warn when a service publishes a host port on the panel's IP. M4's panel domain removes the overlap.

### Audit of auth events

The pre-session endpoints (setup, login, 2FA, reauth, logout) are not registry actions; they write rows directly through `audit.Appender`, intent first:
- `auth.setup`, `auth.login`, `auth.reauth`, `auth.logout`: intent then outcome, actor `user:<id>`, args `{username, ip_key, method}`;
- **if the intent row can't be written, the endpoint fails closed with `503 audit_unavailable` and mints nothing** (consistent with the registry's "intent write fails → not run"; the intent write waits at most about 2 s, and the 503 says "the server is busy or the audit log is not writable; nothing was done" with `Retry-After`) (SYS17). **Exception: actions that only remove access (logout, revoke session, revoke token) always run**: if their intent cannot be written they still run, and the response carries `audit: "unrecorded"` with an ERROR in the log, because refusing to revoke access is worse than a gap in the log; An outcome-write failure after success is logged and the response carries `audit: "unrecorded"`, as for actions;
- failures go through `audit.Denied` (rate-limited per actor + action) with **actor kind `anonymous` and actor id `anonymous:*`**, one id per action, so the limiter bounds them per action rather than per IP (SYS6). The IP key is in `args_json`. **The username is recorded only if that user exists; otherwise `<unknown>`** (SEC5), so a password typed into the username field is never stored.

### API tokens (SEC10 of plan 0002, Q8)

- **Format:** `sd_` + 43 base64url (256 bits); the first 8 characters after `sd_` are the displayable `prefix`. SHA-256 stored; lookup by hash.
- **Table `api_tokens`:** `id`, `user_id`, `name`, `prefix`, `hash` (unique), `scope` (`read` | `write` | `admin`), `created_at`, `expires_at` (**required**), `last_used_at`, `revoked_at`.
- **Expiry:** default 90 days, maximum 365. Expired → 401 `token_expired`. `last_used_at` written at most once a minute.
- **Accepted only from `Authorization: Bearer`** on `SessionOrToken` routes.
- **What a token can do (Q8):** the registry denies tokens every `Sensitive`, `Destructive`, `SessionOnly` and `ConsoleOnly` action. Token, auth and user management are `SessionOnly`, so **no token can create, list or revoke tokens, change auth settings or manage users**. The route matrix checks this for every action.
- **Creation** (`tokens.create`) is `SessionOnly` and `Sensitive` (fresh 2FA, audited intent-first). The plaintext token is in the response once; the audit row records name, scope and expiry only. Rotation = create new + revoke old.
- **Revocation:** `tokens.revoke`. Password change and TOTP re-enrolment revoke **all** tokens unless `keep_tokens: true`. `admin reset` revokes all.

### Actions added to the registry

Registry changes:
- **`ConsoleOnly bool`** in `Def`: every actor except the `SystemInvoker` is denied. `Register` panics if `ConsoleOnly` is set without a `System` reason. REST answers 404 and OpenAPI omits them.
- **Execution errors are redacted before `Invoke` returns them.**
- **`Info` gains `OutputType`** for the schema reflector.

| Action | Risk | Scope | Flags | Notes |
|---|---|---|---|---|
| `system.info`, `system.docker`, `audit.verify` | read | read, read, admin | — | existing (M1) |
| `system.panel_cert` | read | read | — | fingerprint, not-after, created (public data) |
| `auth.session_info` | read | read | SessionOnly | user, fresh-until, notices, recovery codes left |
| `auth.password_change` | mutate | admin | SessionOnly, Sensitive | `{current_password, new_password}` both `sd:"secret"`, `keep_tokens`; revokes all sessions |
| `auth.totp_reenroll_begin` | mutate | admin | SessionOnly, Sensitive | pending seed (secretbox, 10 min) |
| `auth.totp_reenroll_confirm` | mutate | admin | SessionOnly, Sensitive | `{enrollment_id, code, keep_tokens}`; 5 tries per enrolment; revokes all sessions |
| `auth.recovery_regenerate` | mutate | admin | SessionOnly, Sensitive | returns 10 new codes once |
| `auth.sessions_list` | read | admin | SessionOnly | |
| `auth.sessions_revoke` | mutate | write | SessionOnly | `{public_id}` or `{all_others: true}` |
| `tokens.create` | mutate | admin | SessionOnly, Sensitive | `{name, scope, expires_in_days}` |
| `tokens.list` | read | admin | SessionOnly | never returns hashes |
| `tokens.revoke` | mutate | admin | SessionOnly | `{id}` |

**`auth.admin_reset`** (destructive, admin, **ConsoleOnly**, System = "console recovery: the operator ran `serverdock admin reset` as the data owner with the server stopped") is **registered only in the `admin reset` process**, never in the server's registry. `TestServerRegistryHasNoAdminReset` asserts that, and M1's unattended-destructive check (`TestEveryUnattendedDestructiveActionIsListedInTheThreatModel`) is extended to walk the `admin reset` registry too, so the action must be listed in `docs/security.md` "Unattended destructive actions".

### REST and OpenAPI (from the registry)

**JSON Schema reflector** (`internal/actions/schema.go`, JSON Schema 2020-12 subset used by OpenAPI 3.1):
- struct → `object` with `properties` from `json` tags, `required` for non-`omitempty`, non-pointer fields, `additionalProperties: false`;
- `string`, `bool`, integers (`minimum: 0` for unsigned), floats, slices/arrays, pointers (`type: [T, "null"]`), `time.Time` (`date-time`);
- a named string type with `Enum() []string` → `enum`; `sd:"secret"` → `writeOnly: true, format: password`; `doc:"…"` → `description`;
- string-keyed maps → `additionalProperties`; interfaces → `{}`;
- anything else → error, and a test over every registered action fails.

**Generic REST handler:**
- `POST /api/v1/actions/{name}`; the body is the action input (≤ 64 KiB in the 1 MiB HTTP cap).
- Option headers: `SD-Dry-Run: 1`, `SD-Confirm: <target>`. The request id is always generated by the server and returned in the response.
- **Success:** `200 {"data": …, "plan": … (dry runs), "audit": "recorded" | "unrecorded" | "not_audited", "request_id": …}`. `unrecorded` is a warning in the UI, never a success (rule 5).
- **Errors:** `{"error": {"code", "message", "request_id"}}` with `400 invalid_input`, `401 unauthenticated` / `token_expired` / `idle_expired` / `absolute_expired` / `revoked`, `403 denied` / `csrf` / `origin`, `404 unknown_action` (also `ConsoleOnly`), `421 misdirected`, `429 rate_limited`, `503 audit_unavailable` / `busy`, `500 action_failed` (redacted).
- **`audit.verify` is limited to 2 calls a minute per actor**, hard-coded in the handler for that one action (plan 0002).

**OpenAPI 3.1:**
- `GET /api/v1/openapi.json` (session or token), generated on request, not cached.
- One path per non-`ConsoleOnly` action, `operationId` = name, `x-sd-risk`, `x-sd-scope`, `x-sd-sensitive`, `x-sd-session-only`. Security schemes `cookieAuth` (with the CSRF header) and `bearerAuth`. The pre-session endpoints are a hand-written fragment.
- **Golden file** `docs/api/openapi.json` (`go test ./internal/api -run TestOpenAPIGolden -update`), checked on every PR. The SPA's types (`web/src/lib/api/types.ts`) are generated from it by `openapi-typescript`; CI fails if stale.

### Route table and route matrix

Every route is declared in **one table** in `internal/api/routes.go`; the mux is built only from it, and `api.Server.Handle` is removed.

```go
type Route struct {
    Method, Pattern string
    Auth    AuthKind // Public | SetupOpen | Pending2FA | Session | SessionOrToken | DebugSocket
    Upgrade bool     // WebSocket: requires Session and an exact Origin
    Handler http.Handler
}
```

| Method | Path | Auth | Notes |
|---|---|---|---|
| GET | `/healthz` | Public | |
| GET | `/` and SPA files (fallback `index.html`) | Public | static, CSP |
| GET | `/agent/` | Public | always 404 (ADR 0009) |
| GET | `/api/v1/setup/status` | Public | `{setup_required, token_expires_at, token_file, server_time}`; only `{setup_required:false, server_time}` once an admin exists |
| POST | `/api/v1/setup/enroll` | SetupOpen | `{token}`; 404 once an admin exists |
| POST | `/api/v1/setup/complete` | SetupOpen | sets the session cookie; returns recovery codes and CSRF token |
| POST | `/api/v1/auth/login` | Public | sets the pending cookie |
| POST | `/api/v1/auth/2fa` | Pending2FA | sets the session cookie |
| GET | `/api/v1/auth/session` | Session | user, CSRF token, fresh-until, notices |
| POST | `/api/v1/auth/reauth` | Session | step-up |
| POST | `/api/v1/auth/logout` | Session | |
| POST | `/api/v1/actions/{name}` | SessionOrToken | |
| GET | `/api/v1/openapi.json` | SessionOrToken | |
| GET | `/debug/mem`, `/debug/agents`, `/debug/agents/{id}/logs/{container}` | DebugSocket | debug socket only |

**The route-matrix test** (`internal/api/routematrix_test.go`; skeleton in step 6a, extended by every later step and milestone; its SetupOpen cells run against stub handlers until **step 9 at the latest**, when they switch to the real handlers, with the real behaviour already covered by the explicit 6b tests):
- the real handler, a test registry with every built-in action plus one test action per policy cell, and a test-only WebSocket route;
- for **every route** (and every registered action on `/api/v1/actions/{name}`) × every credential case: none; pending cookie; valid session without CSRF; valid session with CSRF; session with fresh 2FA; **stale (unknown, expired, revoked) session cookie**; token `read`, `write`, `admin`; expired and revoked token; cookie **and** `Authorization` together; token in a query string; wrong `Host`; **cross-site `Origin`**; wrong content type;
- explicit cases: a stale session cookie on `auth/login`, `auth/2fa` and `setup/complete` (ignored, then cleared or revoked); a pending cookie plus a valid session cookie on `auth/2fa` (pending used, old session revoked on success); a cross-site `Origin` on `setup/enroll`, `setup/complete`, `auth/login` and `auth/2fa` → 403;
- the expected status for each cell is in the test; a route missing from the expectations fails it;
- a recording wrapper around the registry asserts **no request produced a `system` actor** and no token reached a `Sensitive`, `Destructive`, `SessionOnly` or `ConsoleOnly` action;
- a mounted pattern not in the table fails the test;
- **debug socket:** every `DebugSocket` route answers 404 on the control socket and through Caddy, and every other route answers 404 on the debug socket;
- step 14 adds an end-to-end subset through real Caddy on `:443` and `:80`.

### `serverdock admin reset`

- `serverdock admin reset [--data DIR] [--yes]`. Without `--yes` the operator types `reset` (stdin; refuses without a terminal and without `--yes`).
- **Owner uid only.** It must run as the uid that owns `DATA`. Running with euid 0 against a `DATA` owned by another uid is **refused** (no privilege dropping): "DATA is owned by serverdock; run: sudo -u serverdock serverdock admin reset". Any other non-owner is refused by `datadir.Prepare`. This guarantees it can't leave root-owned files in `DATA`.
- **Docker form** (documented): `docker run --rm -v <volume>:/var/lib/serverdock <image> admin reset --yes` (the image runs as the uid that owns the volume; stop the server container first).
- Follows the [startup order](#startup-order-one-rule-for-server-and-admin-reset): umask, `datadir.Prepare`, **`store.Open`, which takes the flock and runs pending migrations** (so a reset with a newer binary upgrades the DB, with M1's pre-migrate backup). A running server holds the flock → "ServerDock is running; stop it first (systemctl stop serverdock)", exit 1.
- Builds a registry holding only `auth.admin_reset`, takes the `SystemInvoker`, and invokes it. One transaction: clear the credentials as in [G4c](#auth-state-machine-g4); delete recovery codes, sessions, pending rows and known IPs; revoke every token. Then create `master.key` if it is absent. Intent and outcome rows as for any destructive system action.
- Prints counts (sessions deleted, tokens revoked), the audit row id, and "Start the server; it will write a new setup token to <DATA>/setup-token". Never prints a secret. Exit codes 0 / 1 / 2.
- Keeps the panel leaf, hub CA, agent token and an existing master key.

### SPA shell (ADR 0005)

**Build and embedding.**
- `web/`: SvelteKit, `adapter-static` in SPA mode (`fallback: 'index.html'`, `ssr = false`, `prerender = false`), TypeScript, Svelte 5, hand-written CSS with custom properties, system font stack.
- **Node runs only in CI, in `make web` and `make dev`** (QM2-1). Output goes to `web/build/` (git-ignored).
- **Build tags:** `web/embed_placeholder.go` (`//go:build !spa`) embeds the committed placeholder `web/dist/index.html`; `web/embed_spa.go` (`//go:build spa`) embeds `web/build`. `serverdock version` prints `ui=spa` or `ui=placeholder`; the release job and the bench assert `ui=spa` (SYS14).
- **Bench image (SYS14):** no Node stage. CI's `web` job uploads `web/build` as an artifact; the `bench` and `build` jobs download it into the checkout and build with `-tags spa`. `bench/Dockerfile` copies the context (which then contains `web/build`) and builds with `-tags spa`; run locally, `make bench` runs `make web` first. The bench fails if `serverdock version` doesn't say `ui=spa`.
- **Dev loop (SYS13):** `make dev` runs the server binary on high ports (`--listen 127.0.0.1:8080 --https-listen 127.0.0.1:8443`, temp `DATA`) and Vite's dev server with a proxy for `/api` to `https://localhost:8443` (`secure: false`, `changeOrigin: false`). Vite itself serves **HTTPS** with a self-signed dev certificate, so `Origin` is `https://localhost:5173` and equals `https://` + `Host`; the Origin rule is never loosened for dev. Step 11a checks that a POST succeeds through the dev proxy.
- **Pre-compressed assets:** `.gz` beside each `.js` and `.css` (gzip -9); served with `Content-Encoding: gzip` and `Vary: Accept-Encoding` when accepted. `/_app/immutable/` gets `max-age=31536000, immutable`; `index.html` gets `no-cache`. No runtime compression.
- **Budget check** (`web/scripts/size-check.mjs`): gzip JS under `_app/immutable` ≤ **250 KB**; the summary prints first-load and total JS.

**Layout source (A11).** There is no wireframe for sign-in, two-factor, recovery codes or Settings › Security. They follow the **Onboarding card layout** (sign-in screens) and the **Settings layout** (Security page), and `accessibility` reviews the result in steps 12–13. Hints are paragraphs linked to their fields, never placeholder text.

**Screens:**

1. **Setup** (`/setup`), "Step 1 of 2" / "Step 2 of 2" (QM2-3), the stepper an `<ol>` with `aria-current="step"`:
   - **Step 1, "Prove you own this server":** the token field (the SPA keeps it; `setup/enroll` needs the username too, so it is called when step 2's username and password are submitted and returns the key and QR, not at the end of step 1). Where the token is: the real path from `setup/status` in a code block with a Copy button (`sudo cat /var/lib/serverdock/setup-token`). **The wireframe's `docker logs serverdock | grep "setup token"` copy is replaced**, because the token is never logged. "Check the certificate first": compare the browser's certificate details with the fingerprint in the server log (docs link), and the SSH-tunnel alternative. The expiry time ("This token works until 14:05 UTC. After that, restart ServerDock to get a new one."). Step 1 has no Back button (there is nothing before it).
   - **Step 2, "Admin account and two-factor":** username (default `admin`) and new password first (rule shown before typing, show/hide toggle, no confirm field); submitting them calls `setup/enroll {token, username}`; then the TOTP key and QR (the account label is the validated username), then the 6-digit code. A Back button returns to step 1 keeping the username.
   - **Recovery codes** ("Save your recovery codes", still "Step 2 of 2"): see A7.
2. **Sign in** (`/login`) and **Two-factor** (`/login/2fa`, with "Use a recovery code instead").
3. **Signed out** (`/signed-out?reason=…`): says why (signed out by you; idle 12 h; 7-day limit; step-up failures; password or 2FA changed; revoked from another device) and offers "Sign in again".
4. **Shell:** `<header>` on phones, `<nav aria-label="Main">` with **Overview** and **Settings** only (QM2-3) and `aria-current="page"`, one `<main>`, and the footer "ServerDock itself · 52 MB RAM · 0.3% CPU · v0.x" from `system.info`, polled every 15 s **only while the page is visible**. Phone (< 768 px): a top bar with a visible **"Menu"** text button.
5. **Overview** (`/`): the empty state ("No services yet", "Deploying from an image arrives in the next version", docs link) and the banners.
6. **Settings › Security** (`/settings/security`): a labelled sub-nav (`<nav aria-label="Settings">`) with **Security** only (QM2-3); panel certificate fingerprint, created and expiry dates; admin created from/at; sessions; API tokens; change password; re-enrol 2FA; regenerate recovery codes.

**Client API layer** (`web/src/lib/api.ts`): same-origin `fetch`, JSON content type, the CSRF header on unsafe methods, the error envelope as typed errors, 401 → Signed-out with the reason, 403 "fresh 2FA required" → step-up dialog then one retry, `Retry-After` exposed, a network failure mapped to "Could not reach the server. Nothing was changed."

### Accessibility (WCAG 2.2 AA, plan 0002 rule 6)

Base rules: every input has a visible label; hints and errors linked with `aria-describedby`; a failed submit moves focus to an error summary linking each field; route changes move focus to the `<h1>` and set `document.title` ("Sign in · ServerDock"); `lang="en"` on `<html>`; a skip link; visible focus with `scroll-padding-top` so a focused element is never hidden under the sticky phone bar; contrast AA for text and non-text (footer text included); reflow at 320 CSS px; no `user-scalable=no`; 44 × 44 px targets; states as word + symbol (● active, ▲ expired, ◌ unknown).

- **A1 — Time limits.**
  - Setup shows the token's actual expiry time and says what happens at expiry. If the token expires while the form is open, the expired message appears and the username is kept.
  - The two-factor step says up front it lasts 5 minutes, announces politely about 1 minute before expiry ("About a minute left"), and on expiry shows "This step expired. Sign in again" with the username kept.
  - The Signed-out screen says why (step-up failures, idle, absolute expiry, revoked, credentials changed).
  - Playwright runs these without a test-only server flag: each state-changing test starts a fresh binary and expires or revokes state by editing its temporary database with Node's built-in `node:sqlite` (`sessions.idle_expires_at` and `abs_expires_at`, `auth_pending.expires_at`, `setup_state.expires_at`), and `page.clock` moves the browser's timers. `web/e2e/server.ts` therefore has to follow any renamed or removed column in those tables (a migration that touches them must update it).
- **A2 — Live regions.**
  - The back-off countdown updates visually every second but is announced only at the start ("Too many attempts. Try again in 2 minutes"), at coarse steps (each minute, then at 10 s), and at the end ("You can try again now").
  - One assertive region (`role="alert"`) per page carries errors; status messages use one `role="status"` region.
  - "Signing in…": the form gets `aria-busy="true"`, a status message is announced, and the button uses `aria-disabled="true"` (not `disabled`) so focus stays on it.
  - "Copied" is announced through the status region.
  - The footer poll is **not** a live region; on failure or when older than 45 s it shows "unknown" (◌) instead of stale numbers (A12).
  - Banners use `role="status"`; the "audit record could not be written" warning uses `role="alert"`.
- **A3 — QR code.**
  - DOM order: the text key (grouped in fours, with Copy) comes **before** the QR. The QR has `role="img"` with the label "QR code for your authenticator app" (never the secret), or is `aria-hidden` next to the text key.
  - Always dark modules on a light background, a 4-module quiet zone, never inverted in dark mode or forced colours (it keeps its own background), and a minimum size that fits at 320 px (at least 2 px per module, about 170 px).
  - The `otpauth://` URI is offered as a link ("Open in authenticator app on this device") and as a copyable field, with a one-line instruction for same-device setup.
- **A4 — Themes and preferences.**
  - Light and dark via `prefers-color-scheme`; axe colour-contrast checked in both.
  - `prefers-contrast: more` (stronger borders, no low-contrast greys) and `forced-colors: active` (system colours, borders on focus and buttons, symbols kept) are supported.
  - **Reduced motion** (`prefers-reduced-motion: reduce`) means: no transitions or animations longer than 0 s except opacity fades ≤ 100 ms; no smooth scrolling; the busy spinner becomes a static "…" with text.
  - Text-spacing check: content survives WCAG 1.4.12 spacing (a Playwright test injects the spacing CSS and asserts no clipped text in key screens).
- **A5 — Testing bar.**
  - axe must report **zero violations** under the tags `wcag2a`, `wcag2aa`, `wcag21aa`, `wcag22aa` and `best-practice`.
  - Playwright projects: desktop Chromium 1280 × 800; phone 390 × 844; **320 × 568**; **200% zoom** (1280 × 800 at device scale and CSS zoom); **dark**; **`reducedMotion: 'reduce'`**; **`forcedColors: 'active'`**; **WebKit** desktop (self-signed leaf and SNI behaviour in Safari's engine).
  - Each UI step lists its screen states explicitly (below), and axe runs on every one.
  - A **manual screen-reader pass** (VoiceOver on iOS, plus NVDA on Windows or VoiceOver on macOS) is an exit item for steps 12 and 13, recorded in the PR.
- **A6 — Errors and no data loss.**
  - Typed input is kept on any failure; only passwords and one-time codes may be cleared.
  - Distinct, plain messages: token wrong / expired / already used; "That code was already used. Wait for the next one"; the password rule shown before typing; "This enrolment was replaced; scan the new code"; "Could not reach the server. Nothing was changed."
  - The generic sign-in error says what to do next: "Wrong username or password. Check both and try again. Forgot it? Ask the server's operator to run `serverdock admin reset`."
  - Code fields: `inputmode="numeric"`, `autocomplete="one-time-code"`, paste allowed, **no auto-submit**.
  - **3.3.8:** paste allowed everywhere; `autocomplete` (`username`, `current-password`, `new-password`, `one-time-code`); copy buttons for the key, recovery codes and new tokens.
- **A7 — Recovery codes.**
  - Shown as a labelled list (`<ol aria-label="Recovery codes">`) of text, with "Copy all" and "Download as text".
  - A `beforeunload` guard until "I have saved these codes" is ticked.
  - A note: "You can make new codes later in Settings › Security."
  - Continue uses `aria-disabled="true"` with a stated reason ("Tick the box to confirm you saved the codes") until the box is ticked.
- **A8 — Confirmation dialogs.**
  - Name the exact target and consequence: token name and prefix; the session's device and IP, or "this device (you will be signed out)"; the count for "Sign out everywhere else" ("3 other sessions"); "Your 6 unused codes will stop working"; "All 4 sessions and 2 API tokens will be revoked" (with the keep-tokens option stated).
  - Verb-specific buttons ("Revoke token", "Sign out 3 sessions"), default focus on **Cancel**, Escape closes, focus is trapped only inside the dialog, and when a row is removed focus moves to the next row's action, else the previous one, else the table heading.
- **A9 — Tables** (sessions, tokens): below 768 px a stacked card layout (each card a list with labels); above, a `<table>` with `<caption>` and `<th scope>`; states as word + symbol; absolute times with timezone ("2026-11-20 14:02 UTC").
- **A10 — Page structure:** as in the shell above; the phone menu closes on Escape and returns focus to the Menu button.

## Footprint rules for new Go code (S4)

S4 (remote agent + its Caddy) is **44.2 MB against 45.0 MB**: 0.8 MB of headroom, and the same binary runs in agent mode.
- **No package-level work** in `internal/auth`, `internal/secretbox`, `internal/datadir`, `internal/api` or the schema reflector: no package-level `regexp.MustCompile`, no init-built tables, no `init()`. Use `sync.OnceValue`. Step 4 adds a check (a test that parses these packages and fails on package-level calls other than `errors.New`).
- The embedded SPA is read-only data; agent mode never reads it.
- The only new linked module code is `golang.org/x/crypto/argon2` (and `blake2b`, whose `init` only registers a hash).
- The certmagic maintenance goroutine already counts in S4 (since M1); M2 doesn't change it.

## Footprint budget

> Since [ADR 0011](../decisions/0011-footprint-ceiling.md) (2026-11-21) the S2/S4 numbers below are tripwire allocations; CI fails only above 200 MB (S2) / 100 MB (S4). M2 measured S4 46.0 MB (over the 45.0 allocation); the footprint investigation found no leak (file pages of the shared binary plus lazy heap return), recorded here as the written reason.

**Allocation (plan 0002):** S2 ≤ 53 MB, S4 ≤ 45 MB, slack + 2 / + 0.5 MB. M1: S2 47.6 MB, S4 44.2 MB, binary 18.9 MB gzip.

| | M1 measured | M2 estimate | Allocation | Where the cost comes from |
|---|---|---|---|---|
| **S2** | 47.6 MB | **≈ 50 MB (range 49.5–51.5)** | ≤ 53 | Caddy's TLS server on `:443` and the loaded leaf (+0.5–1.0); registry, route table and schema (+0.2); SQLite pages for new tables (+0.1–0.3); SPA index touched by the burst. |
| **S4** | 44.2 MB | **≈ 44.2–44.4 MB** | ≤ 45 | Nothing runs in agent mode; the agent's Caddy config is byte-identical (golden test); no package init. |
| Idle CPU | ≤ 0.01% | unchanged | | **No new timers or goroutines at idle.** |
| Binary (gzip) | 18.9 MB | ≈ 19.4 MB | ≤ 40 MB | SPA assets, new Go code |
| SPA JS | — | ≤ 250 KB gzip | ≤ 250 KB | new CI check |

**Not like-for-like:** from step 3 on, S2's burst goes over HTTPS, so the burst includes TLS handshakes, and the M1 → M2 S2 delta includes TLS session state that the M1 number didn't have. The PR for step 3 reports S2 before and after that step separately.

**Bench changes (R4):**
- **S1:** the control plane only has its socket. The server runs with `--control-socket /debug/cp.sock` on the shared volume; readiness and the burst run in the curl helper container on that volume (`--unix-socket`). Success: 200.
- **S2:** maps `:443`; readiness and burst use `curl -k https://…`. Success: 200.
- **S3 and S4 server stack:** the server container maps `:443`; readiness uses `curl -k https://…/healthz`. **S3's burst** hits the server's `https://…` (as before, now over TLS); success 200.
- **S4's burst** goes through the agent's own Caddy (`--proxy-upstream sdbench-srv:80`), whose upstream now answers 308. The agent's proxy does the same work either way, so for S4 the burst counts **200 or 308** as success (`burst` takes the accepted codes as an argument). The agent's config is unchanged.

**Measured, and when:** CI bench on every Go PR; **QUICK S4 deltas** in the PRs for steps 4, 9 and 11a (rule 4: a QUICK delta > 0.5 MB needs a full S4 run); a full S4 at step 14; **report-only "S2 after one login"** at steps 6b and 14 (VmRSS 10 s and 60 s after one login; expected S2 + ≤ 1 MB; peak about S2 + 19–22 MB, inside the 64 MiB soft limit).

**Raises idle RAM?** Only the `:443` TLS server, which Q2 requires.

### Dependencies (plan 0002 rule 4)

| Dependency | Kind | Gzip binary delta | S4 QUICK delta | Why |
|---|---|---|---|---|
| `golang.org/x/crypto/argon2` (module already indirect) | Go, runtime | est. +20–40 KB (step 4) | est. 0 (step 4) | no argon2 in the standard library |
| `uqr` (approved, QM2-1) | web runtime, lazy chunk on enrolment screens | 0 Go; ≈ 5 KB gzip JS | 0 | a QR encoder isn't worth hand-writing; a Go one would add code to every mode |
| `svelte`, `@sveltejs/kit`, `@sveltejs/adapter-static`, `vite`, `typescript`, `svelte-check` (approved, QM2-1) | web build-time | in the JS budget | 0 | ADR 0005 |
| `@playwright/test`, `@axe-core/playwright`, `openapi-typescript` | test/dev | — | — | don't count |
| Node.js (pinned LTS) | CI and `make web`/`make dev` only | — | — | don't count |

Dependabot gets an `npm` entry for `/web`; the web job runs `npm audit --omit=dev` (fails on high or critical).

## Security notes

- **New surface:** `:443`, setup, login, 2FA, step-up, the REST action endpoint, OpenAPI, the SPA. `:80` only redirects. The control plane loses its TCP listener.
- **Secrets:**

  | Secret | At rest | In transit | Never in |
  |---|---|---|---|
  | Admin password (current and new) | argon2id PHC | POST body | logs, audit, errors, URLs |
  | TOTP seed, pending seeds (setup, re-enrolment) | secretbox | enrolment response only | logs, audit, later responses |
  | `enrollment_id` | SHA-256 | enrol response, complete/confirm body | logs, audit |
  | Recovery codes | SHA-256 | setup / regenerate response | logs, audit |
  | Setup token | SHA-256; plaintext in `DATA/setup-token` 0600 until used | POST body | logs (path only), URLs |
  | Session id, pending id | SHA-256 | `__Host-` cookies | logs, audit, URLs, JS |
  | CSRF token | not stored (derived) | response body, `X-SD-CSRF` | logs, audit, URLs |
  | API token | SHA-256 | `Authorization` | logs, audit, URLs, cookies |
  | Master key | `DATA/keys/master.key` 0600 | never | anywhere else |
  | Panel leaf key | `DATA/pki/panel.pem` 0600 | never | anywhere else |

- **Permissions:** `DATA` 0700, umask 0077, socket dirs 0700, `admin reset` as the owner uid only.
- **Token powers (Q8):** never sensitive, destructive, `SessionOnly` or `ConsoleOnly`.
- **No `system` actor from HTTP:** the API package never gets the `SystemInvoker`; `auth.admin_reset` exists only in the `admin reset` process; the route matrix asserts it.
- **Accepted risks** (ADR 0010, `docs/security.md`): first-run trust on a self-signed leaf after G5; login availability (distributed attack; a password holder delaying the second factor by up to 1 h); in-memory back-off reset on restart; host-scoped, not port-scoped, cookies on IP and tunnel access (SEC6); a lost master key forces `admin reset`; the `:80` redirect port is wrong behind Docker port mapping.

### Gap coverage (G2–G10)

| Gap | Design section | Tests | Step |
|---|---|---|---|
| **G2** | [Control plane on a unix socket](#control-plane-on-a-unix-socket-g2) | `TestNoTCPControlListener`; `TestForgedXFFOnSocketIgnoredByCaddy`; `TestXFFIgnoredWithoutSocketMark`; `TestMultipleXFFValuesRejected`; `TestControlSocketModes`; `TestControlSocketConnectionCap` | 2 |
| **G3** | [Login protection](#login-protection-g3) | `TestBackoffDoublingAndCap`; `TestBackoffNeverEvictsBlocked`; `TestBackoffEvictsUnblockedOldestFirst`; `TestBackoffFullOfBlockedRefusesNewKey`; `TestBackoffInstancesIndependent`; `TestUnknownUsernamesShareOneBucket`; `TestIPv6Slash48Bucket`; `TestKnownIPExemptFromAccountBackoff`; `TestKnownIPReachesHashWhenMapsFullOfBlocked`; `TestPendingTokenFiveTries`; `TestSecondFactorBackoffAcrossIPs`; `TestReauthFailuresRevokeSession`; `TestHasherFast503`; `TestHasherWait503`; `TestBackoffCheckedBeforeHash`; `TestAdminNoticeAfterTOTPFailures` | 4, 6b |
| **G4** | [Auth state machine](#auth-state-machine-g4), [Credential resolution](#credential-resolution-g8) | `TestPendingIsNotASession`; `TestSetupChecksBeforeHash`; `TestSetupCompleteIsAtomic`; `TestConcurrentSetupOneWinner`; `TestAdminExistsDefinition`; `TestSetupReopensOnlyWithNewTokenAfterReset`; `TestLoginMintsNewSessionID`; `TestOldSessionRevokedAtLogin`; `TestClientChosenSessionIDRefused`; `TestPendingPrunedAndCapped`; matrix stale-cookie cases | 5, 6a, 6b, 7 |
| **G5** | [First-run on-path risk](#first-run-on-path-risk-g5) | `TestStartupLogHasPathFingerprintAndTunnelHint`; `TestAdminCreatedLoggedAndStored`; `TestSetupStatusNeverReturnsFingerprint`; Playwright: tunnel hint, no fingerprint, "admin created from <IP> at <time>" on the Security page (it replaced the deferred banner) | 5, 6b, 12 |
| **G6** | [Data directory](#data-directory-and-file-modes-g6), [Secretbox](#secretbox-internalsecretbox-g6) | `TestDataDirCreated0700`; `TestLooserDataDirRefused`; `TestForeignOwnedDataDirRefused`; `TestFileModesAfterFirstRun` (built binary); `TestSecretboxAADBinding`; `TestSecretboxRefusesBadKeyFile`; `TestMissingKeyWithCiphertextsRefusesStart`; `TestWrongKeyIDClearError`; `TestTOTPSeedNotInDBPlaintext`; `TestSecretPathsCoverEverySecretFile`; `TestAuditVerifyAsRootOnServiceOwnedData` | 1, 5, 10 |
| **G7** | [Sessions](#sessions-g7) | `TestExpiredSessionsPrunedOnInsert`; `TestSessionCapTwenty`; `TestLastSeenWrittenOncePerMinute`; `TestLastSeenFailureIsBestEffort`; `TestExpiredSessionRefusedClearedAndDeleted` | 5, 6a |
| **G8** | [Host allowlist](#host-allowlist-origin-and-security-headers-g8), [Credential resolution](#credential-resolution-g8) | route matrix (every cell); `localhost`, IP literals and `--panel-host` accepted; `localhost.evil.test` → 421; WebSocket test route | 6a, 6b, 8 |
| **G9** | step 10 | `TestSecretsNeverLeak` (full sentinel set, see step 10) | 10 |
| **G10** | [Panel TLS](#panel-tls-q2-adr-0010-g10) | `TestPanelLeafNotFromHubCA`; `TestPanelLeafPathAndModes`; `TestPanelLeafRegeneratedWithWarning`; `TestPanelLeafRenewedUnder60Days`; `TestPanelFingerprintLoggedEveryStart`; SNI tests; `TestSecretPathsCoverEverySecretFile` | 3 |

## Steps (each one PR-sized, in order)

Security-critical foundations first, then the auth core, then the HTTP surface, then the UI. **Each step updates its own rows in `docs/security.md`** (Gap → Built, with test names). **Migrations:** M2's schema is migration `0003_auth.sql` (step 5). Once step 5 merges, 0003 is never edited; any later M2 change adds `0004_…` and up (M1 checksum rule).

1. **ADR 0010, data directory, startup order, secretbox (G6).**
   - Do: copy Appendix A to `docs/decisions/0010-panel-tls-and-auth.md` (status "proposed"); `internal/datadir`; umask 0077; the startup order (move the hub token and pki calls after `store.Open`); `internal/secretbox` and the key rules; `internal/testbin` (built-binary harness); `TestAuditVerifyAsRootOnServiceOwnedData` beside the kept `TestAuditVerifyAsAnotherUser` (with the test-only mode-check skip); `bench/Dockerfile` `/out/data` 0700.
   - Tests: `TestDataDirCreated0700`, `TestLooserDataDirRefused`, `TestForeignOwnedDataDirRefused`; umask (built binary: a new file is 0600); `TestSecondServerFailsOnLockAndFirstStillAnswers`; secretbox round trip, `TestSecretboxAADBinding`, wrong key id refused, `TestSecretboxRefusesBadKeyFile`, key file 0600 in 0700, atomic write; both `audit verify` tests.
   - Reviewers: `reviewer`, `security`, `systems`.

2. **Control plane on a unix socket (G2).**
   - Do: `--control-socket`, remove `--internal`; `listenPrivateSocket`; 256-connection cap kept; the connection mark; `limits.ClientIP(r)`; Caddy upstream `unix//…`; the agent proxy keeps its TCP upstream. Bench S1 through the helper on the shared volume.
   - Tests: `TestNoTCPControlListener`, `TestControlSocketModes` (0600 in 0700, foreign owner refused, long path refused), `TestControlSocketConnectionCap`, `TestForgedXFFOnSocketIgnoredByCaddy` (real Caddy, client sends `X-Forwarded-For: 6.6.6.6`, the handler sees one value equal to the real peer), `TestXFFIgnoredWithoutSocketMark`, `TestMultipleXFFValuesRejected`, control and debug paths must differ, agent `BuildConfig` golden unchanged.
   - Reviewers: `reviewer`, `security`, `systems`, `footprint`.

3. **Panel TLS and the `:80` redirect (Q2, G10).**
   - Do: `pki.EnsurePanelLeaf` (825 days, renew under 60 days at start, `--panel-host` SANs); `--https-listen` and `--panel-host`; `proxy.Config.Panel` (TLS server with `default_sni`/`fallback_sni` = `localhost`, redirect server); `system.panel_cert` data source; the certmagic row moved to "running since M1" in `docs/architecture.md` and plan 0002 rule 3. Bench S2, S3 and S4 changes (R4). The Host/Origin/header middleware is **not** in this step (6a).
   - Tests: `TestPanelLeafNotFromHubCA`, `TestPanelLeafPathAndModes`, `TestPanelLeafRegeneratedWithWarning`, `TestPanelLeafRenewedUnder60Days` (injected clock), `TestPanelFingerprintLoggedEveryStart`; **SNI:** a handshake with **no SNI** to an IP that is **not** in the leaf's SANs gets the panel leaf; SNI `localhost` gets it; SNI = an arbitrary name (`example.invalid`) gets it; `:80` → 308 with the port taken from `--https-listen` and nothing proxied; `:443` `/healthz` with the leaf matching `panel.fingerprint`; Caddy at WARN by default.
   - Reviewers: `reviewer`, `security`, `systems`, `footprint` (S2 delta before/after this step reported separately).

4. **Back-off, the hash semaphore and argon2 (G3 core).**
   - Do: `limits.Backoff` (oldest-first eviction of unblocked keys, `bypassFull`); `limits.Key48`; `auth.Hasher` (PHC, dummy hash, semaphore, `FreeOSMemory` from the releasing goroutine); the package-level-work check.
   - Tests: `TestBackoffDoublingAndCap`, `TestBackoffNeverEvictsBlocked`, `TestBackoffEvictsUnblockedOldestFirst`, `TestBackoffFullOfBlockedRefusesNewKey`, `bypassFull` admits a known key, `TestBackoffForgetsAfter24h`, `TestIPv6Slash48Bucket`, `TestHasherFast503` (5th caller < 50 ms), `TestHasherWait503` (≈ 2 s), `FreeOSMemory` not on the response path (the response returns before it runs), PHC round trip and parsing, the dummy hash within 20% of a real one, the package-level-work check.
   - Footprint: QUICK S4 and binary deltas in the PR.
   - Reviewers: `reviewer`, `security`, `footprint`.

5. **Auth core (G4, G6, G7).** No HTTP yet.
   - Do: migration `0003_auth.sql`, listing every column:
     - `users(id, username UNIQUE NOCASE, password_hash, totp_seed, totp_pending_seed, totp_pending_expires_at, totp_pending_enrollment_hash, totp_pending_tries, totp_confirmed_at, totp_last_counter, totp_enrolled_host, scope, setup_ip, setup_at, password_changed_at, totp_fail_since_login, totp_fail_last_at, totp_fail_last_ip, created_at)`;
     - `recovery_codes(user_id, hash, used_at)`;
     - `sessions(id_hash, public_id, user_id, created_at, last_seen_at, idle_expires_at, abs_expires_at, mfa_fresh_until, reauth_failures, ip_key, user_agent, notice_json)`;
     - `auth_pending(id_hash, user_id, expires_at, tries_left, ip_key, created_at)`;
     - `auth_known_ips(user_id, ip_key, last_ok_at)`;
     - `api_tokens(id, user_id, name, prefix, hash UNIQUE, scope, created_at, expires_at, last_used_at, revoked_at)`;
     - `setup_state(id CHECK = 1, token_hash, expires_at, enrollment_hash, pending_seed, created_at)`;
   - `auth.Store` methods for every state-machine transition (setup's read-only checks, then compare-and-delete); the setup token at start; the startup log line; TOTP; recovery codes; sessions (create, lookup with reasons, rotate, prune, cap, best-effort `last_seen_at`); pending rows (prune, cap 5 per user); the master-key "ciphertexts exist" check.
   - Tests: RFC 6238 Appendix B vectors; ±1 accepted, ±2 refused; `TestTOTPCounterNeverReused` (sequential and concurrent); `TestAdminExistsDefinition`; `TestSetupChecksBeforeHash` (wrong token, enrolment or code → the instrumented hasher is never called); `TestSetupCompleteIsAtomic` (fault after each statement); `TestConcurrentSetupOneWinner`; expired token, replaced enrolment refused; `TestSetupTokenFileModeAndRemoval`; `TestStartupLogHasPathFingerprintAndTunnelHint`; `TestPendingTokenFiveTries`; `TestPendingPrunedAndCapped`; `TestExpiredSessionsPrunedOnInsert`; `TestSessionCapTwenty`; `TestLastSeenWrittenOncePerMinute`; `TestLastSeenFailureIsBestEffort`; `TestExpiredSessionRefusedClearedAndDeleted`; `TestTOTPSeedNotInDBPlaintext`; `TestMissingKeyWithCiphertextsRefusesStart`; `TestWrongKeyIDClearError`; `TestFileModesAfterFirstRun` (built binary); migrations fresh and from `v2.db`.
   - Reviewers: `reviewer`, `security`, `systems`.

6a. **Route table, credential resolution, CSRF, middleware, matrix skeleton (G8).**
   - Do: `internal/api/routes.go` (remove `Server.Handle`); credential resolution by `AuthKind` with cookie clearing; CSRF derivation; Host allowlist (421), `Origin` (when present, on every unsafe request), JSON content type, headers, CSP; the debug routes moved into the table; **the Host allowlist must also apply to `:80`, or `:80` redirects to a fixed name** (today it reflects any `Host`, so a forged `Host` yields a redirect to an attacker-chosen name; the redirect answers only 308 with no cookies, but it must not be an open redirect); `routematrix_test.go` with the existing routes and stub handlers for the auth endpoints.
   - Tests: the matrix cells for every route so far; stale session cookie → treated as none and cleared; `Session` route with `Authorization` → 400; token in query refused; `localhost`, IP literals and `--panel-host` accepted, unknown name and `localhost.evil.test` → 421; WebSocket test route needs session and exact `Origin`; CSP hashes match the embedded `index.html`; mounted-but-undeclared pattern fails; no `system` actor.
   - Reviewers: `reviewer`, `security` (required), `systems`.

6b. **Setup, login, 2FA, reauth and logout endpoints (G3 wiring, G4, G5).**
   - Do: the endpoints; back-off wiring in the stated order with the four instances and known-IP rules; reauth failure revocation; the admin notice; auth audit rows (fail-closed intent, `anonymous:*` denials, `<unknown>` usernames); `server_time` in `setup/status`; the "S2 after one login" bench helper (report-only).
   - Tests: matrix cases for each endpoint (stale cookie on login, 2fa and setup; pending + valid session on 2fa; cross-site `Origin` on setup, login, 2fa → 403); `TestPendingIsNotASession`; `TestLoginMintsNewSessionID`; `TestOldSessionRevokedAtLogin`; `TestClientChosenSessionIDRefused`; `TestBackoffCheckedBeforeHash`; `TestBackoffInstancesIndependent`; `TestUnknownUsernamesShareOneBucket`; `TestSecondFactorBackoffAcrossIPs`; `TestKnownIPExemptFromAccountBackoff`; `TestKnownIPReachesHashWhenMapsFullOfBlocked`; `TestReauthFailuresRevokeSession`; `TestAdminNoticeAfterTOTPFailures` (reauth failures counted); `TestNoUserEnumeration`; `TestAdminCreatedLoggedAndStored`; `TestSetupStatusNeverReturnsFingerprint`; `TestAuthIntentFailureFailsClosed` (503, no session); failed-login audit row has `<unknown>` for a non-existent name; setup endpoints 404 once an admin exists.
   - Reviewers: `reviewer`, `security` (required), `systems`.

7. **`serverdock admin reset` (G4c).**
   - Do: `ConsoleOnly`; `auth.admin_reset` registered only in this process; the owner-uid rule; master-key creation; **clearing `meta.secretbox_key_id` in the same transaction as the ciphertexts**; the transaction also clears `sessions`, `auth_pending`, `auth_known_ips`, `recovery_codes`, `totp_last_counter`, the `totp_fail_*` counters and `setup_state.pending_seed` (so a pending password-only row or a session from before the reset can never complete or authenticate: `LookupSession` and `CompleteLogin` also require the user to be an admin, which is already enforced in step 5); the subcommand and its Docker form in the docs; the "Unattended destructive actions" entry. **Row updates for the setup-token deviation:** plan 0002 M2 "`serverdock admin reset`" bullet ("writes a fresh setup token file" → "the next server start writes it") and its Part C row 1 note; `docs/security.md` A11 row "`admin reset` leaving the panel half-configured…" (mitigation text) and the "M2 surface" table's `admin reset` row.
   - Tests: refused while a server holds the flock; refused without a terminal and without `--yes`; **as root against `DATA` owned by another uid: refused with the `sudo -u` hint and no file created or changed in `DATA`**; clears exactly the listed columns and rows, keeps the user id, leaf, CA, agent token and key; creates a missing key; **with the key file removed, reset clears `meta.secretbox_key_id` and then the server starts and creates a new key**; **a pending row (and a session) from before the reset cannot complete or authenticate**; revokes every token; intent and outcome rows with actor `system`; `TestSetupReopensOnlyWithNewTokenAfterReset`; `TestServerRegistryHasNoAdminReset`; the extended unattended-destructive check; `ConsoleOnly` denied for every other actor; no secret in the output; a pending migration runs during reset.
   - Reviewers: `reviewer`, `security`, `systems`.

8. **API tokens and auth-management actions (Q8).**
   - Do: the bearer path (bad-token bucket before lookup, expiry, revocation, `last_used_at` once a minute); `tokens.*`, `auth.password_change`, `auth.totp_reenroll_*`, `auth.recovery_regenerate`, `auth.sessions_*`, `auth.session_info`, `system.panel_cert`; revocation and cookie clearing on password or TOTP change; redacted execution errors; matrix rows for every new action.
   - Tests: no token reaches a `SessionOnly` action at any scope; expiry default 90, max 365, required; expired and revoked → 401; token in a cookie or query ignored; password change revokes all sessions and tokens, `keep_tokens` keeps tokens; re-enrolment swaps the seed and updates `totp_enrolled_host`; `tokens.create` without fresh 2FA → 403; plaintext token never in the audit row; the 11th bad token from one IP in a minute → 429 before any lookup.
   - Reviewers: `reviewer`, `security` (required).

9. **JSON Schema reflector, REST handler, OpenAPI 3.1.**
   - Do: `schema.go`; `Info.OutputType`; the REST handler (dry-run and confirm headers, server-generated request id, envelope, errors, the hard-coded `audit.verify` limit); **cookie clearing: REST clears `__Host-sd_session` after a successful `auth.password_change` or `auth.totp_reenroll_confirm` (they revoke the caller's own session), and after `auth.sessions_revoke` when it removed the caller's own session; REST builds the user actor with `UserSessionActor(...).WithHostKind(hostKind(r.Host))`, so `host_kind` is derived from `r.Host` and is never client input (a caller without a Host records nothing)**; `openapi.json` route and golden; `make openapi`.
   - Tests: **the matrix rows check the clearing `Set-Cookie` for those three actions and its absence for every other action; `host_kind` in a request body is refused;** schema goldens per kind; unsupported kinds error; every action reflects; golden up to date; `ConsoleOnly` absent and 404; `audit: "unrecorded"` on an injected outcome failure; the 3rd `audit.verify` in a minute → 429; error messages carry no sentinel.
   - Footprint: QUICK S4 delta.
   - Reviewers: `reviewer`, `security`, `footprint`.

10. **Secret sentinel suite (G9).**
    - Do: `cmd/serverdock/sentinel_test.go` (skipped with `-short`), using `testbin` with `SERVERDOCK_TEST_PROXY_LOG_LEVEL=DEBUG`: setup (abandoning one enrolment first), login with TOTP and with a recovery code, step-up, token create and use, password change, TOTP re-enrolment (one abandoned), logout, deliberate failures (the password submitted as the username; a wrong password containing a sentinel; malformed JSON containing a sentinel; a bad token), stop, `admin reset`.
    - Tests (`TestSecretsNeverLeak`): none of these appears in the DB file, the WAL (also after a checkpoint), server and Caddy logs, audit `args_json` and `error`, any error body, or any `DATA` file outside `SecretPaths`: current and new passwords; the TOTP seed (raw, base32, in the URI); the abandoned setup and re-enrolment pending seeds; every `enrollment_id`; every recovery code; the setup token; the API token; session and pending ids; the CSRF token; the raw `Authorization` and `Cookie` header values; the **master key bytes** (raw, hex, base64); the **panel private key** (DER, PEM body). Also `TestSecretPathsCoverEverySecretFile`.
    - Reviewers: `reviewer`, `security` (required).

11a. **SPA build, embedding, CI and serving.**
    - Do: the SvelteKit project skeleton; build tags; `serverdock version` `ui=`; pre-compressed serving and cache headers; CSP from the embedded `index.html`; `make web`, `make dev`; CI `web` job (pinned `setup-node`, `npm ci`, `svelte-check`, build, gzip, size check, generated-types check, `npm audit --omit=dev`, artifact); `build` and `bench` jobs download the artifact, build `-tags spa`, assert `ui=spa`; Dependabot `npm`.
    - Tests: gzip negotiation, `Vary`, immutable caching, CSP hashes, no inline event handlers, placeholder build with plain `go test`, size ≤ 250 KB, `ui=spa` assertion fails on a placeholder build.
    - Footprint: QUICK S4 with `-tags spa`; binary delta.
    - Reviewers: `reviewer`, `security` (CSP, serving), `footprint`, `systems` (CI and bench wiring).

11b. **Shell UI and the Playwright harness.**
    - Do: Playwright (global setup runs `testbin` on high ports with a temp `DATA`, reads the token file, `ignoreHTTPSErrors`; all A5 projects including WebKit; a `securitypolicyviolation` listener; axe with the A5 tags after every state); the shell (header, main nav, footer poll with "unknown", phone Menu), Overview empty state, Signed-out screen, `api.ts`, light/dark/contrast/forced-colours CSS, reduced motion.
    - Screen states: shell desktop; shell phone with menu closed and open; Overview empty; footer "unknown"; Signed-out for each reason.
    - Tests: zero axe violations on every state in every project; keyboard-only path through the shell; skip link; menu `aria-expanded`, Escape returns focus; focus not hidden under the sticky bar; polling stops when hidden; footer not a live region; text-spacing check; 44 px targets (bounding boxes).
    - Reviewers: `reviewer`, `accessibility` (required), `security`.

12. **Setup and sign-in screens; first-run e2e over HTTPS.**
    - Do: `/setup` (steps 1–2, the lazy QR chunk, recovery codes), `/login`, `/login/2fa`, step-up dialog, banners, back-off and busy states, A1–A3, A6, A7.
    - Screen states: setup step 1 (empty, wrong token, expired token, network error); step 2 (account form with rule, password error, QR + key, wrong code, reused code with server time, enrolment replaced); recovery codes (unticked, ticked); sign-in (empty, wrong credentials, back-off countdown, busy 503); two-factor (empty, wrong code, about-to-expire warning, expired, recovery-code mode); step-up dialog; admin-created banner; failed-attempts banner; audit-unrecorded alert.
    - Tests (all A5 projects): the full first run from the token file to the Overview; expiry tests with injected short expiries keep the username; countdown announcements only at the stated points; `aria-busy` and `aria-disabled` on submit; recovery-code `beforeunload` guard and Continue reason; QR after the text key, labelled without the secret, light background in dark and forced colours, readable at 320 px; `otpauth://` link present; the setup page shows the real token path with Copy, the tunnel hint and **no** fingerprint; typed input kept on errors; no auto-submit; paste works; QR chunk not loaded on `/login`; zero axe violations; no CSP violations. **Exit item:** manual screen-reader pass (VoiceOver iOS + NVDA or VoiceOver macOS), recorded in the PR.
    - Reviewers: `reviewer`, `accessibility` (required), `security`.

13. **Settings › Security screen.**
    - Do: the page with its sub-nav (Security only); fingerprint and dates (with the under-60-days warning); admin created; sessions and tokens (A9 tables/cards); create token with step-up, show once; revoke; change password; re-enrol 2FA; regenerate recovery codes; A8 dialogs. The password-change and re-enrolment dialogs show "Keep API tokens (only if you are rotating routinely, not after a suspected compromise)", unticked by default (`keep_tokens` defaults to false and is audited).
    - Screen states: page loaded (desktop table, phone cards); empty tokens list; create-token form and its errors; new token shown once; each confirmation dialog (revoke token, revoke session, revoke this device, sign out everywhere else, regenerate codes, change password, re-enrol); step-up prompt; certificate expiry warning.
    - Tests (all A5 projects): each flow end to end; dialogs name target and consequence, default focus on Cancel, Escape closes, focus moves to the next row after removal; after a password change the Signed-out screen gives the reason and the old token fails; a created token works from the test via `fetch` and is shown once; tables have `<caption>`/`<th scope>` and become cards below 768 px; absolute times with timezone; zero axe violations. **Exit item:** manual screen-reader pass, recorded in the PR.
    - Reviewers: `reviewer`, `accessibility` (required), `security`.

14. **Record and close.**
    - Do: `docs/security.md` (deployed shape, A-rows, G2–G10 Built with tests, the accepted risks incl. SEC6, the `audit verify` wording, M2 close-out); ADR 0010 → accepted after `security` approval; `docs/architecture.md` (request path, socket, startup order, Background work rows, layout, security summary); `docs/roadmap.md`; plan 0002 M2 status; bench numbers (CI S2, full S4, binary, SPA JS, "S2 after one login") in `bench/results/` and research/05; `internal/store/testdata/v3.db` with an upgrade test from `v2.db`; the matrix subset through real Caddy.
    - Test: links resolve; CI green; full S4 green.
    - Reviewers: `reviewer`, `security`, `footprint`, `systems`.

## Test plan

- **Unit:** everything named in the steps and the gap table.
- **Integration (real embedded Caddy, temp `DATA`, short socket paths; process-wide behaviour through `testbin`):** first run over HTTPS; SNI cases; `:80` redirect; forged XFF replaced; setup token file mode, removal and absence from logs; concurrent setup; second server on the lock; `admin reset` (running server, root vs owner); both `audit verify` tests; the route matrix in-process and through Caddy; goleak.
- **Sentinel suite (G9):** step 10, every PR (skipped only with `-short`).
- **Playwright + axe:** all A5 projects (desktop, 390 px, 320 px, 200% zoom, dark, reduced motion, forced colours, WebKit); every listed screen state; zero violations under the A5 tags; no CSP violations; keyboard-only paths; 44 px targets. Manual screen-reader passes at steps 12 and 13.
- **CI bench:** S2/S4 gates on every Go PR; QUICK S4 at steps 4, 9, 11a; full S4 at step 14; "S2 after one login" at 6b and 14.
- **Fuzz:** `limits.ClientIP` header parsing, the REST option headers, the PHC parser: seeds in CI plus a 5-minute local run before merge.
- **Manual 1 GB VPS check:** not required (first VPS bench is the M7a exit; Q6 open). If a 1 GB VPS is available, step 14 runs the `-tags spa` binary there, does the first run from a phone and a desktop (once via the SSH tunnel, once with the fingerprint check), and records idle VmRSS before and 60 s after a login in research/05 (informational).

## Reviews required

- `reviewer`: this plan and every step's diff.
- `security` (**required**): ADR 0010 and steps 1–10, 11a, 11b, 12, 13, 14.
- `systems`: steps 1, 2, 3, 5, 6a, 6b, 7, 11a, 14.
- `footprint`: steps 2, 3, 4, 9, 11a, 14.
- `accessibility` (**required on every UI step**): steps 11b, 12, 13.

## Maintainer answers (2026-11-20)

- **QM2-1:** yes to SvelteKit, with Node only in CI and the bench build, and `uqr` loaded only on the enrolment screens.
- **QM2-2:** sessions expire after 12 h idle and 7 days absolute; fresh 2FA is needed for sensitive or destructive actions after 5 minutes.
- **QM2-3:** the sidebar shows only the real items (Overview, Settings); setup shows only its two real steps with a "Step 1 of 2" indicator; Settings shows only the Security item.

No open questions remain for M2.

## Review round 1 changes

| # | Item | Where it was fixed |
|---|---|---|
| R1 | certmagic selects by SNI; IP handshakes have none | Panel TLS › Certificate selection (`default_sni`/`fallback_sni` = `localhost`); step 3 SNI tests; ADR 0010 decision 1 |
| R2 | Credential resolution with stale/extra cookies | Credential resolution (by `AuthKind`, stale cookie = none + cleared, clearing list); matrix cases; step 6a/6b |
| R3 | `admin reset` registration and scope | Actions › `auth.admin_reset` only in the reset process, `TestServerRegistryHasNoAdminReset`, extended unattended check; step 7 |
| R4 | Bench bursts after the `:80` redirect | Footprint › Bench changes (S1–S4 targets and success codes; S2 TLS not like-for-like) |
| R5 | Step split: middleware out of the TLS step | Middleware moved to step 6a; step 3 is TLS only |
| R6 | Keep the M1 `audit verify` test | Data directory › `audit verify` tests (kept + new root test; rationale; security.md wording); step 1 |
| R7 | Where the umask is set for file-mode tests | Data directory (umask in `serverMain`; `testbin` built-binary harness; `TestFileModesAfterFirstRun` uses it) |
| R8 | `admin reset` as root | `admin reset` › owner uid only (refuse with `sudo -u` hint); step 7 test |
| R9 | Socket cap; how compare-and-delete reads | Control plane (256-connection cap kept, test); Auth state machine (b) (read first, rows-affected compare-and-delete, no `RETURNING`) |
| SEC1 | Cookie on pre-session routes | = R2 |
| SEC2 | `admin reset` in the server registry | = R3 |
| SEC3 | Back-off instances, unknown names, eviction | Login protection (four instances, `<unknown>` bucket, oldest-first eviction, known-IP `bypassFull`, remaining risk); ADR 0010 decision 8; tests |
| SEC4 | Reauth brute force | Sessions (5 failed reauth codes revoke the session; counted in `totp_fail_since_login`); `TestReauthFailuresRevokeSession` |
| SEC5 | Username in failed-login audit | Audit of auth events (`<unknown>` unless the user exists); sentinel: password as username |
| SEC6 | Host-scoped cookies on IP and tunnel | Sessions › accepted risk; M3a requirement; Security notes; ADR 0010 consequences |
| SEC7 | Sentinel set | Step 10 (master key, panel key, pending seeds, `enrollment_id`, CSRF token, current/new password) |
| SEC8–9 | `admin reset` uid and Docker form; migrations | `admin reset` (owner uid, Docker command, `store.Open` runs migrations) |
| SEC10 | `Origin` on pre-session POSTs | Host allowlist › Origin (when present, every unsafe request); matrix cross-site cases |
| SYS1 | Startup order | Startup order section; hub token/pki moved after the lock; `TestSecondServerFailsOnLockAndFirstStillAnswers` |
| SYS2 | Root-owned files from `admin reset` | = R8 |
| SYS3 | Pending rows growth | Auth state machine (a) (prune on insert, 5 per user); `TestPendingPrunedAndCapped` |
| SYS4 | SNI | = R1 |
| SYS5 | Leaf validity | 825 days, renew under 60 days at start with WARN; Security page warning; WebKit project |
| SYS6 | Denied-row actor ids | Audit of auth events (`anonymous:*` per action, IP in args) |
| SYS7 | Panel domain in M2 | Removed (setting, validator, `DomainCertValid`); M4 owns it; `--panel-host` kept; Out of scope |
| SYS8 | Back-off fail-closed vs admin | = SEC3 (known IPs bypass map-full) |
| SYS9 | Hash before checks; verify endpoint | Auth state machine (b) (read-only checks → argon2 → tx); `/setup/verify` dropped; `TestSetupChecksBeforeHash` |
| SYS10 | Missing or wrong master key | Secretbox (refuse with ciphertexts, clear key-id error, reset creates key, manual steps) |
| SYS11 | Migration editing | Steps preamble (never edit 0003 after merge) and step 5 lists every column |
| SYS12 | Step sizes | Steps 6 → 6a/6b, 11 → 11a/11b |
| SYS13 | Dev loop | SPA › `make dev` |
| SYS14 | Proving the SPA is embedded; bench Node stage | `serverdock version` `ui=`; release and bench assert `spa`; bench reuses the CI web artifact (no Node stage) |
| SYS15 | Redirect port | Panel TLS (`:80` port from `--https-listen`; Docker mapping caveat) |
| SYS16 | Clock skew | `setup/status` `server_time`; shown on rejected codes |
| SYS17 | Best-effort writes; audit on login | Sessions (`last_seen_at` best-effort); Audit of auth events (intent failure → 503, no session); tests |
| SYS18 | `FreeOSMemory` placement | Passwords (from the releasing goroutine, off the response path); step 4 test |
| SYS19 | Trims | `--proxy-log-level` → test-only env var; hard-coded `audit.verify` limit; `SD-Request-Id` and meta-schema check dropped; package-level-work check kept |
| A1 | Time limits | Accessibility A1; steps 12–13 states and tests |
| A2 | Live regions | Accessibility A2; steps 11b–12 |
| A3 | QR code | Accessibility A3; step 12 |
| A4 | Themes and preferences | Accessibility A4; A5 projects |
| A5 | Testing bar | Accessibility A5 (tags, projects, explicit states, manual SR pass as exit item) |
| A6 | Errors and no data loss | Accessibility A6; step 12 |
| A7 | Recovery codes | Accessibility A7; step 12 |
| A8 | Confirmation dialogs | Accessibility A8; step 13 |
| A9 | Tables | Accessibility A9; step 13 |
| A10 | Page structure | Accessibility base rules, A10; SPA shell |
| A11 | Missing wireframes, hints, token path, Back | SPA › Layout source; setup step 1 (real path + Copy, no Back), step 2 Back; the username is asked before the QR code because `setup/enroll` takes it (M2 step 6b) |
| A12 | Footer and Menu label | A2 (footer "unknown"), shell (visible "Menu" label) |

---

# Appendix A — ADR 0010: Panel TLS and authentication

*Step 1 copies this section to `docs/decisions/0010-panel-tls-and-auth.md`.*

# 0010 — Panel TLS and authentication

**Status:** proposed (plan 0003; awaiting `security` review) · **Date:** 2026-11-20

**Context.** M2 exposes the first real attack surface: the panel, its first run, sign-in, sessions and API tokens. The maintainer decided (Q2) that the panel is reached over HTTPS on the server IP with a self-signed certificate whose fingerprint the console prints, that a panel domain can be added later, and that the IP redirects to the domain once the domain has a valid certificate. The maintainer also decided (Q8) that API tokens can't run destructive, sensitive or session-only actions, and can never manage tokens, auth settings or users. The threat model ([`docs/security.md`](../security.md) Draft 2) listed gaps G2–G10 for M2. ADR 0009 already separated the hub CA from the panel.

**Decision.**

1. **Panel transport.** The embedded Caddy serves the panel on `:443` with a **self-signed ECDSA P-256 leaf of its own** at `DATA/pki/panel.pem` (0600), with a public `DATA/pki/panel.fingerprint`. It is never issued by the hub CA. Its SANs are `localhost`, `127.0.0.1`, `::1`, the host's addresses at creation and any `--panel-host` names. The TLS connection policy sets **`default_sni` and `fallback_sni` to `localhost`**, so the leaf is served for handshakes without SNI (IP literals) and for any unknown name. It is valid for 825 days and regenerated at start when fewer than 60 days remain, when missing or invalid, or when the `--panel-host` set changes; each regeneration logs a warning with the new fingerprint. The fingerprint is logged at every start and shown on Settings › Security, **never on an unauthenticated page**. `:80` answers only `308` to HTTPS, using the port of `--https-listen`. Users are never told to install a CA.
2. **Panel domain (M4).** M2 has no panel-domain setting. M4 adds the setting, ACME, the IP → domain redirect once the domain certificate is valid, HSTS and a break-glass option to keep the IP panel (G15). M2 only accepts extra names through `--panel-host`.
3. **Control plane on a unix socket.** No TCP listener. Caddy reaches it on `DATA/run/cp.sock` (0600 in a 0700 directory owned by the service user, 256 connections). `X-Forwarded-For` is trusted only on that socket and only as a single value; Caddy has no `trusted_proxies`, so it replaces client values. Loopback is trusted for nothing.
4. **Host and Origin.** Accepted `Host`: an IP literal, `localhost`, or a `--panel-host` name; otherwise 421. On every unsafe request, an `Origin` that is present must equal `https://` + `Host`; cookie-authenticated unsafe requests and WebSocket upgrades must carry it. Every API `POST` must be `application/json`.
5. **Credentials.** Each route declares its kind, and only that kind's credential is read: none for public and setup routes, the pending cookie for the 2FA route, the session cookie for session routes, and for session-or-token routes `Authorization` first (cookies then ignored) or else the session cookie. An unknown, expired or revoked session cookie counts as none and is cleared. Unsafe session requests need the `X-SD-CSRF` header (an HMAC of the session secret) and a matching `Origin`. Tokens and session ids are never accepted from URLs; session ids are only created by the server.
6. **Auth state machine.** "An admin exists" means a user with a password and a confirmed TOTP. Setup needs the console setup token (256 bits, hashed, 1 h, written to `DATA/setup-token` 0600 at every start while no admin exists; only the path is logged). Completion checks the token, the enrolment and a code from the server-generated pending seed **before** hashing the password, then in one transaction consumes the token (compare-and-delete) and writes the admin, seed and recovery codes. A correct password yields a pending-2FA token (hashed, 5 min, 5 tries, at most 5 per user), never a session. A session is minted only after the second factor, and an older session presented at login is revoked. Password or TOTP changes revoke every session and, by default, every token. Five failed step-up codes revoke the session. If an auth event's audit intent can't be written (it waits about 2 s), nothing is minted (503). **Actions that only remove access (logout, revoke session, revoke token) always run, and flag the gap (`audit: "unrecorded"`, an ERROR in the log).**
7. **Secrets and parameters.** Passwords: argon2id (19 MiB, t = 2, p = 1), one hash at a time, at most 4 waiters, 2 s wait, then a fast 503; all request checks run before hashing. TOTP: RFC 6238, SHA-1, 6 digits, ±1 step, counters never reused; seeds stored with AES-256-GCM (secretbox) under `DATA/keys/master.key` with table/row/column as AAD. The server refuses to start if the key is missing while ciphertexts exist or while `meta.secretbox_key_id` is recorded (even when no ciphertexts exist), if the key file is unreadable, or if its id differs from the recorded one; `admin reset` clears the encrypted admin data and `meta.secretbox_key_id` in one transaction and creates a key. Recovery codes: 10 × 80 bits, SHA-256, single use. Session, pending, setup and token secrets: 256 bits, SHA-256. `DATA` must be 0700; umask 0077. Startup takes the data-directory lock before writing anything under `DATA`.
8. **Login protection without lockout.** Four independent exponential back-offs, all checked before hashing: per client IP (IPv4 or IPv6 /64), per IPv6 /48, per account for passwords (one shared bucket for all non-existent names), and per account for second factors (cap 1 h). Blocked keys are never evicted; unblocked keys are evicted oldest-first; a full map of blocked keys refuses new keys, except IPs that completed a full login in the last 30 days, which also skip the per-account password back-off. Remaining risk: back-off state resets on restart; a distributed attacker can keep the hash slot busy; a password holder can delay the admin's second factor by up to 1 h; a flood can block sign-in from a new network. Failed second factors after a correct password are shown at the next sign-in. **Accepted: the shared `<unknown>` bucket lets a multi-IP attacker tell whether a user name exists** (saturate `<unknown>` from many IPs, then a probe for a name that does not exist is refused 429 while an existing name still gets 401). Single clients see identical answers and timing. The user name is not a secret, the password and TOTP still guard the account, and per-name buckets would undo the map-filling protection.
9. **API tokens.** `sd_` + 256 bits, hashed, scoped `read` < `write` < `admin`, required expiry (default 90 days, at most 365), accepted only from `Authorization: Bearer`, created only from a session with fresh 2FA, audited. The registry denies tokens every sensitive, destructive, session-only and console-only action; token, auth and user management are session-only.
10. **Recovery.** `serverdock admin reset` runs only as the uid that owns `DATA` (root against another owner is refused), takes the data-directory lock (the server must be stopped), runs pending migrations, and runs the console-only action `auth.admin_reset`, which exists only in that process's registry. It clears the password, TOTP and recovery codes, deletes sessions, revokes tokens, clears `meta.secretbox_key_id` in the same transaction as the ciphertexts, and creates a missing master key; the next server start writes a new setup token.

**Consequences.**
- The panel works on a fresh server with no domain, at the cost of a browser warning on first use. The first-run on-path risk remains for users who skip both the SSH tunnel and the fingerprint check; this is accepted, and M4 recommends re-enrolling TOTP once the domain has a valid certificate.
- Cookies are host-scoped, not port-scoped: on IP and tunnel access, another HTTPS service on the same host name could receive them. `SameSite=Strict`, `HttpOnly` and fresh 2FA limit the impact; M3a warns about services published on the panel IP; M4's domain removes the overlap.
- Login can be delayed, but not taken over, by a distributed attacker or a password holder. Existing sessions keep working, and the console can recover.
- A server running more than 60 days without a restart can end up serving an expired self-signed leaf; Settings › Security warns, and a restart renews it.
- Behind Docker port mapping, the `:80` redirect points at the in-container HTTPS port.
- Losing `master.key` makes encrypted admin data unreadable and requires `admin reset`; losing `panel.pem` changes the fingerprint.
- Development and the bench reach the control plane through the socket, and `--internal` is gone.
- Idle cost: Caddy's TLS server on `:443`. No new timers or goroutines.

## Final notes from round 2 approvals (2026-11-20)

- Step 11a: the dev loop serves Vite over HTTPS (reviewer); a POST through the dev proxy must succeed.
- `skipDataDirModeCheck` is dropped; the M1 cross-uid test runs unchanged (reviewer).
- Bench S3/S4 bursts must not follow redirects (curl without `-L`), so S4 measures only the agent's Caddy (systems).
- Mention `SERVERDOCK_TEST_PROXY_LOG_LEVEL` in CONTRIBUTING (reviewer, optional).
- Steps 12–13 (accessibility): the 2FA expiry warning uses the status region and never steals focus; after a hidden-page pause the footer shows "unknown" until the first fresh poll.
- For plans 0004/0006 (security): once apps share `:443`, the panel's `fallback_sni` must not serve the panel leaf for app SNI names; test it. M4 re-checks the "IP literal accepted" Host rule with the redirect and G15.
- For the passkey work (plan 0006, M4 and Phase 2): a passkey-only sign-in needs its own counterpart to (a) the "admin exists" definition (today: a password and a confirmed TOTP; a user with only a passkey would not count) and (b) the login back-offs (a passkey assertion is not a password or a TOTP code: it needs its own per-IP and per-account failure accounting and the same fail-closed rules), and the `<unknown>` bucket rule applies to unknown credential ids too (reviewer, M2 step 6a/6b review).
- For plan 0005 (M3b, app secrets): once app-secret ciphertext columns exist, `admin reset` must **keep `meta.secretbox_key_id` while uncleared ciphertexts remain** (it clears only the admin columns it owns), or verify the key against one of those ciphertexts before it adopts or re-records the key. Otherwise a reset could bless a wrong key for app secrets (reviewer, M2 step 7 review).
- For step 14 (idle-memory check, systems): on Linux (CI or a VM, not Docker Desktop), after the last login sample RssAnon every 5 s for 5 min, and at +60 s read memstats from the debug socket (HeapInuse, HeapIdle minus HeapReleased, NumGC). Flat, zig-zag or a growth that stops in HeapIdle minus HeapReleased: record it and close. Steady growth, or a rising HeapInuse: take a heap profile and add a Background work row. (The local +60 s reading after the third login was +2 MB anon with no activity in between; it needs this check before it is called noise.)
- `TestSecretPathsCoverEverySecretFile` (steps 1, 5 and 10 name it) is implemented as the subtest `SecretPathsCoverEverySecretFile` of `TestSecretsNeverLeak` (`cmd/serverdock/sentinel_test.go`), because only the full flows leave a realistic DATA tree to walk; it fails when a file outside `datadir.SecretPaths()` holds any sentinel (reviewer, M2 step 10 review).
- Step 11a/11b implementation notes: SvelteKit 3 reads its configuration from `vite.config.ts` and has no `$lib` alias, so imports are relative and the generated API types live in `web/src/lib/api-types.ts` (not `lib/api/types.ts`, which would shadow `api.ts`). TypeScript is 6.x with an explicit `overrides` entry for `openapi-typescript`. SvelteKit's route announcer has an inline `style` attribute that the CSP blocks, so a build plugin swaps it for a class and fails the build if SvelteKit's markup changes. The 11b Settings link opens a placeholder `/settings/security` page until step 13, and "Sign in again" links to `/login` until step 12 (a not-found page shows meanwhile).

### Step 12 manual screen-reader pass (checklist for the maintainer)

The automated suite cannot hear anything. Before step 12 merges, with VoiceOver on iOS Safari and NVDA on Windows (or VoiceOver on macOS Safari), on a fresh `serverdock server` run through the real flow, record pass or fail and notes in the PR:

1. **Setup step 1:** the stepper reads "Step 1 of 2" as the current step; the token path and the Copy button are reachable; "Command copied." is announced once; the expiry sentence is read with the field's hint.
2. **Errors:** submit empty, with a wrong token and with a short password. The error summary is announced once. **Check for a double announcement** (the summary is a `role="alert"` and also takes focus), and note whether the field error and the summary read as one message or two. Following a summary link lands on the field.
3. **Step 2:** the rule is read before the field; Show/Hide password announces its pressed state; the setup key is read before the QR code, grouped in fours; the QR code is announced as "QR code for your authenticator app" and never reads the secret; "Open in authenticator app on this device" works on the phone.
4. **Recovery codes:** "Recovery codes" is announced as a list of 10; Continue is announced as dimmed with its reason; leaving the page before ticking the box asks first.
5. **Route changes:** after each page change the new heading is read once. **Check for a double announcement** from the heading taking focus and SvelteKit's route announcer (`#svelte-announcer`, which says the new page title); if it is noisy, record it so the announcer can be turned off for pages that move focus.
6. **Sign in and two-factor:** the busy state ("Signing in…") is announced and the button stays on focus; the "About a minute left" status is announced without moving focus (leave the step open for 4 minutes); the expiry message and "Sign in again" link; the back-off countdown (needs a rate-limited account) is announced at the start and at coarse steps only.
7. **Footer:** it is not announced while idle.
8. **Overview banners:** after a sign-in that had failed codes or an unwritten audit record, the banners are announced on load (the audit warning as an alert, the notice as a status). Each Dismiss button has its own name ("Dismiss audit warning", "Dismiss sign-in notice") and focus lands on the Overview heading afterwards.
9. **Recovery-code toggle (sign-in):** switching between the authenticator and a recovery code moves focus to the new field and announces the change once.
10. **Two-factor expiry:** after the step runs out, focus lands on the "This step expired" alert, which is read, and "Sign in again" is reachable next.
11. **Wrong password:** the message is read and linked to the password field, the password field is empty, the username is kept.
12. **Back-off countdown in browse mode:** the visible "Try again in M:SS" is readable with the virtual cursor (it is not hidden), and activating the dimmed Sign in button says how long is left.
13. **Key and recovery-code groups:** the setup key and the `sudo cat` command are announced as labelled groups ("Setup key for admin", "Read it on the server with:") and read as text.
14. **Zoom and text size:** at 200% browser zoom and the largest system text size nothing is clipped on either screen set.

### Step 13 manual screen-reader pass (checklist for the maintainer)

With VoiceOver on iOS Safari and NVDA on Windows (or VoiceOver on macOS Safari), on Settings › Security of a running server; record pass or fail and notes in the PR:

1. **Page structure:** the "Settings" sub-navigation and "Main" navigation are both announced; the Security link is "current page"; the headings (This server, Sessions, API tokens, Password, Authenticator, Recovery codes) can be jumped to.
2. **Tables (wide screen):** the session and token tables are announced with their caption, column headers and row headers (device, name); each row action says what it acts on ("Sign out session from 203.0.113.9, Firefox", "Revoke token backup script (abcd1234)"). Below 768 px the cards read as lists with labelled values.
3. **States:** each state is read as a word ("Active", "Expired", "Revoked", "This device"); the symbols are not read.
4. **Dialogs:** each confirmation is announced with its title and body (target and consequence); focus starts on Cancel; Escape closes and focus returns to the button that opened it; the page behind cannot be reached while the dialog is open; after a row is removed focus lands on the next row's action.
5. **Step-up:** the "Confirm it is you" dialog announces; a wrong code is read as an alert and focus returns to the code field; Cancel returns focus to the opener.
6. **New token:** "Your new token" takes focus and says it is shown once; Copy is announced; after hiding it the token is gone from the page (browse mode).
7. **Recovery codes:** the same checks as setup, including the guard against leaving and the disabled Continue with its reason.
8. **Signed out:** after changing the password or the authenticator the Signed-out page reads "password or authenticator changed".
9. **Certificate warning:** with fewer than 60 days left (the maintainer can test by editing the leaf's expiry) the warning is announced as a status and is not only red.
10. **Zoom and text size:** at 200% zoom and the largest text size the tables become cards and nothing is clipped.
