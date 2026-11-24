package deploy

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/netip"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/Docker-Hunterpedia/serverdock/internal/actions"
	"github.com/Docker-Hunterpedia/serverdock/internal/agentops"
	"github.com/Docker-Hunterpedia/serverdock/internal/audit"
	"github.com/Docker-Hunterpedia/serverdock/internal/policy"
	"github.com/Docker-Hunterpedia/serverdock/internal/store"
)

type svcClock struct {
	mu sync.Mutex
	t  time.Time
}

func (c *svcClock) Now() time.Time { c.mu.Lock(); defer c.mu.Unlock(); return c.t }
func (c *svcClock) Add(d time.Duration) {
	c.mu.Lock()
	c.t = c.t.Add(d)
	c.mu.Unlock()
}

// fakePorts is the exposure source: it counts the questions asked of "Docker".
type fakePorts struct {
	mu    sync.Mutex
	calls int
	res   agentops.PortsResult
	err   error
}

func (f *fakePorts) ContainerPorts(context.Context) (agentops.PortsResult, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.calls++
	return f.res, f.err
}

func (f *fakePorts) n() int { f.mu.Lock(); defer f.mu.Unlock(); return f.calls }

type svcRig struct {
	t     *testing.T
	st    *store.Store
	px    *fakeProxy
	r     *Routes
	reg   *actions.Registry
	clk   *svcClock
	ports *fakePorts
	boots int
}

func newSvcRig(t *testing.T, mod ...func(*ServiceDeps)) *svcRig {
	t.Helper()
	st := openStore(t)
	px := &fakeProxy{}
	g := &svcRig{t: t, st: st, px: px, r: NewRoutes(st, px, nil), clk: &svcClock{t: time.Date(2026, 11, 22, 12, 0, 0, 0, time.UTC)}, ports: &fakePorts{}}
	log := slog.New(slog.NewTextHandler(io.Discard, nil))
	al, err := audit.New(st, log)
	if err != nil {
		t.Fatal(err)
	}
	g.reg = actions.New(al, log)
	deps := ServiceDeps{DB: st, Routes: g.r, ReservedPorts: []int{7443, 8443, 8080}, Exposure: g.ports, Now: g.clk.Now, Log: log}
	for _, m := range mod {
		m(&deps)
	}
	RegisterServiceActions(g.reg, deps)
	return g
}

var (
	adminActor = func() actions.Actor { return actions.UserActor("1", actions.ScopeAdmin, time.Now().Add(time.Hour)) }
	readTok    = actions.TokenActor("1", actions.ScopeRead)
	writeTok   = actions.TokenActor("2", actions.ScopeWrite)
	adminTok   = actions.TokenActor("3", actions.ScopeAdmin)
)

type m = map[string]any

func callAs[T any](g *svcRig, a actions.Actor, name string, in any) (T, error) {
	g.t.Helper()
	raw, err := json.Marshal(in)
	if err != nil {
		g.t.Fatal(err)
	}
	res, err := g.reg.Invoke(context.Background(), a, name, raw, actions.Options{})
	var out T
	if res.Out != nil {
		out = res.Out.(T)
	}
	return out, err
}

// must is callAs as the admin user, failing the test on an error.
func must[T any](g *svcRig, name string, in any) T {
	g.t.Helper()
	out, err := callAs[T](g, adminActor(), name, in)
	if err != nil {
		g.t.Fatalf("%s: %v", name, err)
	}
	return out
}

func (g *svcRig) create(project, name string, extra m) ServiceChangeOut {
	g.t.Helper()
	in := m{"project": project, "name": name, "image_ref": "nginx:1.27"}
	for k, v := range extra {
		in[k] = v
	}
	return must[ServiceChangeOut](g, "services.create", in)
}

func (g *svcRig) get(ref string) ServiceOut {
	g.t.Helper()
	return must[ServiceOut](g, "services.get", m{"service": ref})
}

func wantKind(t *testing.T, err, kind error) {
	t.Helper()
	if !errors.Is(err, kind) {
		t.Fatalf("error %v, want kind %v", err, kind)
	}
}

func TestServiceCreateAndRead(t *testing.T) {
	g := newSvcRig(t)
	open(t, g.r)
	out := g.create("shop", "api", m{"container_port": 8000})
	s := out.Service
	if !out.Applied || s.Service != "shop/api" || s.ImageRef != "nginx:1.27" || *s.ContainerPort != 8000 || s.PublicPort != nil {
		t.Fatalf("%+v", out)
	}
	// Defaults: tcp check with a container port, 60 s, 3 passes, blue-green, 10 s drain and grace.
	if s.Health != (HealthOut{Kind: "tcp", TimeoutS: 60, Passes: 3}) || s.Strategy != "blue_green" || s.DrainS != 10 || s.StopGraceS != 10 || s.State != "not_deployed" {
		t.Errorf("defaults: %+v", s)
	}
	if len(s.Warnings) != 0 {
		t.Errorf("warnings without a public port: %+v", s.Warnings)
	}
	// A worker (no container port) defaults to "stays running".
	w := g.create("shop", "worker", nil).Service
	if w.ContainerPort != nil || w.Health.Kind != "running" {
		t.Errorf("worker: %+v", w)
	}
	// A bare name is stored as the reference Docker pulls (":latest" added).
	if l := g.create("other", "x", m{"image_ref": "redis"}).Service; l.ImageRef != "redis:latest" {
		t.Errorf("image_ref %q", l.ImageRef)
	}
	list := must[ServicesOut](g, "services.list", m{})
	if len(list.Services) != 3 || list.Services[0].Service != "other/x" || list.Services[1].Service != "shop/api" {
		t.Errorf("list order: %+v", list.Services)
	}
	pr := must[ProjectsOut](g, "projects.list", m{})
	if len(pr.Projects) != 2 || pr.Projects[0].Name != "other" || pr.Projects[1].Name != "shop" || pr.Projects[1].Services != 2 {
		t.Errorf("projects: %+v", pr.Projects)
	}
	if got := g.get("shop/api"); got.Service != "shop/api" || got.CreatedAt != "2026-11-22T12:00:00.000Z" {
		t.Errorf("get: %+v", got)
	}
	if _, err := callAs[ServiceOut](g, adminActor(), "services.get", m{"service": "shop/nosuch"}); !errors.Is(err, actions.ErrNotFound) {
		t.Errorf("get unknown: %v", err)
	}
}

func TestServiceCreateValidationErrors(t *testing.T) {
	g := newSvcRig(t)
	open(t, g.r)
	base := func(k string, v any) m {
		in := m{"project": "shop", "name": "api", "image_ref": "nginx:1.27", "container_port": 8000}
		if k != "" {
			in[k] = v
		}
		return in
	}
	for name, in := range map[string]m{
		"bad project":          base("project", "Shop"),
		"project ends in dash": base("project", "shop-"),
		"bad name":             base("name", "a/b"),
		"empty name":           base("name", ""),
		"bad image":            base("image_ref", "NGINX:1"),
		"no image":             base("image_ref", ""),
		"image with spaces":    base("image_ref", "nginx 1"),
		"bad container port":   base("container_port", 70000),
		"zero container port":  base("container_port", 0),
		"public without cport": {"project": "p", "name": "n", "image_ref": "nginx", "public_port": 9000},
		"public 80":            base("public_port", 80),
		"public 443":           base("public_port", 443),
		"public low":           base("public_port", 1023),
		"public high":          base("public_port", 65536),
		"public agent port":    base("public_port", 7443),
		"public https-listen":  base("public_port", 8443),
		"public listen":        base("public_port", 8080),
		"http without path":    base("health", m{"kind": "http"}),
		"http path no slash":   base("health", m{"kind": "http", "path": "healthz"}),
		"http path spaces":     base("health", m{"kind": "http", "path": "/a b"}),
		"tcp with path":        base("health", m{"kind": "tcp", "path": "/x"}),
		"running with path":    base("health", m{"kind": "running", "path": "/x"}),
		"unknown kind":         base("health", m{"kind": "grpc"}),
		"timeout low":          base("health", m{"timeout_s": 4}),
		"timeout high":         base("health", m{"timeout_s": 601}),
		"passes zero":          base("health", m{"passes": 0}),
		"passes high":          base("health", m{"passes": 11}),
		"strategy":             base("strategy", "rolling"),
		"drain below grace":    base("drain_s", 9),
		"drain high":           base("drain_s", 301),
		"stop grace high":      base("stop_grace_s", 301),
		"unknown field":        base("env", m{"A": "b"}),
		"link-local registry":  base("image_ref", "169.254.169.254/x:1"),
		"loopback registry":    base("image_ref", "127.0.0.1:5000/x:1"),
	} {
		t.Run(name, func(t *testing.T) {
			_, err := callAs[ServiceChangeOut](g, adminActor(), "services.create", in)
			wantKind(t, err, actions.ErrInvalidInput)
		})
	}
	// A tcp or http check needs a container port.
	_, err := callAs[ServiceChangeOut](g, adminActor(), "services.create", m{"project": "p", "name": "n", "image_ref": "nginx", "health": m{"kind": "tcp"}})
	wantKind(t, err, actions.ErrInvalidInput)
	// Nothing was written by any refusal (not even a project).
	if n := countRows(t, g.st, "services") + countRows(t, g.st, "projects"); n != 0 {
		t.Errorf("%d rows after refused creates", n)
	}
}

func countRows(t *testing.T, st *store.Store, table string) int {
	t.Helper()
	var n int
	if err := st.R.QueryRow(`SELECT COUNT(*) FROM ` + table).Scan(&n); err != nil {
		t.Fatal(err)
	}
	return n
}

func TestServiceCreateImageRefUsesThePolicyOptions(t *testing.T) {
	// Without --allow-loopback-registry a loopback registry is refused; with it, accepted.
	g := newSvcRig(t)
	_, err := callAs[ServiceChangeOut](g, adminActor(), "services.create", m{"project": "p", "name": "n", "image_ref": "127.0.0.1:5000/app:1"})
	wantKind(t, err, actions.ErrInvalidInput)
	g2 := newSvcRig(t, func(d *ServiceDeps) { d.Policy = policy.Options{AllowLoopbackRegistry: true} })
	if s := g2.create("p", "n", m{"image_ref": "127.0.0.1:5000/app:1"}).Service; s.ImageRef != "127.0.0.1:5000/app:1" {
		t.Errorf("%+v", s)
	}
	// update and set_source apply the same rule.
	_, err = callAs[ServiceChangeOut](g, adminActor(), "services.set_source", m{"service": "p/n", "image_ref": "127.0.0.1:5000/app:2"})
	wantKind(t, err, actions.ErrInvalidInput) // g has no such service: the image is checked first
	g.create("p", "n", nil)
	_, err = callAs[ServiceChangeOut](g, adminActor(), "services.set_source", m{"service": "p/n", "image_ref": "127.0.0.1:5000/app:2"})
	wantKind(t, err, actions.ErrInvalidInput)
	_, err = callAs[ServiceChangeOut](g, adminActor(), "services.update", m{"service": "p/n", "image_ref": "127.0.0.1:5000/app:2"})
	wantKind(t, err, actions.ErrInvalidInput)
}

func TestDuplicateServiceNames(t *testing.T) {
	g := newSvcRig(t)
	g.create("shop", "api", nil)
	_, err := callAs[ServiceChangeOut](g, adminActor(), "services.create", m{"project": "shop", "name": "api", "image_ref": "redis"})
	wantKind(t, err, actions.ErrConflict)
	if !strings.Contains(err.Error(), "shop/api") {
		t.Errorf("message %q", err)
	}
	// The same service name in another project is fine, and so is a service named like a project.
	g.create("blog", "api", nil)
	g.create("shop", "shop", nil)
	if countRows(t, g.st, "projects") != 2 || countRows(t, g.st, "services") != 3 {
		t.Error("rows")
	}
}

func TestTheHundredAndFirstServiceIsRefused(t *testing.T) {
	g := newSvcRig(t)
	mustExec(t, g.st, `INSERT INTO projects(id, name, created_at) VALUES (1, 'p', 'x')`)
	for i := 0; i < policy.MaxServices-1; i++ {
		mustExec(t, g.st, `INSERT INTO services(project_id, name, image_ref, health_kind, health_timeout_s, health_passes, strategy, drain_s, stop_grace_s, created_at, updated_at)
			VALUES (1, ?, 'nginx:1', 'running', 60, 3, 'blue_green', 10, 10, 'x', 'x')`, fmt.Sprintf("s%d", i))
	}
	g.create("p", "last", nil) // the 100th
	_, err := callAs[ServiceChangeOut](g, adminActor(), "services.create", m{"project": "new", "name": "extra", "image_ref": "nginx"})
	wantKind(t, err, actions.ErrConflict)
	if !strings.Contains(err.Error(), "100") {
		t.Errorf("message %q", err)
	}
	if countRows(t, g.st, "services") != policy.MaxServices {
		t.Error("the 101st service was created")
	}
	if countRows(t, g.st, "projects") != 1 {
		t.Error("the refused create left a project behind")
	}
}

func TestPublicPortIsUniqueAndReserved(t *testing.T) {
	g := newSvcRig(t)
	open(t, g.r)
	g.create("p", "a", m{"container_port": 80, "public_port": 9000})
	// Another service cannot take 9000, at create or through set_public_port.
	_, err := callAs[ServiceChangeOut](g, adminActor(), "services.create", m{"project": "p", "name": "b", "image_ref": "nginx", "container_port": 80, "public_port": 9000})
	wantKind(t, err, actions.ErrConflict)
	if !strings.Contains(err.Error(), "p/a") {
		t.Errorf("the message does not name the owner: %q", err)
	}
	g.create("p", "b", m{"container_port": 80})
	_, err = callAs[ServiceChangeOut](g, adminActor(), "services.set_public_port", m{"service": "p/b", "public_port": 9000})
	wantKind(t, err, actions.ErrConflict)
	// Reserved and out-of-range ports (the same list proxy_apply uses).
	for _, port := range []int{22, 80, 443, 1023, 7443, 8443, 8080, 65536, 0} {
		_, err = callAs[ServiceChangeOut](g, adminActor(), "services.set_public_port", m{"service": "p/b", "public_port": port})
		wantKind(t, err, actions.ErrInvalidInput)
	}
	// Both or neither of public_port and clear is refused.
	for _, in := range []m{{"service": "p/b"}, {"service": "p/b", "public_port": 9001, "clear": true}} {
		_, err = callAs[ServiceChangeOut](g, adminActor(), "services.set_public_port", in)
		wantKind(t, err, actions.ErrInvalidInput)
	}
	// A service without a container port cannot be exposed.
	g.create("p", "worker", nil)
	_, err = callAs[ServiceChangeOut](g, adminActor(), "services.set_public_port", m{"service": "p/worker", "public_port": 9002})
	wantKind(t, err, actions.ErrInvalidInput)
	// Clearing frees the port for another service.
	must[ServiceChangeOut](g, "services.set_public_port", m{"service": "p/a", "clear": true})
	if out := must[ServiceChangeOut](g, "services.set_public_port", m{"service": "p/b", "public_port": 9000}); !out.Applied || *out.Service.PublicPort != 9000 {
		t.Errorf("%+v", out)
	}
	// The database has the last word: a UNIQUE violation inside the write is a conflict, not a 500.
	g2 := newSvcRig(t)
	open(t, g2.r)
	g2.create("p", "a", m{"container_port": 80})
	g2.create("p", "b", m{"container_port": 80, "public_port": 9100})
	d := &svcDeps{ServiceDeps: ServiceDeps{DB: g2.st, Routes: g2.r, Now: g2.clk.Now}}
	err = d.writePort(context.Background(), 1, nullInt(9100))
	wantKind(t, err, actions.ErrConflict)
}

func TestServiceUpdateSameRepositoryRule(t *testing.T) {
	g := newSvcRig(t)
	g.create("p", "web", m{"image_ref": "nginx:1.26", "container_port": 80})
	up := func(in m) (ServiceChangeOut, error) {
		in["service"] = "p/web"
		return callAs[ServiceChangeOut](g, adminActor(), "services.update", in)
	}
	// Another tag, a digest, and the long spelling of the same repository.
	digest := "sha256:" + strings.Repeat("a", 64)
	for _, ref := range []string{"nginx:1.27", "nginx@" + digest, "docker.io/library/nginx:1.28", "DOCKER.IO/library/nginx:1.29"} {
		out, err := up(m{"image_ref": ref})
		if err != nil || out.Service.ImageRef == "" || out.Notice != "Saved. Deploy to apply." {
			t.Fatalf("%s: %+v %v", ref, out, err)
		}
	}
	// Another repository, another registry, or a lookalike: refused for everyone.
	before := g.get("p/web").ImageRef
	for _, ref := range []string{"redis:7", "nginxinc/nginx:1", "registry.example/nginx:1", "docker.io/library/nginx2:1"} {
		_, err := up(m{"image_ref": ref})
		wantKind(t, err, actions.ErrInvalidInput)
		if !strings.Contains(err.Error(), "services.set_source") {
			t.Errorf("%s: message %q", ref, err)
		}
	}
	if got := g.get("p/web").ImageRef; got != before {
		t.Errorf("the image changed to %q", got)
	}
	// Nothing to change, and each field's validation.
	for _, in := range []m{{}, {"container_port": 0}, {"strategy": "x"}, {"drain_s": 1}, {"stop_grace_s": 999},
		{"health": m{"kind": "http"}}, {"health": m{"timeout_s": 1}}, {"image_ref": "BAD"}} {
		_, err := up(in)
		wantKind(t, err, actions.ErrInvalidInput)
	}
	// Health and strategy: change the check, the timings and the strategy; unchanged fields stay.
	out, err := up(m{"health": m{"kind": "http", "path": "/healthz", "timeout_s": 30}, "strategy": "stop_first", "drain_s": 20, "stop_grace_s": 0})
	if err != nil {
		t.Fatal(err)
	}
	s := out.Service
	if s.Health != (HealthOut{Kind: "http", Path: "/healthz", TimeoutS: 30, Passes: 3}) || s.Strategy != "stop_first" || s.DrainS != 20 || s.StopGraceS != 0 {
		t.Errorf("%+v", s)
	}
	// Leaving the http kind drops its path; the other fields stay.
	out, err = up(m{"health": m{"kind": "tcp"}})
	if err != nil || out.Service.Health != (HealthOut{Kind: "tcp", TimeoutS: 30, Passes: 3}) {
		t.Errorf("%+v %v", out, err)
	}
	if _, err = up(m{"health": m{"path": "/x"}}); err == nil {
		t.Error("a path on a tcp check was accepted")
	}
	// A service that never had a container port cannot get a tcp check.
	g.create("p", "job", nil)
	_, err = callAs[ServiceChangeOut](g, adminActor(), "services.update", m{"service": "p/job", "health": m{"kind": "tcp"}})
	wantKind(t, err, actions.ErrInvalidInput)
	out2, err := callAs[ServiceChangeOut](g, adminActor(), "services.update", m{"service": "p/job", "container_port": 3000, "health": m{"kind": "tcp"}})
	if err != nil || *out2.Service.ContainerPort != 3000 || out2.Service.Health.Kind != "tcp" {
		t.Errorf("%+v %v", out2, err)
	}
	_, err = callAs[ServiceChangeOut](g, adminActor(), "services.update", m{"service": "p/nosuch", "strategy": "stop_first"})
	wantKind(t, err, actions.ErrNotFound)
}

func TestServiceSetSourceChangesTheRepository(t *testing.T) {
	g := newSvcRig(t)
	g.create("p", "web", m{"image_ref": "nginx:1"})
	out := must[ServiceChangeOut](g, "services.set_source", m{"service": "p/web", "image_ref": "ghcr.io/acme/web:2"})
	if out.Service.ImageRef != "ghcr.io/acme/web:2" || out.Notice != "Saved. Deploy to apply." {
		t.Errorf("%+v", out)
	}
	_, err := callAs[ServiceChangeOut](g, adminActor(), "services.set_source", m{"service": "p/nosuch", "image_ref": "nginx"})
	wantKind(t, err, actions.ErrNotFound)
	_, err = callAs[ServiceChangeOut](g, adminActor(), "services.set_source", m{"service": "p/web", "image_ref": "169.254.169.254/x"})
	wantKind(t, err, actions.ErrInvalidInput)
}

func TestSetPublicPortGoesThroughApplyRoutes(t *testing.T) {
	g := newSvcRig(t)
	open(t, g.r)
	g.create("p", "web", m{"container_port": 80})
	if g.px.n() != 0 {
		t.Fatalf("a service without a port sent proxy_apply %d times", g.px.n())
	}
	out := must[ServiceChangeOut](g, "services.set_public_port", m{"service": "p/web", "public_port": 9000})
	if !out.Applied || out.Notice != "" || *out.Service.PublicPort != 9000 || !out.Service.RoutesReady {
		t.Fatalf("%+v", out)
	}
	if got := g.px.last(); len(got) != 1 || got[0].Port != 9000 || got[0].Page != agentops.PageNotRunning {
		t.Errorf("proxy_apply got %+v", got)
	}
	// The DB write committed before ApplyRoutes returned.
	if v := g.get("p/web").PublicPort; v == nil || *v != 9000 {
		t.Errorf("stored %v", v)
	}
	// A change of port is one apply; clearing is one more with an empty list.
	must[ServiceChangeOut](g, "services.set_public_port", m{"service": "p/web", "public_port": 9001})
	if got := g.px.last(); len(got) != 1 || got[0].Port != 9001 {
		t.Errorf("%+v", got)
	}
	n := g.px.n()
	cl := must[ServiceChangeOut](g, "services.set_public_port", m{"service": "p/web", "clear": true})
	if !cl.Applied || cl.Service.PublicPort != nil || len(g.px.last()) != 0 || g.px.n() != n+1 {
		t.Errorf("%+v calls %d (was %d)", cl, g.px.n(), n)
	}
	// Clearing a port that is not set writes and applies nothing.
	n = g.px.n()
	if again := must[ServiceChangeOut](g, "services.set_public_port", m{"service": "p/web", "clear": true}); !again.Applied || g.px.n() != n {
		t.Errorf("%+v %d", again, g.px.n())
	}
}

// A mutate that runs under the route lock must not need a second write
// transaction while the caller holds one, and must commit before ApplyRoutes
// returns: a proxy that fails still leaves the port saved.
func TestSetPublicPortReportsAFailedApplyHonestly(t *testing.T) {
	g := newSvcRig(t)
	open(t, g.r)
	g.create("p", "web", m{"container_port": 80})
	g.px.fail = errors.New("agent gone: /secret/path")
	out := must[ServiceChangeOut](g, "services.set_public_port", m{"service": "p/web", "public_port": 9000})
	if out.Applied || !strings.Contains(out.Notice, "could not apply") || strings.Contains(out.Notice, "secret") {
		t.Fatalf("%+v", out)
	}
	if v := g.get("p/web").PublicPort; v == nil || *v != 9000 {
		t.Errorf("the port was not saved: %v", v)
	}
	// The next try (the agent is back) applies it, and says so.
	g.px.fail = nil
	again := must[ServiceChangeOut](g, "services.set_public_port", m{"service": "p/web", "public_port": 9000})
	if !again.Applied || g.px.last()[0].Port != 9000 {
		t.Errorf("%+v %+v", again, g.px.last())
	}
}

func TestSetPublicPortBeforeTheBootGateIsNotReportedApplied(t *testing.T) {
	g := newSvcRig(t) // the gate is closed: no Sync yet, and no Boot hook
	g.create("p", "web", m{"container_port": 80})
	out := must[ServiceChangeOut](g, "services.set_public_port", m{"service": "p/web", "public_port": 9000})
	if out.Applied || !strings.Contains(out.Notice, "not applied yet") || out.Service.RoutesReady {
		t.Fatalf("%+v", out)
	}
	if v := g.get("p/web").PublicPort; v == nil || *v != 9000 {
		t.Errorf("the port was not saved: %v", v)
	}
	if g.px.n() != 0 {
		t.Error("proxy_apply was sent before the gate opened")
	}
	// The same through create with a public port, and through retry_port.
	out = g.create("p", "two", m{"container_port": 80, "public_port": 9001})
	if out.Applied || *out.Service.PublicPort != 9001 {
		t.Errorf("%+v", out)
	}
	rt := must[ServiceChangeOut](g, "services.retry_port", m{"service": "p/two"})
	if rt.Applied {
		t.Errorf("retry_port claimed success with the gate closed: %+v", rt)
	}
	// Same port again with the gate closed: still not applied (RetryFailedPorts has nothing to retry).
	again := must[ServiceChangeOut](g, "services.set_public_port", m{"service": "p/two", "public_port": 9001})
	if again.Applied {
		t.Errorf("%+v", again)
	}
}

// A closed gate is opened lazily by the Boot hook before the route is applied.
func TestActionsOpenTheBootGateLazily(t *testing.T) {
	var g *svcRig
	g = newSvcRig(t, func(d *ServiceDeps) {
		d.Boot = func(ctx context.Context) error {
			g.boots++
			seq := []string{}
			return g.r.Sync(ctx, &fakeSource{seq: &seq})
		}
	})
	g.create("p", "web", m{"container_port": 80})
	out := must[ServiceChangeOut](g, "services.set_public_port", m{"service": "p/web", "public_port": 9000})
	if !out.Applied || g.boots != 1 || !g.r.Ready() {
		t.Fatalf("%+v boots=%d", out, g.boots)
	}
	if got := g.px.last(); len(got) != 1 || got[0].Port != 9000 {
		t.Errorf("%+v", got)
	}
	// Once ready the hook is not called again.
	must[ServiceChangeOut](g, "services.set_public_port", m{"service": "p/web", "public_port": 9001})
	if g.boots != 1 {
		t.Errorf("boots=%d", g.boots)
	}
}

// A port the proxy could not open (another program has it) is reported with the
// service and kept until retried; saving the same port again, and the retry
// action, both send the routes again.
func TestBindFailureIsReportedAndRetried(t *testing.T) {
	g := newSvcRig(t)
	open(t, g.r)
	g.create("p", "web", m{"container_port": 80})
	g.px.failed = []int{9000}
	out := must[ServiceChangeOut](g, "services.set_public_port", m{"service": "p/web", "public_port": 9000})
	if out.Applied || !strings.Contains(out.Notice, "port 9000 could not be opened") || out.Service.RouteError != BindMessage(9000) {
		t.Fatalf("%+v", out)
	}
	// BindStatus is on get and list too, for that service only.
	g.create("p", "other", m{"container_port": 80})
	if got := g.get("p/web").RouteError; got != BindMessage(9000) {
		t.Errorf("get: %q", got)
	}
	for _, s := range must[ServicesOut](g, "services.list", m{}).Services {
		if (s.Name == "web") != (s.RouteError != "") {
			t.Errorf("list: %s route_error %q", s.Name, s.RouteError)
		}
	}
	// Still taken: saving the same port again retries and still says so.
	n := g.px.n()
	again := must[ServiceChangeOut](g, "services.set_public_port", m{"service": "p/web", "public_port": 9000})
	if again.Applied || g.px.n() != n+1 {
		t.Errorf("%+v calls %d (was %d)", again, g.px.n(), n)
	}
	// "Try again" while it is still taken: sends, still reports it.
	n = g.px.n()
	rt := must[ServiceChangeOut](g, "services.retry_port", m{"service": "p/web"})
	if rt.Applied || g.px.n() != n+1 {
		t.Errorf("%+v", rt)
	}
	// The other program lets go: "Try again" opens it.
	g.px.failed = nil
	rt = must[ServiceChangeOut](g, "services.retry_port", m{"service": "p/web"})
	if !rt.Applied || rt.Service.RouteError != "" || rt.Notice != "" {
		t.Fatalf("%+v", rt)
	}
	// Nothing failed now: another "Try again" sends nothing.
	n = g.px.n()
	must[ServiceChangeOut](g, "services.retry_port", m{"service": "p/web"})
	if g.px.n() != n {
		t.Error("retry_port reloaded with nothing failed")
	}
	// The same port saved again after the failure cleared is also a retry that works.
	g.px.failed = []int{9000}
	must[ServiceChangeOut](g, "services.set_public_port", m{"service": "p/web", "public_port": 9001})
	g.px.failed = nil
	if out := must[ServiceChangeOut](g, "services.set_public_port", m{"service": "p/web", "public_port": 9001}); !out.Applied {
		t.Errorf("%+v", out)
	}
	// The resend is global, so retry_port needs only an existing service, not a port.
	if rt := must[ServiceChangeOut](g, "services.retry_port", m{"service": "p/other"}); !rt.Applied {
		t.Errorf("%+v", rt)
	}
	_, err := callAs[ServiceChangeOut](g, adminActor(), "services.retry_port", m{"service": "p/nosuch"})
	wantKind(t, err, actions.ErrNotFound)
}

func TestSharesPanelAddressWarning(t *testing.T) {
	g := newSvcRig(t)
	open(t, g.r)
	has := func(s ServiceOut) bool {
		for _, w := range s.Warnings {
			if w.Code == WarnSharesPanelAddress {
				if !strings.Contains(w.Message, "same address as the panel") || !strings.Contains(w.Message, "plain HTTP") ||
					!strings.Contains(w.Message, "port 9000") || !strings.Contains(w.Message, "cookies") {
					t.Errorf("wording: %q", w.Message)
				}
				return true
			}
		}
		return false
	}
	// create with a public port, get, list and set_public_port all carry it.
	c := g.create("p", "web", m{"container_port": 80, "public_port": 9000})
	if !has(c.Service) || !has(g.get("p/web")) {
		t.Fatal("no warning on a service with a public port")
	}
	if l := must[ServicesOut](g, "services.list", m{}); !has(l.Services[0]) {
		t.Error("list lacks the warning")
	}
	// A service without one has none, and the field is an empty list (not null).
	n := g.create("p", "plain", m{"container_port": 80}).Service
	if has(n) || n.Warnings == nil {
		t.Errorf("%+v", n.Warnings)
	}
	raw, _ := json.Marshal(n)
	if !strings.Contains(string(raw), `"warnings":[]`) {
		t.Errorf("%s", raw)
	}
	s := must[ServiceChangeOut](g, "services.set_public_port", m{"service": "p/plain", "public_port": 9000 + 1})
	if !strings.Contains(s.Service.Warnings[0].Message, "port 9001") {
		t.Errorf("%+v", s.Service.Warnings)
	}
	// Clearing the port clears the warning.
	if cl := must[ServiceChangeOut](g, "services.set_public_port", m{"service": "p/plain", "clear": true}); has(cl.Service) {
		t.Error("the warning stayed after the port was cleared")
	}
}

func TestServiceStateFollowsTheStateMap(t *testing.T) {
	g := newSvcRig(t)
	g.create("p", "web", m{"container_port": 80, "public_port": 9000})
	var id, ip = int64(0), "10.250.1.2"
	mustExec(t, g.st, `UPDATE services SET public_port = 9000 WHERE name = 'web'`)
	var svc int64
	_ = g.st.R.QueryRow(`SELECT id FROM services WHERE name = 'web'`).Scan(&svc)
	id = addDeploy(t, g.st, svc, 7, "succeeded", ip)
	mustExec(t, g.st, `UPDATE services SET active_deploy_id = ? WHERE id = ?`, id, svc)
	if s := g.get("p/web"); s.State != "unknown" || *s.ActiveDeploy != 7 || s.RoutesReady {
		t.Errorf("before the gate opens: %+v", s)
	}
	open(t, g.r)
	if s := g.get("p/web"); s.State != "missing" {
		t.Errorf("no container: %q", s.State)
	}
	for state, want := range map[string]string{StateRunning: "running", StateRestarting: "restarting", "exited": "stopped"} {
		if err := g.r.SetContainer(context.Background(), up(svc, id, 1, state, ip)); err != nil {
			t.Fatal(err)
		}
		if s := g.get("p/web"); s.State != want {
			t.Errorf("%s: state %q, want %q", state, s.State, want)
		}
	}
	// A container of the right deploy at another address is not "running".
	if err := g.r.SetContainer(context.Background(), up(svc, id, 1, StateRunning, "10.250.1.9")); err != nil {
		t.Fatal(err)
	}
	if s := g.get("p/web"); s.State != "stopped" {
		t.Errorf("wrong address: %q", s.State)
	}
}

func nullInt(v int) sql.NullInt64 { return sql.NullInt64{Int64: int64(v), Valid: true} }

// ---- system.exposure ----

func pub(name string, ports ...agentops.PublishedPort) agentops.PublishingContainer {
	return agentops.PublishingContainer{Name: name, Ports: ports}
}

func pp(host string, port int) agentops.PublishedPort {
	return agentops.PublishedPort{HostIP: host, HostPort: port, ContainerPort: port, Proto: "tcp"}
}

// system.exposure lists the containers that publish on all interfaces or on the
// panel's own addresses, leaves out what is not reachable from there, keeps the
// answer for 30 seconds on read (no timer), and does not cache a failure. (The
// agent leaves ServerDock's own containers out: agentops
// TestContainerPortsExcludesOurOwnContainers.)
func TestExposureListsForeignPublishedPorts(t *testing.T) {
	panel := netip.MustParseAddr("203.0.113.5")
	g := newSvcRig(t, func(d *ServiceDeps) {
		d.PanelAddrs = func() []netip.Addr { return []netip.Addr{panel, netip.MustParseAddr("127.0.0.1")} }
	})
	g.ports.res = agentops.PortsResult{Containers: []agentops.PublishingContainer{
		pub("portainer", pp("0.0.0.0", 9443)),
		pub("grafana", pp("::", 3000), pp("127.0.0.1", 3001)),
		pub("private", pp("10.0.0.7", 5432)),                     // another address: not the panel's
		pub("bound", pp("203.0.113.5", 8081)),                    // exactly the panel's address
		pub("mapped", pp("::ffff:203.0.113.5", 8082)),            // IPv4-mapped form of it
		pub("tunnel", pp("127.0.0.1", 7000)),                     // the panel is reachable on loopback here
		pub("other-v6", pp("2001:db8::9", 8083)),                 // not the panel's
		pub("junk", agentops.PublishedPort{HostIP: "not-an-ip"}), // unparsable: not counted
	}, Truncated: true}
	out, err := callAs[ExposureOut](g, readTok, "system.exposure", m{})
	if err != nil {
		t.Fatal(err)
	}
	var got []string
	for _, c := range out.Containers {
		for _, p := range c.Ports {
			got = append(got, fmt.Sprintf("%s:%d", c.Name, p.HostPort))
		}
	}
	want := "bound:8081 grafana:3000 grafana:3001 mapped:8082 portainer:9443 tunnel:7000"
	if strings.Join(got, " ") != want || !out.Truncated || !out.CheckedAt.Equal(g.clk.Now()) {
		t.Fatalf("got %q truncated=%v checked=%v, want %q", strings.Join(got, " "), out.Truncated, out.CheckedAt, want)
	}

	// Cached for 30 s: one question to Docker however many callers ask.
	if g.ports.n() != 1 {
		t.Fatalf("calls %d", g.ports.n())
	}
	g.ports.res = agentops.PortsResult{}
	g.clk.Add(29 * time.Second)
	for i := 0; i < 5; i++ {
		again, err := callAs[ExposureOut](g, readTok, "system.exposure", m{})
		if err != nil || len(again.Containers) != 5 || g.ports.n() != 1 {
			t.Fatalf("cached read %d: %d containers, %d calls, %v", i, len(again.Containers), g.ports.n(), err)
		}
	}
	// Past the TTL the next read asks again; nothing refreshed it in between (no timer).
	g.clk.Add(2 * time.Second)
	fresh, err := callAs[ExposureOut](g, readTok, "system.exposure", m{})
	if err != nil || len(fresh.Containers) != 0 || g.ports.n() != 2 {
		t.Fatalf("after the TTL: %+v calls %d %v", fresh, g.ports.n(), err)
	}
	// A failure is reported as unavailable and remembered for 5 s (no call per
	// waiter behind a hung Docker); after that the next read asks again.
	g.clk.Add(31 * time.Second)
	g.ports.err = errors.New("dockerd: /var/run/docker.sock: connection refused")
	_, err = callAs[ExposureOut](g, readTok, "system.exposure", m{})
	wantKind(t, err, actions.ErrUnavailable)
	if strings.Contains(err.Error(), "docker.sock") {
		t.Errorf("the message leaks the cause: %v", err)
	}
	g.ports.err = nil
	g.clk.Add(3 * time.Second)
	if _, err = callAs[ExposureOut](g, readTok, "system.exposure", m{}); !errors.Is(err, actions.ErrUnavailable) || g.ports.n() != 3 {
		t.Errorf("within 5 s the failure is remembered: %v calls %d", err, g.ports.n())
	}
	g.clk.Add(3 * time.Second)
	if _, err = callAs[ExposureOut](g, readTok, "system.exposure", m{}); err != nil || g.ports.n() != 4 {
		t.Errorf("%v calls %d", err, g.ports.n())
	}
	// A clock that went backwards does not serve a stale answer forever.
	g.clk.Add(-time.Hour)
	if _, err = callAs[ExposureOut](g, readTok, "system.exposure", m{}); err != nil || g.ports.n() != 5 {
		t.Errorf("%v calls %d", err, g.ports.n())
	}
}

func TestExposureWithNoPanelAddressesListsOnlyAllInterfaces(t *testing.T) {
	g := newSvcRig(t)
	g.ports.res = agentops.PortsResult{Containers: []agentops.PublishingContainer{pub("a", pp("0.0.0.0", 1)), pub("b", pp("10.0.0.1", 2)), pub("c", pp("", 3))}}
	out, err := callAs[ExposureOut](g, readTok, "system.exposure", m{})
	if err != nil || len(out.Containers) != 2 || out.Containers[0].Name != "a" || out.Containers[1].Name != "c" {
		t.Errorf("%+v %v", out, err)
	}
	// No source at all: unavailable, not a panic.
	g2 := newSvcRig(t, func(d *ServiceDeps) { d.Exposure = nil })
	_, err = callAs[ExposureOut](g2, readTok, "system.exposure", m{})
	wantKind(t, err, actions.ErrUnavailable)
}

// ---- token powers ----

// A read token lists; a write token may update the health check and the tag but
// may not create, change the source, set or retry the public port, nor change
// the repository through update; an admin token can do all of those; no token
// can delete (services.delete arrives in step 9 as a destructive, admin,
// user-only action: tokens are denied by the registry whatever scope they hold).
func TestTokenScopesForServiceActions(t *testing.T) {
	g := newSvcRig(t)
	open(t, g.r)
	g.create("p", "web", m{"image_ref": "nginx:1", "container_port": 80})

	denied := func(who string, a actions.Actor, name string, in m) {
		t.Helper()
		before := dump(t, g.st)
		_, err := callAs[any](g, a, name, in)
		wantKind(t, err, actions.ErrDenied)
		if dump(t, g.st) != before {
			t.Errorf("%s ran %s although denied", who, name)
		}
	}
	allowed := func(who string, a actions.Actor, name string, in m) {
		t.Helper()
		if _, err := callAs[any](g, a, name, in); err != nil {
			t.Errorf("%s %s: %v", who, name, err)
		}
	}
	createIn := func(n string) m {
		return m{"project": "p", "name": n, "image_ref": "nginx"}
	}
	web := func(extra m) m {
		in := m{"service": "p/web"}
		for k, v := range extra {
			in[k] = v
		}
		return in
	}

	// read: lists and gets, and the exposure check; nothing that writes.
	for _, n := range []string{"projects.list", "services.list", "system.exposure"} {
		allowed("read", readTok, n, m{})
	}
	allowed("read", readTok, "services.get", web(nil))
	denied("read", readTok, "services.update", web(m{"strategy": "stop_first"}))
	denied("read", readTok, "services.create", createIn("r1"))
	denied("read", readTok, "services.set_source", web(m{"image_ref": "redis"}))
	denied("read", readTok, "services.set_public_port", web(m{"public_port": 9000}))
	denied("read", readTok, "services.retry_port", web(nil))

	// write: update (health, tag), not the rest.
	allowed("write", writeTok, "services.update", web(m{"health": m{"kind": "http", "path": "/up"}}))
	allowed("write", writeTok, "services.update", web(m{"image_ref": "nginx:1.27"}))
	allowed("write", writeTok, "services.get", web(nil))
	denied("write", writeTok, "services.create", createIn("w1"))
	denied("write", writeTok, "services.set_source", web(m{"image_ref": "nginx:1.28"}))
	denied("write", writeTok, "services.set_public_port", web(m{"public_port": 9000}))
	denied("write", writeTok, "services.retry_port", web(nil))
	// A write token cannot change the repository through update (and nor can anyone).
	_, err := callAs[any](g, writeTok, "services.update", web(m{"image_ref": "evil.example/miner:1"}))
	wantKind(t, err, actions.ErrInvalidInput)
	if got := g.get("p/web"); got.ImageRef != "nginx:1.27" || got.Health.Path != "/up" {
		t.Errorf("%+v", got)
	}

	// A write token may not move the traffic of an exposed service: changing the
	// container port of a service that has a public port needs admin. It may on a
	// service that is not exposed, and may "change" it to the value it has.
	must[ServiceChangeOut](g, "services.create", m{"project": "p", "name": "exposed", "image_ref": "nginx", "container_port": 80, "public_port": 9300})
	if _, err := callAs[any](g, writeTok, "services.update", m{"service": "p/exposed", "container_port": 81}); !errors.Is(err, actions.ErrDenied) {
		t.Errorf("a write token changed the container port of an exposed service: %v", err)
	}
	if got := g.get("p/exposed"); *got.ContainerPort != 80 {
		t.Errorf("container port %d", *got.ContainerPort)
	}
	allowed("write", writeTok, "services.update", m{"service": "p/exposed", "container_port": 80})
	allowed("write", writeTok, "services.update", m{"service": "p/web", "container_port": 81}) // not exposed yet
	allowed("admin", adminTok, "services.update", m{"service": "p/exposed", "container_port": 81})

	// admin token: all of them.
	allowed("admin", adminTok, "services.create", createIn("a1"))
	allowed("admin", adminTok, "services.set_source", web(m{"image_ref": "redis:7"}))
	allowed("admin", adminTok, "services.set_public_port", web(m{"public_port": 9000}))
	allowed("admin", adminTok, "services.retry_port", web(nil))
	allowed("admin", adminTok, "services.update", web(m{"strategy": "stop_first"}))
	// Even an admin token cannot change the repository through update.
	_, err = callAs[any](g, adminTok, "services.update", web(m{"image_ref": "nginx:1"}))
	wantKind(t, err, actions.ErrInvalidInput)

	// MCP clients are read-only.
	mcp := actions.MCPActor("m1")
	allowed("mcp", mcp, "services.list", m{})
	denied("mcp", mcp, "services.update", web(m{"strategy": "blue_green"}))
	denied("mcp", mcp, "services.create", createIn("m1"))

	// No token can delete: there is no such action yet, and when step 9 adds it
	// the registry's own rule (destructive: user session only, fresh 2FA, typed
	// target) already refuses every token. Both must hold for every service action.
	for _, tok := range []actions.Actor{readTok, writeTok, adminTok} {
		if _, err := callAs[any](g, tok, "services.delete", web(nil)); err == nil {
			t.Error("a token ran services.delete")
		}
	}
	for _, info := range g.reg.List() {
		if !strings.HasPrefix(info.Name, "services.") {
			continue
		}
		if info.Risk == actions.RiskDestructive && (info.Scope < actions.ScopeAdmin || info.Name == "services.delete" && info.System != "") {
			t.Errorf("%s is destructive with scope %v / system %q", info.Name, info.Scope, info.System)
		}
		if info.JobOnly || info.ConsoleOnly {
			t.Errorf("%s is job-only or console-only: step 7 queues no job", info.Name)
		}
	}
}

// The scope of every action step 7 adds, as the plan's table says.
func TestServiceActionRiskAndScopeTable(t *testing.T) {
	g := newSvcRig(t)
	want := map[string]struct {
		risk  actions.Risk
		scope actions.Scope
	}{
		"projects.list": {actions.RiskRead, actions.ScopeRead}, "services.list": {actions.RiskRead, actions.ScopeRead},
		"services.get": {actions.RiskRead, actions.ScopeRead}, "system.exposure": {actions.RiskRead, actions.ScopeRead},
		"services.create": {actions.RiskMutate, actions.ScopeAdmin}, "services.update": {actions.RiskMutate, actions.ScopeWrite},
		"services.set_source": {actions.RiskMutate, actions.ScopeAdmin}, "services.set_public_port": {actions.RiskMutate, actions.ScopeAdmin},
		"services.retry_port": {actions.RiskMutate, actions.ScopeAdmin},
		// Step 8a: queuing a deploy and cancelling one (write scope, like update).
		"services.deploy": {actions.RiskMutate, actions.ScopeWrite}, "deploys.cancel": {actions.RiskMutate, actions.ScopeWrite},
	}
	for name, w := range want {
		info, ok := g.reg.Lookup(name)
		if !ok || info.Risk != w.risk || info.Scope != w.scope || info.Sensitive || info.SessionOnly || info.System != "" {
			t.Errorf("%s: %+v", name, info)
		}
	}
	if got := len(g.reg.List()); got != len(want) {
		t.Errorf("%d actions registered, the table has %d", got, len(want))
	}
}

// dump is a digest of the tables a service action may change.
func dump(t *testing.T, st *store.Store) string {
	t.Helper()
	var b strings.Builder
	for _, q := range []string{
		`SELECT id, name FROM projects ORDER BY id`,
		`SELECT id, project_id, name, image_ref, container_port, public_port, health_kind, health_path, health_timeout_s, health_passes, strategy, drain_s, stop_grace_s, updated_at FROM services ORDER BY id`,
	} {
		rs, err := st.R.Query(q)
		if err != nil {
			t.Fatal(err)
		}
		cols, _ := rs.Columns()
		for rs.Next() {
			vals := make([]any, len(cols))
			ptrs := make([]any, len(cols))
			for i := range vals {
				ptrs[i] = &vals[i]
			}
			if err := rs.Scan(ptrs...); err != nil {
				t.Fatal(err)
			}
			fmt.Fprintln(&b, vals...)
		}
		rs.Close()
	}
	return b.String()
}

func TestServiceActionsRegisterNeedsDependencies(t *testing.T) {
	defer func() {
		if recover() == nil {
			t.Error("no panic without a database")
		}
	}()
	RegisterServiceActions(actions.New(&noSink{}, slog.New(slog.DiscardHandler)), ServiceDeps{})
}

type noSink struct{}

func (*noSink) Append(context.Context, audit.Entry) (int64, error) { return 0, nil }
func (*noSink) Denied(context.Context, audit.Entry) (bool, error)  { return false, nil }

// A change with no route involvement is applied by definition, whether or not
// the routes are ready: no "not applied" and no "Try again" for a portless
// service, a tag, a health check, or clearing a port that is not set.
func TestChangesWithoutARouteAreAppliedEvenWithTheGateClosed(t *testing.T) {
	var g *svcRig
	g = newSvcRig(t, func(d *ServiceDeps) { // gate closed, and a Boot hook that counts its calls
		d.Boot = func(context.Context) error { g.boots++; return nil }
	})
	check := func(what string, out ServiceChangeOut) {
		t.Helper()
		if !out.Applied || strings.Contains(out.Notice, "Try again") || strings.Contains(out.Notice, "not applied") {
			t.Errorf("%s: %+v", what, out)
		}
	}
	check("create (worker)", g.create("p", "worker", nil))
	check("create (portless)", g.create("p", "web", m{"container_port": 80}))
	check("update", must[ServiceChangeOut](g, "services.update", m{"service": "p/web", "strategy": "stop_first"}))
	check("set_source", must[ServiceChangeOut](g, "services.set_source", m{"service": "p/web", "image_ref": "redis:7"}))
	n := g.px.n()
	check("clear a port that is not set", must[ServiceChangeOut](g, "services.set_public_port", m{"service": "p/web", "clear": true}))
	if g.px.n() != n || g.boots != 0 {
		t.Error("a change without a route touched the proxy or the boot")
	}
	// With a port the same gate is "not applied".
	if out := must[ServiceChangeOut](g, "services.set_public_port", m{"service": "p/web", "public_port": 9000}); out.Applied || g.boots != 1 {
		t.Errorf("%+v boots=%d", out, g.boots)
	}
}

// A refused request (duplicate name, 101st service, a UNIQUE race) never builds
// or sends the routes: no reload, even with a port that failed to open.
func TestRefusedChangesNeverReloadTheProxy(t *testing.T) {
	g := newSvcRig(t)
	open(t, g.r)
	g.create("p", "a", m{"container_port": 80, "public_port": 9000})
	g.px.failed = []int{9000}
	must[ServiceChangeOut](g, "services.retry_port", m{"service": "p/a"}) // a failed port is on record now
	n := g.px.n()
	// Duplicate name with a public port: refused by the insert inside the mutate.
	_, err := callAs[ServiceChangeOut](g, adminActor(), "services.create", m{"project": "p", "name": "a", "image_ref": "nginx", "container_port": 80, "public_port": 9001})
	wantKind(t, err, actions.ErrConflict)
	// A UNIQUE race inside the write.
	g.create("p", "b", m{"container_port": 80, "public_port": 9100})
	d := &svcDeps{ServiceDeps: ServiceDeps{DB: g.st, Now: g.clk.Now}}
	werr := g.r.ApplyRoutes(context.Background(), func() error { return d.writePort(context.Background(), 1, nullInt(9100)) })
	var me *MutateError
	if !errors.As(werr, &me) || !errors.Is(werr, actions.ErrConflict) {
		t.Fatalf("%v", werr)
	}
	if g.px.n() != n+1 { // only the successful create of b applied
		t.Errorf("proxy_apply calls %d, want %d", g.px.n(), n+1)
	}
}

func TestStaleRouteErrorShowsOnServicesAndClears(t *testing.T) {
	g := newSvcRig(t)
	open(t, g.r)
	g.create("p", "web", m{"container_port": 80})
	g.create("p", "other", m{"container_port": 80})
	g.px.fail = errors.New("agent gone")
	out := must[ServiceChangeOut](g, "services.set_public_port", m{"service": "p/web", "public_port": 9000})
	if out.Applied || out.Service.RouteError != StaleMessage {
		t.Fatalf("%+v", out)
	}
	if got := g.get("p/web"); got.RouteError != StaleMessage || !got.RoutesStale {
		t.Errorf("get: %q stale=%v", got.RouteError, got.RoutesStale)
	}
	// routes_stale is system-wide: a service that was just cleared shows it too.
	must[ServiceChangeOut](g, "services.set_public_port", m{"service": "p/web", "clear": true})
	if got := g.get("p/other"); !got.RoutesStale || got.RouteError != "" {
		t.Errorf("other: %+v", got)
	}
	g.px.fail = nil
	must[ServiceChangeOut](g, "services.retry_port", m{"service": "p/web"})
	if got := g.get("p/web"); got.RouteError != "" || got.RoutesStale {
		t.Errorf("after a successful apply: %+v", got)
	}
}

// A client that goes away right after the commit still gets the change applied.
func TestSetPublicPortAppliesAfterTheClientCancels(t *testing.T) {
	g := newSvcRig(t)
	open(t, g.r)
	g.create("p", "web", m{"container_port": 80})
	ctx, cancel := context.WithCancel(context.Background())
	// The rig's fake proxy runs on whatever context ApplyRoutes hands it; cancel
	// once the write has committed by wrapping the write in the mutate.
	d := &svcDeps{ServiceDeps: ServiceDeps{DB: g.st, Routes: g.r, Now: g.clk.Now}}
	err := g.r.ApplyRoutes(ctx, func() error {
		if err := d.writePort(ctx, 1, nullInt(9000)); err != nil {
			return err
		}
		cancel()
		return nil
	})
	if err != nil || g.px.n() != 1 || g.px.last()[0].Port != 9000 {
		t.Fatalf("%v calls %d", err, g.px.n())
	}
}

func TestPublicPortsAreRefusedWithTheProxyOff(t *testing.T) {
	g := newSvcRig(t, func(d *ServiceDeps) { d.NoProxy = true })
	g.create("p", "web", m{"container_port": 80}) // portless is fine
	for _, c := range []struct {
		name string
		in   m
	}{
		{"services.create", m{"project": "p", "name": "x", "image_ref": "nginx", "container_port": 80, "public_port": 9000}},
		{"services.set_public_port", m{"service": "p/web", "public_port": 9000}},
		{"services.retry_port", m{"service": "p/web"}},
	} {
		_, err := callAs[ServiceChangeOut](g, adminActor(), c.name, c.in)
		wantKind(t, err, actions.ErrConflict)
		if !strings.Contains(err.Error(), "embedded proxy is off") {
			t.Errorf("%s: %q", c.name, err)
		}
	}
	if countRows(t, g.st, "services") != 1 || g.boots != 0 || g.px.n() != 0 {
		t.Error("a refused request wrote or applied something")
	}
	// Clearing a port that is set (saved before the proxy was switched off) just clears it.
	mustExec(t, g.st, `UPDATE services SET public_port = 9100 WHERE name = 'web'`)
	if out := must[ServiceChangeOut](g, "services.set_public_port", m{"service": "p/web", "clear": true}); !out.Applied || out.Service.PublicPort != nil {
		t.Errorf("%+v", out)
	}
}

// Waiters do not convoy behind a hung Docker call: they return when their own
// context ends, and the first caller's answer then serves everyone.
func TestExposureWaitersReturnOnTheirOwnContext(t *testing.T) {
	hang := &hangingPorts{release: make(chan struct{}), entered: make(chan struct{}, 4)}
	g2 := newSvcRig(t, func(d *ServiceDeps) { d.Exposure = hang })
	first := make(chan error, 1)
	go func() {
		_, err := callAs[ExposureOut](g2, readTok, "system.exposure", m{})
		first <- err
	}()
	<-hang.entered
	ctx, cancel := context.WithTimeout(context.Background(), 50*time.Millisecond)
	defer cancel()
	raw, _ := json.Marshal(m{})
	start := time.Now()
	_, err := g2.reg.Invoke(ctx, readTok, "system.exposure", raw, actions.Options{})
	wantKind(t, err, actions.ErrUnavailable)
	if time.Since(start) > 2*time.Second {
		t.Error("the waiter waited for the hung call")
	}
	close(hang.release)
	if err := <-first; err != nil {
		t.Fatal(err)
	}
	if hang.calls() != 1 {
		t.Errorf("Docker was asked %d times", hang.calls())
	}
}

type hangingPorts struct {
	release chan struct{}
	entered chan struct{}
	mu      sync.Mutex
	n       int
}

func (h *hangingPorts) ContainerPorts(context.Context) (agentops.PortsResult, error) {
	h.mu.Lock()
	h.n++
	h.mu.Unlock()
	h.entered <- struct{}{}
	<-h.release
	return agentops.PortsResult{}, nil
}

func (h *hangingPorts) calls() int { h.mu.Lock(); defer h.mu.Unlock(); return h.n }

// An action that arrives before the panel is up (the control socket serves
// before the local agent loads the proxy) starts no boot and is told "not
// applied yet"; once the panel is up the boot goes in order and a Try again works.
func TestActionBeforeThePanelIsUpStartsNoBoot(t *testing.T) {
	var g *svcRig
	panelUp := make(chan struct{})
	var booter *Booter
	g = newSvcRig(t, func(d *ServiceDeps) {
		d.Boot = func(ctx context.Context) error { return booter.Run(ctx) }
		d.EnsureBound = 100 * time.Millisecond
	})
	s := &bootSeq{}
	en := &fakeEnsurer{s: s}
	booter = NewBooter(BootConfig{Instance: bootInstance, AppNet: &fakeSubnet{s: s, subnet: "10.250.7.0/24"}, Net: en, Routes: g.r,
		Src: &fakeSource{seq: &s.seq}, SyncRoutes: true, PanelUp: panelUp})
	g.create("p", "web", m{"container_port": 80})
	// ensureRoutes waits for the panel, bounded by EnsureBound (100 ms here).
	raw, _ := json.Marshal(m{"service": "p/web", "public_port": 9000})
	res, err := g.reg.Invoke(context.Background(), adminActor(), "services.set_public_port", raw, actions.Options{})
	if err != nil {
		t.Fatal(err)
	}
	if out := res.Out.(ServiceChangeOut); out.Applied || !strings.Contains(out.Notice, "not applied yet") {
		t.Fatalf("%+v", out)
	}
	if len(s.seq) != 0 || en.calls.Load() != 0 || g.r.Ready() || g.px.n() != 0 {
		t.Fatalf("a boot ran before the panel was up: %v", s.seq)
	}
	close(panelUp)
	out := must[ServiceChangeOut](g, "services.retry_port", m{"service": "p/web"})
	if !out.Applied || strings.Join(s.seq, ",") != "subnet,check,network_ensure,list" {
		t.Errorf("%+v %v", out, s.seq)
	}
}

// A failure that is only the caller's own context ending is not remembered: a
// client that disconnects must not make the next caller get a 503.
func TestExposureCallersCancellationIsNotCached(t *testing.T) {
	src := &ctxPorts{}
	g := newSvcRig(t, func(d *ServiceDeps) { d.Exposure = src })
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Millisecond) // ends during the Docker call
	defer cancel()
	raw, _ := json.Marshal(m{})
	if _, err := g.reg.Invoke(ctx, readTok, "system.exposure", raw, actions.Options{}); err == nil {
		t.Fatal("a cancelled caller got an answer")
	}
	if _, err := callAs[ExposureOut](g, readTok, "system.exposure", m{}); err != nil {
		t.Fatalf("the next caller got %v", err)
	}
	if src.n != 2 {
		t.Errorf("Docker was asked %d times, want 2", src.n)
	}
}

// ctxPorts blocks until the caller's context ends (when it has an end) and then
// fails with its error; with no deadline it answers at once.
type ctxPorts struct{ n int }

func (c *ctxPorts) ContainerPorts(ctx context.Context) (agentops.PortsResult, error) {
	c.n++
	if _, ok := ctx.Deadline(); ok {
		<-ctx.Done()
	}
	return agentops.PortsResult{}, ctx.Err()
}
