# 0008 — Gate the footprint on the deployed shapes; slim agent in Phase 2

**Status:** accepted by the maintainer · **Date:** 2026-11-20 · Hard limits superseded by [ADR 0011](0011-footprint-ceiling.md) (ceiling 200 / 100 MB; 80 / 50 MB become tripwires). The choice of gated scenarios still holds.

**Context.** The Phase 0 local bench ([research/05](../research/05-phase0-measurements.md)) showed that ServerDock ships as one ~48 MB binary, and about 30–34 MB of every scenario's VmRSS is `RssFile`: read-only pages of that binary, which the kernel can drop and share. Those numbers came from Docker Desktop and are informational only. As a result, the two setups people don't run on their own failed their gates: server without proxy (45.8 MB against 40) and remote agent without proxy (40.7 MB against 20). The two setups people actually run passed: single node at 52.5 MB against 80, and remote agent with its own Caddy at 49.8 MB against 50. A remote agent always runs with its own Caddy, because Caddy is what routes traffic to the apps on that server.

**Decision.**
1. CI gates on the two deployed shapes: **scenario 2, single node ≤ 80 MB / < 1% CPU**, and **scenario 4, remote node (agent + Caddy) ≤ 50 MB / < 0.5% CPU**, both with +5% memory and +0.2 points CPU tolerance.
2. Scenario 1 (server, `--proxy=false`) and scenario 3 (agent without proxy) become **report only**. They keep their 40 MB and 20 MB targets so we still see trends.
3. Phase 2 (multi-server) adds a **slim agent build** without SQLite, the SPA or the control-plane packages. Scenario 3 then returns as a gate at 20 MB, with a hard fail at 25.
4. The budget table in [architecture.md](../architecture.md#footprint-budget) and the gates in `bench/report` follow this ADR.

**Consequences.** The gates protect what users run, and the two totals don't change. Per-component numbers are tracked but don't block merges until the slim agent exists. If the CI or 1 GB VPS run shows scenario 2 or 4 over budget, this ADR doesn't help, and ADR 0004 (embedded Caddy) is revisited as planned.
