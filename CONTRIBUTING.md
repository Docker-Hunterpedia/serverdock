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
- `SERVERDOCK_TEST_PROXY_LOG_LEVEL=DEBUG` makes the embedded Caddy log at DEBUG. It exists for `TestSecretsNeverLeak`, which scans Caddy's and the server's logs for secrets. It is read by the server only for that purpose: there is no flag and it is not in `--help`. The server logs a WARN at start when it is set to anything but WARN or ERROR. The proxy's log encoder (`redactedHeaders()` in `internal/agent/proxy/redact.go`) hides the `X-SD-CSRF` header from Caddy's logs: **extend that list for any new secret-bearing header**, and add the secret to the sentinel suite.

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

## The web app

The single-page app lives in `web/` (SvelteKit, static output). Node (see `web/.nvmrc`) is needed only to build it: `go build` and `go test` work without it and embed a placeholder page (`serverdock version` prints `ui=placeholder`).

- `make web` installs the pinned dependencies (`npm ci`), runs svelte-check and builds `web/build` (gzip twins, 250 KB JavaScript budget). `make build` then embeds it (`-tags spa`, `ui=spa`).
- `make dev` runs a throwaway server and Vite over HTTPS (self-signed certificate) at https://localhost:5173, proxying `/api`. The Host allowlist and Origin rule are not loosened; `web/scripts/dev-proxy-check.sh` verifies that.
- `make web-types` regenerates `web/src/lib/api-types.ts` from `docs/api/openapi.json`; CI fails if it is stale.
- npm dependencies are pinned exactly and kept minimal; add one only with a plan that names it.
- TypeScript is pinned to 6.x (SvelteKit's peer). `openapi-typescript` (codegen only, dev) declares a 5.x peer, so `package.json` has an explicit `overrides` entry that lets it use the same TypeScript; every `npm ci` reproduces that. Drop the override when the tool supports 6.
- `npm audit --omit=dev` and `npm audit --audit-level=high` (dev included, since vite and its plugins write the shipped bundle) both run in CI.
- Any future release workflow must build `web/build` itself from the checked-out commit (`npm ci --ignore-scripts && npm run build`). It must never download an artifact by run id or with a token, because a release must not embed a bundle it did not build.
- `make e2e` runs the Playwright + axe suite (`web/e2e`) against `bin/serverdock` (so run `make web build` first; the first time, `cd web && npx playwright install chromium webkit`). It starts the binary on high ports with a temp data directory, does the first run through the API and checks every shell state in eight projects (desktop, phone, 320 px, 200% zoom, dark, reduced motion, forced colours, WebKit). Zero axe violations and zero CSP violations are required.
- Playwright tests that change state use the `server` and `admin` fixtures (`web/e2e/fixtures.ts`): a fresh binary per test, so back-off counters and the one-time setup token are never shared. They expire or revoke sessions, pending sign-ins and setup tokens by editing the temp database with Node's built-in `node:sqlite`, so no test-only flag exists in the server.
