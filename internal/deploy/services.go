package deploy

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"log/slog"
	"net/netip"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/Docker-Hunterpedia/serverdock/internal/actions"
	"github.com/Docker-Hunterpedia/serverdock/internal/agentops"
	"github.com/Docker-Hunterpedia/serverdock/internal/policy"
	"github.com/Docker-Hunterpedia/serverdock/internal/store"
)

// This file registers the service actions of plan 0004 step 7: projects and
// services (list, get, create, update, set_source, set_public_port, retry_port)
// and system.exposure. No deploy is queued here: services.create refuses
// deploy:true until step 8a, and nothing in this file enqueues a job.
//
// Token powers (plan 0004, QM3a-4 narrowed): the actions that choose WHAT runs
// or WHERE it is exposed (create, set_source, set_public_port, retry_port) need
// admin scope; update needs write and cannot change the repository.

// ExposureTTL is how long system.exposure keeps its answer. The cache is lazy:
// the age is checked on read, and there is no timer.
const ExposureTTL = 30 * time.Second

// exposureErrTTL is how long a failed Docker call is remembered.
const exposureErrTTL = 5 * time.Second

const timeLayout = "2006-01-02T15:04:05.000Z"

// PortLister is what system.exposure asks Docker: *agentops.Client.
type PortLister interface {
	ContainerPorts(ctx context.Context) (agentops.PortsResult, error)
}

// ServiceDeps are what the service actions need.
type ServiceDeps struct {
	DB     *store.Store
	Routes *Routes
	// Boot opens Routes' boot gate when it is still closed (network_ensure, then
	// Sync): the actions call it before applying a route so that a Docker that was
	// down at start does not leave every later change "not applied". Nil in tests.
	Boot func(ctx context.Context) error
	// NoProxy is true under --proxy=false: there is no embedded proxy to serve an
	// app, so setting a public port is refused with a clear message instead of
	// being saved and never applied.
	NoProxy bool
	// EnsureBound bounds the lazy boot an action may run (default 10 s).
	EnsureBound time.Duration
	// Policy carries --allow-loopback-registry (image references are checked with
	// it here, and again at deploy time).
	Policy policy.Options
	// ReservedPorts are ServerDock's own ports (--agent-listen, --listen,
	// --https-listen): the same list the agent's proxy_apply uses.
	ReservedPorts []int
	// Exposure lists the containers that publish host ports (system.exposure).
	Exposure PortLister
	// Jobs queues deploys (services.deploy, services.create with deploy:true) and
	// cancels them (deploys.cancel): *jobs.Scheduler. Nil means deploys are not
	// available: those actions answer 503.
	Jobs DeployQueue
	// PanelAddrs returns the addresses the panel is reachable on, for the
	// exposure check. It is called only when the cache is refreshed.
	PanelAddrs func() []netip.Addr
	Now        func() time.Time
	Log        *slog.Logger
}

type svcDeps struct {
	ServiceDeps
	exp exposureCache
}

type exposureCache struct {
	mu  sync.Mutex // guards at, val and ok; never held across a Docker call
	at  time.Time
	val ExposureOut
	ok  bool
	// errAt is when Docker last failed (valid with errOK): the failure is
	// remembered for exposureErrTTL, so waiters behind a hung Docker do not each
	// make a long call of their own. Age is checked on read; no timer.
	errAt time.Time
	errOK bool
	// sem lets one caller at a time ask Docker; the others wait on it in a select
	// against their own context, so a hung Docker call cannot make them pile up.
	sem chan struct{}
}

// ---- types ----

// ProjectOut is one entry of projects.list.
type ProjectOut struct {
	Name      string `json:"name"`
	Services  int    `json:"services"`
	CreatedAt string `json:"created_at"`
}

// ProjectsOut is the output of projects.list.
type ProjectsOut struct {
	Projects []ProjectOut `json:"projects"`
}

// HealthIn is the health check of a service, in a request. Every field is
// optional: create fills in defaults, update keeps what it is not given.
type HealthIn struct {
	Kind     string  `json:"kind,omitempty" doc:"http, tcp or running (default: tcp with a container port, running without)"`
	Path     *string `json:"path,omitempty" doc:"http only: starts with /, at most 256 bytes"`
	TimeoutS *int    `json:"timeout_s,omitempty" doc:"5 to 600, default 60"`
	Passes   *int    `json:"passes,omitempty" doc:"consecutive passes, 1 to 10, default 3"`
}

// HealthOut is a service's health check.
type HealthOut struct {
	Kind     string `json:"kind"`
	Path     string `json:"path,omitempty"`
	TimeoutS int    `json:"timeout_s"`
	Passes   int    `json:"passes"`
}

// Warning is something the caller should read though nothing failed.
type Warning struct {
	Code    string `json:"code"`
	Message string `json:"message"`
}

// WarnSharesPanelAddress is the SEC6 warning code (plan 0004, "SEC6").
const WarnSharesPanelAddress = "shares_panel_address"

// ServiceOut is a service as services.get and services.list show it.
type ServiceOut struct {
	Service       string    `json:"service" doc:"<project>/<service>: the name every service action takes"`
	Project       string    `json:"project"`
	Name          string    `json:"name"`
	ImageRef      string    `json:"image_ref" doc:"the image the next deploy uses"`
	ContainerPort *int      `json:"container_port" doc:"the port the app listens on; null for a worker"`
	PublicPort    *int      `json:"public_port" doc:"the host port the proxy serves; null when not exposed"`
	Health        HealthOut `json:"health"`
	Strategy      string    `json:"strategy" doc:"blue_green or stop_first"`
	DrainS        int       `json:"drain_s"`
	StopGraceS    int       `json:"stop_grace_s"`
	// State is what the control plane sees now: not_deployed, running, restarting,
	// stopped, missing, or unknown (the first container list has not finished).
	State          string    `json:"state" doc:"not_deployed, running, restarting, stopped, missing or unknown"`
	ActiveDeploy   *int      `json:"active_deploy" doc:"the number of the live deploy, if any"`
	NeedsAttention string    `json:"needs_attention,omitempty"`
	RoutesStale    bool      `json:"routes_stale" doc:"true while the proxy may not match the desired routes (the last apply failed); Try again"`
	RoutesReady    bool      `json:"routes_ready" doc:"false until the first container list has finished: no app route is served before that"`
	RouteError     string    `json:"route_error,omitempty" doc:"set when the proxy could not open the public port"`
	Warnings       []Warning `json:"warnings"`
	CreatedAt      string    `json:"created_at"`
	UpdatedAt      string    `json:"updated_at"`
}

// ServicesOut is the output of services.list.
type ServicesOut struct {
	Services []ServiceOut `json:"services"`
}

// ServiceChangeOut is the output of the actions that change a service. Applied
// is true when the proxy's routes reflect this change: nothing route-related is
// waiting. A change that involves no route (a service without a public port, a
// new tag, a health check) is applied by definition; "Saved. Deploy to apply."
// is the notice for update and set_source, whose effect comes with the next
// deploy. A public port that was set, changed, cleared or retried is applied
// only once the proxy has the list and could open the port. When Applied is
// false, Notice says why, in words for the user, and the change itself was saved.
type ServiceChangeOut struct {
	Service ServiceOut `json:"service"`
	Applied bool       `json:"applied" doc:"the proxy's routes reflect this change (nothing route-related is waiting)"`
	Notice  string     `json:"notice,omitempty"`
	// Deploy is set when services.create was asked to deploy (deploy:true): the
	// first deploy, queued.
	Deploy *DeployOut `json:"deploy,omitempty"`
}

// ExposedPortOut is one host port a container publishes.
type ExposedPortOut struct {
	HostIP        string `json:"host_ip"`
	HostPort      int    `json:"host_port"`
	ContainerPort int    `json:"container_port"`
	Proto         string `json:"proto"`
}

// ExposedContainerOut is a container ServerDock did not create that publishes
// host ports on the panel's address.
type ExposedContainerOut struct {
	Name           string           `json:"name"`
	Ports          []ExposedPortOut `json:"ports"`
	PortsTruncated bool             `json:"ports_truncated,omitempty"`
}

// ExposureOut is the output of system.exposure.
type ExposureOut struct {
	Containers []ExposedContainerOut `json:"containers"`
	Truncated  bool                  `json:"truncated,omitempty"`
	// CheckedAt is when Docker was asked (the answer is kept for 30 seconds).
	CheckedAt time.Time `json:"checked_at"`
}

// ---- inputs ----

// ServiceRefIn names a service as "<project>/<service>".
type ServiceRefIn struct {
	Service string `json:"service" doc:"<project>/<service>"`
}

// Validate implements actions.Validator.
func (in ServiceRefIn) Validate() error { _, _, err := splitRef(in.Service); return err }

// splitRef parses "<project>/<service>".
func splitRef(s string) (project, service string, err error) {
	p, sv, ok := strings.Cut(s, "/")
	if !ok || strings.Contains(sv, "/") {
		return "", "", errors.New("service must look like <project>/<service>")
	}
	if err := policy.ValidateName(p); err != nil {
		return "", "", fmt.Errorf("project: %w", err)
	}
	if err := policy.ValidateName(sv); err != nil {
		return "", "", fmt.Errorf("service: %w", err)
	}
	return p, sv, nil
}

// ServiceCreateIn is the input of services.create.
type ServiceCreateIn struct {
	Project       string    `json:"project"`
	Name          string    `json:"name"`
	ImageRef      string    `json:"image_ref"`
	ContainerPort *int      `json:"container_port,omitempty"`
	PublicPort    *int      `json:"public_port,omitempty" doc:"needs a container port; exposes the app on this host port over plain HTTP"`
	Health        *HealthIn `json:"health,omitempty"`
	Strategy      string    `json:"strategy,omitempty" doc:"blue_green (default) or stop_first"`
	DrainS        *int      `json:"drain_s,omitempty" doc:"10 to 300, default 10"`
	StopGraceS    *int      `json:"stop_grace_s,omitempty" doc:"0 to 300, default 10"`
	Deploy        bool      `json:"deploy,omitempty" doc:"true queues the service's first deploy right after it is created"`
}

// Validate implements actions.Validator. It checks everything that does not
// need the install's options or the database.
func (in ServiceCreateIn) Validate() error {
	if err := policy.ValidateName(in.Project); err != nil {
		return fmt.Errorf("project: %w", err)
	}
	if err := policy.ValidateName(in.Name); err != nil {
		return fmt.Errorf("name: %w", err)
	}
	if _, err := policy.ParseRef(in.ImageRef); err != nil {
		return fmt.Errorf("image_ref: %w", err)
	}
	_, err := in.settings()
	return err
}

func (in ServiceCreateIn) target() string { return in.Project + "/" + in.Name }

// svcSettings are the resolved, validated settings of a service.
type svcSettings struct {
	containerPort *int
	health        HealthOut
	strategy      string
	drainS        int
	stopGraceS    int
}

// settings applies the defaults and validates the result.
func (in ServiceCreateIn) settings() (svcSettings, error) {
	s := svcSettings{containerPort: in.ContainerPort, strategy: "blue_green", drainS: 10, stopGraceS: 10}
	if in.ContainerPort != nil {
		if err := policy.ValidateContainerPort(*in.ContainerPort); err != nil {
			return s, err
		}
	}
	if in.PublicPort != nil && in.ContainerPort == nil {
		return s, errors.New("public_port needs a container_port: say which port the app listens on")
	}
	base := HealthOut{Kind: "running", TimeoutS: 60, Passes: 3}
	if in.ContainerPort != nil {
		base.Kind = "tcp"
	}
	h, err := mergeHealth(base, in.Health, in.ContainerPort != nil)
	if err != nil {
		return s, err
	}
	s.health = h
	if in.Strategy != "" {
		s.strategy = in.Strategy
	}
	if err := checkStrategy(s.strategy); err != nil {
		return s, err
	}
	if in.DrainS != nil {
		s.drainS = *in.DrainS
	}
	if in.StopGraceS != nil {
		s.stopGraceS = *in.StopGraceS
	}
	if err := policy.ValidateDrainS(s.drainS); err != nil {
		return s, err
	}
	return s, policy.ValidateStopGraceS(s.stopGraceS)
}

func checkStrategy(s string) error {
	if s != "blue_green" && s != "stop_first" {
		return errors.New("strategy must be blue_green or stop_first")
	}
	return nil
}

// mergeHealth overlays in on base and validates the result. hasPort says
// whether the service has a container port (http and tcp checks need one).
func mergeHealth(base HealthOut, in *HealthIn, hasPort bool) (HealthOut, error) {
	h := base
	if in != nil {
		if in.Kind != "" && in.Kind != h.Kind {
			h.Kind = in.Kind
			if in.Path == nil {
				h.Path = "" // a path belongs to the http kind only
			}
		}
		if in.Path != nil {
			h.Path = *in.Path
		}
		if in.TimeoutS != nil {
			h.TimeoutS = *in.TimeoutS
		}
		if in.Passes != nil {
			h.Passes = *in.Passes
		}
	}
	switch h.Kind {
	case "http":
		if err := policy.ValidateHealthPath(h.Path); err != nil {
			return h, fmt.Errorf("health.path: %w", err)
		}
		if !hasPort {
			return h, errors.New("an http health check needs a container port")
		}
	case "tcp":
		if !hasPort {
			return h, errors.New("a tcp health check needs a container port")
		}
		if h.Path != "" {
			return h, errors.New("health.path is for the http kind only")
		}
	case "running":
		if h.Path != "" {
			return h, errors.New("health.path is for the http kind only")
		}
	default:
		return h, errors.New("health.kind must be http, tcp or running")
	}
	if err := policy.ValidateHealthTimeoutS(h.TimeoutS); err != nil {
		return h, err
	}
	return h, policy.ValidateHealthPasses(h.Passes)
}

// ServiceUpdateIn is the input of services.update: only the fields given change.
type ServiceUpdateIn struct {
	Service       string    `json:"service" doc:"<project>/<service>"`
	ImageRef      *string   `json:"image_ref,omitempty" doc:"another tag or digest of the same repository (changing the repository needs services.set_source)"`
	ContainerPort *int      `json:"container_port,omitempty"`
	Health        *HealthIn `json:"health,omitempty"`
	Strategy      *string   `json:"strategy,omitempty"`
	DrainS        *int      `json:"drain_s,omitempty"`
	StopGraceS    *int      `json:"stop_grace_s,omitempty"`
}

// Validate implements actions.Validator.
func (in ServiceUpdateIn) Validate() error {
	if _, _, err := splitRef(in.Service); err != nil {
		return err
	}
	if in.ImageRef == nil && in.ContainerPort == nil && in.Health == nil && in.Strategy == nil && in.DrainS == nil && in.StopGraceS == nil {
		return errors.New("nothing to change: give at least one field")
	}
	if in.ImageRef != nil {
		if _, err := policy.ParseRef(*in.ImageRef); err != nil {
			return fmt.Errorf("image_ref: %w", err)
		}
	}
	if in.ContainerPort != nil {
		if err := policy.ValidateContainerPort(*in.ContainerPort); err != nil {
			return err
		}
	}
	if in.Strategy != nil {
		if err := checkStrategy(*in.Strategy); err != nil {
			return err
		}
	}
	if in.DrainS != nil {
		if err := policy.ValidateDrainS(*in.DrainS); err != nil {
			return err
		}
	}
	if in.StopGraceS != nil {
		if err := policy.ValidateStopGraceS(*in.StopGraceS); err != nil {
			return err
		}
	}
	if h := in.Health; h != nil {
		if h.Kind != "" && h.Kind != "http" && h.Kind != "tcp" && h.Kind != "running" {
			return errors.New("health.kind must be http, tcp or running")
		}
		if h.Path != nil && *h.Path != "" {
			if err := policy.ValidateHealthPath(*h.Path); err != nil {
				return fmt.Errorf("health.path: %w", err)
			}
		}
		if h.TimeoutS != nil {
			if err := policy.ValidateHealthTimeoutS(*h.TimeoutS); err != nil {
				return err
			}
		}
		if h.Passes != nil {
			if err := policy.ValidateHealthPasses(*h.Passes); err != nil {
				return err
			}
		}
	}
	return nil
}

// ServiceSourceIn is the input of services.set_source.
type ServiceSourceIn struct {
	Service  string `json:"service" doc:"<project>/<service>"`
	ImageRef string `json:"image_ref" doc:"the new image, any repository or registry"`
}

// Validate implements actions.Validator.
func (in ServiceSourceIn) Validate() error {
	if _, _, err := splitRef(in.Service); err != nil {
		return err
	}
	if _, err := policy.ParseRef(in.ImageRef); err != nil {
		return fmt.Errorf("image_ref: %w", err)
	}
	return nil
}

// PublicPortIn is the input of services.set_public_port: a port to set or
// change, or clear:true to stop exposing the service. Exactly one of the two.
type PublicPortIn struct {
	Service    string `json:"service" doc:"<project>/<service>"`
	PublicPort *int   `json:"public_port,omitempty" doc:"1024 to 65535, never 80, 443 or one of ServerDock's own ports"`
	Clear      bool   `json:"clear,omitempty" doc:"true stops exposing the service"`
}

// Validate implements actions.Validator.
func (in PublicPortIn) Validate() error {
	if _, _, err := splitRef(in.Service); err != nil {
		return err
	}
	if (in.PublicPort == nil) == !in.Clear {
		return errors.New("give either public_port or clear:true")
	}
	return nil
}

// ---- registration ----

// RegisterServiceActions registers the step 7 actions. It panics on a missing
// database or Routes (a wiring bug fails at start).
func RegisterServiceActions(r *actions.Registry, deps ServiceDeps) {
	if deps.DB == nil || deps.Routes == nil {
		panic("deploy: RegisterServiceActions needs a database and Routes")
	}
	if deps.Now == nil {
		deps.Now = time.Now
	}
	if deps.Log == nil {
		deps.Log = slog.New(slog.DiscardHandler)
	}
	d := &svcDeps{ServiceDeps: deps, exp: exposureCache{sem: make(chan struct{}, 1)}}
	refTarget := func(in ServiceRefIn) string { return in.Service }

	actions.Register(r, actions.Def[actions.None, ProjectsOut]{
		Name: "projects.list", Summary: "List the projects and how many services each has.",
		Risk: actions.RiskRead, Scope: actions.ScopeRead,
		Execute: func(ctx context.Context, _ actions.Call, _ actions.None) (ProjectsOut, error) { return d.projects(ctx) },
	})
	actions.Register(r, actions.Def[actions.None, ServicesOut]{
		Name: "services.list", Summary: "List the services with their state, live deploy, route problems and warnings.",
		Risk: actions.RiskRead, Scope: actions.ScopeRead,
		Execute: func(ctx context.Context, _ actions.Call, _ actions.None) (ServicesOut, error) { return d.list(ctx) },
	})
	actions.Register(r, actions.Def[ServiceRefIn, ServiceOut]{
		Name: "services.get", Summary: "One service: settings, state, live deploy, route problems and warnings.",
		Risk: actions.RiskRead, Scope: actions.ScopeRead,
		Execute: func(ctx context.Context, _ actions.Call, in ServiceRefIn) (ServiceOut, error) {
			row, err := d.load(ctx, d.DB.R, in.Service)
			if err != nil {
				return ServiceOut{}, err
			}
			return d.out(row), nil
		},
	})
	actions.Register(r, actions.Def[ServiceCreateIn, ServiceChangeOut]{
		Name: "services.create", Summary: "Create a service from an image (creates the project if it is new). With deploy:true it also queues the first deploy.",
		Risk: actions.RiskMutate, Scope: actions.ScopeAdmin, Target: ServiceCreateIn.target,
		Execute: func(ctx context.Context, c actions.Call, in ServiceCreateIn) (ServiceChangeOut, error) {
			return d.create(ctx, c, in)
		},
	})
	actions.Register(r, actions.Def[ServiceUpdateIn, ServiceChangeOut]{
		Name: "services.update", Summary: "Change a service's health check, strategy, timings, container port or image tag (the same repository only). Takes effect at the next deploy.",
		Risk: actions.RiskMutate, Scope: actions.ScopeWrite, Target: func(in ServiceUpdateIn) string { return in.Service },
		Execute: func(ctx context.Context, c actions.Call, in ServiceUpdateIn) (ServiceChangeOut, error) {
			return d.update(ctx, c, in)
		},
	})
	actions.Register(r, actions.Def[ServiceSourceIn, ServiceChangeOut]{
		Name: "services.set_source", Summary: "Change which image repository or registry a service deploys. Takes effect at the next deploy.",
		Risk: actions.RiskMutate, Scope: actions.ScopeAdmin, Target: func(in ServiceSourceIn) string { return in.Service },
		Execute: func(ctx context.Context, _ actions.Call, in ServiceSourceIn) (ServiceChangeOut, error) {
			return d.setSource(ctx, in)
		},
	})
	actions.Register(r, actions.Def[PublicPortIn, ServiceChangeOut]{
		Name: "services.set_public_port", Summary: "Set, change or clear the host port the proxy serves a service on. Applies now and exposes the app on this server's address over plain HTTP.",
		Risk: actions.RiskMutate, Scope: actions.ScopeAdmin, Target: func(in PublicPortIn) string { return in.Service },
		Execute: func(ctx context.Context, _ actions.Call, in PublicPortIn) (ServiceChangeOut, error) {
			return d.setPublicPort(ctx, in)
		},
	})
	actions.Register(r, actions.Def[ServiceRefIn, ServiceChangeOut]{
		Name: "services.retry_port", Summary: "Try again: send the route list to the proxy again (after another program let go of a port, or a change was not applied). Same power as services.set_public_port.",
		Risk: actions.RiskMutate, Scope: actions.ScopeAdmin, Target: refTarget,
		Execute: func(ctx context.Context, _ actions.Call, in ServiceRefIn) (ServiceChangeOut, error) {
			return d.retryPort(ctx, in)
		},
	})
	registerDeployActions(r, d)
	actions.Register(r, actions.Def[actions.None, ExposureOut]{
		Name: "system.exposure", Summary: "Containers ServerDock did not create that publish host ports on the panel's address (SEC6). Kept for 30 seconds.",
		Risk: actions.RiskRead, Scope: actions.ScopeRead,
		Execute: func(ctx context.Context, _ actions.Call, _ actions.None) (ExposureOut, error) { return d.exposure(ctx) },
	})
}

// ---- reading ----

const serviceCols = `s.id, p.name, s.name, s.image_ref, s.container_port, s.public_port, s.health_kind, s.health_path,
	s.health_timeout_s, s.health_passes, s.strategy, s.drain_s, s.stop_grace_s,
	s.active_deploy_id, d.number, d.ip, s.needs_attention, s.created_at, s.updated_at`

const serviceFrom = ` FROM services s JOIN projects p ON p.id = s.project_id LEFT JOIN deploys d ON d.id = s.active_deploy_id`

// svcRow is a service row.
type svcRow struct {
	id                         int64
	project, name, imageRef    string
	containerPort, publicPort  sql.NullInt64
	healthKind                 string
	healthPath                 sql.NullString
	timeoutS, passes           int
	strategy                   string
	drainS, graceS             int
	activeDeploy, activeNumber sql.NullInt64
	activeIP, attention        sql.NullString
	created, updated           string
}

type rowScanner interface{ Scan(dest ...any) error }

func scanService(s rowScanner) (svcRow, error) {
	var r svcRow
	err := s.Scan(&r.id, &r.project, &r.name, &r.imageRef, &r.containerPort, &r.publicPort, &r.healthKind, &r.healthPath,
		&r.timeoutS, &r.passes, &r.strategy, &r.drainS, &r.graceS, &r.activeDeploy, &r.activeNumber, &r.activeIP, &r.attention, &r.created, &r.updated)
	return r, err
}

type rowQuerier interface {
	QueryRowContext(ctx context.Context, query string, args ...any) *sql.Row
}

// load reads one service by "<project>/<service>".
func (d *svcDeps) load(ctx context.Context, q rowQuerier, ref string) (svcRow, error) {
	project, name, err := splitRef(ref)
	if err != nil {
		return svcRow{}, actions.InvalidInput("%v", err)
	}
	r, err := scanService(q.QueryRowContext(ctx, `SELECT `+serviceCols+serviceFrom+` WHERE p.name = ? AND s.name = ?`, project, name))
	if errors.Is(err, sql.ErrNoRows) {
		return svcRow{}, actions.NotFound("no service %s/%s", project, name)
	}
	return r, err
}

func intPtr(n sql.NullInt64) *int {
	if !n.Valid {
		return nil
	}
	v := int(n.Int64)
	return &v
}

// out turns a row into the API shape, with the state and the warnings.
func (d *svcDeps) out(r svcRow) ServiceOut {
	o := ServiceOut{
		Service: r.project + "/" + r.name, Project: r.project, Name: r.name, ImageRef: r.imageRef,
		ContainerPort: intPtr(r.containerPort), PublicPort: intPtr(r.publicPort),
		Health:   HealthOut{Kind: r.healthKind, Path: r.healthPath.String, TimeoutS: r.timeoutS, Passes: r.passes},
		Strategy: r.strategy, DrainS: r.drainS, StopGraceS: r.graceS,
		ActiveDeploy: intPtr(r.activeNumber), NeedsAttention: r.attention.String,
		RoutesReady: d.Routes.Ready(), RoutesStale: d.Routes.Stale(), Warnings: []Warning{}, CreatedAt: r.created, UpdatedAt: r.updated,
	}
	o.State = d.state(r)
	if r.publicPort.Valid {
		port := int(r.publicPort.Int64)
		o.RouteError = d.Routes.BindStatus(port)
		if o.RouteError == "" && d.Routes.Stale() {
			o.RouteError = StaleMessage
		}
		o.Warnings = append(o.Warnings, Warning{Code: WarnSharesPanelAddress, Message: SharesPanelAddressMessage(port)})
	}
	return o
}

// SharesPanelAddressMessage is the SEC6 wording (plan 0004). The action layer
// knows no host name (the panel may be reached by IP, name or tunnel), so the
// message says "this server's address" and the SPA adds the one it was opened on.
func SharesPanelAddressMessage(port int) string {
	return fmt.Sprintf("This app is reachable on the same address as the panel, on port %d. "+
		"Browsers share cookies by host name, not by port. ServerDock serves this app over plain HTTP, "+
		"so the panel's secure cookies are never sent to it. Don't run any other HTTPS service on this address. "+
		"Once domains arrive, give the panel and your apps their own names.", port)
}

// state is the service's state as the control plane sees it: from the state map
// (a container of this service and its live deploy at the deploy's address).
func (d *svcDeps) state(r svcRow) string {
	if !r.activeDeploy.Valid {
		return "not_deployed"
	}
	if !d.Routes.Ready() {
		return "unknown"
	}
	seen := false
	for _, c := range d.Routes.State().snapshot() {
		if c.Service != r.id || c.Deploy != r.activeDeploy.Int64 {
			continue
		}
		seen = true
		if c.IP != "" && c.IP == r.activeIP.String {
			switch c.State {
			case StateRunning:
				return "running"
			case StateRestarting:
				return "restarting"
			}
		}
	}
	if seen {
		return "stopped"
	}
	return "missing"
}

func (d *svcDeps) projects(ctx context.Context) (ProjectsOut, error) {
	rs, err := d.DB.R.QueryContext(ctx, `SELECT p.name, p.created_at, (SELECT COUNT(*) FROM services s WHERE s.project_id = p.id)
		FROM projects p ORDER BY p.name`)
	if err != nil {
		return ProjectsOut{}, err
	}
	defer rs.Close()
	out := ProjectsOut{Projects: []ProjectOut{}}
	for rs.Next() {
		var p ProjectOut
		if err := rs.Scan(&p.Name, &p.CreatedAt, &p.Services); err != nil {
			return ProjectsOut{}, err
		}
		out.Projects = append(out.Projects, p)
	}
	return out, rs.Err()
}

func (d *svcDeps) list(ctx context.Context) (ServicesOut, error) {
	rs, err := d.DB.R.QueryContext(ctx, `SELECT `+serviceCols+serviceFrom+` ORDER BY p.name, s.name`)
	if err != nil {
		return ServicesOut{}, err
	}
	defer rs.Close()
	out := ServicesOut{Services: []ServiceOut{}}
	for rs.Next() {
		r, err := scanService(rs)
		if err != nil {
			return ServicesOut{}, err
		}
		out.Services = append(out.Services, d.out(r))
	}
	return out, rs.Err()
}

// ---- writing ----

func (d *svcDeps) stamp() string { return d.Now().UTC().Format(timeLayout) }

// checkImage validates a reference with the install's options (the loopback
// registry switch). The text it returns is policy's, which echoes only a cleaned,
// shortened part of the input.
func (d *svcDeps) checkImage(ref string) (string, error) {
	pull, err := policy.ValidateImageRef(ref, d.Policy)
	if err != nil {
		return "", actions.InvalidInput("image_ref: %v", err)
	}
	return pull, nil
}

// checkPort validates a public port and that no other service has it.
func (d *svcDeps) checkPort(ctx context.Context, q rowQuerier, port int, self int64) error {
	if err := policy.ValidatePublicPort(port, d.ReservedPorts...); err != nil {
		return actions.InvalidInput("%v", err)
	}
	var owner string
	err := q.QueryRowContext(ctx, `SELECT p.name || '/' || s.name FROM services s JOIN projects p ON p.id = s.project_id
		WHERE s.public_port = ? AND s.id <> ?`, port, self).Scan(&owner)
	switch {
	case err == nil:
		return actions.Conflict("port %d is already used by %s", port, owner)
	case errors.Is(err, sql.ErrNoRows):
		return nil
	}
	return err
}

func isUnique(err error) bool {
	return err != nil && strings.Contains(err.Error(), "UNIQUE constraint failed")
}

func (d *svcDeps) create(ctx context.Context, c actions.Call, in ServiceCreateIn) (ServiceChangeOut, error) {
	pull, err := d.checkImage(in.ImageRef)
	if err != nil {
		return ServiceChangeOut{}, err
	}
	if in.Deploy && d.Jobs == nil {
		return ServiceChangeOut{}, actions.Unavailable("deploys are not available on this server")
	}
	set, err := in.settings()
	if err != nil {
		return ServiceChangeOut{}, actions.InvalidInput("%v", err)
	}
	if in.PublicPort != nil {
		if err := d.checkPort(ctx, d.DB.R, *in.PublicPort, 0); err != nil {
			return ServiceChangeOut{}, err
		}
	}
	var id int64
	insert := func() error {
		return d.DB.WriteTx(ctx, func(tx *sql.Tx) error {
			var n int
			if err := tx.QueryRowContext(ctx, `SELECT COUNT(*) FROM services`).Scan(&n); err != nil {
				return err
			}
			if err := policy.CheckServiceCount(n); err != nil {
				return actions.Conflict("%v", err)
			}
			now := d.stamp()
			if _, err := tx.ExecContext(ctx, `INSERT OR IGNORE INTO projects(name, created_at) VALUES (?, ?)`, in.Project, now); err != nil {
				return err
			}
			var pid int64
			if err := tx.QueryRowContext(ctx, `SELECT id FROM projects WHERE name = ?`, in.Project).Scan(&pid); err != nil {
				return err
			}
			var dup int
			if err := tx.QueryRowContext(ctx, `SELECT COUNT(*) FROM services WHERE project_id = ? AND name = ?`, pid, in.Name).Scan(&dup); err != nil {
				return err
			}
			if dup > 0 {
				return actions.Conflict("service %s/%s already exists", in.Project, in.Name)
			}
			var path any
			if set.health.Path != "" {
				path = set.health.Path
			}
			res, err := tx.ExecContext(ctx, `INSERT INTO services(project_id, name, image_ref, container_port, public_port, health_kind, health_path,
				health_timeout_s, health_passes, strategy, drain_s, stop_grace_s, created_at, updated_at)
				VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`,
				pid, in.Name, pull, in.ContainerPort, in.PublicPort, set.health.Kind, path,
				set.health.TimeoutS, set.health.Passes, set.strategy, set.drainS, set.stopGraceS, now, now)
			if err != nil {
				if isUnique(err) {
					return actions.Conflict("service %s/%s or its public port is already taken", in.Project, in.Name)
				}
				return err
			}
			id, err = res.LastInsertId()
			return err
		})
	}
	var routeErr error
	if in.PublicPort == nil {
		// No route is involved: the service is saved and nothing waits to be applied,
		// whether or not the routes are ready.
		if err := insert(); err != nil {
			return ServiceChangeOut{}, err
		}
	} else {
		if err := d.needProxy(); err != nil {
			return ServiceChangeOut{}, err
		}
		d.ensureRoutes(ctx)
		routeErr = d.Routes.ApplyRoutes(ctx, insert)
		var me *MutateError
		if errors.As(routeErr, &me) {
			return ServiceChangeOut{}, me.Err // refused: nothing was built or sent
		}
	}
	row, err := d.loadID(ctx, id)
	if err != nil {
		return ServiceChangeOut{}, err
	}
	applied, notice := d.outcome(in.PublicPort != nil, row.publicPort, routeErr)
	out := ServiceChangeOut{Service: d.out(row), Applied: applied, Notice: notice}
	if in.Deploy {
		q, err := d.queueDeploy(ctx, c, row, row.imageRef)
		if err != nil {
			// The service exists; only the deploy was not queued. Say both.
			var de *actions.DeniedError
			if errors.As(err, &de) && de.Reason == SessionGoneMessage {
				return ServiceChangeOut{}, &actions.DeniedError{Reason: SessionGoneMessage + " (the service was created but not deployed)"}
			}
			return ServiceChangeOut{}, err
		}
		out.Deploy = &q
	}
	return out, nil
}

// needProxy refuses a public port when the embedded proxy is off.
func (d *svcDeps) needProxy() error {
	if d.NoProxy {
		return actions.Conflict("the embedded proxy is off on this server (--proxy=false): public ports are not available")
	}
	return nil
}

func (d *svcDeps) loadID(ctx context.Context, id int64) (svcRow, error) {
	r, err := scanService(d.DB.R.QueryRowContext(ctx, `SELECT `+serviceCols+serviceFrom+` WHERE s.id = ?`, id))
	if errors.Is(err, sql.ErrNoRows) {
		return svcRow{}, actions.NotFound("the service no longer exists")
	}
	return r, err
}

func (d *svcDeps) update(ctx context.Context, c actions.Call, in ServiceUpdateIn) (ServiceChangeOut, error) {
	var pull string
	if in.ImageRef != nil {
		p, err := d.checkImage(*in.ImageRef)
		if err != nil {
			return ServiceChangeOut{}, err
		}
		pull = p
	}
	var id int64
	err := d.DB.WriteTx(ctx, func(tx *sql.Tx) error {
		cur, err := d.load(ctx, tx, in.Service)
		if err != nil {
			return err
		}
		id = cur.id
		image := cur.imageRef
		if in.ImageRef != nil {
			// The write-token rule (QM3a-4): a tag or digest of the repository the
			// service already deploys, never another repository or registry.
			if !policy.SameRepository(cur.imageRef, pull) {
				return actions.InvalidInput("changing the image repository needs services.set_source")
			}
			image = pull
		}
		cport := cur.containerPort
		if in.ContainerPort != nil {
			// The container port decides where an exposed service's traffic goes:
			// for a service that has a public port, changing it is a "where is it
			// exposed" decision, so it needs admin scope (a write token may not).
			if cur.publicPort.Valid && (!cur.containerPort.Valid || cur.containerPort.Int64 != int64(*in.ContainerPort)) && c.Actor.Scope() < actions.ScopeAdmin {
				return &actions.DeniedError{Reason: "changing the container port of a service that has a public port needs an admin token or a signed-in session"}
			}
			cport = sql.NullInt64{Int64: int64(*in.ContainerPort), Valid: true}
		}
		base := HealthOut{Kind: cur.healthKind, Path: cur.healthPath.String, TimeoutS: cur.timeoutS, Passes: cur.passes}
		h, err := mergeHealth(base, in.Health, cport.Valid)
		if err != nil {
			return actions.InvalidInput("%v", err)
		}
		strategy, drain, grace := cur.strategy, cur.drainS, cur.graceS
		if in.Strategy != nil {
			strategy = *in.Strategy
		}
		if in.DrainS != nil {
			drain = *in.DrainS
		}
		if in.StopGraceS != nil {
			grace = *in.StopGraceS
		}
		var path any
		if h.Path != "" {
			path = h.Path
		}
		_, err = tx.ExecContext(ctx, `UPDATE services SET image_ref = ?, container_port = ?, health_kind = ?, health_path = ?,
			health_timeout_s = ?, health_passes = ?, strategy = ?, drain_s = ?, stop_grace_s = ?, updated_at = ? WHERE id = ?`,
			image, cport, h.Kind, path, h.TimeoutS, h.Passes, strategy, drain, grace, d.stamp(), cur.id)
		return err
	})
	if err != nil {
		return ServiceChangeOut{}, err
	}
	row, err := d.loadID(ctx, id)
	if err != nil {
		return ServiceChangeOut{}, err
	}
	return ServiceChangeOut{Service: d.out(row), Applied: true, Notice: "Saved. Deploy to apply."}, nil
}

func (d *svcDeps) setSource(ctx context.Context, in ServiceSourceIn) (ServiceChangeOut, error) {
	pull, err := d.checkImage(in.ImageRef)
	if err != nil {
		return ServiceChangeOut{}, err
	}
	var id int64
	err = d.DB.WriteTx(ctx, func(tx *sql.Tx) error {
		cur, err := d.load(ctx, tx, in.Service)
		if err != nil {
			return err
		}
		id = cur.id
		_, err = tx.ExecContext(ctx, `UPDATE services SET image_ref = ?, updated_at = ? WHERE id = ?`, pull, d.stamp(), cur.id)
		return err
	})
	if err != nil {
		return ServiceChangeOut{}, err
	}
	row, err := d.loadID(ctx, id)
	if err != nil {
		return ServiceChangeOut{}, err
	}
	return ServiceChangeOut{Service: d.out(row), Applied: true, Notice: "Saved. Deploy to apply."}, nil
}

// ensureBound bounds the lazy boot an action may run: a hung Docker must not
// hold the request for long.
const ensureBound = 10 * time.Second

// ensureRoutes opens the boot gate if it is closed (one bounded, single-flight
// Booter.Run). A failure is logged by the booter; the caller then finds the
// routes not ready and says so.
func (d *svcDeps) ensureRoutes(ctx context.Context) {
	if d.Boot == nil || d.Routes.Ready() {
		return
	}
	bound := d.EnsureBound
	if bound <= 0 {
		bound = ensureBound
	}
	ctx, cancel := context.WithTimeout(ctx, bound)
	defer cancel()
	_ = d.Boot(ctx)
}

// outcome says honestly whether the change is applied, in the sense of
// ServiceChangeOut.Applied. A change with no route involvement is applied; one
// that involved a route is not while the routes are not ready, the proxy failed,
// or the port could not be opened. The change itself is saved in every case.
func (d *svcDeps) outcome(routeInvolved bool, port sql.NullInt64, err error) (applied bool, notice string) {
	if !routeInvolved {
		return true, ""
	}
	switch {
	case errors.Is(err, ErrRoutesNotReady) || (err == nil && !d.Routes.Ready()):
		return false, "Saved, but not applied yet: ServerDock has not finished listing the containers on this server (is Docker running?). Use Try again once it is."
	case err != nil:
		d.Log.Warn("applying the app routes failed", "err", err.Error())
		return false, "Saved, but the proxy could not apply the change; the previous routes keep running. Use Try again."
	case port.Valid:
		if msg := d.Routes.BindStatus(int(port.Int64)); msg != "" {
			return false, "Saved, but " + msg + ". Use Try again once it is free."
		}
	}
	return true, ""
}

func (d *svcDeps) setPublicPort(ctx context.Context, in PublicPortIn) (ServiceChangeOut, error) {
	cur, err := d.load(ctx, d.DB.R, in.Service)
	if err != nil {
		return ServiceChangeOut{}, err
	}
	var want sql.NullInt64
	if !in.Clear {
		if err := d.needProxy(); err != nil {
			return ServiceChangeOut{}, err
		}
		if !cur.containerPort.Valid {
			return ServiceChangeOut{}, actions.InvalidInput("the service has no container port: set one with services.update first")
		}
		if err := d.checkPort(ctx, d.DB.R, *in.PublicPort, cur.id); err != nil {
			return ServiceChangeOut{}, err
		}
		want = sql.NullInt64{Int64: int64(*in.PublicPort), Valid: true}
	}
	if !want.Valid && !cur.publicPort.Valid {
		// Clearing a port that is not set: nothing to write and no route involved.
		return ServiceChangeOut{Service: d.out(cur), Applied: true}, nil
	}
	if d.NoProxy {
		// Clearing a port with no proxy: only the setting changes, no route exists.
		if err := d.writePort(ctx, cur.id, want); err != nil {
			return ServiceChangeOut{}, err
		}
		row, err := d.loadID(ctx, cur.id)
		return ServiceChangeOut{Service: d.out(row), Applied: true}, err
	}
	d.ensureRoutes(ctx)
	var routeErr error
	if want == cur.publicPort {
		// The same port again: nothing to write, but a port that could not be opened
		// (or a change the proxy never got) is sent again.
		routeErr = d.Routes.Refresh(ctx)
	} else {
		routeErr = d.Routes.ApplyRoutes(ctx, func() error { return d.writePort(ctx, cur.id, want) })
		var me *MutateError
		if errors.As(routeErr, &me) {
			return ServiceChangeOut{}, me.Err // refused: nothing was built or sent
		}
	}
	row, err := d.loadID(ctx, cur.id)
	if err != nil {
		return ServiceChangeOut{}, err
	}
	applied, notice := d.outcome(true, row.publicPort, routeErr)
	return ServiceChangeOut{Service: d.out(row), Applied: applied, Notice: notice}, nil
}

// writePort is the DB write that ApplyRoutes runs as its mutate function: it
// opens and commits its own transaction (no write transaction is open while the
// route lock is taken) and takes no service or allocation lock.
func (d *svcDeps) writePort(ctx context.Context, id int64, port sql.NullInt64) error {
	err := d.DB.WriteTx(ctx, func(tx *sql.Tx) error {
		_, err := tx.ExecContext(ctx, `UPDATE services SET public_port = ?, updated_at = ? WHERE id = ?`, port, d.stamp(), id)
		return err
	})
	if isUnique(err) {
		return actions.Conflict("that public port is already used by another service")
	}
	return err
}

// retryPort is "Try again": it sends the route list again (Routes.Refresh). The
// resend is global (one list for every service), so the service need not have a
// public port; it only has to exist.
func (d *svcDeps) retryPort(ctx context.Context, in ServiceRefIn) (ServiceChangeOut, error) {
	cur, err := d.load(ctx, d.DB.R, in.Service)
	if err != nil {
		return ServiceChangeOut{}, err
	}
	if err := d.needProxy(); err != nil {
		return ServiceChangeOut{}, err
	}
	d.ensureRoutes(ctx)
	routeErr := d.Routes.Refresh(ctx)
	applied, notice := d.outcome(true, cur.publicPort, routeErr)
	return ServiceChangeOut{Service: d.out(cur), Applied: applied, Notice: notice}, nil
}

// ---- exposure ----

// exposure answers system.exposure from a 30-second cache. The age is checked
// here, on read: there is no timer, and Docker is asked only when someone asks
// and the answer is older than the TTL. A failure is remembered for 5 s
// (unless it was the caller's own context ending): after a failed refresh the
// next 5 s answer 503, not the last good answer. That is deliberate: a stale
// exposure list would be reported as current, and a hung Docker must not make
// every caller make its own long call.
func (d *svcDeps) exposure(ctx context.Context) (ExposureOut, error) {
	if d.Exposure == nil {
		return ExposureOut{}, actions.Unavailable("the exposure check needs the local agent")
	}
	if v, failed, ok := d.cachedExposure(); ok {
		if failed {
			return ExposureOut{}, actions.Unavailable("could not ask Docker which containers publish ports")
		}
		return v, nil
	}
	// One caller asks Docker; the others wait here in a select against their own
	// context (and then find the answer in the cache), so a hung Docker call does
	// not make them pile up behind it.
	select {
	case d.exp.sem <- struct{}{}:
	case <-ctx.Done():
		return ExposureOut{}, actions.Unavailable("could not ask Docker which containers publish ports")
	}
	defer func() { <-d.exp.sem }()
	if v, failed, ok := d.cachedExposure(); ok {
		if failed {
			return ExposureOut{}, actions.Unavailable("could not ask Docker which containers publish ports")
		}
		return v, nil
	}
	res, err := d.Exposure.ContainerPorts(ctx)
	if err != nil {
		d.Log.Warn("exposure check failed", "err", err.Error())
		// Remember the failure for a few seconds (age checked on read, no timer), so
		// callers behind a hung Docker do not each make a long call of their own.
		// Not when the failure is the caller's own context ending: a client that
		// disconnects must not make everyone else get a 503.
		if ctx.Err() == nil {
			d.exp.mu.Lock()
			d.exp.errAt, d.exp.errOK = d.Now(), true
			d.exp.mu.Unlock()
		}
		return ExposureOut{}, actions.Unavailable("could not ask Docker which containers publish ports")
	}
	var panel []netip.Addr
	if d.PanelAddrs != nil {
		panel = d.PanelAddrs()
	}
	out := ExposureOut{Containers: []ExposedContainerOut{}, Truncated: res.Truncated, CheckedAt: d.Now().UTC()}
	for _, c := range res.Containers {
		e := ExposedContainerOut{Name: c.Name, Ports: []ExposedPortOut{}, PortsTruncated: c.PortsTruncated}
		for _, p := range c.Ports {
			if sharesPanelAddress(p.HostIP, panel) {
				e.Ports = append(e.Ports, ExposedPortOut{HostIP: p.HostIP, HostPort: p.HostPort, ContainerPort: p.ContainerPort, Proto: p.Proto})
			}
		}
		if len(e.Ports) > 0 {
			out.Containers = append(out.Containers, e)
		}
	}
	sort.Slice(out.Containers, func(i, j int) bool { return out.Containers[i].Name < out.Containers[j].Name })
	d.exp.mu.Lock()
	d.exp.at, d.exp.val, d.exp.ok = d.Now(), out, true
	d.exp.errOK = false
	d.exp.mu.Unlock()
	return out, nil
}

// cachedExposure returns a fresh answer, or (failed=true) a remembered recent
// failure; ok is false when Docker must be asked.
func (d *svcDeps) cachedExposure() (v ExposureOut, failed, ok bool) {
	d.exp.mu.Lock()
	defer d.exp.mu.Unlock()
	now := d.Now()
	if d.exp.errOK && now.Sub(d.exp.errAt) < exposureErrTTL && !d.exp.errAt.After(now) {
		return ExposureOut{}, true, true
	}
	if d.exp.ok && now.Sub(d.exp.at) < ExposureTTL && !d.exp.at.After(now) {
		return d.exp.val, false, true
	}
	return ExposureOut{}, false, false
}

// sharesPanelAddress is true for a port published on every interface or on one
// of the panel's addresses (a loopback-only port is not reachable from outside).
func sharesPanelAddress(hostIP string, panel []netip.Addr) bool {
	if hostIP == "" {
		return true
	}
	a, err := netip.ParseAddr(hostIP)
	if err != nil {
		return false
	}
	a = a.Unmap()
	if a.IsUnspecified() {
		return true
	}
	for _, p := range panel {
		if p.Unmap() == a {
			return true
		}
	}
	return false
}
