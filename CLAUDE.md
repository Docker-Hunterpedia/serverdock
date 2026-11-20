# ServerDock

A lightweight, open-source, self-hosted server manager / PaaS. The core promise is a **small footprint** (it should run on a 1 GB VPS beside your apps), **safe operations**, and **simple setup**.

Start here: [docs/vision.md](docs/vision.md) → [docs/features.md](docs/features.md) → [docs/architecture.md](docs/architecture.md) → [docs/roadmap.md](docs/roadmap.md). Research and sources are in [docs/research/](docs/research/).

## Agent workflow

| Role | Agent | Model | When |
|---|---|---|---|
| Adviser | `advisor` | Fable 5.2 | Direction, trade-offs, tie-breaks |
| Plan | `planner` | Opus 5.5 | Before any non-trivial work → `docs/plans/` |
| Review | `reviewer` | Opus 5.5 | Approves plans; reviews diffs |
| Implement | `implementer` | Sonnet 5.5 | Only after the plan is APPROVED |
| Security | `security` | Opus 5.5 | Auth, secrets, Docker socket, agent channel, terminal, AI tools, input |
| Accessibility | `accessibility` | Sonnet 5.5 | Any UI change |
| Systems / complexity | `systems` | Opus 5.5 | Distributed state, failure modes, upgrades, simplicity |
| Footprint | `footprint` | Sonnet 5.5 | Anything adding background work, deps, or weight; every release |
| Research | `researcher` | Opus 5.5 | Facts with sources |
| Tests | `tester` | Sonnet 5.5 | Test writing and running |

Flow: **idea → (advisor if it's a direction question) → planner → reviewer approves plan → implementer → tester → reviewer + required specialists → merge.**

Lasting decisions go in `docs/decisions/NNNN-title.md` (ADR format: context, decision, consequences).

## Rules
- Respect the footprint budgets in `docs/architecture.md`. Raising idle RAM/CPU needs a written reason.
- Never log, return, or send secrets to an AI provider.
- Report state honestly in UI and CLI — never claim success before the work finishes.
- Private or internal projects mentioned in conversation are for ideas only; never name or copy them in this repo.
