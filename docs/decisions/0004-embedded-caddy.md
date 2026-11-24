# 0004 — Embedded Caddy as the proxy

**Status:** accepted by the maintainer · **Date:** 2026-11-20

**Context.** Traefik idles ~80–120 MB and its label/config paths caused Dokploy bugs and CVEs; Caddy ~20–40 MB with an atomic JSON config API and automatic HTTPS.

**Decision.** Embed Caddy as a Go library in the agent; generate full JSON config from desired state; on-demand TLS gated by our DB. Fallback: official Caddy container controlled over a Unix-socket admin API.

**Consequences.** Zero extra containers on managed servers; +20–30 MB binary size. Validate idle RSS in Phase 0 Spike 1; switch to the fallback if over budget.

---

## Addendum A — Upstream switches without a proxy reload

**Status:** accepted by the maintainer (plan 0004 Revision 11, 2026-11-24) · built in M3a step 6b.

**Context.** The first app-route design (plan 0004 step 6) gave every exposed app its own Caddy server and rebuilt the whole config on every route change, so a deploy switch was a `caddy.Load`. A reload retires the old `net/http` servers and a connection accepted at that moment can be closed before its request is read (measured in Revision 9: about 0.6 to 0.9 new connections per reload, on the panel and on every app). That broke the promise that deploying one app costs other apps and the panel nothing. Alternatives checked against Caddy v2.11.9: `dynamic_upstreams` (a 5-minute global goroutine that never stops, `reverseproxy/hosts.go:166-177`, per-request provisioning, and static pages still need a matcher), a listener wrapper (the retiring server closes the accepted connection, so it cannot fix the drop), and moving apps or the panel out of Caddy (against ADRs 0004 and 0010).

**Decision.**
- A small handler module, `http.handlers.serverdock_app` (`internal/agent/proxy/app.go`), reads one package-level `atomic.Pointer` to an immutable table keyed by the local port of `http.LocalAddrContextKey`, loaded once per request. A static entry (the "not running" and maintenance pages) is written by the handler and never reaches `reverse_proxy`; an upstream entry sets the request variable `sd_upstream` and calls next; unknown port or no table is the "not running" page. It never calls next without setting the variable.
- One `reverse_proxy` for all apps, with a static upstream `{http.vars.sd_upstream}` (placeholders are replaced per request, `reverseproxy/hosts.go:117`), `network_proxy: none`, and none of: passive health checks, `max_requests`, retries, `dynamic_upstreams`. The one host entry is shared by every app, so a failing app must never mark it down.
- All public ports are listeners of one Caddy server. The Caddy config depends only on the set of ports; the table carries everything else. `Applier.Apply` loads (with bind isolation, a failed port leaves the listen list) only when the port set changes or a retry of failed ports is asked (`proxy_apply.retry_failed`, set by the control plane for an explicit retry while a port is failed). Otherwise it swaps the table and calls no `caddy.Load`, even while another service's port is taken. The table is stored after every successful load, so table and running config never diverge.
- `proxy_apply` validates upstreams against the recorded `meta.app_subnet` (no Docker call), so a switch works while Docker is hung.

**Consequences.**
- One custom Caddy module (about 100 lines) to maintain and re-test on every Caddy upgrade (`TestClientCannotInjectUpstream`, `TestReverseProxyWithoutTheVarFailsClosed`, `TestHandlerUnderConcurrentSwaps`, `TestOneTickerForAllAppRoutes` and the 20-switch integration test run on every bump). It depends on `http.LocalAddrContextKey`, request vars and upstream placeholders.
- The table is package-global: exactly one `Applier` may write it per process (server mode); the remote agent never builds one with apps, and its config is unchanged (golden test).
- The upstream-health metrics ticker is now exactly one 10 s ticker once at least one public port exists (it was one per upstream route), not rebuilt on a switch; it is rebuilt only by a reload.
- A deploy, rollback, redeploy or restart of one app reloads nothing and drops no request on other apps or the panel. Adding, changing or clearing a public port, deleting an exposed service, the first sync at boot, "Try again" for a failed port and bind isolation still reload, and keep the Revision 9 behaviour (a connection being opened at that moment can be closed before its request is read; open streams are closed within 10 s). Step 15 measures them.
- Between a load and the table store, a newly added port answers "not running".
- M4 can key the same handler by `Host` (domain routing) instead of the local port.
