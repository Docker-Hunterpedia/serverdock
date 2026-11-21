package agentops

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/netip"
	"regexp"
	"time"

	"github.com/Docker-Hunterpedia/serverdock/internal/agent"
	"github.com/Docker-Hunterpedia/serverdock/internal/agent/dockerapi"
)

// Caller runs one v1 op on the local agent. *hub.Session implements it.
type Caller interface {
	Call(ctx context.Context, op string, args any, onLine func(agent.Line)) (json.RawMessage, error)
}

// ErrBadReply marks a reply that arrived intact (the protocol layer accepted it)
// but does not decode into the op's result type, or holds a value that cannot
// be right (a short container id, an address that is not an address). The agent
// is untrusted: the control plane never uses such a reply.
var ErrBadReply = errors.New("agentops: malformed reply from the agent")

// Client is the typed control-plane side of the local agent ops. Every reply is
// decoded with agent.DecodeArgs (unknown and duplicate keys refused) and its
// text cleaned before it is returned.
type Client struct{ c Caller }

// NewClient returns a Client over c.
func NewClient(c Caller) *Client { return &Client{c: c} }

// IsCode reports whether err is a failed result with the given protocol code.
func IsCode(err error, code string) bool {
	var oe *agent.OpError
	return errors.As(err, &oe) && oe.Code == code
}

func bad(op string, why string) error { return fmt.Errorf("%w: %s: %s", ErrBadReply, op, why) }

// call runs op and decodes its result into dst.
func (c *Client) call(ctx context.Context, op string, args, dst any, onLine func(agent.Line)) error {
	raw, err := c.c.Call(ctx, op, args, onLine)
	if err != nil {
		return err
	}
	if err := agent.DecodeArgs(raw, dst); err != nil {
		return bad(op, err.Error())
	}
	return nil
}

var (
	fullIDRe  = regexp.MustCompile(`^[0-9a-f]{64}$`)
	stateWord = regexp.MustCompile(`^[a-z]{0,16}$`)
)

func okID(id string) bool      { return fullIDRe.MatchString(id) }
func okImageID(id string) bool { return imageIDRe.MatchString(id) }

// okAddr accepts "" or a canonical IPv4 address.
func okAddr(s string) bool {
	if s == "" {
		return true
	}
	a, err := netip.ParseAddr(s)
	return err == nil && a.Is4() && a.String() == s
}

// ImagePull pulls ref and returns the image id (from an inspect the agent made
// after the pull). stall 0 means the agent's default. onProgress (may be nil) is
// called for each forwarded progress line, at most two a second.
func (c *Client) ImagePull(ctx context.Context, ref string, stall time.Duration, onProgress func(PullProgress)) (PullResult, error) {
	stallS := 0
	if stall > 0 {
		stallS = int((stall + time.Second - 1) / time.Second) // round up: never 0 for a short stall
	}
	args := PullArgs{Ref: ref, StallS: stallS}
	var r PullResult
	err := c.call(ctx, agent.OpImagePull, args, &r, func(l agent.Line) {
		if l.T != agent.LineProgress || onProgress == nil {
			return
		}
		var p PullProgress
		if agent.DecodeArgs(l.P, &p) != nil {
			return // a bad progress line is cosmetic: drop it
		}
		p.Status = agent.CleanText(p.Status, agent.MaxDetailText)
		onProgress(p)
	})
	if err != nil {
		return PullResult{}, err
	}
	if !okImageID(r.ImageID) {
		return PullResult{}, bad(agent.OpImagePull, "image id")
	}
	r.RepoDigest = agent.CleanText(r.RepoDigest, 256)
	return r, nil
}

// ImageInspect inspects an image by reference or, with byID, by id.
func (c *Client) ImageInspect(ctx context.Context, refOrID string, byID bool) (ImageInfo, error) {
	args := ImageInspectArgs{Ref: refOrID}
	if byID {
		args = ImageInspectArgs{ImageID: refOrID}
	}
	var r ImageInfo
	if err := c.call(ctx, agent.OpImageInspect, args, &r, nil); err != nil {
		return ImageInfo{}, err
	}
	if !okImageID(r.ID) || len(r.RepoDigests) > maxRepoDigests || len(r.ExposedPorts) > maxImageLists ||
		len(r.Volumes) > maxImageLists || len(r.ReservedLabelKeys) > maxReserved {
		return ImageInfo{}, bad(agent.OpImageInspect, "image info")
	}
	r.RepoDigests = cleanAll(r.RepoDigests, 256)
	r.ExposedPorts = cleanAll(r.ExposedPorts, 256)
	r.Volumes = cleanAll(r.Volumes, 256)
	r.ReservedLabelKeys = cleanAll(r.ReservedLabelKeys, 256)
	return r, nil
}

func cleanAll(in []string, max int) []string {
	out := make([]string, len(in))
	for i, s := range in {
		out[i] = agent.CleanText(s, max)
	}
	return out
}

// ContainerCreate creates a container. spec must carry the five caller labels
// only: the agent adds serverdock.instance itself and refuses a request that
// carries it.
func (c *Client) ContainerCreate(ctx context.Context, name string, spec dockerapi.CreateSpec, ip, imageID string) (CreateResult, error) {
	var r CreateResult
	if err := c.call(ctx, agent.OpContainerCreate, CreateArgs{Name: name, Spec: spec, IP: ip, ImageID: imageID}, &r, nil); err != nil {
		return CreateResult{}, err
	}
	if !okID(r.ID) {
		return CreateResult{}, bad(agent.OpContainerCreate, "container id")
	}
	return r, nil
}

func (c *Client) idOp(ctx context.Context, op, id string, args any) error {
	if !okID(id) {
		return errors.New("agentops: id must be a full 64-character hex container id")
	}
	var r Empty
	return c.call(ctx, op, args, &r, nil)
}

// ContainerStart starts a container (already running is success).
func (c *Client) ContainerStart(ctx context.Context, id string) error {
	return c.idOp(ctx, agent.OpContainerStart, id, IDArgs{ID: id})
}

// ContainerRestart restarts a container with a stop grace of grace seconds.
func (c *Client) ContainerRestart(ctx context.Context, id string, grace int) error {
	return c.idOp(ctx, agent.OpContainerRestart, id, GraceArgs{ID: id, Grace: grace})
}

// ContainerStop stops a container with a grace of grace seconds.
func (c *Client) ContainerStop(ctx context.Context, id string, grace int) error {
	return c.idOp(ctx, agent.OpContainerStop, id, GraceArgs{ID: id, Grace: grace})
}

// ContainerRemove removes a stopped container and its anonymous volumes.
func (c *Client) ContainerRemove(ctx context.Context, id string) error {
	return c.idOp(ctx, agent.OpContainerRemove, id, IDArgs{ID: id})
}

// ContainerInspect inspects a container by full id.
func (c *Client) ContainerInspect(ctx context.Context, id string) (ContainerState, error) {
	if !okID(id) {
		return ContainerState{}, errors.New("agentops: id must be a full 64-character hex container id")
	}
	var r ContainerState
	if err := c.call(ctx, agent.OpContainerInspect, IDArgs{ID: id}, &r, nil); err != nil {
		return ContainerState{}, err
	}
	if r.ID != id || !stateWord.MatchString(r.State) || !okAddr(r.IP) || !okAddr(r.ConfiguredIP) ||
		(r.ImageID != "" && !okImageID(r.ImageID)) || len(r.Labels) > 16 {
		return ContainerState{}, bad(agent.OpContainerInspect, "container state")
	}
	r.Labels = cleanLabels(r.Labels)
	r.StartedAt = agent.CleanText(r.StartedAt, 40)
	return r, nil
}

func cleanLabels(in map[string]string) map[string]string {
	out := make(map[string]string, len(in))
	for k, v := range in {
		out[agent.CleanText(k, 64)] = agent.CleanText(v, 256)
	}
	return out
}

// ContainerList lists this install's containers in every state.
func (c *Client) ContainerList(ctx context.Context) (ListResult, error) {
	var r ListResult
	if err := c.call(ctx, agent.OpContainerList, nil, &r, nil); err != nil {
		return ListResult{}, err
	}
	if len(r.Containers) > MaxList {
		return ListResult{}, bad(agent.OpContainerList, "too many entries") // never a partial list
	}
	for i := range r.Containers {
		e := &r.Containers[i]
		if !okID(e.ID) || !okAddr(e.IP) || !okAddr(e.ConfiguredIP) || (e.ImageID != "" && !okImageID(e.ImageID)) {
			return ListResult{}, bad(agent.OpContainerList, "container entry")
		}
		e.Name = agent.CleanText(e.Name, 128)
		e.State = agent.CleanText(e.State, 16)
		e.Labels = ListedLabels{
			Project: agent.CleanText(e.Labels.Project, 64), Service: agent.CleanText(e.Labels.Service, 64),
			Deploy: agent.CleanText(e.Labels.Deploy, 64), Attempt: agent.CleanText(e.Labels.Attempt, 64),
		}
	}
	return r, nil
}

// ContainerPorts lists every container that publishes a host port.
func (c *Client) ContainerPorts(ctx context.Context) (PortsResult, error) {
	var r PortsResult
	if err := c.call(ctx, agent.OpContainerPorts, nil, &r, nil); err != nil {
		return PortsResult{}, err
	}
	if len(r.Containers) > MaxList {
		return PortsResult{}, bad(agent.OpContainerPorts, "too many entries")
	}
	for i := range r.Containers {
		e := &r.Containers[i]
		if len(e.Ports) > MaxPortsEach || (e.PortsTruncated && len(e.Ports) == 0) {
			return PortsResult{}, bad(agent.OpContainerPorts, "too many ports")
		}
		e.Name = agent.CleanText(e.Name, MaxNameText)
		for j := range e.Ports {
			p := &e.Ports[j]
			p.HostIP, p.Proto = agent.CleanText(p.HostIP, 45), agent.CleanText(p.Proto, 8)
		}
	}
	return r, nil
}

// NetworkEnsure creates the app network if it is missing (idempotent).
func (c *Client) NetworkEnsure(ctx context.Context, name, subnet string) (NetworkResult, error) {
	var r NetworkResult
	if err := c.call(ctx, agent.OpNetworkEnsure, NetworkArgs{Name: name, Subnet: subnet}, &r, nil); err != nil {
		return NetworkResult{}, err
	}
	if !okAddr(r.Gateway) || r.Gateway == "" {
		return NetworkResult{}, bad(agent.OpNetworkEnsure, "gateway")
	}
	if _, err := netip.ParsePrefix(r.Subnet); err != nil {
		return NetworkResult{}, bad(agent.OpNetworkEnsure, "subnet")
	}
	if r.Subnet != subnet {
		return NetworkResult{}, bad(agent.OpNetworkEnsure, "subnet is not the one asked for")
	}
	r.ID = agent.CleanText(r.ID, 80)
	return r, nil
}

// NetworkInspect returns the addresses in use on the app network.
func (c *Client) NetworkInspect(ctx context.Context) (UsedAddrs, error) {
	var r UsedAddrs
	if err := c.call(ctx, agent.OpNetworkInspect, nil, &r, nil); err != nil {
		return UsedAddrs{}, err
	}
	if len(r.Used) > MaxUsedAddrs || r.Gateway == "" || !okAddr(r.Gateway) {
		return UsedAddrs{}, bad(agent.OpNetworkInspect, "addresses")
	}
	for _, a := range r.Used {
		if a == "" || !okAddr(a) {
			return UsedAddrs{}, bad(agent.OpNetworkInspect, "address")
		}
	}
	if _, err := netip.ParsePrefix(r.Subnet); err != nil {
		return UsedAddrs{}, bad(agent.OpNetworkInspect, "subnet")
	}
	if r.IPRange != "" {
		if _, err := netip.ParsePrefix(r.IPRange); err != nil {
			return UsedAddrs{}, bad(agent.OpNetworkInspect, "ip range")
		}
	}
	return r, nil
}

// HealthCheck probes target (ip:port) once. A failed probe is a result with
// OK false, not an error; an error means the op itself failed or was refused.
func (c *Client) HealthCheck(ctx context.Context, a HealthArgs) (HealthResult, error) {
	var r HealthResult
	if err := c.call(ctx, agent.OpHealthCheck, a, &r, nil); err != nil {
		return HealthResult{}, err
	}
	if r.Status < 0 || r.Status > 999 || r.MS < 0 {
		return HealthResult{}, bad(agent.OpHealthCheck, "result")
	}
	r.Err = agent.CleanText(r.Err, 128)
	return r, nil
}

// Events streams this install's container events to fn until ctx ends, the
// stream ends (nil: the caller resyncs and reconnects) or fn returns an error
// (which stops the stream and is returned). onReady (may be nil) is called once
// when Docker's stream is open, so a caller can list state without missing an
// event. A line that does not decode, or
// whose id is not a full id, is skipped.
func (c *Client) Events(ctx context.Context, onReady func(), fn func(Event) error) error {
	ctx, cancel := context.WithCancel(ctx)
	defer cancel()
	var fnErr error
	var r Empty
	ready := false
	err := c.call(ctx, agent.OpEvents, nil, &r, func(l agent.Line) {
		if l.T == agent.LineProgress && !ready {
			ready = true // Docker's stream is open: nothing after this is missed
			if onReady != nil {
				onReady()
			}
			return
		}
		if l.T != agent.LineEvent || fnErr != nil {
			return
		}
		var e Event
		if agent.DecodeArgs(l.E, &e) != nil || !okID(e.ID) {
			return
		}
		e.Action, e.Service = agent.CleanText(e.Action, 64), agent.CleanText(e.Service, 64)
		e.Deploy, e.ExitCode = agent.CleanText(e.Deploy, 64), agent.CleanText(e.ExitCode, 16)
		if fnErr = fn(e); fnErr != nil {
			cancel()
		}
	})
	if fnErr != nil {
		return fnErr
	}
	return err
}
