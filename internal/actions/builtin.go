package actions

import (
	"context"
	"database/sql"
	"syscall"
	"time"

	"github.com/Docker-Hunterpedia/serverdock/internal/audit"
	"github.com/Docker-Hunterpedia/serverdock/internal/memstat"
)

// Deps are what the built-in actions need.
type Deps struct {
	Version string
	Started time.Time
	Audit   *audit.Logger
	// Docker reports the engine and negotiated API version (dockerapi.Client).
	Docker interface {
		Info(ctx context.Context) (engine, api string, err error)
	}
	// DB is a reader on the database, for audit.verify.
	DB *sql.DB
}

// None is the input of an action that takes no arguments.
type None struct{}

// Validate implements Validator.
func (None) Validate() error { return nil }

// SystemInfo is the output of system.info.
type SystemInfo struct {
	Version       string  `json:"version"`
	UptimeSeconds int64   `json:"uptime_seconds"`
	RSSBytes      uint64  `json:"rss_bytes"`
	CPUSeconds    float64 `json:"cpu_seconds"`
	Goroutines    int     `json:"goroutines"`
	// AuditHead and AuditRows let an operator (and M7b's state backups) notice a
	// truncated audit log by comparing against an older copy.
	AuditHead string `json:"audit_head"`
	AuditRows int64  `json:"audit_rows"`
}

// DockerInfo is the output of system.docker.
type DockerInfo struct {
	Available bool   `json:"available"`
	Engine    string `json:"engine,omitempty"`
	API       string `json:"api,omitempty"`
	Error     string `json:"error,omitempty"`
}

// AuditVerifyResult is the output of audit.verify.
type AuditVerifyResult struct {
	OK       bool   `json:"ok"`
	BrokenID int64  `json:"broken_id,omitempty"`
	Rows     int64  `json:"rows"`
	Head     string `json:"head,omitempty"`
}

// RegisterBuiltins registers system.info, system.docker and audit.verify. All
// are plain reads: no HTTP route exposes them in M1.
func RegisterBuiltins(r *Registry, d Deps) {
	Register(r, Def[None, SystemInfo]{
		Name: "system.info", Summary: "Version, uptime, own memory and CPU, and the audit log head.",
		Risk: RiskRead, Scope: ScopeRead,
		Execute: func(ctx context.Context, _ Call, _ None) (SystemInfo, error) {
			out := SystemInfo{Version: d.Version, UptimeSeconds: int64(time.Since(d.Started).Seconds())}
			snap := memstat.Read()
			out.RSSBytes, out.Goroutines = snap.Proc.VmRSS, snap.Goroutines
			var ru syscall.Rusage
			if syscall.Getrusage(syscall.RUSAGE_SELF, &ru) == nil {
				out.CPUSeconds = float64(ru.Utime.Sec) + float64(ru.Utime.Usec)/1e6 + float64(ru.Stime.Sec) + float64(ru.Stime.Usec)/1e6
			}
			if d.Audit != nil {
				h, err := d.Audit.Head(ctx)
				if err != nil {
					return out, err
				}
				out.AuditHead, out.AuditRows = h.HashHex(), h.Count
			}
			return out, nil
		},
	})
	Register(r, Def[None, DockerInfo]{
		Name: "system.docker", Summary: "Docker engine version and the negotiated API version.",
		Risk: RiskRead, Scope: ScopeRead,
		Execute: func(ctx context.Context, _ Call, _ None) (DockerInfo, error) {
			if d.Docker == nil {
				return DockerInfo{Error: "no docker client"}, nil
			}
			engine, api, err := d.Docker.Info(ctx)
			if err != nil {
				return DockerInfo{Error: err.Error()}, nil // an unreachable Docker is a result, not a failure
			}
			return DockerInfo{Available: true, Engine: engine, API: api}, nil
		},
	})
	Register(r, Def[None, AuditVerifyResult]{
		Name: "audit.verify", Summary: "Walk the audit chain and report the first broken row.",
		Risk: RiskRead, Scope: ScopeAdmin, // a full scan: admins only
		Execute: func(ctx context.Context, _ Call, _ None) (AuditVerifyResult, error) {
			res, err := audit.Verify(ctx, d.DB)
			if err != nil {
				return AuditVerifyResult{}, err
			}
			out := AuditVerifyResult{OK: res.OK, BrokenID: res.BrokenID, Rows: res.Rows}
			if res.Rows > 0 {
				out.Head = audit.Head{Hash: res.Head, Count: res.Rows}.HashHex()
			}
			return out, nil
		},
	})
}
