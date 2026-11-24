package deploy

import (
	"encoding/json"
	"errors"
	"fmt"

	"github.com/Docker-Hunterpedia/serverdock/internal/agent"
	"github.com/Docker-Hunterpedia/serverdock/internal/jobs"
	"github.com/Docker-Hunterpedia/serverdock/internal/policy"
)

// SnapshotVersion is the schema version of deploys.snapshot_json.
const SnapshotVersion = 1

// SnapHealth is the health check as a deploy snapshot records it.
type SnapHealth struct {
	Kind     string `json:"kind"`
	Path     string `json:"path,omitempty"`
	TimeoutS int    `json:"timeout_s"`
	Passes   int    `json:"passes"`
}

// Snapshot is the immutable record of what one deploy runs (data model, schema
// version 1): the image reference, the container port, the health check, the
// strategy and the timings. The public port is NOT in it: it is a service setting,
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
}

// Settings are the values that decide the job's timeout.
func (s Snapshot) Settings() jobs.Settings {
	return jobs.Settings{Strategy: s.Strategy, HealthTimeoutS: s.Health.TimeoutS, DrainS: s.DrainS, StopGraceS: s.StopGraceS}
}

// Marshal encodes the snapshot.
func (s Snapshot) Marshal() (json.RawMessage, error) {
	if s.V == 0 {
		s.V = SnapshotVersion
	}
	return json.Marshal(s)
}

// Validate checks every field of a snapshot read from the database or about to be
// written: a row the runner cannot trust fails the deploy before anything runs.
func (s Snapshot) Validate() error {
	if s.V != SnapshotVersion {
		return fmt.Errorf("snapshot version %d is not supported", s.V)
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
	if (s.ImageID == "") != (s.RepoDigest == "") && s.ImageID == "" {
		return errors.New("a pinned repo digest needs its image id")
	}
	if s.ImageID != "" && !imageIDShape(s.ImageID) {
		return errors.New("image_id is not a sha256 image id")
	}
	return nil
}

// ParseSnapshot decodes and validates a snapshot (strict: unknown and duplicate
// keys are refused).
func ParseSnapshot(raw string) (Snapshot, error) {
	var s Snapshot
	if err := agent.DecodeArgs(json.RawMessage(raw), &s); err != nil {
		return Snapshot{}, fmt.Errorf("the deploy's snapshot is not valid: %w", err)
	}
	if err := s.Validate(); err != nil {
		return Snapshot{}, fmt.Errorf("the deploy's snapshot is not valid: %w", err)
	}
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
