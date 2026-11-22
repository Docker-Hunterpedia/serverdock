package agentops

import (
	"net/netip"

	"github.com/Docker-Hunterpedia/serverdock/internal/agent/dockerapi"
	"github.com/Docker-Hunterpedia/serverdock/internal/policy"
)

// StaticAddr reports whether ip is an address ServerDock may give a container
// on the network described by n (policy.StaticAddr, the "static range" of plan
// 0004).
func StaticAddr(n dockerapi.NetworkInfo, ip netip.Addr) error {
	return policy.StaticAddr(n.Subnet, n.Gateway, n.IPRange, ip)
}
