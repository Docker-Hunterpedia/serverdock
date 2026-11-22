# Plan 0004 — Phase 1 M3a: deploy from image, safely

**Status:** approved — 2026-11-21 (reviewer, systems, security, footprint); QM3a-1 confirmed by the maintainer 2026-11-21; implementation open · **Author:** planner (AI) · **Date:** 2026-11-21 (revisions 2 and 3: 2026-11-21)

**Roadmap:** Phase 1, milestone M3a of [plan 0002](0002-phase-1-mvp-solo.md). It serves these roadmap items (plan 0002 Part C numbering): **4** "deploy from image" (the image part; Dockerfile is M5, Compose is M6), **6** "blue-green handover, health gate, automatic rollback, one-click rollback from deploy snapshots; auto-cancel superseded deploys" (all of it), **14** "stop-first strategy; connection draining on blue-green" (CPU/RAM limits are M3b), **15** "keep logs of the last N deploys; live pull progress; restart and redeploy-without-rebuild" (context variables are M3b, build progress is M5), and **23** "accessible, phone-usable UI" for the new screens. This plan is AI-written; the maintainer's answers are recorded in [Maintainer answers](#maintainer-answers-2026-11-21).

**Binding inputs:**
- plan 0002: phase-wide rules 1–11, the M3a section (every "plan 0004 must" line; listed in [Traceability](#traceability)), the footprint allocation, and the maintainer answers Q1, Q7, Q8;
- plan 0003: the "Open items" item 3 obligations that target plan 0004 / M3a and the round-2 notes for plans 0004–0006 (listed in [Traceability](#m2-hand-off-obligations));
- [ADR 0011](../decisions/0011-footprint-ceiling.md): ceilings S2 200 MB / S4 100 MB, tripwires 80 / 50 MB; ADR 0009 (agent channel, `RemoteOK`), ADR 0010 (panel TLS and auth), ADR 0004 (embedded Caddy), ADR 0005 (SPA);
- [`docs/security.md`](../security.md) Draft 3: rules 1–13, the A4 rows "Planned (M3a policy)", G13 (log content is attacker-controlled), the SEC6 accepted risk;
- the M2 code on `main` (3724264): `internal/actions` (`Authorize`, `SystemInvoker.InvokeAs`, `ConsoleOnly`), `internal/agent` (op table, `logs`, the error reply in `session.go`), `internal/agent/dockerapi` (`call`/`stream`, negotiation), `internal/agent/proxy` (panel config), `internal/hub` (`Session.open`, stream caps), `internal/store` (migrations 0001–0003, `foreign_keys(1)`), the route matrix and the Playwright + axe harness (8 projects);
- the [maintainer answers](#maintainer-answers-2026-11-21) to QM3a-2 to QM3a-9 (2026-11-21).

**How to read this plan.** Same parts as plan 0003: goal, out of scope, design, footprint, security notes, steps, test plan, reviews, then the maintainer answers, [risks](#risks), [traceability](#traceability) and **Appendix A, ADR 0012** (Docker client and agent op protocol v1), which step 1 copies to `docs/decisions/`. The [Revision 2](#revision-2-changes) and [Revision 3](#revision-3-changes) tables map every review finding to where it landed.

## Revision 2 changes

Reviewer, systems and security returned CHANGES NEEDED on revision 1. Each finding and where it is now:

| # | Finding | Where it landed |
|---|---|---|
| B1 | `deploys.job_id` FK blocks job pruning (`foreign_keys(1)`) | Data model: `ON DELETE SET NULL`; step 4a `TestFinishedJobsPrunedOnInsert` with a referencing deploy row |
| B2 | Reused row ids could let a new service adopt an old container | Data model: `AUTOINCREMENT` on `projects`, `services`, `deploys`, `jobs`; Concepts: the 409 path also requires the container's image id = the snapshot's resolved `image_id`; step 4a `TestIDsNeverReusedAfterDeletingTheNewestService`, step 3 test |
| B3 | Route regeneration races (lost updates) | App routes › one global route lock across read → build → `proxy_apply` → hash; every caller listed; step 6 `TestConcurrentRouteChangesForTwoServicesLoseNothing` |
| B4 | Job deadline could cut the switch/commit; unbounded stages | Deadlines and contexts (new section): pre-switch deadline only; switch/commit on `WithoutCancel` with their own bounds; drain, stop_old and re-apply bounds; the job deadline derived from the settings; step 8b `TestDeadlineNeverCutsSwitchOrCommit` |
| B5 | Container IP reuse after a dockerd restart → misrouting | Concepts › App network and addresses: a static IPv4 per container from our own subnet, recorded on the deploy row, set through `IPAMConfig`; Policy: `CheckCreate` allows exactly that key and value; boot applies app routes only after the first list + inspect; steps 2, 3, 5, 6, 10 |
| B6 | Orphan removal could remove a running deploy's container | Reconciler › Resync rules: skips services with a running job (checked under the service lock), protects only the current attempt, runs on every events reconnect; step 10 tests |
| B7 | Boot pass and resync did container work in two places | Reconciler: the boot pass changes DB state only; all container work is one resync rule set run on every successful (re)connect; the scheduler waits for the first successful resync; the UI shows ◌ unknown meanwhile; step 10 |
| B8 | Delete vs the service lock and the 5-minute fresh-2FA window | Restart, …, delete: cancellation happens when `services.delete` queues; a queued delete skips the rest of the drain; if the window still expires the job fails with "confirm again" (tested); step 9 test wording fixed |
| B9 | Apps are offline while ServerDock is down (embedded Caddy) | Goal 4 reworded ("reachable again after a restart"); QM3a-9 accepted risk; Concepts container settings reworded; step 10 measures the gap |
| B10 | `write` scope = running arbitrary code with the host's network reach | Security notes › accepted risk in plain words; step 15 adds it to `docs/security.md` |
| QM3a-2…9 | Maintainer answers | [Maintainer answers](#maintainer-answers-2026-11-21); actions table (narrowed token powers, `services.set_source`, `services.set_public_port`), public-port confirm dialog (step 13), accepted risk QM3a-9 |
| N1 | Pin the image by id | Concepts › Container settings (`Image: sha256:…`); `CheckCreate` asserts it equals the checked image; step 5 |
| N2 | `CheckCreate` to the leaves; run it in the agent; agent sets the instance label | Policy › `CheckCreate`; ops table (`container_create`); steps 3, 5 |
| N3 | Ownership ops: full 64-hex ids, act on the inspected id | Ops table; step 3 test |
| N4 | `health_check` refuses network, gateway, broadcast | Ops table; step 3 test |
| N5 | Protocol: trailing bytes, `hello` cap, version string, `RemoteOK` independent of `hello`, rule 9 update, v0 detection on real M2 bytes, no `follow` to v0 | Agent op protocol v1; step 1 (incl. plan 0002 rule 9 and `docs/security.md` updates) |
| N6 | Resource caps: `PidsLimit`, max services, disk check before pull, `no-new-privileges`, OOM score note for M7a | Concepts › Container settings; Limits (new subsection); Traceability notes for plan 0009 |
| N7 | Registry SSRF text overclaimed; loopback registries | Security notes › Registry hosts (rewritten); `--allow-loopback-registry` opt-in |
| N8 | Deploy logs can hold generated passwords; bind plan 0005 to a sentinel test | Deploy logs; Traceability notes for plan 0005 |
| N9 | Explicit session revoke should stop that session's jobs | Jobs run as the actor that queued them; step 4b test |
| N10 | SEC6 route-matrix case; "services can't reach each other" wording | SEC6; Concepts › App network; step 7 |
| N11 | Rollback targets | Restart, … › Rollback: only deploys that were once live (`committed_at`); dialog says so |
| N12 | `system.exposure` shows foreign container names to `read` tokens | Security notes; step 15 security rows |
| N13 | "Bridge IPs reachable from the host only" overclaimed | Upstream addressing table softened; accepted risk; step 15 researcher item |
| N14 | Amend plan 0002's interrupt rule; `finish_deploy` reason; SIGTERM in drain | Reconciler; actions table; Shutdown; step 15 amends plan 0002 lines 281 and 297; ADR 0012 decision 7 |
| N15 | Step 8 must not depend on step 10's watcher for IPs | Step 8a (the runner inspects its candidate itself) |
| N16 | Split steps 4 and 8; v4.db and `install_id` earlier | Steps 4a/4b, 8a/8b; `meta.install_id` inside 0004; `v4.db` in step 4a |
| N17 | Stop-first: cancellable column, cancel after the old stopped, maintenance override | Stop-first table; override map variants |
| N18 | Caddy ticker as a rule 3 exception; events `Timeout` 0; hash dedupe test | Background work rows (exception, footprint sign-off); ops table; step 6 `TestCrashLoopingContainerCausesNoReload` |
| N19 | Docker hung → state unknown | Reconciler › In-memory state |
| N20 | Disk full rules | Failure rules (new subsection) |
| N21 | Drop `jobs.state = unknown`; route status in memory | Data model |
| N22 | Events seen during the list | Reconciler › Watcher (re-inspect buffered ids) |
| N23 | `drain_s` vs Caddy grace; `proxy_apply` timeout | Data model ranges (`drain_s` ≥ 10); ops table (`proxy_apply` 20 s > 10 s grace) |

## Revision 3 changes

The four re-reviews (reviewer, systems, security, footprint) confirmed the revision 2 blockers resolved and asked for:

| # | Finding | Where it landed |
|---|---|---|
| R1 | The allocator could hand out an address a stopped container still owns (`network_inspect` lists only connected endpoints) | Concepts › App network and addresses (the "held" rule, allocation at `check` under one allocation lock after the start gate, `IPRange` slice, `ip_pool_exhausted`); Data model (`deploys.ip` nullable, partial unique index `deploys_ip_live`); App routes (an upstream only when the state map shows the service's own container running at that address); Policy and ops table (the agent checks the address is a host address of `meta.app_subnet`); step 8a tests `TestAllocatorNeverReusesAStoppedOrActiveContainersAddress`, `TestTwoConcurrentChecksNeverGetTheSameAddress`, `TestForcedAddressCollisionNeverRoutesToAnotherService` |
| R2 | Resync could block on a service lock | Reconciler › Resync rules (`TryLock`, busy services skipped and covered by the per-service resync after each job); new "Lock order" paragraph (service lock → allocation lock → route lock, never reversed; the watcher takes only the route lock); step 10 tests |
| R3 | The bench never measured the watcher (no Docker socket in S2) | Footprint › Bench changes: "S2 live watcher" (socket mounted with its group) is the number gated against the 80 MB tripwire, "S2 no Docker" kept; bench clean-up removes `serverdock.managed` containers; first watcher measurement in step 10; "S2 with one exposed service" gated against the tripwire; an S2 run with 10 exposed idle services (RSS and idle CPU) in step 15 |
| N-a | Label count differs between control plane and agent | Policy › `CheckCreate` (control plane: five keys; agent: six after it adds `serverdock.instance`) |
| N-b | Stop-first "restart the old container" missing from the deadlines | Deadlines table (fresh context, `health_timeout_s` + 30 s; included in the stop-first `timeout_s` sum) |
| N-c | Watchdog wording inconsistent | Deadlines: `timeout_s` is the whole-sum watchdog, checked only before the switch |
| N-d | "Anonymous volumes accumulate" contradicts QM3a-3 | Security notes: only images accumulate until M8 |
| N-e | "Same repository" needs normalised references | Actions table and Policy (`policy.SameRepository`: `docker.io/library` default, host case, default port); step 5 table and fuzz cases |
| N-f | Subnet overlap later; hand-attached containers; pool size | Concepts › App network (`network_ensure` refuses on overlap with "set --app-subnet", deploys refuse, running apps unaffected; `IPRange` dynamic slice; `ip_pool_exhausted`; a /24 gives 190 static addresses; the count bound is corrected in Revision 4); note for plan 0009 (doctor check) |
| N-g | Footprint wording and bounds | Background work exception row (at most 100 tickers, only for services with an upstream route, torn down on every reload; the text also goes into plan 0002 rule 3 in step 6); idle CPU cell; one S2 estimate; deploy-log bound uncompressed, streamed gzip, `LimitReader`; step 15 RSS 60 s after a deploy; Services list and Overview don't poll, deploy detail backs off to 5 s after 2 minutes; reload cost named; deploy rows: last 200 per service kept |

## Revision 4 (post-approval nits)

- **Address count:** the bound is ≤ 100 active deploys + 2 candidates (queued deploys hold no address; at most 2 workers) + a few failed-removal leftovers, well inside 190 (Concepts, Limits).
- **Gateway:** the app network is created with `Gateway` explicitly `.1`; step 2 tests it.
- **State on `die`:** inspect the container (event-driven, no timer) and take restarting/exited from `State.Restarting`/`Running`, not event order (`docker stop` emits kill → die → stop); a restarting container keeps its upstream; step 10 tests `TestDieThenStopEndsExited` and `TestDieWithNoRestartIsReportedExited`.
- **Allocator source:** a fresh `container_list` (all states) at each allocation under the allocation lock, not the state map; Docker unreachable → the deploy fails `docker` (transient, retried) (Concepts, steps 4a and 8a).
- **Footprint wording:** "about 1.7 MB (≈ 52 MB)" in the text matches the table; step 10's PR reports the "S2 live watcher" number.

## Revision 5 (step 3 implementation notes)

Recorded after step 3 and its reviews; later steps follow these.

- **Wiring is deferred to step 4a.** `agentops.Handlers` needs `meta.install_id`, which migration 0004 creates. Step 4a wires `agent.Config{Ops: …}` with `Deps{Instance, CheckCreate: InterimCheckCreate}` in `runServer`, with a test that the local agent serves the ops and a remote agent refuses them as unknown.
- **Result fields beyond the table:** `container_inspect` adds `running`, `restarting`, `configured_ip`, `owned`; `container_list` adds `unowned` per entry and is **never partial** (more than 1,000 containers or over the size cap fails `conflict`); `container_ports` adds `truncated` and a per-container `ports_truncated`; `network_inspect` adds subnet, gateway and range and fails `conflict` over 1,024 addresses (step 4a maps it to `ip_pool_exhausted`); `events` sends one progress line when Docker's stream is open (`Client.Events` `onReady`), so step 10 opens the stream before it lists.
- **Chosen limits:** `container_ports` keeps 4 distinct ports per container (IPv4/IPv6 duplicates merged, all-interfaces first); `health_check` counts HTTP 200–399 as healthy, refuses the dynamic range as well as network, gateway and broadcast, and caps response headers at 16 KiB; stop and restart ops time out at 390 s; `image_pull` at 30 min with `stall_s` 1..600; `network_inspect` MaxResult 64 KiB; `container_create` refuses an image with any `serverdock.*` label, an image id it cannot inspect, and a name whose deploy and attempt differ from the labels.
- **Step 8b:** the `stop_old` runner bound should be `stop_grace_s + 45 s` (or rely on the op timeout), with `timeout_s` updated.
- **Step 8a:** failure clean-up after a timed-out create looks the container up by its deploy and attempt labels (via `container_list`), not by the id it never received.
- **Steps 6 and 8a:** trust `container_inspect` addresses only when `owned` is true and the labels match the expected service and deploy.
- **Step 5:** deletes `InterimCheckCreate` when `policy.CheckCreate` lands; the new check must refuse case-variant and duplicate keys (the interim one refuses duplicates) and keep the fuzz test.

## Entry gate

1. **Phase 0 carry-over (plan 0002 Part D item 9, roadmap Phase 0): the maintainer confirms vision, features and architecture before M3a starts** (QM3a-1: confirmed by the maintainer, 2026-11-21).
2. This plan approved by `reviewer`, with `systems` (required for M3a) and `security` sign-off on the design.
3. Plan 0002 Q3 (S4 headroom) is closed by ADR 0011 (S4 ceiling 100 MB, slim agent stays in Phase 2).

## Goal

1. **Deploy a public Docker image as a service** on the local server, from the panel or the REST API, as a queued Job with an honest visible state (rule 5).
2. **Blue-green by default, stop-first on request**, with a health gate (HTTP, TCP or "stays running") from the agent. **Automatic rollback:** an unhealthy candidate is removed and the old version keeps serving, with zero non-2xx responses.
3. **Fixed commit order** (start → health → switch → commit → drain → stop old). The old container is stopped only after the commit. A superseding deploy cancels the older one, and a supersede during the health gate counts as an unhealthy result.
4. **A ServerDock failure never leaves an app broken:** after a kill -9 at any stage, a ServerDock restart, a Docker daemon restart or a dropped events stream, the app is **reachable again once ServerDock is running**, through routes generated from DB desired state. **While ServerDock itself is down, apps with a public port are unreachable**, because Caddy runs in the ServerDock process (accepted for Phase 1, QM3a-9; the containers keep running). Step 10 measures that gap.
5. **One-click rollback** to any earlier deploy that was once live (an immutable snapshot pinned to an image id), **redeploy without rebuild**, **restart**, **cancel**, **delete** (destructive, fresh 2FA, typed confirmation).
6. **Deploy logs of the last 10 deploys** per service (gzip, capped) and **live pull progress**.
7. **The basic UI**: Services list, New service (image only), Service detail with deploy history, Deploy detail with live stages, and Overview with service states. It passes Playwright + axe in the 8 projects.
8. **Footprint:** S2 stays under the 80 MB tripwire (planning number ≤ 56 MB, plan 0002 allocation), S4 unchanged (≤ 45.5 MB planning number); no new idle timers of ours.

## Out of scope

- **Env vars and secrets, private registry credentials, volumes, per-project networks with service DNS, CPU/RAM limits, context variables, the Docker version-change warning, the Docker RC CI matrix** (M3b). M3a containers get no user env, no mounts, no CPU/RAM limits (only the PID limit below).
- **Automatic stop-first when a writable volume is attached** (M3b). In M3a, stop-first is a setting the user picks.
- **Domains, ACME, HTTPS for apps, IPv6** (M4). Apps are reached on a plain-HTTP public port (QM3a-2), IPv4 only.
- **Git builds, webhooks, the outbound-URL policy (`internal/netpolicy`)** (M5).
- **Compose, databases, backups, cron schedules and the daily housekeeping job** (M6). The scheduler is built so M6 can add cron to the same timer.
- **Image and container cleanup, disk alerts, metrics, live logs, alerts** (M8). Old images stay on disk; deploy logs are the only retention M3a does.
- **Keeping apps reachable while ServerDock is down** (QM3a-9; revisited with M7b self-update).
- **WebSocket routes** (QM3a-6). Live progress polls `deploys.get`.
- **Release phase / pre-deploy commands, replicas, processes, custom command or entrypoint** (Phase 2).
- **Remote-agent deploys.** Every new op is `LocalOnly` (rule 9).
- **Project management** beyond a name (QM3a-5).
- **A web terminal, exec, or one-off commands.**
- **Changing the app subnet after first start.**

## Design

### Overview of the deploy path

```
browser / token ──REST──▶ services.deploy (mutate, write)  ─ Authorize + validate + plan
                              │ one tx: deploy row (snapshot; address allocated later, at check) + job row; queued
                              │ jobs of this service cancelled as "superseded"; running one flagged
                              ▼
                      jobs scheduler (one goroutine, one "next due" timer, ≤ 2 workers,
                      one running job per lock key "service:<id>"; starts after the first resync)
                              │ SystemInvoker.InvokeAs(stored actor, "jobs.run_deploy")
                              ▼                 (policy re-checked now; audit executor=system)
                      internal/deploy runner ──agentops──▶ hub.Session.Call (op protocol v1)
                              │                                  │ in-memory pipe, LocalOnly
                              │                                  ▼
                              │                        local agent: dockerapi (pull, create,
                              │                        start, inspect, health, stop, remove,
                              │                        events) and proxy_apply (embedded Caddy)
                              ▼
                      DB: deploys, deploy_stages, services.active_deploy_id (the commit)

Docker events (one stream, label-filtered) ──▶ watcher ──▶ resync rules + state map
                                                            ──▶ applyRoutes (one global route lock)
```

### Concepts, names and ownership

- **Project** → **Service** → **Deploy**. Environments are Phase 2. A service in M3a is one container from one image. **At most 100 services per install** (`services.create` refuses the 101st).
- **Container name:** `sd-<project>-<service>-d<deploy id>-a<attempt>` (plan 0002). Project and service names match `^[a-z][a-z0-9-]{0,29}$` and may not end in `-`. Deploy ids are `AUTOINCREMENT`, so a name is never reused across services, even after the newest service was deleted. A repeated create of the same name gets Docker's 409; the agent then inspects it and returns "already created" **only if its labels match this install, service, deploy and attempt, and its image id equals the snapshot's resolved `image_id`**. Otherwise the deploy fails with "a container ServerDock does not own uses this name", and that container is never touched.
- **Labels on every container we create** (and only ours): `serverdock.managed=1`, `serverdock.instance=<install id>`, `serverdock.project=<id>`, `serverdock.service=<id>`, `serverdock.deploy=<id>`, `serverdock.attempt=<n>`. The **agent** sets `serverdock.instance` itself and refuses a create request that carries it or any label outside the other five. **`install id`** is 16 random bytes (hex) written by migration 0004 (`lower(hex(randomblob(16)))` into `meta`). Two installs on one Docker host never touch each other's containers.
- **Ownership rule ("only labelled resources are ever deleted"):** the agent refuses to stop, restart or remove a container whose `serverdock.instance` label is not ours (`refused_unowned`). Those ops accept only a full 64-hex container id, inspect it, and act on the inspected id. The reconciler only removes containers with our instance label. A control-plane bug can't delete a foreign container.
- **App network and addresses.** One bridge network `serverdock-<install id[:8]>`, labelled with our instance, IPv4, **created with an explicit subnet** so containers can get static addresses (Docker allows `IPv4Address` only on a network with a user-set subnet). At first start the server picks the first `/24` in `10.250.0.0/16` that overlaps no existing Docker network and no host interface or route, and records it in `meta.app_subnet`; `--app-subnet CIDR` (a `/24` to `/20`, inside RFC 1918 or 100.64.0.0/10) overrides it at first start only; it is checked for overlap before it is saved, and an overlap refuses to start. If a network with our name exists without our label, startup logs an error and deploys refuse with that reason. **If `meta.app_subnet` now overlaps a host interface, route or another Docker network** (a VPN added later, say), `network_ensure` refuses with "the app subnet 10.250.0.0/24 now overlaps <what>; changing the app subnet of an existing install is not supported yet (see the plan 0009 doctor check)" (an existing network is not recreated; `--app-subnet` is first-start only, so the message must not tell the user to use it): new deploys refuse with that reason, running apps are unaffected. The network is created with **`Gateway` explicitly the subnet's `.1`** (with `IPRange` set, Docker could otherwise take the gateway from the range) and **`IPRange` set to the top quarter of the subnet** (a /26 of a /24), so a container attached by hand gets a dynamic address from that slice and never takes one ServerDock allocates.
  - **Static addresses.** Each deploy gets a static IPv4 from the rest of the subnet (a /24 gives 190: `.2`–`.191`; a larger `--app-subnet` gives more). At most about 105 are in use at once: ≤ 100 active deploys, ≤ 2 candidates (queued deploys hold no address and at most 2 workers run), and a few leftovers whose removal failed until the next resync. It is **allocated at the deploy's `check` stage**, after the scheduler's start gate (so after the first resync has filled the state map), **under one allocation lock**, recorded in `deploys.ip` (NULL until then) and set at create through `EndpointsConfig.<net>.IPAMConfig.IPv4Address`. No free address → the deploy fails `ip_pool_exhausted`.
  - **An address is held (never allocated) when any of these is true:** its deploy is some service's active deploy; its deploy is queued or running; **a container with our instance label still exists with that configured address**, in any state (from a **fresh `container_list` with `all=1` called at each allocation under the allocation lock**, not the state map, reading the configured `IPAMConfig`, not `NetworkSettings`, so a stopped or exited container still holds its address; if Docker is unreachable the deploy fails `docker`, which is transient and retried); or `network_inspect` shows it in use (an extra exclusion for hand-attached containers). The partial unique index `deploys_ip_live` (data model) makes two non-terminal deploys with one address impossible even if the lock were bypassed.
  - A container keeps its address across container and daemon restarts, so an upstream can never point at another service's container after a dockerd restart; and the route generator emits an upstream only when it sees the service's own container running at that address (App routes). **`com.docker.network.bridge.enable_icc=false`**: containers can't talk to each other directly on this network. They can still reach every service's **public port through the host** like any client, and the host itself. M3b adds per-project networks with service DNS.
- **Container settings we set** (the user can't change them in M3a): `Image` = the **resolved image id** (`sha256:…`), never a tag, so the container runs exactly the image the `check` stage inspected; `RestartPolicy unless-stopped` (containers come back after a daemon or host restart; they are **reachable through Caddy only once ServerDock runs**, QM3a-9); `LogConfig` driver `local` with exactly `max-size=10m` and `max-file=3`; `PidsLimit 4096` (a fork bomb can't exhaust the host's PIDs); the network and static address above; our labels; `ExposedPorts` for the container port. No `PortBindings`: nothing is published on the host; Caddy is the only entry point. **`no-new-privileges` was considered and is not set by default**: it breaks images that rely on setuid binaries (`su`, `sudo`, some `ping` builds); M3b revisits it as a per-service option.

### Upstream addressing: static container IP, not `127.0.0.1` ports (plan 0002 asks to weigh this)

| | Static IP on our bridge network | Publish the container port on `127.0.0.1:<ephemeral>` |
|---|---|---|
| Changes on restart | no (a static address on a user-set subnet survives container and daemon restarts) | the ephemeral host port can change |
| Misrouting risk after a dockerd restart | none (addresses are ours, recorded per deploy) | low, but a port can be reassigned |
| Extra processes on the VPS | none | one `docker-proxy` per published port with the default userland proxy (a few MB RSS each on a 1 GB host) |
| Host surface | the bridge subnet is routed on the host; on Docker Engine before 28, hosts on the same LAN could in some setups route to unpublished container ports (accepted risk below; `researcher` confirms in step 15) | a loopback port per service, reachable by every local user (A5) |
| ServerDock itself in a container | must be on the host or `--network host` (QM3a-7) | same limitation |
| Health gate | the agent dials the same address Caddy uses | the same |

**Decision: static container IPs** on our network. The `127.0.0.1` option costs a `docker-proxy` process per service on the user's 1 GB VPS and adds loopback ports. If ServerDock can't reach the network (it runs in a container without host networking), the health gate fails honestly: "ServerDock can't reach the app network; run ServerDock on the host or with host networking."

### Data model: migration `0004_deploy.sql`

Every column is listed. Once step 4a merges, 0004 is never edited (M1 checksum rule); later M3a changes add `0005_…`. `foreign_keys(1)` is on for every connection, so every foreign key states its delete behaviour.

```sql
projects(id INTEGER PRIMARY KEY AUTOINCREMENT, name TEXT NOT NULL UNIQUE COLLATE NOCASE,
  created_at TEXT NOT NULL) STRICT;

services(
  id INTEGER PRIMARY KEY AUTOINCREMENT,
  project_id INTEGER NOT NULL REFERENCES projects(id) ON DELETE RESTRICT,
  name TEXT NOT NULL,
  image_ref TEXT NOT NULL,              -- desired image for the next deploy (validated reference)
  container_port INTEGER,               -- the port the app listens on; NULL = no HTTP entry (worker)
  public_port INTEGER UNIQUE,           -- host port Caddy serves (QM3a-2); NULL = not exposed
  health_kind TEXT NOT NULL,            -- http | tcp | running
  health_path TEXT,                     -- http only
  health_timeout_s INTEGER NOT NULL,    -- 5..600, default 60
  health_passes INTEGER NOT NULL,       -- consecutive passes, 1..10, default 3 (1 s apart)
  strategy TEXT NOT NULL,               -- blue_green | stop_first
  drain_s INTEGER NOT NULL,             -- 10..300, default 10 (never below Caddy's 10 s reload grace)
  stop_grace_s INTEGER NOT NULL,        -- 0..300, default 10
  active_deploy_id INTEGER REFERENCES deploys(id) ON DELETE SET NULL,   -- THE commit point
  needs_attention TEXT,                 -- NULL, or a reason code (delete_interrupted, ...)
  created_at TEXT NOT NULL, updated_at TEXT NOT NULL,
  UNIQUE(project_id, name)
) STRICT;

deploys(
  id INTEGER PRIMARY KEY AUTOINCREMENT,
  service_id INTEGER NOT NULL REFERENCES services(id) ON DELETE CASCADE,
  number INTEGER NOT NULL,              -- per-service "#128"
  job_id INTEGER REFERENCES jobs(id) ON DELETE SET NULL,   -- jobs are pruned after 30 days
  kind TEXT NOT NULL,                   -- deploy | redeploy | rollback
  source_deploy_id INTEGER,             -- redeploy/rollback: the deploy whose snapshot was copied (no FK: history)
  snapshot_json TEXT NOT NULL,          -- immutable (trigger below)
  ip TEXT,                              -- static IPv4, allocated at the check stage (NULL before)
  image_id TEXT, repo_digest TEXT,      -- resolved after the pull
  status TEXT NOT NULL,                 -- queued | running | succeeded | failed | cancelled | superseded
  stage TEXT,                           -- see the stage tables
  reason_code TEXT, reason TEXT,        -- reason ≤ 512 bytes, cleaned (see "Untrusted text")
  app_safe TEXT,                        -- yes | no | unknown: did the previous version keep serving?
  superseded_by INTEGER,                -- deploy id (no FK: history)
  committed_at TEXT,                    -- set in the commit transaction: "was once live" (rollback targets)
  actor_kind TEXT NOT NULL, actor_id TEXT NOT NULL,
  created_at TEXT NOT NULL, started_at TEXT, finished_at TEXT,
  UNIQUE(service_id, number)
) STRICT;
CREATE UNIQUE INDEX deploys_ip_live ON deploys(ip) WHERE ip IS NOT NULL AND status IN ('queued', 'running');
CREATE TRIGGER deploys_snapshot_immutable BEFORE UPDATE OF snapshot_json ON deploys
  BEGIN SELECT RAISE(ABORT, 'deploy snapshots are immutable'); END;

deploy_stages(deploy_id INTEGER NOT NULL REFERENCES deploys(id) ON DELETE CASCADE,
  stage TEXT NOT NULL, state TEXT NOT NULL,   -- pending | running | ok | failed | skipped
  started_at TEXT, finished_at TEXT, detail TEXT,   -- detail ≤ 256 bytes, cleaned
  PRIMARY KEY(deploy_id, stage)) STRICT;

jobs(
  id INTEGER PRIMARY KEY AUTOINCREMENT,
  kind TEXT NOT NULL,                   -- deploy | restart | service_delete (M6 adds cron kinds)
  lock_key TEXT NOT NULL,               -- "service:<id>"
  state TEXT NOT NULL,                  -- queued | running | succeeded | failed | cancelled
  actor_kind TEXT NOT NULL, actor_id TEXT NOT NULL,
  actor_session TEXT,                   -- the queuing session's public id (user actors from REST)
  actor_blob TEXT NOT NULL,             -- actions.Actor job encoding (never kind "system" for user work)
  confirm TEXT,                         -- the Confirm value of a destructive request
  enqueued_at TEXT NOT NULL,
  payload_json TEXT NOT NULL,           -- ≤ 64 KiB, no secrets (M3b stores references, never values)
  attempt INTEGER NOT NULL DEFAULT 1, max_attempts INTEGER NOT NULL DEFAULT 1,
  run_after TEXT NOT NULL,              -- due time, UTC
  timeout_s INTEGER NOT NULL,           -- derived from the snapshot settings (see Deadlines)
  cancel_requested INTEGER NOT NULL DEFAULT 0, cancel_reason TEXT,
  boot_id TEXT,                         -- the boot that set it running
  request_id TEXT,                      -- "job-<id>-a<attempt>": the audit request id of its run
  created_at TEXT NOT NULL, started_at TEXT, finished_at TEXT, error TEXT
) STRICT;
CREATE INDEX jobs_due ON jobs(state, run_after);
CREATE INDEX jobs_session ON jobs(actor_session) WHERE state IN ('queued', 'running');
CREATE UNIQUE INDEX jobs_one_running_per_lock ON jobs(lock_key) WHERE state = 'running';
INSERT OR IGNORE INTO meta(key, value) VALUES ('install_id', lower(hex(randomblob(16))));
```

- **Not in the DB:** `route_status` and `route_error` (bind failures) live in memory in the route generator and are recomputed at each apply; `services.get` reports them from there.
- **Snapshot** (`snapshot_json`, schema version 1): `image_ref`, `container_port`, `health` (kind, path, timeout, passes), `strategy`, `drain_s`, `stop_grace_s`, and for redeploy/rollback the pinned `image_id` and `repo_digest` of the source deploy. **The public port is a service setting, not part of the snapshot**: a rollback never moves the address users reach the app on. M4 domains follow the same rule.
- **Index scope:** `deploys_ip_live` covers queued and running deploys (the ones racing for an address); addresses of active and stopped containers are protected by the allocator's "held" rule, which reads live Docker state.
- **Retention:** the last **200 deploys per service** are kept (about 2 KB each, so at most about 40 MB for 100 services); older finished rows are deleted lazily when a deploy finishes, **never the active deploy**. Rollback targets are limited to the kept rows. Finished jobs older than 30 days are pruned lazily, at most 100 per job insert (G7 pattern); their deploys keep `job_id = NULL`. `deploy_stages` has at most 10 rows per deploy.
- **Fixture:** step 4a adds `internal/store/testdata/v4.db` with an upgrade test from `v3.db`.

### Agent op protocol v1 (ADR 0012)

Plan 0002: one yamux stream per op, a request line, progress lines, one result line, a `hello` handshake that carries the protocol version, size caps, agent output untrusted.

- **Request** (hub → agent), one line, at most **64 KiB**: `{"v":1,"op":"<name>","args":{…}}`. `args` is decoded strictly per op (`DisallowUnknownFields`); any byte after the JSON object other than the line's `\n` is refused. The existing `logs` op stays v0 (`{"op":"logs",…}`, raw bytes back). It gains an optional `"follow": false` (absent = true), which the hub **never sends to a v0 session** (an M1/M2 agent would refuse the unknown field).
- **Response lines** (agent → hub), each a JSON object ending in `\n`, with nothing after the object:
  - `{"t":"progress","p":{…}}`: op-specific typed struct, **≤ 1 KiB**;
  - `{"t":"event","e":{…}}`: `events` op only, **≤ 1 KiB**;
  - `{"t":"result","ok":true,"r":{…}}` or `{"t":"result","ok":false,"err":{"code":"…","msg":"…"}}`: exactly one, last, **≤ the op's `MaxResult`** (default 16 KiB; `hello` 256 B; `container_list`, `container_ports` and `network_inspect` 512 KiB). Error codes: `invalid`, `not_found`, `conflict`, `refused_unowned`, `policy`, `docker`, `timeout`, `stalled`, `disk_full`, `cancelled`.
- **`OpSpec` grows:** `{Name, RemoteOK, MaxResult, MaxProgressLines, Timeout}`. The zero value stays local-only. `events` has `Timeout = 0` (bounded by its context only).
- **Hub side** (`Session.Call(ctx, op, args, onLine)`): a capped line reader; strict decode, trailing bytes refused; an over-long line, an unknown field, a second result, a missing result at EOF, more progress lines than `MaxProgressLines` (pull: 20,000; others: 0), or more than 4 MiB in total → the op fails `agent_protocol`, the stream is closed and the hub logs once per op name per minute. Text fields go through `agent.CleanText` before they reach the DB, logs or the UI.
- **`hello`** (`RemoteOK`: read-only, no secrets): the hub opens it once per session after attach: `{"v":1,"op":"hello","args":{"hub_max_v":1}}` → `{"v":1,"agent":"<version>"}`, the version string cleaned and capped at 64 bytes. An M1/M2 agent answers with the line its `handleStream` writes on a parse error (`serverdock-agent: bad op: …` or `serverdock-agent: unknown op "hello"`, `internal/agent/session.go`) and closes: the hub records the session as **v0 (`logs` only)**. **The negotiated version never widens what a remote session may run**: `RemoteOK` is checked independently of `hello`, and v1 ops on a v0 session are refused.
- **Two-sided `LocalOnly`:** the hub refuses non-`RemoteOK` ops to every session other than `LocalID` (M1), **and** `serverdock agent` (remote mode) builds its handler table with `RemoteOK` ops only. The remote allowlist becomes **`{hello, logs}`**; step 1 updates plan 0002 rule 9, ADR 0009's wording is unchanged in substance (ADR 0012 records the addition), and `docs/security.md` (A3 row, rule 3).
- **Ops added** (all `LocalOnly` except `hello`, all v1):

  | Op | Args | Result / lines | Notes |
  |---|---|---|---|
  | `image_pull` | `ref`, `stall_s` (default 120) | progress `{layers, layers_done, bytes, bytes_total, status}` at most 2 per second; result `{image_id, repo_digest}` | **first one `statfs` of Docker's data root** (from `/info`): free space below 1 GiB → `disk_full` before pulling; not readable → proceeds, noting "free disk space unknown". Docker's JSON lines capped at 64 KiB; no new bytes or status for `stall_s` → `stalled`; ENOSPC → `disk_full` |
  | `image_inspect` | `ref` or `image_id` | `{id, repo_digests[≤8], exposed_ports[≤32], volumes[≤32], reserved_label_keys[≤8]}` | never returns label values |
  | `container_create` | the typed create spec, the expected `ip` and `image_id` | `{id, existed}` | **the agent adds `serverdock.instance`, refuses extra labels, checks that `ip` is a host address of `meta.app_subnet` (not its network, gateway or broadcast address, and outside the dynamic `IPRange`), then runs `policy.CheckCreate` on the exact bytes it sends to Docker**; 409 → inspect by name → labels and image id must match |
  | `container_start`, `container_restart` | `id` (64 hex), `grace` | `{}` | ownership checked first; acts on the inspected id |
  | `container_inspect` | `id` | `{state, exit_code, oom_killed, restart_count, ip, image_id, started_at, labels(ours only)}` | |
  | `container_stop` | `id` (64 hex), `grace` | `{}` | ownership checked; bounded by grace + 15 s (M1) |
  | `container_remove` | `id` (64 hex) | `{}` | ownership checked; `force=false`; `v=1` (removes the container's **anonymous** volumes only; Docker never removes named volumes this way, QM3a-3) |
  | `container_list` | — | ours only (label filter on instance), **all states**, ≤ 1,000 summaries with the configured `IPAMConfig` address, the current address, state, labels and image id | resync, the allocator's "held" rule |
  | `container_ports` | — | every container that publishes a host port: name (≤ 64 chars), ports, host IP | SEC6 exposure check; ≤ 1,000 |
  | `network_ensure` | `name`, `subnet` | `{id, subnet, gateway}` | idempotent; refuses a same-name network without our label or with another subnet |
  | `network_inspect` | — | used addresses on our network (≤ 1,024) | IP allocation |
  | `events` | — | event lines `{action, id, service, deploy, exit_code, time}`; result only when Docker's stream ends | `Timeout = 0`; one Docker stream per op; an over-long Docker event (> 64 KiB) ends the op so the watcher resyncs |
  | `health_check` | `target` (ip:port), `kind`, `path`, `timeout_ms` | `{ok, status, ms, err}` | target must be a host address of our subnet and **not its network, gateway or broadcast address**; the runner passes the deploy's recorded `ip`; no redirects, no proxy env, `Host:` the IP, body discarded (≤ 4 KiB read), 2 s timeout |
  | `proxy_apply` | the app route list | `{applied, failed_ports[]}` | the agent owns Caddy (ADR 0004); **op timeout 20 s**, longer than Caddy's 10 s reload grace |

- **Untrusted text (`agent.CleanText`)** for every string that came from Docker, a registry, an image or a container (error messages, pull status, event attributes, log tails, the agent version): invalid UTF-8 replaced, C0/C1 control characters (except `\n` and `\t` in log bodies) and Unicode bidi overrides removed, length capped (512 bytes for reasons, 256 for stage details). This is the M3a part of G13; M8 owns live logs.

### Docker client additions (ADR 0012)

`internal/agent/dockerapi` stays the hand-rolled typed client. Every new method goes through `call` (per-call deadline) or `stream` (context-bounded, header deadline); the helper-usage test covers them. New: `ImagePull`, `ImageInspect`, `ContainerCreate` (typed body; only the fields the policy allows exist in the Go type), `ContainerStart`, `ContainerInspect`, `ContainerRestart`, `ContainerRemove`, `ContainerList` (label filter), `ContainerPorts`, `NetworkEnsure` (with `IPAM.Config.Subnet`), `NetworkInspect`, `Info` (data root), `Events` (label + type filter). Status codes map to typed errors (`ErrNotFound`, `ErrConflict`, `ErrNotModified`). Fields used are stable across API 1.44–1.52.

### Limits

| Limit | Value | Where |
|---|---|---|
| Services per install | 100 | `services.create` (≤ 100 active + 2 candidates + a few failed-removal leftovers fit the 190 static addresses of a /24) |
| Static addresses | 190 on the default /24 (`.2`–`.191`; `.192`–`.255` is Docker's dynamic slice) | allocator; `ip_pool_exhausted` when none is free |
| Deploy rows | last 200 per service (never the active one) | lazy prune when a deploy finishes |
| PIDs per container | 4096 | create body (`HostConfig.PidsLimit`), checked by `CheckCreate` |
| Free space on Docker's data root before a pull | ≥ 1 GiB | `image_pull` (one `statfs`, no timer) |
| Deploy log per deploy | 1 MiB before compression, last 10 per service | deploy logs |
| Workers | 2 | scheduler |
| Op lines | request 64 KiB; progress/event 1 KiB; result per op | protocol |

### Jobs and the scheduler (`internal/jobs`)

- **One goroutine, one timer** (rule 3). The scheduler sleeps on `select { timer, notify, ctx }`. After each pick it resets the timer to the earliest `run_after` of a queued job, or **stops it when nothing is due later**. In M3a only transient pull retries schedule work for later, so at idle there is no timer at all. M6 adds cron and the daily housekeeping job to this timer.
- **Start gate:** the scheduler dispatches nothing until the reconciler's **first successful resync** has finished (see Reconciler); until then jobs stay queued and the UI shows ◌ unknown service states.
- **Pick:** `UPDATE jobs SET state='running', boot_id=?, started_at=?, request_id=? WHERE id = (SELECT … WHERE state='queued' AND run_after <= ? AND lock_key NOT IN (running keys) ORDER BY run_after, id LIMIT 1)`. The partial unique index makes a second running job per lock key impossible even if two pickers race.
- **Workers:** a goroutine per running job, **at most 2** in total. Nothing runs at idle. A per-service in-memory lock in `internal/deploy` is held by the job for that service and by the resync rules when they touch that service.
- **Enqueue + supersede in one transaction:** `services.deploy` inserts the deploy row (no address yet) and the job row; every **queued** deploy job of that service becomes `cancelled` (its deploy `superseded`, `superseded_by` set); a **running** one gets `cancel_requested=1, cancel_reason='superseded'`, and the scheduler closes that job's in-memory cancel channel.
- **Cancel:** `deploys.cancel` uses the same flag. Shutdown pauses the scheduler and cancels running job contexts (see Shutdown).
- **Pause / Resume** exist from M3a (used by shutdown). M7b pauses the scheduler before its "no job running" check.
- **Retries:** only a pull that fails with a transient error (registry 429 or 5xx, timeout, connection refused, daemon unavailable) is retried: `attempt+1`, `run_after = now + 15 s`, then `+ 60 s`, at most 3 attempts. **`disk_full` (ENOSPC or the free-space check) is permanent.** Each attempt uses a new container name suffix.
- **No `unknown` job state.** A job interrupted by a restart is always resolved by the boot pass (`failed` or `succeeded`).

### Jobs run as the actor that queued them (plan 0002 "Jobs never turn a user action into a system action")

- The job row stores the actor (`actor_blob`, plus `actor_session` for a user session), the time of enqueue and, for destructive requests, the `Confirm` value. **`actions.Actor` gains `JobBlob()`** (kind, id, scope, `mfa_fresh_until`, session public id, host kind) and **`SystemInvoker.ActorFromJob(blob)`** rebuilds it. Only the holder of the one-time invoker can rebuild an actor, and it refuses kind `system` and unknown kinds. Jobs nobody triggered (none in M3a; M6 cron) are stored as `system` and run through `SystemInvoker.Invoke`, never `InvokeAs`.
- **At enqueue:** the user-facing action runs through `Invoke` as usual (decode, validate, policy, intent row); its Execute **enqueues** and returns `{deploy, number, job}`. Its audit rows say "queued".
- **At run time:** the worker calls **`auth.RefreshJobActor(actor, enqueuedAt)`**, then `SystemInvoker.InvokeAs(actor, "jobs.run_<kind>", {job}, Options{Confirm, RequestID: "job-<id>-a<n>"})`. The registry checks the policy again, now, for that actor. The audit rows carry that actor, with `system` only in the `executor` column.
  - **Token actor:** the token must still exist, not be revoked or expired; the scope is read from the DB. Otherwise the job fails `denied` ("the API token that queued this job was revoked or has expired").
  - **User actor:** the user must still be an admin (M2 definition), and `max(password_changed_at, totp_confirmed_at)` must not be later than `enqueued_at`. A password change, a TOTP re-enrolment or `admin reset` therefore stops work queued before it. Signing out does not.
  - **Explicit session revoke:** `auth.sessions_revoke` (one session, or "all others") also cancels the queued and running jobs whose `actor_session` is a revoked session (reason "the session that queued this job was revoked"). A running deploy stops at its next cancellable point.
  - The fresh-2FA window comes from the blob, so a destructive job that waits past its 5-minute window is refused at run time (M1 behaviour).
- **New registry flag `JobOnly`:** denied to every invocation except `SystemInvoker.InvokeAs`. REST answers 404, OpenAPI omits it, and the route matrix checks both. `Register` panics if a `JobOnly` action has a `System` reason.

### Deadlines and contexts

The job's own deadline must never cut the switch or the commit (that could leave traffic on a candidate the DB doesn't know).

| Phase | Context | Bound |
|---|---|---|
| Pre-switch: `pull`, `check`, `start` (and for stop-first `maintenance`, `stop_old_first`) | the job context (cancellable) | 10 min for pull + check + start; stop-first adds `stop_grace_s + 15 s` |
| `health` | the job context (cancellable) | `health_timeout_s` |
| `switch` | `context.WithoutCancel` + own timeout | 30 s (`proxy_apply` 20 s, plus the IP and state reads) |
| `commit` | `WithoutCancel` + own timeout | 10 s |
| `drain` | `WithoutCancel` | exactly `drain_s` (cut short only by a queued delete, below) |
| `stop_old` | `WithoutCancel` | `stop_grace_s + 15 s` for the stop, 30 s for the remove |
| re-apply after a failed switch or commit; candidate removal on failure | a **fresh** context | 30 s each |
| stop-first "restart the old container" (any failure or cancel after `stop_old_first`) | a **fresh** context | `health_timeout_s` + 30 s (start, health gate, route back) |

**`jobs.timeout_s` is derived at enqueue** from the snapshot: the sum of every row above for the chosen strategy, including the failure paths (blue-green at the maximum settings: 600 + 600 + 30 + 10 + 300 + 345 + 60 = 1,945 s; stop-first adds `stop_grace_s` + 15 s and the restart path, `health_timeout_s` + 30 s, at most 945 s more), so no allowed setting can exceed it. **`timeout_s` is the watchdog for the whole sum, but it is enforced only while the job is before its switch**: if it fires there, the job is cancelled like a supersede (candidate removed, old version kept). Once the switch has begun, the phases run to their own bounds above and the watchdog is not applied; because the bounds sum to at most `timeout_s`, the job still ends within it.

### Blue-green deploy (the commit order)

"Cancellable" means a supersede, a `deploys.cancel`, an explicit session revoke or shutdown takes effect there.

| # | Stage | What happens | Cancellable | On failure |
|---|---|---|---|---|
| 1 | `pull` | `image_pull`, or "image present" for a redeploy/rollback whose `image_id` exists locally; else pull by `repo_digest`. Records `image_id`, `repo_digest`. | yes | failed `pull_failed` / `pull_stalled` / `disk_full` / `image_gone`; app_safe yes |
| 2 | `check` | `image_inspect` → `policy.CheckImage` (reserved labels refused, declared volumes noted); **the address allocated under the allocation lock** (the "held" rule) and written to `deploys.ip`; the create body built with `Image = image_id` and `ip`, passed through `policy.CheckCreate`. | yes | failed `policy` / `ip_pool_exhausted`; app_safe yes |
| 3 | `start` | `container_create` (the agent re-runs `CheckCreate` on its exact bytes) → `container_start` → `container_inspect` (the runner confirms the IP equals `deploys.ip` and records the container in the state map itself; it does not wait for the watcher). | yes (removes the candidate) | candidate removed; failed `start_failed` |
| 4 | `health` | Checks every 1 s until `health_passes` in a row pass, within `health_timeout_s`. **Fail fast** on a `die`/`oom` event or an inspect showing exited/restarting. **A supersede or cancel here counts as an unhealthy result.** | yes, as "unhealthy" | log tail captured, candidate removed; failed `health_timeout` / `exited` / `oom` / `superseded`; app_safe yes |
| 5 | `switch` | Under the route lock: set `override[service] = upstream(candidate)`, build, `proxy_apply`. | **no** | override cleared, routes re-applied (old) with a fresh context, candidate removed; failed `switch_failed`; app_safe yes if the re-apply succeeded |
| 6 | `commit` | One transaction: `services.active_deploy_id = d`, `deploys.committed_at`, `deploys.stage='drain'`. Then, under the route lock, the override is cleared (the DB now says the same). | **no** | routes re-applied from the DB (old deploy) with a fresh context; candidate removed; failed `commit_failed` |
| 7 | `drain` | Wait `drain_s` (≥ 10 s, Caddy's reload grace). | no (a newer deploy waits; a queued delete cuts the rest short) | — |
| 8 | `stop_old` | Capture the old container's log tail into **its** deploy log; `container_stop` (grace) → `container_remove`. | no | deploy still `succeeded`, with the warning "the previous container could not be removed: …"; the next resync removes it |
| 9 | done | `status=succeeded`, outcome audit row. | | |

- **The old container is stopped only after the commit.** Steps 5–6 can't be cancelled.
- **Routes during the switch:** generated from DB desired state plus the override map, which has an entry only between steps 5 and 6 (variants: **upstream** for a candidate, **maintenance** for the stop-first gap). A crash between 5 and 6 leaves the DB naming the old deploy, whose container is still running.
- **"Automatic rollback"** in M3a means: an unhealthy candidate is removed and traffic never leaves the old version. There is no post-switch monitoring window (alerts are M8).
- **No previous version** (first deploy): `app_safe` is `unknown` on failure, and the route serves the static "not running" page.

### Stop-first

| # | Stage | Cancellable | On failure or cancel |
|---|---|---|---|
| 1–2 | `pull`, `check` | yes | failed; app_safe yes |
| 3 | `maintenance`: override = **maintenance** (503, `Retry-After: 10`, "Deploying, back in a moment"), `proxy_apply` | yes | override cleared; failed; app_safe yes |
| 4 | `stop_old_first`: `container_stop` on the active container (not removed) | yes | **restart the old container** (below) |
| 5 | `start` | yes | candidate removed; **restart the old container** |
| 6 | `health` | yes, as "unhealthy" | candidate removed; **restart the old container** |
| 7–8 | `switch`, `commit` | **no** | as blue-green, then **restart the old container** |
| 9 | `stop_old`: remove the already stopped old container (log tail first) | no | warning only |

- **Restarting the old container** (any failure or cancel after stage 4): remove the candidate; `container_start` the old one; run the same health gate on it; clear the override (route back to it). `app_safe` is `yes` if it passes, otherwise `no` with "the previous version did not come back: <reason>", and the service shows ◆ "Needs attention".
- **Why it exists in M3a:** the roadmap item, and apps that must never run twice. Automatic selection when a writable volume is attached is M3b.

### Restart, redeploy, rollback, cancel, delete

- **Restart** (`services.restart`, job): `container_restart` on the active container with `stop_grace_s`. It causes a short outage; the UI says so and points to Redeploy. The static address is unchanged.
- **Redeploy without rebuild** (`services.redeploy`): a new deploy whose snapshot copies the active deploy's (same `image_id`), with a new IP and the service's strategy.
- **Rollback** (`services.rollback {service, to}`): a new deploy of kind `rollback` with the snapshot of deploy `to`, pinned to its `image_id` (local) or `repo_digest` (pulled). **Targets are limited to deploys that were once live** (`committed_at` set); the dialog and the API error say so. If the image is gone: failed `image_gone`.
- **Cancel** (`deploys.cancel`): a queued deploy is cancelled at once; a running one stops at the next cancellable point; during switch/commit the API answers `409 not_cancellable`.
- **Delete** (`services.delete`, **destructive**: admin scope, fresh 2FA, `Confirm == "<project>/<service>"`, session only in practice since tokens are denied destructive actions by Q8).
  - **When the request is queued** (in the same transaction as the job): every queued deploy or restart job of the service is cancelled, a running one gets `cancel_requested` (reason `deleted`), and a running deploy that is already past its commit is told to **skip the rest of its drain** and go straight to `stop_old` (normal grace).
  - The delete job then waits for the service lock like any job. Worst case it waits for `stop_grace_s + 45 s` after the request. If that pushes it past the request's 5-minute fresh-2FA window (only possible with very long stop graces, plus queueing behind other work), the run-time policy check refuses it and the job fails with "Confirming again is needed: the 5-minute window passed while the service was stopping"; the UI offers "Delete again". Tested in step 9.
  - The job removes the route (route lock), stops and removes every container labelled with this service and our instance (log tails are not kept: the logs are deleted next), deletes the deploy log directory, then the rows (and the project if it is now empty). Interrupted → `needs_attention=delete_interrupted`; every step is idempotent.

### Reconciler, events and interrupts (`internal/deploy`)

**Watcher.** One `events` op on the local session, kept open. When it ends or fails: reconnect with back-off **1 s → 30 s, jittered** (exists only while the stream is down). **On every (re)connect: open the events stream first, then `container_list`; events that arrive during the list are buffered by container id, and every container with a buffered event is re-inspected after the list** (no timestamp comparison). Then the resync rules run.

**In-memory state:** container id → {service, deploy, state, IP, image id}. Service state is derived on read: ● running, ▲ restarting, ▲ exited, ◆ missing (the active container is gone), ◌ unknown. **◌ unknown** when the events stream is down, the first resync has not finished, or **any op for that service ended in a `docker` or `timeout` error** (until the next successful op or event for it; no timer). Before each deploy, the runner inspects the active container directly instead of trusting the cache.

**Event handling:** `start`/`stop`/`die`/`oom`/`destroy` update the state map and call `applyRoutes` (a no-op when nothing changed: addresses are static and a restarting container keeps its upstream, so a crash-looping container causes no reload). A `die`/`oom` of a candidate fails its health gate fast. A `destroy` of an active container → ◆ missing; the route serves the static "not running" page.

**Boot pass: DB state only, before the first resync.** For each job in state `running` with an older `boot_id`:

| Job | Condition | DB changes |
|---|---|---|
| deploy d | `services.active_deploy_id == d` (the commit happened) | d `succeeded` with "finished after a restart: the previous version is stopped at start-up"; job succeeded; audit outcome `ok` |
| deploy d | not committed | d `failed`, reason `interrupted` ("interrupted by restart"), app_safe `unknown` until the resync reports the active container's state (then `yes` if it runs); job failed; audit outcome `error` |
| restart | — | job failed `interrupted` |
| service_delete | — | job failed `interrupted`; `needs_attention=delete_interrupted` |

The audit outcome resolves the `unknown` intent of `jobs.run_<kind>` (found by `request_id`, `job-<id>-a<n>`) through the M1 later-boot outcome path: the intent's actor, `executor = system`. **This refines plan 0002's "rolled back" rule: the commit point is authoritative**; a deploy that crashed after its commit is finished, not rolled back. Step 15 amends plan 0002 (M3a section, the interrupt rule and its test line) and ADR 0012 decision 7 records it. There is no "resume" of an uncommitted deploy.

**Resync rules (the only place the reconciler touches containers).** They run after **every successful events (re)connect** (including the first one at boot) and after each job for a service finishes (for that service only). **The resync never waits for a service lock:** it uses `TryLock`; a busy service (a job holds it) is skipped, and the per-service resync that runs when that job finishes covers it. For each service whose lock it got:

1. **A service with a running job** is always busy (the runner holds its service lock for the whole job), so it is skipped above. As a second safeguard, rules 2–3 never touch the current attempt of a deploy whose job is `running` in the DB; containers of that deploy with an **older attempt** number are orphans (removed by the per-service resync after the job).
2. **Orphans:** every container with our instance label whose service doesn't exist, or whose deploy is not the service's active deploy, is removed through `reconciler.remove_container`: stop with the service's `stop_grace_s` (10 s if the service is gone), capture its log tail into its deploy's log file when that deploy row exists, then remove (anonymous volumes only). This covers the old version of a deploy committed before a crash and the candidate of an uncommitted one.
3. **Stopped active container:** if the active deploy's container exists but is not running and not restarting (for example after a crash during stop-first, or a manual `docker stop`), start it through `reconciler.finish_deploy`. Then report its state; an interrupted deploy's `app_safe` becomes `yes` if it runs.
4. **Routes:** refresh the state map and `applyRoutes`. **At boot, app routes are applied only after this first list + inspect has filled the state map**; until then only the panel is loaded.

Then the scheduler's start gate opens. **Passes that change nothing write no audit rows.**

**Lock order (deadlock rule).** Three locks: the per-service lock, the allocation lock and the global route lock. They are only ever taken in the order **service lock → allocation lock → route lock**, never in reverse, and the allocation and route locks are never held together with a second service's lock. The runner holds its service lock for the whole job and takes the allocation lock at `check` and the route lock inside `applyRoutes`. The resync takes service locks with `TryLock` only. **The events watcher takes only the route lock** (through `applyRoutes`) and never a service lock. A test-only lock-order checker (`testhooks`) fails any test that takes them out of order.

**Shutdown.** SIGTERM pauses the scheduler and cancels running job contexts. A deploy before its switch removes the candidate (fresh 5 s context; for stop-first, restarts the old container within the same bound if it can) and is marked `failed`, reason `shutdown`. A deploy inside switch/commit finishes the commit (it runs on `WithoutCancel`). **A deploy in drain or stop_old at SIGTERM is left as it is: the boot pass marks it succeeded and the resync removes the old container.** Anything not finished within the 10 s process grace is handled the same way.

### Failure rules

- **Disk full:** ENOSPC during a pull, or the free-space check, fails the deploy `disk_full` (not retried). A failure to write a deploy log file never fails a deploy (the stage detail says "log not kept: <reason>").
- **DB write failures:** before the switch → the deploy fails and the candidate is removed (fresh context). After the commit → the deploy stays succeeded; a failed stage or outcome write is logged and the deploy shows "record incomplete" (honest, never success-without-record for the commit itself, which is the transaction).
- **Docker hung or erroring:** every Docker call has a deadline (M1); an op that ends in `docker` or `timeout` sets the service's state to ◌ unknown (above) and fails the stage it was in.

### App routes in the embedded Caddy (`internal/agent/proxy`)

- **`proxy.Config.Apps []AppRoute`**, set only in server mode: `{Port, Upstream "ip:port"}` or `{Port, Static {Status, Body, RetryAfter}}`. Each becomes a Caddy server `app-<port>` listening on `:<port>`, plain HTTP, with the panel's public limits, `automatic_https` disabled, `protocols: [h1]` (no h2c or HTTP/3, so no UDP). **The remote agent's config is unchanged** (golden test), and so is the panel server, including its TLS policy and SNI defaults.
- **Static pages:** "This app is not running (ServerDock)" (503) when there is no running active container; "Deploying, back in a moment" (503, `Retry-After: 10`) for the maintenance override.
- **An upstream only for the service's own running container.** For each exposed service the generator emits `Upstream <ip>:<container_port>` only when the state map shows a container **with this service's labels and the target deploy's id, at that address, in state running or restarting** (restarting is kept as an upstream so a crash loop doesn't flip the config; Caddy answers 502 while the app is down). Otherwise (no container, stopped by a `stop` event or by us, unknown, or an address mismatch) it emits the static "not running" page. On a `die` event the watcher inspects the container (event-driven, no timer) and takes restarting or exited from `State.Restarting` / `State.Running`, never from event order (`docker stop` emits kill → die → stop). An address collision, however it arose, therefore can never route one service's traffic to another service's container.
- **Reload cost.** Each `proxy_apply` that changes the config makes Caddy provision a new config with every app server (and the panel) and gracefully retire the old one: a few milliseconds of CPU and a short allocation burst, plus one new upstream-health ticker per app route (the old ones stop). Hash dedupe means repeated events that change nothing (a crash-looping container with its static address, `die`/`start` pairs) cause no reload.
- **One global route lock.** `deploy.applyRoutes(mutate func())` takes the lock, runs the optional mutation (set or clear an override), reads the services and active deploys from the DB, the override map and the state map, builds the sorted route list, compares its hash with the last applied one, calls `proxy_apply` only if it changed, then records the hash and the per-port bind results, and releases the lock. **Every caller goes through it:** the events watcher, the runner's switch, commit and failure re-applies, `services.set_public_port`, the resync rules and `services.delete`. No other code calls `proxy_apply`.
- **Bind failures are isolated per port.** At boot, the panel is loaded alone first (as today). When `caddy.Load` fails, the agent retries without each *newly added* port in turn (usually one; at boot it adds them one by one) and reports `failed_ports`; those services show "port 8080 is in use by another program on this server"; the others serve. `caddy.Load` keeps the previous config running when a new one fails.
- **Allowed public ports (QM3a-2):** 1024–65535, unique across services, and never `--listen`'s port, `--https-listen`'s port, `--agent-listen`'s port, 80 or 443. No app can share the panel's TLS port, so the panel's `fallback_sni` question (plan 0003 round-2 note) stays with M4. **A host firewall applies on top**; the UI says "exposes the app on the internet at http://<address>:<port>, unless your host's firewall blocks the port".

### Policy check (`internal/policy`, shared with M3b and M6)

- **Validators** (fuzzed): project and service names; **image references** (Docker's grammar: optional registry `host[:port]`, lowercase path components, tag `[\w][\w.-]{0,127}`, digest `sha256:<64 hex>`; total ≤ 255); **registry hosts**: link-local and metadata IP literals refused, and **loopback hosts (`localhost`, 127.0.0.0/8, `::1`) refused unless `--allow-loopback-registry` is set** (used by tests and homelab registries; Docker pulls from 127.0.0.0/8 over plain HTTP); ports; health path (starts with `/`, ≤ 256 bytes, printable ASCII, no spaces); the numeric ranges above.
- **`SameRepository(a, b)`** (the `write`-token rule, QM3a-4) compares **normalised** references: the default registry `docker.io` and the `library/` prefix for single-component names are filled in (`nginx` = `docker.io/library/nginx`), the registry host is compared case-insensitively, `:443` on a registry host equals no port, and the path is compared exactly; only the tag or digest may differ. Anything that fails to parse is "not the same".
- **`CheckImage`:** an image that carries any `serverdock.*` label is refused ("label spoofing rejected", including on image deploys). Declared `VOLUME`s produce the warning "data written to /var/lib/… is not kept between deploys until persistent volumes arrive" (QM3a-3). Exposed ports are suggested as the container port.
- **`CheckCreate`: default-deny on the final Docker create body, down to the leaves.** The body is decoded into a generic tree and every path must match the allowlist exactly:
  - `Image` = the expected `sha256:` image id (the one `check` inspected);
  - `Labels`: in the control plane exactly **five** keys (`managed`, `project`, `service`, `deploy`, `attempt`) with the expected values; in the agent, after it adds `serverdock.instance`, exactly **six**;
  - `ExposedPorts`: exactly `{"<container_port>/tcp": {}}` (or absent);
  - `HostConfig.RestartPolicy` = `{"Name": "unless-stopped"}` (no `MaximumRetryCount` other than 0);
  - `HostConfig.LogConfig` = `{"Type": "local", "Config": {"max-size": "10m", "max-file": "3"}}`, no other keys;
  - `HostConfig.NetworkMode` = our network's name (refuses `host`, `bridge`, `none`, `container:…`, anything else);
  - `HostConfig.PidsLimit` = 4096;
  - `NetworkingConfig.EndpointsConfig` = exactly `{"<our network>": {"IPAMConfig": {"IPv4Address": "<the allocated ip>"}}}` (security's "exactly empty" rule, reconciled with static addresses: this one key, this one value, nothing else); the address must be a host address of `meta.app_subnet`, not its network, gateway or broadcast address, and outside the dynamic `IPRange`;
  - **anything else** (`Privileged`, `PidMode`, `IpcMode`, `UTSMode`, `UsernsMode`, `CapAdd`, `Devices`, `DeviceRequests`, `Binds`, `Mounts`, `VolumesFrom`, `PortBindings`, `PublishAllPorts`, `SecurityOpt`, `Sysctls`, `CgroupParent`, `Env`, `Cmd`, `Entrypoint`, `User`, `Hostname`, `Aliases`, …) → refused.
  It runs twice: in the control plane at `check`, and **in the agent on the exact bytes sent to Docker**. M3b and M6 widen the allowlist explicitly.
- **User-supplied labels:** M3a has no label input. `policy.ValidateUserLabels` (rejects `serverdock.*`) is built and tested now for M3b/M6.

### Deploy logs and live progress

- **Files:** `DATA/deploy-logs/<service id>/<deploy id>.log.gz`, 0600 in a 0700 directory. Contents: a header (deploy number, image, digest, times), the stage lines, and the container output tail captured through `logs` with `follow:false`, `tail:2000`, read up to **1 MiB uncompressed per deploy** (worst case on disk: 1 MiB × 10 deploys × 100 services, less after compression). The tail is **streamed** through a gzip writer taken from a `sync.Pool` (no whole-log buffer); reads go through `io.LimitReader` (1 MiB of decompressed output), so a hostile file can't expand in memory. Captured for the candidate before it is removed (failed deploys), for the old container before it is removed after a success, and for orphans the resync removes.
- **Retention:** the last **10** deploys per service keep a file; older files are deleted when a deploy finishes (lazy). Older deploys show "Log no longer kept (only the last 10 deploys keep logs)".
- **Read:** `deploys.log` returns the decompressed text (≤ 1 MiB) through `CleanText` (log mode). The UI renders it as text only (G13). **Tokens need `write` scope** for it (QM3a-4); MCP (read-only) never gets it.
- **Container output is sensitive.** Many images print a generated admin password or a setup token at first start, so deploy logs can hold credentials even in M3a. Hence the `write` scope for tokens, files 0600, and the binding notes for plan 0005 (a sentinel test covering `deploys.log`, `deploys.get` reasons and stage details once app secrets exist) and plan 0009 (`doctor --bundle` excludes `deploy-logs/`). They are not in `datadir.SecretPaths` (that list holds ServerDock's own secrets).
- **Progress:** the runner keeps the latest pull progress of each running deploy in memory and returns it from `deploys.get`; the final summary is written to `deploy_stages.detail`. **The SPA polls `deploys.get` every 2 s while the deploy runs and the page is visible** (QM3a-6), **backing off to every 5 s once a pull has run for 2 minutes** (large images). Only the Deploy detail page polls; the Services list and Overview load on navigation and have a Refresh button. No server timer.

### SEC6: apps on the panel's address (plan 0003 obligation)

Cookies are host-scoped, not port-scoped (ADR 0010, accepted risk SEC6).

1. **Our services.** `services.create`, `services.set_public_port` and `services.get` return `warnings: [{code: "shares_panel_address", …}]` for a service with a public port; the New service form shows it next to the field and the Traffic card shows it. Wording: "This app is reachable on the same address as the panel (203.0.113.5:8080). Browsers share cookies by host name, not by port. ServerDock serves this app over plain HTTP, so the panel's secure cookies are never sent to it. Don't run any other HTTPS service on this address. Once domains arrive, give the panel and your apps their own names." + docs link.
2. **A page served by an app can't act on the panel.** A request from `http://<panel ip>:<app port>` carries that `Origin`, which never equals `https://` + `Host`; the route matrix gains this case for every unsafe route (403 `origin`).
3. **Containers ServerDock didn't create.** `system.exposure` (read) runs `container_ports` and lists every non-ServerDock container that publishes a host port on all interfaces or on one of the panel's addresses. Cached 30 s in memory, computed only when asked (Overview load), no timer. Overview banner: "2 containers publish ports on this server's address: grafana (3000/tcp), portainer (9443/tcp). A service there that uses HTTPS could receive your panel sign-in cookie." + docs link. It shows container names to any `read` caller (noted in Security notes).

### Actions added to the registry

Inputs name services as `"<project>/<service>"`; that string is the `Target`, and the destructive `Confirm`. Token powers follow QM3a-4 (narrowed): the actions that choose **what** runs or **where it is exposed** need `admin` scope (a session, or an `admin` token).

| Action | Risk | Scope | Flags | Notes |
|---|---|---|---|---|
| `projects.list` | read | read | — | |
| `services.list`, `services.get` | read | read | — | state, active deploy, route status, warnings |
| `services.create` | mutate | **admin** | — | `{project, name, image_ref, container_port?, public_port?, health…, strategy, drain_s, stop_grace_s, deploy: bool}`; creates the project if new; `deploy:true` enqueues the first deploy |
| `services.update` | mutate | write | — | health, strategy, drain, stop grace, container port; `image_ref` only as **another tag or digest of the same repository** (`policy.SameRepository`, normalised); takes effect at the next deploy |
| `services.set_source` | mutate | **admin** | — | changes the image repository or registry |
| `services.set_public_port` | mutate | **admin** | — | sets, changes or clears the public port; applies now (route lock); the UI asks for confirmation first |
| `services.deploy` | mutate | write | — | `{service, image_ref?}`: an optional new **tag or digest of the current repository** only (`policy.SameRepository`; another repository → `invalid_input` "changing the repository needs services.set_source"); enqueues; supersedes |
| `services.redeploy`, `services.restart` | mutate | write | — | |
| `services.rollback` | mutate | write | — | `{service, to}`; only deploys that were once live |
| `services.delete` | **destructive** | admin | — | fresh 2FA, `Confirm`; tokens denied (Q8) |
| `deploys.list`, `deploys.get` | read | read | — | stages, progress, reason, app_safe, cancellable |
| `deploys.log` | read | **write** | — | text ≤ 1 MiB, cleaned (container output can hold credentials) |
| `deploys.cancel` | mutate | write | — | `{service, number}`; 409 inside switch/commit |
| `system.exposure` | read | read | — | SEC6 part 3 |
| `jobs.run_deploy`, `jobs.run_restart` | mutate | write | **JobOnly** | run as the queuing actor |
| `jobs.run_service_delete` | destructive | admin | **JobOnly** | `Confirm` from the job row; fresh 2FA re-checked |
| `reconciler.finish_deploy` | mutate | admin | ConsoleOnly (SystemInvoker only), `System` = "crash reconciliation: record the outcome of a deploy interrupted by a restart (finished if it had committed, failed if not) and start the active deploy's stopped container" | |
| `reconciler.remove_container` | destructive | admin | ConsoleOnly, `System` = "crash reconciliation: remove a container ServerDock labelled (this install) that belongs to no active deploy and to no running deploy's current attempt" | listed in `docs/security.md` |

`ConsoleOnly` already means "denied to every actor except the SystemInvoker; REST 404; not in OpenAPI". The reconciler holds the SystemInvoker, so these two actions reuse it; step 4b updates the flag's doc comment.

**REST:** no new routes; everything goes through `POST /api/v1/actions/{name}`. The OpenAPI golden and `web/src/lib/api-types.ts` are regenerated. The route matrix runs every new action in all 24 credential cases plus the app-origin case; `JobOnly` and `ConsoleOnly` actions answer 404 for every credential.

### UI (SPA)

Sidebar (QM2-3 rule, only working pages): **Overview, Services, Settings**.

1. **Overview** (`/`): service counts by state, a "Needs attention" list (last deploy failed, ◆ missing, route error, ◌ unknown, delete interrupted), the SEC6 exposure banner, recent deploys (5). Empty state: "No services yet" + "Deploy an image" button + docs link.
2. **Services** (`/services`): grouped by project (`<h2>` per project). ≥ 768 px a `<table>` with `<caption>` (name, state, image, live deploy, address, last deploy); below, cards (A9). Button "New service". Empty state as above.
3. **New service** (`/services/new`): heading "Deploy a Docker image". Fields: Project (text with existing ones listed, default `default`), Name, Image, Port your app listens on (optional; suggested from the image), Public port (optional; the SEC6 hint and "exposes the app on the internet at http://<address>:<port>, unless your host's firewall blocks the port"), Health check (HTTP path / TCP / "stays running"), Strategy (Blue-green / Stop-first, explained), an "Advanced" disclosure (health timeout and passes, drain, stop grace). Submit **Create and deploy** → the Deploy detail page. No cards for later sources (QM2-3 rule); a one-line hint says Git, Compose and databases come later.
4. **Service detail** (`/services/[project]/[name]`): `<h1>` name + state; actions **Deploy** (dialog with an editable tag or digest of the current image), **Redeploy**, **Restart** (dialog states the downtime). Cards: Running now, Traffic (address, SEC6 warning, health check, route error), Strategy. Sub-navigation (`<nav aria-label="Service">`, links with `aria-current`): **Deploys** and **Settings**. Deploys: history with #, result (● Live, ○ Replaced, ▲ Failed · app safe, ▲ Failed · app not safe, ◌ Interrupted, ⊘ Cancelled, ⊘ Superseded by #N), image, triggered by, stages summary, when (absolute UTC), Details, **Roll back to #N** (only on deploys that were once live; the dialog names target and image). Settings: the fields ("Saved. Deploy to apply."); **Image repository** (separate form, `services.set_source`); **Public port** (separate form; **confirmation dialog** "Applied now: exposes the app on the internet at http://203.0.113.5:8080, unless your host's firewall blocks the port." or, when clearing, "Applied now: the app is no longer reachable at …"); Danger zone **Delete service** (type `shop/api`, then the M2 step-up dialog).
5. **Deploy detail** (`/services/[project]/[name]/deploys/[number]`): `<h1>` "Deploy #128" + status; "What happened" (result, "Your app: safe, #127 kept serving" / "not safe: …" / "unknown", new container, reason); "This deploy"; stages as an `<ol>` with word + symbol (✓ Done, ✕ Failed, — Skipped, … Running, ○ Waiting); a labelled `<progress>` with text during the pull; **Cancel deploy** while cancellable (during switch: `aria-disabled` with the reason); "Container log (last 40 lines)" in a labelled `<pre>` and "Show full log"; Redeploy / Roll back where they apply.

**Accessibility (plan 0002 rule 6; M2's A1–A12 carry over):**
- **Live updates.** Only the Deploy detail page polls (2 s, 5 s after 2 minutes of pulling); the Services list and Overview refresh on navigation or with a Refresh button. Polling never moves focus. One `role="status"` region announces **stage transitions only** and success; failure is announced once through the page's `role="alert"` region. The `<progress>` element and its text are **not** a live region. Polling stops when the page is hidden; on return the state is refreshed before any announcement. If polling fails or the data is older than 10 s, a ◌ "Status unknown: could not reach the server" line replaces the stale state (A12).
- **Log text** in a `<pre tabindex="0">` with an accessible name, horizontal scroll, and a skip link past it.
- **Dialogs** (A8): target and consequence named, verb-specific buttons, default focus on Cancel, Escape closes, focus returns to the opener.
- **Forms** (A6): visible labels, linked hints (the SEC6 and firewall hints on the public-port field), an error summary, typed input kept.
- **States** word + symbol, absolute times with timezone, 44 px targets, reflow at 320 px, both themes, forced colours, reduced motion.

## Footprint rules for new Go code (S4)

- **No package-level work** in `internal/jobs`, `internal/deploy`, `internal/policy`, `internal/agentops` or the new agent op code (`sync.OnceValue` instead). Step 4a extends the M2 check (`nopkginit_test.go`) to these packages.
- **Agent mode runs none of it.** The remote agent serves only `hello` and `logs`, opens no events stream, starts no scheduler and has no app routes (its Caddy config is byte-identical: golden test).
- **No new Go dependency.**

## Footprint budget

**Allocation (plan 0002, tripwire planning numbers under ADR 0011):** S2 ≤ 56 MB, S4 ≤ 45.5 MB. **Ceilings:** S2 200 MB, S4 100 MB. **Tripwires:** 80 / 50 MB. M2 CI: S2 50.3 MB, S4 44.7 MB, binary 19.3 MB gzip, SPA JS 73.2 KB total / 40.6 KB first load.

| | M2 measured | M3a estimate | Planning number | Where the cost comes from |
|---|---|---|---|---|
| **S2 live watcher** (idle, no services, Docker socket mounted: the gated number) | 50.3 MB (no Docker) | **≈ 52 MB** | ≤ 56 (tripwire 80) | events stream (one Docker connection, one yamux stream, two goroutines, buffers ≈ 0.2 MB); scheduler goroutine; new code pages of a larger binary (≈ 0.5–1 MB); new tables |
| S2 no Docker (report only, the M2-comparable run) | 50.3 MB | ≈ 51 MB | — | the larger binary only; the watcher waits in its back-off |
| **S2 with one exposed service** (tripwire 80, step 15) | — | ≈ 52.5–53 MB | ≤ 80 | one Caddy app server and its `reverse_proxy` handler |
| S2 with 10 exposed idle services (report: RSS and idle CPU, step 15) | — | ≈ 55–58 MB; + 10 Caddy ticks per 10 s | ≤ 80 | ten app servers and tickers |
| **S4** | 44.7 MB | **≈ 45 MB** | ≤ 45.5 (tripwire 50) | nothing new runs in agent mode; only file-backed pages of a larger binary |
| Idle CPU | ≤ 0.01% | no timers of ours at idle; Caddy's 10 s ticker only when a service is exposed (one per exposed service) | < 1% / < 0.5% | |
| Peak during a deploy | — | + ≤ 5 MB transient; back to baseline within about 60 s after the deploy (measured in step 15) | — | pull progress parsing, the pooled gzip writer, health checks, one Caddy reload |
| Binary (gzip) | 19.3 MB | ≈ 19.8 MB | ≤ 40 MB | new Go code |
| SPA JS | 73.2 / 40.6 KB | ≈ 100 / ≤ 45 KB | ≤ 250 KB first load | five screens, route-split |
| Disk | — | deploy logs ≤ 1 MiB uncompressed × 10 per service (≤ 1 GiB uncompressed at 100 services, far less on disk); deploy rows ≤ 200 per service × ≈ 2 KB | — | **images are not cleaned until M8** (risk R4) |

**Bench changes (step 10, then every PR):**
- **"S2 live watcher"** mounts the Docker socket into the S2 server container (with the socket's group added, so the server's uid can use it). It is the S2 number compared with the 80 MB tripwire from step 10 on, because only then do the events stream, the first resync and the app network exist. **"S2 no Docker"** (today's run, no socket) stays as a report-only line, comparable with M2.
- The bench's clean-up also removes every container labelled `serverdock.managed` (and the bench's app network), so runs don't leak containers into each other.
- Step 15 adds **"S2 with one exposed service"** (gated against the 80 MB tripwire) and **"S2 with 10 exposed idle services"** (RSS and idle CPU reported; tripwire 80 MB), both on the live-watcher setup with the test app image from the local registry.

**Background work rows** (added to `docs/architecture.md` and plan 0002 rule 3 in the steps that add them):

| What | Where | Interval | When it runs |
|---|---|---|---|
| Job scheduler "next due" timer | `internal/jobs` | the next due time | one goroutine; **no timer when nothing is due** |
| Docker events reconnect back-off | `internal/deploy` (watcher) — moved from plan 0002's `internal/agent/dockerapi`, because the control plane owns the reconnect | 1 s → 30 s, jittered | only while the events stream is down |
| Caddy reverse-proxy upstream-health metrics updater, **one per app route** | embedded Caddy | 10 s | **A written exception to rule 3's 15 s floor, needing `footprint` sign-off in step 6** (the same text goes into plan 0002 rule 3 in that step): **at most 100 tickers (the service cap), only for services with an upstream route** (a static "not running" route has no `reverse_proxy` handler and no ticker), **torn down and recreated on every reload**. Caddy starts it unconditionally per `reverse_proxy` handler, it is not configurable, and it only sets an in-memory gauge (the panel's own has run since M1) |
| Health-gate checks, pull stall watchdog, drain wait | `internal/deploy`, local agent | 1 s / 120 s / `drain_s` | **during a deploy only** (not idle polling) |

Event-driven, no timer: the events reader, `applyRoutes` (hash-deduplicated), the resync rules, deploy-log retention, finished-job pruning (on insert), the `system.exposure` cache, the free-space check (one `statfs` per pull).

**Raises idle RAM?** Yes, by about 1.7 MB (50.3 → ≈ 52 MB "S2 live watcher"): the events stream and the scheduler goroutine. **Reason:** without the events stream, ServerDock can't tell that a container died, restarted or vanished, so service state and routes would go stale (plan 0002 requires regeneration on events), and the scheduler is the one shared timer rule 3 asks for.

**Measured, and when:** CI bench on every Go PR; QUICK S4 deltas in the PRs for steps 1, 2, 3 and 4a (rule 4: a QUICK delta > 0.5 MB needs a full S4 run); **the first "S2 live watcher" run in step 10**; at step 15 a full S4, "S2 with one exposed service" (tripwire), "S2 with 10 exposed idle services" (RSS and idle CPU), and RSS sampled for 60 s after a deploy finishes (back to the idle baseline).

**Dependencies (rule 4):** none at runtime. Test-only and CI-only: the `registry:2` image and a tiny test app image built in CI from `internal/deploy/testdata/app` (a static Go binary with `/healthz` and flags for "unhealthy", "slow start", "exit 1" and "crash loop").

## Security notes

- **New attack surface:**
  - **Authenticated write power over Docker.** A session or an `admin` token chooses what runs (`services.create`, `services.set_source`) and where it is exposed (`services.set_public_port`); a `write` token can deploy another tag or digest of an already chosen repository, redeploy, roll back, restart and cancel (QM3a-4). Delete is destructive (session, fresh 2FA, typed target).
  - **App ports:** Caddy listens on each public port (plain HTTP, panel limits); each app server proxies only to its container.
  - **Docker output as input:** registry errors, pull status, image metadata, events, container logs: capped and cleaned; the hub decodes agent lines strictly with per-op caps.
- **Accepted risk B10, in plain words (step 15 adds it to `docs/security.md`):** **anyone who may deploy can run arbitrary code with this host's network reach.** A deployed container is unprivileged and can't touch the host's files, but it can open network connections like any program on the host: the cloud metadata service (169.254.169.254, which on some clouds hands out credentials), private and LAN addresses, other machines the host can reach, and services on the host itself through the bridge gateway (for example the agent listener when `--agent-listen` is on, or anything bound to all interfaces). ServerDock does not firewall containers in Phase 1. So the permission to deploy (a session, or a token with `write` scope for already chosen images, `admin` for new ones) should be given only to people and systems trusted with that reach. Block the metadata service at the host or cloud firewall if it matters.
- **Accepted risk QM3a-9:** while ServerDock is stopped, restarting or crashed, apps with a public port are unreachable (Caddy runs in its process); containers keep running. Revisited with M7b self-update.
- **Accepted risk N13:** on Docker Engine versions before 28, hosts on the same LAN could in some setups route to container ports that are not published (Docker 28 tightened this). ServerDock requires Engine 25+, so older engines in that range are affected; step 15 asks `researcher` to confirm the affected versions and the wording.
- **New inputs and validators (rule 1, fuzzed):** names, image references, registry hosts, ports, health path, ranges; the v1 op request line (agent side) and response lines (hub side); Docker JSON lines; the create body (`CheckCreate`, in both processes).
- **No new ServerDock secrets.** No credentials are stored (public images, no env). The sentinel suite runs unchanged. Container output may contain app credentials (deploy logs above).
- **Ownership and deletion:** only containers with our instance label are stopped or removed, enforced in the agent on full ids it inspected; the reconciler removes only our orphans, never a running deploy's current attempt; label spoofing by an image is refused; ids are never reused (`AUTOINCREMENT`), and adopting an existing container also requires the expected image id.
- **Jobs and actors:** no `system` actor from HTTP (route matrix); jobs run as their queuing actor with the run-time refresh (token revoked/expired, user no longer admin, credentials changed, session explicitly revoked); `JobOnly` actions unreachable from REST; the two reconciler actions are `ConsoleOnly` and the destructive one is listed.
- **Remote agents:** every new op is `LocalOnly`, refused by the hub and not served by a remote agent process. `hello` is the only new `RemoteOK` op; the negotiated version never widens the remote op set.
- **Registry hosts and SSRF (deferred to M3b, honestly stated).** The Docker daemon, not ServerDock, fetches images, so ServerDock's dial-time checks (`internal/netpolicy`, M5) can't sit on that path. Refusing link-local and metadata IP literals is **cosmetic**: a registry can redirect, its token `realm` can point anywhere, and a name can resolve to any address. Pull error text from the registry can echo parts of a response body (it is cleaned and capped, not hidden). The real reason this is acceptable for M3a: a deployer can already run a container with more reach than any pull (B10). Loopback registries are refused unless `--allow-loopback-registry` is set, because Docker pulls from 127.0.0.0/8 over plain HTTP. M3b, which sends registry credentials, applies the rule-10 checks to registry hosts.
- **SEC6:** the warnings and the app-origin route-matrix case above.
- **Information exposure:** `system.exposure` shows the names and published ports of every container on the host to any `read` caller (including `read` tokens). Accepted: a `read` token is already trusted with the service list; step 15 records it in the security rows.
- **Network isolation:** `enable_icc=false` stops direct container-to-container traffic on the app network; services still reach each other's public ports through the host. Per-project networks are M3b.
- **G13 (partly, M3a):** deploy log tails are cleaned on the server and rendered as text; live logs remain M8.
- **WebSockets:** none added (QM3a-6).
- **Other accepted risks added by M3a:** `services.restart` causes a short outage (stated in the UI); old images accumulate until M8 cleanup (anonymous volumes are removed with their container, QM3a-3, so they don't); ServerDock must run on the host or with host networking to reach apps (QM3a-7); no `no-new-privileges` by default (revisited in M3b).

## Steps (each one PR-sized, in order)

**Migrations:** M3a's schema is `0004_deploy.sql` (step 4a); later changes add `0005_…`. **Each step updates its own rows** in `docs/security.md` and the Background work table. Integration tests need Docker: they run with `-tags integration` in the CI job added in step 2.

1. **ADR 0012, ADR renumbering, agent op protocol v1 framing.**
   - Do: copy Appendix A to `docs/decisions/0012-docker-client-and-op-protocol.md` (status "proposed"); shift the ADR reservations in plan 0002 rule 8 by one (builds 0013, Compose 0014, backups 0015, install shape 0016, signing 0017, self-update 0018) and the forward references in plan 0002 and `docs/security.md` (15 places); **update plan 0002 rule 9 and `docs/security.md` (A3 row, rule 3) for the remote allowlist `{hello, logs}`**; `internal/agent`: the v1 envelope, response line types, trailing-byte refusal, `OpSpec{MaxResult, MaxProgressLines, Timeout}`, `hello` (`RemoteOK`, 256 B result, cleaned 64-byte version), `CleanText`, the `logs` `follow` field (never sent to v0); `hub.Session.Call`; per-mode handler tables.
   - Tests: `TestHelloNegotiatesV1`; **`TestM2AgentReplyIsDetectedAsV0`** (the exact bytes an M2 `handleStream` writes for an unknown op and for a bad op, from `session.go`); `TestFollowIsNeverSentToAV0Session`; `TestV1NeverWidensTheRemoteOpSet`; `TestRemoteSessionRefusesEveryLocalOnlyOp`; `TestRemoteAgentProcessServesOnlyRemoteOKOps`; `TestRemoteOKAllowlistIsHelloAndLogs`; hostile fake agent: an over-long progress line, an over-long result, a `hello` result over 256 B, two results, no result, trailing bytes after a line, an unknown field, a progress flood, more than 4 MiB, invalid UTF-8 and escape sequences → `agent_protocol`, stream closed, no leaked goroutine; a request line over 64 KiB or with trailing bytes refused by the agent; `TestLogsFollowFalseEndsAtTail`; `TestCleanText`; `FuzzParseAgentLine`; `FuzzParseV1Request`.
   - Footprint: QUICK S4.
   - Reviewers: `reviewer`, `security` (required: the allowlist change), `systems`, `footprint`.

2. **Docker client: images, containers, networks, events.**
   - Do: the `dockerapi` methods in the design (including `NetworkEnsure` with a subnet, `NetworkInspect`, `Info`), typed errors, the 64 KiB Docker line cap; a fake dockerd on a unix socket; the **`integration` CI job** (`ubuntu-latest`: starts `registry:2` on `127.0.0.1:5000`, builds and pushes the test app image, runs `go test -race -tags integration ./...` with `--allow-loopback-registry` in the test harness; inputs through `env:`, rule 11; CODEOWNERS review).
   - Tests: each method against the fake (200, 404, 409, 304, 5xx, a hanging header, a closed stream); the helper-usage test extended; an events line over 64 KiB → error; pull progress with `errorDetail` and an ENOSPC message; the create body type has only allowed fields; integration: pull from the local registry, create with a static `IPv4Address` on a subnet network (**`TestAppNetworkGatewayIsDotOne`**: the created network's gateway is `.1` with the `IPRange` set), start, inspect, stop, remove, events, `NetworkEnsure` idempotent and refusing a same-name foreign network.
   - Footprint: QUICK S4, binary delta.
   - Reviewers: `reviewer`, `security`, `systems`, `footprint`.

3. **Agent ops on the local agent; `internal/agentops`.**
   - Do: the `LocalOnly` ops in the table except `proxy_apply` (step 6): progress coalescing, stall, the free-space check, 64-hex ids and the ownership check on the inspected id, the instance label set by the agent, `CheckCreate` on the exact bytes (stub allowlist until step 5 lands the full one, then wired), the 409 path with labels and image id, `health_check` rules, `container_ports`, `network_inspect`; the typed client `internal/agentops`.
   - Tests: every op over the in-memory session against the fake dockerd; `TestAgentRefusesToTouchAnUnownedContainer` (no label, another instance's label; stop, restart, remove); `TestOwnershipOpsRefuseShortIDsAndNames`; `TestCreateRefusesCallerSuppliedInstanceOrExtraLabels`; `TestCreateConflictWithForeignContainerFails`; **`TestCreateConflictWithOwnLabelsButAnotherImageFails`**; `TestCreateConflictWithOwnContainerReturnsExisted`; `TestPullProgressCoalescedToTwoPerSecond`; `TestPullStallFails`; `TestPullRefusedBelowFreeSpace`; `TestHealthCheckRefusesTargetOutsideOurSubnet`; **`TestHealthCheckRefusesNetworkGatewayAndBroadcast`**; **`TestCreateRefusesAnAddressOutsideTheStaticRange`** (outside `meta.app_subnet`, network, gateway, broadcast, inside the dynamic `IPRange`); `TestContainerListReturnsStoppedContainersWithTheirConfiguredAddress`; `TestHealthCheckDoesNotFollowRedirectsOrUseProxyEnv`; `TestEventsOpHasNoTimeoutAndEndsWithDockerStream`; result caps; goleak.
   - Footprint: QUICK S4.
   - Reviewers: `reviewer`, `security` (required), `systems`, `footprint`.

4a. **Schema, jobs and the scheduler.**
   - Do: `0004_deploy.sql` (every table, `AUTOINCREMENT`, the FK delete rules, the trigger, the indexes, `meta.install_id`); app subnet selection into `meta.app_subnet` and `--app-subnet`, the overlap refusal, the dynamic `IPRange`; the IP allocator (the "held" rule over the DB and a fresh `container_list` with all states at each allocation, under the allocation lock; `docker` error when Docker is unreachable; called at `check` from step 8a); `internal/jobs` (store, enqueue with supersede, pick, transitions, cancel flag and channels, retries with `disk_full` permanent, lazy pruning, derived `timeout_s`, scheduler with one timer, notify, worker limit 2, the start gate, Pause/Resume/Shutdown); `internal/store/testdata/v4.db`; the scheduler Background work row; the package-level-work check over the new packages.
   - Tests: `TestNoTimerWhenNothingIsDue`; `TestTimerSetToEarliestDueJob`; `TestSchedulerWakesOnNotify`; `TestSchedulerWaitsForTheStartGate`; `TestOneRunningJobPerLockKey`; `TestWorkerLimitTwo`; `TestSupersedeCancelsQueuedInTheSameTransaction`; `TestCancelReachesARunningJob`; **`TestFinishedJobsPrunedOnInsert`** (with a deploy row referencing a pruned job: the prune succeeds under `foreign_keys(1)` and the deploy keeps `job_id = NULL`); **`TestIDsNeverReusedAfterDeletingTheNewestService`** (projects, services, deploys, jobs); `TestSnapshotIsImmutable`; `TestInstallIDCreatedByMigration`; `TestAppSubnetAvoidsExistingNetworks`; `TestAppSubnetOverlapRefusesDeploysWithAHint`; `TestIPAllocatorSkipsNetworkGatewayBroadcastAndTheDynamicRange`; `TestIPAllocatorHeldRule` (active deploy, queued/running deploy, any-state container with our label, `network_inspect` entry); `TestPoolExhaustedError`; **`TestDeploysIPLiveIndexRefusesASecondLiveDeployWithTheSameAddress`**; `TestDeployRowsPrunedToTwoHundredKeepingTheActive`; `TestTimeoutDerivedFromSettings`; migrations fresh, from `v3.db`, and the `v4.db` fixture opens.
   - Footprint: QUICK S4; S2 delta reported.
   - Reviewers: `reviewer`, `systems` (required), `security`, `footprint`.

4b. **Actors in jobs; `JobOnly`.**
   - Do: `Actor.JobBlob`, `SystemInvoker.ActorFromJob`; `auth.RefreshJobActor`; `auth.sessions_revoke` cancels the revoked sessions' jobs; the `JobOnly` flag (policy, REST 404, OpenAPI omission, `Register` rule); the `ConsoleOnly` doc comment.
   - Tests: **`TestJobQueuedByTokenCannotReachADeniedAction`** (plan 0002 must: a `write` token queues a test job whose run invokes a destructive action through `InvokeAs` → denied, the denied row records the token actor with `executor = system`, nothing executed); `TestRevokedTokenJobDeniedAtRunTime`; `TestExpiredTokenJobDeniedAtRunTime`; `TestCredentialChangeDeniesAQueuedUserJob` (password change, TOTP re-enrolment, `admin reset`); `TestSignOutDoesNotCancelAQueuedJob`; **`TestExplicitSessionRevokeCancelsThatSessionsJobs`** (one session and "all others"); `TestActorFromJobRefusesSystem`; `TestJobAuditRowsCarryTheActorWithExecutorSystem`; `TestJobOnlyActionsAre404AndNotInOpenAPI`; `TestRegisterRefusesJobOnlyWithSystemReason`.
   - Reviewers: `reviewer`, `security` (required), `systems`.

5. **Policy and the container spec.**
   - Do: `internal/policy` (validators, registry-host rules and `--allow-loopback-registry`, `CheckImage`, `CheckCreate` to the leaves, `ValidateUserLabels`); the create-body builder in `internal/deploy`; wire the full `CheckCreate` into the agent's `container_create`.
   - Tests: one fixture per refused key and per wrong leaf value: `RestartPolicy` `always` / `on-failure` / a retry count; `LogConfig` `json-file`, an extra option, a third key; `NetworkMode` `host`, `bridge`, `none`, `container:x`, another network; `PidsLimit` 0, -1, 4097; `EndpointsConfig` another network, a second network, `Aliases`, `IPAMConfig.IPv6Address`, another IPv4; `Image` a tag, another id; a seventh label, a wrong label value; and every key in the "anything else" list → refused; the builder's output passes in both processes; an image with a `serverdock.*` label refused; declared volumes warn; registry IP literals in link-local and metadata ranges refused; loopback registries refused without the flag, accepted with it; reserved public ports refused; labels: five keys accepted by the control-plane check, six (with `serverdock.instance`) by the agent's, each refused by the other; **`SameRepository` table**: `nginx` = `docker.io/library/nginx` = `DOCKER.IO/library/nginx`, `registry.example:443/a/b` = `registry.example/a/b`, `ghcr.io/a/b` ≠ `ghcr.io/a/c`, `nginx` ≠ `example/nginx`, a tag change and a digest change are the same repository, an unparsable reference is never the same; `FuzzImageRef`, `FuzzSameRepository`, `FuzzName`, `FuzzHealthPath`, `FuzzCheckCreate` (seeds in CI, a 5-minute local run before merge).
   - Reviewers: `reviewer`, `security` (required).

6. **App routes in the embedded Caddy.**
   - Do: `proxy.Config.Apps` (upstream and static routes, per-port servers, h1 only, panel limits); `proxy_apply` (20 s, per-port bind isolation); `applyRoutes` with the **global route lock** and hash dedupe; the override variants (upstream, maintenance); boot order (panel alone first; apps only after the first list + inspect); upstreams only for the service's own running or restarting container at its address; the per-route Caddy ticker row as a written rule 3 exception, with the same text added to plan 0002 rule 3.
   - Tests: `TestAgentBuildConfigGolden` unchanged; **`TestPanelConfigUnchangedWithApps`**; `TestAppRouteGolden`; `TestBindFailureIsolatedToOnePort`; `TestBootLoadsPanelBeforeApps`; **`TestBootAppliesAppRoutesOnlyAfterFirstListAndInspect`**; `TestNoReloadWhenRoutesUnchanged`; **`TestCrashLoopingContainerCausesNoReload`** (repeated `start`/`die` events for one container: zero `proxy_apply` calls after the first); **`TestConcurrentRouteChangesForTwoServicesLoseNothing`** (two services switching while events and a `set_public_port` arrive concurrently: the final applied config has both new upstreams and the new port; `-race`); `TestOverrideWinsDuringSwitch`; `TestStaticNotRunningAndMaintenancePages`; **`TestUpstreamOnlyForTheServicesOwnContainerAtItsAddress`** (another service's container at that address, a stopped own container, an address mismatch → the "not running" page); `TestStaticRouteHasNoReverseProxyTicker`; integration: 20 reloads under load → zero non-2xx on the panel and on an app.
   - Footprint: report-only S2 with one route; `footprint` signs the ticker exception.
   - Reviewers: `reviewer`, `systems` (required), `security`, `footprint`.

7. **Service actions and the SEC6 warnings (no deploys yet).**
   - Do: `projects.list`, `services.create` (without `deploy:true` until step 8a), `services.update` (same-repository rule), `services.set_source`, `services.set_public_port` (through `applyRoutes`), `services.list`, `services.get`, `system.exposure`; `network_ensure` at start (`enable_icc=false`, the recorded subnet); the 100-service limit; warnings; OpenAPI golden and generated types; route-matrix rows, including **an unsafe request with `Origin: http://<panel ip>:<app port>` → 403 on every unsafe route**.
   - Tests: validation errors; duplicate names; the 101st service refused; public port unique and reserved; **`TestTokenScopesForServiceActions`** (`read` lists; `write` can update health and tag but not create, `set_source`, `set_public_port`, nor change the repository through `update`; `admin` token can do those; no token can delete); **`TestSharesPanelAddressWarning`**; **`TestExposureListsForeignPublishedPorts`** (ours excluded, cached 30 s, no timer); the sentinel suite unchanged and green.
   - Reviewers: `reviewer`, `security` (required: token powers), `systems`.

8a. **Deploy runner: pre-switch stages.**
   - Do: `services.deploy` (same-repository rule), `services.create` with `deploy:true`, `jobs.run_deploy`, `deploys.cancel`; stages 1–4 with the per-service lock, **address allocation at `check` under the allocation lock, from a fresh `container_list` (all states) plus the DB**, IP confirmation from the runner's own inspect, health gate and fail-fast, supersede and cancel rules, transient retries, the pre-switch deadline, failure clean-up on fresh contexts, the failure rules (disk full, DB write before switch), shutdown before switch, the progress map, `deploy_stages`, `app_safe`; the `testhooks` build tag (pause points at every stage boundary, only in `internal/testbin` builds; a test checks the default build has none).
   - Tests (unit, fake `agentops`): `TestSupersedeDuringHealthGateIsUnhealthy`; `TestNameCollisionReusesOwnContainer`; `TestForeignNameCollisionFails`; `TestFailFastOnExitAndOOM`; `TestTransientPullRetryThenSuccess`; `TestDiskFullIsNotRetried`; `TestDBWriteFailureBeforeSwitchRemovesCandidate`; `TestShutdownBeforeSwitchRemovesCandidate`; `TestFirstDeployFailureIsUnknownAppSafe`; `TestRunnerRecordsCandidateWithoutTheWatcher`; `TestDockerTimeoutSetsServiceUnknown`; **`TestAllocatorNeverReusesAStoppedOrActiveContainersAddress`** (an exited active container, the stopped old container in a stop-first gap, an old container whose `stop_old` removal failed: none of their addresses is handed out); **`TestTwoConcurrentChecksNeverGetTheSameAddress`** (two services reach `check` at once; also with the allocation lock bypassed, the `deploys_ip_live` index refuses the second); **`TestForcedAddressCollisionNeverRoutesToAnotherService`** (a container of service Y forced onto service X's address: X's route serves the "not running" page, never Y); `TestAddressAllocatedOnlyAfterTheStartGate`; `TestAllocationFailsTransientlyWhenDockerIsUnreachable`; `TestPoolExhaustedFailsTheDeployWithTheOldVersionServing`. Integration: **`TestUnhealthyCandidateKeepsOldServingWithZeroNon2xx`** (an old version deployed by a direct DB + container fixture until 8b; constant load, keep-alive and new connections); **`TestStalledPullFails`**.
   - Reviewers: `reviewer`, `systems` (required), `security`.

8b. **Deploy runner: switch, commit, drain, stop old.**
   - Do: stages 5–9 through `applyRoutes`; `WithoutCancel` bounds for switch, commit, drain and stop_old; fresh contexts for re-applies; post-commit write failures as warnings; the deploy-log write never failing a deploy.
   - Tests: **`TestCommitOrder`** (recorded sequence: create, start, health, `proxy_apply`, commit transaction, drain, stop/remove old; no stop of the old container before the commit, in every path); **`TestSwitchAndCommitAreNotCancellable`**; **`TestDeadlineNeverCutsSwitchOrCommit`** (a job context that expires during `proxy_apply` and during the commit: both complete, then drain and stop_old run to their own bounds); `TestFailedSwitchReappliesOldRoutesWithAFreshContext`; `TestPostCommitWriteFailureIsAWarning`; `TestDeployLogWriteFailureDoesNotFailTheDeploy`; the deploy state-machine table test. Integration: **`TestBlueGreenSwitchWithZeroNon2xx`**; two services deploying at the same time both end live.
   - Reviewers: `reviewer`, `systems` (required), `security`.

9. **Stop-first, restart, redeploy, rollback, delete.**
   - Do: the stop-first stages, the maintenance override and the "restart the old container" path for every failure or cancel after `stop_old_first`; `services.restart` + `jobs.run_restart`; `services.redeploy`; `services.rollback` (once-live targets); `services.delete` + `jobs.run_service_delete` with cancellation at queue time and the drain cut.
   - Tests: stop-first serves the maintenance 503 with `Retry-After` during the gap, then 2xx; **a cancel after `stop_old_first` restarts the old container**; a failed stop-first restarts the old container (`app_safe=yes`); old one fails too → `app_safe=no` and ◆; rollback pins `image_id`; rollback to a deploy that was never live is refused; rollback with the local image removed pulls by digest; image gone → `image_gone`; redeploy recreates a missing container; restart keeps the address. **Delete:** tokens denied; needs fresh 2FA and the exact `Confirm`; **queuing the delete cancels the service's queued deploys and flags its running deploy; a running deploy that is past its commit skips the rest of its drain and stops the old container with its normal grace; then the delete runs**; **`TestDeleteAfterTheFreshWindowExpiresAsksToConfirmAgain`** (a long stop grace pushes the run past 5 minutes: the job fails with the "confirm again" reason and nothing is deleted); removes only containers labelled with this service and instance (a look-alike with another instance label survives); deletes the deploy-log directory; the project goes with its last service; interrupted → `needs_attention`.
   - Reviewers: `reviewer`, `systems` (required), `security` (required: delete).

10. **Reconciler: boot pass, events, resync, interrupts.**
    - Do: the watcher (events op, back-off, events-then-list, re-inspect of buffered ids); the state map and ◌ unknown rules; the DB-only boot pass; the resync rule set (`TryLock` on service locks, busy services skipped; only the current attempt protected; orphans with grace and log tail; start a stopped active container); the lock order and its test-only checker; the start gate; **bench: the "S2 live watcher" run (Docker socket mounted with its group) as the S2 number against the 80 MB tripwire, "S2 no Docker" kept as report-only, clean-up of `serverdock.managed` containers and the app network**; `reconciler.finish_deploy` and `reconciler.remove_container`; the `docs/security.md` "Unattended destructive actions" entry; the back-off Background work row.
    - Tests: **kill -9 at each stage** (`testhooks` pause, SIGKILL, fresh start) for every blue-green and stop-first stage: the app is reachable again after the restart; an uncommitted deploy is `failed (interrupted by restart)` and its containers are removed by the resync; a committed one is `succeeded` and its old container is removed with its log tail; a stop-first old container is running again; the `jobs.run_deploy` intent has exactly one outcome row with the user's actor and `executor = system`; **each run records the outage: the time from SIGKILL to the first 2xx from the app after the restart** (reported in the PR and research/05, QM3a-9). **`TestBootPassChangesOnlyTheDB`** (no Docker op before the first resync). **`TestSchedulerStartsOnlyAfterTheFirstResync`**. **`TestDaemonRestartKeepsRoutesCorrect`** (integration, CI only: two services, `sudo systemctl restart docker`; each public port answers 2xx from its own service again within 30 s, never from the other). **`TestEventsStreamDropTriggersResync`**. **`TestResyncSkipsAServiceWithARunningJob`** (a candidate in its health gate survives a reconnect); **`TestResyncNeverBlocksOnABusyServiceLock`** (a reconnect while a job holds a service lock finishes the other services and the routes at once, and the per-service resync after the job covers the skipped one); **`TestLockOrderIsNeverReversed`** (the checker over every runner, resync, watcher and delete test); `TestWatcherTakesOnlyTheRouteLock`; **`TestResyncRemovesAnOlderAttemptOfTheRunningDeploy`**; **`TestEventsDuringListAreReinspected`**; **`TestDieThenStopEndsExited`**; **`TestDieWithNoRestartIsReportedExited`**; `TestRestartingContainerKeepsItsUpstream`; `TestNoOpReconcileWritesNoAuditRows`; `TestOrphanRemovalOnlyTouchesOurInstance`; `TestOrphanRemovalRemovesOnlyAnonymousVolumes` (a named volume attached by hand survives); `TestStateUnknownWhileEventsAreDown`; `TestReconcilerActionsAreSystemOnlyAndListed`; `TestBackoffOnlyWhileDown`.
    - Footprint: the PR must report the first "S2 live watcher" number (against the 80 MB tripwire and the ≈ 52 MB estimate).
    - Reviewers: `reviewer`, `systems` (required), `security`, `footprint`.

11. **Deploy logs and progress reads.**
    - Do: capture through `logs` with `follow:false`; gzip files, modes, retention 10, 1 MiB cap; `deploys.list`, `deploys.get` (progress, `cancellable`), `deploys.log` (`write` scope for tokens).
    - Tests: retention keeps 10 per service; the cap holds with a container that logs 50 MiB; files 0600 in a 0700 directory (built binary); the candidate's tail captured before removal; the old container's tail after a success; an orphan's tail on resync; `deploys.log` output has no ESC, OSC, CSI, NUL or bidi override bytes from a hostile log; `read` token refused for `deploys.log`, `write` token allowed; deleted service → files gone; `deploys.get` progress while pulling.
    - Reviewers: `reviewer`, `security`, `footprint` (disk).

12. **UI: Services list, New service, Overview.**
    - Do: the sidebar's Services item; `/services`; `/services/new` (with the firewall and SEC6 hints on the public port); Overview summary, needs-attention list and exposure banner; Playwright fixtures (local registry and test app in the e2e job; seeded rows through `node:sqlite` for states that need no containers).
    - Screen states: Services empty; Services with 3 services in 2 projects (desktop table, phone cards) including ● running, ▲ exited, ◆ missing, ◌ unknown; New service empty, with field errors and the error summary, with the public-port hints, with the suggested port, Advanced open; Overview empty, with services, with the needs-attention list, with the exposure banner.
    - Tests (all 8 projects): zero axe violations on every state; create and deploy end to end (desktop and phone); **the Services list and Overview send no requests while idle** (request count over 30 s) and reload on Refresh; the hints linked to their field; typed input kept on errors; keyboard-only path; 44 px targets; no CSP violations.
    - Reviewers: `reviewer`, `accessibility` (required), `security`.

13. **UI: Service detail and its actions.**
    - Do: the detail page, Deploys and Settings sub-pages, the Deploy (tag or digest of the current repository), Redeploy, Restart and Roll back dialogs, the Settings form, the **Image repository** form, the **Public port** form with its confirmation dialog ("Applied now: exposes the app on the internet at …" / "… no longer reachable at …"), Delete with the typed target, the step-up, and the "Delete again" path.
    - Screen states: running service with history (desktop table, phone cards); route error; SEC6 warning; missing container; each dialog including both public-port confirmations; Settings saved; Delete dialog with the wrong and the right target; step-up prompt; delete failed "confirm again".
    - Tests (all 8 projects): roll back end to end (Roll back offered only on once-live deploys); the public-port change takes effect only after the dialog is confirmed and Cancel changes nothing; restart dialog states the downtime; delete needs the exact target and step-up, then lands on Services with a status message; dialogs name target and consequence, default focus on Cancel, Escape closes; zero axe violations.
    - Reviewers: `reviewer`, `accessibility` (required), `security`.

14. **UI: Deploy detail with live progress.**
    - Do: the deploy page, the stage list, the pull `<progress>`, polling (2 s, visible only, stops when finished), the announcements, Cancel with its disabled reason during switch, the log tail and "Show full log".
    - Screen states: queued; pulling; health gate running; succeeded; failed at health with app safe; failed with app not safe (stop-first); interrupted; superseded; cancelled; "waiting to retry the pull"; disk full; log no longer kept; status unknown (polling failed).
    - Tests (all 8 projects): a live deploy announces each stage transition once and nothing for progress ticks; a failed deploy announces once through the alert; polling stops when hidden and when finished, and backs off to 5 s after 2 minutes of pulling (request count, `page.clock`); Cancel works while pulling and shows its reason during switch; a hostile log line renders as text; zero axe violations; no CSP violations. **Exit item:** a manual screen-reader pass of steps 12–14 (checklist written in this step), recorded in the PR.
    - Reviewers: `reviewer`, `accessibility` (required), `security`.

15. **Record and close.**
    - Do: `docs/security.md` (M3a surface table; the A4 rows "Planned (M3a policy)" → Built with tests; rule 1 validators; the G13 partial; the unattended list; **the accepted risks B10 (deploy = host network reach, in plain words), QM3a-9 (apps offline while ServerDock is down), N13 (pre-28 Docker LAN routing, after `researcher` confirms the versions), `system.exposure` names, deploy logs holding app credentials, the registry-host deferral**; the M3a close-out); ADR 0012 → accepted after `security` approval; **amend plan 0002's M3a section: the interrupt rule (line 281: a deploy interrupted after its commit is finished, not rolled back) and its test line (line 297)**; `docs/architecture.md` (deploy flow for image sources, strategies as built, static addresses, Background work rows, layout entries for `internal/jobs`, `internal/deploy`, `internal/policy`, `internal/agentops`); `docs/roadmap.md` ticks; plan 0002 M3a status; bench numbers in `bench/results/` and research/05: CI "S2 live watcher" and "S2 no Docker", full S4, **"S2 with one exposed service" (gated against the 80 MB tripwire)**, **"S2 with 10 exposed idle services" (RSS and idle CPU)**, **RSS sampled for 60 s after a deploy finishes (back to the idle baseline)**, binary, SPA JS, and the measured restart outage; **`researcher` confirms which Docker Engine versions before 28 let LAN hosts reach unpublished container ports, and the accepted-risk wording**; the recordings named in the exit evidence.
    - Tests: links resolve; CI green (unit, integration, e2e, bench); full S4 green.
    - Reviewers: `reviewer`, `security`, `systems`, `footprint`, `accessibility`.

## Test plan

- **Unit:** everything named in the steps, with fake dockerd and fake `agentops` for the runner and the reconciler.
- **Integration (`-tags integration`, real Docker, `registry:2` on loopback with `--allow-loopback-registry`, the test app image):** Docker client calls; agent ops; zero non-2xx with an unhealthy candidate and across a healthy blue-green switch (about 50 requests/s, keep-alive and new connections); two concurrent deploys; stop-first gap; Caddy reloads under load; stalled pull; kill -9 at every stage with restart and the measured outage; **Docker daemon restart with two services** (CI only); events-stream drop and resync; delete touches only our containers; goleak.
- **Route matrix:** every new action × 24 credential cases plus the app-origin case; `JobOnly` and `ConsoleOnly` 404 everywhere; no `system` actor from HTTP; token scopes per QM3a-4.
- **Sentinel suite:** unchanged and green.
- **Fuzz:** image references, names, health path, `CheckCreate`, v1 request and response lines, Docker pull and events lines: seeds in CI, a 5-minute local run before merge.
- **Playwright + axe:** all 8 projects; every listed screen state; real deploy, rollback, public-port and delete flows on desktop and phone; seeded states for the rest; manual screen-reader pass (steps 12–14).
- **CI bench:** S2/S4 on every Go PR ("S2 live watcher" is the gated S2 from step 10, "S2 no Docker" report-only); QUICK S4 at steps 1–4a; at step 15 full S4, "S2 with one exposed service" (tripwire), "S2 with 10 exposed idle services" (RSS, idle CPU), RSS 60 s after a deploy.
- **Manual 1 GB VPS check (informational; Q6 still open):** if a 1 GB VPS is available, step 15 deploys `nginx` with a public port, runs a blue-green redeploy and a failing deploy under a `curl` loop (no non-2xx), rolls back, restarts Docker, restarts ServerDock (records the outage), and records idle VmRSS with zero and with one service in research/05; also the M2 hand-off first run from a phone and a desktop.

## Reviews required

- `reviewer`: this plan and every step's diff.
- `systems` (**required for M3a**): this plan; steps 1, 2, 3, 4a, 4b, 6, 7, 8a, 8b, 9, 10, 15.
- `security` (**required**): ADR 0012; every step.
- `footprint`: steps 1, 2, 3, 4a, 6 (the ticker exception), 10, 11, 15.
- `accessibility` (**required on every UI step**): steps 12, 13, 14, 15.

## Maintainer answers (2026-11-21)

- **QM3a-1 — confirmed 2026-11-21 (maintainer: "Confirmed, start M3a"):** confirm `docs/vision.md`, `docs/features.md` and `docs/architecture.md` (plan 0002 Part D item 9) before M3a starts.
- **QM3a-2 — decided (a):** an optional per-service public port, plain HTTP through Caddy, 1024–65535, never 80, 443 or ServerDock's own ports. The host firewall applies on top. Setting or changing the public port needs a confirmation dialog ("Applied now: exposes the app on the internet at …").
- **QM3a-3 — decided (a):** warn about image-declared volumes; anonymous volumes are removed together with the old container; orphan removal removes only anonymous volumes.
- **QM3a-4 — decided, narrowed (a):** `write` tokens may deploy (`services.deploy` may change only the tag or digest of the current repository), redeploy, roll back, restart and cancel. `services.create`, changing the repository or registry, and setting or changing the public port need a session or an `admin` token. Delete stays session-only. `deploys.log` needs `write` scope for tokens.
- **QM3a-5 — decided (a):** minimal projects (a name, created with the first service, removed with the last).
- **QM3a-6 — decided (a):** 2 s polling while the deploy runs and the page is visible; no WebSocket route in M3a.
- **QM3a-7 — decided (a):** ServerDock runs on the host or with host networking in M3a; the container form is decided with Q4 in M7a.
- **QM3a-8 — decided (a):** the Docker client ADR is 0012; later reservations shift by one; step 1 updates the references.
- **QM3a-9 — decided, accepted for Phase 1:** apps are offline while ServerDock restarts or crashes (Caddy is embedded). Recorded as an accepted risk; step 10's kill -9 test records the gap; revisited with M7b self-update.

The defaults in the design (health check `GET /`, 60 s, 3 passes 1 s apart; drain 10 s, never below 10; stop grace 10 s; 2 workers; pull stall 120 s; 3 pull attempts; 1 GiB free-space floor; 10 deploy logs per service; 100 services; PID limit 4096) are the planner's choice; the maintainer may change any of them.

## Risks

| # | Risk | Likelihood | Mitigation |
|---|---|---|---|
| R1 | A Caddy reload drops requests during the switch | low | zero-non-2xx integration tests at steps 6 and 8b; `grace_period` 10 s; `drain_s` ≥ 10 |
| R2 | A port taken by another program blocks every route | medium | per-port bind isolation; panel loaded alone first; honest route error |
| R3 | Docker API behaviour differs across Engine 25–29 | medium | typed bodies with fields stable since 1.44; integration on CI's Docker; RC matrix in M3b |
| R4 | Disk fills with old images before M8 cleanup | medium on small disks | the 1 GiB free-space check before each pull; documented; M8 cleanup |
| R5 | ServerDock in a container can't reach apps | medium for container users | QM3a-7; the health gate says why |
| R6 | The runner and reconciler are the most complex code so far | high | commit-order and stage tables, the deadlines table, one route lock, DB-only boot pass + one resync rule set, `testhooks`, kill -9 at every stage, `systems` required |
| R7 | The events stream stays connected but stops delivering | low | resync on every reconnect and after each job; the runner inspects the active container before each deploy |
| R8 | Docker Hub rate limits in CI | medium | local `registry:2` and a CI-built test image |
| R9 | Each exposed service adds a 10 s Caddy ticker | certain, small | written rule 3 exception, `footprint` sign-off; revisit if it shows in idle CPU |
| R10 | Footprint noise (THP, file pages) | medium | QUICK and full S4 runs per rule 4; ADR 0011 tripwires decide |
| R11 | The chosen app subnet collides with a LAN or VPN route added later | low | the subnet is chosen against current interfaces and routes; `--app-subnet` at first start; changing later is out of scope (documented) |
| R12 | Apps are offline during ServerDock restarts | certain | accepted (QM3a-9); measured in step 10; M7b revisits |

## Traceability

### Roadmap items → steps

| Roadmap item (plan 0002 Part C) | M3a part | Steps |
|---|---|---|
| 4 Deploy from image | public images, one container per service | 2, 3, 5, 7, 8a, 12 |
| 6 Blue-green, health gate, automatic and one-click rollback, auto-cancel superseded | all | 4a, 6, 8a, 8b, 9, 10, 13, 14 |
| 14 Stop-first; connection draining | stop-first (explicit), drain | 8b, 9 |
| 15 Deploy logs, pull progress, restart, redeploy | all four | 3, 9, 11, 14 |
| 23 Accessible, phone-usable UI | the new screens | 12, 13, 14 |

### Plan 0002 "plan 0004 must" → where

| Requirement | Where |
|---|---|
| Op protocol v1: one stream per op, request → progress → result, `hello` with version, all deploy ops `LocalOnly` | Agent op protocol v1; steps 1, 3 |
| Cap line and progress sizes; agent output untrusted | Agent op protocol v1 (caps, strict decode, trailing bytes, `CleanText`); step 1 |
| ADR for the Docker client | Appendix A (ADR 0012); step 1 |
| Jobs: lock key, state, attempts, cancel flag, timeout, payload; one "next due" timer + notify | Data model `jobs`; Jobs and the scheduler; step 4a |
| Jobs record the queuing actor; policy at enqueue and on every action; `system` only as executor | Jobs run as the actor that queued them; step 4b |
| Test: a job queued by a token can't reach an action that token is denied | `TestJobQueuedByTokenCannotReachADeniedAction`, step 4b |
| Per-service mutex + global worker limit 1–2 | Jobs (2 workers, partial unique index, service lock); step 4a |
| Commit order; old stopped only after commit | Blue-green table; `TestCommitOrder`, step 8b |
| Switch and commit not cancellable; supersede during health = unhealthy | Blue-green table; Deadlines; steps 8a, 8b |
| Boot: interrupted deploys rolled back, routes follow `active_deploy_id`, marked interrupted, no resume | Reconciler boot pass (**refined:** a committed deploy is finished; plan 0002 amended in step 15) |
| Names with deploy + attempt, 409 → inspect | Concepts (plus labels and image id); steps 3, 8a |
| Pull deadline and stall timeout | Deadlines; `image_pull`; `TestStalledPullFails` |
| Caddy routes generated from DB desired state, never patched | App routes (one route lock, override only between switch and commit); step 6 |
| Upstreams regenerated on `start` events, boot, every events reconnect (full list + resync) | Reconciler; static addresses make the upstream stable; step 10 |
| Weigh `127.0.0.1` ports vs container IPs | Upstream addressing (decision: static container IP) |
| Reconciler: no-op passes not audited; resolves `unknown` intents | Reconciler; step 10 |
| Immutable snapshots; blue-green default; stop-first | Data model (trigger); Stop-first |
| Health gate HTTP or TCP from the agent | `health_check` op (+ "stays running") |
| Deploy logs: gzip, last 10, capped | Deploy logs; step 11 |
| Default-deny policy shared with M3b/M6; user `serverdock.*` labels rejected incl. image deploys | Policy; step 5 |
| Fuzzed validators; only labelled resources deleted; label spoofing rejected | Policy; ownership rule; steps 3, 5, 9, 10 |
| Tests: state machine, commit order, supersede during health, name collision; zero non-2xx; kill -9; daemon restart; events drop; stalled pull; Playwright | steps 8a, 8b, 10, 12–14 |
| Background rows: scheduler timer, events reconnect back-off | Footprint › Background work rows |
| Footprint S2 → ≤ 56 MB | Footprint budget |
| Exit evidence: zero-downtime and interrupt recordings; daemon-restart test green; bench green | step 15 |

### M2 hand-off obligations

| Obligation (plan 0003) | Where |
|---|---|
| Warn when an app publishes a host port on the panel's IP (SEC6) | SEC6 (our services, other containers, the app-origin matrix case); steps 7, 12, 13 |
| WebSocket routes must pass session + exact `Origin` and join the route matrix | none added (QM3a-6); the rule carries to M8 |
| Once apps share `:443`, `fallback_sni` must not serve the panel leaf for app SNI names | M3a apps can never use 443 or the panel port (`TestPanelConfigUnchangedWithApps`); the SNI test stays with M4 (plan 0006) |
| The optional manual 1 GB VPS first run from a phone and a desktop | Test plan › manual 1 GB VPS check |

### Notes this plan hands to later plans

- **Plan 0005 (M3b):** `deploys.log` output goes through the secret-value redactor, and a sentinel test must cover `deploys.log`, `deploys.get` reasons and stage details; registry-host rule-10 checks with credentials; `no-new-privileges` as a per-service option; automatic stop-first for writable volumes.
- **Plan 0009 (M7a):** `doctor --bundle` excludes `deploy-logs/`; `doctor` checks whether `meta.app_subnet` overlaps a host route, interface or Docker network and says to set `--app-subnet`; the unit sets a negative `OOMScoreAdjust` for ServerDock so the kernel kills an app before the process that serves every app's traffic.
- **Plan 0010 (M7b):** pause the scheduler (built here); the trial boot runs the resync rules' route part only; revisit QM3a-9.

---

# Appendix A — ADR 0012: Docker client and agent op protocol v1

*Step 1 copies this section to `docs/decisions/0012-docker-client-and-op-protocol.md`.*

# 0012 — Docker client and agent op protocol v1

**Status:** proposed (plan 0004; awaiting `security` and `systems` review) · **Date:** 2026-11-21

**Context.** M3a is the first milestone that changes Docker state: it pulls images, creates, starts, stops and removes containers, follows Docker events, and reloads the embedded Caddy with app routes. Plan 0002 reserved an ADR for the Docker client and required a versioned op protocol between the control plane and the agent, with size caps and agent output treated as untrusted. M1 built a small typed client (`internal/agent/dockerapi`) with per-call deadlines and API negotiation (Engine 25+, API 1.44–1.52), and an op table in which the zero value is local-only (ADR 0009). Remote agents are logs-only until Phase 2 (Q1).

**Decision.**

1. **Keep the hand-rolled, typed Docker client.** No Docker SDK module: it would add a large dependency tree, package `init` work in every mode (S4), and API churn we don't use. Every endpoint is a typed method with a fixed path; every request goes through `call` (a per-call deadline) or `stream` (bounded by its context, with a header deadline). Request bodies are Go types that contain only the fields ServerDock is allowed to set, and the final create body passes a default-deny check down to every leaf, in the control plane and again in the agent on the exact bytes sent to Docker. Docker's JSON lines are read with a 64 KiB per-line cap.
2. **Ownership by label, enforced in the agent.** Every container ServerDock creates carries `serverdock.managed`, `serverdock.instance` (a random id per data directory, set by the agent itself), `serverdock.project`, `serverdock.service`, `serverdock.deploy` and `serverdock.attempt`, runs a pinned image id, and is named `sd-<project>-<service>-d<deploy>-a<attempt>` with ids that are never reused. The agent stops, restarts or removes only containers with its own instance label, addressed by a full id it inspected. Images that carry any `serverdock.*` label are refused.
3. **One app network per install with static addresses.** A bridge network named after the install id, created with a subnet ServerDock chose, labelled, with inter-container traffic disabled in M3a. Every container gets a static IPv4 recorded with its deploy, so an upstream never moves to another container after a container or daemon restart. ServerDock publishes no host ports for apps; Caddy is the only entry point.
4. **Op protocol v1.** One yamux stream per op. The hub writes one request line (at most 64 KiB) `{"v":1,"op":…,"args":…}`; the agent answers with progress or event lines (at most 1 KiB each) and exactly one result line (at most the op's limit). Both sides refuse trailing bytes and unknown fields. The hub enforces per-op limits on line size, line count and total bytes, and cleans every string that came from Docker, a registry, an image or a container. A violation fails the op and closes the stream. A `hello` op (read-only, allowed on remote sessions, result at most 256 bytes) carries the protocol version; an agent that does not know it is treated as v0 (`logs` only). The v0 `logs` op is unchanged apart from an optional `follow` that is never sent to a v0 agent.
5. **All new ops are local-only, on both sides.** The hub refuses them to any session other than the in-process local agent, and `serverdock agent` (remote mode) does not serve them. The remote allowlist is `hello` and `logs`; the negotiated version never widens it.
6. **The control plane owns the events reconnect.** It keeps one `events` op open on the local session (no op timeout), reconnects with a 1 s → 30 s jittered back-off while the stream is down, and on every (re)connect opens the stream first, then lists ServerDock's containers and re-inspects those with events buffered during the list.
7. **The commit is authoritative after a crash.** A deploy interrupted by a restart after it set the service's active deploy is finished (the old version is removed), not rolled back; an uncommitted one is marked failed and its containers removed. The boot pass changes only database state; one resync rule set, run after every successful events connection, does all container clean-up. This amends plan 0002's M3a interrupt rule.

**Consequences.**
- No new Go dependency; later milestones add endpoints (networks and volumes in M3b, image load in M5) under the same rules.
- A control-plane bug cannot delete containers that ServerDock did not create, two installs on one Docker host don't interfere, and a new service can never adopt an old service's container.
- ServerDock must reach the app network: it runs on the host or with host networking until the install shape (Q4) decides the container form. The app subnet is chosen at first start and not changed later.
- The per-op limits are part of the protocol; raising one is a protocol change reviewed by `security`.
- Idle cost: one events stream (one Docker connection and two goroutines); no timers of ours while the stream is up.

## Revision 6 (step 4a implementation notes)

Recorded after step 4a and its reviews; later steps follow these.

- **The scheduler is built and tested but not started in `runServer`.** Its start gate is opened by the step 10 reconciler after the first successful resync. The step 10 boot pass must resolve interrupted `running` jobs, call `Scheduler.Notify`, and keep the gate closed until it has finished.
- **`NetworkSubnets` is called directly by the server** (`dockerapi.Client.NetworkSubnets`, `GET /networks`): there is no agent op for it. It is local-only, bounded (4,096 networks, the 4 MiB body cap, the short deadline) and cleaned (`agent.CleanText`).
- **Extra index** `deploys_job` (partial, on `job_id`) so pruning jobs does not scan `deploys`.
- **CHECK constraints on `jobs.state` and `deploys.status` are kept.** A later new status needs a table-rebuild migration; accepted.
- **Cancel and supersede (review fixes):** a cancel that arrives between Pick and worker registration is held in a pending set and applied at registration; `Requeue` refuses a job with `cancel_requested = 1` (the job is finished cancelled); cancelling a queued deploy job, or superseding a queued or retry-pending one, ends its deploy row in the same transaction; a running deploy gets `superseded_by` only from its runner.
- **`--app-subnet`** must be RFC 1918 or 100.64.0.0/10, and an overlap with the host or Docker refuses to start and saves nothing.
- **Restored backups share `install_id`** (and the app subnet): the backup/restore docs (plan 0009) must say that restoring a copy onto a second host is a clone with the same identity, and two live clones on one Docker host would act on each other's containers.
- **`recordAppSubnet` can block startup up to 10 s** when Docker is down; the server then starts without a recorded subnet.
- **Local bench (Docker Desktop arm64, informational; compare like-for-like with the step 3 local QUICK run: S2 55.7 MB, S4 47.2 MB, not with CI):** QUICK S2 55.0 MB (−0.7), QUICK S4 49.7 MB (+2.5). Because the S4 QUICK delta was over 0.5 MB (rule 4), a **full S4** ran (3 runs, 120 s warm-up, 300 s idle): runs 46.0 / 47.5 / 45.1 MB, **median 46.0 MB** (−1.2 against the step 3 QUICK), idle CPU 0.01 %, binary gzip 17.51 MB (+0.05 MB, agentops and deploy now linked). The QUICK +2.5 MB was single-run noise; the remote agent links nothing new in this step. CI numbers on the PR remain the gate (CI baselines S2 50.3, S4 44.7).

## Revision 7 (step 4b implementation notes)

- **No new background work.** Step 4b adds no goroutine, timer, cache or dependency: the actor encoding, `RefreshJobActor` and `JobOnly` are synchronous code on existing paths, and `CancelSessionJobs` is one bounded transaction run inside `auth.sessions_revoke`. No bench was run for this step.
- **`RefreshJobActor` is a method** on `auth.Store` (it reads the database). The credential comparison fails closed on a tie (a change in the same second as the enqueue stops the job).
- **The `ActionDeps.Jobs` canceller is not wired in `runServer` yet**, because the scheduler is built and started by steps 8a and 10 (Revision 6). The wiring is a one-line addition there; tests build the real scheduler and canceller. Until then no job can exist.
- **Cancel failure is reported.** If the sessions were revoked but cancelling their jobs failed, `auth.sessions_revoke` returns an error saying so (the revoke itself stands).
- **`JobOnly` and `ConsoleOnly` exclude each other**; `JobOnly` also refuses the `SystemInvoker`'s own `Invoke` and `InvokeAs` with the system actor.
- **Scheduler nits from the 4a systems review:** a job leaves the cancel map before its result is recorded (a separate `active` count keeps the worker limit and `Running()` honest), and when `Requeue` refuses because of a cancel, the job finishes with the row's `cancel_reason` (for example `superseded`).

