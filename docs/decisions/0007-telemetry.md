# 0007 — No telemetry by default

**Status:** accepted by the maintainer · **Date:** 2026-11-20

**Context.** None of the docs mentioned telemetry. In self-hosted tools, silence on this becomes a trust problem later. ServerDock holds root-equivalent access to user servers.

**Decision.** By default ServerDock sends nothing anywhere. The only outbound calls it makes on its own are update checks (a version manifest) and app image digest checks, and both can be turned off. Any future usage statistics must be opt-in, anonymous, with the exact payload documented and viewable in the UI, and they are always off in air-gapped mode.

**Consequences.** We get less usage data, and we rely on GitHub issues, Discussions and `serverdock doctor --bundle` (sent manually by users) for feedback.
