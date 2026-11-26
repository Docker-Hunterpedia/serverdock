package deploy

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"net/netip"
	"strings"
	"sync"

	"github.com/Docker-Hunterpedia/serverdock/internal/agent"
	"github.com/Docker-Hunterpedia/serverdock/internal/agent/dockerapi"
	"github.com/Docker-Hunterpedia/serverdock/internal/agentops"
	"github.com/Docker-Hunterpedia/serverdock/internal/store"
)

// Failure codes of the allocator (deploy reason codes).
const (
	CodeDocker          = "docker"            // Docker is unreachable or failed: transient, retried
	CodeIPPoolExhausted = "ip_pool_exhausted" // no free static address: permanent
	CodeAppSubnet       = "app_subnet"        // the app subnet overlaps the host, or the network drifted: permanent
)

// Error is a deploy failure with a reason code.
type Error struct {
	Code string
	Msg  string
	// Retryable is true for a transient failure; the runner wraps it with jobs.Retryable.
	Retryable bool
	Err       error
}

func (e *Error) Error() string { return e.Msg }
func (e *Error) Unwrap() error { return e.Err }

// ErrorCode returns the code of an *Error in err's chain, or "".
func ErrorCode(err error) string {
	var de *Error
	if errors.As(err, &de) {
		return de.Code
	}
	return ""
}

// Lister is what the allocator reads from Docker: *agentops.Client.
type Lister interface {
	ContainerList(ctx context.Context) (agentops.ListResult, error)
	NetworkInspect(ctx context.Context) (agentops.UsedAddrs, error)
}

// SubnetSource is the recorded subnet and its overlap check: *AppNet.
type SubnetSource interface {
	Subnet(ctx context.Context) (netip.Prefix, error)
	Check(ctx context.Context) error
}

// Allocator hands out static container addresses on the app network. One
// allocation runs at a time (the allocation lock), and each one asks Docker
// afresh, so a stale state map can never hand out a held address.
type Allocator struct {
	st     *store.Store
	docker Lister
	subnet SubnetSource
	mu     sync.Mutex

	beforeWrite func(ip netip.Addr) // test hook: runs before the address is written
}

// NewAllocator returns an Allocator.
func NewAllocator(st *store.Store, docker Lister, subnet SubnetSource) *Allocator {
	return &Allocator{st: st, docker: docker, subnet: subnet}
}

// Allocate picks a free static address for deployID and records it in
// deploys.ip. The deploy must be queued or running. An address is held (never
// handed out) when any of these is true:
//   - its deploy is some service's active deploy, or is queued or running
//     (the database);
//   - a container with our instance label still exists with it as its configured
//     address, in any state (a fresh container_list with all states, so a stopped
//     or exited container still holds it);
//   - network_inspect shows it in use (a hand-attached container).
//
// The unique index deploys_ip_live backs the lock up: two live deploys with one
// address are impossible even if the lock were bypassed. Docker unreachable is a
// retryable CodeDocker; no free address is CodeIPPoolExhausted.
func (a *Allocator) Allocate(ctx context.Context, deployID int64) (netip.Addr, error) {
	a.mu.Lock()
	lockTaken(lockAlloc, a, 0)
	defer func() {
		lockReleased(lockAlloc, a, 0)
		a.mu.Unlock()
	}()

	if err := a.subnet.Check(ctx); err != nil {
		var oe *OverlapError
		if errors.As(err, &oe) {
			return netip.Addr{}, &Error{Code: CodeAppSubnet, Msg: oe.Error(), Err: err}
		}
		return netip.Addr{}, dockerErr(ctx, "the app subnet could not be checked", err)
	}
	subnet, err := a.subnet.Subnet(ctx)
	if err != nil {
		return netip.Addr{}, dockerErr(ctx, "the app subnet is not known yet", err)
	}
	list, err := a.docker.ContainerList(ctx)
	if err != nil {
		return netip.Addr{}, dockerErr(ctx, "Docker could not list the containers", err)
	}
	used, err := a.docker.NetworkInspect(ctx)
	switch {
	case agentops.IsCode(err, agent.CodeConflict):
		return netip.Addr{}, &Error{Code: CodeIPPoolExhausted, Msg: "the app network has more addresses in use than ServerDock can track", Err: err}
	case err != nil:
		return netip.Addr{}, dockerErr(ctx, "Docker could not inspect the app network", err)
	}
	if used.Subnet != subnet.String() {
		return netip.Addr{}, &Error{Code: CodeAppSubnet, Msg: fmt.Sprintf("the app network's subnet is %s but %s is recorded: restore that subnet or recreate the network", used.Subnet, subnet)}
	}

	held := map[netip.Addr]bool{}
	hold := func(s string) {
		if ip, err := netip.ParseAddr(s); err == nil {
			held[ip] = true
		}
	}
	for _, c := range list.Containers {
		hold(c.ConfiguredIP)
		hold(c.IP) // a running container's actual address, in case the configured one is missing
	}
	for _, ip := range used.Used {
		hold(ip)
	}
	rows, err := a.st.R.QueryContext(ctx, `SELECT ip FROM deploys WHERE ip IS NOT NULL AND id <> ?
		AND (status IN ('queued', 'running') OR id IN (SELECT active_deploy_id FROM services WHERE active_deploy_id IS NOT NULL))`, deployID)
	if err != nil {
		return netip.Addr{}, err
	}
	for rows.Next() {
		var s string
		if err := rows.Scan(&s); err != nil {
			rows.Close()
			return netip.Addr{}, err
		}
		hold(s)
	}
	if err := rows.Err(); err != nil {
		rows.Close()
		return netip.Addr{}, err
	}
	rows.Close()

	info := dockerapi.NetworkInfo{Subnet: used.Subnet, Gateway: used.Gateway, IPRange: used.IPRange}
	for ip := subnet.Masked().Addr().Next().Next(); subnet.Contains(ip); ip = ip.Next() {
		if held[ip] || agentops.StaticAddr(info, ip) != nil {
			continue
		}
		ok, err := a.record(ctx, deployID, ip)
		if err != nil {
			return netip.Addr{}, err
		}
		if ok {
			return ip, nil
		}
	}
	return netip.Addr{}, &Error{Code: CodeIPPoolExhausted, Msg: "no free address is left on the app network (" + subnet.String() + ")"}
}

// record writes ip to the deploy. It reports false when the unique index says
// another live deploy has it (the caller tries the next address).
func (a *Allocator) record(ctx context.Context, deployID int64, ip netip.Addr) (bool, error) {
	if a.beforeWrite != nil {
		a.beforeWrite(ip)
	}
	conflict := false
	err := a.st.WriteTx(ctx, func(tx *sql.Tx) error {
		conflict = false
		res, err := tx.ExecContext(ctx, `UPDATE deploys SET ip = ? WHERE id = ? AND status IN ('queued', 'running')`, ip.String(), deployID)
		if err != nil {
			if strings.Contains(err.Error(), "UNIQUE constraint failed") {
				conflict = true
				return err
			}
			return err
		}
		if n, _ := res.RowsAffected(); n != 1 {
			return fmt.Errorf("deploy %d is not queued or running", deployID)
		}
		return nil
	})
	if conflict {
		return false, nil
	}
	return err == nil, err
}

// dockerErr is the retryable failure for an unreachable Docker; a cancelled or
// expired context is returned as it is.
func dockerErr(ctx context.Context, what string, err error) error {
	if ctx.Err() != nil {
		return ctx.Err()
	}
	return &Error{Code: CodeDocker, Msg: what + ": " + agent.CleanText(err.Error(), 200), Retryable: true, Err: err}
}
