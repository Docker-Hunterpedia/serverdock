//go:build integration

package dockerapi

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"net/netip"
	"os"
	"os/exec"
	"strings"
	"testing"
	"time"
)

// These tests need a running Docker and the app image in a registry on
// loopback, which CI provides (the "integration" job: registry:2 on
// 127.0.0.1:5000 and testdata/app pushed as 127.0.0.1:5000/sd-testapp:1).
// Locally: build and push that image to any registry the daemon can reach and
// set SD_TEST_APP_IMAGE to its reference; without it the tests skip.

func testSock() string {
	if s := os.Getenv("DOCKER_SOCK"); s != "" {
		return s
	}
	return DefaultSocket
}

func appImage(t *testing.T) string {
	t.Helper()
	ref := os.Getenv("SD_TEST_APP_IMAGE")
	if ref == "" {
		t.Skip("SD_TEST_APP_IMAGE is not set (see integration_docker_test.go)")
	}
	return ref
}

func randHex(n int) string {
	b := make([]byte, n)
	_, _ = rand.Read(b)
	return hex.EncodeToString(b)
}

func docker(t *testing.T, args ...string) string {
	t.Helper()
	out, err := exec.Command("docker", args...).CombinedOutput()
	if err != nil {
		t.Fatalf("docker %s: %v: %s", strings.Join(args, " "), err, out)
	}
	return strings.TrimSpace(string(out))
}

// pickSubnet returns a /24 in 10.250.0.0/16 that no Docker network uses.
func pickSubnet(t *testing.T) string {
	t.Helper()
	used := docker(t, "network", "ls", "-q")
	var inspected string
	if used != "" {
		inspected = docker(t, append([]string{"network", "inspect", "-f", "{{range .IPAM.Config}}{{.Subnet}} {{end}}"}, strings.Fields(used)...)...)
	}
	for i := 0; i < 50; i++ {
		b := make([]byte, 1)
		_, _ = rand.Read(b)
		cand := netip.PrefixFrom(netip.AddrFrom4([4]byte{10, 250, 100 + b[0]%100, 0}), 24).String()
		if !strings.Contains(inspected, cand) {
			return cand
		}
	}
	t.Skip("no free test subnet")
	return ""
}

type rig struct {
	c      *Client
	img    string
	inst   string
	net    string
	subnet string
	spec   NetworkSpec
}

func newRig(t *testing.T) *rig {
	t.Helper()
	img := appImage(t)
	c := New(testSock())
	t.Cleanup(c.Close)
	inst := randHex(8)
	r := &rig{c: c, img: img, inst: inst, net: "serverdock-it" + inst[:6], subnet: pickSubnet(t)}
	r.spec = NetworkSpec{Name: r.net, Subnet: r.subnet, Labels: map[string]string{"serverdock.instance": inst}}
	t.Cleanup(func() {
		// Containers of this test first, then the network.
		if ids, err := exec.Command("docker", "ps", "-aq", "--filter", "label=serverdock.instance="+inst).Output(); err == nil {
			for _, id := range strings.Fields(string(ids)) {
				_ = exec.Command("docker", "rm", "-f", "-v", id).Run()
			}
		}
		_ = exec.Command("docker", "network", "rm", r.net).Run()
	})
	return r
}

func (r *rig) hostIP(n int) string {
	a := netip.MustParsePrefix(r.subnet).Addr().As4()
	a[3] = byte(n)
	return netip.AddrFrom4(a).String()
}

func (r *rig) spec1(ip string, id string) CreateSpec {
	return CreateSpec{
		Image: id,
		Labels: map[string]string{
			"serverdock.managed": "1", "serverdock.instance": r.inst, "serverdock.project": "p",
			"serverdock.service": "7", "serverdock.deploy": "1", "serverdock.attempt": "1",
		},
		ExposedPorts: map[string]struct{}{"8080/tcp": {}},
		HostConfig: HostConfig{
			RestartPolicy: RestartPolicy{Name: "unless-stopped"},
			LogConfig:     LogConfig{Type: "local", Config: map[string]string{"max-size": "10m", "max-file": "3"}},
			NetworkMode:   r.net,
			PidsLimit:     4096,
		},
		NetworkingConfig: NetworkingConfig{EndpointsConfig: map[string]EndpointConfig{r.net: {IPAMConfig: EndpointIPAM{IPv4Address: ip}}}},
	}
}

func TestIntegrationDeployLifecycle(t *testing.T) {
	r := newRig(t)
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Minute)
	defer cancel()
	c := r.c

	root, err := c.DataRoot(ctx)
	if err != nil || !strings.HasPrefix(root, "/") {
		t.Fatalf("DataRoot %q %v", root, err)
	}

	var progress int
	if err := c.ImagePull(ctx, r.img, func(PullProgress) { progress++ }); err != nil {
		t.Fatal(err)
	}
	if progress == 0 {
		t.Error("no pull progress")
	}
	img, err := c.ImageInspect(ctx, r.img)
	if err != nil || !strings.HasPrefix(img.ID, "sha256:") || len(img.ExposedPorts) != 1 || img.ExposedPorts[0] != "8080/tcp" || len(img.ReservedLabelKeys) != 0 {
		t.Fatalf("%+v %v", img, err)
	}
	if _, err := c.ImageInspect(ctx, "sd-does-not-exist:1"); !errors.Is(err, ErrNotFound) {
		t.Errorf("missing image: %v", err)
	}
	if err := c.ImagePull(ctx, strings.SplitN(r.img, ":", 2)[0]+"-nope:1", nil); err == nil {
		t.Error("pulled an image that does not exist")
	}

	if _, err := c.NetworkEnsure(ctx, r.spec); err != nil {
		t.Fatal(err)
	}

	// Events: open the stream before anything starts.
	ectx, ecancel := context.WithCancel(ctx)
	defer ecancel()
	events := make(chan Event, 16)
	edone := make(chan error, 1)
	go func() {
		edone <- c.Events(ectx, []string{"serverdock.instance=" + r.inst}, func(e Event) error { events <- e; return nil })
	}()
	time.Sleep(500 * time.Millisecond)

	name := "sd-it-" + r.inst[:6]
	ip := r.hostIP(2)
	cid, err := c.ContainerCreate(ctx, name, r.spec1(ip, img.ID), okVerify)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := c.ContainerCreate(ctx, name, r.spec1(ip, img.ID), okVerify); !errors.Is(err, ErrConflict) {
		t.Fatalf("repeat create: %v", err)
	}
	byName, err := c.ContainerInspect(ctx, name)
	if err != nil || byName.ID != cid || byName.ImageID != img.ID || byName.Labels["serverdock.deploy"] != "1" {
		t.Fatalf("%+v %v", byName, err)
	}

	// A created (not started) container already holds its address in the list.
	list, err := c.ContainerList(ctx, ListOptions{All: true, Labels: []string{"serverdock.instance=" + r.inst}})
	if err != nil || len(list) != 1 || list[0].ID != cid || list[0].State != "created" || list[0].Networks[r.net].ConfiguredIPv4 != ip {
		t.Fatalf("%+v %v", list, err)
	}

	if err := c.ContainerStart(ctx, cid); err != nil {
		t.Fatal(err)
	}
	if err := c.ContainerStart(ctx, cid); !errors.Is(err, ErrNotModified) {
		t.Errorf("second start: %v", err)
	}
	info, err := c.ContainerInspect(ctx, cid)
	if err != nil || !info.State.Running || info.Networks[r.net].IPAddress != ip || info.Networks[r.net].ConfiguredIPv4 != ip || info.NetworkMode != r.net {
		t.Fatalf("%+v %v", info, err)
	}
	// Nothing is published on the host.
	ports, err := c.ContainerPorts(ctx)
	if err != nil {
		t.Fatal(err)
	}
	for _, p := range ports {
		if p.Name == name {
			t.Errorf("the app publishes host ports: %+v", p)
		}
	}
	used, err := c.NetworkInspect(ctx, r.net)
	if err != nil || used.Used[ip] != cid {
		t.Fatalf("%+v %v", used, err)
	}

	if err := c.ContainerRestart(ctx, cid, 2); err != nil {
		t.Fatal(err)
	}
	if err := c.ContainerRemove(ctx, cid); !errors.Is(err, ErrConflict) {
		t.Fatalf("remove while running: %v", err)
	}
	if err := c.Stop(ctx, cid, 2); err != nil {
		t.Fatal(err)
	}
	stopped, err := c.ContainerInspect(ctx, cid)
	if err != nil || stopped.State.Running {
		t.Fatalf("%+v %v", stopped, err)
	}
	// A stopped container is gone from network_inspect but still holds its
	// address in the list.
	used, _ = c.NetworkInspect(ctx, r.net)
	if _, ok := used.Used[ip]; ok {
		t.Error("a stopped container is listed as connected")
	}
	list, _ = c.ContainerList(ctx, ListOptions{All: true, Labels: []string{"serverdock.instance=" + r.inst}})
	if len(list) != 1 || list[0].Networks[r.net].ConfiguredIPv4 != ip {
		t.Errorf("a stopped container lost its address: %+v", list)
	}
	if err := c.ContainerRemove(ctx, cid); err != nil {
		t.Fatal(err)
	}
	if _, err := c.ContainerInspect(ctx, cid); !errors.Is(err, ErrNotFound) {
		t.Fatalf("after remove: %v", err)
	}
	if err := c.ContainerRemove(ctx, cid); !errors.Is(err, ErrNotFound) {
		t.Fatalf("second remove: %v", err)
	}

	// The events seen: start and die (from the stop), for our service.
	seen := map[string]Event{}
	deadline := time.After(10 * time.Second)
	for seen["start"].ID == "" || seen["die"].ID == "" || seen["destroy"].ID == "" {
		select {
		case e := <-events:
			seen[e.Action] = e
		case <-deadline:
			t.Fatalf("events seen: %+v", seen)
		}
	}
	if e := seen["die"]; e.ID != cid || e.Service != "7" || e.Deploy != "1" || e.ExitCode == "" {
		t.Errorf("die event: %+v", e)
	}
	ecancel()
	select {
	case err := <-edone:
		if !errors.Is(err, context.Canceled) {
			t.Errorf("events ended with %v", err)
		}
	case <-time.After(3 * time.Second):
		t.Fatal("events did not stop")
	}
}

// The gateway is explicitly .1 and the IPRange is the top /26: a container
// attached by hand gets a dynamic address from that slice, and a static address
// from the rest of the subnet (here .2) works beside it.
func TestAppNetworkGatewayIsDotOne(t *testing.T) {
	r := newRig(t)
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Minute)
	defer cancel()
	info, err := r.c.NetworkEnsure(ctx, r.spec)
	if err != nil {
		t.Fatal(err)
	}
	if info.Gateway != r.hostIP(1) {
		t.Errorf("gateway %q, want %s", info.Gateway, r.hostIP(1))
	}
	if info.Subnet != r.subnet {
		t.Errorf("subnet %q", info.Subnet)
	}
	// The raw daemon view, not our decoding of it.
	raw := docker(t, "network", "inspect", "-f", "{{range .IPAM.Config}}{{.Subnet}}|{{.Gateway}}|{{.IPRange}}{{end}}|{{index .Options \"com.docker.network.bridge.enable_icc\"}}", r.net)
	want := r.subnet + "|" + r.hostIP(1) + "|" + r.hostIP(192) + "/26|false"
	if raw != want {
		t.Fatalf("daemon says %q, want %q", raw, want)
	}

	img := appImage(t)
	if err := r.c.ImagePull(ctx, img, nil); err != nil {
		t.Fatal(err)
	}
	// By hand: dynamic address from the top /26, never .2.
	hand := docker(t, "run", "-d", "--label", "serverdock.instance="+r.inst, "--network", r.net, img)
	got := docker(t, "inspect", "-f", "{{(index .NetworkSettings.Networks \""+r.net+"\").IPAddress}}", hand)
	if !netip.MustParsePrefix(r.hostIP(192) + "/26").Contains(netip.MustParseAddr(got)) {
		t.Errorf("a hand-attached container got %s, outside the dynamic slice", got)
	}
	// A static address below the slice is accepted.
	id2, err := r.c.ImageInspect(ctx, img)
	if err != nil {
		t.Fatal(err)
	}
	cid, err := r.c.ContainerCreate(ctx, "sd-it-gw-"+r.inst[:6], r.spec1(r.hostIP(2), id2.ID), okVerify)
	if err != nil {
		t.Fatal(err)
	}
	if err := r.c.ContainerStart(ctx, cid); err != nil {
		t.Fatal(err)
	}
	ci, err := r.c.ContainerInspect(ctx, cid)
	if err != nil || ci.Networks[r.net].IPAddress != r.hostIP(2) {
		t.Fatalf("%+v %v", ci, err)
	}
}

func TestIntegrationNetworkEnsureIsIdempotentAndRefusesForeign(t *testing.T) {
	r := newRig(t)
	ctx, cancel := context.WithTimeout(context.Background(), time.Minute)
	defer cancel()
	a, err := r.c.NetworkEnsure(ctx, r.spec)
	if err != nil {
		t.Fatal(err)
	}
	b, err := r.c.NetworkEnsure(ctx, r.spec)
	if err != nil || a.ID != b.ID {
		t.Fatalf("not idempotent: %v %v %v", a.ID, b.ID, err)
	}
	other := r.spec
	other.Subnet = strings.Replace(r.subnet, "/24", "/25", 1)
	if _, err := r.c.NetworkEnsure(ctx, other); !errors.Is(err, ErrSubnetMismatch) {
		t.Errorf("another subnet: %v", err)
	}
	wrong := r.spec
	wrong.Labels = map[string]string{"serverdock.instance": "another-install"}
	if _, err := r.c.NetworkEnsure(ctx, wrong); !errors.Is(err, ErrForeignNetwork) {
		t.Errorf("another install: %v", err)
	}

	// A same-name network that someone else made, with no labels at all.
	foreign := "serverdock-itx" + r.inst[:6]
	docker(t, "network", "create", foreign)
	t.Cleanup(func() { _ = exec.Command("docker", "network", "rm", foreign).Run() })
	fs := NetworkSpec{Name: foreign, Subnet: r.subnet, Labels: r.spec.Labels}
	if _, err := r.c.NetworkEnsure(ctx, fs); !errors.Is(err, ErrForeignNetwork) {
		t.Fatalf("foreign network: %v", err)
	}
	if got := docker(t, "network", "inspect", "-f", "{{.Name}}", foreign); got != foreign {
		t.Errorf("the foreign network was touched: %q", got)
	}
}
