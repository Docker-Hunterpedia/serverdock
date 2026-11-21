package agentops

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net"
	"net/http"
	"net/netip"
	"regexp"
	"sort"
	"strings"
	"sync"
	"syscall"
	"time"

	"github.com/Docker-Hunterpedia/serverdock/internal/agent"
	"github.com/Docker-Hunterpedia/serverdock/internal/agent/dockerapi"
)

// Deps is what the agent-side handlers need. Docker, Instance and CheckCreate
// are required: there is no default policy, and a nil CheckCreate is refused at
// construction rather than treated as "allow".
type Deps struct {
	Docker *dockerapi.Client
	// Instance is this install's id (lowercase hex). The agent sets
	// serverdock.instance to it itself and derives the app network's name from
	// it; the caller never names either.
	Instance string
	// CheckCreate runs on the exact bytes sent to Docker for container_create.
	CheckCreate CheckCreateFunc
	// Log receives internal errors that are not sent to the control plane.
	Log *slog.Logger

	// Test hooks (zero in production).
	freeBytes     func(path string) (uint64, error)
	progressEvery time.Duration
	dial          func(ctx context.Context, network, addr string) (net.Conn, error)
}

var instanceRe = sync.OnceValue(func() *regexp.Regexp { return regexp.MustCompile(`^[0-9a-f]{16,64}$`) })

// NetworkName is the app network's name for an install id.
func NetworkName(instance string) string { return "serverdock-" + instance[:8] }

// containerName is the name every container ServerDock creates carries:
// sd-<project>-<service>-d<deploy id>-a<attempt>.
// Project and service segments start with a letter, never end in "-" and never
// contain "--". The deploy and attempt must equal the labels (checked in create).
var containerName = sync.OnceValue(func() *regexp.Regexp {
	return regexp.MustCompile(`^sd-([a-z][a-z0-9]*(?:-[a-z0-9]+)*)-([a-z][a-z0-9]*(?:-[a-z0-9]+)*)-d([0-9]{1,18})-a([0-9]{1,9})$`)
})

// maxContainerName is 3 + 30 + 1 + 30 + 1 + 19 + 1 + 10 plus the "sd-" prefix slack.
const maxContainerName = 96

var imageIDRe = sync.OnceValue(func() *regexp.Regexp { return regexp.MustCompile(`^sha256:[0-9a-f]{64}$`) })

// defaultProgressEvery is the minimum gap between forwarded pull progress
// lines: two a second.
const defaultProgressEvery = 500 * time.Millisecond

type handlers struct {
	Deps
	network string
	labels  []string // the label filter that selects this install's containers
}

// Handlers builds the agent-side handlers of the local-only ops (everything in
// the op table that is not RemoteOK, except proxy_apply, which step 6 adds),
// keyed by op name, for agent.Config.Ops.
func Handlers(d Deps) (map[string]agent.Handler, error) {
	switch {
	case d.Docker == nil:
		return nil, errors.New("agentops: a Docker client is required")
	case !instanceRe().MatchString(d.Instance):
		return nil, errors.New("agentops: the install id must be 16 to 64 lowercase hex characters")
	case d.CheckCreate == nil:
		return nil, errors.New("agentops: a CheckCreate policy is required")
	}
	if d.freeBytes == nil {
		d.freeBytes = freeBytes
	}
	if d.progressEvery <= 0 {
		d.progressEvery = defaultProgressEvery
	}
	if d.dial == nil {
		d.dial = func(ctx context.Context, network, addr string) (net.Conn, error) {
			return (&net.Dialer{}).DialContext(ctx, network, addr)
		}
	}
	if d.Log == nil {
		d.Log = slog.New(slog.DiscardHandler)
	}
	h := &handlers{Deps: d, network: NetworkName(d.Instance), labels: []string{LabelInstance + "=" + d.Instance}}
	return map[string]agent.Handler{
		agent.OpImagePull:        h.pull,
		agent.OpImageInspect:     h.imageInspect,
		agent.OpContainerCreate:  h.create,
		agent.OpContainerStart:   h.start,
		agent.OpContainerRestart: h.restart,
		agent.OpContainerInspect: h.inspect,
		agent.OpContainerStop:    h.stop,
		agent.OpContainerRemove:  h.remove,
		agent.OpContainerList:    h.list,
		agent.OpContainerPorts:   h.ports,
		agent.OpNetworkEnsure:    h.networkEnsure,
		agent.OpNetworkInspect:   h.networkInspect,
		agent.OpEvents:           h.events,
		agent.OpHealthCheck:      h.health,
	}, nil
}

// opErr builds a protocol error.
func opErr(code, format string, a ...any) *agent.OpError {
	return &agent.OpError{Code: code, Msg: fmt.Sprintf(format, a...)}
}

// fail maps an error from the Docker client or the context to a protocol error.
// Docker's own message (already cleaned and capped) is kept for an API error;
// anything else (a dial error naming the socket, say) is logged here and sent
// as a generic line.
func (h *handlers) fail(op string, err error) error {
	var oe *agent.OpError
	switch {
	case errors.As(err, &oe):
		return oe
	case errors.Is(err, dockerapi.ErrDiskFull):
		return opErr(agent.CodeDiskFull, "no space left on device")
	case errors.Is(err, dockerapi.ErrNotFound):
		return opErr(agent.CodeNotFound, "not found")
	case errors.Is(err, dockerapi.ErrConflict),
		errors.Is(err, dockerapi.ErrForeignNetwork), errors.Is(err, dockerapi.ErrSubnetMismatch),
		errors.Is(err, dockerapi.ErrNetworkSettings), errors.Is(err, dockerapi.ErrSubnetOverlap):
		return opErr(agent.CodeConflict, "%s", strings.TrimPrefix(err.Error(), "dockerapi: "))
	case errors.Is(err, context.DeadlineExceeded):
		return opErr(agent.CodeTimeout, "timed out")
	case errors.Is(err, context.Canceled):
		return opErr(agent.CodeCancelled, "cancelled")
	}
	var pe *dockerapi.PullError
	if errors.As(err, &pe) {
		return opErr(agent.CodeDocker, "%s", pe.Msg)
	}
	var ae *dockerapi.APIError
	if errors.As(err, &ae) {
		return opErr(agent.CodeDocker, "%s", strings.TrimPrefix(ae.Error(), "dockerapi: "))
	}
	h.Log.Warn("docker call failed", "op", op, "err", err)
	return opErr(agent.CodeDocker, "the Docker request failed")
}

func decode(raw json.RawMessage, dst any) error {
	if err := agent.DecodeArgs(raw, dst); err != nil {
		return opErr(agent.CodeInvalid, "%v", err)
	}
	return nil
}

// ownedLabels is the ownership rule: exactly our six labels, none dropped, with
// the expected shape and this install's instance id.
func ownedLabels(labels map[string]string, dropped bool, instance string) bool {
	if dropped || len(labels) != 6 || labels[LabelManaged] != "1" || labels[LabelInstance] != instance {
		return false
	}
	for _, k := range callerLabels[1:] {
		if !labelValueRe().MatchString(labels[k]) {
			return false
		}
	}
	return true
}

// owned inspects a container named by its full id and returns it if it is ours.
// Every op that changes a container goes through it and then acts on the
// inspected id (info.ID), never on the caller's string.
func (h *handlers) owned(ctx context.Context, op, id string) (dockerapi.ContainerInfo, error) {
	if !dockerapi.ValidFullID(id) {
		return dockerapi.ContainerInfo{}, opErr(agent.CodeInvalid, "id must be a full 64-character hex container id")
	}
	info, err := h.Docker.ContainerInspect(ctx, id)
	if err != nil {
		return dockerapi.ContainerInfo{}, h.fail(op, err)
	}
	if info.ID != id {
		return dockerapi.ContainerInfo{}, opErr(agent.CodeInvalid, "id must be a full 64-character hex container id")
	}
	if !ownedLabels(info.Labels, info.LabelsDropped, h.Instance) {
		return dockerapi.ContainerInfo{}, opErr(agent.CodeRefusedUnowned, "this container is not owned by this ServerDock install")
	}
	return info, nil
}

// ---- images ----

func (h *handlers) pull(ctx context.Context, raw json.RawMessage, out *agent.Out) (any, error) {
	var a PullArgs
	if err := decode(raw, &a); err != nil {
		return nil, err
	}
	if !dockerapi.ValidPullRef(a.Ref) {
		return nil, opErr(agent.CodeInvalid, "ref must name one image by tag or sha256 digest")
	}
	if a.StallS == 0 {
		a.StallS = DefaultStallS
	}
	if a.StallS < 1 || a.StallS > MaxStallS {
		return nil, opErr(agent.CodeInvalid, "stall_s must be 1..%d", MaxStallS)
	}
	stall := time.Duration(a.StallS) * time.Second

	// Free space first: one statfs of Docker's data root. Unreadable (ServerDock
	// in a container, say) is not a reason to refuse; say so and go on.
	root, err := h.Docker.DataRoot(ctx)
	if err != nil {
		return nil, h.fail(agent.OpImagePull, err)
	}
	free, ferr := uint64(0), errors.New("no data root")
	if root != "" {
		free, ferr = h.freeBytes(root)
	}
	switch {
	case ferr != nil:
		_ = out.Progress(PullProgress{Status: "free disk space unknown"})
	case free < MinFreeBytes:
		return nil, opErr(agent.CodeDiskFull, "less than 1 GiB free on Docker's data root")
	}

	pctx, cancel := context.WithCancelCause(ctx)
	defer cancel(nil)
	errStalled := errors.New("stalled")
	timer := time.AfterFunc(stall, func() { cancel(errStalled) })
	defer timer.Stop()

	var latest dockerapi.PullProgress
	var haveLatest, unsent bool
	var lastSent time.Time
	send := func() {
		p := latest
		_ = out.Progress(PullProgress{Layers: p.Layers, LayersDone: p.LayersDone, Bytes: p.Bytes, BytesTotal: p.BytesTotal, Status: p.Status})
		lastSent, unsent = time.Now(), false
	}
	err = h.Docker.ImagePull(pctx, a.Ref, func(p dockerapi.PullProgress) {
		if !haveLatest || p != latest {
			// Anything new (bytes, a status, a layer) restarts the stall clock.
			timer.Reset(stall)
		}
		latest, haveLatest, unsent = p, true, true
		// Forwarding is throttled: Docker sends a line per layer chunk.
		if time.Since(lastSent) >= h.progressEvery {
			send()
		}
	})
	if err != nil {
		if errors.Is(context.Cause(pctx), errStalled) && ctx.Err() == nil {
			return nil, opErr(agent.CodeStalled, "the pull made no progress for %d seconds", a.StallS)
		}
		return nil, h.fail(agent.OpImagePull, err)
	}
	if unsent {
		send() // the final line
	}
	// The pull said "done": prove the image is there before reporting it.
	info, err := h.Docker.ImageInspect(ctx, a.Ref)
	if err != nil {
		return nil, h.fail(agent.OpImagePull, err)
	}
	return PullResult{ImageID: info.ID, RepoDigest: pickRepoDigest(a.Ref, info.RepoDigests)}, nil
}

// repoName strips the registry's default names, the tag and the digest.
func repoName(ref string) string {
	ref, _, _ = strings.Cut(ref, "@")
	if i := strings.LastIndex(ref, ":"); i > strings.LastIndex(ref, "/") {
		ref = ref[:i]
	}
	for _, p := range []string{"index.docker.io/", "docker.io/", "library/"} {
		ref = strings.TrimPrefix(ref, p)
	}
	return ref
}

// pickRepoDigest chooses the digest that belongs to ref's repository. An image
// can carry digests for several repositories; a wrong one would pull another
// image later, so with no match it returns none.
func pickRepoDigest(ref string, digests []string) string {
	want := repoName(ref)
	for _, d := range digests {
		if name, _, ok := strings.Cut(d, "@"); ok && repoName(name) == want {
			return d
		}
	}
	return ""
}

func (h *handlers) imageInspect(ctx context.Context, raw json.RawMessage, _ *agent.Out) (any, error) {
	var a ImageInspectArgs
	if err := decode(raw, &a); err != nil {
		return nil, err
	}
	var which string
	switch {
	case (a.Ref == "") == (a.ImageID == ""):
		return nil, opErr(agent.CodeInvalid, "give exactly one of ref and image_id")
	case a.ImageID != "":
		if !imageIDRe().MatchString(a.ImageID) {
			return nil, opErr(agent.CodeInvalid, "image_id must be sha256:<64 hex>")
		}
		which = a.ImageID
	default:
		which = a.Ref
	}
	info, err := h.Docker.ImageInspect(ctx, which)
	if err != nil {
		return nil, h.fail(agent.OpImageInspect, err)
	}
	return ImageInfo{
		ID:                info.ID,
		RepoDigests:       capped(info.RepoDigests, maxRepoDigests),
		ExposedPorts:      capped(info.ExposedPorts, maxImageLists),
		Volumes:           capped(info.Volumes, maxImageLists),
		ReservedLabelKeys: capped(info.ReservedLabelKeys, maxReserved),
	}, nil
}

// capped returns at most n entries, never nil (the result lists are arrays).
func capped(in []string, n int) []string {
	if len(in) > n {
		in = in[:n]
	}
	if in == nil {
		return []string{}
	}
	return in
}

// ---- containers ----

// ourNetwork inspects the app network and requires it to carry this install's
// label: the agent never trusts a network just because of its name.
func (h *handlers) ourNetwork(ctx context.Context, op string) (dockerapi.NetworkInfo, error) {
	n, err := h.Docker.NetworkInspect(ctx, h.network)
	if errors.Is(err, dockerapi.ErrNotFound) {
		return dockerapi.NetworkInfo{}, opErr(agent.CodeNotFound, "the app network does not exist")
	}
	if err != nil {
		return dockerapi.NetworkInfo{}, h.fail(op, err)
	}
	if n.Labels[LabelInstance] != h.Instance {
		return dockerapi.NetworkInfo{}, opErr(agent.CodeRefusedUnowned, "the app network is not this install's")
	}
	return n, nil
}

func (h *handlers) create(ctx context.Context, raw json.RawMessage, _ *agent.Out) (any, error) {
	var a CreateArgs
	if err := decode(raw, &a); err != nil {
		return nil, err
	}
	nm := containerName().FindStringSubmatch(a.Name)
	if nm == nil || len(a.Name) > maxContainerName {
		return nil, opErr(agent.CodeInvalid, "name must be sd-<project>-<service>-d<deploy>-a<attempt>")
	}
	if nm[3] != a.Spec.Labels[LabelDeploy] || nm[4] != a.Spec.Labels[LabelAttempt] {
		return nil, opErr(agent.CodePolicy, "the name's deploy and attempt must equal the labels")
	}
	if !imageIDRe().MatchString(a.ImageID) {
		return nil, opErr(agent.CodeInvalid, "image_id must be sha256:<64 hex>")
	}
	ip, err := netip.ParseAddr(a.IP)
	if err != nil || !ip.Is4() || ip.String() != a.IP {
		return nil, opErr(agent.CodeInvalid, "ip must be a canonical IPv4 address")
	}
	// The agent sets the instance label itself and takes only the other five.
	for k := range a.Spec.Labels {
		if k == LabelInstance {
			return nil, opErr(agent.CodePolicy, "the caller may not set serverdock.instance")
		}
		if !contains(callerLabels, k) {
			return nil, opErr(agent.CodePolicy, "label %q is not allowed", agent.CleanText(k, 64))
		}
	}
	if a.Spec.Image != a.ImageID {
		return nil, opErr(agent.CodePolicy, "the image must be the expected image id")
	}
	if a.Spec.HostConfig.NetworkMode != h.network {
		return nil, opErr(agent.CodePolicy, "the container must join the app network")
	}
	n, err := h.ourNetwork(ctx, agent.OpContainerCreate)
	if err != nil {
		return nil, err
	}
	if err := StaticAddr(n, ip); err != nil {
		return nil, opErr(agent.CodePolicy, "%v", err)
	}
	// An image with serverdock.* labels would merge them into the container
	// (making it fail the ownership rule, so it could never be removed).
	img, err := h.Docker.ImageInspect(ctx, a.ImageID)
	if err != nil {
		return nil, h.fail(agent.OpContainerCreate, err)
	}
	if img.HasReservedLabel {
		return nil, opErr(agent.CodePolicy, "the image carries serverdock.* labels")
	}

	spec := a.Spec
	spec.Labels = make(map[string]string, len(a.Spec.Labels)+1)
	for k, v := range a.Spec.Labels {
		spec.Labels[k] = v
	}
	spec.Labels[LabelInstance] = h.Instance
	exp := CreateExpect{Instance: h.Instance, Network: h.network, IP: a.IP, ImageID: a.ImageID}

	var refused error
	id, err := h.Docker.ContainerCreate(ctx, a.Name, spec, func(body []byte) error {
		// The exact bytes that go to Docker.
		refused = h.CheckCreate(body, exp)
		return refused
	})
	switch {
	case err == nil:
		return CreateResult{ID: id}, nil
	case refused != nil:
		return nil, opErr(agent.CodePolicy, "create refused: %v", refused)
	case !errors.Is(err, dockerapi.ErrConflict):
		return nil, h.fail(agent.OpContainerCreate, err)
	}

	// 409: the name is taken. It is a repeat of this create only if the
	// container is ours, for this deploy attempt, on this image.
	info, err := h.Docker.ContainerInspect(ctx, a.Name)
	if errors.Is(err, dockerapi.ErrNotFound) {
		return nil, opErr(agent.CodeConflict, "the container name was in use and is gone now; retry")
	}
	if err != nil {
		return nil, h.fail(agent.OpContainerCreate, err)
	}
	if !ownedLabels(info.Labels, info.LabelsDropped, h.Instance) || !sameLabels(info.Labels, spec.Labels) {
		return nil, opErr(agent.CodeConflict, "a container ServerDock does not own uses this name")
	}
	if info.ImageID != a.ImageID {
		return nil, opErr(agent.CodeConflict, "the container with this name belongs to this deploy attempt but runs another image")
	}
	if info.Networks[h.network].ConfiguredIPv4 != a.IP {
		return nil, opErr(agent.CodeConflict, "the container with this name has another address")
	}
	return CreateResult{ID: info.ID, Existed: true}, nil
}

func contains(s []string, v string) bool {
	for _, x := range s {
		if x == v {
			return true
		}
	}
	return false
}

func sameLabels(a, b map[string]string) bool {
	if len(a) != len(b) {
		return false
	}
	for k, v := range a {
		if w, ok := b[k]; !ok || w != v {
			return false
		}
	}
	return true
}

func (h *handlers) start(ctx context.Context, raw json.RawMessage, _ *agent.Out) (any, error) {
	var a IDArgs
	if err := decode(raw, &a); err != nil {
		return nil, err
	}
	info, err := h.owned(ctx, agent.OpContainerStart, a.ID)
	if err != nil {
		return nil, err
	}
	// Already running (304) is the state the caller wanted.
	if err := h.Docker.ContainerStart(ctx, info.ID); err != nil && !errors.Is(err, dockerapi.ErrNotModified) {
		return nil, h.fail(agent.OpContainerStart, err)
	}
	return Empty{}, nil
}

func checkGrace(g int) error {
	if g < 0 || g > MaxGraceS {
		return opErr(agent.CodeInvalid, "grace must be 0..%d", MaxGraceS)
	}
	return nil
}

func (h *handlers) restart(ctx context.Context, raw json.RawMessage, _ *agent.Out) (any, error) {
	var a GraceArgs
	if err := decode(raw, &a); err != nil {
		return nil, err
	}
	if err := checkGrace(a.Grace); err != nil {
		return nil, err
	}
	info, err := h.owned(ctx, agent.OpContainerRestart, a.ID)
	if err != nil {
		return nil, err
	}
	if err := h.Docker.ContainerRestart(ctx, info.ID, a.Grace); err != nil {
		return nil, h.fail(agent.OpContainerRestart, err)
	}
	return Empty{}, nil
}

func (h *handlers) stop(ctx context.Context, raw json.RawMessage, _ *agent.Out) (any, error) {
	var a GraceArgs
	if err := decode(raw, &a); err != nil {
		return nil, err
	}
	if err := checkGrace(a.Grace); err != nil {
		return nil, err
	}
	info, err := h.owned(ctx, agent.OpContainerStop, a.ID)
	if err != nil {
		return nil, err
	}
	if err := h.Docker.Stop(ctx, info.ID, a.Grace); err != nil {
		return nil, h.fail(agent.OpContainerStop, err)
	}
	return Empty{}, nil
}

func (h *handlers) remove(ctx context.Context, raw json.RawMessage, _ *agent.Out) (any, error) {
	var a IDArgs
	if err := decode(raw, &a); err != nil {
		return nil, err
	}
	info, err := h.owned(ctx, agent.OpContainerRemove, a.ID)
	if err != nil {
		return nil, err
	}
	if err := h.Docker.ContainerRemove(ctx, info.ID); err != nil {
		return nil, h.fail(agent.OpContainerRemove, err)
	}
	return Empty{}, nil
}

func (h *handlers) inspect(ctx context.Context, raw json.RawMessage, _ *agent.Out) (any, error) {
	var a IDArgs
	if err := decode(raw, &a); err != nil {
		return nil, err
	}
	if !dockerapi.ValidFullID(a.ID) {
		return nil, opErr(agent.CodeInvalid, "id must be a full 64-character hex container id")
	}
	info, err := h.Docker.ContainerInspect(ctx, a.ID)
	if err != nil {
		return nil, h.fail(agent.OpContainerInspect, err)
	}
	r := ContainerState{
		ID: info.ID, State: info.State.Status, Running: info.State.Running, Restarting: info.State.Restarting,
		ExitCode: info.State.ExitCode, OOMKilled: info.State.OOMKilled, RestartCount: info.RestartCount,
		ImageID: info.ImageID, Labels: info.Labels,
		Owned: ownedLabels(info.Labels, info.LabelsDropped, h.Instance),
	}
	if n, ok := info.Networks[h.network]; ok {
		r.IP, r.ConfiguredIP = n.IPAddress, n.ConfiguredIPv4
	}
	if !info.State.StartedAt.IsZero() {
		r.StartedAt = info.State.StartedAt.UTC().Format(time.RFC3339Nano)
	}
	return r, nil
}

func (h *handlers) list(ctx context.Context, raw json.RawMessage, _ *agent.Out) (any, error) {
	if err := decode(raw, &Empty{}); err != nil {
		return nil, err
	}
	all, err := h.Docker.ContainerList(ctx, dockerapi.ListOptions{All: true, Labels: h.labels})
	if err != nil {
		return nil, h.fail(agent.OpContainerList, err)
	}
	res := ListResult{Containers: []ListedContainer{}}
	for _, s := range all {
		if s.Labels[LabelInstance] != h.Instance { // the filter is Docker's; check anyway
			continue
		}
		e := ListedContainer{
			ID: s.ID, ImageID: s.ImageID, State: s.State,
			Labels: ListedLabels{Project: s.Labels[LabelProject], Service: s.Labels[LabelService],
				Deploy: s.Labels[LabelDeploy], Attempt: s.Labels[LabelAttempt]},
			Unowned: !ownedLabels(s.Labels, s.LabelsDropped, h.Instance),
		}
		if len(s.Names) > 0 {
			e.Name = agent.CleanText(s.Names[0], 128)
		}
		if n, ok := s.Networks[h.network]; ok {
			e.IP, e.ConfiguredIP = n.IPAddress, n.ConfiguredIPv4
		}
		res.Containers = append(res.Containers, e)
	}
	sort.Slice(res.Containers, func(i, j int) bool { return res.Containers[i].ID < res.Containers[j].ID })
	// Never a partial list: the allocator's "held" rule and the orphan sweep
	// would act on a list missing containers.
	spec, _ := agent.SpecFor(agent.OpContainerList)
	if len(res.Containers) > MaxList || fits(res, spec) != nil {
		return nil, opErr(agent.CodeConflict, "too many containers to list (more than %d, or the list is too large)", MaxList)
	}
	return res, nil
}

func (h *handlers) ports(ctx context.Context, raw json.RawMessage, _ *agent.Out) (any, error) {
	if err := decode(raw, &Empty{}); err != nil {
		return nil, err
	}
	all, err := h.Docker.ContainerPorts(ctx)
	if err != nil {
		return nil, h.fail(agent.OpContainerPorts, err)
	}
	res := PortsResult{Containers: []PublishingContainer{}}
	for _, c := range all {
		e := PublishingContainer{Name: agent.CleanText(c.Name, MaxNameText), Ports: []PublishedPort{}}
		e.Ports, e.PortsTruncated = pickPorts(c.Ports)
		res.Containers = append(res.Containers, e)
	}
	if len(res.Containers) > MaxList {
		res.Containers, res.Truncated = res.Containers[:MaxList], true
	}
	spec, _ := agent.SpecFor(agent.OpContainerPorts)
	for fits(res, spec) != nil && len(res.Containers) > 0 {
		res.Containers, res.Truncated = res.Containers[:len(res.Containers)*3/4], true
	}
	return res, nil
}

// hostRank orders host addresses: all interfaces first (the exposure that
// matters), then other non-loopback addresses, then loopback.
func hostRank(ip string) int {
	a, err := netip.ParseAddr(ip)
	switch {
	case err != nil || a.IsUnspecified() || ip == "":
		return 0
	case a.IsLoopback():
		return 2
	}
	return 1
}

// pickPorts dedupes a container's published ports (Docker lists an IPv4 and an
// IPv6 entry for the same port) keeping the most exposed host address, orders
// them most exposed first, and keeps at most MaxPortsEach. The flag says some
// were left out.
func pickPorts(in []dockerapi.PublishedPort) ([]PublishedPort, bool) {
	type key struct {
		host, cont int
		proto      string
	}
	best := map[key]dockerapi.PublishedPort{}
	for _, p := range in {
		k := key{p.HostPort, p.ContainerPt, p.Proto}
		if old, ok := best[k]; !ok || hostRank(p.HostIP) < hostRank(old.HostIP) {
			best[k] = p
		}
	}
	list := make([]dockerapi.PublishedPort, 0, len(best))
	for _, p := range best {
		list = append(list, p)
	}
	sort.Slice(list, func(i, j int) bool {
		if ri, rj := hostRank(list[i].HostIP), hostRank(list[j].HostIP); ri != rj {
			return ri < rj
		}
		if list[i].HostPort != list[j].HostPort {
			return list[i].HostPort < list[j].HostPort
		}
		return list[i].Proto < list[j].Proto
	})
	out := []PublishedPort{}
	for _, p := range list {
		if len(out) == MaxPortsEach {
			return out, true
		}
		out = append(out, PublishedPort{HostIP: agent.CleanText(p.HostIP, 45), HostPort: p.HostPort,
			ContainerPort: p.ContainerPt, Proto: agent.CleanText(p.Proto, 8)})
	}
	return out, false
}

// fits reports an error when v would not fit the op's result cap.
func fits(v any, spec agent.OpSpec) error {
	b, err := json.Marshal(v)
	if err != nil {
		return err
	}
	if len(b) > spec.MaxResult-64 {
		return errors.New("too large")
	}
	return nil
}

// ---- networks ----

func (h *handlers) networkEnsure(ctx context.Context, raw json.RawMessage, _ *agent.Out) (any, error) {
	var a NetworkArgs
	if err := decode(raw, &a); err != nil {
		return nil, err
	}
	if a.Name != h.network {
		return nil, opErr(agent.CodePolicy, "the agent manages only the network %s", h.network)
	}
	n, err := h.Docker.NetworkEnsure(ctx, dockerapi.NetworkSpec{
		Name: a.Name, Subnet: a.Subnet, Labels: map[string]string{LabelInstance: h.Instance},
	})
	if err != nil {
		if errors.Is(err, dockerapi.ErrInvalidSubnet) {
			return nil, opErr(agent.CodeInvalid, "subnet must be a canonical IPv4 CIDR from /16 to /26")
		}
		return nil, h.fail(agent.OpNetworkEnsure, err)
	}
	return NetworkResult{ID: n.ID, Subnet: n.Subnet, Gateway: n.Gateway}, nil
}

func (h *handlers) networkInspect(ctx context.Context, raw json.RawMessage, _ *agent.Out) (any, error) {
	if err := decode(raw, &Empty{}); err != nil {
		return nil, err
	}
	n, err := h.ourNetwork(ctx, agent.OpNetworkInspect)
	if err != nil {
		return nil, err
	}
	used := make([]string, 0, len(n.Used))
	for ip := range n.Used {
		used = append(used, ip)
	}
	sort.Strings(used)
	if len(used) > MaxUsedAddrs {
		// Silently dropping an address could let the allocator reuse it.
		return nil, opErr(agent.CodeConflict, "more than %d addresses in use", MaxUsedAddrs)
	}
	return UsedAddrs{Subnet: n.Subnet, Gateway: n.Gateway, IPRange: n.IPRange, Used: used}, nil
}

// ---- events ----

func (h *handlers) events(ctx context.Context, raw json.RawMessage, out *agent.Out) (any, error) {
	if err := decode(raw, &Empty{}); err != nil {
		return nil, err
	}
	err := h.Docker.EventsOpen(ctx, h.labels, func() { _ = out.Progress(EventsOpen{}) }, func(e dockerapi.Event) error {
		return out.Event(Event{Action: e.Action, ID: e.ID, Service: e.Service, Deploy: e.Deploy, ExitCode: e.ExitCode, Time: e.Time})
	})
	if err != nil {
		return nil, h.fail(agent.OpEvents, err)
	}
	return Empty{}, nil // Docker ended the stream: the watcher resyncs
}

// ---- health ----

func healthPathOK(p string) bool {
	if len(p) == 0 || len(p) > 256 || p[0] != '/' {
		return false
	}
	for i := 0; i < len(p); i++ {
		if c := p[i]; c <= ' ' || c >= 0x7f || c == '#' || c == '\\' {
			return false
		}
	}
	return true
}

func (h *handlers) health(ctx context.Context, raw json.RawMessage, _ *agent.Out) (any, error) {
	var a HealthArgs
	if err := decode(raw, &a); err != nil {
		return nil, err
	}
	ap, err := netip.ParseAddrPort(a.Target)
	if err != nil || ap.Port() == 0 || ap.String() != a.Target {
		return nil, opErr(agent.CodeInvalid, "target must be a canonical ip:port")
	}
	switch a.Kind {
	case KindHTTP:
		if !healthPathOK(a.Path) {
			return nil, opErr(agent.CodeInvalid, "path must start with / and be printable ASCII (at most 256 bytes)")
		}
	case KindTCP:
		if a.Path != "" {
			return nil, opErr(agent.CodeInvalid, "path is for http checks only")
		}
	default:
		return nil, opErr(agent.CodeInvalid, "kind must be http or tcp")
	}
	if a.TimeoutMS == 0 {
		a.TimeoutMS = MaxHealthMS
	}
	if a.TimeoutMS < 1 || a.TimeoutMS > MaxHealthMS {
		return nil, opErr(agent.CodeInvalid, "timeout_ms must be 1..%d", MaxHealthMS)
	}
	// Only ServerDock's own static addresses: the check must not become a way
	// to probe the network, the gateway (the host) or anything else.
	n, err := h.ourNetwork(ctx, agent.OpHealthCheck)
	if err != nil {
		return nil, err
	}
	if err := StaticAddr(n, ap.Addr()); err != nil {
		return nil, opErr(agent.CodePolicy, "target refused: %v", err)
	}
	timeout := time.Duration(a.TimeoutMS) * time.Millisecond
	cctx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()
	start := time.Now()
	res := HealthResult{}
	if a.Kind == KindTCP {
		conn, err := h.dial(cctx, "tcp", a.Target)
		if err == nil {
			conn.Close()
			res.OK = true
		} else {
			res.Err = reason(err)
		}
	} else {
		res.Status, res.Err = h.httpGet(cctx, ap, a.Path)
		res.OK = res.Status >= 200 && res.Status < 400
	}
	res.MS = int(time.Since(start) / time.Millisecond)
	return res, nil
}

// httpGet does one GET: no redirects, no proxy (the transport has none, and
// never reads the environment), no keep-alive, Host set to the IP, at most 4 KiB
// of the body read and discarded.
func (h *handlers) httpGet(ctx context.Context, ap netip.AddrPort, path string) (int, string) {
	tr := &http.Transport{Proxy: nil, DialContext: h.dial, DisableKeepAlives: true, DisableCompression: true,
		MaxResponseHeaderBytes: 16 << 10, ResponseHeaderTimeout: 2 * time.Second}
	defer tr.CloseIdleConnections()
	cl := &http.Client{Transport: tr, CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, "http://"+ap.String()+path, nil)
	if err != nil {
		return 0, "bad request"
	}
	req.Host = ap.Addr().String()
	req.Header.Set("User-Agent", "ServerDock-health/1")
	resp, err := cl.Do(req)
	if err != nil {
		return 0, reason(err)
	}
	defer resp.Body.Close()
	_, _ = io.CopyN(io.Discard, resp.Body, healthReadCap)
	return resp.StatusCode, ""
}

// reason is a short, fixed description of a failed probe (never the error text:
// it can name local addresses and is of no use to the caller).
func reason(err error) string {
	var ne net.Error
	switch {
	case errors.Is(err, context.DeadlineExceeded), errors.As(err, &ne) && ne.Timeout():
		return "timeout"
	case errors.Is(err, syscall.ECONNREFUSED):
		return "connection refused"
	case errors.Is(err, syscall.EHOSTUNREACH), errors.Is(err, syscall.ENETUNREACH):
		return "unreachable"
	case errors.Is(err, context.Canceled):
		return "cancelled"
	}
	return "request failed"
}
