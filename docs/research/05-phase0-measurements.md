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

*To be filled in by the maintainer.* Apps are offline while ServerDock is down, because the proxy that serves them is embedded in the ServerDock process (QM3a-9, accepted for Phase 1). Plan 0004 step 10 measures it: for each stage of a blue-green and of a stop-first deploy, the `kill -9` suite (`TestKillNine...` in `cmd/serverdock`, tags `integration,testhooks`) kills the server at that stage with SIGKILL, starts it again on the same data directory and Docker, and logs one parseable line per run:

```
OUTAGE strategy=<blue_green|stop_first> stage=<stage:before|after> ms=<SIGKILL to the first 2xx from the app after the restart>
```

| Strategy | Stage | Run 1 (ms) | Run 2 (ms) | Run 3 (ms) | Notes |
|---|---|---|---|---|---|
| | | | | | |

Also to fill in: the machine (CPU, Docker version, storage driver), how many runs, whether the restart used `--docker-socket` as the default, and the daemon-restart case (`TestDaemonRestartKeepsRoutesCorrect`: time from `systemctl restart docker` to every public port answering 2xx again from its own service).

## What the Phase 0 spikes verified (tests, not measurements)

- Caddy reload while an agent is connected through `/agent/connect`: the drop is **delayed, not avoided**. With `stream_close_delay` 5m the agent keeps its session and open streams through the reload, but Caddy still closes the old upgraded connection 5 minutes later, and the agent then reconnects with backoff. Without the setting the agent is dropped immediately. Phase 1 consequence: every Caddy reload (every deploy) drops every remote agent after 5 minutes; serve `/agent/connect` from a listener Caddy does not rebuild (see docs/security.md, Phase 0 review items).
- Hub shutdown closes hijacked WebSocket sessions (hub context tied to the server lifetime).
- A server-side stream close cancels the Docker request within 2 s (fake Docker socket, end to end through hub, yamux, agent and `dockerapi`); a real busybox integration test (`-tags integration`) passed locally against Docker Desktop.
