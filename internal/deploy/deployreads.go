package deploy

import (
	"context"
	"database/sql"
	"errors"

	"github.com/Docker-Hunterpedia/serverdock/internal/actions"
	"github.com/Docker-Hunterpedia/serverdock/internal/agent"
	"github.com/Docker-Hunterpedia/serverdock/internal/agentops"
	"github.com/Docker-Hunterpedia/serverdock/internal/store"
)

// deploys.list, deploys.get and deploys.log (plan 0004 step 11): the reads of the
// deploy history, the live progress of a pull and the deploy log.

// DeployReadDeps is what the deploy reads need.
type DeployReadDeps struct {
	DB *store.Store
	// Progress is *Runner.Progress: the latest pull progress of a running deploy.
	// Nil means none is known.
	Progress func(deploy int64) (agentops.PullProgress, bool)
	// Logs reads the deploy logs; nil means none are kept on this server.
	Logs *DeployLogs
}

// DeploysListIn is the input of deploys.list.
type DeploysListIn struct {
	Service string `json:"service" doc:"<project>/<service>"`
	Limit   int    `json:"limit,omitempty" doc:"how many deploys, newest first (1 to 200; default 50)"`
}

// Validate implements actions.Validator.
func (in DeploysListIn) Validate() error {
	if _, _, err := splitRef(in.Service); err != nil {
		return err
	}
	if in.Limit < 0 || in.Limit > KeepDeploys {
		return errors.New("limit must be between 1 and 200")
	}
	return nil
}

// DeployItem is one deploy of a service.
type DeployItem struct {
	Number     int    `json:"number"`
	Kind       string `json:"kind" doc:"deploy, redeploy or rollback"`
	Status     string `json:"status" doc:"queued, running, succeeded, failed, cancelled or superseded"`
	Stage      string `json:"stage,omitempty"`
	ReasonCode string `json:"reason_code,omitempty"`
	Reason     string `json:"reason,omitempty"`
	AppSafe    string `json:"app_safe,omitempty" doc:"yes, no or unknown: did the previous version keep serving?"`
	ImageRef   string `json:"image_ref,omitempty"`
	ImageID    string `json:"image_id,omitempty"`
	RepoDigest string `json:"repo_digest,omitempty"`
	Active     bool   `json:"active" doc:"this is the service's live deploy"`
	WasLive    bool   `json:"was_live" doc:"the deploy committed once: it can be rolled back to"`
	LogKept    bool   `json:"log_kept" doc:"a deploy log file exists (read it with deploys.log)"`
	// TriggeredBy is the kind of caller that queued the deploy: user, token, mcp,
	// webhook or system. It never names a user or a token.
	TriggeredBy  string `json:"triggered_by,omitempty" doc:"who queued it: user, token, mcp, webhook or system"`
	Strategy     string `json:"strategy,omitempty" doc:"blue_green or stop_first: the strategy this deploy's settings recorded (a rollback to it keeps it)"`
	SupersededBy int    `json:"superseded_by,omitempty" doc:"for a superseded deploy: the number of the deploy that replaced it in the queue"`
	CreatedAt    string `json:"created_at"`
	StartedAt    string `json:"started_at,omitempty"`
	FinishedAt   string `json:"finished_at,omitempty"`
}

// DeploysListOut is the output of deploys.list.
type DeploysListOut struct {
	Service string       `json:"service"`
	Deploys []DeployItem `json:"deploys"`
}

// DeploysGetIn is the input of deploys.get and deploys.log.
type DeploysGetIn struct {
	Service string `json:"service" doc:"<project>/<service>"`
	Number  int    `json:"number" doc:"the deploy's number"`
}

// Validate implements actions.Validator.
func (in DeploysGetIn) Validate() error {
	if _, _, err := splitRef(in.Service); err != nil {
		return err
	}
	if in.Number < 1 {
		return errors.New("number must be 1 or more")
	}
	return nil
}

// StageItem is one stage of a deploy.
type StageItem struct {
	Stage      string `json:"stage"`
	State      string `json:"state" doc:"pending, running, ok, failed or skipped"`
	StartedAt  string `json:"started_at,omitempty"`
	FinishedAt string `json:"finished_at,omitempty"`
	Detail     string `json:"detail,omitempty"`
}

// PullProgressOut is the latest progress of the image pull of a running deploy
// (in memory: it is not stored; the final summary is the pull stage's detail).
type PullProgressOut struct {
	Layers     int    `json:"layers"`
	LayersDone int    `json:"layers_done"`
	Bytes      int64  `json:"bytes"`
	BytesTotal int64  `json:"bytes_total"`
	Status     string `json:"status,omitempty"`
}

// DeploysGetOut is the output of deploys.get.
type DeploysGetOut struct {
	Service     string           `json:"service"`
	Deploy      DeployItem       `json:"deploy"`
	Stages      []StageItem      `json:"stages"`
	Progress    *PullProgressOut `json:"progress,omitempty" doc:"the pull's latest progress, only while the deploy is pulling"`
	Cancellable bool             `json:"cancellable" doc:"deploys.cancel would take effect now (false inside switch, commit, drain and stop_old, once a cancel was asked, and after the end)"`
}

// DeploysLogOut is the output of deploys.log.
type DeploysLogOut struct {
	Service   string `json:"service"`
	Number    int    `json:"number"`
	Kept      bool   `json:"kept" doc:"false: there is no log; message says why"`
	Text      string `json:"text,omitempty" doc:"the log (at most 1 MiB), cleaned of control and bidirectional characters; show it as plain text"`
	Truncated bool   `json:"truncated,omitempty"`
	Message   string `json:"message,omitempty"`
}

type deployReads struct{ DeployReadDeps }

// RegisterDeployReadActions registers deploys.list, deploys.get and deploys.log.
// It panics on a missing database (a wiring bug fails at start).
func RegisterDeployReadActions(r *actions.Registry, deps DeployReadDeps) {
	if deps.DB == nil {
		panic("deploy: RegisterDeployReadActions needs a database")
	}
	d := &deployReads{deps}
	actions.Register(r, actions.Def[DeploysListIn, DeploysListOut]{
		Name: "deploys.list", Summary: "List a service's deploys, newest first: status, stage, reason, whether it was once live, whether a log is kept.",
		Risk: actions.RiskRead, Scope: actions.ScopeRead,
		Execute: func(ctx context.Context, _ actions.Call, in DeploysListIn) (DeploysListOut, error) {
			return d.list(ctx, in)
		},
	})
	actions.Register(r, actions.Def[DeploysGetIn, DeploysGetOut]{
		Name: "deploys.get", Summary: "One deploy: its stages, the live progress of its image pull, and whether it can be cancelled now.",
		Risk: actions.RiskRead, Scope: actions.ScopeRead,
		Execute: func(ctx context.Context, _ actions.Call, in DeploysGetIn) (DeploysGetOut, error) {
			return d.get(ctx, in)
		},
	})
	actions.Register(r, actions.Def[DeploysGetIn, DeploysLogOut]{
		Name: "deploys.log", Summary: "The log of a deploy: its stages and the tail of its container's output (at most 1 MiB, cleaned). Container output can hold credentials, so an API token needs write scope; MCP never gets it.",
		Risk: actions.RiskRead, Scope: actions.ScopeWrite,
		Execute: func(ctx context.Context, _ actions.Call, in DeploysGetIn) (DeploysLogOut, error) {
			return d.log(ctx, in)
		},
	})
}

const deployItemCols = `d.id, d.service_id, d.number, d.kind, d.status, d.stage, d.reason_code, d.reason, d.app_safe, json_extract(d.snapshot_json, '$.image_ref'),
	d.image_id, d.repo_digest, COALESCE(d.id = s.active_deploy_id, 0), d.committed_at IS NOT NULL, d.created_at, d.started_at, d.finished_at,
	d.actor_kind, COALESCE((SELECT x.number FROM deploys x WHERE x.id = d.superseded_by), 0), COALESCE(json_extract(d.snapshot_json, '$.strategy'), '')`

type itemRow struct {
	id, service int64
	item        DeployItem
}

func scanItem(s rowScanner, extra ...any) (itemRow, error) {
	var r itemRow
	var stage, code, reason, safe, image, imageID, digest, started, finished sql.NullString
	var active, was bool
	dst := append([]any{&r.id, &r.service, &r.item.Number, &r.item.Kind, &r.item.Status, &stage, &code, &reason, &safe, &image, &imageID, &digest,
		&active, &was, &r.item.CreatedAt, &started, &finished, &r.item.TriggeredBy, &r.item.SupersededBy, &r.item.Strategy}, extra...)
	if err := s.Scan(dst...); err != nil {
		return r, err
	}
	r.item.Stage, r.item.ReasonCode, r.item.Reason, r.item.AppSafe = stage.String, code.String, reason.String, safe.String
	r.item.ImageRef, r.item.ImageID, r.item.RepoDigest = image.String, imageID.String, digest.String
	r.item.Active, r.item.WasLive, r.item.StartedAt, r.item.FinishedAt = active, was, started.String, finished.String
	return r, nil
}

func (d *deployReads) serviceID(ctx context.Context, ref string) (int64, error) {
	project, name, err := splitRef(ref)
	if err != nil {
		return 0, actions.InvalidInput("%v", err)
	}
	var id int64
	err = d.DB.R.QueryRowContext(ctx, `SELECT s.id FROM services s JOIN projects p ON p.id = s.project_id WHERE p.name = ? AND s.name = ?`, project, name).Scan(&id)
	if errors.Is(err, sql.ErrNoRows) {
		return 0, actions.NotFound("no service %s/%s", project, name)
	}
	return id, err
}

func (d *deployReads) list(ctx context.Context, in DeploysListIn) (DeploysListOut, error) {
	svc, err := d.serviceID(ctx, in.Service)
	if err != nil {
		return DeploysListOut{}, err
	}
	limit := in.Limit
	if limit == 0 {
		limit = 50
	}
	rows, err := d.DB.R.QueryContext(ctx, `SELECT `+deployItemCols+` FROM deploys d JOIN services s ON s.id = d.service_id
		WHERE d.service_id = ? ORDER BY d.number DESC LIMIT ?`, svc, limit)
	if err != nil {
		return DeploysListOut{}, err
	}
	defer rows.Close()
	kept := d.Logs.Kept(svc)
	out := DeploysListOut{Service: in.Service, Deploys: []DeployItem{}}
	for rows.Next() {
		r, err := scanItem(rows)
		if err != nil {
			return DeploysListOut{}, err
		}
		r.item.LogKept = kept[r.id]
		out.Deploys = append(out.Deploys, r.item)
	}
	return out, rows.Err()
}

func (d *deployReads) one(ctx context.Context, ref string, number int, extra ...any) (itemRow, error) {
	project, name, err := splitRef(ref)
	if err != nil {
		return itemRow{}, actions.InvalidInput("%v", err)
	}
	r, err := scanItem(d.DB.R.QueryRowContext(ctx, `SELECT `+deployItemCols+`, COALESCE(j.cancel_requested, 0), j.id IS NOT NULL
		FROM deploys d JOIN services s ON s.id = d.service_id JOIN projects p ON p.id = s.project_id LEFT JOIN jobs j ON j.id = d.job_id
		WHERE p.name = ? AND s.name = ? AND d.number = ?`, project, name, number), extra...)
	if errors.Is(err, sql.ErrNoRows) {
		return itemRow{}, actions.NotFound("no deploy #%d of %s", number, ref)
	}
	return r, err
}

func (d *deployReads) get(ctx context.Context, in DeploysGetIn) (DeploysGetOut, error) {
	var cancelAsked, hasJob bool
	r, err := d.one(ctx, in.Service, in.Number, &cancelAsked, &hasJob)
	if err != nil {
		return DeploysGetOut{}, err
	}
	out := DeploysGetOut{Service: in.Service, Deploy: r.item, Stages: []StageItem{}}
	r.item.LogKept = d.Logs.Kept(r.service)[r.id]
	out.Deploy = r.item
	rows, err := d.DB.R.QueryContext(ctx, `SELECT stage, state, started_at, finished_at, detail FROM deploy_stages WHERE deploy_id = ? ORDER BY rowid`, r.id)
	if err != nil {
		return DeploysGetOut{}, err
	}
	defer rows.Close()
	for rows.Next() {
		var s StageItem
		var started, finished, detail sql.NullString
		if err := rows.Scan(&s.Stage, &s.State, &started, &finished, &detail); err != nil {
			return DeploysGetOut{}, err
		}
		s.StartedAt, s.FinishedAt, s.Detail = started.String, finished.String, detail.String
		out.Stages = append(out.Stages, s)
	}
	if err := rows.Err(); err != nil {
		return DeploysGetOut{}, err
	}
	live := r.item.Status == StatusQueued || r.item.Status == StatusRunning
	if live && r.item.Stage == StagePull && d.Progress != nil {
		if p, ok := d.Progress(r.id); ok {
			out.Progress = &PullProgressOut{Layers: p.Layers, LayersDone: p.LayersDone, Bytes: p.Bytes, BytesTotal: p.BytesTotal, Status: agent.CleanText(p.Status, 64)}
		}
	}
	out.Cancellable = live && hasJob && !cancelAsked && CancellableStage(r.item.Stage)
	return out, nil
}

func (d *deployReads) log(ctx context.Context, in DeploysGetIn) (DeploysLogOut, error) {
	r, err := d.one(ctx, in.Service, in.Number, new(bool), new(bool))
	if err != nil {
		return DeploysLogOut{}, err
	}
	out := DeploysLogOut{Service: in.Service, Number: in.Number}
	text, truncated, err := d.Logs.Read(r.service, r.id)
	switch {
	case err == nil:
		out.Kept, out.Text, out.Truncated = true, text, truncated
		return out, nil
	case errors.Is(err, ErrLogBusy):
		return DeploysLogOut{}, actions.Unavailable("deploy logs are being read right now; try again in a moment")
	case !errors.Is(err, ErrNoLog):
		return DeploysLogOut{}, actions.Unavailable("the log of deploy #%d could not be read", in.Number)
	}
	switch {
	case d.Logs == nil || d.Logs.Root == "":
		out.Message = "Deploy logs are not kept on this server"
	case r.item.Status == StatusQueued || r.item.Status == StatusRunning:
		out.Message = "No log yet: a deploy's log is kept when its container is removed"
	case r.item.Active:
		out.Message = "No log yet: the log of the live version is kept when a newer deploy replaces it"
	default:
		var recent bool
		if err := d.DB.R.QueryRowContext(ctx, `SELECT EXISTS (SELECT 1 FROM (SELECT id FROM deploys WHERE service_id = ? ORDER BY number DESC LIMIT ?) WHERE id = ?)`,
			r.service, LogKeep, r.id).Scan(&recent); err != nil {
			return DeploysLogOut{}, err
		}
		if recent {
			out.Message = "No log was kept for this deploy: it never ran a container, or the log could not be written"
		} else {
			out.Message = NoLogMessage
		}
	}
	return out, nil
}
