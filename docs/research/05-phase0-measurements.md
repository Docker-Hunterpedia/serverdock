# 05 — Phase 0 measurements

Status: **incomplete.** Only a local, quick, informational run exists. The official numbers (CI and a real 1 GB VPS) are pending. Nothing here proves or disproves the Phase 0 exit criterion.

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

## CI ubuntu-latest (gate): pending first CI run

No CI run has happened. The workflow (`.github/workflows/ci.yml`) has not been executed on GitHub, and the Linux "direct" path of `bench/measure.sh` has not been exercised. Fill in amd64 and arm64 numbers for scenarios 1-4 here after the first run.

## Real 1 GB VPS (required exit): pending

Required exit criterion of Phase 0: one full bench run (3 runs per scenario, 2 min warm-up, 5 min idle) on a real 1 GB VPS. Not done.

## ADR 0004 verdict: provisional

Based only on the local scenario 2 minus scenario 1 difference (+6.7 MB VmRSS, +4.6 MB RssAnon): **keep embedded Caddy for now**; its marginal cost is far below the ~20-30 MB assumed in ADR 0004, and live reload with the admin API disabled works (`caddy.Load` twice and a listener move, tested). The Caddy-container fallback is **not** indicated by this data.

The same data raises a separate question for the reviewers: the single large binary already exceeds the scenario 1 (40 MB) and scenario 3 (20 MB hard fail 25) gates in this informational run, mainly through file-backed pages. Options to evaluate after the CI and VPS numbers: build the remote agent without the proxy and SQLite packages (build tags or a separate `main`), trim linked Caddy modules further, and compare `RssFile` on a real host. This verdict is provisional until the CI and VPS sections above are filled in.

## What the Phase 0 spikes verified (tests, not measurements)

- Caddy reload while an agent is connected through `/agent/connect`: the agent **stays connected on the same session** (`stream_close_delay` 5m). Without that setting the same test shows the agent being dropped and reconnecting.
- Hub shutdown closes hijacked WebSocket sessions (hub context tied to the server lifetime).
- A server-side stream close cancels the Docker request within 2 s (fake Docker socket, end to end through hub, yamux, agent and `dockerapi`); a real busybox integration test (`-tags integration`) passed locally against Docker Desktop.
