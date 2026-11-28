package deploy

import (
	"encoding/json"
	"errors"
	"fmt"

	"github.com/Docker-Hunterpedia/serverdock/internal/agent"
	"github.com/Docker-Hunterpedia/serverdock/internal/jobs"
	"github.com/Docker-Hunterpedia/serverdock/internal/policy"
)

// SnapshotVersion is the schema version ParseSnapshot returns and Marshal writes.
// Version 1 (M3a) is still read, as version 2 with nothing set.
const SnapshotVersion = 2

// SnapHealth is the health check as a deploy snapshot records it.
type SnapHealth struct {
	Kind     string `json:"kind"`
	Path     string `json:"path,omitempty"`
	TimeoutS int    `json:"timeout_s"`
	Passes   int    `json:"passes"`
}

// SnapMount is one mount as a snapshot records it. For a volume, Source is the
// short volume name; for a bind, the host path as saved.
type SnapMount struct {
	Kind     string `json:"kind"`
	Source   string `json:"source"`
	Target   string `json:"target"`
	ReadOnly bool   `json:"read_only,omitempty"`
}

// Snapshot is the immutable record of what one deploy runs (data model, schema
// version 2): the image reference, the container port, the health check, the
// effective strategy and the timings, and (new in 2) the env version, mounts and
// limits, frozen at enqueue. The public port is NOT in it: it is a service setting,
// so a rollback never moves the address users reach the app on. ImageID and
// RepoDigest are set for a redeploy or rollback (the pinned image of the source
// deploy); a plain deploy leaves them empty and resolves them at the pull stage.
type Snapshot struct {
	V             int        `json:"v"`
	ImageRef      string     `json:"image_ref"`
	ContainerPort int        `json:"container_port,omitempty"` // 0: a worker with no HTTP entry
	Health        SnapHealth `json:"health"`
	Strategy      string     `json:"strategy"`
	DrainS        int        `json:"drain_s"`
	StopGraceS    int        `json:"stop_grace_s"`
	ImageID       string     `json:"image_id,omitempty"`
	RepoDigest    string     `json:"repo_digest,omitempty"`

	// Version 2. A v1 snapshot has none of these set.

	// EnvVersion and EnvVersionID name the service's env version the deploy runs
	// with; both are zero for a deploy with no variables.
	EnvVersion   int   `json:"env_version,omitempty"`
	EnvVersionID int64 `json:"env_version_id,omitempty"`
	// StrategyForced: the service's setting was blue_green, but a writable mount
	// makes Strategy stop_first.
	StrategyForced  bool        `json:"strategy_forced,omitempty"`
	Mounts          []SnapMount `json:"mounts,omitempty"`
	MemoryMB        int         `json:"memory_mb,omitempty"`
	CPUsMilli       int         `json:"cpus_milli,omitempty"`
	NoNewPrivileges bool        `json:"no_new_privileges,omitempty"`
}

// HasWritableMount reports whether any recorded mount is writable.
func (s Snapshot) HasWritableMount() bool {
	for _, m := range s.Mounts {
		if !m.ReadOnly {
			return true
		}
	}
	return false
}

func (s Snapshot) hasV2Fields() bool {
	return s.EnvVersion != 0 || s.EnvVersionID != 0 || s.StrategyForced || len(s.Mounts) != 0 ||
		s.MemoryMB != 0 || s.CPUsMilli != 0 || s.NoNewPrivileges
}

// Settings are the values that decide the job's timeout.
func (s Snapshot) Settings() jobs.Settings {
	return jobs.Settings{Strategy: s.Strategy, HealthTimeoutS: s.Health.TimeoutS, DrainS: s.DrainS, StopGraceS: s.StopGraceS}
}

// Marshal encodes the snapshot. It writes only the current version: a snapshot
// read as v1 is written as v2.
func (s Snapshot) Marshal() (json.RawMessage, error) {
	s.V = SnapshotVersion
	return json.Marshal(s)
}

// Validate checks every field of a snapshot read from the database or about to be
// written: a row the runner cannot trust fails the deploy before anything runs.
func (s Snapshot) Validate() error {
	if s.V != 1 && s.V != SnapshotVersion {
		return fmt.Errorf("snapshot version %d is not supported", s.V)
	}
	if s.V == 1 && s.hasV2Fields() {
		return errors.New("a version 1 snapshot cannot carry env, mounts or limits")
	}
	if err := s.validateV2(); err != nil {
		return err
	}
	if _, err := policy.ParseRef(s.ImageRef); err != nil {
		return fmt.Errorf("image_ref: %w", err)
	}
	if s.ContainerPort != 0 {
		if err := policy.ValidateContainerPort(s.ContainerPort); err != nil {
			return err
		}
	}
	switch s.Health.Kind {
	case "http":
		if err := policy.ValidateHealthPath(s.Health.Path); err != nil {
			return fmt.Errorf("health.path: %w", err)
		}
		if s.ContainerPort == 0 {
			return errors.New("an http health check needs a container port")
		}
	case "tcp":
		if s.ContainerPort == 0 {
			return errors.New("a tcp health check needs a container port")
		}
	case "running":
	default:
		return errors.New("health.kind must be http, tcp or running")
	}
	if err := policy.ValidateHealthTimeoutS(s.Health.TimeoutS); err != nil {
		return err
	}
	if err := policy.ValidateHealthPasses(s.Health.Passes); err != nil {
		return err
	}
	if err := policy.ValidateDrainS(s.DrainS); err != nil {
		return err
	}
	if err := policy.ValidateStopGraceS(s.StopGraceS); err != nil {
		return err
	}
	if err := s.Settings().Validate(); err != nil {
		return err
	}
	if s.RepoDigest != "" && s.ImageID == "" {
		return errors.New("a pinned repo digest needs its image id")
	}
	if s.ImageID != "" && !imageIDShape(s.ImageID) {
		return errors.New("image_id is not a sha256 image id")
	}
	if s.RepoDigest != "" {
		// A digest the deploy may pull by must name a digest of the SAME repository
		// as the snapshot's reference, never another image.
		r, err := policy.ParseRef(s.RepoDigest)
		if err != nil || r.Digest == "" {
			return errors.New("repo_digest is not a repository with a sha256 digest")
		}
		if !policy.SameRepository(s.ImageRef, s.RepoDigest) {
			return errors.New("repo_digest is not a digest of the image's repository")
		}
	}
	return nil
}

func (s Snapshot) validateV2() error {
	if (s.EnvVersion == 0) != (s.EnvVersionID == 0) || s.EnvVersion < 0 || s.EnvVersionID < 0 {
		return errors.New("env_version and env_version_id are both set or both absent")
	}
	if len(s.Mounts) > policy.MaxMounts {
		return fmt.Errorf("at most %d mounts", policy.MaxMounts)
	}
	targets := map[string]bool{}
	for i, m := range s.Mounts {
		var err error
		switch m.Kind {
		case policy.MountVolume:
			err = policy.ValidateVolumeName(m.Source)
		case policy.MountBind:
			err = policy.ValidateBindSourceShape(m.Source)
		default:
			err = errors.New("kind must be volume or bind")
		}
		if err != nil {
			return fmt.Errorf("mounts[%d]: %w", i, err)
		}
		if err := policy.ValidateMountTarget(m.Target); err != nil {
			return fmt.Errorf("mounts[%d].target: %w", i, err)
		}
		if targets[m.Target] {
			return fmt.Errorf("mounts[%d]: the target %s is used twice", i, m.Target)
		}
		targets[m.Target] = true
	}
	if s.HasWritableMount() && s.Strategy != jobs.StrategyStopFirst {
		return errors.New("a writable mount needs the stop_first strategy")
	}
	if s.StrategyForced && (s.Strategy != jobs.StrategyStopFirst || !s.HasWritableMount()) {
		return errors.New("strategy_forced needs stop_first and a writable mount")
	}
	if s.MemoryMB != 0 {
		if err := policy.ValidateMemoryMB(s.MemoryMB); err != nil {
			return err
		}
	}
	if s.CPUsMilli != 0 {
		if err := policy.ValidateCPUsMilli(s.CPUsMilli); err != nil {
			return err
		}
	}
	return nil
}

// ParseSnapshot decodes and validates a snapshot (strict: unknown and duplicate
// keys are refused). A version 1 snapshot is returned as version 2 with nothing
// new set.
func ParseSnapshot(raw string) (Snapshot, error) {
	var s Snapshot
	if err := agent.DecodeArgs(json.RawMessage(raw), &s); err != nil {
		return Snapshot{}, fmt.Errorf("the deploy's snapshot is not valid: %w", err)
	}
	if err := s.Validate(); err != nil {
		return Snapshot{}, fmt.Errorf("the deploy's snapshot is not valid: %w", err)
	}
	s.V = SnapshotVersion
	return s, nil
}

func imageIDShape(id string) bool {
	if len(id) != len("sha256:")+64 || id[:7] != "sha256:" {
		return false
	}
	for _, c := range id[7:] {
		if (c < '0' || c > '9') && (c < 'a' || c > 'f') {
			return false
		}
	}
	return true
}
