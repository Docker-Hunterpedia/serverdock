package deploy

import (
	"errors"
	"fmt"
	"strconv"

	"github.com/Docker-Hunterpedia/serverdock/internal/agent/dockerapi"
	"github.com/Docker-Hunterpedia/serverdock/internal/agentops"
	"github.com/Docker-Hunterpedia/serverdock/internal/policy"
)

// CreateInput is what the runner knows when it reaches a deploy's check stage.
type CreateInput struct {
	ProjectName, ServiceName string
	ProjectID, ServiceID     int64
	DeployID                 int64
	Attempt                  int

	ImageID       string // the resolved sha256:<64 hex> image id (never a tag)
	IP            string // the address allocated at this stage
	ContainerPort int    // 0 = a worker with no HTTP entry

	Network string // the app network's name
	Subnet  string // meta.app_subnet
}

// ContainerName is sd-<project>-<service>-d<deploy id>-a<attempt> (plan 0002).
func ContainerName(project, service string, deploy int64, attempt int) string {
	return "sd-" + project + "-" + service + "-d" + strconv.FormatInt(deploy, 10) + "-a" + strconv.Itoa(attempt)
}

// BuildCreate builds the one container the plan allows (Image = the image id,
// restart unless-stopped, the local log driver with its two options, the PID
// cap, our network and static address, five labels, the container port) and
// runs it through policy.CheckCreateControlPlane before anything is sent. The
// agent then adds serverdock.instance and runs policy.CheckCreateAgent on the
// exact bytes it gives Docker. A builder bug therefore fails here, at the check
// stage, with a policy reason.
func BuildCreate(in CreateInput) (agentops.CreateArgs, error) {
	if err := policy.ValidateName(in.ProjectName); err != nil {
		return agentops.CreateArgs{}, fmt.Errorf("project name: %w", err)
	}
	if err := policy.ValidateName(in.ServiceName); err != nil {
		return agentops.CreateArgs{}, fmt.Errorf("service name: %w", err)
	}
	if in.ProjectID < 1 || in.ServiceID < 1 || in.DeployID < 1 || in.Attempt < 1 {
		return agentops.CreateArgs{}, errors.New("ids and the attempt must be positive")
	}
	spec := dockerapi.CreateSpec{
		Image: in.ImageID,
		Labels: map[string]string{
			policy.LabelManaged: "1",
			policy.LabelProject: strconv.FormatInt(in.ProjectID, 10),
			policy.LabelService: strconv.FormatInt(in.ServiceID, 10),
			policy.LabelDeploy:  strconv.FormatInt(in.DeployID, 10),
			policy.LabelAttempt: strconv.Itoa(in.Attempt),
		},
		HostConfig: dockerapi.HostConfig{
			RestartPolicy: dockerapi.RestartPolicy{Name: "unless-stopped"},
			LogConfig:     dockerapi.LogConfig{Type: "local", Config: map[string]string{"max-size": "10m", "max-file": "3"}},
			NetworkMode:   in.Network,
			PidsLimit:     policy.PidsLimit,
		},
		NetworkingConfig: dockerapi.NetworkingConfig{EndpointsConfig: map[string]dockerapi.EndpointConfig{
			in.Network: {IPAMConfig: dockerapi.EndpointIPAM{IPv4Address: in.IP}},
		}},
	}
	if in.ContainerPort != 0 {
		if err := policy.ValidateContainerPort(in.ContainerPort); err != nil {
			return agentops.CreateArgs{}, err
		}
		spec.ExposedPorts = map[string]struct{}{strconv.Itoa(in.ContainerPort) + "/tcp": {}}
	}
	body, err := spec.Marshal()
	if err != nil {
		return agentops.CreateArgs{}, err
	}
	exp := policy.CreateExpect{
		Network: in.Network, IP: in.IP, ImageID: in.ImageID, Subnet: in.Subnet,
		Project: strconv.FormatInt(in.ProjectID, 10), Service: strconv.FormatInt(in.ServiceID, 10),
		Deploy: strconv.FormatInt(in.DeployID, 10), Attempt: strconv.Itoa(in.Attempt),
		ContainerPort: in.ContainerPort,
	}
	if err := policy.CheckCreateControlPlane(body, exp); err != nil {
		return agentops.CreateArgs{}, fmt.Errorf("the create request was refused by policy: %w", err)
	}
	return agentops.CreateArgs{
		Name: ContainerName(in.ProjectName, in.ServiceName, in.DeployID, in.Attempt),
		Spec: spec, IP: in.IP, ImageID: in.ImageID,
	}, nil
}
