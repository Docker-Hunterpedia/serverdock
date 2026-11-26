package deploy

import (
	"bytes"
	"compress/gzip"
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/Docker-Hunterpedia/serverdock/internal/actions"
	"github.com/Docker-Hunterpedia/serverdock/internal/agentops"
	"github.com/Docker-Hunterpedia/serverdock/internal/jobs"
)

// fakeLogSrc is the agent's `logs` op: a container id maps to its output. It records
// whether the container still existed at the time of each call (a log must be read
// before the container is removed).
type fakeLogSrc struct {
	mu    sync.Mutex
	texts map[string]string
	gen   func(container string) io.Reader // overrides texts
	err   error
	calls []string
	alive map[string]bool
	tails []int
	ops   *fakeOps
}

func (f *fakeLogSrc) LogsOnce(_ context.Context, container string, tail int) (io.ReadCloser, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.calls = append(f.calls, container)
	f.tails = append(f.tails, tail)
	if f.alive == nil {
		f.alive = map[string]bool{}
	}
	if f.ops != nil {
		f.alive[container] = f.ops.find(container) != nil
	}
	if f.err != nil {
		return nil, f.err
	}
	if f.gen != nil {
		return io.NopCloser(f.gen(container)), nil
	}
	return io.NopCloser(strings.NewReader(f.texts[container])), nil
}

func (f *fakeLogSrc) set(container, text string) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.texts == nil {
		f.texts = map[string]string{}
	}
	f.texts[container] = text
}

func (f *fakeLogSrc) wasAlive(container string) (alive, called bool) {
	f.mu.Lock()
	defer f.mu.Unlock()
	alive, called = f.alive[container]
	return
}

// logRig is a runner rig whose deploys keep their logs under a temporary root.
type logRig struct {
	*rrig
	dl   *DeployLogs
	src  *fakeLogSrc
	root string
}

func newLogRig(t *testing.T, opts ...rigOpt) *logRig {
	t.Helper()
	l := &logRig{src: &fakeLogSrc{}, root: filepath.Join(t.TempDir(), "deploy-logs")}
	opts = append(opts, func(c *RunnerConfig) {
		c.LogRoot = l.root
		c.KeepLog = func(ctx context.Context, d int64, container string) error { return l.dl.Keep(ctx, d, container) }
		c.PruneLogs = func(ctx context.Context, s int64) { l.dl.Prune(ctx, s) }
	})
	l.rrig = newRRig(t, opts...)
	l.dl = &DeployLogs{Root: l.root, DB: l.st, Src: l.src}
	l.src.ops = l.ops
	RegisterDeployReadActions(l.reg, DeployReadDeps{DB: l.st, Progress: l.runner.Progress, Logs: l.dl})
	return l
}

func (l *logRig) text(deploy int64) string {
	l.t.Helper()
	var svc int64
	if err := l.st.R.QueryRow(`SELECT service_id FROM deploys WHERE id = ?`, deploy).Scan(&svc); err != nil {
		l.t.Fatal(err)
	}
	s, _, err := l.dl.Read(svc, deploy)
	if err != nil {
		l.t.Fatalf("log of deploy %d: %v", deploy, err)
	}
	return s
}

func (l *logRig) hasLog(deploy int64) bool {
	var svc int64
	if err := l.st.R.QueryRow(`SELECT service_id FROM deploys WHERE id = ?`, deploy).Scan(&svc); err != nil {
		l.t.Fatal(err)
	}
	return l.dl.Kept(svc)[deploy]
}

func (l *logRig) stageDetail(deploy int64, stage string) string {
	var d string
	l.st.R.QueryRow(`SELECT COALESCE(detail, '') FROM deploy_stages WHERE deploy_id = ? AND stage = ?`, deploy, stage).Scan(&d)
	return d
}

// ---- the file: format, modes, cap ----

func TestDeployLogFileIsGzipPrivateAndHoldsHeaderStagesAndTail(t *testing.T) {
	g := newLogRig(t)
	d := addDeploy(t, g.st, g.svc, 1, "failed", "10.250.0.2")
	mustExec(t, g.st, `INSERT INTO deploy_stages(deploy_id, stage, state, started_at, finished_at, detail) VALUES (?, 'pull', 'ok', 't0', 't1', '12 layers')`, d)
	g.src.set("c1", "hello from the app\nsecond line\n")
	if err := g.dl.Keep(context.Background(), d, "c1"); err != nil {
		t.Fatal(err)
	}
	dir := filepath.Join(g.root, strconv.FormatInt(g.svc, 10))
	file := filepath.Join(dir, strconv.FormatInt(d, 10)+".log.gz")
	for p, want := range map[string]os.FileMode{g.root: 0o700, dir: 0o700, file: 0o600} {
		fi, err := os.Lstat(p)
		if err != nil {
			t.Fatal(err)
		}
		if fi.Mode().Perm() != want {
			t.Errorf("%s is %v, want %v", p, fi.Mode().Perm(), want)
		}
	}
	raw, _ := os.ReadFile(file)
	zr, err := gzip.NewReader(bytes.NewReader(raw))
	if err != nil {
		t.Fatal("the file is not gzip:", err)
	}
	b, _ := io.ReadAll(zr)
	txt := string(b)
	for _, want := range []string{"ServerDock deploy log", "service: p/web", "deploy: #1", "stage pull", "12 layers", "--- container output", "hello from the app\nsecond line\n"} {
		if !strings.Contains(txt, want) {
			t.Errorf("the log lacks %q:\n%s", want, txt)
		}
	}
	if strings.Index(txt, "stage pull") > strings.Index(txt, "hello from the app") {
		t.Error("the stage lines must come before the container output")
	}
	if got := g.src.tails; len(got) != 1 || got[0] != LogTail {
		t.Errorf("the tail asked of the agent: %v, want [%d]", got, LogTail)
	}
	// No temporary file is left behind.
	if ents, _ := os.ReadDir(dir); len(ents) != 1 {
		t.Errorf("%d files in the directory, want 1", len(ents))
	}
}

// A directory made earlier with a loose mode is tightened; a link in its place is
// refused and never followed.
func TestDeployLogDirectoryIsTightenedAndLinksAreRefused(t *testing.T) {
	g := newLogRig(t)
	d := addDeploy(t, g.st, g.svc, 1, "failed", "10.250.0.2")
	g.src.set("c1", "x")
	dir := filepath.Join(g.root, strconv.FormatInt(g.svc, 10))
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	os.Chmod(g.root, 0o755)
	os.Chmod(dir, 0o755)
	if err := g.dl.Keep(context.Background(), d, "c1"); err != nil {
		t.Fatal(err)
	}
	for _, p := range []string{g.root, dir} {
		if fi, _ := os.Lstat(p); fi.Mode().Perm() != 0o700 {
			t.Errorf("%s is %v", p, fi.Mode().Perm())
		}
	}
	// The service directory is a link to somewhere else.
	elsewhere := t.TempDir()
	os.RemoveAll(dir)
	if err := os.Symlink(elsewhere, dir); err != nil {
		t.Fatal(err)
	}
	if err := g.dl.Keep(context.Background(), d, "c1"); err == nil {
		t.Error("a log was written through a symlinked directory")
	}
	if ents, _ := os.ReadDir(elsewhere); len(ents) != 0 {
		t.Errorf("something was written outside the root: %v", ents)
	}
	if _, _, err := g.dl.Read(g.svc, d); !errors.Is(err, ErrNoLog) {
		t.Errorf("a read through a symlinked directory: %v", err)
	}
	// The file itself is a link to a readable file elsewhere.
	os.Remove(dir)
	os.Mkdir(dir, 0o700)
	secret := filepath.Join(elsewhere, "secret")
	os.WriteFile(secret, []byte("not a log"), 0o600)
	os.Symlink(secret, filepath.Join(dir, strconv.FormatInt(d, 10)+".log.gz"))
	if _, _, err := g.dl.Read(g.svc, d); err == nil || errors.Is(err, ErrNoLog) {
		t.Errorf("a symlinked log file was followed: %v", err)
	}
	if g.dl.Kept(g.svc)[d] {
		t.Error("a symlink counts as a kept log")
	}
}

func TestDeployLogPathsValidateTheirIds(t *testing.T) {
	root := t.TempDir()
	for _, tc := range []struct {
		svc, dep int64
		ok       bool
	}{{1, 1, true}, {0, 1, false}, {-1, 1, false}, {1, 0, false}, {1, -5, false}} {
		p, err := logFile(root, tc.svc, tc.dep)
		if (err == nil) != tc.ok {
			t.Errorf("logFile(%d, %d): %v", tc.svc, tc.dep, err)
		}
		if err == nil && filepath.Dir(filepath.Dir(p)) != root {
			t.Errorf("%s is not exactly two levels under the root", p)
		}
	}
	if _, err := logFile("relative/root", 1, 1); err == nil {
		t.Error("a relative root was accepted")
	}
	for name, ok := range map[string]bool{"12.log.gz": true, "0.log.gz": false, "-1.log.gz": false, "1a.log.gz": false, "../1.log.gz": false, ".log.gz": false, "12.log": false} {
		if _, got := logFileID(name); got != ok {
			t.Errorf("logFileID(%q) = %v", name, got)
		}
	}
}

// A container that logs far more than the cap leaves a file that reads back at no
// more than 1 MiB, with the cut marked.
func TestDeployLogCapHoldsForHugeOutput(t *testing.T) {
	g := newLogRig(t)
	d := addDeploy(t, g.st, g.svc, 1, "failed", "10.250.0.2")
	var served int64
	g.src.gen = func(string) io.Reader {
		line := bytes.Repeat([]byte("a"), 4095)
		line = append(line, '\n')
		return readerFunc(func(p []byte) (int, error) {
			if served >= 50<<20 {
				return 0, io.EOF
			}
			n := copy(p, line)
			served += int64(n)
			return n, nil
		})
	}
	if err := g.dl.Keep(context.Background(), d, "c1"); err != nil {
		t.Fatal(err)
	}
	if served > 2<<20 {
		t.Errorf("%d bytes were read from a 50 MiB output: the cap does not stop the read", served)
	}
	text, truncated, err := g.dl.Read(g.svc, d)
	if err != nil {
		t.Fatal(err)
	}
	if len(text) > LogMaxBytes || truncated {
		t.Errorf("read back %d bytes (truncated %v)", len(text), truncated)
	}
	if len(text) < LogMaxBytes-4096 {
		t.Errorf("only %d bytes were kept of a 50 MiB output", len(text))
	}
	if !strings.Contains(text, "output cut at") {
		t.Error("the cut is not marked")
	}
}

type readerFunc func(p []byte) (int, error)

func (f readerFunc) Read(p []byte) (int, error) { return f(p) }

// A hostile file cannot expand beyond the limit when read: a gzip bomb of 100 MiB of zeros.
func TestDeployLogReadIsLimitedAgainstAGzipBomb(t *testing.T) {
	g := newLogRig(t)
	d := addDeploy(t, g.st, g.svc, 1, "failed", "10.250.0.2")
	dir := filepath.Join(g.root, strconv.FormatInt(g.svc, 10))
	os.MkdirAll(dir, 0o700)
	f, _ := os.Create(filepath.Join(dir, strconv.FormatInt(d, 10)+".log.gz"))
	zw := gzip.NewWriter(f)
	zero := make([]byte, 1<<20)
	for i := 0; i < 100; i++ {
		zw.Write(zero)
	}
	zw.Close()
	f.Close()
	text, truncated, err := g.dl.Read(g.svc, d)
	if err != nil {
		t.Fatal(err)
	}
	if len(text) > LogMaxBytes || !truncated {
		t.Errorf("read %d bytes, truncated %v", len(text), truncated)
	}
	if strings.ContainsRune(text, 0) {
		t.Error("NUL bytes survived")
	}
}

func TestDeployLogKeepFailuresLeaveNoFile(t *testing.T) {
	g := newLogRig(t)
	d := addDeploy(t, g.st, g.svc, 1, "failed", "10.250.0.2")
	dir := filepath.Join(g.root, strconv.FormatInt(g.svc, 10))
	// The agent cannot read the container.
	g.src.err = errors.New("no such container")
	if err := g.dl.Keep(context.Background(), d, "c1"); err == nil {
		t.Error("an unreadable container was reported as kept")
	}
	// The agent answers with its in-band error line.
	g.src.err = nil
	g.src.set("c1", "serverdock-agent: log stream failed\n")
	if err := g.dl.Keep(context.Background(), d, "c1"); err == nil {
		t.Error("the agent's error line was kept as the log")
	}
	// An id that is not a container id, a deploy that does not exist.
	if err := g.dl.Keep(context.Background(), d, "../x"); err == nil {
		t.Error("a bad container id was accepted")
	}
	if err := g.dl.Keep(context.Background(), d+100, "c1"); err == nil {
		t.Error("a missing deploy was accepted")
	}
	if ents, _ := os.ReadDir(dir); len(ents) != 0 {
		t.Errorf("files left: %v", ents)
	}
}

// ---- retention ----

func TestDeployLogRetentionKeepsTheLastTenPerService(t *testing.T) {
	g := newLogRig(t)
	other := g.service("api")
	g.src.set("c1", "output")
	ctx := context.Background()
	otherDeploy := addDeploy(t, g.st, other, 1, "failed", "10.250.0.9")
	if err := g.dl.Keep(ctx, otherDeploy, "c1"); err != nil {
		t.Fatal(err)
	}
	ids := map[int]int64{}
	for n := 1; n <= 15; n++ {
		ids[n] = addDeploy(t, g.st, g.svc, n, "failed", nil)
		if err := g.dl.Keep(ctx, ids[n], "c1"); err != nil {
			t.Fatalf("deploy %d: %v", n, err)
		}
		g.dl.Prune(ctx, g.svc)
	}
	kept := g.dl.Kept(g.svc)
	if len(kept) != LogKeep {
		t.Fatalf("%d logs kept, want %d: %v", len(kept), LogKeep, kept)
	}
	for n := 1; n <= 15; n++ {
		if want := n > 5; kept[ids[n]] != want {
			t.Errorf("deploy %d kept = %v, want %v", n, kept[ids[n]], want)
		}
	}
	if !g.dl.Kept(other)[otherDeploy] {
		t.Error("another service's log was pruned")
	}
	// An older deploy cannot get a log any more (an orphan of #2 is not kept).
	if err := g.dl.Keep(ctx, ids[2], "c1"); err == nil {
		t.Error("a log was written for a deploy outside the last ten")
	}
	// The message an older deploy shows.
	out, err := g.invoke(rigWrite, "deploys.log", m{"service": "p/web", "number": 2})
	if err != nil {
		t.Fatal(err)
	}
	if o := out.(DeploysLogOut); o.Kept || o.Message != NoLogMessage {
		t.Errorf("%+v", o)
	}
}

// A stray temporary file from a crash goes once it is old; a young one (a write in
// progress) stays; unknown names are never touched.
func TestDeployLogPruneRemovesOldTempFilesOnly(t *testing.T) {
	g := newLogRig(t)
	g.dl.Now = func() time.Time { return time.Now().Add(time.Hour) }
	dir := filepath.Join(g.root, strconv.FormatInt(g.svc, 10))
	os.MkdirAll(dir, 0o700)
	old := filepath.Join(dir, ".log-1.tmp")
	other := filepath.Join(dir, "notes.txt")
	os.WriteFile(old, []byte("x"), 0o600)
	os.WriteFile(other, []byte("x"), 0o600)
	// A file of a deploy that no longer exists.
	os.WriteFile(filepath.Join(dir, "999.log.gz"), []byte("x"), 0o600)
	addDeploy(t, g.st, g.svc, 1, "failed", nil)
	g.dl.Prune(context.Background(), g.svc)
	if _, err := os.Stat(old); !os.IsNotExist(err) {
		t.Error("an old temporary file was kept")
	}
	if _, err := os.Stat(filepath.Join(dir, "999.log.gz")); !os.IsNotExist(err) {
		t.Error("the log of a deploy that is gone was kept")
	}
	if _, err := os.Stat(other); err != nil {
		t.Error("an unknown file was removed")
	}
}

// ---- the three capture points ----

// The candidate of a failed deploy: its tail is read before it is removed, and the
// log says why the attempt failed.
func TestFailedCandidatesTailIsKeptBeforeItIsRemoved(t *testing.T) {
	g := newLogRig(t)
	old := g.live(g.svc, 1, "10.250.0.2")
	oldID := g.containerOf(old).id
	g.expose(g.svc, 9000)
	g.px.fail, g.px.failN = errors.New("down"), 1 // the switch fails; the old routes come back
	// The candidate's id is only known once it exists: serve a tail for any container.
	g.src.gen = func(c string) io.Reader { return strings.NewReader("candidate output of " + c[:8] + "\n") }
	g.start(false)
	d, job := g.deploy(g.svc)
	g.wait(job)
	if r := g.row(d); r.status != StatusFailed {
		t.Fatalf("%+v", r)
	}
	cand := ""
	g.src.mu.Lock()
	for _, c := range g.src.calls {
		if c != oldID {
			cand = c
		}
	}
	g.src.mu.Unlock()
	if cand == "" {
		t.Fatal("the candidate's log was never read")
	}
	if alive, _ := g.src.wasAlive(cand); !alive {
		t.Error("the candidate's log was read after it was removed")
	}
	if g.alive(cand) {
		t.Error("the candidate was not removed")
	}
	txt := g.text(d)
	for _, want := range []string{"candidate output of " + cand[:8], "this attempt:", "stage switch"} {
		if !strings.Contains(txt, want) {
			t.Errorf("the log lacks %q:\n%s", want, txt)
		}
	}
	if det := g.stageDetail(d, StageSwitch); strings.Contains(det, "log not kept") {
		t.Errorf("switch detail %q", det)
	}
}

// Cancelled before the switch: the same path keeps the tail.
func TestCancelledCandidatesTailIsKept(t *testing.T) {
	g := newLogRig(t)
	g.live(g.svc, 1, "10.250.0.2")
	g.src.gen = func(c string) io.Reader { return strings.NewReader("output of the cancelled candidate\n") }
	g.ops.healthFn = func(context.Context, agentops.HealthArgs) (agentops.HealthResult, error) {
		mustExecIgnore(g.rrig, `UPDATE jobs SET cancel_requested = 1, cancel_reason = 'cancelled by the user' WHERE state = 'running'`)
		return agentops.HealthResult{OK: true}, nil
	}
	g.start(false)
	d, job := g.deploy(g.svc)
	g.wait(job)
	if r := g.row(d); r.status != StatusCancelled {
		t.Fatalf("%+v", r)
	}
	if txt := g.text(d); !strings.Contains(txt, "output of the cancelled candidate") {
		t.Errorf("%s", txt)
	}
}

// The log of a candidate that could not be kept is a note in the failed stage, and
// the deploy still ends as it would have.
func TestCandidateLogWriteFailureIsNotedInTheStageDetail(t *testing.T) {
	g := newLogRig(t)
	g.live(g.svc, 1, "10.250.0.2")
	g.expose(g.svc, 9000)
	g.px.fail, g.px.failN = errors.New("down"), 1
	g.src.err = errors.New("the log could not be read")
	g.start(false)
	d, job := g.deploy(g.svc)
	g.wait(job)
	r := g.row(d)
	if r.status != StatusFailed || r.code != CodeSwitchFailed {
		t.Fatalf("%+v", r)
	}
	if det := g.stageDetail(d, StageSwitch); !strings.Contains(det, "log not kept") {
		t.Errorf("switch detail %q", det)
	}
	if g.hasLog(d) {
		t.Error("a log exists although it could not be written")
	}
}

// After a success the old container's tail goes to the OLD deploy's log, before the
// container is stopped.
func TestOldContainersTailIsKeptAfterASuccess(t *testing.T) {
	g := newLogRig(t)
	old := g.live(g.svc, 1, "10.250.0.2")
	oldID := g.containerOf(old).id
	g.src.set(oldID, "the old version said goodbye\n")
	l := g.record(oldID)
	g.start(false)
	d, job := g.deploy(g.svc)
	if s := g.wait(job); s != jobs.StateSucceeded {
		t.Fatalf("%s: %+v", s, g.row(d))
	}
	if alive, called := g.src.wasAlive(oldID); !called || !alive {
		t.Errorf("old log read: called %v, container alive at the time %v", called, alive)
	}
	if !strings.Contains(g.text(old), "the old version said goodbye") {
		t.Errorf("old deploy's log: %s", g.text(old))
	}
	if g.hasLog(d) {
		t.Error("the live deploy has a log before it is replaced")
	}
	if det := g.stageDetail(d, StageStopOld); strings.Contains(det, "log not kept") {
		t.Errorf("stop_old detail %q", det)
	}
	if !strings.Contains(strings.Join(l.list(), ","), "stop:old,remove:old") {
		t.Errorf("order %v", l.list())
	}
	// The read says why the live deploy has no log yet.
	out, err := g.invoke(rigWrite, "deploys.log", m{"service": "p/web", "number": 2})
	if err != nil {
		t.Fatal(err)
	}
	if o := out.(DeploysLogOut); o.Kept || !strings.Contains(o.Message, "live version") {
		t.Errorf("%+v", o)
	}
}

// A write failure after a success is a note on stop_old and never fails the deploy.
func TestOldContainersLogWriteFailureNotesStopOld(t *testing.T) {
	g := newLogRig(t)
	g.live(g.svc, 1, "10.250.0.2")
	g.src.err = errors.New("the log could not be read")
	g.start(false)
	d, job := g.deploy(g.svc)
	if s := g.wait(job); s != jobs.StateSucceeded {
		t.Fatalf("%s: %+v", s, g.row(d))
	}
	if det := g.stageDetail(d, StageStopOld); !strings.Contains(det, "log not kept: ") {
		t.Errorf("stop_old detail %q", det)
	}
}

// An orphan the resync removes: its tail is kept first, and LogKept says so.
func TestOrphansTailIsKeptOnResync(t *testing.T) {
	var l *DeployLogs
	src := &fakeLogSrc{}
	root := filepath.Join(t.TempDir(), "deploy-logs")
	g := newRecRig(t, func(_ *recRig, cfg *ReconcilerConfig) {
		cfg.KeepLog = func(ctx context.Context, d int64, c string) error { return l.Keep(ctx, d, c) }
	})
	l = &DeployLogs{Root: root, DB: g.st, Src: src}
	src.ops = g.ops
	g.activeDeploy(g.svc, 1, "10.250.0.2", true)
	dead := addDeploy(t, g.st, g.svc, 2, "failed", "10.250.0.3")
	id := g.put(g.svc, dead, 1, "10.250.0.3", true)
	src.set(id, "the orphan's last words\n")
	g.resync(false)
	if g.ops.find(id) != nil {
		t.Fatal("the orphan was not removed")
	}
	if alive, called := src.wasAlive(id); !called || !alive {
		t.Errorf("the orphan's log: called %v, alive at the time %v", called, alive)
	}
	s, _, err := l.Read(g.svc, dead)
	if err != nil || !strings.Contains(s, "the orphan's last words") {
		t.Errorf("%q %v", s, err)
	}
}

// LogKept is false when nothing was written.
func TestOrphanRemovalReportsLogKeptHonestly(t *testing.T) {
	var l *DeployLogs
	src := &fakeLogSrc{err: errors.New("gone")}
	g := newRecRig(t, func(_ *recRig, cfg *ReconcilerConfig) {
		cfg.KeepLog = func(ctx context.Context, d int64, c string) error { return l.Keep(ctx, d, c) }
	})
	l = &DeployLogs{Root: filepath.Join(t.TempDir(), "deploy-logs"), DB: g.st, Src: src}
	g.activeDeploy(g.svc, 1, "10.250.0.2", true)
	dead := addDeploy(t, g.st, g.svc, 2, "failed", "10.250.0.3")
	id := g.put(g.svc, dead, 1, "10.250.0.3", true)
	raw := []byte(fmt.Sprintf(`{"service":%d,"deploy":%d,"container":%q,"grace":0}`, g.svc, dead, id))
	res, err := g.runner.cfg.Inv.Invoke(context.Background(), ActionRemoveContainer, raw, actions.Options{})
	if err != nil {
		t.Fatal(err)
	}
	out := res.Out.(RemoveContainerOut)
	if !out.Removed || out.LogKept {
		t.Errorf("%+v", out)
	}
	src.err = nil
	src.set(id, "x")
	id2 := g.put(g.svc, dead, 2, "10.250.0.4", true)
	src.set(id2, "y")
	raw = []byte(fmt.Sprintf(`{"service":%d,"deploy":%d,"container":%q,"grace":0}`, g.svc, dead, id2))
	res, err = g.runner.cfg.Inv.Invoke(context.Background(), ActionRemoveContainer, raw, actions.Options{})
	if err != nil {
		t.Fatal(err)
	}
	if out := res.Out.(RemoveContainerOut); !out.Removed || !out.LogKept {
		t.Errorf("%+v", out)
	}
}

// ---- reads ----

// A hostile log cannot put escape sequences, NUL or bidirectional overrides in what
// deploys.log returns, whatever it holds.
func TestDeploysLogOutputIsSanitised(t *testing.T) {
	g := newLogRig(t)
	d := addDeploy(t, g.st, g.svc, 1, "failed", "10.250.0.2")
	hostile := "ok line\n" +
		"\x1b[31mred\x1b[0m \x1b]0;pwned title\x07 \x1b]8;;http://evil.test\x1b\\link\x1b]8;;\x1b\\\n" +
		"nul\x00byte \u009b31m c1 \u0085 \u202eevil\u202c \u2066iso\u2069 \u200e\u200f\u061c \u202a\u202b\u202d zero\u200bwidth \u2028sep \ufeffbom\n" +
		"bad utf8 \xff\xfe end\r\rcarriage\n"
	g.src.set("c1", hostile)
	if err := g.dl.Keep(context.Background(), d, "c1"); err != nil {
		t.Fatal(err)
	}
	out, err := g.invoke(rigWrite, "deploys.log", m{"service": "p/web", "number": 1})
	if err != nil {
		t.Fatal(err)
	}
	o := out.(DeploysLogOut)
	if !o.Kept || !strings.Contains(o.Text, "ok line\n") || !strings.Contains(o.Text, "red") {
		t.Fatalf("%+v", o)
	}
	for _, r := range o.Text {
		switch {
		case r == '\n' || r == '\t':
		case r < 0x20 || r == 0x7f || (r >= 0x80 && r <= 0x9f): // C0 (ESC, NUL, CR, BEL), DEL, C1 (CSI, OSC, NEL)
			t.Errorf("control character U+%04X survived", r)
		case r >= 0x202a && r <= 0x202e, r >= 0x2066 && r <= 0x2069, r == 0x200e, r == 0x200f, r == 0x061c, r == 0x200b, r == 0x2028, r == 0xfeff:
			t.Errorf("format character U+%04X survived", r)
		}
	}
	if !strings.Contains(o.Text, "\uFFFD") {
		t.Error("invalid UTF-8 was not replaced")
	}
}

func TestDeploysLogNeedsWriteScopeAndMCPNeverGetsIt(t *testing.T) {
	g := newLogRig(t)
	d := addDeploy(t, g.st, g.svc, 1, "failed", "10.250.0.2")
	g.src.set("c1", "the app printed a generated password: hunter2\n")
	if err := g.dl.Keep(context.Background(), d, "c1"); err != nil {
		t.Fatal(err)
	}
	in := m{"service": "p/web", "number": 1}
	if _, err := g.invoke(rigRead, "deploys.log", in); !errors.Is(err, actions.ErrDenied) {
		t.Errorf("a read token: %v", err)
	}
	if _, err := g.invoke(actions.MCPActor("mcp"), "deploys.log", in); !errors.Is(err, actions.ErrDenied) {
		t.Errorf("MCP: %v", err)
	}
	out, err := g.invoke(rigWrite, "deploys.log", in)
	if err != nil || !strings.Contains(out.(DeploysLogOut).Text, "hunter2") {
		t.Errorf("a write token: %v %+v", err, out)
	}
	// The other two reads are read scope, MCP included.
	for _, name := range []string{"deploys.list", "deploys.get"} {
		for _, a := range []actions.Actor{rigRead, actions.MCPActor("mcp")} {
			in := m{"service": "p/web"}
			if name == "deploys.get" {
				in["number"] = 1
			}
			if _, err := g.invoke(a, name, in); err != nil {
				t.Errorf("%s: %v", name, err)
			}
		}
	}
	// The log text never appears in deploys.get or deploys.list.
	for _, name := range []string{"deploys.list", "deploys.get"} {
		in := m{"service": "p/web"}
		if name == "deploys.get" {
			in["number"] = 1
		}
		out, err := g.invoke(rigRead, name, in)
		if err != nil || strings.Contains(fmt.Sprintf("%+v", out), "hunter2") {
			t.Errorf("%s leaks the log: %v", name, err)
		}
	}
}

func TestDeploysListAndGet(t *testing.T) {
	g := newLogRig(t)
	live := g.live(g.svc, 1, "10.250.0.2")
	mustExec(t, g.st, `UPDATE deploys SET committed_at = 'x' WHERE id = ?`, live)
	mustExec(t, g.st, `DROP TRIGGER deploys_snapshot_immutable`)
	mustExec(t, g.st, `UPDATE deploys SET snapshot_json = ? WHERE id = ?`, `{"v":1,"image_ref":"nginx:1"}`, live)
	failed := addDeploy(t, g.st, g.svc, 2, "failed", nil)
	mustExec(t, g.st, `UPDATE deploys SET reason_code = 'health', reason = 'never healthy', app_safe = 'yes' WHERE id = ?`, failed)
	mustExec(t, g.st, `INSERT INTO deploy_stages(deploy_id, stage, state, detail) VALUES (?, 'pull', 'ok', 'done'), (?, 'health', 'failed', 'never healthy')`, failed, failed)
	g.src.set("c1", "x")
	if err := g.dl.Keep(context.Background(), failed, "c1"); err != nil {
		t.Fatal(err)
	}
	out, err := g.invoke(rigRead, "deploys.list", m{"service": "p/web"})
	if err != nil {
		t.Fatal(err)
	}
	l := out.(DeploysListOut)
	if len(l.Deploys) != 2 || l.Deploys[0].Number != 2 || l.Deploys[1].Number != 1 {
		t.Fatalf("%+v", l)
	}
	if a, b := l.Deploys[0], l.Deploys[1]; !a.LogKept || a.Active || a.WasLive || a.Reason != "never healthy" || a.AppSafe != "yes" ||
		b.LogKept || !b.Active || !b.WasLive || b.ImageRef != "nginx:1" {
		t.Errorf("%+v / %+v", a, b)
	}
	if _, err := g.invoke(rigRead, "deploys.list", m{"service": "p/web", "limit": 1000}); !errors.Is(err, actions.ErrInvalidInput) {
		t.Errorf("limit 1000: %v", err)
	}
	if _, err := g.invoke(rigRead, "deploys.list", m{"service": "p/nosuch"}); !errors.Is(err, actions.ErrNotFound) {
		t.Errorf("missing service: %v", err)
	}
	got, err := g.invoke(rigRead, "deploys.get", m{"service": "p/web", "number": 2})
	if err != nil {
		t.Fatal(err)
	}
	o := got.(DeploysGetOut)
	if len(o.Stages) != 2 || o.Stages[0].Stage != "pull" || o.Stages[1].State != "failed" || o.Progress != nil || o.Cancellable || !o.Deploy.LogKept {
		t.Errorf("%+v", o)
	}
	if _, err := g.invoke(rigRead, "deploys.get", m{"service": "p/web", "number": 99}); !errors.Is(err, actions.ErrNotFound) {
		t.Errorf("missing deploy: %v", err)
	}
}

// deploys.get reports the pull's progress while a deploy pulls, and cancellable; the
// entry goes when the deploy ends.
func TestDeploysGetReportsProgressWhilePulling(t *testing.T) {
	g := newLogRig(t)
	inPull, release := make(chan struct{}), make(chan struct{})
	g.ops.pullFn = func(ctx context.Context, _ string, on func(agentops.PullProgress)) (agentops.PullResult, error) {
		on(agentops.PullProgress{Layers: 5, LayersDone: 2, Bytes: 1000, BytesTotal: 5000, Status: "Downloading\x1b[31m"})
		close(inPull)
		select {
		case <-release:
		case <-ctx.Done():
			return agentops.PullResult{}, context.Cause(ctx)
		}
		return agentops.PullResult{ImageID: testImageID}, nil
	}
	g.start(false)
	d, job := g.deploy(g.svc)
	<-inPull
	get := func() DeploysGetOut {
		out, err := g.invoke(rigRead, "deploys.get", m{"service": "p/web", "number": 1})
		if err != nil {
			t.Fatal(err)
		}
		return out.(DeploysGetOut)
	}
	o := get()
	p := o.Progress
	if p == nil || p.Layers != 5 || p.LayersDone != 2 || p.Bytes != 1000 || p.BytesTotal != 5000 || strings.ContainsRune(p.Status, 0x1b) {
		t.Fatalf("progress %+v", p)
	}
	if o.Deploy.Status != StatusRunning || o.Deploy.Stage != StagePull || !o.Cancellable {
		t.Errorf("%+v", o)
	}
	// Once a cancel was asked it is no longer offered.
	mustExec(t, g.st, `UPDATE jobs SET cancel_requested = 1 WHERE id = ?`, job)
	if o := get(); o.Cancellable {
		t.Error("cancellable after a cancel was requested")
	}
	close(release)
	g.wait(job)
	g.until("the progress to go", func() bool { return get().Progress == nil })
	if o := get(); o.Cancellable || o.Deploy.Number != 1 || o.Deploy.Status == StatusRunning {
		t.Errorf("%+v (deploy %d)", o, d)
	}
}

// Cancellable is false in the stages a cancel cannot reach, and for queued deploys it
// is true.
func TestDeploysGetCancellableByStage(t *testing.T) {
	g := newLogRig(t)
	d := addDeploy(t, g.st, g.svc, 1, StatusRunning, nil)
	res, _ := g.st.W.Exec(`INSERT INTO jobs(kind, lock_key, state, actor_kind, actor_id, actor_blob, enqueued_at, payload_json, run_after, timeout_s, created_at)
		VALUES ('deploy', 'service:1', 'running', 'token', '1', '{}', 'x', '{}', 'x', 60, 'x')`)
	job, _ := res.LastInsertId()
	mustExec(t, g.st, `UPDATE deploys SET job_id = ? WHERE id = ?`, job, d)
	for stage, want := range map[string]bool{StagePull: true, StageHealth: true, StageSwitch: false, StageCommit: false, StageDrain: false, StageStopOld: false} {
		mustExec(t, g.st, `UPDATE deploys SET stage = ? WHERE id = ?`, stage, d)
		out, err := g.invoke(rigRead, "deploys.get", m{"service": "p/web", "number": 1})
		if err != nil {
			t.Fatal(err)
		}
		if got := out.(DeploysGetOut).Cancellable; got != want {
			t.Errorf("stage %s: cancellable = %v, want %v", stage, got, want)
		}
	}
}

// ---- delete ----

func TestDeleteRemovesRealDeployLogs(t *testing.T) {
	g := newLogRig(t)
	api := g.service("api")
	g.start(false)
	g.services()
	webDeploy := g.liveVia()
	dApi := addDeploy(t, g.st, api, 1, "failed", nil)
	web := g.svc
	g.src.set("c1", "x")
	for _, p := range [][2]int64{{webDeploy, web}, {dApi, api}} {
		if err := g.dl.Keep(context.Background(), p[0], "c1"); err != nil {
			t.Fatal(err)
		}
	}
	// A leftover temporary file goes too.
	os.WriteFile(filepath.Join(g.root, strconv.FormatInt(web, 10), ".log-1.tmp"), []byte("x"), 0o600)
	sess := g.adminSession("sess-d", time.Now().Add(time.Hour))
	out, err := g.deleteAs(sess, "p/web", "p/web")
	if err != nil {
		t.Fatal(err)
	}
	if s := g.wait(out.Job); s != jobs.StateSucceeded {
		t.Fatalf("%s: %s", s, g.jobError(out.Job))
	}
	if _, err := os.Stat(filepath.Join(g.root, strconv.FormatInt(web, 10))); !os.IsNotExist(err) {
		t.Errorf("the deleted service's log directory is still there: %v", err)
	}
	if !g.dl.Kept(api)[dApi] {
		t.Error("another service's log went with it")
	}
}
