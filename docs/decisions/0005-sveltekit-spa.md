# 0005 — SvelteKit static SPA embedded in the binary

**Status:** accepted by the maintainer · **Date:** 2026-11-20

**Context.** The UI has no server RAM cost if served as static files; bundle size and real-time views (logs, terminal, charts) matter. Options: SvelteKit, SolidJS, React+Vite, HTMX.

**Decision.** SvelteKit with adapter-static, embedded via `embed.FS`; uPlot for charts, xterm.js for terminals; REST typed from OpenAPI plus one WebSocket.

**Consequences.** Small bundle and good DX. React+Vite stays the fallback if contributor reach matters more.
