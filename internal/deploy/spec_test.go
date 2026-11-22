package deploy

import (
	"strings"
	"testing"

	"github.com/Docker-Hunterpedia/serverdock/internal/policy"
)

const (
	specInstance = "0123456789abcdef0123456789abcdef"
	specImg      = "sha256:aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"
)

func goodInput() CreateInput {
	return CreateInput{
		ProjectName: "shop", ServiceName: "web", ProjectID: 3, ServiceID: 7, DeployID: 12, Attempt: 1,
		ImageID: specImg, IP: "10.250.7.2", ContainerPort: 8080,
		Network: "serverdock-01234567", Subnet: "10.250.7.0/24",
	}
}

// TestBuildCreateOutputPassesInBothProcesses: the control plane's check ran in
// BuildCreate; the agent adds the instance label and checks the exact bytes
// again with its own function.
func TestBuildCreateOutputPassesInBothProcesses(t *testing.T) {
	in := goodInput()
	a, err := BuildCreate(in)
	if err != nil {
		t.Fatal(err)
	}
	if a.Name != "sd-shop-web-d12-a1" || a.IP != in.IP || a.ImageID != specImg || a.Spec.Image != specImg {
		t.Fatalf("%+v", a)
	}
	exp := policy.CreateExpect{
		Instance: specInstance, Network: in.Network, IP: in.IP, ImageID: in.ImageID, Subnet: in.Subnet,
		Project: "3", Service: "7", Deploy: "12", Attempt: "1", ContainerPort: 8080,
	}
	five, _ := a.Spec.Marshal()
	if err := policy.CheckCreateControlPlane(five, exp); err != nil {
		t.Fatalf("control plane: %v", err)
	}
	if err := policy.CheckCreateAgent(five, exp); err == nil {
		t.Fatal("the agent accepted a body without serverdock.instance")
	}
	// What the agent does: add the sixth label.
	spec := a.Spec
	spec.Labels = map[string]string{policy.LabelInstance: specInstance}
	for k, v := range a.Spec.Labels {
		spec.Labels[k] = v
	}
	six, _ := spec.Marshal()
	if err := policy.CheckCreateAgent(six, exp); err != nil {
		t.Fatalf("agent: %v", err)
	}
	// A worker has no container port and no ExposedPorts.
	in.ContainerPort = 0
	w, err := BuildCreate(in)
	if err != nil || w.Spec.ExposedPorts != nil {
		t.Fatalf("worker: %+v %v", w, err)
	}
	wb, _ := w.Spec.Marshal()
	wexp := exp
	wexp.ContainerPort, wexp.Worker = 0, true
	if err := policy.CheckCreateControlPlane(wb, wexp); err != nil {
		t.Fatalf("worker body: %v", err)
	}
}

func TestBuildCreateRefusesBadInputBeforeAnythingIsSent(t *testing.T) {
	for name, mod := range map[string]func(*CreateInput){
		"bad project name":   func(i *CreateInput) { i.ProjectName = "Shop" },
		"bad service name":   func(i *CreateInput) { i.ServiceName = "web-" },
		"zero deploy":        func(i *CreateInput) { i.DeployID = 0 },
		"zero attempt":       func(i *CreateInput) { i.Attempt = 0 },
		"image is a tag":     func(i *CreateInput) { i.ImageID = "nginx:1" },
		"port out of range":  func(i *CreateInput) { i.ContainerPort = 70000 },
		"the gateway":        func(i *CreateInput) { i.IP = "10.250.7.1" },
		"dynamic range":      func(i *CreateInput) { i.IP = "10.250.7.250" },
		"outside the subnet": func(i *CreateInput) { i.IP = "10.250.8.2" },
		"no network":         func(i *CreateInput) { i.Network = "" },
		"network host":       func(i *CreateInput) { i.Network = "host" },
		"network bridge":     func(i *CreateInput) { i.Network = "bridge" },
		"network none":       func(i *CreateInput) { i.Network = "none" },
		"network default":    func(i *CreateInput) { i.Network = "default" },
		"network container":  func(i *CreateInput) { i.Network = "container:x" },
		"network Host":       func(i *CreateInput) { i.Network = "Host" },
		"host network name":  func(i *CreateInput) { i.Network = "host net" },
	} {
		in := goodInput()
		mod(&in)
		if _, err := BuildCreate(in); err == nil {
			t.Errorf("%s: accepted", name)
		}
	}
	if n := ContainerName(strings.Repeat("a", 30), strings.Repeat("b", 30), 1<<62, 99); len(n) > 128 {
		t.Errorf("container name %d bytes", len(n))
	}
}
