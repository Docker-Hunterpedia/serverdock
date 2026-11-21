package dockerapi

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/netip"
	"net/url"
	"regexp"
	"sort"
	"strings"

	"github.com/Docker-Hunterpedia/serverdock/internal/agent"
)

// ErrForeignNetwork is a same-name network that is not ours (it lacks one of
// the expected labels).
var ErrForeignNetwork = errors.New("dockerapi: a network with this name exists and is not ServerDock's")

// ErrNetworkSettings is our network with settings that differ from what
// NetworkEnsure creates (driver, gateway, IPRange, inter-container traffic).
var ErrNetworkSettings = errors.New("dockerapi: the network exists with other settings")

// ErrSubnetOverlap is Docker refusing to create the network because its subnet
// overlaps another network or a host route.
var ErrSubnetOverlap = errors.New("dockerapi: the subnet overlaps another network")

// ErrSubnetMismatch is our network with another subnet than the one asked for.
var ErrSubnetMismatch = errors.New("dockerapi: the network exists with another subnet")

var networkNameRe = regexp.MustCompile(`^[a-zA-Z0-9][a-zA-Z0-9_.-]{0,63}$`)

// NetworkSpec describes the app network.
type NetworkSpec struct {
	Name   string
	Subnet string            // IPv4 CIDR, canonical, /16 to /26
	Labels map[string]string // all must be present, with these values, on an existing network
}

// NetworkInfo is a network as ServerDock sees it.
type NetworkInfo struct {
	ID      string
	Driver  string
	Subnet  string
	Gateway string
	IPRange string
	ICC     string // the enable_icc option ("" when unset)
	Labels  map[string]string
	// Used maps the address of every connected endpoint to the container id.
	// Docker lists only connected endpoints, so a stopped container is absent.
	Used map[string]string
}

// networkAddrs derives the gateway (the subnet's .1) and the dynamic IPRange
// (the top quarter: a /26 of a /24) of a subnet. This client accepts /16 to /26;
// the policy narrows --app-subnet to /24 to /20 (step 4a).
func networkAddrs(subnet string) (gateway netip.Addr, ipRange netip.Prefix, err error) {
	p, err := netip.ParsePrefix(subnet)
	if err != nil || !p.Addr().Is4() || p.Masked() != p || p.Bits() < 16 || p.Bits() > 26 {
		return netip.Addr{}, netip.Prefix{}, errors.New("dockerapi: subnet must be a canonical IPv4 CIDR from /16 to /26")
	}
	gateway = p.Addr().Next()
	// Top quarter: two more prefix bits, the last of the four blocks.
	a := p.Addr().As4()
	n := uint32(a[0])<<24 | uint32(a[1])<<16 | uint32(a[2])<<8 | uint32(a[3])
	size := uint32(1) << (32 - p.Bits())
	start := n + size - size/4
	r := netip.PrefixFrom(netip.AddrFrom4([4]byte{byte(start >> 24), byte(start >> 16), byte(start >> 8), byte(start)}), p.Bits()+2)
	return gateway, r, nil
}

// DynamicRange returns the IPRange Docker may hand out dynamically on a subnet
// created by NetworkEnsure (its top quarter).
func DynamicRange(subnet string) (netip.Prefix, error) {
	_, r, err := networkAddrs(subnet)
	return r, err
}

// NetworkEnsure makes sure the app network exists, and is idempotent. An
// existing network must carry spec's labels (else ErrForeignNetwork) and the
// same subnet (else ErrSubnetMismatch); it is never recreated. A new network is
// a bridge with the explicit subnet, the Gateway set to the subnet's .1 (with
// IPRange set Docker could otherwise take the gateway from the range), IPRange
// set to the top quarter so a hand-attached container never takes an address
// ServerDock allocates, and inter-container traffic off.
func (c *Client) NetworkEnsure(ctx context.Context, spec NetworkSpec) (NetworkInfo, error) {
	if !networkNameRe.MatchString(spec.Name) {
		return NetworkInfo{}, errors.New("dockerapi: invalid network name")
	}
	if len(spec.Labels) == 0 {
		return NetworkInfo{}, errors.New("dockerapi: a network needs ownership labels")
	}
	gw, rng, err := networkAddrs(spec.Subnet)
	if err != nil {
		return NetworkInfo{}, err
	}
	check := func() (NetworkInfo, error) {
		info, err := c.NetworkInspect(ctx, spec.Name)
		if err != nil {
			return NetworkInfo{}, err
		}
		for k, v := range spec.Labels {
			if got, ok := info.Labels[k]; !ok || got != v {
				return NetworkInfo{}, ErrForeignNetwork
			}
		}
		if info.Subnet != spec.Subnet {
			return NetworkInfo{}, fmt.Errorf("%w (%s, asked for %s)", ErrSubnetMismatch, info.Subnet, spec.Subnet)
		}
		switch {
		case info.Driver != "bridge":
			return NetworkInfo{}, fmt.Errorf("%w: driver is %q, not bridge", ErrNetworkSettings, agent.CleanText(info.Driver, 32))
		case info.Gateway != gw.String():
			return NetworkInfo{}, fmt.Errorf("%w: gateway is %q, not %s", ErrNetworkSettings, agent.CleanText(info.Gateway, 64), gw)
		case info.IPRange != rng.String():
			return NetworkInfo{}, fmt.Errorf("%w: IPRange is %q, not %s", ErrNetworkSettings, agent.CleanText(info.IPRange, 64), rng)
		case info.ICC != "false":
			return NetworkInfo{}, fmt.Errorf("%w: inter-container traffic is not disabled", ErrNetworkSettings)
		}
		return info, nil
	}
	info, err := check()
	if !errors.Is(err, ErrNotFound) {
		return info, err
	}
	body, _ := json.Marshal(createNetwork{
		Name:   spec.Name,
		Driver: "bridge",
		IPAM: createIPAM{Driver: "default", Config: []createIPAMConfig{
			{Subnet: spec.Subnet, IPRange: rng.String(), Gateway: gw.String()},
		}},
		Options: map[string]string{"com.docker.network.bridge.enable_icc": "false"},
		Labels:  spec.Labels,
	})
	st, resp, err := c.call(ctx, c.short, http.MethodPost, "/networks/create", body)
	if err != nil {
		return NetworkInfo{}, err
	}
	if err := statusErr("network create", st, resp, http.StatusCreated); err != nil {
		if errors.Is(err, ErrConflict) { // created by someone else in between
			return check()
		}
		var ae *APIError
		if errors.As(err, &ae) && ae.Status == http.StatusForbidden && strings.Contains(strings.ToLower(ae.Msg), "overlap") {
			return NetworkInfo{}, fmt.Errorf("%w: %s", ErrSubnetOverlap, ae.Msg)
		}
		return NetworkInfo{}, err
	}
	return check()
}

type createNetwork struct {
	Name    string            `json:"Name"`
	Driver  string            `json:"Driver"`
	IPAM    createIPAM        `json:"IPAM"`
	Options map[string]string `json:"Options"`
	Labels  map[string]string `json:"Labels"`
}

type createIPAM struct {
	Driver string             `json:"Driver"`
	Config []createIPAMConfig `json:"Config"`
}

type createIPAMConfig struct {
	Subnet  string `json:"Subnet"`
	IPRange string `json:"IPRange"`
	Gateway string `json:"Gateway"`
}

// NetworkInspect inspects a network by name. A missing network is ErrNotFound.
// Only the first IPv4 IPAM entry is read.
func (c *Client) NetworkInspect(ctx context.Context, name string) (NetworkInfo, error) {
	if !networkNameRe.MatchString(name) {
		return NetworkInfo{}, errors.New("dockerapi: invalid network name")
	}
	st, body, err := c.call(ctx, c.short, http.MethodGet, "/networks/"+url.PathEscape(name), nil)
	if err != nil {
		return NetworkInfo{}, err
	}
	if err := statusErr("network inspect", st, body, http.StatusOK); err != nil {
		return NetworkInfo{}, err
	}
	var r struct {
		Id      string
		Name    string
		Driver  string
		Options map[string]string
		Labels  map[string]string
		IPAM    struct {
			Config []struct{ Subnet, IPRange, Gateway string }
		}
		Containers map[string]struct{ IPv4Address string }
	}
	if err := json.Unmarshal(body, &r); err != nil {
		return NetworkInfo{}, fmt.Errorf("dockerapi: network inspect: %w", err)
	}
	if r.Name != name { // Docker also matches id prefixes
		return NetworkInfo{}, &APIError{Op: "network inspect", Status: http.StatusNotFound}
	}
	info := NetworkInfo{ID: r.Id, Driver: r.Driver, ICC: r.Options["com.docker.network.bridge.enable_icc"], Labels: r.Labels, Used: map[string]string{}}
	for _, cfg := range r.IPAM.Config {
		if p, err := netip.ParsePrefix(cfg.Subnet); err == nil && p.Addr().Is4() {
			info.Subnet, info.Gateway, info.IPRange = cfg.Subnet, cfg.Gateway, cfg.IPRange
			break
		}
	}
	ids := make([]string, 0, len(r.Containers))
	for id := range r.Containers {
		ids = append(ids, id)
	}
	sort.Strings(ids)
	for _, id := range ids {
		if p, err := netip.ParsePrefix(r.Containers[id].IPv4Address); err == nil {
			info.Used[p.Addr().String()] = id
		}
	}
	return info, nil
}
