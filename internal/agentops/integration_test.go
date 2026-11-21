//go:build integration

package agentops

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"net/netip"
	"os"
	"os/exec"
	"strings"
	"testing"
	"time"

	"github.com/Docker-Hunterpedia/serverdock/internal/agent"
	"github.com/Docker-Hunterpedia/serverdock/internal/agent/dockerapi"
)

// These need a running Docker and the app image in a registry on loopback (the
// CI "integration" job provides both; see internal/agent/dockerapi/
// integration_docker_test.go). Without SD_TEST_APP_IMAGE they skip.

func itDocker(t *testing.T, args ...string) string {
	t.Helper()
	out, err := exec.Command("docker", args...).CombinedOutput()
	if err != nil {
		t.Fatalf("docker %s: %v: %s", strings.Join(args, " "), err, out)
	}
	return strings.TrimSpace(string(out))
}

func itRandHex(n int) string {
	b := make([]byte, n)
	_, _ = rand.Read(b)
	return hex.EncodeToString(b)
}

// itSubnet returns a /24 in 10.250.0.0/16 that no Docker network uses.
func itSubnet(t *testing.T) string {
	t.Helper()
	used := itDocker(t, "network", "ls", "-q")
	var inspected string
	if used != "" {
		inspected = itDocker(t, append([]string{"network", "inspect", "-f", "{{range .IPAM.Config}}{{.Subnet}} {{end}}"}, strings.Fields(used)...)...)
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

func TestIntegrationAgentOpsLifecycle(t *testing.T) {
	ref := os.Getenv("SD_TEST_APP_IMAGE")
	if ref == "" {
		t.Skip("SD_TEST_APP_IMAGE is not set")
	}
	sock := os.Getenv("DOCKER_SOCK")
	if sock == "" {
		sock = dockerapi.DefaultSocket
	}
	docker := dockerapi.New(sock)
	t.Cleanup(docker.Close)
	inst := itRandHex(16)
	netName := NetworkName(inst)
	subnet := itSubnet(t)
	t.Cleanup(func() {
		if ids, err := exec.Command("docker", "ps", "-aq", "--filter", "label=serverdock.instance="+inst).Output(); err == nil {
			for _, id := range strings.Fields(string(ids)) {
				_ = exec.Command("docker", "rm", "-f", "-v", id).Run()
			}
		}
		for _, n := range []string{"sd-p-web-d9-a1"} {
			_ = exec.Command("docker", "rm", "-f", "-v", n).Run()
		}
		_ = exec.Command("docker", "network", "rm", netName).Run()
	})
	sess := attachAgent(t, docker, Deps{Docker: docker, Instance: inst, CheckCreate: InterimCheckCreate, Log: quiet})
	cl := NewClient(sess)
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Minute)
	defer cancel()

	// The network: created, idempotent, and the agent refuses another name.
	n, err := cl.NetworkEnsure(ctx, netName, subnet)
	if err != nil || n.Subnet != subnet || !strings.HasSuffix(n.Gateway, ".1") {
		t.Fatalf("%+v %v", n, err)
	}
	if n2, err := cl.NetworkEnsure(ctx, netName, subnet); err != nil || n2.ID != n.ID {
		t.Fatalf("not idempotent: %+v %v", n2, err)
	}
	if _, err := cl.NetworkEnsure(ctx, "bridge", subnet); !IsCode(err, agent.CodePolicy) {
		t.Fatalf("another network name: %v", err)
	}

	// Pull, from the local registry, with the local copy removed first so
	// layers really move; then the id comes from the inspect after the pull.
	_ = exec.Command("docker", "rmi", ref).Run()
	var lines int
	pulled, err := cl.ImagePull(ctx, ref, 0, func(PullProgress) { lines++ })
	if err != nil || !imageIDRe.MatchString(pulled.ImageID) || lines == 0 {
		t.Fatalf("%+v %d lines %v", pulled, lines, err)
	}
	info, err := cl.ImageInspect(ctx, pulled.ImageID, true)
	if err != nil || info.ID != pulled.ImageID || len(info.ExposedPorts) != 1 || info.ExposedPorts[0] != "8080/tcp" || len(info.ReservedLabelKeys) != 0 {
		t.Fatalf("%+v %v", info, err)
	}
	if _, err := cl.ImagePull(ctx, strings.SplitN(ref, ":", 2)[0]+"-nope:1", 0, nil); err == nil {
		t.Fatal("pulled an image that does not exist")
	}

	// Events: prove the stream is open with a probe container first.
	ectx, ecancel := context.WithCancel(ctx)
	defer ecancel()
	events := make(chan Event, 64)
	edone := make(chan error, 1)
	go func() {
		edone <- cl.Events(ectx, func(e Event) error { events <- e; return nil })
	}()
	probed := false
	for i := 0; i < 15 && !probed; i++ {
		itDocker(t, "run", "-d", "--label", "serverdock.instance="+inst, "--label", "serverdock.service=probe", ref, "true")
		select {
		case e := <-events:
			probed = e.Service == "probe"
		case <-time.After(time.Second):
		}
	}
	if !probed {
		t.Fatal("the events op never delivered a probe event")
	}

	// Create with a static address; a repeat is "existed".
	a := goodCreate()
	a.ImageID = pulled.ImageID
	a.Spec.Image = pulled.ImageID
	a.Spec.HostConfig.NetworkMode = netName
	ip := netip.MustParsePrefix(subnet).Addr().Next().Next().String() // .2
	a.IP = ip
	a.Spec.NetworkingConfig.EndpointsConfig = map[string]dockerapi.EndpointConfig{netName: {IPAMConfig: dockerapi.EndpointIPAM{IPv4Address: ip}}}
	created, err := cl.ContainerCreate(ctx, a.Name, a.Spec, a.IP, a.ImageID)
	if err != nil || created.Existed {
		t.Fatalf("%+v %v", created, err)
	}
	again, err := cl.ContainerCreate(ctx, a.Name, a.Spec, a.IP, a.ImageID)
	if err != nil || !again.Existed || again.ID != created.ID {
		t.Fatalf("repeat: %+v %v", again, err)
	}
	st, err := cl.ContainerInspect(ctx, created.ID)
	if err != nil || !st.Owned || st.State != "created" || st.ConfiguredIP != ip || st.ImageID != pulled.ImageID || st.Labels[LabelInstance] != inst {
		t.Fatalf("%+v %v", st, err)
	}
	// A stopped container already holds its address in the list.
	list, err := cl.ContainerList(ctx)
	var found bool
	for _, c := range list.Containers {
		if c.ID == created.ID {
			found = c.ConfiguredIP == ip && !c.Unowned && c.State == "created"
		}
	}
	if err != nil || !found {
		t.Fatalf("list: %+v %v", list, err)
	}

	if err := cl.ContainerStart(ctx, created.ID); err != nil {
		t.Fatal(err)
	}
	if err := cl.ContainerStart(ctx, created.ID); err != nil {
		t.Fatalf("start twice: %v", err)
	}
	used, err := cl.NetworkInspect(ctx)
	if err != nil || len(used.Used) != 1 || used.Used[0] != ip {
		t.Fatalf("%+v %v", used, err)
	}

	// Health: reachable only where the host can route to the bridge (Linux, CI).
	var probe HealthResult
	for i := 0; i < 20; i++ {
		probe, err = cl.HealthCheck(ctx, HealthArgs{Target: ip + ":8080", Kind: KindHTTP, Path: "/", TimeoutMS: 1000})
		if err != nil {
			t.Fatal(err)
		}
		if probe.OK {
			break
		}
		time.Sleep(500 * time.Millisecond)
	}
	if !probe.OK {
		if os.Getenv("CI") != "" {
			t.Fatalf("health check failed in CI: %+v", probe)
		}
		t.Logf("the host cannot reach the bridge (Docker Desktop?): %+v", probe)
	} else if probe.Status != 200 {
		t.Errorf("status %d", probe.Status)
	}
	// The gateway is refused whatever is behind it.
	gw := netip.MustParsePrefix(subnet).Addr().Next().String()
	if _, err := cl.HealthCheck(ctx, HealthArgs{Target: gw + ":8080", Kind: KindTCP}); !IsCode(err, agent.CodePolicy) {
		t.Errorf("gateway target: %v", err)
	}

	// An unowned container is never touched, even with its full id.
	foreign := itDocker(t, "run", "-d", "--label", "app=foreign", ref)
	t.Cleanup(func() { _ = exec.Command("docker", "rm", "-f", "-v", foreign).Run() })
	if err := cl.ContainerStop(ctx, foreign, 1); !IsCode(err, agent.CodeRefusedUnowned) {
		t.Fatalf("stop of a foreign container: %v", err)
	}
	if err := cl.ContainerRemove(ctx, foreign); !IsCode(err, agent.CodeRefusedUnowned) {
		t.Fatalf("remove of a foreign container: %v", err)
	}
	if itDocker(t, "inspect", "-f", "{{.State.Running}}", foreign) != "true" {
		t.Fatal("the foreign container was stopped")
	}
	// A name taken by a container we do not own fails and leaves it alone.
	itDocker(t, "create", "--name", "sd-p-web-d9-a1", ref)
	b := a
	b.Name = "sd-p-web-d9-a1"
	if _, err := cl.ContainerCreate(ctx, b.Name, b.Spec, b.IP, b.ImageID); !IsCode(err, agent.CodeConflict) {
		t.Fatalf("name taken by a foreign container: %v", err)
	}

	// Restart, stop (a die event arrives), remove; the address is free again.
	if err := cl.ContainerRestart(ctx, created.ID, 1); err != nil {
		t.Fatal(err)
	}
	if err := cl.ContainerStop(ctx, created.ID, 1); err != nil {
		t.Fatal(err)
	}
	sawDie := false
	for !sawDie {
		select {
		case e := <-events:
			sawDie = e.Action == "die" && e.ID == created.ID
		case <-time.After(10 * time.Second):
			t.Fatal("no die event for the stopped container")
		}
	}
	if err := cl.ContainerRemove(ctx, created.ID); err != nil {
		t.Fatal(err)
	}
	if _, err := cl.ContainerInspect(ctx, created.ID); !IsCode(err, agent.CodeNotFound) {
		t.Fatalf("inspect after remove: %v", err)
	}

	// Cancelling the caller ends the events op.
	ecancel()
	select {
	case <-edone:
	case <-time.After(10 * time.Second):
		t.Fatal("events did not stop")
	}
}
