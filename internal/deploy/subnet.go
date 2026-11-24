// Package deploy holds the deploy path's shared pieces: the app subnet, the
// static address allocator and deploy-row retention (plan 0004). The runner
// itself arrives in steps 8a and 8b.
//
// No package-level work: this package is linked into agent mode too.
package deploy

import (
	"bufio"
	"context"
	"database/sql"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"math/bits"
	"net"
	"net/netip"
	"os"
	"strconv"
	"strings"
	"sync"

	"github.com/Docker-Hunterpedia/serverdock/internal/agent/dockerapi"
	"github.com/Docker-Hunterpedia/serverdock/internal/agentops"
	"github.com/Docker-Hunterpedia/serverdock/internal/store"
)

// metaAppSubnet is the meta key that records the app subnet. It is written once,
// at first start, and never changed afterwards.
const metaAppSubnet = "app_subnet"

// Subnet size bounds for --app-subnet: /24 (190 static addresses) to /20.
const (
	minSubnetBits = 20
	maxSubnetBits = 24
)

// DefaultPool is where the first-start choice looks for a free /24.
const DefaultPool = "10.250.0.0/16"

// ParseAppSubnet validates a --app-subnet value: a canonical IPv4 CIDR (host bits
// zero, no spaces) from /24 to /20 that lies wholly inside private space: RFC 1918
// (10.0.0.0/8, 172.16.0.0/12, 192.168.0.0/16) or 100.64.0.0/10. A public range
// would shadow real hosts for every container.
func ParseAppSubnet(s string) (netip.Prefix, error) {
	p, err := netip.ParsePrefix(s)
	switch {
	case err != nil || !p.Addr().Is4() || p.String() != s:
		return netip.Prefix{}, errors.New("must be a canonical IPv4 CIDR such as 10.250.0.0/24")
	case p.Masked() != p:
		return netip.Prefix{}, fmt.Errorf("host bits are set: did you mean %s", p.Masked())
	case p.Bits() < minSubnetBits || p.Bits() > maxSubnetBits:
		return netip.Prefix{}, fmt.Errorf("the size must be /%d to /%d", minSubnetBits, maxSubnetBits)
	}
	private := false
	for _, r := range [...]string{"10.0.0.0/8", "172.16.0.0/12", "192.168.0.0/16", "100.64.0.0/10"} {
		if rp := netip.MustParsePrefix(r); rp.Bits() <= p.Bits() && rp.Contains(p.Addr()) {
			private = true
		}
	}
	if !private {
		return netip.Prefix{}, errors.New("must be a private range: inside 10.0.0.0/8, 172.16.0.0/12, 192.168.0.0/16 or 100.64.0.0/10")
	}
	return p, nil
}

// Used is a range that is already taken, and by what.
type Used struct {
	What   string
	Prefix netip.Prefix
}

// ChooseSubnet returns the first /24 of DefaultPool that overlaps none of avoid.
func ChooseSubnet(avoid []Used) (netip.Prefix, error) {
	pool := netip.MustParsePrefix(DefaultPool)
	base := pool.Addr().As4()
	for third := 0; third < 256; third++ {
		cand := netip.PrefixFrom(netip.AddrFrom4([4]byte{base[0], base[1], byte(third), 0}), 24)
		taken := false
		for _, u := range avoid {
			if u.Prefix.Overlaps(cand) {
				taken = true
				break
			}
		}
		if !taken {
			return cand, nil
		}
	}
	return netip.Prefix{}, fmt.Errorf("no free /24 in %s: set --app-subnet", DefaultPool)
}

// routeFile is the Linux routing table.
const routeFile = "/proc/net/route"

// HostNetworks lists the IPv4 ranges the host already uses: every interface
// address and, on Linux, every non-default route. Interfaces and routes named
// excludeIface (our own app network's bridge) are skipped, so the app network
// does not count as overlapping itself.
func HostNetworks(excludeIface string) ([]Used, error) {
	return hostNetworks(routeFile, excludeIface)
}

func hostNetworks(routes, excludeIface string) ([]Used, error) {
	var out []Used
	ifaces, err := net.Interfaces()
	if err != nil {
		return nil, fmt.Errorf("list interfaces: %w", err)
	}
	for _, ifc := range ifaces {
		if excludeIface != "" && ifc.Name == excludeIface {
			continue
		}
		addrs, err := ifc.Addrs()
		if err != nil {
			return nil, fmt.Errorf("list addresses of %s: %w", ifc.Name, err)
		}
		for _, a := range addrs {
			if ipn, ok := a.(*net.IPNet); ok {
				if p, ok := netip.AddrFromSlice(ipn.IP); ok && p.Unmap().Is4() {
					ones, _ := ipn.Mask.Size()
					out = append(out, Used{What: "the address of host interface " + ifc.Name, Prefix: netip.PrefixFrom(p.Unmap(), ones).Masked()})
				}
			}
		}
	}
	f, err := os.Open(routes)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return out, nil // not Linux: interface addresses are all there is
		}
		return nil, err
	}
	defer f.Close()
	r, err := parseRoutes(f, excludeIface)
	return append(out, r...), err
}

// parseRoutes reads /proc/net/route. Destination and Mask are little-endian hex.
// The default route (mask 0) is not an overlap with anything in particular.
func parseRoutes(r io.Reader, excludeIface string) ([]Used, error) {
	var out []Used
	sc := bufio.NewScanner(r)
	sc.Buffer(make([]byte, 0, 4096), 64<<10)
	first := true
	for sc.Scan() {
		if first {
			first = false // header
			continue
		}
		f := strings.Fields(sc.Text())
		if len(f) < 8 {
			continue
		}
		iface := f[0]
		if excludeIface != "" && iface == excludeIface {
			continue
		}
		dest, err1 := strconv.ParseUint(f[1], 16, 32)
		mask, err2 := strconv.ParseUint(f[7], 16, 32)
		if err1 != nil || err2 != nil || mask == 0 {
			continue
		}
		m := bits.ReverseBytes32(uint32(mask)) // host order
		ones := bits.OnesCount32(m)
		if m != ^uint32(0)<<(32-ones) {
			continue // not a contiguous mask
		}
		d := bits.ReverseBytes32(uint32(dest))
		p := netip.PrefixFrom(netip.AddrFrom4([4]byte{byte(d >> 24), byte(d >> 16), byte(d >> 8), byte(d)}), ones).Masked()
		out = append(out, Used{What: "the host route via " + iface, Prefix: p})
	}
	return out, sc.Err()
}

// DockerNetworks is the part of the Docker client AppNet needs.
type DockerNetworks interface {
	NetworkSubnets(ctx context.Context) ([]dockerapi.NetworkSubnet, error)
}

// AppNetOptions configures an AppNet.
type AppNetOptions struct {
	Store *store.Store
	// Instance is the install id; it names our own network, which is never an
	// overlap with itself.
	Instance string
	// Flag is --app-subnet ("" when not given). It applies at first start only.
	Flag   string
	Docker DockerNetworks
	// Host lists the host's ranges, skipping the named bridge interface. Nil means HostNetworks.
	Host func(excludeIface string) ([]Used, error)
	Log  *slog.Logger
}

// AppNet owns the recorded app subnet (meta.app_subnet): it chooses and records
// it at first start, and checks later that it still overlaps nothing. It has no
// goroutine and no timer; it works only when called.
type AppNet struct {
	o      AppNetOptions
	mu     sync.Mutex
	subnet netip.Prefix
}

// NewAppNet validates the flag (so a bad value fails before anything starts) and
// returns an AppNet.
func NewAppNet(o AppNetOptions) (*AppNet, error) {
	if o.Store == nil || o.Docker == nil {
		return nil, errors.New("deploy: AppNet needs a Store and a Docker client")
	}
	if o.Flag != "" {
		if _, err := ParseAppSubnet(o.Flag); err != nil {
			return nil, fmt.Errorf("--app-subnet %q: %w", o.Flag, err)
		}
	}
	if o.Host == nil {
		o.Host = HostNetworks
	}
	if o.Log == nil {
		o.Log = slog.New(slog.DiscardHandler)
	}
	return &AppNet{o: o}, nil
}

// Recorded reads meta.app_subnet and nothing else: it never chooses or records a
// subnet and never calls Docker, and it keeps no copy (every call reads the
// row). It returns "" while none is recorded. The agent's proxy_apply uses it to
// check upstreams without a Docker call (plan 0004 step 6b).
func (a *AppNet) Recorded(ctx context.Context) (string, error) {
	var v string
	err := a.o.Store.R.QueryRowContext(ctx, `SELECT value FROM meta WHERE key = ?`, metaAppSubnet).Scan(&v)
	switch {
	case errors.Is(err, sql.ErrNoRows):
		return "", nil
	case err != nil:
		return "", err
	}
	if p, perr := netip.ParsePrefix(v); perr != nil || !p.Addr().Is4() || p.Masked() != p {
		return "", fmt.Errorf("deploy: meta.app_subnet %q is not a valid IPv4 CIDR", v)
	}
	return v, nil
}

// Subnet returns the recorded app subnet, choosing and recording it first if this
// is the first call ever. The choice needs Docker (every existing network must be
// avoided): while Docker is unreachable nothing is recorded and the error says so,
// and the next call tries again. A flag given after the first start is ignored
// with a warning.
func (a *AppNet) Subnet(ctx context.Context) (netip.Prefix, error) {
	a.mu.Lock()
	defer a.mu.Unlock()
	if a.subnet.IsValid() {
		return a.subnet, nil
	}
	var v string
	err := a.o.Store.R.QueryRowContext(ctx, `SELECT value FROM meta WHERE key = ?`, metaAppSubnet).Scan(&v)
	switch {
	case err == nil:
		p, perr := netip.ParsePrefix(v)
		if perr != nil || !p.Addr().Is4() || p.Masked() != p {
			return netip.Prefix{}, fmt.Errorf("deploy: meta.app_subnet %q is not a valid IPv4 CIDR", v)
		}
		if a.o.Flag != "" && a.o.Flag != v {
			a.o.Log.Warn("--app-subnet is ignored: the app subnet is chosen once, at first start", "recorded", v, "flag", a.o.Flag)
		}
		a.subnet = p
		return p, nil
	case !errors.Is(err, sql.ErrNoRows):
		return netip.Prefix{}, err
	}
	var chosen netip.Prefix
	if a.o.Flag != "" {
		chosen, _ = ParseAppSubnet(a.o.Flag) // validated in NewAppNet
		// An explicit range is still checked before it is saved: an overlap
		// refuses the start and nothing is recorded. Docker's networks are
		// included when Docker answers (if it does not, the per-deploy check
		// catches an overlap later).
		nets, _ := a.o.Docker.NetworkSubnets(ctx)
		if err := a.overlap(chosen, nets, true); err != nil {
			return netip.Prefix{}, err
		}
	} else {
		nets, err := a.o.Docker.NetworkSubnets(ctx)
		if err != nil {
			return netip.Prefix{}, fmt.Errorf("deploy: cannot choose the app subnet until Docker answers: %w", err)
		}
		host, err := a.o.Host("")
		if err != nil {
			return netip.Prefix{}, fmt.Errorf("deploy: cannot read the host's networks: %w", err)
		}
		avoid := host
		for _, n := range nets {
			for _, p := range n.Subnets {
				avoid = append(avoid, Used{What: "the Docker network " + n.Name, Prefix: p})
			}
		}
		if chosen, err = ChooseSubnet(avoid); err != nil {
			return netip.Prefix{}, err
		}
	}
	// Written once; if another caller won the race, its value is the one in use.
	err = a.o.Store.WriteTx(ctx, func(tx *sql.Tx) error {
		_, err := tx.ExecContext(ctx, `INSERT OR IGNORE INTO meta(key, value) VALUES (?, ?)`, metaAppSubnet, chosen.String())
		if err != nil {
			return err
		}
		return tx.QueryRowContext(ctx, `SELECT value FROM meta WHERE key = ?`, metaAppSubnet).Scan(&v)
	})
	if err != nil {
		return netip.Prefix{}, err
	}
	if a.subnet, err = netip.ParsePrefix(v); err != nil {
		return netip.Prefix{}, err
	}
	a.o.Log.Info("app subnet recorded", "subnet", a.subnet.String())
	return a.subnet, nil
}

// OverlapError is the refusal when an app subnet overlaps something on the host.
// FirstStart is true for a --app-subnet given at first start (the server refuses
// to start and records nothing); otherwise the recorded subnet now overlaps
// something (a VPN added later, say): new deploys refuse with it and running apps
// are unaffected.
type OverlapError struct {
	Subnet     netip.Prefix
	What       string
	FirstStart bool
}

func (e *OverlapError) Error() string {
	if e.FirstStart {
		return fmt.Sprintf("--app-subnet %s overlaps %s; choose another range", e.Subnet, e.What)
	}
	return fmt.Sprintf("the app subnet %s now overlaps %s; changing the app subnet of an existing install is not supported yet (see the plan 0009 doctor check)", e.Subnet, e.What)
}

// Check verifies the recorded subnet against the host's interfaces and routes and
// the other Docker networks (our own network and its bridge are skipped). It
// returns an *OverlapError, or another error when Docker or the host cannot be
// read.
func (a *AppNet) Check(ctx context.Context) error {
	subnet, err := a.Subnet(ctx)
	if err != nil {
		return err
	}
	nets, err := a.o.Docker.NetworkSubnets(ctx)
	if err != nil {
		return fmt.Errorf("list Docker networks: %w", err)
	}
	return a.overlap(subnet, nets, false)
}

// overlap compares subnet with the host's interfaces and routes and with the
// Docker networks in nets (our own network and its bridge are skipped).
func (a *AppNet) overlap(subnet netip.Prefix, nets []dockerapi.NetworkSubnet, firstStart bool) error {
	ours := agentops.NetworkName(a.o.Instance)
	exclude := ""
	for _, n := range nets {
		if n.Name == ours && len(n.ID) >= 12 {
			exclude = "br-" + n.ID[:12]
		}
	}
	host, err := a.o.Host(exclude)
	if err != nil {
		return fmt.Errorf("read the host's networks: %w", err)
	}
	for _, u := range host {
		if u.Prefix.Overlaps(subnet) {
			return &OverlapError{Subnet: subnet, What: u.What + " (" + u.Prefix.String() + ")", FirstStart: firstStart}
		}
	}
	for _, n := range nets {
		if n.Name == ours {
			continue
		}
		for _, p := range n.Subnets {
			if p.Overlaps(subnet) {
				return &OverlapError{Subnet: subnet, What: fmt.Sprintf("the Docker network %q (%s)", n.Name, p), FirstStart: firstStart}
			}
		}
	}
	return nil
}
