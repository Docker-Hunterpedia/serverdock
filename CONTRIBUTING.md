# Contributing to ServerDock

Thanks for your interest. ServerDock is in **Phase 0 (foundations)**: there is no code yet, only the plan. See the [roadmap](docs/roadmap.md).

## Before you start

1. Read [docs/vision.md](docs/vision.md), the core promise (light, safe, honest, simple), and [docs/architecture.md](docs/architecture.md).
2. For anything non-trivial, open an issue or discussion first. Features that raise idle RAM/CPU or widen the attack surface need a strong reason.

## How work flows

1. **Plan:** non-trivial changes get a short plan in `docs/plans/NNNN-<slug>.md` (goal, out of scope, design, footprint budget, security notes, test plan).
2. **Review the plan** before writing code.
3. **Implement** in small PRs with tests.
4. **Review:** correctness, plus security, accessibility, and footprint reviews where relevant.

Lasting decisions are recorded as ADRs in [docs/decisions/](docs/decisions/).

## Rules that are not negotiable

- **No shell strings.** Use the Docker API / compose-go; exec only with argv arrays.
- **Footprint budgets** in [docs/architecture.md](docs/architecture.md#footprint-budget) are hard limits.
- **Never log or return secrets**, and never send them to an AI provider.
- **Honest state:** never report success before the work is done.
- Tests for every change; integration tests use a real Docker daemon.

## Sign your commits (DCO)

We use the [Developer Certificate of Origin](https://developercertificate.org/), not a CLA. Add a sign-off to every commit:

```bash
git commit -s -m "Your message"
```

## License

By contributing you agree that your contributions are licensed under the [AGPL-3.0](LICENSE).

## Security issues

Do not open public issues for vulnerabilities. See [SECURITY.md](SECURITY.md).

## Code of conduct

This project follows the [Contributor Covenant](CODE_OF_CONDUCT.md).
