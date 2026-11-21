# 0011 — Footprint ceiling 200 MB, small numbers become tripwires

**Status:** accepted by the maintainer · **Date:** 2026-11-21 · Supersedes the hard limits in [ADR 0008](0008-footprint-gates.md) (its choice of gated scenarios stays).

**Context.** ADR 0008 gated the single node at 80 MB and each remote node (agent + Caddy) at 50 MB. In Phase 1 M2 the remote node reached 46.0 MB in CI (allocation 45.0), although agent mode runs none of the new code. A footprint investigation found no leak: about 78% of agent-mode RSS is file-backed pages of the shared binary, binary growth adds about 1 MB of mapped pages per 0.8 MB, and Go's lazy return of freed heap moves a run by about 2 MB. Keeping 50 MB would mean memory work (trims, an early slim agent build) on every milestone. The maintainer stated that about 200 MB for the server manager is acceptable.

**Decision.**
1. **Hard ceilings (CI fails above them):** single node (scenario 2) **≤ 200 MB**, remote node with its own Caddy (scenario 4) **≤ 100 MB**, both with the existing +5% tolerance. Idle CPU gates are unchanged (< 1% and < 0.5%).
2. **Tripwires (warning, never fail CI):** single node **80 MB**, remote node **50 MB** — the old ADR 0008 numbers. Going above a tripwire is flagged in the bench report and needs a written reason in the PR, reviewed by `footprint`.
3. Per-milestone allocations in the plans (e.g. plan 0002's table) are tripwire planning numbers, not gates.
4. The S4 trims proposed by the investigation are not done, and the slim agent build stays in Phase 2 as planned.
5. Binary size (≤ 40 MB gzip), SPA first load (≤ 250 KB JS) and the report-only scenarios 1 and 3 are unchanged.

**Consequences.** Much more headroom for Phase 1 (builds, metrics, backups) without memory work blocking milestones. "Runs beside your apps on a 1 GB VPS" still holds (200 MB is a fifth of it), but the "lightest panel" claim is weaker; published numbers stay real measurements, and the tripwires keep accidental bloat visible early. If a tripwire is crossed without a convincing reason, the change is reworked or the slim agent is pulled forward.
