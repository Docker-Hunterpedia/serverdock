package agentops

import (
	"errors"
	"fmt"
	"net/netip"

	"github.com/Docker-Hunterpedia/serverdock/internal/agent/dockerapi"
)

// StaticAddr reports whether ip is an address ServerDock may give a container
// on the network described by n: a host address of the subnet that is not the
// network address, the gateway, the broadcast address or inside the dynamic
// IPRange (which Docker hands out to hand-attached containers). It is the
// "static range" of plan 0004. The gateway and range are checked both as
// Docker reports them and as NetworkEnsure derives them, so a network that
// drifted cannot widen the range.
func StaticAddr(n dockerapi.NetworkInfo, ip netip.Addr) error {
	subnet, err := netip.ParsePrefix(n.Subnet)
	if err != nil || !subnet.Addr().Is4() || subnet.Bits() > 30 {
		return errors.New("the app network has no usable IPv4 subnet")
	}
	if !ip.Is4() || !subnet.Contains(ip) {
		return fmt.Errorf("%s is not in the app subnet %s", ip, subnet)
	}
	if ip == subnet.Addr() {
		return fmt.Errorf("%s is the network address", ip)
	}
	if ip == lastAddr(subnet) {
		return fmt.Errorf("%s is the broadcast address", ip)
	}
	if ip == subnet.Addr().Next() {
		return fmt.Errorf("%s is the gateway", ip)
	}
	if gw, err := netip.ParseAddr(n.Gateway); err == nil && ip == gw {
		return fmt.Errorf("%s is the gateway", ip)
	}
	dyn, err := dockerapi.DynamicRange(n.Subnet)
	if err != nil {
		return err
	}
	if dyn.Contains(ip) {
		return fmt.Errorf("%s is in the dynamic range %s", ip, dyn)
	}
	if r, err := netip.ParsePrefix(n.IPRange); err == nil && r.Contains(ip) {
		return fmt.Errorf("%s is in the dynamic range %s", ip, r)
	}
	return nil
}

// lastAddr is the highest address of an IPv4 prefix (its broadcast address).
func lastAddr(p netip.Prefix) netip.Addr {
	a := p.Masked().Addr().As4()
	host := uint32(1)<<(32-p.Bits()) - 1
	n := uint32(a[0])<<24 | uint32(a[1])<<16 | uint32(a[2])<<8 | uint32(a[3])
	n |= host
	return netip.AddrFrom4([4]byte{byte(n >> 24), byte(n >> 16), byte(n >> 8), byte(n)})
}
