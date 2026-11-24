package policy

import (
	"net/netip"
	"testing"
)

func TestStaticAddrOnABiggerSubnet(t *testing.T) {
	for ip, ok := range map[string]bool{
		"10.250.16.2": true, "10.250.28.255": false, "10.250.16.1": false, "10.250.16.0": false, "10.250.31.255": false,
		"10.250.16.3": true, "10.250.15.9": false, "10.250.28.0": false, "10.250.27.254": true,
	} {
		err := StaticAddr("10.250.16.0/20", "", "", netip.MustParseAddr(ip))
		if (err == nil) != ok {
			t.Errorf("%s: err=%v want ok=%v", ip, err, ok)
		}
	}
}

func TestStaticAddrOnADefaultSubnet(t *testing.T) {
	for ip, ok := range map[string]bool{
		"10.250.7.2": true, "10.250.7.191": true, "10.250.7.192": false, "10.250.7.255": false,
		"10.250.7.1": false, "10.250.7.0": false, "10.250.6.9": false, "::1": false,
	} {
		err := StaticAddr("10.250.7.0/24", "10.250.7.1", "10.250.7.192/26", netip.MustParseAddr(ip))
		if (err == nil) != ok {
			t.Errorf("%s: err=%v want ok=%v", ip, err, ok)
		}
	}
	for _, s := range []string{"", "bogus", "10.250.7.0/31", "fd00::/64", "10.250.7.0/8"} {
		if StaticAddr(s, "", "", netip.MustParseAddr("10.250.7.2")) == nil {
			t.Errorf("subnet %q accepted", s)
		}
	}
}

func TestStaticAddrRefusesLoopbackLinkLocalAndUnspecifiedOutright(t *testing.T) {
	for _, ip := range []string{"127.0.0.2", "169.254.169.254", "0.0.0.0", "224.0.0.1"} {
		if err := StaticAddr("10.250.7.0/24", "", "", netip.MustParseAddr(ip)); err == nil {
			t.Errorf("%s accepted", ip)
		}
	}
	// Even a damaged subnet that contains them does not make them acceptable.
	if err := StaticAddr("127.0.0.0/24", "", "", netip.MustParseAddr("127.0.0.2")); err == nil {
		t.Error("a loopback subnet accepted loopback")
	}
}
