package deploy

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"sync"

	"github.com/Docker-Hunterpedia/serverdock/internal/agentops"
)

// NetworkEnsurer is the agent's network_ensure op: *agentops.Client.
type NetworkEnsurer interface {
	NetworkEnsure(ctx context.Context, name, subnet string) (agentops.NetworkResult, error)
}

// BootConfig configures a Booter.
type BootConfig struct {
	// Instance is the install id (it names the app network).
	Instance string
	// AppNet is the recorded app subnet and its overlap check.
	AppNet SubnetSource
	Net    NetworkEnsurer
	Routes *Routes
	// Src is what Routes.Sync lists and inspects.
	Src BootSource
	// SyncRoutes is false when the embedded proxy is off: there is no route to
	// apply, so the boot gate stays closed and the panel (if any) runs alone.
	SyncRoutes bool
	Log        *slog.Logger
}

// Booter runs the boot sequence of the app side, in this order: the panel runs
// alone first (the local agent starts it before anything here), then
// network_ensure with the recorded subnet, then Routes.Sync, which opens the
// boot gate and applies the app routes. It is safe to call again: each part
// that already succeeded is skipped, so a caller that finds the routes not
// ready can retry it lazily. It has no goroutine and no timer.
//
// The sequence is safe before the deploy runner exists (step 8a): no app
// container exists before then, so Sync has nothing to route to.
type Booter struct {
	cfg BootConfig

	mu     sync.Mutex
	netOK  bool
	netErr string
}

// NewBooter returns a Booter; it panics on a missing dependency (a wiring bug).
func NewBooter(cfg BootConfig) *Booter {
	if cfg.Instance == "" || cfg.AppNet == nil || cfg.Net == nil || cfg.Routes == nil || cfg.Src == nil {
		panic("deploy: NewBooter needs an instance, AppNet, Net, Routes and Src")
	}
	if cfg.Log == nil {
		cfg.Log = slog.New(slog.DiscardHandler)
	}
	return &Booter{cfg: cfg}
}

// NetworkProblem is why the app network is not usable ("" when it is, or when
// nothing has tried yet): new deploys refuse with it (step 8a); running apps are
// unaffected. For an overlap it is the *OverlapError text.
func (b *Booter) NetworkProblem() string {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.netErr
}

// Run does the parts that have not succeeded yet and returns what failed. A
// network problem does not stop Sync: containers that already run keep their
// routes. A Sync failure leaves the boot gate closed.
func (b *Booter) Run(ctx context.Context) error {
	b.mu.Lock()
	defer b.mu.Unlock()
	var errs []error
	if !b.netOK {
		if err := b.ensureNetwork(ctx); err != nil {
			errs = append(errs, err)
		}
	}
	if b.cfg.SyncRoutes && !b.cfg.Routes.Ready() {
		if err := b.cfg.Routes.Sync(ctx, b.cfg.Src); err != nil {
			b.cfg.Log.Warn("the app routes are not applied yet: listing the containers failed", "err", err.Error())
			errs = append(errs, err)
		}
	}
	return errors.Join(errs...)
}

// ensureNetwork checks the recorded subnet against the host, then creates the
// app network if it is missing (enable_icc off, the gateway and the dynamic range
// set by the agent). An existing network is never recreated.
func (b *Booter) ensureNetwork(ctx context.Context) error {
	subnet, err := b.cfg.AppNet.Subnet(ctx)
	if err != nil {
		b.cfg.Log.Warn("the app network is not set up: the app subnet is not recorded yet", "err", err.Error())
		return fmt.Errorf("app subnet: %w", err)
	}
	if err := b.cfg.AppNet.Check(ctx); err != nil {
		var oe *OverlapError
		if errors.As(err, &oe) {
			b.netErr = oe.Error()
			b.cfg.Log.Error("the app network is not created: " + oe.Error())
		} else {
			b.cfg.Log.Warn("the app subnet could not be checked", "err", err.Error())
		}
		return err
	}
	if _, err := b.cfg.Net.NetworkEnsure(ctx, agentops.NetworkName(b.cfg.Instance), subnet.String()); err != nil {
		b.netErr = "the app network could not be created or does not match what ServerDock needs"
		b.cfg.Log.Error("network_ensure failed", "err", err.Error())
		return fmt.Errorf("network_ensure: %w", err)
	}
	b.netOK, b.netErr = true, ""
	return nil
}
