package deploy

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"strconv"
	"time"

	"github.com/Docker-Hunterpedia/serverdock/internal/actions"
	"github.com/Docker-Hunterpedia/serverdock/internal/agent"
	"github.com/Docker-Hunterpedia/serverdock/internal/agent/dockerapi"
	"github.com/Docker-Hunterpedia/serverdock/internal/agentops"
)

// The reconciler's two actions (plan 0004 step 10). They are the only way the
// reconciler touches a container, and they are system-only: ConsoleOnly means every
// actor except the SystemInvoker is denied, REST answers 404 and OpenAPI omits them.
// Each run is audited as the system actor (intent first, then the outcome); a pass
// that has nothing to do runs none, so it writes no audit row.

// Action names.
const (
	ActionRemoveContainer = "reconciler.remove_container"
	ActionFinishDeploy    = "reconciler.finish_deploy"
)

// RemoveContainerIn is the input of reconciler.remove_container.
type RemoveContainerIn struct {
	Container string `json:"container" doc:"the full container id"`
	Service   int64  `json:"service" doc:"the service the container is labelled with"`
	Deploy    int64  `json:"deploy" doc:"the deploy the container is labelled with"`
	Grace     int    `json:"grace" doc:"seconds the container gets to stop (the service's stop grace; 10 if the service is gone)"`
}

// Validate implements actions.Validator.
func (in RemoveContainerIn) Validate() error {
	switch {
	case !dockerapi.ValidFullID(in.Container):
		return errors.New("container must be a full container id")
	case in.Service < 1 || in.Deploy < 1:
		return errors.New("service and deploy must be ids")
	case in.Grace < 0 || in.Grace > agentops.MaxGraceS:
		return fmt.Errorf("grace must be 0..%d", agentops.MaxGraceS)
	}
	return nil
}

// RemoveContainerOut is the output of reconciler.remove_container.
type RemoveContainerOut struct {
	Removed bool `json:"removed"`
	// LogKept is true only when the deploy's log tail was kept; false also when it was
	// not attempted (no hook until step 11) or could not be kept (the removal goes on).
	LogKept bool `json:"log_kept"`
}

// FinishDeployIn is the input of reconciler.finish_deploy.
type FinishDeployIn struct {
	Service int64 `json:"service"`
	Deploy  int64 `json:"deploy" doc:"the service's active deploy"`
}

// Validate implements actions.Validator.
func (in FinishDeployIn) Validate() error {
	if in.Service < 1 || in.Deploy < 1 {
		return errors.New("service and deploy must be ids")
	}
	return nil
}

// FinishDeployOut is the output of reconciler.finish_deploy.
type FinishDeployOut struct {
	Started int `json:"started" doc:"how many stopped containers of the deploy were started"`
}

// RegisterReconcilerActions registers the reconciler's two system-only actions.
func RegisterReconcilerActions(reg *actions.Registry, rc *Reconciler) {
	actions.Register(reg, actions.Def[RemoveContainerIn, RemoveContainerOut]{
		Name: ActionRemoveContainer, Summary: "Remove a container of this install that no deploy owns (only the reconciler can run this, as the system).",
		Risk: actions.RiskDestructive, Scope: actions.ScopeAdmin, ConsoleOnly: true,
		System: "reconciler: removes a container of this install whose service is gone or whose deploy is not the service's live one (the candidate of an interrupted deploy, the old version of a deploy that committed before a restart), after the proxy was confirmed not to route to it; anonymous volumes only",
		Target: func(in RemoveContainerIn) string {
			if len(in.Container) >= 12 {
				return "container:" + in.Container[:12]
			}
			return "container:invalid"
		},
		Execute: func(ctx context.Context, _ actions.Call, in RemoveContainerIn) (RemoveContainerOut, error) {
			return rc.removeContainer(ctx, in)
		},
	})
	actions.Register(reg, actions.Def[FinishDeployIn, FinishDeployOut]{
		Name: ActionFinishDeploy, Summary: "Start the stopped container of a service's live deploy again (only the reconciler can run this, as the system).",
		Risk: actions.RiskMutate, Scope: actions.ScopeWrite, ConsoleOnly: true,
		System: "reconciler: starts the live deploy's container when it exists and is stopped (a crash during a stop-first deploy, a Docker restart), never on a service flagged delete_interrupted",
		Target: func(in FinishDeployIn) string { return fmt.Sprintf("service:%d", in.Service) },
		Execute: func(ctx context.Context, _ actions.Call, in FinishDeployIn) (FinishDeployOut, error) {
			return rc.finishDeploy(ctx, in)
		},
	})
}

// removeContainer: stop with the grace, keep the log tail (*DeployLogs.Keep; nil in
// tests), remove (anonymous volumes only: the agent's removal never takes a named
// volume), and forget it. The agent refuses any container that is not this
// install's, so a look-alike is never touched.
func (rc *Reconciler) removeContainer(ctx context.Context, in RemoveContainerIn) (RemoveContainerOut, error) {
	var out RemoveContainerOut
	// The deploy must not be the service's live one, whatever called this.
	var active sql.NullInt64
	if err := rc.cfg.DB.R.QueryRowContext(ctx, `SELECT active_deploy_id FROM services WHERE id = ?`, in.Service).Scan(&active); err != nil && !errors.Is(err, sql.ErrNoRows) {
		return out, errors.New("the service could not be read")
	}
	if active.Valid && active.Int64 == in.Deploy {
		return out, errors.New("the container belongs to the service's live deploy")
	}
	sctx, cancel := context.WithTimeout(ctx, time.Duration(in.Grace)*time.Second+StopExtra)
	stopErr := rc.cfg.Ops.ContainerStop(sctx, in.Container, in.Grace)
	cancel()
	if stopErr != nil && !agentops.IsCode(stopErr, agent.CodeNotFound) {
		return out, fmt.Errorf("the container could not be stopped: %s", opText(stopErr, "Docker did not answer"))
	}
	if !agentops.IsCode(stopErr, agent.CodeNotFound) {
		// Stopped, not yet removed: keep what the container printed, including while it
		// stopped. A short bound: the resync has other containers to settle, and a
		// removal with a capped grace must stay within its promise.
		if rc.cfg.KeepLog != nil {
			var exists bool
			if err := rc.cfg.DB.R.QueryRowContext(ctx, `SELECT EXISTS (SELECT 1 FROM deploys WHERE id = ?)`, in.Deploy).Scan(&exists); err == nil && exists {
				kctx, kcancel := context.WithTimeout(ctx, gapKeepBound)
				err := rc.cfg.KeepLog(kctx, in.Deploy, in.Container)
				out.LogKept = err == nil
				kcancel()
				if err != nil && !errors.Is(err, ErrOlderDeploy) {
					rc.cfg.Log.Warn("an orphan's log was not kept", "deploy", in.Deploy, "err", agent.CleanText(err.Error(), 120))
				}
			}
		}
		rctx, rcancel := context.WithTimeout(ctx, RemoveBound)
		err := rc.cfg.Ops.ContainerRemove(rctx, in.Container)
		rcancel()
		if err != nil && !agentops.IsCode(err, agent.CodeNotFound) {
			return out, fmt.Errorf("the container could not be removed: %s", opText(err, "Docker did not answer"))
		}
	}
	rc.cfg.Routes.Untrack(in.Container)
	out.Removed = true
	return out, nil
}

// finishDeploy starts the stopped containers of the service's live deploy. It
// refuses unless the deploy is still the service's live one and the service is not
// being deleted: the state may have changed since the resync read it.
func (rc *Reconciler) finishDeploy(ctx context.Context, in FinishDeployIn) (FinishDeployOut, error) {
	var out FinishDeployOut
	var active sql.NullInt64
	var attention sql.NullString
	if err := rc.cfg.DB.R.QueryRowContext(ctx, `SELECT active_deploy_id, needs_attention FROM services WHERE id = ?`, in.Service).Scan(&active, &attention); err != nil {
		return out, errors.New("the service could not be read")
	}
	if !active.Valid || active.Int64 != in.Deploy {
		return out, errors.New("the deploy is not the service's live one")
	}
	if (attention.Valid && attention.String == AttentionDeleteInterrupted) || rc.cfg.Routes.State().DeleteFailed(in.Service) {
		return out, errors.New("the service is being deleted")
	}
	list, err := rc.cfg.Ops.ContainerList(ctx)
	if err != nil {
		return out, fmt.Errorf("listing containers: %s", opText(err, "Docker did not answer"))
	}
	svc, dep := strconv.FormatInt(in.Service, 10), strconv.FormatInt(in.Deploy, 10)
	type cand struct{ id string }
	var toStart []cand
	for _, c := range list.Containers {
		if c.Unowned || c.Labels.Service != svc {
			continue
		}
		st, err := rc.cfg.Ops.ContainerInspect(ctx, c.ID)
		if agentops.IsCode(err, agent.CodeNotFound) {
			continue
		}
		if err != nil {
			return out, fmt.Errorf("inspecting a container: %s", opText(err, "Docker did not answer"))
		}
		if !st.Owned {
			continue
		}
		if c.Labels.Deploy != dep {
			if st.Running || st.Restarting {
				// Two versions must never run: refuse, whatever flag or caller said so.
				return out, errors.New("another version of the service is running")
			}
			continue
		}
		if !st.Running && !st.Restarting {
			toStart = append(toStart, cand{c.ID})
		}
	}
	for _, c := range toStart {
		sctx, cancel := context.WithTimeout(ctx, RemoveBound)
		err = rc.cfg.Ops.ContainerStart(sctx, c.id)
		cancel()
		if err != nil {
			return out, fmt.Errorf("the container could not be started: %s", opText(err, "Docker did not answer"))
		}
		out.Started++
	}
	return out, nil
}
