package dockerapi

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/url"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/Docker-Hunterpedia/serverdock/internal/agent"
	"github.com/Docker-Hunterpedia/serverdock/internal/policy"
)

var (
	fullIDRe   = regexp.MustCompile(`^[0-9a-f]{64}$`)
	createName = regexp.MustCompile(`^[a-zA-Z0-9][a-zA-Z0-9_.-]{0,127}$`)
)

// ValidFullID reports whether id is a full 64-hex container id. Every method
// that changes a container takes only this form: a name or a short prefix could
// match a different container than the caller inspected.
func ValidFullID(id string) bool { return fullIDRe.MatchString(id) }

// CreateSpec is the container create body. It has only the fields ServerDock
// is allowed to set (plan 0004, "Policy check"); a field that is not here can't
// be sent. M3b and M6 widen it explicitly.
type CreateSpec struct {
	Image            string              `json:"Image"`
	Labels           map[string]string   `json:"Labels"`
	ExposedPorts     map[string]struct{} `json:"ExposedPorts,omitempty"`
	HostConfig       HostConfig          `json:"HostConfig"`
	NetworkingConfig NetworkingConfig    `json:"NetworkingConfig"`
}

// HostConfig is the create body's HostConfig.
type HostConfig struct {
	RestartPolicy RestartPolicy `json:"RestartPolicy"`
	LogConfig     LogConfig     `json:"LogConfig"`
	NetworkMode   string        `json:"NetworkMode"`
	PidsLimit     int64         `json:"PidsLimit"`
}

// RestartPolicy is a Docker restart policy.
type RestartPolicy struct {
	Name string `json:"Name"`
}

// LogConfig selects the container's log driver and its options.
type LogConfig struct {
	Type   string            `json:"Type"`
	Config map[string]string `json:"Config"`
}

// NetworkingConfig is the create body's NetworkingConfig.
type NetworkingConfig struct {
	EndpointsConfig map[string]EndpointConfig `json:"EndpointsConfig"`
}

// EndpointConfig is one network endpoint of a create body.
type EndpointConfig struct {
	IPAMConfig EndpointIPAM `json:"IPAMConfig"`
}

// EndpointIPAM carries the static address.
type EndpointIPAM struct {
	IPv4Address string `json:"IPv4Address"`
}

// Marshal returns the exact bytes ContainerCreate sends.
func (s CreateSpec) Marshal() ([]byte, error) { return json.Marshal(s) }

// ContainerCreate creates a container named name. verify is required: it gets
// the exact bytes that are about to be sent and must return nil, so a policy
// check runs on what Docker receives (not on a copy that could differ). It
// returns the new container's full id. A name that is taken is ErrConflict.
func (c *Client) ContainerCreate(ctx context.Context, name string, spec CreateSpec, verify func(body []byte) error) (string, error) {
	if verify == nil {
		return "", errors.New("dockerapi: create needs a verify function")
	}
	if !createName.MatchString(name) {
		return "", errors.New("dockerapi: invalid container name")
	}
	body, err := spec.Marshal()
	if err != nil {
		return "", err
	}
	if err := verify(body); err != nil {
		return "", fmt.Errorf("dockerapi: create refused: %w", err)
	}
	st, resp, err := c.call(ctx, c.short, http.MethodPost, "/containers/create?name="+url.QueryEscape(name), body)
	if err != nil {
		return "", err
	}
	if err := statusErr("container create", st, resp, http.StatusCreated); err != nil {
		return "", err
	}
	var r struct{ Id string }
	if err := json.Unmarshal(resp, &r); err != nil || !ValidFullID(r.Id) {
		return "", errors.New("dockerapi: container create: bad response")
	}
	return r.Id, nil
}

// ContainerStart starts a container. A container that is already running is
// ErrNotModified.
func (c *Client) ContainerStart(ctx context.Context, id string) error {
	if !ValidFullID(id) {
		return errors.New("dockerapi: invalid container id")
	}
	st, body, err := c.call(ctx, c.short, http.MethodPost, "/containers/"+id+"/start", nil)
	if err != nil {
		return err
	}
	return statusErr("container start", st, body, http.StatusNoContent)
}

// ContainerRestart restarts a container, giving it grace seconds to stop. The
// call is bounded by grace plus a margin.
func (c *Client) ContainerRestart(ctx context.Context, id string, graceSeconds int) error {
	if !ValidFullID(id) {
		return errors.New("dockerapi: invalid container id")
	}
	if graceSeconds < 0 || graceSeconds > 3600 {
		return errors.New("dockerapi: stop grace out of range")
	}
	timeout := time.Duration(graceSeconds)*time.Second + c.slack
	st, body, err := c.call(ctx, timeout, http.MethodPost, "/containers/"+id+"/restart?t="+strconv.Itoa(graceSeconds), nil)
	if err != nil {
		return err
	}
	return statusErr("container restart", st, body, http.StatusNoContent)
}

// ContainerRemove removes a stopped container and its anonymous volumes
// (v=1); it never forces. A running container is ErrConflict, and so is "removal
// already in progress": on ErrConflict callers re-inspect to tell which.
// Named volumes are never removed this way.
func (c *Client) ContainerRemove(ctx context.Context, id string) error {
	if !ValidFullID(id) {
		return errors.New("dockerapi: invalid container id")
	}
	st, body, err := c.call(ctx, c.short, http.MethodDelete, "/containers/"+id+"?v=1&force=0", nil)
	if err != nil {
		return err
	}
	return statusErr("container remove", st, body, http.StatusNoContent)
}

// maxOurLabels and the two bounds below limit what ourLabels keeps.
const (
	maxOurLabels     = 16
	maxOurLabelKey   = 64
	maxOurLabelValue = 256
)

// ourLabels keeps only the serverdock.* labels: at most maxOurLabels, a key of at
// most maxOurLabelKey bytes, and a value only if it is already clean text of at
// most maxOurLabelValue bytes (CleanText(v) == v). The rest came from an image or
// a user and the deploy path never needs them.
//
// A serverdock.* label that is not kept (too many, an over-long key, a value that
// is not clean) is dropped and reported through dropped, never rewritten: a
// "cleaned" value would be a different value, so a container carrying it must not
// look like ours. The ownership check (exactly six labels, the expected values)
// then refuses it, and so does a caller that sees dropped.
func ourLabels(in map[string]string) (out map[string]string, dropped bool) {
	out = map[string]string{}
	for k, v := range in {
		if !strings.HasPrefix(k, "serverdock.") {
			continue
		}
		if len(out) >= maxOurLabels || len(k) > maxOurLabelKey || agent.CleanText(v, maxOurLabelValue) != v {
			dropped = true
			continue
		}
		out[k] = v
	}
	return out, dropped
}

// ContainerState is the part of State the deploy path reads.
type ContainerState struct {
	Status     string // created, running, restarting, exited, ...
	Running    bool
	Restarting bool
	ExitCode   int
	OOMKilled  bool
	StartedAt  time.Time
}

// ContainerNet is a container's endpoint on one network.
type ContainerNet struct {
	IPAddress      string // the current address ("" when not connected)
	ConfiguredIPv4 string // the static address from IPAMConfig ("" when none)
}

// ContainerInfo is an inspected container.
type ContainerInfo struct {
	ID      string
	Name    string // without the leading slash
	ImageID string // Image: the image id the container was created from
	Labels  map[string]string
	State   ContainerState
	// LabelsDropped is true when a serverdock.* label was not kept (see ourLabels):
	// an ownership check must refuse such a container.
	LabelsDropped bool
	RestartCount  int
	NetworkMode   string
	Networks      map[string]ContainerNet
}

// ContainerInspect inspects a container by full id or by name (a 409 on create
// is resolved by name; every change then goes through the inspected full id).
func (c *Client) ContainerInspect(ctx context.Context, idOrName string) (ContainerInfo, error) {
	if !agent.ValidContainerID(idOrName) {
		return ContainerInfo{}, errors.New("dockerapi: invalid container id")
	}
	st, body, err := c.call(ctx, c.short, http.MethodGet, "/containers/"+url.PathEscape(idOrName)+"/json", nil)
	if err != nil {
		return ContainerInfo{}, err
	}
	if err := statusErr("container inspect", st, body, http.StatusOK); err != nil {
		return ContainerInfo{}, err
	}
	var r struct {
		ID           string
		Name         string
		Image        string
		RestartCount int
		State        struct {
			Status     string
			Running    bool
			Restarting bool
			ExitCode   int
			OOMKilled  bool
			StartedAt  string
		}
		Config     struct{ Labels map[string]string }
		HostConfig struct{ NetworkMode string }
		Network    struct {
			Networks map[string]struct {
				IPAddress  string
				IPAMConfig *struct{ IPv4Address string }
			}
		} `json:"NetworkSettings"`
	}
	if err := json.Unmarshal(body, &r); err != nil {
		return ContainerInfo{}, fmt.Errorf("dockerapi: container inspect: %w", err)
	}
	if !ValidFullID(r.ID) {
		return ContainerInfo{}, errors.New("dockerapi: container inspect: bad id")
	}
	labels, dropped := ourLabels(r.Config.Labels)
	info := ContainerInfo{
		ID: r.ID, Name: strings.TrimPrefix(r.Name, "/"), ImageID: r.Image, Labels: labels, LabelsDropped: dropped,
		RestartCount: r.RestartCount, NetworkMode: r.HostConfig.NetworkMode,
		State: ContainerState{Status: agent.CleanText(r.State.Status, 32), Running: r.State.Running, Restarting: r.State.Restarting,
			ExitCode: r.State.ExitCode, OOMKilled: r.State.OOMKilled},
		Networks: map[string]ContainerNet{},
	}
	if t, err := time.Parse(time.RFC3339Nano, r.State.StartedAt); err == nil && t.Year() > 1 {
		info.State.StartedAt = t
	}
	for n, e := range r.Network.Networks {
		cn := ContainerNet{IPAddress: e.IPAddress}
		if e.IPAMConfig != nil {
			cn.ConfiguredIPv4 = e.IPAMConfig.IPv4Address
		}
		info.Networks[n] = cn
	}
	return info, nil
}

// ContainerSummary is one entry of ContainerList.
type ContainerSummary struct {
	ID      string
	Names   []string // without the leading slash
	ImageID string
	State   string
	Labels  map[string]string
	// LabelsDropped is true when a serverdock.* label was not kept (see ourLabels).
	LabelsDropped bool
	Networks      map[string]ContainerNet
	Ports         []PublishedPort
}

// PublishedPort is a port a container publishes on the host.
type PublishedPort struct {
	HostIP      string
	HostPort    int
	ContainerPt int
	Proto       string
}

// ListOptions selects containers. Labels are "key" or "key=value" filters.
type ListOptions struct {
	All    bool
	Labels []string
}

// ContainerList lists containers (all states when opt.All) that match the label
// filters. It reads the configured IPAMConfig address as well as the current
// one, so a stopped container still shows the address it holds. Entries whose
// id is not a full 64-hex id are skipped, and only serverdock.* label keys are
// returned (LabelsDropped says when one was not kept).
func (c *Client) ContainerList(ctx context.Context, opt ListOptions) ([]ContainerSummary, error) {
	q := url.Values{}
	if opt.All {
		q.Set("all", "1")
	}
	if len(opt.Labels) > 0 {
		for _, l := range opt.Labels {
			if l == "" || len(l) > 256 || strings.ContainsAny(l, "\x00\r\n") {
				return nil, errors.New("dockerapi: invalid label filter")
			}
		}
		f, _ := json.Marshal(map[string][]string{"label": opt.Labels})
		q.Set("filters", string(f))
	}
	st, body, err := c.call(ctx, c.short, http.MethodGet, "/containers/json?"+q.Encode(), nil)
	if err != nil {
		return nil, err
	}
	if err := statusErr("container list", st, body, http.StatusOK); err != nil {
		return nil, err
	}
	var raw []struct {
		ID      string
		Names   []string
		ImageID string
		State   string
		Labels  map[string]string
		Ports   []struct {
			IP          string
			PrivatePort int
			PublicPort  int
			Type        string
		}
		NetworkSettings struct {
			Networks map[string]struct {
				IPAddress  string
				IPAMConfig *struct{ IPv4Address string }
			}
		}
	}
	if err := json.Unmarshal(body, &raw); err != nil {
		return nil, fmt.Errorf("dockerapi: container list: %w", err)
	}
	out := make([]ContainerSummary, 0, len(raw))
	for _, r := range raw {
		if !ValidFullID(r.ID) {
			continue
		}
		labels, dropped := ourLabels(r.Labels)
		s := ContainerSummary{ID: r.ID, ImageID: r.ImageID, State: agent.CleanText(r.State, 32), Labels: labels, LabelsDropped: dropped, Networks: map[string]ContainerNet{}}
		for _, n := range r.Names {
			s.Names = append(s.Names, strings.TrimPrefix(n, "/"))
		}
		for n, e := range r.NetworkSettings.Networks {
			cn := ContainerNet{IPAddress: e.IPAddress}
			if e.IPAMConfig != nil {
				cn.ConfiguredIPv4 = e.IPAMConfig.IPv4Address
			}
			s.Networks[n] = cn
		}
		for _, p := range r.Ports {
			if p.PublicPort > 0 {
				s.Ports = append(s.Ports, PublishedPort{HostIP: p.IP, HostPort: p.PublicPort, ContainerPt: p.PrivatePort, Proto: p.Type})
			}
		}
		out = append(out, s)
	}
	return out, nil
}

// PublishingContainer is a running container that publishes host ports.
type PublishingContainer struct {
	Name  string
	Ports []PublishedPort
	// Instance is the container's serverdock.instance label ("" when it has none):
	// the control-plane ops use it to leave an install's own containers out.
	Instance string
}

// ContainerPorts lists every running container that publishes a host port
// (the SEC6 exposure check), in name order.
func (c *Client) ContainerPorts(ctx context.Context) ([]PublishingContainer, error) {
	list, err := c.ContainerList(ctx, ListOptions{})
	if err != nil {
		return nil, err
	}
	var out []PublishingContainer
	for _, s := range list {
		if len(s.Ports) == 0 {
			continue
		}
		name := s.ID
		if len(s.Names) > 0 {
			name = s.Names[0]
		}
		out = append(out, PublishingContainer{Name: name, Ports: s.Ports, Instance: s.Labels[policy.LabelInstance]})
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Name < out[j].Name })
	return out, nil
}
