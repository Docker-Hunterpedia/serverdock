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

## Testing notes

- `go test ./...` also builds the real binary for a few tests (file modes, the secret-sentinel suite); `go test -short` skips them.
- Run the suite on Linux as root and as a non-root user: some tests need root (running as another uid) and some fail only for a non-root user (privileged ports). The tests never bind `:80` or `:443`.
- `SERVERDOCK_TEST_PROXY_LOG_LEVEL=DEBUG` makes the embedded Caddy log at DEBUG. It exists for `TestSecretsNeverLeak`, which scans Caddy's and the server's logs for secrets. It is read by the server only for that purpose: there is no flag and it is not in `--help`.

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
