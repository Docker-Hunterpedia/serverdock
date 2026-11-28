package policy

import (
	"errors"
	"fmt"
	"net/netip"
)

// StaticAddr reports whether ip is an address ServerDock may give a container
// on the app network: a host address of the subnet that is not the network
// address, the gateway, the broadcast address or inside the dynamic IPRange
// (which Docker hands out to hand-attached containers). gateway and ipRange may
// be empty (a caller that only knows meta.app_subnet); when given they are
// checked as well as the values NetworkEnsure derives, so a network that
// drifted cannot widen the range.
func StaticAddr(subnetStr, gateway, ipRange string, ip netip.Addr) error {
	subnet, err := netip.ParsePrefix(subnetStr)
	if err != nil || !subnet.Addr().Is4() || subnet.Bits() > 30 {
		return errors.New("the app network has no usable IPv4 subnet")
	}
	if ip.IsLoopback() || ip.IsLinkLocalUnicast() || ip.IsUnspecified() || ip.IsMulticast() {
		return fmt.Errorf("%s is not a container address", ip)
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
	if gw, err := netip.ParseAddr(gateway); err == nil && ip == gw {
		return fmt.Errorf("%s is the gateway", ip)
	}
	dyn, err := DynamicRange(subnetStr)
	if err != nil {
		return err
	}
	if dyn.Contains(ip) {
		return fmt.Errorf("%s is in the dynamic range %s", ip, dyn)
	}
	if r, err := netip.ParsePrefix(ipRange); err == nil && r.Contains(ip) {
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

// ErrInvalidSubnet is a subnet the app network may not use.
var ErrInvalidSubnet = errors.New("subnet must be a canonical IPv4 CIDR from /16 to /26")

// NetworkAddrs derives the gateway (the subnet's .1) and the dynamic IPRange
// (the top quarter: a /26 of a /24) of an app subnet. It accepts /16 to /26; the
// --app-subnet flag narrows that to /24 to /20 (step 4a).
func NetworkAddrs(subnet string) (gateway netip.Addr, ipRange netip.Prefix, err error) {
	p, err := netip.ParsePrefix(subnet)
	if err != nil || !p.Addr().Is4() || p.Masked() != p || p.Bits() < 16 || p.Bits() > 26 {
		return netip.Addr{}, netip.Prefix{}, ErrInvalidSubnet
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
// created by the app network code (its top quarter).
func DynamicRange(subnet string) (netip.Prefix, error) {
	_, r, err := NetworkAddrs(subnet)
	return r, err
}
