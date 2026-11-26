package deploy

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"net"
	"sort"
	"strconv"
	"time"

	"github.com/Docker-Hunterpedia/serverdock/internal/actions"
	"github.com/Docker-Hunterpedia/serverdock/internal/agent"
	"github.com/Docker-Hunterpedia/serverdock/internal/agentops"
	"github.com/Docker-Hunterpedia/serverdock/internal/jobs"
)

// This file is the resync rules (plan 0004 step 10, "Resync rules (the only place
// the reconciler touches containers)"). They run after every successful events
// (re)connect (a full resync) and after each job for a service finishes (that
// service only). The resync never waits for a service lock: it takes each one with
// TryLock, and a busy service is skipped (the per-service resync after its job
// covers it).
//
// For each service whose lock it got (settle, before the routes are applied):
//
//	 0. the service's in-memory override is dropped (no job is using it);
//	 1. an active deploy marked failed whose commit unknown-outcome in fact
//	    committed is finished (succeeded), and its versions_kept flag cleared;
//	 2. versions_kept is settled (stop-first kept two versions): the kept candidate
//	    is removed only after a confirmed apply that does not reference it, then the
//	    previous version is started and health-gated, then the flag is cleared;
//	 3. a stopped active container is started again (rule 3 of the plan), unless the
//	    service is flagged delete_interrupted; previous_version_down and versions_kept
//	    are cleared when it comes back up;
//	 4. the app_safe of the service's newest failed deploy is raised to yes when the
//	    old version is seen running.
//
// Then, once, the state map is loaded and the routes applied (rule 4), and only after
// that apply has succeeded does the sweep remove the orphans (rule 2: every container
// of this install whose service does not exist or whose deploy is not the service's
// active one). Routing first means an orphan's stop grace never extends an outage, and
// the apply that precedes a removal never names the container. A pass that changes
// nothing runs no action, so it writes no audit row.

// rcItem is one container of this install as the resync saw it.
type rcItem struct {
	listed  agentops.ListedContainer
	c       Container
	svc     int64
	deploy  int64
	attempt int
	gone    bool // removed during this pass
}

// rsvc is what the resync reads of a service.
type rsvc struct {
	id        int64
	exists    bool
	active    int64 // services.active_deploy_id, 0 if none
	attention string
	graceS    int
	// the active deploy
	activeIP        string
	activeStatus    string
	activeCommitted bool
	snap            Snapshot
	snapOK          bool
}

// pass is one resync: the list as it was seen and the services' rows.
type pass struct {
	rc    *Reconciler
	ctx   context.Context
	first bool
	items []*rcItem
	svcs  map[int64]*rsvc
	// applied: the routes of this pass were applied (or there is no proxy), so the
	// sweep may remove orphans.
	applied bool
}

func (rc *Reconciler) collect(ctx context.Context, only int64) ([]*rcItem, error) {
	list, err := rc.cfg.Ops.ContainerList(ctx)
	if err != nil {
		return nil, fmt.Errorf("listing containers: %w", err)
	}
	var items []*rcItem
	for _, e := range list.Containers {
		if e.Unowned {
			continue // the agent will not touch it, and neither do we (the allocator still counts its address)
		}
		svc, err1 := strconv.ParseInt(e.Labels.Service, 10, 64)
		dep, err2 := strconv.ParseInt(e.Labels.Deploy, 10, 64)
		att, _ := strconv.Atoi(e.Labels.Attempt)
		if err1 != nil || err2 != nil || svc < 1 || dep < 1 {
			continue // not a container of a deploy: never touched
		}
		if only > 0 && svc != only {
			continue
		}
		st, err := rc.cfg.Ops.ContainerInspect(ctx, e.ID)
		if agentops.IsCode(err, agent.CodeNotFound) {
			continue
		}
		if err != nil {
			return nil, fmt.Errorf("inspecting a container: %w", err)
		}
		c, ok := ContainerFromInspect(st)
		if !ok || c.Service != svc || c.Deploy != dep {
			continue // the inspect says it is not what the list said: leave it alone
		}
		items = append(items, &rcItem{listed: e, c: c, svc: svc, deploy: dep, attempt: att})
	}
	return items, nil
}

func (rc *Reconciler) loadServices(ctx context.Context, only int64) (map[int64]*rsvc, error) {
	q := `SELECT s.id, COALESCE(s.active_deploy_id, 0), COALESCE(s.needs_attention, ''), s.stop_grace_s,
		COALESCE(d.ip, ''), COALESCE(d.status, ''), COALESCE(d.committed_at IS NOT NULL, 0), COALESCE(d.snapshot_json, '')
		FROM services s LEFT JOIN deploys d ON d.id = s.active_deploy_id`
	args := []any{}
	if only > 0 {
		q += ` WHERE s.id = ?`
		args = append(args, only)
	}
	rs, err := rc.cfg.DB.R.QueryContext(ctx, q, args...)
	if err != nil {
		return nil, err
	}
	defer rs.Close()
	out := map[int64]*rsvc{}
	for rs.Next() {
		var s rsvc
		var raw string
		if err := rs.Scan(&s.id, &s.active, &s.attention, &s.graceS, &s.activeIP, &s.activeStatus, &s.activeCommitted, &raw); err != nil {
			return nil, err
		}
		s.exists = true
		if raw != "" {
			if sn, err := ParseSnapshot(raw); err == nil {
				s.snap, s.snapOK = sn, true
			}
		}
		out[s.id] = &s
	}
	return out, rs.Err()
}

// fullResync is the resync after a (re)connect: every service, then the state map is
// loaded and the routes applied. first is true only for the first one after a start
// (the only one that retries failed ports; the decision for later resyncs is
// recorded in Revision 16: no retry, so a taken port cannot make every resync
// reload Caddy).
func (rc *Reconciler) fullResync(ctx context.Context, first bool) error {
	items, err := rc.collect(ctx, 0)
	if err != nil {
		return err
	}
	svcs, err := rc.loadServices(ctx, 0)
	if err != nil {
		return fmt.Errorf("reading the services: %w", err)
	}
	p := &pass{rc: rc, ctx: ctx, first: first, items: items, svcs: svcs}
	seen := map[int64]bool{}
	var ids []int64
	for id := range svcs {
		seen[id] = true
		ids = append(ids, id)
	}
	for _, it := range items {
		if !seen[it.svc] {
			seen[it.svc] = true
			ids = append(ids, it.svc) // a service that no longer exists: its containers are orphans
		}
	}
	sort.Slice(ids, func(i, j int) bool { return ids[i] < ids[j] })
	for _, id := range ids {
		if ctx.Err() != nil {
			return ctx.Err()
		}
		p.settle(id)
	}
	// Rule 4: the state map is loaded and the routes applied BEFORE any orphan is
	// removed, so the apps are routed as soon as their containers run (an orphan's
	// stop grace never extends an outage), and a removal always follows a confirmed apply.
	all := p.containers(0)
	if rc.cfg.Proxy {
		err = rc.cfg.Routes.Load(ctx, all, first)
	} else {
		rc.cfg.Routes.LoadStateOnly(all)
	}
	if err != nil {
		if ctx.Err() != nil {
			return ctx.Err()
		}
		// The state is known and the gate is open; the routes are marked stale and
		// the next apply sends again. Nothing is removed on this pass.
		rc.cfg.Log.Warn("the app routes could not be applied after the resync; no orphan is removed yet", "err", err.Error())
	} else {
		p.applied = true
	}
	for _, id := range ids {
		if ctx.Err() != nil {
			return ctx.Err()
		}
		p.sweep(id)
	}
	// The sweep removed containers: the map must not keep them.
	return nil
}

// serviceResync is the resync of one service after a job for it finished.
func (rc *Reconciler) serviceResync(ctx context.Context, service int64) error {
	items, err := rc.collect(ctx, service)
	if err != nil {
		return err
	}
	svcs, err := rc.loadServices(ctx, service)
	if err != nil {
		return fmt.Errorf("reading the service: %w", err)
	}
	p := &pass{rc: rc, ctx: ctx, items: items, svcs: svcs}
	p.settle(service)
	cs := p.containers(service)
	if rc.cfg.Proxy {
		err := rc.cfg.Routes.ReplaceService(ctx, service, cs)
		switch {
		case err == nil || errors.Is(err, ErrRoutesNotReady):
			p.applied = true
		case ctx.Err() != nil:
			return ctx.Err()
		default:
			rc.cfg.Log.Warn("the app routes could not be applied after a service resync; no orphan is removed yet", "service", service, "err", err.Error())
		}
	} else {
		rc.cfg.Routes.ReplaceServiceStateOnly(service, cs)
		p.applied = true
	}
	p.sweep(service)
	return nil
}

func (p *pass) containers(only int64) []Container {
	var out []Container
	for _, it := range p.items {
		if !it.gone && (only == 0 || it.svc == only) {
			out = append(out, it.c)
		}
	}
	return out
}

func (p *pass) itemsOf(service int64) []*rcItem {
	var out []*rcItem
	for _, it := range p.items {
		if it.svc == service && !it.gone {
			out = append(out, it)
		}
	}
	return out
}

// settle is the first half of the rules for one service, run before the routes are
// applied (so a service that needs its container started is routed to it, and an
// orphan's stop grace never delays the apps): the override is dropped, a committed
// deploy marked failed is finished, versions_kept is settled, a stopped active
// container is started and app_safe raised. It runs only if the service's lock is free.
func (p *pass) settle(id int64) {
	rc := p.rc
	unlock, ok := rc.cfg.Locks.TryLock(id)
	if !ok {
		rc.cfg.Log.Debug("a job holds the service's lock; its own resync covers it", "service", id)
		return
	}
	defer unlock()
	sv := p.svcs[id]
	if sv == nil {
		return // the service is gone: only the sweep has something to do
	}
	mine := p.itemsOf(id)
	rc.cfg.Routes.DropOverride(id) // nothing uses it while we hold the lock
	busy, err := p.runningJob(id)
	if err != nil {
		rc.cfg.Log.Warn("the service's running job could not be read; nothing is changed for it", "service", id, "err", err.Error())
		return
	}
	if busy.running {
		return // picked, lock not taken yet: the job's own resync follows
	}
	p.fixCommitted(sv)
	if sv.attention == AttentionVersionsKept {
		p.settleKept(sv, mine)
	} else {
		p.startStopped(sv, mine)
	}
	p.raiseAppSafe(sv, mine)
}

// sweep is the second half, run after the routes were applied (rule 2 of the plan:
// the orphans): a container of this install whose service is gone, or whose deploy is
// not the service's active one, is removed. It needs the apply that came before it to
// have succeeded (applied): that apply never names such a container, so once the proxy
// holds it no route can reach it and its address may be handed out again. The service
// is read again, because a job may have run between the two halves.
func (p *pass) sweep(id int64) {
	rc := p.rc
	if !p.applied {
		return
	}
	unlock, ok := rc.cfg.Locks.TryLock(id)
	if !ok {
		return
	}
	defer unlock()
	fresh, err := rc.loadServices(p.ctx, id)
	if err != nil {
		rc.cfg.Log.Warn("a service could not be read; its orphans stay for the next resync", "service", id, "err", err.Error())
		return
	}
	sv := fresh[id]
	if sv == nil {
		sv = &rsvc{id: id, graceS: 10} // the service is gone: its containers are orphans
	}
	busy, err := p.runningJob(id)
	if err != nil {
		rc.cfg.Log.Warn("the service's running job could not be read; nothing is removed for it", "service", id, "err", err.Error())
		return
	}
	if busy.running {
		// A job is running in the database although its lock was free (it was picked
		// and has not taken the lock yet): only older attempts are orphans.
		p.orphans(sv, p.itemsOf(id), &busy)
		return
	}
	p.orphans(sv, p.itemsOf(id), nil)
}

type runningJob struct {
	running bool
	deploy  int64
	attempt int
}

func (p *pass) runningJob(service int64) (runningJob, error) {
	var r runningJob
	err := p.rc.cfg.DB.R.QueryRowContext(p.ctx, `SELECT j.attempt, COALESCE((SELECT d.id FROM deploys d WHERE d.job_id = j.id), 0)
		FROM jobs j WHERE j.lock_key = ? AND j.state = 'running' LIMIT 1`, jobs.LockKey(service)).Scan(&r.attempt, &r.deploy)
	switch {
	case errors.Is(err, sql.ErrNoRows):
		return runningJob{}, nil
	case err != nil:
		return runningJob{}, err
	}
	r.running = true
	return r, nil
}

// ---- rule 1 (Revision 14): an active deploy marked failed that did commit ----

// fixCommitted: the commit is the point of no return. A deploy whose commit
// outcome could not be read ended "failed" (both versions kept), and if its commit
// in fact happened it is the service's active deploy with committed_at set. It is
// finished here (succeeded), and the flags the failure left are cleared; the
// previous version is then an orphan like any other.
func (p *pass) fixCommitted(sv *rsvc) {
	if sv.active < 1 || sv.activeStatus != StatusFailed || !sv.activeCommitted {
		return
	}
	ctx := p.ctx
	err := p.rc.cfg.DB.WriteTx(ctx, func(tx *sql.Tx) error {
		res, err := tx.ExecContext(ctx, `UPDATE deploys SET status = 'succeeded', stage = ?, reason_code = 'warning',
			reason = 'the commit was recorded although ServerDock could not read it back at the time; ServerDock confirmed it afterwards',
			finished_at = COALESCE(finished_at, ?) WHERE id = ? AND status = 'failed' AND committed_at IS NOT NULL`, StageDone, jobs.FormatTime(time.Now()), sv.active)
		if err != nil {
			return err
		}
		if n, _ := res.RowsAffected(); n == 0 {
			return nil
		}
		_, err = tx.ExecContext(ctx, `UPDATE services SET needs_attention = NULL WHERE id = ? AND needs_attention IN (?, ?)`, sv.id, AttentionPreviousDown, AttentionVersionsKept)
		return err
	})
	if err != nil {
		p.rc.cfg.Log.Warn("a committed deploy marked failed could not be corrected", "deploy", sv.active, "err", err.Error())
		return
	}
	sv.activeStatus = StatusSucceeded
	if sv.attention == AttentionVersionsKept || sv.attention == AttentionPreviousDown {
		sv.attention = ""
	}
	p.rc.cfg.Log.Info("a deploy reported as failed had in fact committed; it is now succeeded", "deploy", sv.active)
}

// ---- rule 2 of the plan: orphans ----

// routesConfirmed is the precondition of settleKept's removal of the kept candidate,
// which happens before the pass's own apply: an apply of the list the database and
// the state map say must have succeeded. That list never names a container of a deploy that is not the
// service's active one (the overrides were dropped), so once the proxy holds it no
// route can reach the container, and its address can be handed out again. Before the
// boot gate opens nothing was ever applied by this process, so there is nothing to
// confirm.
func (p *pass) routesConfirmed() bool {
	rc := p.rc
	if !rc.cfg.Proxy || !rc.cfg.Routes.Ready() {
		return true
	}
	err := rc.cfg.Routes.RefreshNoRetry(p.ctx)
	if err == nil || errors.Is(err, ErrRoutesNotReady) {
		return true
	}
	p.rc.cfg.Log.Warn("the proxy did not confirm the routes, so no container is removed yet", "err", err.Error())
	return false
}

func (p *pass) orphans(sv *rsvc, mine []*rcItem, running *runningJob) {
	for _, it := range mine {
		if it.gone || (sv.exists && it.deploy == sv.active) {
			continue
		}
		if running != nil && it.deploy == running.deploy && it.attempt >= running.attempt {
			continue // the current attempt of the deploy whose job is running
		}
		if sv.attention == AttentionVersionsKept {
			continue // settleKept removes it (after its own confirmation), or it stays
		}
		p.remove(sv, it)
	}
}

// remove runs reconciler.remove_container for it; true when the container is gone.
func (p *pass) remove(sv *rsvc, it *rcItem) bool {
	grace := sv.graceS
	if !sv.exists {
		grace = 10
	}
	raw, _ := json.Marshal(RemoveContainerIn{Container: it.listed.ID, Service: it.svc, Deploy: it.deploy, Grace: grace})
	if _, err := p.rc.cfg.Act.Invoke(p.ctx, ActionRemoveContainer, raw, actions.Options{}); err != nil {
		p.noteErr(it.svc, err)
		p.rc.cfg.Log.Warn("an orphan container could not be removed; the next resync tries again", "service", it.svc, "deploy", it.deploy, "err", agent.CleanText(err.Error(), 200))
		return false
	}
	it.gone = true
	p.rc.cfg.Routes.Untrack(it.listed.ID)
	return true
}

func (p *pass) noteErr(service int64, err error) {
	if isDockerTrouble(err) {
		p.rc.cfg.Routes.State().MarkUnknown(service)
	}
}

// ---- rule 3 of the plan: a stopped active container ----

// startStopped starts the active deploy's container when it exists and none of its
// containers runs (a crash during stop-first, a Docker restart, a manual stop).
// A service flagged delete_interrupted is left alone: its delete is finishing.
func (p *pass) startStopped(sv *rsvc, mine []*rcItem) {
	if sv.active < 1 || sv.attention == AttentionDeleteInterrupted || sv.attention == AttentionVersionsKept {
		return // (a service still flagged versions_kept was not settled: never start the previous version beside the candidate)
	}
	var act []*rcItem
	for _, it := range mine {
		if it.deploy == sv.active && !it.gone {
			act = append(act, it)
		}
	}
	if len(act) == 0 {
		return
	}
	for _, it := range act {
		if it.c.State == StateRunning || it.c.State == StateRestarting || it.c.State == "paused" {
			p.rc.cfg.Routes.State().ClearUnknown(sv.id)
			return
		}
	}
	if !p.finish(sv) {
		return
	}
	p.refresh(act)
	for _, it := range act {
		if !it.gone && (it.c.State == StateRunning || it.c.State == StateRestarting) {
			// It is up again: a flag that said the previous version was down is stale.
			if sv.attention == AttentionPreviousDown || sv.attention == AttentionVersionsKept {
				p.setAttention(sv, "")
			}
			return
		}
	}
}

// finish runs reconciler.finish_deploy for the service's active deploy.
func (p *pass) finish(sv *rsvc) bool {
	raw, _ := json.Marshal(FinishDeployIn{Service: sv.id, Deploy: sv.active})
	if _, err := p.rc.cfg.Act.Invoke(p.ctx, ActionFinishDeploy, raw, actions.Options{}); err != nil {
		p.noteErr(sv.id, err)
		p.rc.cfg.Log.Warn("a stopped container could not be started; the next resync tries again", "service", sv.id, "err", agent.CleanText(err.Error(), 200))
		return false
	}
	return true
}

// refresh re-inspects items after an action changed them.
func (p *pass) refresh(its []*rcItem) {
	for _, it := range its {
		st, err := p.rc.cfg.Ops.ContainerInspect(p.ctx, it.listed.ID)
		if agentops.IsCode(err, agent.CodeNotFound) {
			it.gone = true
			p.rc.cfg.Routes.Untrack(it.listed.ID)
			continue
		}
		if err != nil {
			p.noteErr(it.svc, err)
			continue
		}
		if c, ok := ContainerFromInspect(st); ok {
			it.c = c
			p.rc.cfg.Routes.State().ClearUnknown(it.svc)
		}
	}
}

// setAttention replaces the flags this feature sets (previous_version_down,
// versions_kept) by code, or clears them ("") and never overwrites another reason.
func (p *pass) setAttention(sv *rsvc, code string) {
	ctx := p.ctx
	err := p.rc.cfg.DB.WriteTx(ctx, func(tx *sql.Tx) error {
		var v any
		if code != "" {
			v = code
		}
		_, err := tx.ExecContext(ctx, `UPDATE services SET needs_attention = ? WHERE id = ? AND needs_attention IN (?, ?)`, v, sv.id, AttentionPreviousDown, AttentionVersionsKept)
		return err
	})
	if err != nil {
		p.rc.cfg.Log.Warn("a service's attention flag could not be written", "service", sv.id, "err", err.Error())
		return
	}
	sv.attention = code
}

// ---- Revision 15: versions_kept ----

// settleKept settles a service flagged versions_kept (a stop-first deploy ended with
// its candidate kept next to the stopped previous version). Only after an apply that
// confirms the proxy holds a list that does not name the candidate is the candidate
// removed; then the previous version is started and health-gated, and the flag is
// cleared. If the commit did happen, fixCommitted has already cleared the flag and
// the candidate is the active deploy (and is kept).
func (p *pass) settleKept(sv *rsvc, mine []*rcItem) {
	var cands, olds []*rcItem
	for _, it := range mine {
		if it.gone {
			continue
		}
		if it.deploy == sv.active {
			olds = append(olds, it)
		} else {
			cands = append(cands, it)
		}
	}
	if len(cands) > 0 {
		if !p.routesConfirmed() {
			return // still flagged: the next resync tries again
		}
		for _, it := range cands {
			if !p.remove(sv, it) {
				return
			}
		}
	}
	if len(olds) == 0 {
		// The previous version's container is gone as well: nothing to start. The
		// service shows "needs attention" until a deploy commits.
		p.setAttention(sv, AttentionPreviousDown)
		return
	}
	anyUp := false
	for _, it := range olds {
		if it.c.State == StateRunning || it.c.State == StateRestarting {
			anyUp = true
		}
	}
	if !anyUp {
		if !p.finish(sv) {
			return
		}
		p.refresh(olds)
	}
	reason, unverified := p.gate(sv, olds)
	switch {
	case unverified:
		return // stopping: neither proven nor failed; the flag stays and the next start tries again
	case reason != "":
		p.rc.cfg.Log.Warn("the previous version came back but is not healthy", "service", sv.id, "reason", reason)
		p.setAttention(sv, AttentionPreviousDown)
	default:
		p.setAttention(sv, "")
	}
}

// gate is the health gate of the previous version's containers: every check
// inspects them (an exit, a crash loop or an OOM kill fails it at once) and probes
// the app, until health.passes in a row pass or health_timeout_s runs out. It
// returns why the version is not healthy ("" if it is); unverified is true when the
// process is stopping before the gate could finish.
func (p *pass) gate(sv *rsvc, olds []*rcItem) (reason string, unverified bool) {
	rc := p.rc
	h := SnapHealth{Kind: "running", TimeoutS: 30, Passes: 1}
	port := 0
	if sv.snapOK {
		h, port = sv.snap.Health, sv.snap.ContainerPort
	}
	if h.TimeoutS < 1 {
		h.TimeoutS = 30
	}
	if h.Passes < 1 {
		h.Passes = 1
	}
	hctx, cancel := context.WithTimeout(p.ctx, time.Duration(h.TimeoutS)*time.Second)
	defer cancel()
	ip := sv.activeIP
	if ip == "" {
		ip = olds[0].c.IP
	}
	target := net.JoinHostPort(ip, strconv.Itoa(port))
	passes, lastErr := 0, ""
	for {
		running := true
		for _, it := range olds {
			st, err := rc.cfg.Ops.ContainerInspect(hctx, it.listed.ID)
			if err != nil {
				if hctx.Err() == nil && agentops.IsCode(err, agent.CodeNotFound) {
					return "its container disappeared", false
				}
				lastErr, running = "Docker did not answer the inspect", false
				continue
			}
			switch {
			case st.OOMKilled:
				return "it was killed for using too much memory", false
			case st.Restarting:
				return fmt.Sprintf("it keeps crashing (it restarted after exit code %d)", st.ExitCode), false
			case !st.Running:
				return fmt.Sprintf("it exited with code %d", st.ExitCode), false
			}
			if c, ok := ContainerFromInspect(st); ok {
				it.c = c
			}
		}
		ok := false
		if running {
			if h.Kind == "running" || port == 0 {
				ok = true
			} else {
				kind := agentops.KindTCP
				if h.Kind == "http" {
					kind = agentops.KindHTTP
				}
				res, err := rc.cfg.Ops.HealthCheck(hctx, agentops.HealthArgs{Target: target, Kind: kind, Path: h.Path, TimeoutMS: agentops.MaxHealthMS})
				switch {
				case err != nil:
					lastErr = "the check could not run"
				case !res.OK && res.Status != 0:
					lastErr = fmt.Sprintf("the app answered HTTP %d", res.Status)
				case !res.OK:
					lastErr = res.Err
				default:
					ok = true
				}
			}
		}
		if ok {
			if passes++; passes >= h.Passes {
				return "", false
			}
		} else {
			passes = 0
		}
		c, stop := rc.cfg.NewTimer(rc.cfg.HealthInterval)
		select {
		case <-hctx.Done():
			stop()
			if p.ctx.Err() != nil {
				return "", true
			}
			msg := fmt.Sprintf("it was not healthy within %d seconds", h.TimeoutS)
			if lastErr != "" {
				msg += " (" + lastErr + ")"
			}
			return agent.CleanText(msg, 300), false
		case <-c:
		}
	}
}

// raiseAppSafe: the newest deploy of the service ended failed with app_safe
// unknown (the boot pass or the queue's backstop could not see the old version);
// now that the old version is seen running, it says yes.
func (p *pass) raiseAppSafe(sv *rsvc, mine []*rcItem) {
	if sv.active < 1 {
		return
	}
	serving := false
	for _, it := range mine {
		if !it.gone && it.deploy == sv.active && (it.c.State == StateRunning || it.c.State == StateRestarting) {
			serving = true
		}
	}
	if !serving {
		return
	}
	ctx := p.ctx
	const where = `id = (SELECT MAX(id) FROM deploys WHERE service_id = ?)
		AND status = 'failed' AND committed_at IS NULL AND app_safe = 'unknown' AND reason_code IN ('interrupted', 'job_failed')`
	var one int
	if err := p.rc.cfg.DB.R.QueryRowContext(ctx, `SELECT 1 FROM deploys WHERE `+where, sv.id).Scan(&one); err != nil {
		return // nothing to raise (or unreadable: the next pass looks again)
	}
	err := p.rc.cfg.DB.WriteTx(ctx, func(tx *sql.Tx) error {
		_, err := tx.ExecContext(ctx, `UPDATE deploys SET app_safe = 'yes' WHERE `+where, sv.id)
		return err
	})
	if err != nil {
		p.rc.cfg.Log.Warn("a deploy's app_safe could not be updated", "service", sv.id, "err", err.Error())
	}
}
