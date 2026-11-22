// Package policy is ServerDock's default-deny rulebook for what may run on a
// Docker host (plan 0004, "Policy check"). It has no state and no background
// work: validators for names, image references, registry hosts, ports and
// health paths; the image check; the resource caps; and CheckCreate, which
// decides whether the exact bytes about to be sent to Docker's create endpoint
// are allowed. The control plane runs CheckCreate at the deploy's check stage
// and the agent runs it again on the bytes it sends, so a bug or a hostile
// caller on either side cannot widen what a container may do.
//
// Everything here treats its input as hostile: error texts never echo a value
// longer than a few cleaned characters, and nothing logs.
package policy

// Resource caps (plan 0004, "Limits"). They are policy, not tuning knobs.
const (
	// PidsLimit is the PID cap on every app container (a fork bomb must not
	// exhaust the host's PIDs). CheckCreate requires exactly this value.
	PidsLimit = 4096
	// MaxServices is the most services one install may have.
	MaxServices = 100
	// MinFreeBytes is the free space image_pull needs on Docker's data root.
	MinFreeBytes = 1 << 30
)

// Options are the install-wide switches the validators need.
type Options struct {
	// AllowLoopbackRegistry permits registries on localhost, 127.0.0.0/8 and
	// ::1 (--allow-loopback-registry: homelab registries and the integration
	// tests). Link-local and metadata addresses are refused regardless.
	AllowLoopbackRegistry bool
}
