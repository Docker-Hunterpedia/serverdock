package deploy

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
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
// Per pass, in this order:
//
//	settle (fast, under the service's TryLock, before the routes are applied):
//	 0. the service's in-memory override is dropped (no job is using it);
//	 1. an active deploy marked failed whose commit unknown-outcome in fact
//	    committed is finished (succeeded), its flags cleared, and its audit
//	    outcome corrected;
//	 2. the app_safe of the service's newest failed deploy is raised to yes when
//	    the old version is seen running.
//	apply: the state map is loaded (keeping the entries of every service whose lock
//	    was not got) and the routes applied (rule 4).
//	finish (slow, under the TryLock again, only after that apply succeeded):
//	 3. every container of another deploy than the active one (an orphan, a kept
//	    candidate, the candidate of an interrupted deploy) is removed (rule 2), and
//	    ONLY when none is left a stopped active container is started (rule 3), so two
//	    versions never run; previous_version_down and versions_kept are cleared when it
//	    runs, and previous_version_down is set when it does not come up (or is gone).
//
// Routing first means no slow step (a stop grace, a start) delays any app or the
// scheduler's start gate, and a removal always follows a confirmed apply that never
// names the container. There is no health gate here: events keep the state honest
// after the start. A pass that changes nothing runs no action, so it writes no audit row.

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
	// finish half may remove and start containers.
	applied bool
	// skipped: services whose lock was not got (a job holds it) or that have a job
	// running in the database: their state-map entries are kept as they are.
	skipped map[int64]bool
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
// reload Caddy). applied reports whether the routes were applied (nothing is removed
// or started otherwise).
func (rc *Reconciler) fullResync(ctx context.Context, first bool, onApplied func()) (applied bool, err error) {
	items, err := rc.collect(ctx, 0)
	if err != nil {
		return false, err
	}
	svcs, err := rc.loadServices(ctx, 0)
	if err != nil {
		return false, fmt.Errorf("reading the services: %w", err)
	}
	p := &pass{rc: rc, ctx: ctx, first: first, items: items, svcs: svcs, skipped: map[int64]bool{}}
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
			return false, ctx.Err()
		}
		p.settle(id)
	}
	// Rule 4: the state map is loaded and the routes applied BEFORE any slow step. The
	// services whose lock was not got keep their entries: the list was taken before a job
	// (which holds the lock) may have tracked its candidate.
	all := p.containers(0)
	if rc.cfg.Proxy {
		err = rc.cfg.Routes.Load(ctx, all, first, p.skipped)
	} else {
		rc.cfg.Routes.LoadStateOnly(all, p.skipped)
	}
	if ctx.Err() != nil {
		return false, ctx.Err()
	}
	if err != nil {
		// The state is known, so the resync counts as done for the scheduler's start gate;
		// the routes are marked stale and the session ends right after, so the back-off
		// retries. Nothing is removed or started on this pass. first stays true until a Load works.
		if onApplied != nil {
			onApplied()
		}
		rc.cfg.Log.Warn("the app routes could not be applied after the resync; nothing is removed or started yet", "err", err.Error())
		return false, nil
	}
	rc.loaded.Store(true)
	p.applied = true
	// What happened during the list and the rules is looked at before anything is removed.
	overflow, derr := rc.drainIDs(ctx)
	if derr != nil {
		return false, derr
	}
	if overflow {
		return false, errTooManyEvents // the reconnect's resync starts over
	}
	// The state is loaded, the routes applied and the buffered events handled: the start
	// gate opens; the slow half below delays it not at all. (The service state stays
	// unknown until the whole resync is done: only then is it settled.)
	if onApplied != nil {
		onApplied()
	}
	for _, id := range ids {
		if ctx.Err() != nil {
			return false, ctx.Err()
		}
		p.finish(id)
	}
	return true, nil
}

// serviceResync is the resync of one service after a job for it finished. The lock is
// held from the list to the end (service lock, then the route lock inside the apply:
// the allowed order), so a job cannot start between the list and the state it replaces.
func (rc *Reconciler) serviceResync(ctx context.Context, service int64) error {
	unlock, ok := rc.cfg.Locks.TryLock(service)
	if !ok {
		return nil // another job holds it: that job's own resync follows
	}
	defer unlock()
	items, err := rc.collect(ctx, service)
	if err != nil {
		return err
	}
	svcs, err := rc.loadServices(ctx, service)
	if err != nil {
		return fmt.Errorf("reading the service: %w", err)
	}
	p := &pass{rc: rc, ctx: ctx, items: items, svcs: svcs, skipped: map[int64]bool{}}
	p.settleLocked(service)
	cs := p.containers(service)
	if rc.cfg.Proxy {
		err := rc.cfg.Routes.ReplaceService(ctx, service, cs)
		switch {
		case err == nil || errors.Is(err, ErrRoutesNotReady):
			p.applied = true
		case ctx.Err() != nil:
			return ctx.Err()
		default:
			rc.cfg.Log.Warn("the app routes could not be applied after a service resync; nothing is removed or started yet", "service", service, "err", err.Error())
		}
	} else {
		rc.cfg.Routes.ReplaceServiceStateOnly(service, cs)
		p.applied = true
	}
	if !p.applied {
		return nil
	}
	sv := p.svcs[service]
	if sv == nil {
		sv = &rsvc{id: service, graceS: 10}
	}
	p.finishLocked(sv)
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

// settle is the fast first half for one service, if its lock is free.
func (p *pass) settle(id int64) {
	unlock, ok := p.rc.cfg.Locks.TryLock(id)
	if !ok {
		p.skipped[id] = true
		p.rc.cfg.Log.Debug("a job holds the service's lock; its own resync covers it", "service", id)
		return
	}
	defer unlock()
	p.settleLocked(id)
}

// settleLocked: the caller holds the service's lock.
func (p *pass) settleLocked(id int64) {
	rc := p.rc
	sv := p.svcs[id]
	if sv == nil {
		return // the service is gone: only the finish half has something to do
	}
	rc.cfg.Routes.DropOverride(id) // nothing uses it while we hold the lock
	busy, err := p.runningJob(id)
	if err != nil {
		p.skipped[id] = true
		rc.cfg.Log.Warn("the service's running job could not be read; nothing is changed for it", "service", id, "err", err.Error())
		return
	}
	if busy.running {
		p.skipped[id] = true // picked, lock not taken yet: the job's own resync follows
		return
	}
	p.fixCommitted(sv)
	p.raiseAppSafe(sv, p.itemsOf(id))
}

// finish is the slow second half, run after the routes were applied: containers of
// other deploys are removed, then a stopped active container is started. The apply
// that came before it never names a container of another deploy, so once the proxy
// holds it no route can reach one (the proxy is the in-process Caddy: a restart of
// ServerDock restarts it too, so nothing stale survives a boot). The service is read
// again, because a job may have run between the two halves.
func (p *pass) finish(id int64) {
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
		rc.cfg.Log.Warn("a service could not be read; nothing is removed or started for it", "service", id, "err", err.Error())
		return
	}
	sv := fresh[id]
	if sv == nil {
		sv = &rsvc{id: id, graceS: 10} // the service is gone: its containers are orphans
	}
	p.finishLocked(sv)
}

func (p *pass) finishLocked(sv *rsvc) {
	rc := p.rc
	mine := p.itemsOf(sv.id)
	busy, err := p.runningJob(sv.id)
	if err != nil {
		rc.cfg.Log.Warn("the service's running job could not be read; nothing is changed for it", "service", sv.id, "err", err.Error())
		return
	}
	if busy.running {
		// A job is running in the database although its lock was free (it was picked
		// and has not taken the lock yet): only older attempts are orphans, and nothing is started.
		p.orphans(sv, mine, &busy)
		return
	}
	if !p.orphans(sv, mine, nil) {
		return // another version may still run: the active container is not started
	}
	if sv.exists && sv.active < 1 && (sv.attention == AttentionVersionsKept || sv.attention == AttentionPreviousDown) {
		// No active deploy (a first deploy): there is no previous version to start, and the
		// candidate is gone, so nothing is left for the flag to say.
		p.setAttention(sv, "")
	}
	if sv.exists {
		p.startStopped(sv, mine)
		p.raiseAppSafe(sv, mine) // the container may have been started just now
	}
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
// finished here (succeeded), the flags the failure left are cleared, and the audit
// outcome of its run (left at "error") gets a later "ok" outcome; the previous
// version is then an orphan like any other.
func (p *pass) fixCommitted(sv *rsvc) {
	if sv.active < 1 || sv.activeStatus != StatusFailed || !sv.activeCommitted {
		return
	}
	ctx := p.ctx
	changed := false
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
		changed = true
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
	if changed && p.rc.cfg.Audit != nil {
		correctIntent(ctx, p.rc.cfg.DB, p.rc.cfg.Audit, p.rc.cfg.Log, sv.active)
	}
	p.rc.cfg.Log.Info("a deploy reported as failed had in fact committed; it is now succeeded", "deploy", sv.active)
}

// ---- rule 2 of the plan: containers of other deploys ----

// orphans removes every container of the service that is not of its active deploy
// (every one when the service is gone), except the current attempt of a running job's
// deploy. It reports whether none of another deploy is left (so starting the active
// container cannot run two versions).
//
// Nothing is removed before the proxy is confirmed, now and under the service's lock,
// not to hold a route to it: the override is dropped and the route list the database
// and the state map say is applied (RefreshNoRetry; a list the proxy already holds is
// not sent again). A job may have switched, failed its revert and kept its candidate
// since the pass's own apply. If that apply does not succeed nothing is removed and the
// next resync tries again.
//
// A container of a deploy that never committed (the stop-first gap, an interrupted
// candidate) that stands between a stopped active container and its start is stopped
// with at most 10 s of grace (the stop call's own parameter): it never served the
// service's committed traffic, and the outage lasts as long as its stop. Every other
// orphan gets the service's stop_grace_s.
func (p *pass) orphans(sv *rsvc, mine []*rcItem, running *runningJob) bool {
	var out []*rcItem
	for _, it := range mine {
		if it.gone || (sv.exists && it.deploy == sv.active) {
			continue
		}
		if running != nil && it.deploy == running.deploy && it.attempt >= running.attempt {
			continue // the current attempt of the deploy whose job is running
		}
		out = append(out, it)
	}
	if len(out) == 0 {
		return true
	}
	if !p.confirmRoutes(sv) {
		return false
	}
	startPending := p.startPending(sv, mine)
	clean := true
	for _, it := range out {
		grace := sv.graceS
		if !sv.exists {
			grace = 10
		}
		if startPending && p.uncommitted(it.deploy) {
			grace = min(grace, orphanGraceCap)
		}
		if !p.remove(it, grace) {
			clean = false
		}
	}
	return clean
}

// orphanGraceCap is the longest stop grace of a container of an uncommitted deploy that
// keeps the active container from starting.
const orphanGraceCap = 10

// confirmRoutes: see orphans. True when the proxy holds a list that names no
// container of another deploy (or there is no proxy, or nothing was ever applied).
func (p *pass) confirmRoutes(sv *rsvc) bool {
	rc := p.rc
	if sv.exists {
		rc.cfg.Routes.DropOverride(sv.id)
	}
	if !rc.cfg.Proxy || !rc.cfg.Routes.Ready() {
		return true
	}
	err := rc.cfg.Routes.RefreshNoRetry(p.ctx)
	if err == nil || errors.Is(err, ErrRoutesNotReady) {
		return true
	}
	rc.cfg.Log.Warn("the proxy did not confirm the routes, so no container is removed yet", "service", sv.id, "err", err.Error())
	return false
}

// startPending: the service's active deploy has containers and none of them runs.
func (p *pass) startPending(sv *rsvc, mine []*rcItem) bool {
	if !sv.exists || sv.active < 1 {
		return false
	}
	any := false
	for _, it := range mine {
		if it.gone || it.deploy != sv.active {
			continue
		}
		any = true
		if it.c.State == StateRunning || it.c.State == StateRestarting || it.c.State == "paused" {
			return false
		}
	}
	return any
}

// uncommitted: the deploy's row exists and it never committed.
func (p *pass) uncommitted(deploy int64) bool {
	var committed bool
	err := p.rc.cfg.DB.R.QueryRowContext(p.ctx, `SELECT committed_at IS NOT NULL FROM deploys WHERE id = ?`, deploy).Scan(&committed)
	return err == nil && !committed
}

// remove runs reconciler.remove_container for it with grace; true when the container is gone.
func (p *pass) remove(it *rcItem, grace int) bool {
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
// containers runs (a crash during stop-first, a Docker restart, a manual stop), once
// no other version is left. A service flagged delete_interrupted, or whose delete
// failed in memory, is left alone: its delete is finishing. The flags
// previous_version_down and versions_kept are cleared when it runs, and
// previous_version_down is set when it does not come up (or is gone) after versions_kept.
func (p *pass) startStopped(sv *rsvc, mine []*rcItem) {
	if sv.active < 1 || sv.attention == AttentionDeleteInterrupted || p.rc.cfg.Routes.State().DeleteFailed(sv.id) {
		return
	}
	var act []*rcItem
	for _, it := range mine {
		if it.deploy == sv.active && !it.gone {
			act = append(act, it)
		}
	}
	flagged := sv.attention == AttentionPreviousDown || sv.attention == AttentionVersionsKept
	if len(act) == 0 {
		if sv.attention == AttentionVersionsKept {
			p.setAttention(sv, AttentionPreviousDown) // the previous version's container is gone too
		}
		return
	}
	p.refresh(act) // a fresh look: nothing is invoked (and nothing audited) when there is nothing to start
	up := func() bool {
		for _, it := range act {
			if !it.gone && (it.c.State == StateRunning || it.c.State == StateRestarting || it.c.State == "paused") {
				return true
			}
		}
		return false
	}
	if up() {
		p.rc.cfg.Routes.State().ClearUnknown(sv.id)
		if flagged {
			p.setAttention(sv, "") // it is up and nothing else runs: the flag is stale
		}
		return
	}
	if !p.start(sv) {
		if sv.attention == AttentionVersionsKept {
			p.setAttention(sv, AttentionPreviousDown)
		}
		return
	}
	p.refresh(act)
	if up() {
		if flagged {
			p.setAttention(sv, "")
		}
		for _, it := range act {
			if !it.gone {
				p.rc.routeAfterStart(p.ctx, it.c) // the route follows at once; events keep it right after that
			}
		}
		return
	}
	if sv.attention == AttentionVersionsKept {
		p.setAttention(sv, AttentionPreviousDown)
	}
}

func (rc *Reconciler) routeAfterStart(ctx context.Context, c Container) {
	rc.routeErr(rc.cfg.Routes.SetContainer(ctx, c))
}

// start runs reconciler.finish_deploy for the service's active deploy.
func (p *pass) start(sv *rsvc) bool {
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

// raiseAppSafe: the newest deploy of the service ended failed with app_safe unknown
// (the boot pass or the queue's backstop could not see the old version); now that the
// old version is seen running, it says yes. Only a deploy that ended since this
// process started (the boot pass's, or a job's since) is raised (ReconcilerConfig.Since):
// a deploy that failed long ago must not claim afterwards that the app stayed up.
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
	where := `id = (SELECT MAX(id) FROM deploys WHERE service_id = ?) AND id > ?
		AND status = 'failed' AND committed_at IS NULL AND app_safe = 'unknown' AND reason_code IN ('interrupted', 'job_failed')`
	args := []any{sv.id, sv.active}
	if !p.rc.cfg.Since.IsZero() {
		where += ` AND finished_at >= ?`
		args = append(args, jobs.FormatTime(p.rc.cfg.Since))
	}
	var one int
	if err := p.rc.cfg.DB.R.QueryRowContext(ctx, `SELECT 1 FROM deploys WHERE `+where, args...).Scan(&one); err != nil {
		return // nothing to raise (or unreadable: the next pass looks again)
	}
	err := p.rc.cfg.DB.WriteTx(ctx, func(tx *sql.Tx) error {
		_, err := tx.ExecContext(ctx, `UPDATE deploys SET app_safe = 'yes' WHERE `+where, args...)
		return err
	})
	if err != nil {
		p.rc.cfg.Log.Warn("a deploy's app_safe could not be updated", "service", sv.id, "err", err.Error())
	}
}
