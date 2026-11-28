package testbin

import (
	"net"
	"testing"
)

// The reserved addresses are distinct, stay unavailable until released and are
// available afterwards.
func TestReserveAddrsAreDistinctAndHeldUntilReleased(t *testing.T) {
	const n = 50
	addrs, release := ReserveAddrs(t, n)
	if len(addrs) != n {
		t.Fatalf("%d addresses", len(addrs))
	}
	seen := map[string]bool{}
	for _, a := range addrs {
		if seen[a] {
			t.Fatalf("%s was handed out twice", a)
		}
		seen[a] = true
		if ln, err := net.Listen("tcp", a); err == nil {
			ln.Close()
			t.Fatalf("%s can be bound while reserved", a)
		}
	}
	release()
	release() // idempotent
	for _, a := range addrs {
		ln, err := net.Listen("tcp", a)
		if err != nil {
			t.Fatalf("%s not free after release: %v", a, err)
		}
		ln.Close()
	}
}

func TestFreeAddrsAreDistinct(t *testing.T) {
	for i := 0; i < 200; i++ { // repeated FreeAddr calls could repeat a port; FreeAddrs cannot
		a := FreeAddrs(t, 3)
		if a[0] == a[1] || a[0] == a[2] || a[1] == a[2] {
			t.Fatalf("duplicate in %v", a)
		}
	}
}
