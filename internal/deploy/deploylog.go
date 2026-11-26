package deploy

import (
	"bytes"
	"compress/gzip"
	"context"
	"database/sql"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"log/slog"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/Docker-Hunterpedia/serverdock/internal/agent"
	"github.com/Docker-Hunterpedia/serverdock/internal/store"
)

// Deploy logs (plan 0004 step 11).
//
// DATA/deploy-logs/<service id>/<deploy id>.log.gz, mode 0600 in a 0700 directory:
// a header (the deploy, its image, its times), the stage lines, and the tail of the
// container's output (the agent's `logs` op with follow:false, tail:2000), read up
// to LogMaxBytes. It is written when a container of the deploy is about to be
// removed (the candidate of a failed deploy, the previous version after a success,
// an orphan the resync removes), streamed through a pooled gzip writer into a
// temporary file that is renamed into place: no whole-log buffer, and a reader never
// sees a half-written file. The newest LogKeep deploys of a service keep a file;
// older files are deleted when a deploy finishes (no timer).
//
// Container output is sensitive (many images print a generated password at first
// start), so the files are private, deploys.log needs write scope for a token, and
// the text it returns is cleaned (agent.CleanLogText). The directory is not in
// datadir.SecretPaths: that list holds ServerDock's own secrets.

const (
	// LogTail is the number of lines asked of the container's log.
	LogTail = 2000
	// LogMaxBytes caps the uncompressed container output of one deploy log, and what a
	// read returns of the decompressed file.
	LogMaxBytes = 1 << 20
	// LogKeep is how many of a service's newest deploys keep a log.
	LogKeep = 10
	// NoLogMessage is what an older deploy shows.
	NoLogMessage = "Log no longer kept (only the last 10 deploys keep logs)"

	logFileSuffix = ".log.gz"
	logTmpPrefix  = ".log-"
	logTmpSuffix  = ".tmp"
	logTmpMaxAge  = 10 * time.Minute
	agentErrLine  = "serverdock-agent: "
)

// ErrNoLog means the deploy has no log file.
var ErrNoLog = errors.New("deploy: no log file")

// LogSource opens a container's log without following it: *hub.Session's LogsOnce.
type LogSource interface {
	LogsOnce(ctx context.Context, container string, tail int) (io.ReadCloser, error)
}

// DeployLogs keeps and reads the deploy logs.
type DeployLogs struct {
	Root string // DATA/deploy-logs (absolute)
	DB   *store.Store
	Src  LogSource
	Log  *slog.Logger
	Now  func() time.Time
}

var (
	gzPool  = sync.Pool{New: func() any { w, _ := gzip.NewWriterLevel(io.Discard, gzip.BestSpeed); return w }}
	bufPool = sync.Pool{New: func() any { b := make([]byte, 32<<10); return &b }}
)

type logNoteKey struct{}

// withLogNote adds a line to the header of a log kept under ctx (the reason the
// attempt failed: the stage rows do not have it yet when the candidate is removed).
func withLogNote(ctx context.Context, note string) context.Context {
	return context.WithValue(ctx, logNoteKey{}, note)
}

func (l *DeployLogs) now() time.Time {
	if l.Now != nil {
		return l.Now()
	}
	return time.Now()
}

// logFile is <root>/<service id>/<deploy id>.log.gz, validated: ids are positive
// integers and the file is exactly one level under its service's directory.
func logFile(root string, service, deploy int64) (string, error) {
	dir, err := safeLogDir(root, service)
	if err != nil {
		return "", err
	}
	if deploy < 1 {
		return "", errors.New("not a deploy id")
	}
	name := strconv.FormatInt(deploy, 10) + logFileSuffix
	p := filepath.Join(dir, name)
	if filepath.Dir(p) != dir || filepath.Base(p) != name {
		return "", errors.New("the deploy log is outside its directory")
	}
	return p, nil
}

// ensureDir creates the root and the service's directory (0700, whatever the
// umask: a mode is only ever tightened by it, and Chmod fixes a directory made
// earlier by something else) and refuses a link in either place.
func ensureDir(root string, service int64) (string, error) {
	dir, err := safeLogDir(root, service)
	if err != nil {
		return "", err
	}
	if err := os.MkdirAll(root, 0o700); err != nil {
		return "", err
	}
	for _, d := range []string{root, dir} {
		if d == dir {
			if err := os.Mkdir(d, 0o700); err != nil && !errors.Is(err, fs.ErrExist) {
				return "", err
			}
		}
		fi, err := os.Lstat(d)
		if err != nil {
			return "", err
		}
		if !fi.IsDir() { // a symlink or a file in the way: never followed
			return "", errors.New("the deploy-log directory is not a plain directory")
		}
		if fi.Mode().Perm() != 0o700 {
			if err := os.Chmod(d, 0o700); err != nil {
				return "", err
			}
		}
	}
	return dir, nil
}

type deployHead struct {
	service                       int64
	number                        int
	kind, status, stage           string
	project, name                 string
	snapshot                      string
	imageID, digest, reason, code sql.NullString
	created                       string
	started, finished, committed  sql.NullString
}

func (l *DeployLogs) head(ctx context.Context, deploy int64) (deployHead, error) {
	var h deployHead
	err := l.DB.R.QueryRowContext(ctx, `SELECT d.service_id, d.number, d.kind, d.status, COALESCE(d.stage, ''), d.snapshot_json, d.image_id, d.repo_digest,
		d.reason_code, d.reason, d.created_at, d.started_at, d.finished_at, d.committed_at, p.name, s.name
		FROM deploys d JOIN services s ON s.id = d.service_id JOIN projects p ON p.id = s.project_id WHERE d.id = ?`, deploy).
		Scan(&h.service, &h.number, &h.kind, &h.status, &h.stage, &h.snapshot, &h.imageID, &h.digest, &h.code, &h.reason, &h.created, &h.started, &h.finished, &h.committed, &h.project, &h.name)
	return h, err
}

// Keep writes the log of deploy, with the tail of container's output, if the
// deploy is one of its service's newest LogKeep. A failure is returned for the
// caller to report ("log not kept: <reason>"); nothing here can fail a deploy.
func (l *DeployLogs) Keep(ctx context.Context, deploy int64, container string) error {
	if l == nil || l.Root == "" || l.Src == nil || l.DB == nil {
		return errors.New("deploy logs are not kept on this server")
	}
	if !agent.ValidContainerID(container) {
		return errors.New("not a container id")
	}
	h, err := l.head(ctx, deploy)
	if err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return errors.New("the deploy is not in the database")
		}
		return errors.New("the deploy could not be read")
	}
	var recent bool
	if err := l.DB.R.QueryRowContext(ctx, `SELECT EXISTS (SELECT 1 FROM (SELECT id FROM deploys WHERE service_id = ? ORDER BY number DESC LIMIT ?) WHERE id = ?)`,
		h.service, LogKeep, deploy).Scan(&recent); err != nil {
		return errors.New("the service's deploys could not be read")
	}
	if !recent {
		return errors.New("an older deploy: only the last 10 keep logs")
	}
	final, err := logFile(l.Root, h.service, deploy)
	if err != nil {
		return err
	}
	stages, err := l.stageLines(ctx, deploy)
	if err != nil {
		return errors.New("the deploy's stages could not be read")
	}
	// Open the container's log before anything is created: a container that is gone
	// leaves no file.
	src, err := l.Src.LogsOnce(ctx, container, LogTail)
	if err != nil {
		return fmt.Errorf("the container's log could not be read: %s", cleanErr(err))
	}
	defer src.Close()
	buf := bufPool.Get().(*[]byte)
	defer bufPool.Put(buf)
	// The agent reports a failure as one line in the stream: do not keep it as output.
	first, err := io.ReadFull(src, (*buf)[:len(agentErrLine)])
	if err != nil && !errors.Is(err, io.EOF) && !errors.Is(err, io.ErrUnexpectedEOF) {
		return fmt.Errorf("the container's log could not be read: %s", cleanErr(err))
	}
	if bytes.HasPrefix((*buf)[:first], []byte(agentErrLine)) {
		return errors.New("the container's log could not be read: Docker did not return it")
	}
	dir, err := ensureDir(l.Root, h.service)
	if err != nil {
		return fmt.Errorf("the log directory is not usable: %s", cleanErr(err))
	}
	tmp, err := os.CreateTemp(dir, logTmpPrefix+"*"+logTmpSuffix) // mode 0600
	if err != nil {
		return fmt.Errorf("the log file could not be created: %s", cleanErr(err))
	}
	ok := false
	defer func() {
		if !ok {
			tmp.Close()
			os.Remove(tmp.Name())
		}
	}()
	if err := tmp.Chmod(0o600); err != nil {
		return err
	}
	gz := gzPool.Get().(*gzip.Writer)
	gz.Reset(tmp)
	defer gzPool.Put(gz)
	// The whole file is at most LogMaxBytes uncompressed (what a read returns): the
	// output gets what the header and the cut marker leave.
	cw := &countWriter{w: gz}
	l.writeHead(ctx, cw, h, stages)
	fmt.Fprintf(cw, "--- container output (last %d lines, at most %d KiB in all) ---\n", LogTail, LogMaxBytes>>10)
	room := LogMaxBytes - cw.n - cutMarkerRoom
	if room < int64(first) {
		return errors.New("the deploy's header is too large")
	}
	cw.Write((*buf)[:first])
	n, err := io.CopyBuffer(cw, io.LimitReader(src, room-int64(first)), *buf)
	n += int64(first)
	if err != nil {
		return fmt.Errorf("the container's log could not be read: %s", cleanErr(err))
	}
	if n >= room {
		var one [1]byte
		if m, _ := src.Read(one[:]); m > 0 {
			fmt.Fprintf(gz, "\n--- output cut at %d KiB ---\n", LogMaxBytes>>10)
		}
	}
	if err := gz.Close(); err != nil {
		return fmt.Errorf("the log file could not be written: %s", cleanErr(err))
	}
	if err := tmp.Close(); err != nil {
		return fmt.Errorf("the log file could not be written: %s", cleanErr(err))
	}
	if err := os.Rename(tmp.Name(), final); err != nil {
		return fmt.Errorf("the log file could not be put in place: %s", cleanErr(err))
	}
	ok = true
	return nil
}

// cutMarkerRoom is what the marker of a cut output needs.
const cutMarkerRoom = 128

// countWriter counts what is written through it.
type countWriter struct {
	w io.Writer
	n int64
}

func (c *countWriter) Write(p []byte) (int, error) {
	n, err := c.w.Write(p)
	c.n += int64(n)
	return n, err
}

func cleanErr(err error) string {
	return agent.CleanText(err.Error(), 120)
}

func (l *DeployLogs) stageLines(ctx context.Context, deploy int64) ([]string, error) {
	rows, err := l.DB.R.QueryContext(ctx, `SELECT stage, state, started_at, finished_at, detail FROM deploy_stages WHERE deploy_id = ? ORDER BY rowid`, deploy)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []string
	for rows.Next() {
		var stage, state string
		var started, finished, detail sql.NullString
		if err := rows.Scan(&stage, &state, &started, &finished, &detail); err != nil {
			return nil, err
		}
		line := fmt.Sprintf("stage %-12s %-8s", stage, state)
		if started.Valid {
			line += " from " + started.String
		}
		if finished.Valid {
			line += " to " + finished.String
		}
		if detail.String != "" {
			line += ": " + detail.String
		}
		out = append(out, agent.CleanText(line, 400))
	}
	return out, rows.Err()
}

func (l *DeployLogs) writeHead(ctx context.Context, w io.Writer, h deployHead, stages []string) {
	c := func(s string) string { return agent.CleanText(s, 300) }
	image := ""
	if snap, err := ParseSnapshot(h.snapshot); err == nil {
		image = snap.ImageRef
	}
	fmt.Fprintf(w, "ServerDock deploy log\nservice: %s\ndeploy: #%d (%s)\nimage: %s\n", c(h.project+"/"+h.name), h.number, c(h.kind), c(image))
	fmt.Fprintf(w, "image id: %s\ndigest: %s\n", c(h.imageID.String), c(h.digest.String))
	fmt.Fprintf(w, "status when this log was kept: %s %s\n", c(h.status), c(h.stage))
	fmt.Fprintf(w, "created: %s\nstarted: %s\nfinished: %s\nonce live since: %s\nlog kept at: %s\n",
		c(h.created), c(h.started.String), c(h.finished.String), c(h.committed.String), l.now().UTC().Format(time.RFC3339))
	if h.reason.Valid {
		fmt.Fprintf(w, "reason: %s %s\n", c(h.code.String), c(h.reason.String))
	}
	if note, _ := ctx.Value(logNoteKey{}).(string); note != "" {
		fmt.Fprintf(w, "this attempt: %s\n", c(note))
	}
	for _, s := range stages {
		fmt.Fprintln(w, s)
	}
}

// Read returns the log of a deploy as text: the decompressed file read through a
// 1 MiB limit (a hostile file cannot expand in memory), cleaned of control and
// bidirectional characters. truncated is true when the file held more.
func (l *DeployLogs) Read(service, deploy int64) (text string, truncated bool, err error) {
	if l == nil || l.Root == "" {
		return "", false, ErrNoLog
	}
	p, err := logFile(l.Root, service, deploy)
	if err != nil {
		return "", false, err
	}
	dir := filepath.Dir(p)
	if fi, err := os.Lstat(dir); err != nil || !fi.IsDir() {
		return "", false, ErrNoLog
	}
	fi, err := os.Lstat(p)
	if errors.Is(err, fs.ErrNotExist) {
		return "", false, ErrNoLog
	}
	if err != nil || !fi.Mode().IsRegular() {
		return "", false, errors.New("the log file is not readable")
	}
	f, err := os.Open(p)
	if err != nil {
		return "", false, errors.New("the log file is not readable")
	}
	defer f.Close()
	zr, err := gzip.NewReader(io.LimitReader(f, 4*LogMaxBytes))
	if err != nil {
		return "", false, errors.New("the log file is damaged")
	}
	b, err := io.ReadAll(io.LimitReader(zr, LogMaxBytes+1))
	if err != nil && len(b) == 0 {
		return "", false, errors.New("the log file is damaged")
	}
	if len(b) > LogMaxBytes {
		b, truncated = b[:LogMaxBytes], true
	}
	return agent.CleanLogText(string(b), LogMaxBytes), truncated, nil
}

// Kept lists the deploy ids of a service that have a log file.
func (l *DeployLogs) Kept(service int64) map[int64]bool {
	out := map[int64]bool{}
	if l == nil || l.Root == "" {
		return out
	}
	dir, err := safeLogDir(l.Root, service)
	if err != nil {
		return out
	}
	ents, err := os.ReadDir(dir)
	if err != nil {
		return out
	}
	for _, e := range ents {
		if id, ok := logFileID(e.Name()); ok && e.Type().IsRegular() {
			out[id] = true
		}
	}
	return out
}

func logFileID(name string) (int64, bool) {
	s, ok := strings.CutSuffix(name, logFileSuffix)
	if !ok || s == "" || len(s) > 18 {
		return 0, false
	}
	for _, r := range s {
		if r < '0' || r > '9' {
			return 0, false
		}
	}
	id, err := strconv.ParseInt(s, 10, 64)
	return id, err == nil && id > 0
}

// Prune deletes the service's log files of deploys that are not among its newest
// LogKeep (and temporary files a crash left behind). The runner calls it when a
// deploy finishes; nothing runs on a timer.
func (l *DeployLogs) Prune(ctx context.Context, service int64) {
	if l == nil || l.Root == "" || l.DB == nil {
		return
	}
	dir, err := safeLogDir(l.Root, service)
	if err != nil {
		return
	}
	ents, err := os.ReadDir(dir)
	if err != nil {
		return
	}
	var keep map[int64]bool
	for _, e := range ents {
		name := e.Name()
		path := filepath.Join(dir, name)
		if strings.HasPrefix(name, logTmpPrefix) && strings.HasSuffix(name, logTmpSuffix) {
			if fi, err := e.Info(); err == nil && l.now().Sub(fi.ModTime()) > logTmpMaxAge {
				os.Remove(path)
			}
			continue
		}
		id, ok := logFileID(name)
		if !ok {
			continue
		}
		if keep == nil {
			keep = map[int64]bool{}
			rows, err := l.DB.R.QueryContext(ctx, `SELECT id FROM deploys WHERE service_id = ? ORDER BY number DESC LIMIT ?`, service, LogKeep)
			if err != nil {
				l.warn("deploy logs could not be pruned", err)
				return
			}
			for rows.Next() {
				var d int64
				if rows.Scan(&d) == nil {
					keep[d] = true
				}
			}
			err = rows.Err()
			rows.Close()
			if err != nil {
				l.warn("deploy logs could not be pruned", err)
				return
			}
		}
		if !keep[id] {
			if err := os.Remove(path); err != nil && !errors.Is(err, fs.ErrNotExist) {
				l.warn("an old deploy log could not be deleted", err)
			}
		}
	}
}

func (l *DeployLogs) warn(msg string, err error) {
	if l.Log != nil {
		l.Log.Warn(msg, "err", err.Error())
	}
}
