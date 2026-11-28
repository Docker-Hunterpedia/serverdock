# 05 — Phase 0 measurements

Status: **CI done; VPS run deferred.** The full CI bench passed every gate on 2026-11-20. No 1 GB VPS is available yet, so the maintainer deferred the real-VPS run to the Phase 1 exit (which already requires a fresh 1 GB VPS). Phase 0 exits on the CI numbers.

Written by the implementer agent (AI), 2026-11-20, from plan 0001 steps 1-7. Not confirmed by the maintainer.

## Method (what the numbers mean)

`bench/run.sh` starts each scenario in a container with `--memory 1g --cpus 1` (distroless nonroot image), warms up, sends a 500-request burst, then samples **from outside** the container: `VmRSS`/`VmHWM`/`RssAnon`/`RssFile` from `/proc/<pid>/status`, cgroup `memory.current`/`memory.stat`, and CPU from the `cpu.stat usage_usec` delta over the idle window. Gates use the median of per-run medians, with +5% tolerance on memory and +0.2 points on CPU. Memory is in decimal MB (1 MB = 1,000,000 bytes), matching `docs/architecture.md`.

## Local run: Docker Desktop macOS arm64 (QUICK, informational only)

- Date 2026-11-20; `QUICK=1`: 20 s warm-up, 60 s idle window, **1 run per scenario**, 7 samples each.
- Measured through the `--pid=host --privileged alpine` helper container inside the Docker Desktop VM, so results are informational only (VM kernel, shared resources, different page-fault behaviour from a real VPS).
- Binary (linux/arm64, `-s -w`): 48.1 MB raw, 16.9 MB gzip -9 (gate: 40 MB gzip, pass).

| Scenario | VmRSS (MB) | RssAnon | RssFile | VmHWM | Idle CPU | Plan gate (informational) |
|---|---|---|---|---|---|---|
| 1 server, `--proxy=false` | 45.8 | 13.4 | 32.5 | 45.8 | 0.00% | 40 MB (+5% = 42): **above** |
| 2 single node (server + local agent + Caddy) | 52.5 | 18.0 | 34.4 | 54.6 | 0.00% | 80 MB: within |
| 3 remote agent | 40.7 | 10.4 | 30.3 | 40.7 | 0.00% | 20 MB (hard fail 25): **above** |
| 4 remote agent + own Caddy (report only) | 49.8 | 17.3 | 32.5 | 49.8 | 0.00% | target 50 MB: at target |

Observations:

- Most resident memory is **file-backed** (`RssFile` 30-34 MB), i.e. pages of the 48 MB binary that were touched, not Go heap (`RssAnon` 10-18 MB). The same large binary is used for every mode, including `--proxy=false` and the remote agent, so scenarios 1 and 3 carry the weight of everything linked in (Caddy, modernc SQLite, otel/cel dependencies pulled in by `caddyhttp`).
- Scenario 2 minus scenario 1: **+6.7 MB VmRSS** (+4.6 MB RssAnon). The marginal cost of running Caddy is small. The baseline of the shared binary is the problem for scenarios 1 and 3, not Caddy's runtime.
- Idle CPU reads 0.00% in all four; the 60 s window is too short to say more.
- Whether `RssFile` behaves the same on a real Linux host is unknown. File-backed pages can be reclaimed under pressure, but the plan gates on VmRSS.

## CI (gate): full run, 2026-11-20

[Run 44715013248](https://github.com/Docker-Hunterpedia/serverdock/actions/runs/44715013248) on PR #1. Full method: 3 runs per scenario, 2 min warm-up, 500-request burst, 5 min idle at 10 s intervals (31 samples), `--memory 1g --cpus 1`, Linux direct measurement path. VmRSS is the median of the per-run medians; RssFile is from the middle run. ubuntu-latest (amd64) gates; arm64 reports only.

| Scenario | amd64 VmRSS | amd64 RssFile | arm64 VmRSS | Idle CPU | Gate (ADR 0008) |
|---|---|---|---|---|---|
| 1 server, `--proxy=false` | 44.8 MB | 34.6 | 42.4 MB | 0.00% | report only (target 40): above |
| 2 single node | **47.6 MB** | 36.5 | 45.1 MB | 0.00% | ≤ 80 MB (+5%): **pass** |
| 3 remote agent, no proxy | 40.6 MB | 33.0 | 38.1 MB | 0.00% | report only (target 20): above |
| 4 remote node, agent + Caddy | **44.1 MB** | 35.1 | 43.0 MB | ≤ 0.01% | ≤ 50 MB (+5%): **pass** |

Binary gzip -9: 18.8 MB amd64, 16.9 MB arm64 (gate 40 MB): pass.

Reading: anonymous memory (RssAnon) is only 7–11 MB in every scenario. The rest is file-backed pages of the shared binary, which matches the local run and the reasoning in ADR 0008. Embedded Caddy costs about +2.8 MB VmRSS (scenario 2 minus scenario 1, amd64). The slim agent should bring scenario 3 close to its 20 MB target mainly by shrinking RssFile.

## Real 1 GB VPS: deferred to the Phase 1 exit

Decided by the maintainer on 2026-11-20: no 1 GB VPS is available yet. CI already runs every scenario in a container capped at 1 GB and 1 CPU on a real Linux kernel, which is the closest stand-in we have. The full bench (`bench/run.sh`, 3 runs, 2 min warm-up, 5 min idle) runs on a fresh 1 GB VPS as part of the Phase 1 exit, and its numbers get published there. A VPS result over budget reopens ADR 0004 and ADR 0008.

## ADR 0004 verdict: provisional

Based only on the local scenario 2 minus scenario 1 difference (+6.7 MB VmRSS, +4.6 MB RssAnon): **keep embedded Caddy for now**; its marginal cost is far below the ~20-30 MB assumed in ADR 0004, and live reload with the admin API disabled works (`caddy.Load` twice and a listener move, tested). The Caddy-container fallback is **not** indicated by this data.

The same data raises a separate question for the reviewers: the single large binary already exceeds the scenario 1 (40 MB) and scenario 3 (20 MB hard fail 25) gates in this informational run, mainly through file-backed pages. Options to evaluate after the CI and VPS numbers: build the remote agent without the proxy and SQLite packages (build tags or a separate `main`), trim linked Caddy modules further, and compare `RssFile` on a real host. This verdict is provisional until the CI and VPS sections above are filled in.

**Follow-up (2026-11-20):** the maintainer accepted [ADR 0008](../decisions/0008-footprint-gates.md). CI now gates scenarios 2 (≤ 80 MB) and 4 (≤ 50 MB); scenarios 1 and 3 are report only until the Phase 2 slim agent.

## M3a: outage while ServerDock is killed and restarted (QM3a-9)

Apps are offline while ServerDock is down, because the proxy that serves them is embedded in the ServerDock process (QM3a-9, accepted for Phase 1). Plan 0004 step 10 measures it: for each stage of a blue-green and of a stop-first deploy, the `kill -9` suite (`TestKillNine...` in `cmd/serverdock`, tag `integration`; the test builds the server binary with `testhooks` itself) kills the server at that stage with SIGKILL, starts it again on the same data directory and Docker, and logs one parseable line per run:

```
OUTAGE strategy=<blue_green|stop_first> stage=<stage:before|after> ms=<SIGKILL to the first 2xx from the app after the restart>
```

Measured on commit `e76aa77` (plan 0004 Revision 16), 3 runs (`-race -count=3`, three scenarios in parallel per run), in a 4-vCPU cloud container (Intel Xeon 2.10 GHz), Docker 29.10.0 with the overlayfs storage driver, a local `registry:2` on loopback, and the test app's `stop_grace_s` of 2 s. The restart uses the default Docker socket. Every run also checks that the right container answers, the deploy's end state and containers, the single outcome row of the `jobs.run_deploy` intent (the user's actor, executor `system`), and, for stop-first, that containers of two deploys never run at the same time (sampled with `docker ps` until settled).

| Strategy | Stage | Run 1 (ms) | Run 2 (ms) | Run 3 (ms) | Notes |
|---|---|---|---|---|---|
| blue-green | pull:before | 60 | 61 | 59 | |
| blue-green | check:before | 59 | 64 | 59 | |
| blue-green | start:before | 60 | 60 | 86 | |
| blue-green | health:before | 88 | 60 | 60 | the candidate is removed after the restart |
| blue-green | switch:before | 61 | 99 | 66 | |
| blue-green | commit:before | 64 | 90 | 74 | uncommitted: the old version keeps serving |
| blue-green | commit:after | 61 | 99 | 90 | committed: the new version serves, the old one is removed |
| blue-green | drain:before | 61 | 62 | 63 | |
| blue-green | stop_old:before | 62 | 61 | 60 | |
| stop-first | pull:before | 58 | 98 | 64 | |
| stop-first | check:before | 63 | 63 | 63 | |
| stop-first | maintenance:before | 68 | 61 | 88 | |
| stop-first | stop_old_first:before | 59 | 118 | 59 | |
| stop-first | start:before | 278 | 345 | 232 | the stopped old version is started again |
| stop-first | start:after | 2388 | 2519 | 2363 | the candidate is stopped (grace 2 s) before the old version starts |
| stop-first | health:before | 2573 | 2451 | 2410 | as start:after |
| stop-first | switch:before | 2425 | 2502 | 2403 | as start:after |
| stop-first | commit:before | 2481 | 2463 | 2425 | as start:after |
| stop-first | commit:after | 62 | 69 | 60 | committed: the new version serves |
| stop-first | stop_old:before | 72 | 67 | 59 | |

The outage is about 60–100 ms (the server's own start and its first route apply) except where the old stop-first version must be started again. Where a stop-first candidate exists, **two versions never run**: the candidate is stopped first, with its `stop_grace_s` capped at 10 s, so on a real service those four stages cost up to that cap plus the old version's start. The CI run of the same suite on `e76aa77` gave the same shape (stop-first start:after 2,292 ms, health:before 2,272, switch:before 2,299, commit:before 2,460; every other stage 58–220 ms).

**Docker daemon restart** (`TestDaemonRestartKeepsRoutesCorrect`, two services, CI only): in CI (`sudo systemctl restart docker`, GitHub-hosted Ubuntu runner) both public ports answered 2xx from their own service again **12.1 s** after the restart began, never from the other one. A local run in the cloud container (no systemd; a script that restarts `dockerd`) took 20.6 s and 23.4 s. The bound is 30 s.

## M3a release measurements (step 15)

Status: **local runs recorded; CI gate recorded 2026-11-28 (S2 live watcher 54.0 MB, S4 46.5 MB, both pass).** The numbers below are from local runs in a cloud container (amd64, `--memory 1g --cpus 1` containers, 3 runs each, median of the per-run medians), **not the CI gate and not a 1 GB VPS**. The CI "S2 live watcher" value is the release gate; its row and the CI S4 row come from the bench job on the ready PR #6. Scenarios are described in `bench/run.sh`; the result file is [bench/results/2026-11-27-m3a-release.md](../../bench/results/2026-11-27-m3a-release.md).

Gates (ADR 0011): S2 ceiling 200 MB, tripwire 80 MB; S4 ceiling 100 MB, tripwire 50 MB. VmHWM is a peak, is reported and is not gated.

**Run:** bench images built from commit `92186ad` (S2, S4) and `197c847` (S5 to S9); the bind-isolation fix `d5f0327` came after these images were built, so these numbers do not include it. Date 2026-11-27. Temporary vendor and cgroup shims were used for the local runs (never committed).

### Footprint

| Measurement | Scenario | VmRSS (MB) | VmHWM (MB) | Idle CPU | Gate | Result |
|---|---|---|---|---|---|---|
| Full S4 (remote node: agent + Caddy) | 4 | **47.3** median (47.0, 47.3, 47.8) | max 50.2 | 0.01% | tripwire 50 MB, ceiling 100 | under both; 1.8 MB over the 45.5 MB planning figure (file-backed pages of the larger binary; no agent-mode code added) |
| Step 13 build, same session (A/B) | 4 | 48.0 median (48.0, 48.0, 47.7) | 50.0 | | none | the step 13 build (50.0) and the final build (50.2) differ by less than the run-to-run spread: noise |
| **S2 live watcher (CI, the release gate)** | 2 | **54.0** median (54.0, 54.8, 53.8) | max 57.7 | 0.00% | tripwire 80 MB; planning figure 56 MB | **pass** ([CI run 46147556661](https://github.com/Docker-Hunterpedia/serverdock/actions/runs/46147556661), commit `5eded76`, 2026-11-28) |
| S4 (CI) | 4 | **46.5** median (46.5, 47.7, 45.9) | max 49.4 | 0.00% | tripwire 50 MB, ceiling 100 | **pass** ([CI run 46147556661](https://github.com/Docker-Hunterpedia/serverdock/actions/runs/46147556661), commit `5eded76`, 2026-11-28); the S4 base of plan 0002 Revision 1 |
| S2 live watcher, local cloud container (not the gate) | 2 | 54.7 median (53.3, 55.5, 54.7) | 57.6 | 0.01% | tripwire 80 MB | under |
| S2 no Docker (report only) | 2 with `S2_DOCKER=0` | 53.7 | 57.3 | | none | reported |
| S9: same setup, 0 services (the baseline of S5 and S6) | 9 | 54.0 | about 71 | | none | reported |
| S2 with one exposed service (gated) | 5 | 56.7 (+2.7 over S9) | about 71 | 0.01% | tripwire 80 MB | **pass** |
| S2 with 10 exposed idle services (report only) | 6 | 57.5 (+3.5 over S9) | about 71 | 0.01% | none (reported against 80 MB) | under; ten services cost about 0.8 MB more than one |
| One redeploy | 7 | baseline 56.0 · peak 56.3 (+0.3) · +60 s 56.3 | | | budget 5 MB | within |
| Two concurrent redeploys | 8 | baseline 55.6 · peak 57.3 (+1.7) · +60 s 57.3 | | | budget 5 MB | within |

Reading S5 to S9: VmHWM is about 71 MB in S5, S6 and S9 alike, so it comes from the shared setup (sign-in, argon2, the API session), not from the services. All are under the 80 MB tripwire (VmHWM margin about 9 MB). **S5 minus S2 is not the cost of one service**; minus S9 is.

Reading S7 and S8: at +60 s the value equals the peak, so nothing grew after it. Baselines were unstable (3 to 4 MB of settling) in 2 of 3 runs, and in S8 2 of 3 runs were not within 1 MB of the baseline at +60 s. We read this as baseline settling, not a leak, because the post-redeploy level matches the S6 steady state. **"Back to baseline within 60 s" is not demonstrated at 1 MB resolution for S8.** Redeploys used cached layers, so pull-time buffers are not measured.

Scenarios 5 to 9 run locally, are not part of the CI bench job, and run the server with host networking and `--allow-loopback-registry`, so they are not the shipped deployment shape.

### Size

| Measurement | Value | Gate | Result |
|---|---|---|---|
| Binary, gzip -9 (amd64) | 20.0 MB (54.9 MB raw) | 40 MB | pass |
| SPA first-load JS (gzip) | 41.5 KB first load; 115.0 KB total | 250 KB | pass |

### Restart outage (QM3a-9)

Measured: see [the section above](#m3a-outage-while-serverdock-is-killed-and-restarted-qm3a-9) (blue-green 59 to 99 ms at every stage; stop-first 58 to 118 ms except the gap stages at about 2.4 s with a 2 s grace; a Docker daemon restart 12.1 s in CI; commit `e76aa77`, with one or two services). For 100 services, the boot cost before the first app is routed is in the reload table below (about 95 ms for the list, 100 inspects and the load in the test rig).

### Remaining reload cases with 100 routes (plan 0004 Revision 11, step 15)

A switch, revert, restart, rollback and the maintenance page never reload the proxy (zero-drop tests). The cases below still do. Measured on commit `6ada1ea` (the `d5f0327` fix plus comments), `SD_TEST_RELOAD_N=5`, in the cloud container, 2026-11-27; the test passed (0 requests failed after one retry in every case). Drops are per **load attempt** (successful or failed); a case with no successful load still counts its failed loads. Before `d5f0327`, the 2-taken boot case cost about 97 successful and 105 failed loads per operation (0.45 s, about 0.5 s of CPU) and 5 to 6 panel requests failed even after one retry.

**Caveats to print with the numbers:**
- One real app container is the upstream of all 100 routes; the other 99 services are rows and fixtures.
- Only 2 app routes carry load (the load generator's targets), plus the panel.
- The first-sync-at-boot list is one real container with 100 real inspects; `loadServices` and the settle step are not run.
- Memory is the **test process's** (Caddy, the route code, the store and the load generator together), not the server's. Only the **delta** across a case is a fair estimate; the absolute value is not comparable to the server's (the server also holds the API, auth, the hub, the SPA and the reconciler, so its own VmHWM can be higher).
- Drops are read as "expected never to have been processed" until the measurements confirm it.

Columns (every row): Caddy loads per operation (successful / failed) · drops per load, **per target (panel, app) x class (keep-alive, new connection)** · Go-internal keep-alive retries · requests that **failed after one retry on a new connection after 50 ms** · reload time mean / max (ms) · CPU per reload · VmHWM delta (MB).

| Case | Loads per op (ok / failed) | Drops per load: panel keep-alive / new | Drops per load: app keep-alive / new | Go keep-alive retries | Failed after 1 retry | Reload ms mean / max | CPU per reload | VmHWM delta (MB) |
|---|---|---|---|---|---|---|---|---|
| No operation (load generator baseline) | 0.00 / 0.00 | 0 / 0 (no load; counts over 5 x 250 ms) | 0 / 0 | 0 | 0 | n/a (250 ms idle window) | 0.00 ms net per op | 0.2 (max 0.5) |
| One new port, taken (the fast path) | 0.00 / 1.00 | 0.00 / 0.20 | 0.00 / 0.00 | 0 | 0 | 5.8 / 7.5 | 5.20 ms net per op | 0.3 (max 0.7) |
| First sync at boot with 1 taken port | 1.00 / 1.00 | 0.00 / 0.25 | 0.00 / 0.00 (no app load at boot) | 0 | 0 | 8.6 / 10.1 | 12.21 ms net per op | 0.3 (max 0.4) |
| First sync at boot with 2 taken ports (k + 1 = 3 loads; was about 2N + 3 before `d5f0327`) | 1.00 / 2.00 | 0.00 / 0.17 | 0.00 / 0.00 (no app load at boot) | 0 | 0 | 10.9 / 11.9 | 15.04 ms net per op | 0.6 (max 0.8) |
| Try again, port still taken | 0.00 / 1.00 | 0.00 / 0.00 | 0.00 / 0.00 | 0 | 0 | 5.5 / 8.0 | 3.99 ms net per op | 0.5 (max 0.7) |
| Try again, port freed | 1.00 / 0.00 | 0.00 / 0.20 | 0.40 / 0.20 | 1 | 0 | 6.3 / 7.6 | 4.87 ms net per op | 0.3 (max 0.7) |
| Add a public port | 1.00 / 0.00 | 0.00 / 0.80 | 0.20 / 0.00 | 5 | 0 | 6.0 / 8.0 | 5.09 ms net per op | 0.2 (max 0.7) |
| Change a public port | 1.00 / 0.00 | 0.00 / 0.20 | 0.60 / 0.00 | 0 | 0 | 9.9 / 19.0 | 3.76 ms net per op | 0.5 (max 1.1) |
| Clear a public port | 1.00 / 0.00 | 0.20 / 0.20 | 0.80 / 0.00 | 0 | 0 | 7.2 / 12.2 | 5.74 ms net per op | 0.6 (max 0.8) |
| Delete an exposed service | 1.00 / 0.00 | 0.20 / 0.60 | 0.20 / 0.20 | 2 | 0 | 7.3 / 11.1 | 6.73 ms net per op | 0.6 (max 1.1) |
| First sync at boot (all 100 routes) | 1.00 / 0.00 | 0.00 / 0.33 | 0.00 / 0.00 (no app load at boot) | 0 | 0 | 6.5 / 8.3 | 10.34 ms net per op | 0.6 (max 1.3) |

First sync at boot, extra figures: time for the list and the 100 inspects before the first route apply: 88.6 ms (1-taken: 87.1 ms; 2-taken: 87.6 ms); the list and inspect of 100 containers plus the `Load`: about 95 ms. Idle RSS with 100 routes configured (steady state, no load, test process): +1.4 MB over no routes. The load generator itself used about 1.04 CPU-seconds per wall second, so the net CPU column subtracts that rate and is noisy.

### Zero-downtime recording (plan 0004 exit evidence)

From the regression runs on commit `d5f0327`, 2026-11-27, in the cloud container (`-race -tags integration`, the real embedded Caddy and a real app container, load on the deploying app, a second app and the panel, GET and POST): `TestBlueGreenSwitchWithZeroNon2xx`, `TestUnhealthyCandidateKeepsOldServingWithZeroNon2xx`, `TestIntegrationStopFirstGapUnderLoad` and `TestIntegrationFailedStopFirstRestartsOldUnderLoad`, 3 runs each. Ranges are over the 3 runs. These run in the integration test rig, not through the built binary; the kill -9 suite exercises the built binary.

| Run | Strategy | Load (requests, keep-alive / new connection, GET and POST) | Non-2xx (and which) | Drops | Go retries | Caddy loads during the deploy | Commit recorded (stage, time) | Result |
|---|---|---|---|---|---|---|---|---|
| Blue-green, healthy candidate | blue-green | 5,043–5,184 keep-alive / 3,214–3,354 new connection (3 runs) | 0 | 0 | 0 | 0 (1 `proxy_apply`, a table swap) | committed; the new container answered after the switch (old x188–196, new x1,915–1,995) | pass, 3 of 3 |
| Blue-green, unhealthy candidate (old version keeps serving) | blue-green | 871–913 keep-alive / 852–898 new connection (3 runs) | 0 | 0 | 0 | 0 (no `proxy_apply`) | none (expected) | pass, 3 of 3 |
| Stop-first, healthy | stop-first | 3 runs; deploy 2.10–2.20 s | only the maintenance 503 with `Retry-After`: 507–534 keep-alive, 463–489 new connection | 0 | 0 | 0 (2 `proxy_apply`, table swaps) | committed; the new container answered after the gap | pass, 3 of 3 |
| Stop-first, failing (old version started again) | stop-first | 3 runs; deploy 7.47–7.71 s | only the maintenance 503: 2,114–2,207 keep-alive, 1,961–2,039 new connection | 0 | 0 | 0 (2 `proxy_apply`) | none (expected); the old version answered again | pass, 3 of 3 |

### Notes on scenarios 5 to 8

Scenarios 5 to 8 (`bench/run.sh`) **run locally and are not part of the CI bench job**. They run the server with host networking (the app network's addresses are not reachable from another bridge) and `--allow-loopback-registry`, so they are not the shipped deployment shape. **Scenario 5 minus scenario 2 is not the cost of one service**: scenario 5 also includes the set-up (sign-in, service creation, the first deploy and its history rows) that scenario 2 does not do, so it is an upper bound.

### Earlier figures (not release numbers)

For orientation only, from plan 0004 and measured on a cloud container, not on the CI runner or a 1 GB VPS: full S2 live watcher 54.9 MB (step 10, commit `6b79e71`); full S4 46.9 to 47.6 MB, VmHWM 50.3 MB (steps 10 to 14); binary 19.96 MB gzip and first-load JS 41.5 KB (step 14). They do not replace the PENDING cells above.

### Manual 1 GB VPS check (plan 0004 test plan; informational, Q6 still open)

PENDING: no 1 GB VPS is available. If one is, deploy `nginx` with a public port, run a blue-green redeploy and a failing deploy under a `curl` loop, change the public port under the same loop, roll back, restart Docker, restart ServerDock (record the outage), and record idle VmRSS with zero and with one service.

## What the Phase 0 spikes verified (tests, not measurements)

- Caddy reload while an agent is connected through `/agent/connect`: the drop is **delayed, not avoided**. With `stream_close_delay` 5m the agent keeps its session and open streams through the reload, but Caddy still closes the old upgraded connection 5 minutes later, and the agent then reconnects with backoff. Without the setting the agent is dropped immediately. Phase 1 consequence: every Caddy reload (every deploy) drops every remote agent after 5 minutes; serve `/agent/connect` from a listener Caddy does not rebuild (see docs/security.md, Phase 0 review items).
- Hub shutdown closes hijacked WebSocket sessions (hub context tied to the server lifetime).
- A server-side stream close cancels the Docker request within 2 s (fake Docker socket, end to end through hub, yamux, agent and `dockerapi`); a real busybox integration test (`-tags integration`) passed locally against Docker Desktop.
