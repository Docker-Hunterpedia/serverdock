# Plan 0002 — Phase 1: MVP "Solo on one server" (phase plan + milestone 1 steps)

**Status:** APPROVED (reviewer round 2, security round 4, systems round 4 conditional on update.json states — applied). Pre-M1 answers recorded 2026-11-20 (see Part D); M1 approved to start. **Revision 1 (2026-11-27), the milestone reorder, needs `reviewer` approval of its text**; the maintainer approved the decisions behind it on 2026-11-27. · **Author:** planner (AI) · **Date:** 2026-11-20, revised 2026-11-27

**Roadmap:** Phase 1 — MVP: "Solo on one server" (all 23 items) and the Phase 1 exit check. This plan is AI-written and not yet confirmed by the maintainer.

## Revision 1 (2026-11-27): milestone reorder for an early usable version

Maintainer decisions of 2026-11-27 (approved). This revision applies them throughout the plan; the "Revision 1 changes" table at the end lists every place.

1. **New order:** M1, M2, M3a (in progress, PR #6), M3b, M4a, M7a, which together are the **early usable version**; then M7b, M5, M6, M4b, M8, M9.
2. **M4 is split.** **M4a** = domains, automatic HTTPS, per-route validation, ACME, diagnostics, the panel domain (HSTS G15, TOTP re-enrol G5) and IPv6 (plan 0006). **M4b** = passkeys, everything in the old M4 passkeys bullet (library, ADR, threat-model update, tests), still in Phase 1, after M6 (new **plan 0013**, the next free number).
3. **Early use** (from the M7a exit): only apps whose data is disposable or backed up elsewhere, until M6 adds app-data backups (confirmed 2026-11-27; originally worded "until M7b"). *Systems review (round 3) narrows what M7b lifts: M7b replaces the "copy `DATA` before each reinstall" rule, but app-data backups arrive only in M6, so the plan keeps the app-data rule until M6. The maintainer confirmed this on 2026-11-27: the app-data rule stays until M6.*
4. **Two accepted risks** (maintainer, 2026-11-27), recorded in `docs/security.md` "Accepted risks (Phase 1)": a public panel domain with phishable password + TOTP sign-in from M4a/M7a until M4b; and no out-of-band notice of sign-ins or brute-force back-off in Phase 1. Mitigations: the panel domain is optional; the early-use guide recommends IP access over an SSH tunnel or a firewall allowlist on 443; G3 back-off; fresh 2FA for dangerous actions.

**Why reorder.** Real use starts sooner: M3a + M3b + M4a + M7a is enough to install ServerDock with one command and run image-based apps on a domain with HTTPS, and real use finds problems that tests miss. **The Phase 1 exit date does not get earlier**: the work is the same, and the counted dogfood window still sits at the end.

**Notes on numbering.** Plan numbers keep their old assignments (0005 M3b, 0006 M4a, 0007 M5, 0008 M6, 0009 M7a, 0010 M7b, 0011 M8, 0012 M9) so existing links stay valid; M4b gets 0013. Plan numbers no longer follow the run order; the Overview table is the run order. Milestone sections below stay in milestone-number order. In other docs written before this revision (plan 0003, ADR 0010, `docs/security.md`, `docs/features.md`), "M4" means **M4a** for domains, the panel domain, HSTS and G15, and **M4b** (plan 0013, not 0006) for passkeys.

**How to read this plan**

- **Part A** is the phase plan: 12 milestones, each a shippable vertical slice. Milestones M2–M9 each get their own detailed step plan (0003–0013; see the Overview for which is which), approved by `reviewer` before implementation starts. A line of the form "plan 00NN must …" is a binding requirement for that later plan.
- **Part B** is milestone 1, written out step by step like plan 0001. Once this plan is approved, M1 can be implemented without a separate plan.
- **Part C** maps every roadmap item to a milestone and every exit criterion to its evidence.
- **Part D** lists the open questions for the maintainer and the decisions that need advisor input.
- **The four "Review round N changes" tables** at the end map each review item to where it was fixed; **"Revision 1 changes"** maps the 2026-11-27 reorder.

## Goal

A solo developer can replace Coolify or Dokploy with ServerDock on one 1 GB VPS. That means installing in one command, setting up the admin with 2FA, deploying from an image, a Dockerfile in git, or Compose, getting automatic HTTPS, running databases with S3 backups, seeing metrics, logs and alerts, and updating ServerDock safely. Idle use must stay at or below 80 MB RAM and under 1% CPU, measured on a real 1 GB VPS.

## Out of scope (whole phase)

- **Everything in Phase 2 and later.** That includes remote-agent enrollment and mTLS (security open item #3), the slim agent build (unless it is pulled forward; see Q3), builder servers, Railpack, other git providers, PR previews, environments, cron jobs for apps, the web terminal, templates, MCP write actions, multi-user, volume backups, and DB major-version upgrades.
- **Write operations on remote agents.** Until Phase 2 enrollment, remote agents are read-only (logs; bench). See Phase-wide rule 9.
- **A general CLI.** Phase 1 ships only these subcommands: `server`, `agent` (experimental, read-only; see Q1), `version`, `start/stop/status`, `uninstall`, `doctor --bundle`, `bench`, `audit verify`, and two console-only recovery commands, recorded as exceptions: **`admin reset`** (M2) and **`state restore`** (M7b).
- **Built-in AI** (Phase 3).
- **More notification channels.** Phase 1 has only email, webhook and Telegram.
- **Servers screen.** In Phase 1 it shows only the local server. "Add server" is labelled Phase 2.
- **Translations.** The UI is English only.

## Phase-wide rules

1. **Nothing is exposed before M1 lands.**
   - M1 closes security open items #1, #2, #4, #5 and #6. No new HTTP route ships before M1.
   - **Auth is not exposed (M2) until `security` has reviewed the threat model**, a Phase 0 leftover that M1 step 2 schedules.
   - From M2 on, every route declares its auth requirement in code, including the debug listener's routes. A test fails on any route without that declaration; this is the start of the RBAC route-matrix test.
2. **Footprint gates (ADR 0008) stay green on every PR.**
   - The gates are S2 single node ≤ 80 MB with < 1% CPU, and S4 remote node ≤ 50 MB with < 0.5% CPU, plus the existing tolerances.
   - Each milestone also has an allocation (table below). **Slack: S2 may go up to 2 MB over its allocation and S4 up to 0.5 MB.** Anything beyond that needs a written reason and `footprint` review before merge.
   - Allocations are a review rule; only the ADR 0008 gates fail CI.
3. **Background-work inventory.** `docs/architecture.md` gets a "Background work" table that lists every goroutine that wakes up on a timer, with its interval and why it exists. Any PR that adds a timer adds a row, and `footprint` checks the table.
   - No fixed-interval polling below 15 s.
   - **Written exception (plan 0004 step 6, replaced in step 6b by Revision 11; `footprint` signed): Caddy's upstream-health metrics ticker (10 s).** Caddy starts one `metricsUpstreamsHealthyUpdater` goroutine per `reverse_proxy` handler, unconditionally; it is not configurable and only sets an in-memory gauge (the panel's own has run since M1). ServerDock adds **exactly one extra 10 s Caddy metrics ticker once at least one public port exists, not rebuilt on a switch** (it is torn down and recreated only by a reload, that is a port-set change; with no public port there is no app server and no app ticker). All app ports share one Caddy server and one `reverse_proxy` handler, so the count does not grow with the number of services. `TestOneTickerForAllAppRoutes` counts it.
   - **All scheduled work (jobs due later, cron schedules, daily housekeeping) shares ONE "next due" timer in the job scheduler** (introduced in M3a; the daily housekeeping job added in M7b; cron schedules added in M6). Daily tasks are recurring jobs on it, not separate timers.

   | What | Where | Interval | When it runs | Milestone |
   |---|---|---|---|---|
   | yamux keepalive (hub side) | `internal/hub/hub.go` `Attach` | 30 s | per **remote** agent session (off on the local pipe) | Phase 0 |
   | yamux keepalive (agent side) | `internal/agent/session.go` `ServeSession` | 30 s | per remote session, in the agent process | Phase 0 |
   | Reconnect backoff timer | `internal/agent/client.go` `sleepCtx` (line 77) | 1 s → 30 s, jitter | only while a remote agent is disconnected | Phase 0 |
   | Hub cert renewal back-off | `internal/pki` | ≥ 1 min | only after a failed renewal; checked lazily on handshake, no goroutine | M1 |
   | Job scheduler "next due" timer | `internal/jobs` | next due time | one timer for all scheduled work | M3a |
   | Docker events reconnect backoff | `internal/deploy` (`Reconciler.Run`; built in the control plane, not in `dockerapi`) | 1 s → 30 s, jittered | only while the events stream is down | M3a |
   | Caddy/certmagic maintenance (`(*certmagic.Cache).maintainAssets`) and events | embedded Caddy | 10 min renew check, 1 h OCSP check (certmagic defaults) | whenever the proxy runs, in **server and agent mode** (`caddyhttp` provisions the `tls` app); certificate expiry alerts use these, not our own timer | running since M1 (S2 and S4); confirmed by a goroutine dump in M2 step 3 |
   | BuildKit idle stop | `internal/build` | 5 min | only while the BuildKit container runs | M5 |
   | Webhook replay-cache expiry | `internal/api` | none (lazy on insert) | no timer | M5 |
   | Daily housekeeping job (update check, scheduled state backup and its local pruning; later backup retention, cleanup, metrics retention pruning, image digest check) | on the scheduler timer | daily | one recurring job | M7b (job, update check, state backup), tasks added in M6/M8 |
   | Metrics collector | `internal/agent/metrics` | 15 s | always (the first always-on periodic work) | M8 |

   Metrics rollups run inside the collector tick (every 40th tick), not on their own timer.
4. **Dependency rule.** Each new direct dependency needs a row in its milestone plan with:
   - gzip binary delta;
   - S4 RSS delta measured with `QUICK=1`. Package `init()` work runs in **every** mode, including the remote agent. **A QUICK S4 delta above 0.5 MB also needs a full, non-QUICK S4 run before merge**;
   - why the standard library or an already-linked module is not enough.

   Test-only and CI-only tools are listed but do not count.
5. **Honest state.** Every long operation is a Job with a visible state (`queued`, `running`, `succeeded`, `failed`, `cancelled`, `unknown`). The UI and CLI never report success before the work has finished, or when its audit outcome could not be recorded.
6. **UI bar for every UI milestone.** Playwright e2e tests on desktop and on a 390 px phone viewport, with axe (no serious or critical violations). Touch targets are 44 px. States use a word plus a symbol, never colour alone. Every empty state has a hint and a docs link.
7. **Secrets.**
   - Secrets never appear in logs, errors, URLs, audit rows (arguments and the `error` column) or MCP output. Tests check this by searching DB files, logs and bundles for planted sentinel values.
   - Agent output is untrusted: op-protocol lines and progress messages are size-capped.
8. **ADRs.** Lasting decisions get an ADR in the milestone that makes them:
   - 0009 agent channel (M1);
   - 0010 panel access (M2);
   - 0012 Docker client (M3a);
   - 0013 builds (M5);
   - 0014 Compose (M6);
   - 0015 backups (M6);
   - 0016 install shape (M7a);
   - 0017 release signing (M7a);
   - 0018 self-update handover (M7b);
   - passkeys (M4b, Revision 1): the next free number when it is written (0019 if no other ADR comes first).

   ADR 0011 is the footprint ceiling. Since Revision 1 the ADRs are written in run order, so 0016/0017 (M7a) and 0018 (M7b) come before 0013 (M5) and 0014/0015 (M6); the numbers stay as assigned.
9. **Remote agents are read-only until Phase 2.**
   - Open item #3 (no per-agent identity) stays open through Phase 1, so anyone with the shared token could claim an agent id.
   - Every agent op is declared `LocalOnly` unless it is a read with no secrets. **The hub refuses to send a `LocalOnly` op to any session other than the in-process local agent** (`hub.LocalID`, reachable only through the in-memory pipe).
   - All write and secret-bearing ops (deploys, env, backups) are `LocalOnly`. Remote agents get only `hello` (the read-only version handshake, no secrets, result at most 256 bytes) and `logs` (bench and spike use). The remote allowlist is `{hello, logs}`; the negotiated protocol version never widens it, and `serverdock agent` (remote mode) builds its handler table from `RemoteOK` ops only (ADR 0012).
10. **One outbound-URL policy** (`internal/netpolicy`, first used in M5; then S3 in M6 and notifications in M8). Any user-supplied URL ServerDock fetches must meet these rules:
    - **https only**, unless the admin explicitly opts in to a local http endpoint.
    - **Blocked by default:**
      - loopback;
      - link-local;
      - cloud metadata addresses;
      - **every IP on the host's own interfaces, including its public IP**;
      - **every Docker network gateway** (`docker0`, `br-*`);
      - **RFC 1918, CGNAT (100.64/10) and ULA (fc00::/7)**.
    - **Per-target admin allowlist.** An admin can allow a private address for one specific target (for example a LAN MinIO or SMTP server). The allowlist is audited.
    - **Enforced in a custom `DialContext`** that checks the IP actually being dialed. This makes the check hold against DNS rebinding and on every redirect, rather than relying on a lookup done earlier.
11. **CI never uses `pull_request_target`.** Values from `needs.*` and `github.event.*` reach shell steps only through `env:`, never inline `${{ }}` inside `run:`.

---

# Part A — Milestones

## Overview

Rows are in run order (Revision 1, 2026-11-27).

| Run | # | Milestone | Shippable slice | Detailed plan |
|---|---|---|---|---|
| 1 | M1 | Hardening and core plumbing | Security open items closed, CI bench fix, Phase 0 carry-over, migrations, audit log, action registry, local-only agent ops. No new user features. | **This plan, Part B** |
| 2 | M2 | Auth, first run, SPA shell | HTTPS panel, setup token → admin + TOTP → login; `admin reset`; sessions, API tokens, REST + OpenAPI from the registry, real SvelteKit SPA | 0003 |
| 3 | M3a | Deploy from image, safely (in progress, PR #6) | Jobs + scheduler, image deploys, blue-green and stop-first, health gate, automatic and one-click rollback, auto-cancel superseded, restart/redeploy, basic UI | 0004 |
| 4 | M3b | Service configuration | Env vars and secrets, registry credentials, volumes, project networks, CPU/RAM limits, context variables, Docker version warning, Docker RC CI matrix | 0005 |
| 5 | M4a | Domains and automatic HTTPS | Caddy config from desired state, per-route validation, ACME, TLS kept separate from app health, diagnostics, panel domain (HSTS G15, TOTP re-enrol G5), IPv6 | 0006 |
| 6 | M7a | Install, lifecycle, signed releases | Install script + pre-flight, **re-run upgrades in place**, hardened systemd unit, `start/stop/status/uninstall/doctor --bundle`, signed releases, `serverdock bench`, **first VPS bench**. **Exit = early usable version** (disposable or externally backed-up apps only, until M6) | 0009 |
| 7 | M7b | State backup and self-update | Daily housekeeping job, encrypted state backup (local only until M6) → self-update (trial boot + rollback) → rehearse restore → `state restore`. Replaces the "copy `DATA` before each reinstall" rule; the app-data rule stays until M6. | 0010 |
| 8 | M5 | Git builds and push-to-deploy | Dockerfile builds on capped BuildKit, GitHub App push-to-deploy + commit status, generic webhook, live build progress, outbound-URL policy | 0007 |
| 9 | M6 | Compose, databases and backups | Compose (compose-go, policy-checked), one-click Postgres/MySQL/Redis, cron scheduling, encrypted S3 backups (also a state-archive target), restore with approval, **VPS bench re-run** | 0008 |
| 10 | M4b | Passkeys | WebAuthn sign-in on the panel domain, TOTP and recovery codes as fallback, passkeys ADR, threat-model update | **0013** |
| 11 | M8 | Observability and housekeeping | Metrics, live logs, log search, alerts, notifications, disk cleanup + alerts, "update available" | 0011 |
| 12 | M9 | API, MCP, docs and Phase 1 exit | OpenAPI complete, read-only MCP, docs site, full reviews, final VPS bench, 5-minute test, dogfood report (counted window: last 14 days before exit) | 0012 |

**Why this order (Revision 1).** The reorder gets ServerDock into real use sooner; it does not make the Phase 1 exit earlier.
- M1–M2 are the foundations: safe exposure, a schema, the registry and audit, and auth.
- M3a, M3b and M4a are the smallest set that runs a real app: image deploys with rollback, configuration and volumes, and a domain with HTTPS. An early version without HTTPS on a domain would not be usable.
- M7a comes next so the early version is a real signed install on a real VPS, and the first VPS bench runs early. Re-running `install.sh` is the update path until M7b.
- M7b follows at once, so the reinstall-to-update period is short and ServerDock's own state is backed up before more of it builds up. Its quiesce check is generic, so builds and backups added later are covered.
- M5 still comes after M4a because the GitHub App needs a public HTTPS panel. M6 follows M5 because S3 uses the outbound-URL policy that M5 introduces.
- M4b (passkeys) comes after M6: first use doesn't need it (TOTP and recovery codes cover sign-in), and it only depends on M4a's panel domain.
- M8 adds the first always-on periodic work (the metrics collector), so it comes late and the milestones before it are measured without it. Its certificate-expiry alerts need M4a, and its backup alerts need M6.
- The counted dogfood window needs builds with M5, M6 and M8, so it runs during M9, and the exit can't come before it ends.

## Footprint allocation (cumulative S2 / S4 VmRSS targets; cloud-container base until the CI re-base)

> Since [ADR 0011](../decisions/0011-footprint-ceiling.md) (2026-11-21) these allocations are **tripwire planning numbers**, not gates: CI fails only above 200 MB (S2) / 100 MB (S4). Crossing an allocation needs a written reason in the PR.

Baseline from [research/05](../research/05-phase0-measurements.md): **S2 47.6 MB, S4 44.1 MB**, binary 18.8 MB gzip. Slack per rule 2: S2 +2 MB, S4 +0.5 MB.

Revision 1 (2026-11-27): rows are in the new run order. The S2 column keeps each milestone's old increment; the old M4 increment (+4) is split into M4a +3 and M4b +1, so the M9 total is unchanged at 75 MB. The S4 column is **re-based on M3a's measurement**, because the old S4 targets were passed in M2 (46.0 MB, see ADR 0011) and M3a.

**Provisional M3a base.** The M3a figures in the table (S2 about 54–55, S4 47.6, VmHWM 49.8) come from the **cloud container** bench (a temporary cgroup-v1 shim), **not from CI amd64** as this section's heading says. The CI S2/S4 medians for M3a are **not yet measured**: the bench job is skipped on draft PRs. The tripwire comparison is owned by the CI bench median (the gate metric). **The table is re-based once, on the first CI numbers from PR #6**, and the S4 targets after M3a shift by the difference.

| Run | After | S2 target (increment) | S4 target (increment) | Main new idle cost |
|---|---|---|---|---|
| 1 | M1 | ≤ 49.0 (+1.4) — measured 47.6 | ≤ 45.0 (+0.9) — measured 44.2 | TLS listener, migration runner (startup only) |
| 2 | M2 | ≤ 53 (+4) | ≤ 45 — measured 46.0 | Real SPA (file-backed), auth, sessions |
| 3 | M3a | ≤ 56 (+3) — about 54–55 (cloud container, full run, live watcher; CI not yet measured) | **47.6 (cloud container, full run, median of per-run medians)**, the provisional S4 base until the first CI run on PR #6; VmHWM 49.8 (watch figure only) | Docker events stream, job scheduler, reconciler |
| 4 | M3b | ≤ 58 (+2) | ≤ 47.8 (+0.2) | Secretbox in use, more state |
| 5 | M4a | ≤ 61 (+3) | ≤ 48.0 (+0.2) | ACME issuance and domain routes (certmagic maintenance already runs since M1) |
| 6 | M7a | ≤ 61.5 (+0.5) | ≤ 48.0 (+0) | Lifecycle commands, `sd_notify` (no idle cost) |
| 7 | M7b | ≤ 62 (+0.5) | ≤ 48.0 (+0) | Daily housekeeping job with update check and state backup (on the shared timer) |
| 8 | M5 | ≤ 64 (+2) | ≤ 48.0 (+0) | Webhook handler only; BuildKit runs only during builds |
| 9 | M6 | ≤ 68 (+4) | ≤ 48.5 (+0.5) | compose-go init, cron, S3 client |
| 10 | M4b | ≤ 69 (+1) | ≤ 48.5 (+0, required) | WebAuthn library (control plane only) |
| 11 | M8 | ≤ 74 (+5) | ≤ 49.5 (+1) | 15 s metrics collector, alert evaluation |
| 12 | M9 | ≤ 75 (+1) | ≤ 49.5 (+0) | MCP handler (no idle cost) |

Check: S2 increments after M3a are 2 + 3 + 0.5 + 0.5 + 2 + 4 + 1 + 5 + 1 = 19, and 56 + 19 = 75 (tripwire 80). S4 increments after M3a are 0.2 + 0.2 + 0.5 + 1 = 1.9, and 47.6 + 1.9 = 49.5 (tripwire 50).

**Which number is compared.** The tripwires and these targets are checked against the **CI** bench gate metric: the **median of the per-run medians** of idle VmRSS (`bench/run.sh` line 24, `bench/report`). The full-run VmHWM (high-water mark) is reported as a **watch figure only**: it is not compared with the tripwire (plan 0004 Revision 17).

**How a step is judged.** A milestone's footprint step is judged by its **full-run median plus an explained cause**, not by a single delta. The +0.2 MB S4 steps (M3b, M4a) are inside the full-run spread of about 0.9 MB, so one run can't show them; a target overrun is accepted when the full-run median and the cause (new code, a dependency, binary growth) explain it.

**S4 headroom is the tightest constraint in Phase 1.** On the provisional M3a base the S4 median is 47.6 MB, about **2.4 MB under the 50 MB tripwire** (ADR 0011; the hard ceiling is 100 MB). The planned targets leave **0.5 MB at M9** (49.5). Control-plane dependencies raise S4 through `init()` work and through mapped pages of a larger binary, even though agent mode never calls them. So:
- every S4 increase uses up headroom that the slack rule (S4 +0.5 MB) barely covers. A milestone whose S4 median lands above its target needs the written reason and `footprint` review; one whose median crosses the 50 MB tripwire needs the ADR 0011 written reason, and a crossing without a convincing reason triggers Q3 (pull the slim agent forward);
- the S4 VmHWM (49.8 MB at M3a) is recorded in each bench report so a rising trend is visible, but it is not a gate or a tripwire. **It will likely pass 50 MB from M3b on; Q3 triggers only on the median**;
- M4a, M5, M7a and M7b should add close to nothing to S4: none of them adds a heavy dependency, and certmagic already runs in agent mode. If any of them shows a QUICK S4 delta above 0.5 MB, rule 4's full S4 run applies (named for M4a in its section);
- M6 (compose-go, the S3 client) and M4b (WebAuthn library) need the full, non-QUICK S4 run of rule 4 before merge, whatever the QUICK delta; M4b must leave S4 unchanged;
- **the planned S4 increases are M6 (+0.5: compose-go and S3 client `init()` and binary growth) and M8 (+1: the metrics collector also runs in agent mode)**. M8 also needs a full S4 run.
- **Remote nodes with domains cost more than the empty S4 scenario** (certificates, routes, ACME state). S4 measures an agent with no domains; the cut-down loaded run in the M7a VPS bench shows the domain cost on S2, and the docs must not quote empty S4 as the cost of a remote node that serves domains.

**Report-only "S2-loaded" scenario (added in M8):** 3 image services, 1 Postgres, 2 domains (internal issuer). A cut-down version (image services with 1–2 domains, internal issuer, no Postgres) runs earlier, in the M7a VPS bench.

---

## M1 — Hardening and core plumbing

Full step plan in **Part B**.

- **Goal:** close the Phase 1 security open items before anything is exposed; fix the CI bench trigger; carry over the Phase 0 boxes; add the migration runner (with a read-only open path), the hash-chained audit log, the typed action registry, and local-only agent ops.
- **Roadmap items:** Action registry (typed ops, risk tiers, audit log); part of "Docker API version negotiation + supported Docker range"; foundations for REST, MCP and auth.
- **Packages:** `.github/workflows/ci.yml`, `.github/scripts/`, `internal/hub`, `internal/agent`, `internal/agent/dockerapi`, `internal/agent/proxy`, `cmd/serverdock`, new `internal/pki`, `internal/limits`, `internal/actions`, `internal/audit`, `internal/redact`, `internal/store`, `bench/run.sh`, `docs/roadmap.md`.
- **ADRs:** 0009.
- **Footprint:** S2 +≤ 1.4 MB (≤ 49.0), S4 +≤ 0.9 MB (≤ 45.0); no new timers or goroutines at idle; binary +< 0.5 MB.
- **Security:**
  - resolves open items #1, #2, #4, #5, #6;
  - enforces read-only remote agents (rule 9);
  - removes `--debug-allow-remote`;
  - the registry denies by default and audits;
  - schedules the threat-model review.
- **Reviewers:** `reviewer`, `security`, `systems`, `footprint`.
- **Status: M1 DONE (2026-11-20), pending the CI bench numbers and merge.** Commits on `phase-1-m1`:
  - step 1 CI: `69363c7`; step 2 Phase 0 carry-over: `188c9df`; step 3 goleak: `ad9e8af`, `bcebd8c`;
  - step 4 Docker deadlines and negotiation: `478e4d0`, review fixes `c013c68`;
  - step 5 agent TLS listener, hub CA (ADR 0009): `3be6009`, review fixes `e449a0e`, `97983a4`;
  - step 6 limits: `ef6ad27`, stream cap `02a1de6`;
  - step 7 migrations and read-only open: `83e8836`; step 8 audit log: `487e6f3`; step 9 action registry: `b72f508`;
  - security and review fixes: `55b4807` (G1, G12, G3 part), `5b02b20` (nested redaction), `4c7b823` (backups, locks, denied rows), `04870af` (self-marshalling secrets), `5a6ebc3` (private lock file);
  - step 10 record and close: this commit (the `v2` fixture, `docs/security.md`, `docs/architecture.md`, `docs/roadmap.md`, Dependabot).
  - Deviations from this plan are listed in the commit messages and in `docs/security.md` "M1 close-out status": the yamux window stays at 256 KiB (streams capped instead), the `update.json` backup skip moved to M7b, and `Op.Validate` relies on the op table.
  - CI bench numbers (S2, S4): [run 44749284417](https://github.com/Docker-Hunterpedia/serverdock/actions/runs/44749284417) at 0ad2602, amd64: S2 **47.6 MB** (allocation ≤ 49.0), S4 **44.2 MB** (≤ 45.0), idle CPU ≤ 0.01%, binary 18.9 MB gzip. All gates and `bench-gate` passed.
- **Exit evidence:**
  - open items #1/#2/#4/#5/#6 resolved in `docs/security.md`, with links to the tests;
  - CI bench green (S2, S4);
  - a PR showing a docs-only push skipping the bench while `bench-gate` stays green;
  - `audit verify` passes as a different uid on a live data dir and fails on a tampered one;
  - Phase 0 boxes closed or carried over.

## M2 — Auth, first run, SPA shell

- **Status (2026-11-21):** built on `phase-1-m2` (PR #3); the detailed plan is [plan 0003](0003-m2-auth-first-run-spa.md), steps 1–14 done, with its [status table](0003-m2-auth-first-run-spa.md#status-m2-close-out-2026-11-21). Open: the maintainer's manual screen-reader passes (steps 12 and 13), the CI bench numbers and the PR merge.
- **Entry gate:** `security` has reviewed the threat model (carried over by M1 step 2).
- **Goal:**
  - the panel is reachable over HTTPS;
  - the first run is protected by the console setup token;
  - the admin signs in with password + TOTP and can recover from the console;
  - the real SPA shell exists: layout, sidebar with ServerDock's own RAM/CPU, onboarding steps 1–2, login, security settings.
- **Roadmap items:** setup token, admin + TOTP, first-run onboarding (UI); token expiry; REST skeleton (OpenAPI from the registry); accessible, phone-usable UI (baseline).
- **Packages:** `internal/auth`, `internal/secretbox` (new), `internal/actions` (JSON Schema reflector), `internal/api` (authz, REST, OpenAPI), `internal/agent/proxy` (`:443`; `:80` redirect only), `cmd/serverdock` (`admin reset`), `web/`, `bench/Dockerfile` (Node stage), CI (web build, e2e).
- **Key decisions:**
  - **Panel TLS (Q2, ADR 0010).** Recommended: HTTPS on the server IP with a **self-signed ECDSA leaf of its own; it is never issued by the hub CA**.
    - The console prints the **leaf's SHA-256 fingerprint**.
    - Onboarding tells the user to compare it with the browser's certificate details before entering the setup token.
    - Docs never tell users to install a CA.
    - Until M4a, `:80` only redirects to HTTPS.
  - **Setup token.**
    - ≥ 128 bits from `crypto/rand`, single-use, stored hashed, compared in constant time, expires after 1 h, regenerated on restart while no admin exists.
    - Sent only in the POST body, never in a URL query.
    - **Written to `DATA/setup-token` (0600, removed once used).** The console and journal print only that file's path and the leaf fingerprint, because the journal is readable by the `adm`/`systemd-journal` groups.
    - The setup endpoint returns 404 once an admin exists.
  - **Passwords.**
    - argon2id from `golang.org/x/crypto`: m = 19 MiB, t = 2, p = 1.
    - One hash runs at a time, behind a semaphore with a **bounded wait (e.g. 2 s) that returns a fast 503**.
    - **The per-IP limit is checked before hashing.**
  - **TOTP.**
    - RFC 6238 in-repo, accepting **±1 step**.
    - **The last accepted counter is stored and codes are never reused.**
    - 10 single-use recovery codes, stored hashed.
    - The QR code dependency is chosen in plan 0003.
  - **Login protection: no hard lockout.**
    - Per-IP rate limits with exponential back-off, with **IPv6 keyed by /64**, on login, TOTP, setup and recovery codes.
    - Plus **a per-account TOTP-failure slowdown across all IPs** (e.g. 10/min). Crossing it notifies the admin (shown in the panel; also sent through notifications once M8 adds them).
    - Client IP from `X-Forwarded-For` only when the request comes from loopback. Plan 0003 must test that our Caddy **replaces** a client-supplied XFF instead of appending to it.
  - **`serverdock admin reset`.**
    - Console-only. Takes the data-dir flock, so it refuses while the server runs.
    - Clears the password and TOTP, revokes all sessions and API tokens, and writes an audit row through the system invoker. **It does not write a setup token itself: the next server start writes it** (plan 0003, M2 step 7: no admin exists, so setup reopens only with that new token). It runs only as the uid that owns `DATA` (root against another owner is refused with a `sudo -u` hint), runs pending migrations when it opens the store, and creates a missing master key.
  - **Sessions.**
    - Opaque 256-bit `__Host-` cookie (`HttpOnly; Secure; SameSite=Strict`), stored hashed, with idle and absolute expiry.
    - "Fresh 2FA" is valid for 5 minutes.
    - CSRF: SameSite + an `X-SD-CSRF` header token.
    - WebSockets require a session and an exact `Origin` match.
  - **API tokens.**
    - `sd_…`, random, hashed, with scopes and a required expiry (default 90 days); last-used is written at most once per minute.
    - **Accepted only from the `Authorization` header.**
    - **Created or rotated only from a user session with fresh 2FA, and audited.**
    - **Tokens can never manage tokens, auth settings or users**: those actions are marked `SessionOnly` in the registry.
    - Destructive and sensitive actions are refused for tokens (Q8).
    - **A password or 2FA change revokes all sessions and, by default, all tokens.**
  - **Secretbox.** AES-256-GCM, AAD = table/row/column, key id prefix. The master key is `DATA/keys/master.key` (0600).
  - **REST / OpenAPI.**
    - One generic handler.
    - The JSON Schema reflector (moved here from M1) feeds OpenAPI 3.1.
    - **`audit.verify` over REST is rate-limited.**
  - **SPA.** Node runs only in CI and in `make web`; the placeholder `web/dist/index.html` stays committed; strict CSP in hash mode.
- **Footprint:** S2 → ≤ 53 MB; SPA JS ≤ 250 KB gzip (new CI check); no new timers.
- **Security:**
  - the first real attack surface;
  - route-matrix test, which includes the debug socket routes (0600 unix socket only);
  - no user enumeration;
  - threat model updated in `docs/security.md`.
- **Tests:**
  - unit: TOTP vectors, counter reuse, the ±1 step window, /64 keying, the semaphore's fast 503, CSRF, token rules (header only, `SessionOnly`, revoked on password change), XFF replacement, schema goldens;
  - integration: the first-run flow over HTTPS; the setup token file's mode, and that it is never logged; `admin reset`;
  - Playwright + axe on desktop and phone;
  - route matrix.
- **Reviewers:** `security` (required), `accessibility`, `footprint`, `systems`.
- **Exit evidence:** e2e and route matrix green in CI; security review notes; bench green with bundle size.

## M3a — Deploy from image, safely

- **Status (2026-11-27):** built on `phase-1-m3a`; the detailed plan is [plan 0004](0004-m3a-deploy-from-image.md), steps 1–14 done and reviewed (Revisions 5–20), step 15 "Record and close" in progress. `security` approved the close-out and ADR 0012 is accepted. **Open:** the maintainer's manual screen-reader pass for steps 12–14 ([checklist](../accessibility/m3a-screen-reader-checklist.md)), the maintainer's item "Confirm vision, features, architecture" (still unticked in the roadmap), and the release measurements (the CI "S2 live watcher" number, S2 with one and ten exposed services, full S4, RSS after a deploy, the reload cases with 100 routes), all PENDING in [research 05](../research/05-phase0-measurements.md#m3a-release-measurements-step-15). The interrupt rule below was amended in step 15 (the commit point is authoritative).
- **Goal:** deploy a public image with blue-green or stop-first, a health gate and automatic rollback; one-click rollback; ServerDock never leaves the app down because of its own failure.
- **Roadmap items:** deploy from image; blue-green, health gate, automatic and one-click rollback, auto-cancel superseded; stop-first; connection draining; logs of the last N deploys, live pull progress, restart and redeploy-without-rebuild.
- **Packages:** `internal/jobs`, `internal/deploy` (new), `internal/agent` (op protocol v1), `internal/agent/dockerapi` (containers, images, events), `internal/agent/proxy`, `internal/store`, `web/`.
- **Key decisions:**
  - **Agent op protocol v1.**
    - One yamux stream per op: request line → progress lines → one result line, with a `hello` handshake that carries the protocol version.
    - All deploy ops are `LocalOnly` (rule 9).
    - Plan 0004 must cap line and progress sizes and treat agent output as untrusted.
  - **Docker client → ADR 0012:** keep the hand-rolled typed `dockerapi` with M1's per-call deadlines.
  - **Jobs and scheduler.** Jobs carry a lock key, state, attempts, a cancel flag, a timeout and a payload. The scheduler has one "next due" timer plus a channel notify (rule 3).
  - **Jobs never turn a user action into a system action.**
    - A job records **the actor that queued it**.
    - The registry policy is checked at enqueue **and again on every action the job runs**, as that actor.
    - Audit rows carry that actor, with `system` shown only in the `executor` column beside it.
    - `SystemInvoker` is used only for work nobody triggered: scheduled backups, cleanup, and crash reconciliation.
    - Plan 0004 must test that a job queued by a token can't reach an action that token is denied.
  - **Plan 0004 must specify these rules for concurrency, commit order and interrupts:**
    - **Concurrency:** a per-service in-memory mutex, plus a global worker limit of 1–2.
    - **Commit order:**
      1. start the candidate;
      2. health gate;
      3. switch Caddy;
      4. **commit `active_deploy_id`** in the DB;
      5. drain;
      6. stop the old container.

      **The old container is stopped only after the commit.**
    - **Steps 3–4 cannot be cancelled.** A supersede that arrives during the health gate is treated as an unhealthy result: the candidate is removed and the newer deploy proceeds.
    - **Interrupts (amended in M3a step 15, ADR 0012 decision 7 and plan 0004 Revision 16: the commit point is authoritative):** on boot, a `running` deploy from an older `boot_id` is settled by whether it had **committed** (set `active_deploy_id`):
      - **uncommitted:** **rolled back**: containers labelled with its deploy + attempt are removed, routes follow `active_deploy_id`, and the deploy is marked `failed (interrupted by restart)`; for an uncommitted **stop-first** deploy the resync removes the candidate first and then starts the previous version again (two versions never run);
      - **committed:** **finished, not rolled back**: it is marked `succeeded`, the new version keeps serving and the old container is removed (its log tail is kept when possible);
      - the boot pass changes only database state; one resync rule set, run after every successful events connection, does all container clean-up. There is no "resume".
    - **Idempotency:** container names include deploy + attempt (`sd-<project>-<service>-d<deploy>-a<attempt>`), so a repeated create gets Docker's 409 and is treated as "already exists" after an inspect check.
    - **Pulls** get a job deadline and a stall timeout (no progress for N seconds → fail).
    - **Caddy routes are generated from DB desired state starting here** (the active deploys' upstreams), never patched in place.
  - **Upstream IPs change** after a container or daemon restart. Upstreams are regenerated on container `start` events, on boot, and **on every events-stream reconnect: a full container list plus a resync**. Plan 0004 must also weigh publishing ports on `127.0.0.1` instead of dialing container IPs.
  - **Reconciler audit.** Passes that change nothing write no audit rows. When it resolves an `unknown` intent, it writes the outcome row against that `intent_id` (M1 audit rule).
  - **Snapshots and strategy.** A deploy is an immutable snapshot. Blue-green by default; stop-first when a writable volume is attached.
  - **Health gate:** HTTP or TCP from the agent.
  - **Deploy logs:** gzip files, last 10, capped.
  - **Policy check** (default-deny), shared with M3b and M6. **User-supplied `serverdock.*` labels are rejected, including on image deploys.**
- **Footprint:** S2 → ≤ 56 MB. Background rows: scheduler timer, events reconnect backoff.
- **Security:** fuzzed validators; only labelled resources are ever deleted; label spoofing rejected.
- **Tests:**
  - unit: state machine, commit order, a supersede during the health gate, name-collision handling;
  - integration:
    - an unhealthy candidate — the old version keeps serving with zero non-2xx responses;
    - kill -9 at each stage, then restart — an uncommitted deploy is rolled back and marked interrupted, a committed one is finished (`succeeded`, old container removed), and the app is reachable; each run records the outage (research 05);
    - **the Docker daemon restarts — the app is reachable again through regenerated upstreams**;
    - an events-stream drop triggers a resync;
    - a stalled pull fails;
  - Playwright.
- **Reviewers:** `systems` (required), `security`, `footprint`, `accessibility`.
- **Exit evidence:** zero-downtime and interrupt recordings; the daemon-restart test green; bench green.

## M3b — Service configuration

- **Goal:** env vars and secrets, private registries, persistent volumes, service DNS, resource limits.
- **Roadmap items:** env vars & secrets (encrypted, audited reveal); private registry credentials; persistent volumes; per-project network with service DNS; per-app CPU/RAM limits; deploy context variables; Docker RC in CI; Docker version-change warning.
- **Packages:** `internal/deploy`, `internal/secretbox`, `internal/agent/dockerapi` (networks, volumes, `X-Registry-Auth`), `internal/store`, `web/`, CI integration matrix.
- **Key decisions:**
  - The snapshot references the env ciphertext version.
  - **Secret reveal is `Sensitive`: the intent row is written first, and if that write fails, nothing is revealed** (M1 rule).
  - Registry credentials are sent only per pull.
  - Network alias = service name.
  - Bind mounts are admin-only.
  - Context variables `SD_FQDN`, `SD_COMMIT`, `SD_ENV`, `SD_DEPLOY_ID`.
  - Engine version stored in `meta`; a change raises "needs attention".
- **Footprint:** S2 → ≤ 58 MB, S4 → ≤ 47.8 MB (Revision 1 re-base on M3a's provisional 47.6 MB median, re-based again on PR #6's first CI numbers; judged by the full-run median plus a cause, since +0.2 is inside the run spread; the S4 VmHWM will likely pass 50 MB here, which is a watch figure, not a Q3 trigger); no new timers.
- **Security:** AAD binding; sentinel tests; fuzzed path validation.
- **Tests:**
  - a private `registry:2` with auth;
  - name resolution between services;
  - stop-first triggered by a volume;
  - OOM reported;
  - **RC matrix: Docker stable + latest RC**;
  - Playwright.
- **Reviewers:** `security` (required), `systems`, `footprint`, `accessibility`.
- **Exit evidence:** RC matrix green; sentinel tests green; bench green.

## M4a — Domains and automatic HTTPS

*Revision 1 (2026-11-27): the old M4 is split. M4a keeps plan 0006 and everything except passkeys, which moved to M4b (plan 0013, below). M4a runs right after M3b and is part of the early usable version.*

- **Goal:** domains for services and the panel, automatic HTTPS, actionable diagnostics. A certificate or DNS problem never takes a running app down.
- **Roadmap items:** domains with automatic HTTPS and diagnostics; TLS never takes a running app down; IPv6.
- **Packages:** `internal/agent/proxy`, `internal/domains` (new), `internal/store`, `web/`.
- **Key decisions:**
  - **Per-route validation, not all-or-nothing.** The generator validates each route/host. Failing routes are **left out** and get status `error` with a reason; the rest are loaded.
  - **On boot, the config is generated from the DB minus failing routes, never loaded from a stale copy on disk.**
  - ACME via HTTP-01/TLS-ALPN-01 for explicit domains; `:80` now also serves challenges. A per-host redirect is added only once a valid certificate exists.
  - Plan 0006 starts with a spike on observing certificate state through Caddy's own events. **Certificate-expiry alerts (M8) use certmagic's maintenance and events, not a timer of ours.**
  - Diagnostics run on demand (`unknown` when undecidable).
  - IPv6: dual-stack.
  - **Panel domain** (moved here from M2 by plan 0003): set the panel domain, issue its certificate, redirect the IP address to it once the certificate is valid, add HSTS (G15), and prompt the admin to re-enrol TOTP (G5). Plan 0006 must note that the panel domain is also the passkey relying-party ID in M4b, so changing it later invalidates passkeys (M4b adds that warning).
  - **Plan 0006 must also (security, Revision 1):**
    - redirect IP → domain **only while the domain certificate is valid**; when it isn't, the IP panel answers again instead of redirecting to a broken page;
    - offer a **console break-glass** that keeps the IP panel (no redirect), audited;
    - show certmagic failure and expiry events in the Overview "needs attention" list **from M4a** (M8 later adds notifications for them);
    - send HSTS **without `includeSubDomains` or `preload` by default**, so other services under the same parent domain are not forced to HTTPS by the panel.
  - **The panel domain is optional.** Before M4b, sign-in on it is password + TOTP, which can be phished (accepted risk in `docs/security.md`, maintainer 2026-11-27); the early-use guide recommends IP access over an SSH tunnel or a firewall allowlist on 443.
- **Footprint:** S2 → ≤ 61 MB, S4 → ≤ 48.0 MB; certmagic timers measured and listed. **If the QUICK S4 delta is above 0.5 MB, a full, non-QUICK S4 run is needed before merge (rule 4).**
- **Security:** IDNA validation; only the panel domain routes to the control plane; changing it needs fresh 2FA.
- **Tests:**
  - goldens;
  - one invalid route doesn't block the others;
  - boot with a failing route;
  - Pebble: issuance, failure while HTTP stays up, reload safety;
  - IPv6;
  - IP → domain redirect only while the certificate is valid (Pebble: expire it, the IP panel answers again); the console break-glass keeps the IP panel; HSTS has no `includeSubDomains` or `preload`; a certmagic failure shows in "needs attention";
  - Playwright.
- **Reviewers:** `security`, `systems`, `footprint`, `accessibility`.
- **Exit evidence:** Pebble tests green; a manual real-domain check; bench green.

## M4b — Passkeys

*Revision 1 (2026-11-27): split out of the old M4. Detailed plan **0013**. Runs after M6 and before M8; still in Phase 1. It depends only on M4a's panel domain.*

- **Goal:** the admin can sign in with a passkey on the panel domain, with TOTP and recovery codes as the always-available fallback.
- **Roadmap items:** **passkeys for the panel** (added by the maintainer 2026-11-20).
- **Packages:** `internal/auth` (passkeys), `internal/store`, `web/`.
- **Key decisions:**
  - **Passkeys (WebAuthn), maintainer request 2026-11-20.** Passkeys need a domain (the relying-party ID can't be an IP address) and a trusted certificate (browsers turn WebAuthn off on pages with certificate errors), so they arrive after the panel domain works (M4a).
    - A passkey can replace the TOTP step (password + passkey), or sign in on its own (passkey only, user verification required). Several passkeys per account, each with a name, last-used time and a revoke button in Settings › Security.
    - The relying-party ID is the panel domain. Passkey sign-in is offered only on that origin; on the IP address the panel keeps password + TOTP.
    - TOTP and recovery codes stay as the always-available fallback (IP access, lost device). Adding or removing a passkey needs fresh 2FA, is audited, and revokes nothing else. `admin reset` clears all passkeys.
    - Changing the panel domain invalidates passkeys registered for the old one; the UI says so before the change.
    - Stored: credential ID, public key, sign counter, transports, AAGUID, name, created and last-used times. No attestation requirement (`none`), so any authenticator works.
    - Library: one maintained WebAuthn library (e.g. `go-webauthn/webauthn`), justified with a footprint measurement. Control plane only: it must not link into agent mode, and S4 must stay unchanged.
    - New ADR (passkeys) and a `security` threat-model update: challenge single-use and expiry, origin and RP-ID checks, counter regression handling, user-verification policy, and the same back-off as login.
  - Plan 0003's notes for "M4 / passkeys (plan 0006)" apply to plan 0013: `admin reset` clears passkeys; a passkey-only sign-in needs its own back-off and its own "admin exists" rule; the `<unknown>` bucket rule applies to unknown credential ids.
- **Footprint:** S2 → ≤ 69 MB; passkey library delta measured (S2 only). **S4 unchanged (≤ 48.5 MB), shown by a full, non-QUICK S4 run** (S4 headroom, see the allocation table).
- **Security:** a new sign-in path; `security` review required.
- **Tests:**
  - registration and sign-in with a virtual authenticator (Playwright/CDP);
  - wrong origin and wrong RP ID refused;
  - replayed challenge refused;
  - counter regression handled;
  - passkey not offered on the IP address;
  - TOTP fallback still works;
  - `admin reset` clears passkeys;
  - changing the panel domain warns first and invalidates the old passkeys;
  - Playwright, with accessibility checks for the passkey screens.
- **Reviewers:** `security` (required), `accessibility`, `footprint`, `systems`.
- **Exit evidence:** virtual-authenticator tests green; a manual passkey sign-in on a real domain; full S4 run unchanged; bench green.

## M5 — Git builds and push-to-deploy

- **Goal:** GitHub push-to-deploy of Dockerfile apps with live progress and commit status; builds can't OOM the server.
- **Roadmap items:** deploy from Dockerfile (git); GitHub push-to-deploy + generic webhook, commit status; local builds with cgroup caps; size-capped BuildKit cache; build concurrency; live build progress.
- **Packages:** `internal/build`, `internal/sources/github`, `internal/netpolicy` (new), `internal/api` (webhooks), `internal/deploy`, `web/`.
- **Key decisions (ADR 0013, advisor A2):**
  - Recommended: on-demand pinned `moby/buildkit` container with caps and a GC-capped cache. It is **stopped after 5 min idle**, and that timer runs only while BuildKit runs.
  - `buildctl` is run through Docker exec with argv, and BuildKit fetches git itself.
  - Output goes to `POST /images/load` with its own deadline.
  - No new Go dependency.
  - Concurrency semaphore (default 1).
  - GitHub App via the manifest flow.
  - Generic webhook: HMAC, a **replay cache that expires lazily on insert**, 2 MiB cap.
- **`webhook` actor (defined in M1):** may only invoke actions flagged `WebhookAllowed` (deploy) whose target is the service bound to that webhook. Never destructive or sensitive.
- **Outbound-URL policy (rule 10)** is introduced here.
  - **The gap.** BuildKit fetches git inside its own container, so our `DialContext` isn't on that path.
  - Plan 0007 must run the URL pre-check (resolve the host, apply the rule 10 block list) **inside the BuildKit container's network namespace** right before the build, then document the remaining gap: BuildKit resolves the name again, so DNS rebinding between the check and the fetch is still possible.
  - Plan 0007 must also restrict the BuildKit container's network to limit what that gap exposes.
- **Runs after M7b (Revision 1).** Self-update already exists, so plan 0007 must add **a test that a self-update defers while a build job runs** (M7b's quiesce check is generic over running jobs; this test proves builds are covered).
- **Footprint:** S2 → ≤ 64 MB at idle, S4 ≤ 48.0 MB; peak usage during a build measured on 1 GB (repeated in the M6-exit VPS bench).
- **Security:**
  - verify signatures before parsing;
  - repos are untrusted;
  - **BuildKit runs without the `security.insecure` and `network.host` entitlements**;
  - rootless BuildKit is a `security` review point;
  - no tokens in URLs;
  - no fork builds.
- **Tests:**
  - a self-update defers while a build runs;
  - signatures, replay, JWT, argv;
  - netpolicy (metadata IP, a redirect to loopback, DNS rebinding answered with a blocked IP);
  - over-cap build;
  - cache cap;
  - webhook actor can't deploy another service;
  - Playwright.
- **Reviewers:** `security` (required), `systems`, `footprint`, `accessibility`.
- **Exit evidence:** push → deploy → status on a test repo; OOM-cap test green; bench green.

## M6 — Compose, databases and backups

- **Goal:** safe Compose stacks; one-click Postgres, MySQL and Redis; scheduled encrypted S3 backups and restore with approval.
- **Roadmap items:** deploy Compose (policy-checked); Postgres/MySQL/Redis + scheduled S3 backups + restore.
- **Packages:** `internal/compose`, `internal/databases`, `internal/backup` (new), `internal/jobs` (cron with explicit timezone), `internal/statebackup` (S3 target), `internal/netpolicy` (S3), `web/`.
- **Key decisions:**
  - **Compose → ADR 0014:** compose-go v2, our policy, our orchestrator; unsupported fields are rejected with a list. A footprint row is needed, probably with a full S4 run.
  - **Plan 0008 must require:**
    - interpolation **never reads the process environment** (only the service's own variables);
    - **no remote `include`/`extends`**;
    - `env_file`, `configs.*.file`, `secrets.*.file`, `include`, `extends` and `build.context` stay **inside the project directory**: no absolute paths, no `..`, symlinks resolved before the check;
    - **the policy runs on the fully resolved model**;
    - deny-list additions, beyond M3a's list:
      - `network_mode`/`pid`/`ipc` set to `container:` or `service:` pointing outside the stack;
      - `userns_mode: host`;
      - `cgroup_parent`;
      - `security_opt` with `unconfined`;
      - `sysctls`;
      - `device_cgroup_rules`;
      - host-IP port bindings;
      - external networks and volumes;
    - user-supplied `serverdock.*` labels rejected.
  - **Databases:** pinned images, credentials as secrets, no published ports, stop-first.
  - **Backups → ADR 0015:**
    - argv dumps → compression → chunked AES-GCM (per-backup key wrapped by the master key) → S3 multipart, with S3 endpoints checked by netpolicy;
    - `DATA/backups/` is 0700 and backup files are 0600;
    - restore: typed confirmation, fresh 2FA, a pre-restore dump.
    - a restore job cut off by a restart gets its own boot-pass rule (not the generic default): `failed (interrupted by restart)` plus `needs_attention restore_interrupted`, because the app's database may be half-restored (systems, Revision 1).
  - **S3 client chosen on measured cost (footprint, Revision 1).** ADR 0015 (backups) reports, for each candidate S3 client, the `GODEBUG=inittrace=1` output (init time and allocations in agent mode) and the gzip binary delta, and picks the client on those numbers (a small signer on `net/http` is a valid candidate). ADR 0014 (Compose) reports the same two numbers for compose-go.
  - **Where the upload runs.** Plan 0008 must say whether the backup upload runs on the agent (local, `LocalOnly`) or in the control plane. If it runs on the agent, the multipart buffer is bounded (part size × parts in flight, stated in MB) and shown in the footprint row.
  - **Scheduling.** Cron schedules run on the M3a scheduler timer, next to the **daily housekeeping job that M7b added** (Revision 1 moved the job from M6 to M7b); there is no second timer. M6 adds backup retention pruning as a housekeeping task.
  - **State archives off the box (Revision 1).** M7b writes state archives locally only. Plan 0008 must add the S3 backup target (checked by netpolicy) as a state-archive target, so scheduled archives can leave the server, and then change the UI's "local only, copy it off this server by hand" notice to show where each archive went.
  - **Self-update during backups.** M7b's quiesce check is generic over running jobs. Plan 0008 must add **tests that a self-update defers while a backup job runs and while a restore job runs**.
- **VPS bench re-run (required at M6 exit, Revision 1).** The M7a VPS bench runs before M5 and M6, so it doesn't measure compose-go, the S3 client or a BuildKit build. At the M6 exit, run the full `bench/run.sh` plus `serverdock bench` again, plus peak RAM during one capped build and one S3 backup, and append the results to `docs/research/06-phase1-vps-bench.md`. Per the Q6 partial answer (2026-11-22) it runs on **the same host as the M7a run: the maintainer's Hetzner test server (Ubuntu 24.04, 2 vCPU, 3.8 GiB RAM), which is not a true 1 GB machine**; the measured container is capped at `--memory 1g --cpus 1`, and the entry says the host is larger than 1 GB, so host-level free memory and swap pressure during the build are not the 1 GB case. The true 1 GB check stays with the M9 final run. VmRSS on this host is comparable with a 1 GB host, but page-cache behaviour is not (the host has spare memory for cache), and the entry says so. Over the tripwires without a convincing reason → reopen ADR 0004/0011 before M4b.
- **Footprint:** S2 → ≤ 68 MB, S4 → ≤ 48.5 MB (full S4 run required for compose-go and the S3 client, rule 4).
- **Security:** Compose is untrusted (above); S3 keys go in secretbox; backups encrypted before upload; no shell.
- **Tests:**
  - policy fixtures for every deny-list entry;
  - path-escape fixtures (`..`, absolute paths, symlinks);
  - a remote include is refused;
  - process env not interpolated;
  - MinIO round trips, including a state archive uploaded to the S3 target;
  - a corrupted backup is refused;
  - a self-update defers while a backup runs, and while a restore runs;
  - Playwright.
- **Reviewers:** `security` (required), `systems`, `footprint`, `accessibility`.
- **Exit evidence:** round trips green; policy fixtures green; footprint rows; bench green; **VPS bench re-run results**.

## M7a — Install, lifecycle, signed releases

*Revision 1 (2026-11-27): M7a now runs right after M4a. Its exit is the **early usable version**. It also defines "update by reinstalling", the only update path until M7b.*

- **Goal:** one command installs a verified, signed release on a fresh VPS, with honest lifecycle commands; re-running it upgrades an existing install in place. The first real VPS bench runs here.
- **Roadmap items:** install script with pre-flight; signed releases + `serverdock bench` per release; `uninstall`, `start/stop/status`, `doctor --bundle`; supported Docker range published.
- **Packages:** `install.sh`, `cmd/serverdock`, `.github/workflows/release.yml`, `web/` (onboarding server checks).
- **Key decisions:**
  - **Install shape → ADR 0016 (Q4).** Recommended:
    - systemd, a dedicated `serverdock` user in the `docker` group, `CAP_NET_BIND_SERVICE`.
    - **The binary lives in a directory the service user can write (`/var/lib/serverdock/bin/`)** so self-update can swap it. ADR 0016 records that this costs nothing extra, since the docker group is already root-equivalent.
    - Plan 0009 must harden the unit (`NoNewPrivileges`, `ProtectSystem=strict`, `ReadWritePaths=` the data dir, `PrivateTmp`, `Restart=always`) and check that each setting works with Docker socket access.
    - The unit uses **`Type=notify`**: the server sends `READY=1` (stdlib `sd_notify` over `NOTIFY_SOCKET`, about 20 lines) only after its own health check passes. M7b relies on this.
  - **Pre-flight** enforces the minimum Docker version (Q7) and offers, after asking, to install Docker from Docker's official repository.
  - **Signing → ADR 0017.**
    - Each artifact gets a signed manifest that **binds version, OS/arch, file name and SHA-256**.
    - The binary embeds **a key set (current + next)**. ADR 0017 documents rotation (sign with the next key, ship a new next key, retire the old one).
    - Verification uses stdlib `crypto/ecdsa`.
    - Where the key lives is Q5. **If CI signs, it does so only in a protected `release` environment** (required reviewer, tag-only trigger, never `pull_request_target`).
  - **`install.sh` refuses to install when it can't verify the signature** (for example when `openssl` is missing). On a **fresh install only**, `--no-verify` overrides this with a warning. **Upgrade, repair and downgrade modes refuse `--no-verify` outright** (below).
  - **`EXTEND_TIMEOUT_USEC` from M7a (systems, Revision 1).** Reinstalls run migrations before M7b exists, so plan 0009 already makes the server send `EXTEND_TIMEOUT_USEC` while migrations make progress and `READY=1` only after them and the health check. M7b reuses this.
  - **`doctor --bundle`:**
    - redacted;
    - excludes `DATA/keys`, `DATA/pki`, `DATA/setup-token`, `DATA/agent.token` and `DATA/backups/`;
    - reads the DB **only through `store.OpenReadOnly`**.
  - **`--debug-allow-remote` was removed in M1, and the debug listener is a 0600 unix socket.** Plan 0009 must confirm that the release binary has no TCP debug listener.
  - **Re-running `install.sh` upgrades in place (Revision 1).** Until M7b ships self-update, this is how an install is updated, so plan 0009 must define it:
    - it detects an existing install (unit file, service user, `DATA`) and switches to upgrade mode, saying so before it changes anything;
    - **verification in upgrade, repair and downgrade modes:** the new release is verified like a fresh install (signed manifest, version, arch), and **`--no-verify` is refused outright**. These modes don't need `openssl`: the **installed binary embeds the release key set** and checks the new manifest with a subcommand (for example `serverdock verify-manifest`); when `openssl` is present the script checks with it too, and both must pass;
    - **the bytes that are hashed are the bytes that are installed:** the download goes into a root-owned `mktemp -d`, the binary is copied to a temp file in the same directory as `bin/serverdock`, that exact file is hashed and checked against the manifest, and only then renamed into place;
    - it **refuses a version older than the installed one**. The server already refuses a DB schema newer than itself (M1 migration runner), so a plain downgrade could not start anyway. The only way back is the downgrade mode below;
    - **repair:** re-running with **the same version** re-verifies and rewrites the binary and our own unit file, keeps `DATA` and config as below, and restarts the service;
    - **downgrade mode, for restore-then-downgrade only:** it takes an explicit target, **`--downgrade-to X.Y.Z`**, prints that this version is older than the installed one, and **never skips signature verification**. It works **only once the copied `DATA` has been put back**, that is, when the DB schema is no newer than the target binary; otherwise it refuses. The server writes an audit row for the downgrade on its next start;
    - it **keeps `DATA` untouched** (DB, `DATA/keys`, `DATA/pki`, `DATA/agent.token`); it never writes a setup token and never regenerates keys;
    - it **never overwrites config**: a config file or unit drop-in the user changed stays as it is. If a new default differs, it is written next to it as `.new` and the script says so. Our own unit file may be replaced; user changes belong in drop-ins (`systemctl edit`), and the docs say so;
    - **running jobs:** there is no quiesce check before M7b, so the service is stopped even if a job runs. Upgrade mode first reads `serverdock status` (plan 0009 makes it list running jobs). **When it shows running jobs (for example a deploy) it warns, names them, and asks**: the prompt reads `/dev/tty`, the default answer is no, and with no TTY it refuses unless `--yes` is given. If `status` fails, the state is "unknown" and it asks the same way. If the user goes on, M3a's boot pass settles any interrupted deploy: **finished if its commit happened, otherwise `failed (interrupted by restart)`**, so the app stays on its active version;
    - it stops the service, swaps the binary, starts it, and **reports success only after `READY=1`** (`Type=notify`; `EXTEND_TIMEOUT_USEC` keeps a long migration inside the deadline). If the new version doesn't become ready, it says so, prints the journal command, and leaves the copied `DATA` recovery steps on screen. It does not roll back on its own; automatic rollback is M7b;
    - migrations run through the M1 runner, which takes its pre-migrate backup in `DATA/backups/`;
    - apps keep running in Docker during the reinstall; the proxy is down for a few seconds, and the script says so first.
  - **After M7b there is one upgrade path (systems, Revision 1).** Plan 0009 writes upgrade mode so that, **once the installed binary has self-update** (it reports this through a version capability), upgrade mode:
    - **refuses while `DATA/update.json` is not terminal** (`trial` or `rolling_back`) and prints `serverdock status`;
    - for a newer version, hands off to `serverdock update` (or tells the user to run it) instead of swapping the binary itself.

    Reinstalling stays for three cases only: **the M7a → M7b hop**, **repair**, and **restore-then-downgrade**. **The hop:** the first M7b release is installed by re-running `install.sh`, which also rewrites our unit with what M7b needs: a `Restart=` setting that also restarts after a clean exit (`Restart=always`, as above; the hop makes sure of it), so the swap-and-exit in M7b step 2 brings up the new binary; and the `TimeoutStartSec` that serves as the trial's health deadline.
  - **Operating rules before M7b (early use).** Printed by the upgrade mode and written in the README (the docs site comes in M9), until M7b lands:
    - only apps whose data is disposable or backed up elsewhere (this rule stays until M6);
    - **before each reinstall, stop the service and copy `DATA` off the server, including `DATA/keys/master.key`** (without the master key, encrypted secrets in the DB can't be read). Plan 0009 ships **one exact, tested command** that streams the copy encrypted and **never stages plaintext on the server**, shown in the README and in the upgrade printout. For example, run from the user's own machine: `ssh root@server 'systemctl stop serverdock && tar -C /var/lib/serverdock -cf - .' | age -p > serverdock-data.tar.age` (with `umask 077`), or the same `tar` stream saved locally and then encrypted locally; Many VPS images disable root login over SSH, so the README also shows a `sudo` form (for example `ssh user@server 'sudo sh -c "systemctl stop serverdock && tar -C /var/lib/serverdock -cf - ."'`), and the test covers whichever form the README shows. The command stops the service on purpose, because the next step is the reinstall, which starts it again; the printout says so, and for a copy without a reinstall it ends with `systemctl start serverdock`.
    - **what the copy holds:** besides the DB and `DATA/keys/master.key`, also `DATA/pki` (hub CA and panel keys), `DATA/agent.token`, the pre-migrate DB backups and the deploy logs. **Keep it like a password: encrypted, in a private place**, never in a repo, chat or shared drive. The master key is not rotated in Phase 1, so **every old copy stays a live secret**; destroy the old copies once M7b's encrypted state archives exist;
    - the pre-migrate backup is on the same disk and doesn't replace this copy;
    - recovering from a bad reinstall: stop the service, put the copied `DATA` back, and run `install.sh --downgrade-to <previous version>` (downgrade mode above);
    - **check free disk space by hand** (`df -h`) before each reinstall and now and then; disk alerts arrive with M8;
    - **prune old images by hand** (`docker image prune` with care for rollback images); automatic cleanup arrives with M8;
    - prefer reaching the panel by IP over an SSH tunnel, or allowlist 443 in a firewall, over a public panel domain until M4b (accepted risks in `docs/security.md`, 2026-11-27).
- **VPS bench (required at M7a exit):** full `bench/run.sh` plus `serverdock bench` on the maintainer's Hetzner test server (Q6 partial answer: 3.8 GiB RAM, measured container capped at 1 GiB, not a true 1 GB host), with results in `docs/research/06-phase1-vps-bench.md` (first section). VmRSS is comparable with a 1 GB host, but page-cache behaviour is not, and the entry says so. **Since Revision 1 this run comes before M5 and M6, so it doesn't measure compose-go, the S3 client or BuildKit**; those are measured by the re-run at the M6 exit. **It adds a cut-down loaded S2 run** (image services with 1–2 domains, internal issuer), report-only, so the domain cost shows before M8's full S2-loaded scenario. Over the tripwires without a convincing reason → reopen ADR 0004/0011 before M7b.
- **Footprint:** S2 → ≤ 61.5 MB, S4 ≤ 48.0 MB.
- **Security:** supply chain; verify-or-refuse install; upgrade, repair and downgrade never accept `--no-verify`; the installed bytes are the hashed bytes; downgrade only by explicit target after restoring `DATA`, audited; `DATA` and config never overwritten; the `DATA` copy is streamed encrypted; prompts read `/dev/tty` and default to no; dry-run uninstall.
- **Tests:**
  - shellcheck;
  - installs on fresh Debian/Ubuntu containers and a systemd VM;
  - tampered artifact, wrong-arch manifest and older-version manifest all refused;
  - unverifiable fresh install refused without `--no-verify`;
  - **`--no-verify` refused in upgrade, repair and downgrade modes, with nothing changed**, each mode tested;
  - upgrade without `openssl` on the host verifies through the installed binary; with `openssl` present, both checks run;
  - a binary swapped in the temp directory after download but before hashing is refused (the hashed file is the renamed file);
  - **re-run on an existing install** (systemd VM, with an admin, one image app with a domain and a changed config file): the admin still signs in with the same TOTP; the app keeps running; `DATA/keys` and `DATA/pki` are byte-identical; the changed config is byte-identical and a `.new` file appears when a default changed; a schema change leaves a pre-migrate backup; no setup token is written;
  - **a migration longer than `TimeoutStartSec` succeeds through a reinstall** (`EXTEND_TIMEOUT_USEC`), and `READY=1` comes only after it;
  - re-run with an older release → refused, nothing changed; `--downgrade-to` with a newer DB schema → refused; `--downgrade-to` with restored `DATA` and a tampered artifact → refused; a successful downgrade prints the warning and leaves an audit row on next start;
  - re-run with the same version → repair: binary and unit rewritten, `DATA` and config unchanged;
  - re-run while a deploy job runs → warning naming the job; no TTY and no `--yes` → refused; default answer → nothing changed; after going on, the deploy is settled and the app still serves its active version; `status` failing → "unknown" and the same prompt;
  - re-run with a new binary that never sends `READY=1` → reported as failed, not as success;
  - the documented `DATA` copy command runs as shown and leaves no plaintext file on the server;
  - bundle redaction.
- **Reviewers:** `security` (required), `systems`, `footprint`, `accessibility`.
- **Exit evidence:** a signed pre-release; install on a fresh VM recorded; **a re-run upgrade recorded**; **VPS bench results**, including the cut-down loaded run; the early-use rules published in the README and the first pre-dogfood run logged in `docs/research/07-dogfood.md`.

## M7b — State backup and self-update

*Revision 1 (2026-11-27): M7b now runs right after M7a and before M5, M6 and M8. It adds the daily housekeeping job itself (moved here from M6), writes state archives locally only (S3 arrives in M6), and its update quiesce check covers any running job other than the update itself. Self-update no longer starts the counted dogfood window (Part C).*

- **Goal:** ServerDock backs up its own state, updates itself with automatic rollback, rehearses restores, and can be restored from the console. From here, early installs update through self-update instead of reinstalling (after the one M7a → M7b reinstall hop, see M7a). **M7b replaces the "copy `DATA` before each reinstall" rule**: plan 0010 replaces that advice with "copy the encrypted state archive off the server" and tells users to **destroy their earlier raw `DATA` copies** (they hold the unrotated master key). **The app-data rule (disposable or backed up elsewhere) stays until M6**, which adds app-data backups.
- **Roadmap items:** self-update with candidate + rollback; ServerDock state backup + rehearse restore.
- **Packages:** `internal/statebackup`, `internal/update` (new), `internal/jobs` (daily housekeeping job), `internal/store`, `cmd/serverdock` (`state restore`), `web/` (Updates, Settings).
- **Order:**
  1. daily housekeeping job (one recurring job on the M3a scheduler timer, rule 3; its first tasks are the update check and the scheduled state backup);
  2. state backup;
  3. self-update (pre-dogfood runs switch from reinstalling to self-update);
  4. rehearse restore;
  5. `state restore`.
- **Daily housekeeping job (moved from M6).** One recurring job with a list of tasks; each task can be turned off on its own; a task failure is recorded on the job and doesn't stop the other tasks. M6 adds backup retention, M8 adds cleanup, metrics retention pruning and the image digest check. Cron schedules stay in M6.
- **State backup archives.**
  - Contents: online `VACUUM INTO` + `DATA/keys` + `DATA/pki` + **the audit head hash and row count**.
  - **Always encrypted, in a way that lets the server encrypt without knowing the passphrase:**
    1. At setup, argon2id(passphrase, salt) derives an **X25519 key pair**. The server keeps **only the public key**.
    2. Each archive uses an ephemeral X25519 key and ECDH with the stored public key (`crypto/ecdh`), then HKDF (`crypto/hkdf`) into chunked AES-256-GCM. All of this is stdlib.
    3. The archive header stores the argon2 parameters, the salt and the ephemeral public key.
    4. Restore needs the passphrase.
    5. **Restore caps the argon2id parameters it accepts from an archive header** (memory at most 256 MiB, with bounds on time and parallelism; plan 0010 sets the exact caps): the header is read before the archive is authenticated, so a crafted header must not make restore allocate gigabytes.
  - **Passphrase strength.** Setting the passphrase enforces a minimum length (plan 0010 sets it, for example 12 characters) and warns on weak or common passphrases.
  - Because of this, **scheduled daily** archives (a task of the housekeeping job above, once a passphrase is set) run unattended.
  - **Local only until M6 (Revision 1).** M7b has no remote target: netpolicy arrives in M5 and S3 in M6. Archives go to `DATA/backups/state/` (0700, files 0600), written as `.tmp`, fsynced, renamed, then verified. **The verification can only run during the write, while the per-archive key (from the ephemeral ECDH) is still in memory**: the server keeps only the public key, so it can't decrypt an archive later. Plan 0010 verifies by reading the written file back and checking every AEAD chunk with that in-memory key before discarding it. The housekeeping task keeps the newest 7 and **deletes older ones only after the new archive is fully written and verified**; a partial `.tmp` never counts as one of the 7 (leftover `.tmp` files are wiped on boot, M1). Free-space check first. **The UI (Settings › Backups and the Overview "needs attention" list) says that archives are stored only on this server and must be copied off it by hand**, and shows the time of the newest archive. It never calls a local-only archive "backed up off-site". **With no passphrase set, the needs-attention item says that no state backup exists. It also fires when the newest archive is older than about 48 h** (the daily task failed or was turned off). Plan 0008 adds S3 as a target.
  - Free-space check first; files 0600.
  - **This is separate from the local rollback snapshot.** Before an update, the old process also takes an online `VACUUM INTO` **snapshot** into `DATA/backups/` (0600, same trust as the live DB, not encrypted). Rollback uses it without a passphrase.
- **Self-update → ADR 0018 (A3 closed: option b, restart).** SO_REUSEPORT overlap is ruled out: the data-dir flock, the SQLite single writer and the in-process Caddy can't run twice. Apps keep running in Docker; the proxy is down for a few seconds during the restart, and the UI says so beforehand.

  Plan 0010 must implement this trial boot.

  **`update.json` state machine (governs every step below).**
  - Phases: `trial` → `done`, or `trial` → `rolling_back` → `rolled_back`.
  - **Every write of the file is atomic** (temp file, fsync, rename, dir fsync), and each phase is written **before** the step it guards. This includes the attempt-counter increment.
  - **Who may act on it:**
    - A binary counts attempts, runs the trial, starts a rollback or skips the pre-migrate backup **only if `phase == "trial"` and `to` equals its own version**.
    - The `to` binary also finishes an interrupted rollback: on `rolling_back` it continues from step 5.2; on `rolled_back` it only swaps `.prev` back (step 5.4) and exits.
    - The `from` binary, on finding `rolled_back`, writes the rollback audit row (step 7).
    - In every other case a binary **ignores the file**. When the phase is terminal (`done`, or `rolled_back` after step 7), the binary that finds it archives it once to `DATA/backups/update-<utc>.json`. A non-terminal file that isn't addressed to the running binary is shown as "stale update" in `status` and never acted on.

  **The update job and the boot pass (systems, Revision 1).**
  - The update is itself a job (rule 5). Its outcome comes from `update.json`: `done` → `succeeded`; `rolled_back` → `failed ("rolled back")`, linked to the step 7 audit row.
  - **Boot-pass default rule:** any job left `running` by an older `boot_id` whose kind has no specific rule (M3a has one for deploys) is marked `failed (interrupted by restart)`. The update job is the exception: it takes its outcome from `update.json` as above.
  - **Settle before archiving:** the update job's outcome is written before `update.json` is archived, so a crash in between never falls into the default rule. The candidate settles it after writing `done` and before starting the worker; the old binary settles it in step 7, on the job row from the restored snapshot. If the file is already archived, the boot pass finds the outcome in `DATA/backups/update-<utc>.json` by `started_at`. Test (plan 0010): a kill between archiving and settling still gives `succeeded` or `failed ("rolled back")`.

  1. **Old process — quiesce.**
     - **Pause the scheduler first**, so no new job can start.
     - Then wait until **no job other than this update is running under the current `boot_id`**. The check excludes the update's own job id and jobs left over from older boots (the boot pass settles those); it is otherwise a generic check on job state, not a list of job types. Recurring jobs that are scheduled but not running don't count. A running housekeeping job counts like any other, since a state archive may be mid-write. This way builds (M5) and backups and restores (M6), which arrive after M7b, are covered without changing M7b; plans 0007 and 0008 each add a test that proves it.
     - **The wait is bounded** (for example 10 minutes). If the jobs finish in time, the update proceeds. Otherwise the scheduler resumes and the update job ends in a terminal **`deferred`** state whose message names the blocking job (kind, target, id). The Overview "needs attention" list shows **"update deferred since T, blocked by <job>"** until an update succeeds.
  2. **Old process — verify and prepare.**
     - Verify the signed manifest. **Refuse downgrades**; the only exception is rolling back to the local `.prev`.
     - **Run `bin/serverdock.new version --self-check`.** It must exit 0 and report the expected version and arch, which proves the new binary loads and initialises on this host.
     - **Stop serving writes**: the HTTP server answers 503 to non-GET requests, and the API, webhooks and scheduler are paused.
     - Run the **free-space check** (2.2 × DB size + 64 MiB).
     - Take the rollback snapshot (online `VACUUM INTO`, written as `.tmp`, fsynced, renamed, then `quick_check`).
     - Write `DATA/update.json` `{from, to, phase: "trial", attempts: 0, snapshot, started_at}` atomically.
     - Swap `bin/serverdock` → `bin/serverdock.prev` and `serverdock.new` → `bin/serverdock` (renames plus a directory fsync).
     - Exit. systemd restarts the service.
  3. **Candidate — start.**
     - **Its first action, before opening the DB or anything else,** is to read `update.json`. If the file is addressed to it (state machine above), it increments `attempts` with an atomic write.
     - With `Type=notify` (M7a), `TimeoutStartSec` is the health deadline.
     - It runs migrations, sending **`EXTEND_TIMEOUT_USEC` while they make progress**, so migration time doesn't count against the deadline.
  4. **Candidate — trial mode.** Until `phase: "done"`, the trial **must not act on Docker**:
     - no job worker and no scheduler;
     - the reconciler only regenerates Caddy routes from the DB.

     The candidate then runs its health check. On success it sends `READY=1`, writes `phase: "done"` atomically, **deletes the rollback snapshot** (only the newest snapshot is ever kept), and only then starts the worker and scheduler.
  5. **Attempts and timeouts.** A start timeout or a crash counts as an attempt, and systemd restarts the service. **At attempt 3**, the candidate rolls back instead of starting:
     1. write `phase: "rolling_back"` (atomic);
     2. restore the snapshot (step 6);
     3. write `phase: "rolled_back"` (atomic);
     4. swap `.prev` back into place (dir fsync);
     5. exit, so systemd starts the old binary.

     Writes made during the trial are discarded, and the UI says so.
  6. **Snapshot restore order** (used by rollback and by `state restore`):
     1. close the DB;
     2. delete `-wal`/`-shm`;
     3. rename the snapshot into place;
     4. fsync the directory.

     **Safe to repeat:** "snapshot file gone + `phase == rolling_back`" means the rename already happened, so the restore is complete and the binary moves on to writing `rolled_back`.
  7. **Rollback audit row.** The **old binary**, on its first start that finds `phase: "rolled_back"`, appends one `system` audit row recording the discarded window: from/to version, start and end times, and the trial's last audit head hash. The row is idempotent, keyed by the update's `started_at`. The old binary then archives the file. Because the row is written by the binary whose schema matches the restored DB, no cross-version write is needed.
  8. **The update's rollback snapshot replaces the migration runner's own pre-migrate backup.** The runner skips its own `VACUUM INTO` **only when `phase == "trial"` and `to` is its own version**. A stale or terminal `update.json` never suppresses a pre-migrate backup. **M1's runner has no such skip; M7b adds it together with the code that writes `update.json`, and the skip applies only when `phase == "trial"` and `to` equals the running version.**

  Release notes are shown first; config is never overwritten; the daily update check is a housekeeping task and can be disabled (ADR 0007). The earlier systemd `OnFailure=` unit is dropped: the self-check, the attempt counter being the first action, and `Type=notify` cover the realistic failure cases without a root rollback unit.
- **Rehearse restore:** extract into `DATA/tmp/` (0700) after a space check; open the copy with `immutable=1` through `OpenReadOnly`; run `audit verify` and `PRAGMA integrity_check`; run migrations on a scratch copy; report. **`DATA/tmp/` is wiped on boot.**
- **`serverdock state restore <archive>`.**
  - Console-only (a recorded CLI exception). Takes the flock, so it refuses while the server runs.
  - **Does not go through the registry.**
  - Needs the passphrase; AEAD makes the archive authentic.
  - **Refuses an archive whose schema is newer than the binary.**
  - Uses the snapshot restore order (step 6). Afterwards it appends one `system` audit row to the **restored** DB with the archive's head hash and row count.
  - Network restore (UI/REST) is **not offered in Phase 1**. If it is added later, it needs fresh 2FA, an authentic archive, and an existing admin.
  - The console path is allowed on a fresh install because it is the disaster-recovery path and needs root on the host.
- **Footprint:** S2 → ≤ 62 MB, S4 ≤ 48.0 MB; no new timer (the housekeeping job runs on the shared scheduler timer).
- **Security:**
  - signature checked before any swap;
  - downgrade refusal;
  - backups always encrypted;
  - restore only from the console, on an authentic archive (see `docs/security.md` rule 11, "no unauthenticated windows").
- **Tests:**
  - a good update;
  - **an update that includes a migration and whose health check fails: binary and DB both restored, trial writes gone, old version serving**;
  - a new binary that fails `--self-check` → no swap;
  - a candidate that hangs → start timeout → attempt 3 rolls back;
  - migrations longer than `TimeoutStartSec` still succeed (`EXTEND_TIMEOUT_USEC`);
  - **a kill between each step of the snapshot restore order** → the next start completes the restore or rollback correctly;
  - during the trial, no container is created or stopped and no job runs;
  - the rollback audit row is present, and written exactly once;
  - **after a rollback, the old binary starts normally, writes the audit row, archives `update.json`, and does not roll back again**;
  - **a stale `update.json` (a terminal phase, or `to` ≠ the running version) does not suppress the next pre-migrate backup**;
  - a kill between each `update.json` phase write → the next start continues correctly (including "snapshot gone + `rolling_back`");
  - writes get 503 before the snapshot is taken;
  - too little free space → the update is refused before the snapshot;
  - the rollback snapshot is deleted after `done`;
  - the scheduler is paused before the job check (a job queued during the check doesn't start);
  - a job that finishes within the wait bound → the update proceeds;
  - an update deferred while a deploy job runs past the bound, and while a job of a test-only kind runs (proves the check is generic); the job ends `deferred`, the message names the blocking job, and "needs attention" shows "update deferred since T, blocked by <job>";
  - an update deferred while the housekeeping job runs (a state archive mid-write); a recurring job that is only scheduled does not defer it;
  - **an update does not defer on its own job** (with no other job running, it proceeds);
  - **an orphaned `running` job of a test-only kind from an older boot doesn't block an update after a restart**; the boot pass marks it `failed (interrupted by restart)`;
  - the update job shows `succeeded` after `done`, and `failed (rolled back)` after a rollback, linked to the rollback audit row;
  - restore refuses an archive header whose argon2id memory, time or parallelism is over the caps, before allocating;
  - a passphrase below the minimum is refused, and a weak one warns;
  - the "newest archive older than about 48 h" item appears when the daily task is turned off;
  - the housekeeping job: one recurring job on the shared timer (no new timer in a goroutine dump); turning off the update check works; a failing task doesn't stop the others;
  - local state archives: the newest 7 are kept; a kill while writing the 8th leaves the previous 7 intact and the `.tmp` is not counted; an archive that fails verification deletes nothing; the "copy it off this server" notice shows while only local archives exist; with no passphrase set, "no state backup exists" is shown;
  - an install upgraded from M7a by reinstalling, then updated by self-update, keeps its data. **The first M7b self-update test uses a fixture DB taken from a real M7a install upgraded by reinstall** (added to `internal/store/testdata/`);
  - **with the M7b binary installed, `install.sh` upgrade mode is refused while `update.json` is in `trial` and while it is in `rolling_back`** (prints `serverdock status`), and for a newer version hands off to `serverdock update`;
  - a scheduled archive written without the passphrase can be restored with it, and not with a wrong one;
  - `state restore` refuses an archive with a newer schema and appends its audit row;
  - tampered, downgrade and wrong-arch manifests refused;
  - N-1 → N and N-5 → N upgrades using the per-milestone fixture DBs;
  - rehearsal leaves the live data dir untouched;
  - `state restore` refused while the server runs, and working on a fresh install;
  - a wrong passphrase is refused;
  - Playwright.
- **Reviewers:** `security` (required), `systems` (required), `footprint`, `accessibility`.
- **Exit evidence:** recordings of the failed-update rollback (with a migration) and of a restore; the pre-dogfood log shows the first self-update. (The counted dogfood window starts later; see Part C.)

## M8 — Observability and housekeeping

- **Goal:** metrics, live logs, search, alerts; disk never fills silently.
- **Roadmap items:** metrics, live logs, log search, basic alerts; email/webhook/Telegram; disk cleanup + disk alerts; "update available".
- **Packages:** `internal/agent/metrics`, `internal/metrics`, `internal/alerts`, `internal/notify`, `internal/agent/logs`, `internal/cleanup`, `web/`.
- **Key decisions:**
  - **Metrics:** collector every 15 s. **Two tiers: 15 s for 24 h, 10 min for 30 d.** Rollups happen inside the collector tick. This simplifies the four tiers in `docs/architecture.md`, and plan 0011 updates that doc. Retention pruning is a housekeeping task.
  - Plan 0011 must measure disk writes per day and idle CPU.
  - **Alerts:** on the collector tick, on Docker events, on backup job outcomes (M6), and on certmagic events for certificate expiry (M4a; no separate daily check). Both milestones run before M8 in the Revision 1 order.
  - **Cleanup, the image digest check and retention pruning** are tasks of the daily housekeeping job (added in M7b), with a dry-run preview for cleanup. Cleanup replaces the hand pruning of the M7a operating rules.
  - **Notifications:** SMTP with STARTTLS required, signed webhooks, Telegram. Targets are checked with netpolicy.
- **Footprint:** S2 → ≤ 74 MB, S4 → ≤ 49.5 MB (the collector also runs in agent mode; this leaves 0.5 MB under the 50 MB tripwire on the median, so an overrun past the slack crosses it and needs the ADR 0011 written reason, or Q3 applies; VmHWM reported as a watch figure). **Plan 0011 caps the collector's buffers** (samples held in memory before a write, per-container series; the bound stated in MB) **and requires a full, non-QUICK S4 run before merge**; < 1% CPU with 10 services; the S2-loaded scenario is added here.
- **Dogfood clock (Revision 1).** The first build that includes M5, M6 and M8 is the earliest start of the counted dogfood window (Part C).
- **Security:** RE2 only, with a length cap; notification secrets in secretbox; live-log WebSockets use the M2 authz.
- **Tests:**
  - cgroup fixtures for both drivers;
  - rollup math;
  - dedupe;
  - OOM → webhook;
  - search across services + proxy;
  - cleanup keeps rollback images;
  - netpolicy on notification targets;
  - Playwright.
- **Reviewers:** `footprint` (required), `systems`, `security`, `accessibility`.
- **Exit evidence:** S2-loaded numbers; disk-write and CPU measurements; bench green.

## M9 — API, MCP, docs and Phase 1 exit

- **Goal:** publish the API and read-only MCP, ship the docs, run every exit check.
- **Roadmap items:** REST API (OpenAPI); read-only MCP; docs site and guide, in-UI links, empty states; full a11y audit.
- **Packages:** `internal/mcp`, `internal/api`, `internal/redact`, `docs/site/`, `web/`.
- **Key decisions:**
  - MCP over Streamable HTTP at `/mcp`. Tools are the `read`, non-sensitive actions. Output is redacted, marked untrusted, and audited with the `mcp` actor kind.
  - The library is chosen in plan 0012 with a footprint row.
  - Docs hosting is Q6 (the docs part).
  - The counted dogfood window (last 14 consecutive days before exit) runs during M9; M9 can't close before it ends (Part C).
- **Footprint:** S2 → ≤ 75 MB, S4 ≤ 49.5 MB.
- **Security:**
  - the route matrix includes `/mcp` and asserts that no HTTP or MCP path produces a `system` actor;
  - redaction sentinels on every tool's output;
  - rate limits.
- **Tests:** OpenAPI completeness; MCP smoke test; final Playwright + axe run; route matrix.
- **Reviewers:** `security` (required), `accessibility` (required), `footprint`, `systems`.
- **Exit evidence:** Part C.

---

# Part B — Milestone 1 in full: Hardening and core plumbing

## Goal

1. Close the Phase 1 security open items: **#1** wss/TLS for the agent channel, **#2** agent route off Caddy, **#4** caps and rate limits, **#5** Docker call timeouts, **#6** goroutine-leak tests. Also enforce read-only remote agents (rule 9).
2. Fix the CI `changes` job so docs-only pushes don't re-run the bench, with a gate that fails closed.
3. Close or carry over the open Phase 0 boxes, and schedule the threat-model review.
4. Add the migration runner with a read-only open path, the hash-chained audit log, and the typed action registry.

## Out of scope

- HTTP routes for the registry or audit (M2).
- The JSON Schema reflector (M2).
- Auth, SPA, secretbox (M2).
- Jobs (M3a).
- Per-agent identity (Phase 2).
- Panel TLS (M2).

## Design

### CI: when the bench runs, and a gate that can't fail open

**Concurrency** (superseded runs are cancelled; push runs never cancel each other):

```yaml
concurrency:
  group: ci-${{ github.event_name }}-${{ github.event.pull_request.number || github.sha }}
  cancel-in-progress: ${{ github.event_name == 'pull_request' }}
```

**Reuse rule** (`.github/scripts/bench-needed.sh`, run in the `changes` job):

1. If the event is `pull_request` with action `synchronize`, and `before` is an ancestor of `after` (not a force-push), compute the increment `git diff --name-only before after`.
2. If the increment is docs-only, **and** the latest `bench-gate` job of **the `ci.yml` workflow** on `before` concluded `success` → `bench=false`, with reason "reused bench-gate result from <before>".
   - The lookup only trusts our own workflow:

     ```sh
     gh run list --workflow ci.yml --commit "$BEFORE" --event pull_request --limit 1 --json databaseId
     gh run view <id> --json jobs
     ```

     The script reads the `bench-gate` job's conclusion from the latest attempt. Check runs from other apps or workflows are ignored.
3. Otherwise: use the full PR diff against the base, as today.

**Why this is safe (by induction).** A passing `bench-gate` on `before` traces back through docs-only increments to a real passing bench on bench-identical code. If main gains bench-relevant changes, merging them makes the increment non-docs.

**Bench-path regex:**

```
(\.go$|^go\.mod$|^go\.sum$|^bench/|^web/|^\.github/workflows/|^\.github/scripts/)
```

**`changes` job settings:** `permissions: { contents: read, checks: read, actions: read }` (`actions: read` for `gh run`), env `GH_TOKEN: ${{ github.token }}`, `fetch-depth: 0`. The event values are passed in as env (`BEFORE`, `AFTER`, `ACTION`, `BASE`), never inline.

**`bench-gate`** (inputs through `env:`, per rule 11):

```yaml
bench-gate:
  needs: [changes, bench]
  if: always()          # never !cancelled(): a skipped gate counts as passing
  runs-on: ubuntu-latest
  permissions: { contents: read }
  env:
    CHANGES_RESULT: ${{ needs.changes.result }}
    BENCH_NEEDED: ${{ needs.changes.outputs.bench }}
    BENCH_RESULT: ${{ needs.bench.result }}
    REASON: ${{ needs.changes.outputs.reason }}
  steps:
    - run: |
        [ "$CHANGES_RESULT" = success ] || { echo "changes job did not succeed"; exit 1; }
        if [ "$BENCH_NEEDED" = true ] && [ "$BENCH_RESULT" != success ]; then
          echo "bench required but result is $BENCH_RESULT"; exit 1
        fi
        echo "bench: $BENCH_NEEDED ($REASON)" >> "$GITHUB_STEP_SUMMARY"
```

**Why `bench-gate` is the required check.** When the `bench` matrix job is skipped as a whole, GitHub never creates the expanded `bench (…)` checks. Requiring those names would block every PR that legitimately skips the bench.

**Maintainer action:** make `bench-gate` the required check, **with its source pinned to the GitHub Actions app**. `lint-test` and `build` stay required.

### Phase 0 carry-over

| Box | Action |
|---|---|
| Repo skeleton + CI "not yet run on GitHub" | Tick it, linking run 44715013248 (PR #1). |
| Bench harness + CI bench job | Tick it, linking the same run; note it is implemented as scripts. |
| Threat model reviewed by `security` | **M2 entry gate.** Run during M1, after step 6, so the review covers the listener and limits. |
| Maintainer confirms vision, features, architecture | Carried over; due before M3a (Part D). |

### goleak

- `go.uber.org/goleak` is test-only, used in `TestMain` of `internal/hub`, `internal/agent`, `internal/agent/dockerapi`, `internal/api` and `cmd/serverdock`.
- Every ignored third-party goroutine has a comment saying why.
- `runServer` joins the `h.Attach` and `agent.ServeSession` goroutines.
- `dockerapi.Client.Close()` (calls `CloseIdleConnections`) is called on shutdown and in tests.

### Docker client: per-call deadlines and version negotiation

- **No transport-wide `ResponseHeaderTimeout`.**
- `c.call(ctx, timeout, req)`:
  - `DefaultCallTimeout` 30 s for inspect, `_ping` and `version`;
  - callers pass longer values where needed (`stop?t=N` uses N + 15 s; image load later gets its own bound).
- `c.stream(ctx, req)` is bounded only by `ctx`; used for log follow, and later events, pull and build.
- A unit test checks that every exported method uses one of the two helpers.
- Dial timeout 5 s.
- **Negotiation:**
  - on first use, `GET /_ping` reads `API-Version`;
  - the client uses `min(serverAPI, MaxAPIVersion)` and refuses below `MinAPIVersion` (Q7, recommended 1.44);
  - paths are prefixed `/v<negotiated>`;
  - success is cached, failure is not;
  - `researcher` confirms the version range.
- Startup logs the engine version and the negotiated API.

### Agent channel: own TLS listener, hub CA (ADR 0009)

**`internal/pki` creates, on first start, `DATA/pki/` (0700) with:**
- an ECDSA P-256 **hub CA**:
  - `ca.key` 0600, `ca.crt`, 10 years;
  - `BasicConstraints` CA with **`MaxPathLenZero`**, so it can sign leaves but no intermediate CAs;
- a hub leaf (1 year).

**What the hub CA signs:**
- **only** hub server leaves now, and Phase 2 agent client certs later;
- Phase 2 client certs get the **`clientAuth` EKU only**, which ADR 0009 records;
- **the M2 panel certificate is a separate self-signed leaf, never issued by this CA.**

ECDSA because Ed25519 certificates have weak support in TLS clients. The CA fingerprint goes to `DATA/pki/ca.fingerprint` and the log; it is not a secret.

**Hub leaf identity:**
- **`--agent-san` is required whenever `--agent-listen` is set** (repeatable; DNS names or IPs). There is no default, because auto-detected addresses would pick up `docker0` and `br-*` bridge IPs.
- The leaf also carries the URI SAN `urn:serverdock:role:hub`. This is defence in depth: the panel no longer uses this CA, but the marker stops any future leaf from this CA being accepted as the hub by mistake.

**Agent verification (`VerifyConnection`, with `--ca-fingerprint sha256:<hex>` required for `wss`):**
1. find the CA in the peer chain whose fingerprint matches;
2. `x509.Verify` the leaf against it, with `DNSName` = the dialed host (IP hosts are matched against IP SANs) and `ExtKeyUsageServerAuth`;
3. require the role marker.

`ws://` is allowed only for loopback literals.

**Lazy renewal (no timer, no goroutine).**
- `GetCertificate` **ignores SNI** and returns the cached leaf.
- On each handshake it checks `NotAfter` against an injectable clock, and whether the SAN set still matches the flags.
- If fewer than 30 days remain or the SAN set changed, it re-issues under a mutex and writes atomically (temp file + rename + dir fsync).
- **If re-issuing fails**, it keeps serving the current leaf while that leaf is still valid, records `nextRetry = now + 1 min`, and logs once per back-off. There is no retry storm.
- If the current leaf has expired and re-issue fails, the handshake fails (honest).

**Agent token (`SD_AGENT_TOKEN`).**
- The server refuses a token shorter than 43 characters, with characters outside base64url/hex, or with fewer than 16 distinct characters (a generated token is 256-bit; a hand-made one is only screened, not guaranteed).
- If `--agent-listen` is set and no token is given, the server **generates** one into `DATA/agent.token` (0600) and prints the path, not the token.
- The bench switches to a generated 43-character token.

**Hub listener.**
- `--agent-listen ADDR`, **off by default**, serving only `GET /agent/connect` over TLS 1.3.
- **Order of checks:**
  1. the per-IP auth-failure limiter;
  2. the bearer-token compare (constant time);
  3. the session cap;
  4. `websocket.Accept`.

  This way a rate-limited IP never reaches the compare, and an over-cap request is never upgraded.
- The Caddy `/agent/connect` route and `StreamCloseDelay` are removed, so `:80/agent/connect` returns 404.

**Local-only ops (rule 9).**
- The agent protocol keeps a table of op specs `{Name, RemoteOK}`; the zero value is local-only.
- `hello` and `logs` are the only ops with `RemoteOK` (ADR 0012; `hello` was added in M3a step 1).
- `Session.open(name)` in the hub looks the op up in the table, refuses unknown names, and refuses non-`RemoteOK` ops unless `s.ID == LocalID`.
- `LocalID` is reserved, and only `runServer` attaches it, over the in-memory pipe.
- M3a and later add ops without `RemoteOK`, so they are local-only by default. A test that iterates the spec table fails if an op not on the read-only allowlist is marked `RemoteOK`.

**Debug listener.**
- **`--debug-allow-remote` is removed.** `--debug-listen ADDR` is replaced by **`--debug-socket PATH`**: a unix socket created with mode **0600** inside a **0700 directory**, owned by the service user, and off by default. There is no TCP debug listener anymore.
- The bench mounts a shared volume at the socket's directory. A helper container on the same volume runs `curl --unix-socket` (running as the same uid), so the shipped binary is still the one measured.

**Bench.** Scenarios 3–4 use:
- server: `--agent-listen :7443 --agent-san sdbench-srv`;
- the fingerprint, read with `docker cp`;
- agent: `wss://sdbench-srv:7443/agent/connect --ca-fingerprint …`.

### Limits and rate limits (`internal/limits`)

- **Keyed token-bucket limiter**, max 4096 keys, lazy eviction, no goroutine. Uses x/time/rate if already linked, otherwise an in-repo bucket.
- **`ClientIP(r, trustLoopbackProxy)`:** `RemoteAddr` only; XFF only from loopback, last hop. Keys are **normalised to /64 for IPv6**.
- **Hub:**
  - **16 sessions max by default** (`--agent-max-sessions`), refused with 503;
  - 5 auth failures/min per IP (burst 10), answered with 429 + `Retry-After`;
  - auth-failure logs limited to one line per IP per minute.
- **yamux, hub side:**
  - `AcceptBacklog` 8;
  - **an agent-opened stream is a protocol violation: the hub ends that session and logs once** (closing just the stream would leave it buffered in yamux until its close timeout);
  - a per-session semaphore of server-opened streams, **8 for a remote session and 32 for the local one** (when full, calls error);
  - **`MaxStreamWindowSize` stays at yamux's 256 KiB minimum** (yamux rejects 64 KiB; decision 2026-11-20);
  - explicit open and close timeouts.
  - Worst-case pinned memory: 16 sessions × 8 streams × 256 KiB = **32 MiB** steady state, reachable only by authenticated agents. It holds because an agent-opened stream ends the session; closed hub streams can linger up to 10 s, and a violating agent pins about 2 MiB per connection briefly. Recorded in `docs/security.md` row 8.
- **yamux, agent side:** 32 concurrent streams.
- **Op request lines** stay capped at `MaxOpLine`.
- **HTTP servers:**
  - `MaxHeaderBytes` 64 KiB;
  - `ReadHeaderTimeout` 10 s;
  - body cap 1 MiB;
  - `netutil.LimitListener` (256 internal, 64 agent), wrapped by a **per-IP concurrent-connection cap enforced in the listener's `Accept`** (agent listener: 4 per IP; a connection over the cap is closed at once);
  - no global `ReadTimeout` on listeners that serve upgrades.
- **Caddy public server:** `read_header` 10 s, `idle` 2 m, `max_header_bytes` 64 KiB.

### Store: migrations and read-only open

**Migration runner.**
- Embedded `NNNN_name.sql` files, recorded in `schema_migrations(version, name, sha256, applied_at)`. Forward-only.
- Phase 0 DBs (`meta.schema_version = '1'`) are adopted as v1, and **`meta.schema_version` is never written again** (a test asserts this).
- **Refuses to start** on a checksum mismatch or an unknown, newer version.
- **Foreign keys during migrations.** The DSN sets `foreign_keys(1)`, so a table rebuild would cascade-delete child rows. The runner therefore pins one writer connection (`db.Conn(ctx)`) and runs:
  1. `PRAGMA foreign_keys=OFF` (it can't be changed inside a transaction);
  2. `BEGIN IMMEDIATE`;
  3. the migration;
  4. **`PRAGMA foreign_key_check`**; any row → `ROLLBACK` and refuse to start;
  5. `COMMIT`;
  6. `PRAGMA foreign_keys=ON`.

  The `ON` step runs in a `defer`, so the pooled connection never goes back with FK checks off.
- **Pre-migrate backup** (only for a DB that has data; **M1 has no skip**: M7b adds the `update.json` skip in the same change that writes the file):
  1. **free space ≥ 2.2 × DB size + 64 MiB** on the `DATA` filesystem; otherwise refuse to migrate, with a clear error;
  2. `VACUUM INTO DATA/backups/pre-migrate-<from>-<to>-<utc>.db.tmp`, fsync the file, rename to `.db`, fsync the directory;
  3. **`PRAGMA quick_check` on the backup**; if it fails, refuse to migrate;
  4. keep the last 3.

  `DATA/backups/` is 0700, files are 0600.
- **On boot**, leftover `DATA/backups/*.tmp` files and the whole `DATA/tmp/` directory are wiped.
- One server per data dir: `flock` on `DATA/serverdock.lock`.
- `WriteTx` helper.
- **Fixture DBs:** `internal/store/testdata/` gets one fixture per milestone merge that changes the schema (not per release), so the N-5 tests in M7b have fine-grained steps.

**Read-only open, `store.OpenReadOnly(dir, opts)`.**
- **No `commonPragmas`** (`journal_mode(WAL)` is a write); only `mode=ro` and `query_only(1)`.
- **No migrations, no flock.**
- **Must never create files** (in particular no root-owned `-wal`/`-shm`). Modes:
  - **The live server is running** (the `-wal`/`-shm` files exist): open with `mode=ro`. This needs SQLite's read-only shm support (pure-Go driver; confirmed by a test).
  - **No `-wal` file, or `opts.Immutable`** (used by rehearse restore on its own copy): open with `immutable=1`.
  - **Otherwise** (a `-wal` with content, but no server): refuse with "start the server once or copy the DB to scratch".
- **Schema reporting:** known version → ok; newer → error; a Phase 0 DB without `schema_migrations` → **"v1 (unadopted)"**.
- **Used by:** `audit verify`, `doctor --bundle` (M7a), rehearse restore (M7b).

### Audit log (`internal/audit`, migration 0002)

```sql
audit_log(
  id INTEGER PRIMARY KEY, ts TEXT, boot_id TEXT,
  actor_kind TEXT, actor_id TEXT, executor TEXT /* NULL, or 'system' when a job/system runs it on the actor's behalf */,
  action TEXT, target TEXT, risk TEXT,
  dry_run INTEGER, phase TEXT /* intent|outcome|denied */, intent_id INTEGER NULL,
  args_json TEXT /* redacted */, result TEXT /* pending|ok|error|denied */,
  error TEXT /* redacted */, approver TEXT NULL, request_id TEXT,
  prev_hash BLOB, hash BLOB
) STRICT;
CREATE INDEX audit_log_intent ON audit_log(intent_id);
```

The table is append-only.

**Status of an intent:**
- it has an outcome row (any boot, joined via `intent_id`) → that outcome's result;
- no outcome, same `boot_id` → `pending`;
- no outcome, older `boot_id` → `unknown`.

**An outcome row written in a later boot may resolve an `unknown` intent.** The M3a reconciler uses this when it can establish what actually happened.

**Hash:** `SHA-256(prev_hash || enc(fields))`, with a length-prefixed fixed-order encoding that includes `boot_id`. Genesis is zero bytes. Appends happen inside the single writer's `BEGIN IMMEDIATE`, so the chain is linear.

**What gets written:**
- mutate, destructive **and sensitive** actions (including sensitive reads such as a secret reveal) write an intent row first and an outcome row after;
- other reads are not audited;
- denied attempts write one `denied` row, **rate-limited per actor + action** (at most 10/min; beyond that, one "N denied attempts suppressed" row per minute).

**Partial failures:**
- **The intent write fails:** nothing executes and nothing is revealed; return "audit unavailable; action not run".
- **The outcome write fails after success:** return the result with `AuditOutcomeRecorded=false` + `ErrAuditOutcome`, and log an error (action, target, request id).
- **Both execution and the outcome write fail:** return the execution error wrapped with `ErrAuditOutcome`.

**Redaction:** the `error` column and every audit-related log line go through `internal/redact`. In M1 it removes the values of the current call's `sd:"secret"` fields; M3b adds known secret-store values.

**Verify:**
- `Verify(ctx, ro *sql.DB)` returns the first broken id.
- `serverdock audit verify --data DIR` uses `OpenReadOnly`. Exit codes: 0 valid, 1 broken, 2 could not open.
- **Head hash and row count** are logged at startup and returned by `system.info`. M7b also stores them in state backups, so truncating the tail of the log can be detected against an older copy.

### Action registry (`internal/actions`)

**Scope hierarchy:** `read` < `write` < `admin`; a higher scope includes the lower ones.

```go
type Def[In Validator, Out any] struct {
    Name, Summary   string
    Risk            Risk            // Read | Mutate | Destructive
    Scope           Scope           // must fit Risk (checked by Register)
    Sensitive       bool            // audited intent-first; fresh 2FA for users
    SessionOnly     bool            // only user sessions (token/auth/user management; used from M2)
    System          string          // non-empty = SystemInvoker may run it; the written reason (security reviews it for destructive actions)
    WebhookAllowed  bool            // webhook actor may run it for its bound target (used from M5)
    Target          func(In) string
    DryRun          func(context.Context, Call, In) (Plan, error)
    Execute         func(context.Context, Call, In) (Out, error)
}
```

**`Register` panics (at startup, so a test catches it) when:**
- **the scope doesn't fit the risk.** Read: any scope. Mutate: `write` or higher. Destructive or Sensitive: `admin`.
- **`In` contains a free-form field** (`map`, `any`/interface, `json.RawMessage`) without the tag `sd:"secret"` (redacted) or `sd:"audit"` (explicitly safe to record).
- (no panic, but a CI check) every destructive action with a non-empty `System` is listed in `docs/security.md` "Unattended destructive actions", so that `security` reviews it.

A test registers every built-in action and walks the registry to check these rules.

**Actors.**
- `Actor` has unexported fields. The exported constructors are `UserActor(id, scopes, mfaFreshUntil)`, `TokenActor(id, scopes)`, `MCPActor(tokenID)` and `WebhookActor(hookID, boundTarget)`.
- **There is no exported way to build a `system` actor.** System calls go through a `SystemInvoker` handle.
  - `reg.SystemInvoker()` may be called once; a second call panics.
  - `cmd/serverdock` hands it to `admin reset` and to the M3a scheduler and reconciler. It is used **only for work nobody triggered** (scheduled backups, cleanup, crash reconciliation). `internal/api` and `internal/mcp` never receive it.
  - **Jobs queued by an actor run as that actor.** The job stores the actor, and `Invoke` checks the policy for that actor both at enqueue and on each action the job runs. The audit row then carries that actor with `executor = 'system'`. The executor marker is **not** a plain option field. It can only be set through the handle, `inv.InvokeAs(actor, name, raw, opts)`, which the job runner holds. (`state restore` doesn't use the registry; see M7b.)
  - An import test fails if `internal/api` or `internal/mcp` refer to `SystemInvoker`. The M2/M9 route-matrix test also checks that no HTTP or MCP request produces a `system` actor kind.
  - (A literally unexported constructor can't be used from `internal/jobs`, so the one-time handle is how this is enforced.)

**Risk policy in `Invoke`:**

| Actor | Read | Sensitive | Mutate | Destructive |
|---|---|---|---|---|
| `user` | scope | scope + fresh MFA | scope | scope + fresh MFA + `Confirm == Target` |
| `token` | scope | **denied** | scope (not `SessionOnly`) | **denied** (Q8) |
| `mcp` | `read` scope | denied | denied | denied |
| `webhook` | denied | denied | only `WebhookAllowed` and `Target == bound target` | denied |
| `system` (invoker) | allowed | allowed, audited | only `System != ""` | only `System != ""` |

`SessionOnly` actions refuse every actor except `user`. In M1, no action is `WebhookAllowed`, so webhook actors are denied everything.

**Flow:**
1. size check (64 KiB) and strict decode;
2. `Validate`;
3. policy (table above);
4. audit intent;
5. `Execute` or `DryRun`;
6. audit outcome.

Panics are recovered and recorded as an `error` outcome.

**Built-ins:** `system.info` (version, uptime, own RSS/CPU, audit head hash + row count), `system.docker`, `audit.verify`.

**No HTTP exposure in M1.**

## Footprint budget

- **S2:** +≤ 1.4 MB → **≤ 49.0 MB** (47.6 + 1.4; slack +2). The agent listener is off by default; pki runs at startup only; limiter maps are empty.
- **S4:** +≤ 0.9 MB → **≤ 45.0 MB** (44.1 + 0.9; slack +0.5). Adds a TLS client and one session; smaller yamux windows cap buffer growth.
- **Idle CPU:** unchanged. No new timers; renewal and its back-off are lazy.
- **Binary:** +< 0.5 MB gzip.
- **Disk:** pre-migrate backups (last 3, with a space check); audit ≈ 300 B per row.

## Security notes

- **Resolves:**
  - #1: TLS 1.3 + pinned CA + name and role checks;
  - #2: Caddy no longer carries agent traffic;
  - #4: caps, limiter-first ordering, 16 sessions, 64 KiB windows;
  - #5: per-call deadlines;
  - #6: goleak.
- **Mitigates #3** (still open until Phase 2): remote agents can't receive `LocalOnly` ops; the token must be strong; the listener is off by default.
- **Removes** the unauthenticated remote debug route (`--debug-allow-remote`).
- **New secrets:** `DATA/pki/*.key` and `DATA/agent.token` (0600, directory 0700, never logged, excluded from bundles, included in encrypted state backups).
- **New inputs:** `--ca-fingerprint`, `--agent-san`, token length; registry inputs (strict, fuzzed, free-form fields refused); XFF.
- **Audit secrecy:** redacted args and errors; intent-first for sensitive reads; denied rows rate-limited.
- **No system actor can be reached from HTTP or MCP.**
- **CI:** our own workflow's gate only; inputs through env; never `pull_request_target`.

## Steps (each one PR-sized, in order)

1. **CI bench trigger and gate.**
   - Do: `bench-needed.sh` (with the `gh run list --workflow ci.yml` lookup), `concurrency`, job permissions and `GH_TOKEN`, env-only inputs, the extended regex, `bench-gate`, and **`.github/CODEOWNERS` assigning `.github/` to the maintainer** (with "require code owner review" switched on in branch protection by the maintainer).
   - Test (`bench-needed_test.sh`, scratch repo + stubbed `gh`):
     - a Go change → bench;
     - **A (Go) → B (docs) → C (docs)**: B and C reuse; with B's gate stubbed as failed → C runs a full diff;
     - a gate result from another workflow is ignored;
     - force-push → full diff;
     - a missing `before` → full diff;
     - a `.github/scripts/` change → bench.
   - Real PR: a docs-only skip; a cancel-and-rebench; a broken `changes` → gate fails.
   - The maintainer sets the required check (pinned to GitHub Actions).
2. **Phase 0 carry-over.**
   - Do: tick the CI and bench boxes with links; add the carried-over items; schedule the `security` threat-model review after step 6.
   - Test: links resolve; no Phase 0 box is left without a decision.
3. **goleak, joined goroutines, `Client.Close()`.**
   - Test: `go test -race ./...` passes under goleak.
4. **Docker per-call deadlines + negotiation** (needs Q7).
   - Tests:
     - an inspect that hangs fails at its deadline;
     - `stop?t=N` is not cut at 30 s;
     - a log follow longer than 30 s keeps streaming;
     - `min()` negotiation;
     - a daemon that is too old is refused;
     - a ping retry after a failure;
     - the helper-usage test;
     - the integration busybox test.
5. **pki, agent TLS listener, token rules, local-only ops, debug flag removal.**
   - Do: ADR 0009; `internal/pki`; `--agent-listen` with a required `--agent-san`; `GetCertificate` with lazy renewal and back-off; agent `VerifyConnection`; token length check and generation; the op-spec table (`RemoteOK`, zero value local-only) and the hub check; remove the Caddy route, `StreamCloseDelay` and `--debug-allow-remote`; bench updates (netns helper, generated token, SAN).
   - Tests:
     - CA has `MaxPathLenZero`; file modes;
     - wrong fingerprint, another CA, a same-CA leaf without the role marker, and a SAN mismatch are all refused;
     - `--agent-listen` without `--agent-san` refuses to start;
     - **lazy renewal**: at 31 days no re-issue, at 29 days a re-issue; concurrent handshakes issue only once; a SAN change triggers re-issue; SNI is ignored;
     - **a failed write still completes the handshake with the current leaf, and a burst of handshakes inside the back-off attempts exactly one re-issue**;
     - an expired leaf with a failing re-issue → the handshake fails;
     - a short token is refused; a generated token file is 0600;
     - **a non-`RemoteOK` op to a remote session is refused, an unknown op is refused everywhere, and a non-`RemoteOK` op to the local session is allowed**;
     - the spec-table iteration test;
     - `:80/agent/connect` → 404;
     - the debug socket is created 0600, there is no TCP debug listener, and the bench helper reaches it through the shared volume;
     - a Caddy reload keeps the agent session;
     - reconnect over TLS.
   - CI bench green.
6. **Limits.**
   - Tests:
     - the 17th session → 503;
     - **ordering: a rate-limited IP is refused without the token being compared** (counted via an instrumented comparer), and **an over-cap request is refused before `websocket.Accept`** (no upgrade response);
     - the 11th bad token from one IP → 429;
     - IPv6 /64 keying;
     - the map stays ≤ 4096 entries;
     - a 5th concurrent TCP connection from one IP to the agent listener is closed in `Accept`, while other IPs still connect;
     - an agent-opened stream ends the session;
     - the 33rd stream errors on the local session, and **the 9th stream errors on a remote session**;
     - **the yamux window is pinned at 256 KiB** (yamux rejects 64 KiB; a test notices if that changes);
     - a stream opened with an already-cancelled context is closed without a data race (`-race`);
     - the agent listener answers with `Connection: close` and drops a silent connection at its 5 s header/handshake timeout;
     - oversized body and header are refused;
     - XFF ignored from non-loopback peers.
7. **Migration runner + read-only open.**
   - Tests:
     - fresh DB and re-open;
     - Phase 0 adoption (`meta.schema_version` untouched afterwards);
     - checksum and newer-version refusals;
     - **rebuilding a parent table keeps its child rows** (FK off/check/on);
     - an FK violation introduced by a migration → rollback and refusal;
     - the pooled connection has `foreign_keys=1` afterwards;
     - a too-small free-space value (injected) → refused, DB untouched;
     - the backup goes `.tmp` → rename, passes `quick_check`, ends up 0600 in a 0700 directory;
     - leftover `.tmp` files and `DATA/tmp` are wiped on boot;
     - a second server is refused;
     - `OpenReadOnly`: works next to a live writer; **opened as a different uid it creates no new files in DATA, and the server can still write afterwards**; uses `immutable=1` when there is no WAL; refuses a dirty WAL with no server; reports "v1 (unadopted)" for a Phase 0 DB.
8. **Audit log.**
   - Tests:
     - chain verification; tampering detected with the right id (field, `boot_id`, deleted row, swapped rows);
     - 50 concurrent appenders;
     - `pending` / `unknown` by `boot_id`;
     - **an outcome written in a later boot resolves an `unknown` intent**;
     - the `intent_id` index exists;
     - a failed intent write → no execution;
     - a failed outcome write → result with `AuditOutcomeRecorded=false`;
     - a sentinel secret in an error message is redacted from the `error` column and the logs;
     - denied rows are rate-limited and a "suppressed" row appears;
     - **`audit verify` against a running server's data dir, as a different uid**: succeeds, creates no files, the server keeps writing;
     - CLI exit codes.
9. **Action registry.**
   - Tests:
     - `Register` panics on scope/risk mismatch and on untagged `map`/`any`/`RawMessage` fields;
     - the CI check fails when a destructive action has `System` set but isn't listed in `docs/security.md`;
     - **an `Invoke` carrying a token actor with the job-executor marker is still denied an action the token is denied, and the audit row records the token actor with `executor = 'system'`** (the job runner itself is tested in M3a);
     - the walk over every action;
     - each cell of the policy table (user, token, mcp, webhook, system × read, sensitive, mutate, destructive);
     - `SessionOnly` refuses tokens;
     - a second `SystemInvoker()` call panics;
     - the import test (`internal/api` and `internal/mcp` don't reference `SystemInvoker`);
     - a sensitive read whose intent write fails reveals nothing;
     - dry-run never executes;
     - panic → `error` outcome;
     - a sentinel never appears in the DB bytes;
     - `system.info` returns the head hash and row count;
     - fuzz seeds in CI, plus a 5-minute local fuzz run before merge.
10. **Record and close.**
    - Do:
      - `docs/security.md`: resolve #1/#2/#4/#5/#6; record #3's Phase 1 mitigation, the audit limits (rebuilt chain, tail truncation) and the yamux bound;
      - `docs/architecture.md`: the components above, and the **Background work** table (rule 3);
      - roadmap tick;
      - M1 bench numbers in `bench/results/` and research/05.
    - Test: links resolve; CI green.

## Test plan

- **Unit:** everything listed in the steps.
- **Integration (default build):** hub ↔ agent over TLS with reconnect; Caddy reload; limits on real listeners; local-only refusal over a real remote session; `audit verify` against a live server as another uid; goleak.
- **Integration (`-tags integration`):** a log stream through the negotiated client; `stop?t=N`.
- **CI:** bench green; skip and fail-closed behaviour shown on a real PR.
- **Manual 1 GB VPS:** not required for M1; the first VPS bench is at the M7a exit.

## Reviews required

- `reviewer`: this plan and every step's diff.
- `security`: steps 1, 5, 6, 8, 9 and ADR 0009; plus the threat-model review (M2 entry gate).
- `systems`: steps 1, 3, 5, 6, 7, 8.
- `footprint`: steps 4, 5, 6, 10.
- `accessibility`: not needed (no UI change).

---

# Part C — Mapping

## Every Phase 1 roadmap item → milestone

| # | Roadmap item | Milestone(s) |
|---|---|---|
| 1 | Install script with pre-flight checks, setup token, admin + TOTP, first-run onboarding | M2 (setup token, admin + TOTP, onboarding UI, `admin reset`), M7a (install, pre-flight, server checks), M5 (connect GitHub) |
| 2 | Signed releases (cosign); `serverdock bench` published with each release | M7a |
| 3 | Action registry (typed ops, risk tiers, audit log) | **M1** (schema reflector in M2) |
| 4 | Deploy from image, Dockerfile (git), and Compose (policy-checked) | M3a (image), M5 (Dockerfile), M6 (Compose) |
| 5 | GitHub push-to-deploy + generic webhook, commit status | M5 |
| 6 | Blue-green, health gate, automatic rollback, one-click rollback from snapshots; auto-cancel superseded | M3a |
| 7 | Domains with automatic HTTPS and actionable diagnostics; TLS never takes a running app down | M4a |
| 7a | Passkeys for the panel (WebAuthn), with TOTP and recovery codes as fallback | M4b |
| 8 | Env vars & secrets (encrypted, audited reveal) | M2 (secretbox), M3b |
| 9 | Postgres / MySQL / Redis + scheduled S3 backups + restore | M6 |
| 10 | Metrics, live logs, log search, basic alerts; email/webhook/Telegram | M8 |
| 11 | Local builds with cgroup caps | M5 |
| 12 | Private registry credentials; persistent volumes; per-project network with service DNS | M3b |
| 13 | Disk cleanup + disk alerts; size-capped BuildKit cache; build concurrency | M5 (cache, concurrency), M8 (cleanup, alerts) |
| 14 | Per-app CPU/RAM limits; stop-first; connection draining | M3a (stop-first, draining), M3b (limits) |
| 15 | Logs of last N deploys; live build/pull progress; restart and redeploy-without-rebuild; deploy context variables | M3a (deploy logs, pull progress, restart, redeploy), M3b (context vars), M5 (build progress) |
| 16 | "Update available" notice for app images | M8 |
| 17 | IPv6; Docker API negotiation + supported range + Docker RC in CI | M1 (negotiation), M3b (RC matrix, version warning), M4a (IPv6), M7a (range published, pre-flight) |
| 18 | Token expiry; `uninstall`, `start/stop/status`, `doctor --bundle` | M2 (token expiry), M7a |
| 19 | Docs site + first-deploy guide; in-UI docs links and empty-state hints | every UI milestone, M9 |
| 20 | Self-update with candidate + rollback; state backup + rehearse restore | M7a (update by reinstalling, until M7b), M7b, M6 (S3 target for state archives) |
| 21 | REST API (OpenAPI) | M2 (skeleton), each milestone, M9 (complete) |
| 22 | Read-only MCP with scoped `read` tokens and redaction | M9 |
| 23 | Accessible, phone-usable UI | every UI milestone (M2–M8, including M4a and M4b), M9 (full audit) |

- **Security open items:** #1, #2, #4, #5, #6 → **M1**. #3 → Phase 2, mitigated in Phase 1 by rule 9 (M1 step 5). #7 (bench socket) → ongoing.
- **Phase 0 leftovers:** CI and bench boxes → M1 step 2; threat-model review → M2 entry gate; docs confirmation → before M3a.

## Phase 1 exit criteria → evidence

| Exit criterion | Evidence | When |
|---|---|---|
| Full `bench/run.sh` on a fresh 1 GB VPS (deferred from Phase 0) | **First run at the M7a exit**: unmodified `bench/run.sh` (3 runs, 2 min warm-up, 5 min idle) plus `serverdock bench` on the systemd install, in `docs/research/06-phase1-vps-bench.md` with provider, plan, kernel and Docker version. The M7a run is on the maintainer's Hetzner test server (3.8 GiB RAM, measured container capped at 1 GiB; Q6 partial answer), so it is not a true 1 GB host. It comes before M5 and M6, so **a re-run at the M6 exit, on the same host and terms,** adds compose-go, the S3 client and peak RAM during a capped build and an S3 backup. **Final run at M9, on a true 1 GB VPS** unless the maintainer decides otherwise, on the release candidate, appended to the same file. Over the tripwires without a convincing reason at any point → reopen ADR 0004/0011. | M7a + M6 + M9 |
| Fresh 1 GB VPS → first git app on HTTPS in < 5 min | Timed log from `curl … \| sh` to the first HTTPS 200 from the app domain (`bench/first-deploy-timer.sh`, human steps by the maintainer), median of 3 fresh VPSs, plus a screen recording; in research/06. | M9 |
| Idle ≤ 80 MB / < 1% CPU published | Phase 1 release notes, the README and the docs site, linked to the tag's CI bench and the VPS results. | M9 |
| Security review passed | Full-surface `security` review with no open high or critical findings, in the M9 PR and summarised in `docs/security.md`. | M9 (+ per milestone, + the M2 entry review) |
| Accessibility review passed | `accessibility` audit of every screen (WCAG 2.2 AA, 390 px, 44 px, keyboard, screen reader) plus green Playwright + axe runs. | M9 (+ per milestone) |
| Dogfooded for 2 weeks | `docs/research/07-dogfood.md` (Revision 1 clock): **the counted window is the last 14 consecutive days before the Phase 1 exit**, on builds that include M5, M6 and M8, and every update inside the window goes through self-update, with **at least one self-update inside the window**. A reinstall, a hand-swapped binary or a `state restore` inside the window restarts the 14 days. An automatic rollback inside the window is logged as an incident and does not restart the clock. The file lists the apps, the server, every deploy, update and incident with its fix, and bench snapshots, and is confirmed by the maintainer. Use before the window (from the M7a exit on) is logged in the same file under **"Pre-dogfood runs"** and doesn't count; until M6 those runs are limited to apps whose data is disposable or backed up elsewhere. | Pre-dogfood runs from M7a; counted window after M8, ending at the exit (M9) |

---

# Part D — Open questions and decisions

## Questions for the maintainer

1. **Q1 — Remote agents in Phase 1** *(before M1 step 5)*.
   - Recommended: keep `serverdock agent` as **experimental and read-only**. It gets `logs` only; every write or secret-bearing op is local-only until Phase 2 enrollment.
   - It sits behind `--agent-listen` (off by default, port 7443 suggested), with a required `--agent-san`, a strong token, and 16 sessions max.
   - This keeps bench S3/S4 measuring the remote shape.
   - The alternative is disabling remote agents entirely, which drops S3/S4 and changes ADR 0008's S4 gate.
2. **Q2 — Panel access before a domain exists** *(before M2)*.
   - (a) **Recommended:** HTTPS on the IP with a self-signed panel leaf (not the hub CA); the leaf fingerprint is printed on the console for the user to compare.
   - (b) Require a panel domain at install.
   - (c) Plain HTTP during setup (not recommended).
3. **Q3 — S4 headroom** *(advisor A1; Revision 1: **due at the first S4 tripwire crossing on the gate metric** (the CI median of per-run medians), or before M6 at the latest, since M6 and M8 are the planned S4 increases. On the provisional cloud-container M3a base the median is 47.6 MB, about 2.4 MB of headroom, re-checked on PR #6's first CI numbers; the 49.8 MB VmHWM is a watch figure, will likely pass 50 MB from M3b, and doesn't trigger Q3)*. If the S4 median goes over 50 MB: pull the slim agent forward (recommended), or accept the crossing with an ADR 0011 written reason? (Since ADR 0011 the 50 MB number is a tripwire, not a gate.)
4. **Q4 — Install shape** *(**due before M7a**, which since Revision 1 starts right after M4a, so earlier than first planned; it also covers the reinstall-upgrade behaviour)*. Recommended: systemd, a dedicated user in the docker group, `CAP_NET_BIND_SERVICE`, the binary in `/var/lib/serverdock/bin/`, a hardened unit. Alternatives: root, or a container.
5. **Q5 — Release signing key** *(**due before M7a**, now right after M4a; the early usable version is the first signed release)*. **Not a plain repository Actions secret.**
   - (a) **Recommended for Phase 1:** a hardware key held by the maintainer, signing each release manifest locally.
   - (b) CI signing **only** in a protected `release` environment (required reviewer, tag-only trigger), with the key in a cloud KMS or an environment-scoped secret.

   Either way, the binary embeds the current and next public keys.
6. **Q6 — Dogfood, VPS and docs hosting.**
   - **Partial answer (maintainer, 2026-11-22): the M7a VPS bench runs on the maintainer's test server (Hetzner, Ubuntu 24.04, 2 vCPU, 3.8 GiB RAM).** It is not a 1 GB machine: the bench already caps the measured container at `--memory 1g --cpus 1`, and the research/06 entry must say the host is larger than 1 GB (so host-level free memory and swap pressure are not the 1 GB case). The M6-exit re-run (Revision 1) uses the same host on the same terms. A true 1 GB VPS stays required for the M9 final run unless the maintainer decides otherwise.
   - *(Still open, **due before M7a**, which since Revision 1 starts right after M4a, because the first pre-dogfood run is part of its exit.)* Which of our apps? Until M7b, the apps must be ones whose data is disposable or backed up elsewhere; say which apps join later.
   - *(Still open, due before M9.)* Which true 1 GB VPS provider for the M9 final run, and which subdomain for the docs site?
7. **Q7 — Minimum Docker version** *(before M1 step 4)*.
   - Engine 25 / API 1.44 excludes Debian 12's `docker.io` (20.10).
   - **Recommendation:** require 25+. Pre-flight explains why and offers (after asking) to install from Docker's official repository.
   - The alternative is supporting API 1.41, which costs an extra CI matrix entry and limits which API features we can use.
8. **Q8 — Can API tokens run destructive actions in Phase 1?** *(before M2)*. **Default: no** (also not sensitive or `SessionOnly` actions). Saying yes would need a separate short-lived `destructive` token scope; not recommended.
9. **Carry-over — confirm vision, features, architecture** *(before M3a)*.

## Decisions that need advisor input

- **A1 — S4 strategy** (Q3).
- **A2 — Build and git-fetch strategy** (M5, ADR 0013): `buildctl` in an on-demand BuildKit container (recommended) vs. the BuildKit Go client vs. go-git.
- **A3 — Self-update handover: closed.** Option (b), restart with a trial boot (ADR 0018, M7b), as `systems` required. SO_REUSEPORT is ruled out by the flock, the single writer and the in-process Caddy.

## Maintainer answers (2026-11-20)

- **Q1:** remote agents are logs only and off by default, as recommended.
- **Q7:** Docker Engine 25+ (API 1.44), as recommended.
- **Branch protection:** yes. `bench-gate` becomes a required check pinned to GitHub Actions, with code-owner review on `.github/`. It is set up once step 1 has added the check.
- **M1 scope:** accepted, including goleak as a test-only dependency, removing `--debug-allow-remote`, the debug unix socket and CODEOWNERS. Start M1.
- **Q2 (2026-11-20):** HTTPS on the server IP with a self-signed panel certificate whose fingerprint is compared against the console. The user can add a panel domain afterwards; once it has a real certificate, the IP address redirects to it.
- **Q8 (2026-11-20):** no. API tokens can't run destructive, sensitive or SessionOnly actions, and can never manage tokens, auth settings or users.
- Q3–Q6 are still open and will be asked before the milestone that needs each one. (Revision 1: Q4, Q5 and the apps-and-VPS part of Q6 are now due before M7a, which follows M4a; Q3 is due at the first S4 tripwire crossing on the CI median, or before M6 at the latest.)

## Maintainer decisions (2026-11-27)

- **Milestone order** (Revision 1): M1, M2, M3a, M3b, M4a, M7a (early usable version), then M7b, M5, M6, M4b, M8, M9. The order is a Phase 1 planning choice and expires at the Phase 1 exit, so it has no ADR.
- **M4 split** into M4a (domains, HTTPS, panel domain, IPv6) and M4b (passkeys, plan 0013), both in Phase 1.
- **Early use and the dogfood clock:** only disposable or externally backed-up apps until M6 (maintainer confirmed 2026-11-27, after the round 3 systems review; originally M7b); the counted window is the last 14 consecutive days before exit, on builds with M5, M6 and M8, updated only through self-update; earlier use is a logged pre-dogfood run. 
- **Accepted risks:** a public panel domain with phishable password + TOTP sign-in from M4a/M7a until M4b, and no out-of-band notice of sign-ins or brute-force back-off in Phase 1. Recorded in `docs/security.md` "Accepted risks (Phase 1)".

## Must be settled before M1 starts (settled 2026-11-20)

- **Q1** (step 5) and **Q7** (step 4).
- The maintainer agrees to make `bench-gate` the required check (pinned to GitHub Actions) after step 1.
- Approval of the test-only `go.uber.org/goleak`.
- Acknowledgement that:
  - M1 ships no user-visible feature and no new HTTP route;
  - it removes `--debug-allow-remote` and moves the debug listener to a 0600 unix socket (`--debug-socket`);
  - it adds `.github/CODEOWNERS` (the maintainer turns on code-owner review);
  - the threat-model review must be done before M2 auth merges.

---

# Review round 1 changes

| # | Review item | Where it was fixed |
|---|---|---|
| 1 | bench-gate fails open | Part B › CI `bench-gate` (`needs.changes.result` required, `if: always()`); `changes` permissions + `GH_TOKEN`; step 1 test |
| 2 | Reuse rule covered one docs push only | Part B › CI reuse rule + induction; step 1 A → B → C test; corrected required-check reasoning |
| 3a | Hub SANs and name check | Part B › Agent channel (`--agent-san`, name check, role marker); ADR 0009 |
| 3b | Re-issue at startup | Part B › lazy renewal in `GetCertificate`; step 5 tests |
| 4 | Audit boot id, partial failures | Part B › Audit log; step 8 |
| 5 | Read-only open path | Part B › Store `OpenReadOnly`; steps 7–8 |
| 6 | Non-interactive actors | Part B › Action registry policy table; Part D Q8 |
| 7 | Background-work inventory | Phase-wide rule 3 table |
| 8 | Footprint slack | Phase-wide rules 2 and 4 |
| 9a–c | Min Docker / admin recovery / Phase 0 leftovers | Part D Q7; M2 `admin reset` + back-off; Part B Phase 0 carry-over and step 2 |
| 10 | Split M3 and M7 | M3a/M3b, M7a/M7b; Part C renumbered |
| S | Suggestions (reflector to M2, per-call deadlines, `Client.Close()`, session cap, concurrency group, `meta.schema_version`, free space, `.github/scripts/`, `:80` redirect) | Part B Docker/goleak/Limits/CI/Store; M2 |

# Review round 2 changes

| # | Item | Where it was fixed |
|---|---|---|
| R1 | "(rule 11)" pointed at a missing rule | M7b Security now cites `docs/security.md` rule 11 |
| R2 | Handover gets its own ADR | ADR 0018 (rule 8 list, M7b, Part D A3) |
| R3 | M1 budget arithmetic | Allocation table, M1 summary, Part B footprint: S2 47.6 + 1.4 = 49.0; S4 44.1 + 0.9 = 45.0 |
| SYS1 | Rollback vs downgrade protection | M7b trial boot step 4 (binary + DB snapshot restored together, trial writes discarded); expand-only rule (step 7); test: migration + failed health check |
| SYS2 | Close A3 as restart; trial boot | M7b (`update.json`, attempts, 3rd attempt/deadline rollback, old process takes the backup, defer during jobs, migration time excluded, `OnFailure=` unit for an early crash); Part D A3 closed; M7a binary in `/var/lib/serverdock/bin/`, noted in ADR 0016 |
| SYS3 | FK cascade on table rebuild | Part B › Store migration runner (pinned conn, FK off → check → on); step 7 test |
| SYS4 | Pre-migrate backup safety | Part B › Store (`.tmp` + fsync + rename + dir fsync + `quick_check`, 2.2× + 64 MiB, `.tmp` wiped on boot); step 7 |
| SYS5 | OpenReadOnly correctness | Part B › Store read-only open (no pragmas, never creates files, `immutable=1`, "v1 (unadopted)"); rehearse restore uses `immutable`; `doctor --bundle` uses it (M7a); steps 7–8 tests as a different uid |
| SYS6 | M3a interrupts and commit order | M3a "plan 0004 must" (roll back on boot, commit order, non-cancellable switch, supersede = unhealthy, names with deploy + attempt, mutex + worker limit, pull deadlines, routes from DB) |
| SYS7 | Upstream IPs change | M3a (regenerate on start events, boot and events reconnect; daemon-restart test; 127.0.0.1 alternative weighed in plan 0004) |
| SYS8 | M4 last-known-good | M4 (per-route validation, failing routes left out with `error`; boot from generated config) |
| SYS9 | Audit resolution across boots | Part B › Audit (later-boot outcome resolves `unknown`, `intent_id` index); M3a (no-op reconciler passes not audited); step 8 |
| SYS10 | Real restore path | M7b `serverdock state restore` (console, flock); state backups scheduled daily on the housekeeping job; rehearsal in `DATA/tmp` after a space check; `DATA/tmp` wiped on boot (Part B Store); Out of scope CLI list |
| SYS11 | Background-work rows | Phase-wide rule 3 table (all listed rows; one shared scheduler timer; daily tasks merged into one housekeeping job; certificate expiry via certmagic events); M8 updated |
| SYS12 | VPS bench at M7a exit | M7a section; Part C exit table (M7a + M9); Q6 timing |
| SYS13 | SAN default picks up bridge IPs | Part B › Agent channel: `--agent-san` required with `--agent-listen`; step 5 test |
| SYS opt | Two metric tiers; update backup replaces pre-migrate backup; fixtures per milestone | M8; M7b step 6 + Part B Store; Part B Store fixtures |
| SEC1 | Remote agents must not get writes or secrets | Phase-wide rule 9; Part B › Local-only ops; step 5 test; Q1; Out of scope |
| SEC2 | Registry policy hardening | Part B › Action registry (scope hierarchy + `Register` panics; `SystemInvoker` handle + import test + route-matrix check; `SystemMutate`/`SystemDestructive` with reason; `webhook` kind); M5; M9; step 9 |
| SEC3 | Audit secrecy | Part B › Registry (free-form fields refused unless tagged); Audit (`error` column + logs redacted; sensitive reads intent-first); M3b reveal; steps 8–9 |
| SEC4 | Compose rules | M6 "plan 0008 must"; M3a label rule |
| SEC5 | Signing and update | M7a (manifest binds version/arch/artifact; key set + rotation; protected `release` env; install refuses without `--no-verify`); M7b (downgrade refusal; backups always argon2id-AEAD encrypted; restore rules); Q5 |
| SEC6 | Panel TLS separate from hub CA | M2 (self-signed leaf, leaf fingerprint, no CA install); Part B (`MaxPathLenZero`, clientAuth-only for Phase 2, CA signs hub/clients only); Q2 |
| SEC7 | Renewal failure | Part B › lazy renewal (keep current leaf, 1 min back-off, log once, SNI ignored); step 5 test; rule 3 row |
| SEC8 | Token strength, check ordering | Part B › Agent token and hub listener check order; steps 5–6 tests |
| SEC9 | M2 login | M2 (/64, ±1 step + last counter, per-account slowdown + notify, bounded semaphore wait + fast 503, per-IP check before hashing); Part B limits /64 |
| SEC10 | API tokens | M2 (fresh-2FA session to create/rotate, audited; `SessionOnly`; header only; revoked on password/2FA change); registry `SessionOnly` |
| SEC11 | Setup token | M2 (≥ 128 bits, constant time, POST only, 0600 file with only its path printed, fingerprint check first) |
| SEC12 | Debug listener | `--debug-allow-remote` removed in M1 step 5 (bench uses a netns helper, so the shipped binary is still measured); route matrix includes the debug routes (rule 1, M2); M7a confirms |
| SEC13 | CI reuse source | Part B › CI (`gh run list --workflow ci.yml`, required check pinned to GitHub Actions, env-only inputs); rule 11 (never `pull_request_target`) |
| SEC14 | Outbound-URL policy | Phase-wide rule 10; M5 (introduced, BuildKit entitlements); M6 S3; M8 notifications |
| SEC opt | Head hash + row count; denied-row rate limit; backup perms; Caddy XFF test; 64 KiB window; op line caps; systemd hardening; `audit.verify` rate limit | Part B Audit/Registry (`system.info`), Audit (denied limit), Store (0700/0600), M2 (XFF test, REST rate limit), Part B Limits (64 KiB), rule 7 + M3a (line caps), M7a (unit hardening) |

Some round 2 entries were changed again in round 3: the expand-only rule, the `OnFailure=` unit and the split system flags. See below.

# Review round 3 changes

| # | Item | Where it was fixed |
|---|---|---|
| S1 | Jobs must not turn user actions into system actions | M3a "Jobs never turn a user action into a system action" (actor stored, policy at enqueue and per action, `system` only as executor, plan 0004 token test); Part B audit `executor` column; Part B Actors (`SystemInvoker` only for untriggered work, executor marker); step 9 test |
| S2 | Rule 10 block list | Phase-wide rule 10 (host interface IPs incl. public, Docker gateways, RFC 1918/CGNAT/ULA by default, per-target audited admin allowlist) |
| Y1 | `Type=notify` | M7a unit line (`READY=1` after health, stdlib sd_notify); M7b trial boot steps 3–5 (`TimeoutStartSec` = deadline, `EXTEND_TIMEOUT_USEC` during migrations, timeout counts as an attempt, attempt 3 rolls back); tests |
| Y2 | Self-check, drop `OnFailure=` | M7b step 2 (`serverdock.new version --self-check`), step 3 (attempt counter is the first action); `OnFailure=` removed with the reason; tests |
| Y3 | Snapshot restore order | M7b step 6 (close → delete `-wal`/`-shm` → rename → dir fsync), used by rollback and `state restore`; kill-between-steps test |
| Y4 | Trial must not act on Docker | M7b step 4 (no worker/scheduler until `done`; reconciler regenerates routes only); step 1 (scheduler paused before the no-jobs check); tests |
| Y5 | Unattended encrypted backups | M7b "State backup archives" (argon2id → X25519, only the public key stored, `crypto/ecdh` + HKDF + AES-GCM, header holds params, salt and ephemeral key); local rollback snapshot kept separate; test |
| Y6 | `state restore` details | M7b (not through the registry; `system` row appended to the restored DB with head hash + row count; newer-schema archive refused); Part B Actors note; test |
| Y7 | Outbound-URL enforcement | Rule 10 (custom `DialContext` on the dialed IP); M5 (pre-check inside the BuildKit namespace + documented gap); BuildKit entitlements moved to M5 Security |
| opt | Drop expand-only rule | Removed from M7b (rollback always restores the snapshot) |
| opt | One `System string` | Part B registry `Def`, `Register` rules (CI check + `docs/security.md` list for destructive ones), policy table; step 9 |
| opt | Per-IP connection cap in `Accept` | Part B Limits (HTTP servers); step 6 test |
| opt | Rollback audit row | M7b step 7; test |
| opt | Debug listener on a 0600 unix socket | Part B Debug listener (`--debug-socket`, bench helper via shared volume); M2 route matrix; M7a; step 5 test; must-settle list |
| opt | CODEOWNERS on `.github/` | Step 1; must-settle list |

# Review round 4 changes

| # | Item | Where it was fixed |
|---|---|---|
| Sys 1 | `update.json` terminal states and version check | M7b "`update.json` state machine" (phases `trial` → `done` / `rolling_back` → `rolled_back`, atomic writes before each guarded step, acts only on `phase == trial` and `to` == own version, the `to` binary finishes an interrupted rollback, the `from` binary writes the audit row, terminal files archived, stale files ignored); steps 3, 4, 5, 6 (repeatable: snapshot gone + `rolling_back` = done), 7, 8; tests (old binary doesn't roll back again; a stale file doesn't suppress the pre-migrate backup; a kill between each phase write) |
| opt | Stop writes before the snapshot | M7b step 2 (503 for writes; API, webhooks and scheduler paused); test |
| opt | Free-space check for the rollback snapshot | M7b step 2; test |
| opt | Delete the snapshot at `done` | M7b step 4; test |
| opt | `doctor --bundle` exclusions | M7a (`DATA/backups/` and `DATA/agent.token` added) |
| opt | Executor marker only via the handle | Part B Actors (`inv.InvokeAs(actor, …)`, not an option field) |
| opt | Debug socket in a 0700 directory | Part B Debug listener |

# Revision 1 changes (2026-11-27): milestone reorder

| # | Change | Where |
|---|---|---|
| 1 | New run order; reason (first real use sooner, exit not earlier) | "Revision 1" section at the top; Overview (run column); "Why this order (Revision 1)" |
| 2 | M4 split into M4a (plan 0006) and M4b (plan 0013) | M4a and M4b sections; Overview; rule 8 (passkeys ADR in M4b); Part C rows 7, 7a, 17; "How to read" (12 milestones, plans 0003–0013) |
| 3 | Early usable version at the M7a exit; early-use data rule until M7b | Overview; M7a (operating rules); M7b goal; Part C dogfood row |
| 4 | Re-running `install.sh` upgrades in place, keeps `DATA`, never overwrites config | M7a key decisions, security, tests, exit evidence; Part C row 20 |
| 5 | Pre-M7b operating rules (stop, copy `DATA` incl. `DATA/keys/master.key` off the box, prune images by hand) | M7a; M8 (cleanup replaces hand pruning) |
| 6 | Daily housekeeping job moved from M6 to M7b; cron stays in M6 | Rule 3 text and table; M6 packages and scheduling; M7b order and housekeeping bullet; M8 |
| 7 | State archives local only until M6; UI says to copy them off by hand; local pruning | M7b "State backup archives"; M6 (S3 as a state-archive target); Part C row 20 |
| 8 | Update quiesce check is generic over running jobs; plans 0007/0008 test deferral during a build, backup and restore | M7b step 1 and tests; M5 and M6 tests |
| 9 | VPS bench: M7a run no longer covers compose-go, S3, BuildKit; re-run at the M6 exit | M7a; M6; Part C exit table (M7a + M6 + M9) |
| 10 | Footprint table in run order; S2 total unchanged (75); S4 re-based on M3a's 47.6 MB, S4 headroom called out as the tightest constraint | Footprint allocation section; per-milestone footprint lines (M3b, M4a, M4b, M5, M6, M7a, M7b, M8, M9) |
| 11 | Dogfood clock redefined; pre-dogfood runs | Revision 1 section; M7a and M7b exit evidence; M8; M9; Part C exit table |
| 12 | Questions now due earlier | Part D Q3, Q4, Q5, Q6; maintainer answers note; "Maintainer decisions (2026-11-27)" |
| 13 | Cross-references to the old order | M2 (`:80` until M4a); M4a (panel domain is the later RP ID); M8 (alerts rely on M4a and M6, both earlier); M5 (still after M4a for the GitHub App); `docs/architecture.md` (passkeys: M4b, plan 0013) |
| R1 | Reviewer REVISE: rebased on M3a (Rule 3 Caddy-ticker exception, ADR renumbering 0012–0018, Q6 partial answer kept) | Rule 3; rule 8 (passkeys ADR = next free number); Q6; M6 re-run and M7a bench on the Hetzner test server, not a true 1 GB host; Part C bench row |
| R2 | S4 metric is the median of per-run medians; VmHWM is a watch figure | Footprint allocation (M3a row, "Which number is compared", headroom 2.4 MB at M3a and 0.5 MB at M9); M3b; M8; Q3 |
| R3 | Update quiesce excludes its own job; housekeeping defers it; message names the blocker | M7b step 1 and tests |
| R4 | NITs: downgrade flag rules and same-version repair; `master.key` copy kept like a password; archive pruning only after a verified write, `.tmp` not counted, "no state backup" with no passphrase; reinstall warns on running jobs | M7a key decisions and tests; M7b "State backup archives" and tests |
| R5 | Maintainer decision 2026-11-27: two accepted risks (public panel domain with phishable sign-in until M4b; no out-of-band sign-in or back-off notice in Phase 1) | Revision 1 section item 4; `docs/security.md` "Accepted risks (Phase 1)"; M4a (panel domain optional); M7a operating rules (SSH tunnel or firewall allowlist); Part D decisions |
| R6 | Footprint (blocking): M3a base is cloud-container, not CI; CI median owns the tripwire; re-base once on PR #6's first CI numbers | Footprint allocation ("Provisional M3a base", M3a row, headroom text); M3b; Q3 |
| R7 | Footprint NITs: step judged by full-run median plus cause; S3 client chosen on `inittrace` and gzip delta, upload location and multipart bound; cut-down loaded S2 run at M7a; remote nodes with domains cost more than empty S4; VmHWM likely past 50 from M3b; M8 collector buffer caps and full S4 run; M4a full S4 run past 0.5 MB QUICK; page cache not comparable on the bench host; argon2id caps on restore; M6 and M8 are both planned S4 increases | Footprint allocation; M4a; M6; M7a VPS bench; M7b state backup archives; M8 |
| R8 | Systems B1: boot-pass default rule, update job outcome from `update.json`, quiesce counts only current-boot jobs | M7b "The update job and the boot pass", step 1, tests |
| R9 | Systems B2: one upgrade path after M7b; upgrade mode refuses during `trial`/`rolling_back`; the M7a → M7b hop rewrites the unit | M7a "After M7b there is one upgrade path"; M7b tests |
| R10 | Systems B3: `EXTEND_TIMEOUT_USEC` from M7a | M7a key decisions and tests |
| R11 | Systems should-fix and NITs: M7b replaces the `DATA`-copy rule, app-data rule stays until M6 (maintainer confirmed 2026-11-27); bounded wait and terminal `deferred` state; boot-pass wording; dogfood clock details; 48 h archive age alert; `df -h` rule; reinstall-upgraded fixture DB | Revision 1 item 3; Overview; M7a; M7b; Part C dogfood row; Part D decisions; `docs/roadmap.md` |
| R12 | Security B1/B2 and N1–N7: `--no-verify` refused in upgrade, repair and downgrade; verification through the installed binary; one encrypted streaming `DATA` copy command and what the copy holds; raw copies replaced and destroyed after M7b; `/dev/tty` prompt; hashed bytes are renamed bytes; `--downgrade-to` with audit row; "update deferred" in needs attention; plan 0006 redirect, break-glass, certmagic events, HSTS defaults; passphrase strength | M7a; M7b; M4a |
| R13 | Reviewer NITs: verification only during the write; M6 and M8 both planned S4 increases; Q3 "before M6 at the latest" in the answers note; downgrade and repair bullets moved into upgrade mode | M7b archives; Footprint allocation; Part D; M7a |
