// Package agentops holds the local-only agent ops of plan 0004 (ADR 0012) on
// both sides of the op protocol: the typed request and result structs shared by
// both, the agent-side handlers (Handlers, built over the Docker client), and
// the control plane's typed client (Client, over hub.Session.Call).
//
// Every request is decoded with agent.DecodeArgs (strict keys, no duplicates,
// no trailing bytes); every reply is decoded the same way and its text cleaned
// with agent.CleanText before anything else sees it.
package agentops

import (
	"github.com/Docker-Hunterpedia/serverdock/internal/agent/dockerapi"
	"github.com/Docker-Hunterpedia/serverdock/internal/policy"
)

// Limits shared by the handlers and the client.
const (
	// MinFreeBytes is the free space image_pull needs on Docker's data root.
	MinFreeBytes = policy.MinFreeBytes
	// DefaultStallS is the image_pull stall limit when the request leaves it out.
	DefaultStallS = 120
	MaxStallS     = 600

	MaxGraceS = 300 // longest stop grace (data model: stop_grace_s 0..300)

	MaxList       = 1000 // container_list and container_ports entries
	MaxUsedAddrs  = 1024 // network_inspect addresses
	MaxPortsEach  = 4    // published ports kept per container in container_ports
	MaxNameText   = 64   // container name in container_ports
	MaxHealthMS   = 2000 // health_check timeout
	healthReadCap = 4 << 10
)

// Our label keys.
const (
	LabelManaged  = policy.LabelManaged
	LabelInstance = policy.LabelInstance
	LabelProject  = policy.LabelProject
	LabelService  = policy.LabelService
	LabelDeploy   = policy.LabelDeploy
	LabelAttempt  = policy.LabelAttempt
)

// CreateExpect is what the agent hands the create check (see policy).
type CreateExpect = policy.CreateExpect

// CheckCreateFunc is the policy hook: it gets the exact bytes about to be sent
// to Docker and returns nil only if every part of the body is allowed. The
// server plugs policy.CheckCreateAgent in here. There is deliberately no "allow
// all" implementation: Deps.CheckCreate is required.
type CheckCreateFunc func(body []byte, exp CreateExpect) error

// callerLabels are the five keys a caller may send; the agent adds the sixth
// (LabelInstance) itself.
var callerLabels = []string{LabelManaged, LabelProject, LabelService, LabelDeploy, LabelAttempt}

// PullArgs is the image_pull request. StallS 0 means DefaultStallS.
type PullArgs struct {
	Ref    string `json:"ref"`
	StallS int    `json:"stall_s,omitempty"`
}

// PullProgress is one image_pull progress line.
type PullProgress struct {
	Layers     int    `json:"layers"`
	LayersDone int    `json:"layers_done"`
	Bytes      int64  `json:"bytes"`
	BytesTotal int64  `json:"bytes_total"`
	Status     string `json:"status"`
}

// PullResult is the image_pull result. The image id comes from an inspect made
// after the pull, so the image is known to be there.
type PullResult struct {
	ImageID    string `json:"image_id"`
	RepoDigest string `json:"repo_digest,omitempty"`
}

// ImageInspectArgs names the image by exactly one of Ref or ImageID.
type ImageInspectArgs struct {
	Ref     string `json:"ref,omitempty"`
	ImageID string `json:"image_id,omitempty"`
}

// Caps on the image_inspect result lists.
const (
	maxRepoDigests = 8
	maxImageLists  = 32
	maxReserved    = 8
)

// ImageInfo is the image_inspect result. It never carries label values.
type ImageInfo struct {
	ID                string   `json:"id"`
	RepoDigests       []string `json:"repo_digests"`
	ExposedPorts      []string `json:"exposed_ports"`
	Volumes           []string `json:"volumes"`
	ReservedLabelKeys []string `json:"reserved_label_keys"`
}

// CreateArgs is the container_create request: the typed spec (Labels carry the
// five caller keys, never serverdock.instance), the expected static address and
// the expected image id (the one the control plane inspected).
type CreateArgs struct {
	Name    string               `json:"name"`
	Spec    dockerapi.CreateSpec `json:"spec"`
	IP      string               `json:"ip"`
	ImageID string               `json:"image_id"`
}

// CreateResult is the container_create result. Existed is true when Docker
// answered 409 and the container with that name is ours, for this very deploy
// attempt and image.
type CreateResult struct {
	ID      string `json:"id"`
	Existed bool   `json:"existed"`
}

// IDArgs names a container by its full 64-hex id.
type IDArgs struct {
	ID string `json:"id"`
}

// GraceArgs is IDArgs plus a stop grace in seconds (0..MaxGraceS).
type GraceArgs struct {
	ID    string `json:"id"`
	Grace int    `json:"grace"`
}

// Empty is a result with no fields.
type Empty struct{}

// ContainerState is the container_inspect result. Labels are ours only.
type ContainerState struct {
	ID           string            `json:"id"`
	State        string            `json:"state"`
	Running      bool              `json:"running"`
	Restarting   bool              `json:"restarting"`
	ExitCode     int               `json:"exit_code"`
	OOMKilled    bool              `json:"oom_killed"`
	RestartCount int               `json:"restart_count"`
	IP           string            `json:"ip"`
	ConfiguredIP string            `json:"configured_ip,omitempty"`
	ImageID      string            `json:"image_id"`
	StartedAt    string            `json:"started_at,omitempty"`
	Labels       map[string]string `json:"labels"`
	// Owned is true when the container passes the ownership rule: exactly our
	// six labels and nothing dropped.
	Owned bool `json:"owned"`
}

// ListedLabels are the four labels of a listed container that vary.
type ListedLabels struct {
	Project string `json:"project"`
	Service string `json:"service"`
	Deploy  string `json:"deploy"`
	Attempt string `json:"attempt"`
}

// ListedContainer is one container_list entry (a container with our instance
// label, in any state). Unowned is true when it fails the ownership rule (an
// extra or odd serverdock.* label): the agent will not stop or remove it, and
// the allocator still counts its address as held.
type ListedContainer struct {
	ID           string       `json:"id"`
	Name         string       `json:"name"`
	ImageID      string       `json:"image_id"`
	State        string       `json:"state"`
	IP           string       `json:"ip,omitempty"`
	ConfiguredIP string       `json:"configured_ip,omitempty"`
	Labels       ListedLabels `json:"labels"`
	Unowned      bool         `json:"unowned,omitempty"`
}

// ListResult is the container_list result. It is always the complete set: the
// op fails (conflict) rather than return a partial list, because the allocator
// and the orphan sweep must never act on a list that is missing containers.
type ListResult struct {
	Containers []ListedContainer `json:"containers"`
}

// PublishedPort is one host port a container publishes.
type PublishedPort struct {
	HostIP        string `json:"host_ip"`
	HostPort      int    `json:"host_port"`
	ContainerPort int    `json:"container_port"`
	Proto         string `json:"proto"`
}

// PublishingContainer is a container that publishes host ports.
type PublishingContainer struct {
	Name  string          `json:"name"`
	Ports []PublishedPort `json:"ports"`
	// PortsTruncated is true when the container publishes more distinct ports
	// than MaxPortsEach (all-interfaces ports are listed first).
	PortsTruncated bool `json:"ports_truncated,omitempty"`
}

// PortsResult is the container_ports result. Truncated means more containers
// than fit: the banner says "and more".
type PortsResult struct {
	Containers []PublishingContainer `json:"containers"`
	Truncated  bool                  `json:"truncated,omitempty"`
}

// NetworkArgs is the network_ensure request. The agent manages one network, the
// one named after its install id; any other name is refused.
type NetworkArgs struct {
	Name   string `json:"name"`
	Subnet string `json:"subnet"`
}

// NetworkResult is the network_ensure result.
type NetworkResult struct {
	ID      string `json:"id"`
	Subnet  string `json:"subnet"`
	Gateway string `json:"gateway"`
}

// UsedAddrs is the network_inspect result: the addresses of the endpoints
// connected to our network now (a stopped container is absent) and the network's
// shape.
type UsedAddrs struct {
	Subnet  string   `json:"subnet"`
	Gateway string   `json:"gateway"`
	IPRange string   `json:"ip_range"`
	Used    []string `json:"used"`
}

// Event is one events line.
type Event struct {
	Action   string `json:"action"`
	ID       string `json:"id"`
	Service  string `json:"service"`
	Deploy   string `json:"deploy"`
	ExitCode string `json:"exit_code"`
	Time     int64  `json:"time"`
}

// EventsOpen is the one progress line the events op sends once Docker's stream
// is open: from then on no event can be missed.
type EventsOpen = Empty

// Health check kinds.
const (
	KindHTTP = "http"
	KindTCP  = "tcp"
)

// HealthArgs is the health_check request. TimeoutMS 0 means MaxHealthMS.
type HealthArgs struct {
	Target    string `json:"target"`
	Kind      string `json:"kind"`
	Path      string `json:"path,omitempty"`
	TimeoutMS int    `json:"timeout_ms,omitempty"`
}

// HealthResult is the health_check result. OK is true for a TCP connect, or an
// HTTP status of 200 to 399 (redirects are not followed). Err is a short,
// cleaned reason when OK is false and no status was read.
type HealthResult struct {
	OK     bool   `json:"ok"`
	Status int    `json:"status,omitempty"`
	MS     int    `json:"ms"`
	Err    string `json:"err,omitempty"`
}

// Route pages (the static answers of a route with no upstream).
const (
	PageNotRunning  = "not_running"
	PageMaintenance = "maintenance"
)

// MaxRoutes is the most app routes one proxy_apply carries (the service cap).
const MaxRoutes = 100

// Route is one exposed app: plain HTTP on Port, proxied to Upstream ("ip:port"
// on the app network) or answered with a static Page. Exactly one of the two.
type Route struct {
	Port     int    `json:"port"`
	Upstream string `json:"upstream,omitempty"`
	Page     string `json:"page,omitempty"`
}

// ApplyArgs is the proxy_apply request: the complete route list.
type ApplyArgs struct {
	Routes []Route `json:"routes"`
}

// ApplyResult is the proxy_apply result. FailedPorts are the ports Caddy could
// not bind (another program has them); every other route is live.
type ApplyResult struct {
	Applied     bool  `json:"applied"`
	FailedPorts []int `json:"failed_ports"`
}
