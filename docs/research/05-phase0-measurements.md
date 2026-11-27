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

Status: **PENDING.** Plan 0004 step 15 asks for the numbers below. None of them is recorded yet: every cell that says PENDING is to be filled from the bench run for the M3a pull request (the maintainer fills them from the run in progress), with the commit, the date and the run link. **Nothing in this section is a measurement until its cell holds a number.** Scenarios 5 to 8 and the reload cases are described in `bench/run.sh` and `internal/deploy/reload_cases_integration_test.go`; results go to `bench/results/` (see [the placeholder](../../bench/results/2026-11-27-m3a-release-PENDING.md)).

Gates (ADR 0011): S2 ceiling 200 MB, tripwire 80 MB; S4 ceiling 100 MB, tripwire 50 MB. VmRSS is the median of the per-run medians.

**Run:** commit PENDING · date PENDING · CI run PENDING · runner PENDING.

### Footprint

| Measurement | Scenario | VmRSS (MB) | VmHWM (MB) | Idle CPU | Gate | Result |
|---|---|---|---|---|---|---|
| Full S4 (remote node: agent + Caddy) | 4 | PENDING | PENDING | PENDING | tripwire 50 MB | PENDING |
| **S2 live watcher (CI, the release gate)** | 2 | **PENDING (release gate: the CI bench job does not run on a draft PR, so this number comes only after the PR is marked ready)** | PENDING | PENDING | tripwire 80 MB; planning figure ≤ 56 MB | PENDING |
| S2 no Docker (report only) | 2 with `S2_DOCKER=0` | PENDING | PENDING | PENDING | none | PENDING |
| S2 with one exposed service (gated against the tripwire) | 5 | PENDING | PENDING | PENDING | tripwire 80 MB | PENDING |
| S2 with 10 exposed idle services (report only) | 6 | PENDING | PENDING | PENDING (the one app ticker) | none (reported against 80 MB) | PENDING |
| RSS 60 s after a deploy finishes | 7 | idle baseline PENDING · peak PENDING · 60 s after PENDING | PENDING | | back to the baseline | PENDING |
| Peak RSS during two concurrent deploys | 8 | peak PENDING | PENDING | | none | PENDING |
| Step 13 build versus the final build, same session (settles whether S4's VmHWM of 49.7 to 50.3 MB is noise; plan 0004 Revision 20) | 4 | step 13: PENDING · final: PENDING | PENDING / PENDING | | none (nothing gates VmHWM) | PENDING |

### Size

| Measurement | Value | Gate | Result |
|---|---|---|---|
| Binary, gzip -9 (amd64) | PENDING | 40 MB | PENDING |
| SPA first-load JS (gzip) | PENDING | 250 KB | PENDING |

### Restart outage (QM3a-9)

Already measured: see [the section above](#m3a-outage-while-serverdock-is-killed-and-restarted-qm3a-9) (blue-green 59 to 99 ms at every stage; stop-first 58 to 118 ms except the gap stages at about 2.4 s with a 2 s grace; a Docker daemon restart 12.1 s in CI, on commit `e76aa77`). To be re-run on the release commit if the maintainer wants the final figure: PENDING.

### Remaining reload cases with 100 routes (plan 0004 Revision 11, step 15)

A switch, revert, restart, rollback and the maintenance page never reload (zero-drop tests). These cases still reload the proxy. Run: `SD_TEST_RELOAD_CASES=1 SD_TEST_APP_IMAGE=127.0.0.1:5000/sd-testapp:1 go test -tags integration -run TestIntegrationReloadCasesWith100Routes -v -count=1 ./internal/deploy` (repeats per case: `SD_TEST_RELOAD_N`, default 3). One real app container is the upstream of all 100 routes, so VmHWM is the test process's, an upper bound of the server's. The drops are counted per reload, in the two connection classes; "never processed, safe to retry" is the Revision 9 reading, to be confirmed by the numbers.

| Case | Reloads | Reload time mean / max (ms) | Drops per reload, keep-alive | Drops per reload, new connection | Failed after one client retry | VmHWM before → after the case (MB) |
|---|---|---|---|---|---|---|
| `control_no_reload` (a switch, for comparison) | PENDING | PENDING | PENDING | PENDING | PENDING | PENDING |
| `add_public_port` | PENDING | PENDING | PENDING | PENDING | PENDING | PENDING |
| `change_public_port` | PENDING | PENDING | PENDING | PENDING | PENDING | PENDING |
| `clear_public_port` | PENDING | PENDING | PENDING | PENDING | PENDING | PENDING |
| `delete_exposed_service` | PENDING | PENDING | PENDING | PENDING | PENDING | PENDING |
| `first_sync_at_boot` | PENDING | PENDING | PENDING | PENDING | PENDING | PENDING |
| `try_again_port_still_taken` | PENDING | PENDING | PENDING | PENDING | PENDING | PENDING |
| `try_again_port_freed` | PENDING | PENDING | PENDING | PENDING | PENDING | PENDING |
| `bind_isolation` (worst case 2N + 3 reloads) | PENDING | PENDING | PENDING | PENDING | PENDING | PENDING |

### Earlier figures (not release numbers)

For orientation only, from plan 0004 and measured on a cloud container, not on the CI runner or a 1 GB VPS: full S2 live watcher 54.9 MB (step 10, commit `6b79e71`); full S4 46.9 to 47.6 MB, VmHWM 50.3 MB (steps 10 to 14); binary 19.96 MB gzip and first-load JS 41.5 KB (step 14). They do not replace the PENDING cells above.

### Manual 1 GB VPS check (plan 0004 test plan; informational, Q6 still open)

PENDING: no 1 GB VPS is available. If one is, deploy `nginx` with a public port, run a blue-green redeploy and a failing deploy under a `curl` loop, change the public port under the same loop, roll back, restart Docker, restart ServerDock (record the outage), and record idle VmRSS with zero and with one service.

## What the Phase 0 spikes verified (tests, not measurements)

- Caddy reload while an agent is connected through `/agent/connect`: the drop is **delayed, not avoided**. With `stream_close_delay` 5m the agent keeps its session and open streams through the reload, but Caddy still closes the old upgraded connection 5 minutes later, and the agent then reconnects with backoff. Without the setting the agent is dropped immediately. Phase 1 consequence: every Caddy reload (every deploy) drops every remote agent after 5 minutes; serve `/agent/connect` from a listener Caddy does not rebuild (see docs/security.md, Phase 0 review items).
- Hub shutdown closes hijacked WebSocket sessions (hub context tied to the server lifetime).
- A server-side stream close cancels the Docker request within 2 s (fake Docker socket, end to end through hub, yamux, agent and `dockerapi`); a real busybox integration test (`-tags integration`) passed locally against Docker Desktop.
