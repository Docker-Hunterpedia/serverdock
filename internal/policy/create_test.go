package policy

import (
	"encoding/json"
	"strings"
	"testing"
)

const (
	tInstance = "0123456789abcdef0123456789abcdef"
	tNet      = "serverdock-01234567"
	tIP       = "10.250.7.2"
	tSubnet   = "10.250.7.0/24"
	tImg      = "sha256:aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"
	tOtherImg = "sha256:bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb"
)

func goodExp() CreateExpect {
	return CreateExpect{
		Instance: tInstance, Network: tNet, IP: tIP, ImageID: tImg, Subnet: tSubnet,
		Project: "3", Service: "7", Deploy: "12", Attempt: "1", ContainerPort: 8080,
	}
}

// tree is the allowed body as the builder marshals it, written out so this
// package does not depend on the Docker client.
func tree(t testing.TB, withInstance bool) map[string]any {
	t.Helper()
	l := map[string]any{LabelManaged: "1", LabelProject: "3", LabelService: "7", LabelDeploy: "12", LabelAttempt: "1"}
	if withInstance {
		l[LabelInstance] = tInstance
	}
	return map[string]any{
		"Image":        tImg,
		"Labels":       l,
		"ExposedPorts": map[string]any{"8080/tcp": map[string]any{}},
		"HostConfig": map[string]any{
			"RestartPolicy": map[string]any{"Name": "unless-stopped"},
			"LogConfig":     map[string]any{"Type": "local", "Config": map[string]any{"max-size": "10m", "max-file": "3"}},
			"NetworkMode":   tNet, "PidsLimit": 4096,
		},
		"NetworkingConfig": map[string]any{"EndpointsConfig": map[string]any{
			tNet: map[string]any{"IPAMConfig": map[string]any{"IPv4Address": tIP}}}},
	}
}

func sub(m map[string]any, path ...string) map[string]any {
	for _, p := range path {
		m = m[p].(map[string]any)
	}
	return m
}

func enc(m map[string]any) []byte { b, _ := json.Marshal(m); return b }

// refusals is the fixture table: one entry per refused key and per wrong leaf.
// Each is applied to a tree of the agent's (six-label) body.
var refusals = map[string]func(m map[string]any){
	// the "anything else" list
	"Privileged":      func(m map[string]any) { sub(m, "HostConfig")["Privileged"] = true },
	"PidMode":         func(m map[string]any) { sub(m, "HostConfig")["PidMode"] = "host" },
	"IpcMode":         func(m map[string]any) { sub(m, "HostConfig")["IpcMode"] = "host" },
	"UTSMode":         func(m map[string]any) { sub(m, "HostConfig")["UTSMode"] = "host" },
	"UsernsMode":      func(m map[string]any) { sub(m, "HostConfig")["UsernsMode"] = "host" },
	"CapAdd":          func(m map[string]any) { sub(m, "HostConfig")["CapAdd"] = []string{"SYS_ADMIN"} },
	"Devices":         func(m map[string]any) { sub(m, "HostConfig")["Devices"] = []any{} },
	"DeviceRequests":  func(m map[string]any) { sub(m, "HostConfig")["DeviceRequests"] = []any{} },
	"Binds":           func(m map[string]any) { sub(m, "HostConfig")["Binds"] = []string{"/:/host"} },
	"Mounts":          func(m map[string]any) { sub(m, "HostConfig")["Mounts"] = []any{} },
	"VolumesFrom":     func(m map[string]any) { sub(m, "HostConfig")["VolumesFrom"] = []string{"x"} },
	"PortBindings":    func(m map[string]any) { sub(m, "HostConfig")["PortBindings"] = map[string]any{} },
	"PublishAllPorts": func(m map[string]any) { sub(m, "HostConfig")["PublishAllPorts"] = true },
	"SecurityOpt":     func(m map[string]any) { sub(m, "HostConfig")["SecurityOpt"] = []string{"seccomp=unconfined"} },
	"Sysctls":         func(m map[string]any) { sub(m, "HostConfig")["Sysctls"] = map[string]any{"a": "1"} },
	"CgroupParent":    func(m map[string]any) { sub(m, "HostConfig")["CgroupParent"] = "x" },
	"Env":             func(m map[string]any) { m["Env"] = []string{"A=1"} },
	"Cmd":             func(m map[string]any) { m["Cmd"] = []string{"sh"} },
	"Entrypoint":      func(m map[string]any) { m["Entrypoint"] = []string{"sh"} },
	"User":            func(m map[string]any) { m["User"] = "root" },
	"Hostname":        func(m map[string]any) { m["Hostname"] = "x" },
	"Volumes":         func(m map[string]any) { m["Volumes"] = map[string]any{"/x": map[string]any{}} },
	"WorkingDir":      func(m map[string]any) { m["WorkingDir"] = "/" },
	"Aliases": func(m map[string]any) {
		sub(m, "NetworkingConfig", "EndpointsConfig", tNet)["Aliases"] = []string{"db"}
	},
	"Links":           func(m map[string]any) { sub(m, "NetworkingConfig", "EndpointsConfig", tNet)["Links"] = []string{"x"} },
	"top-level extra": func(m map[string]any) { m["Tty"] = true },
	"HostConfig extra Memory": func(m map[string]any) {
		sub(m, "HostConfig")["Memory"] = 1
	},

	// case variants of keys that matter: Docker's decoder would read them
	"case-variant privileged": func(m map[string]any) { sub(m, "HostConfig")["privileged"] = true },
	"case-variant hostconfig": func(m map[string]any) { m["hostconfig"] = map[string]any{"Privileged": true} },
	"case-variant binds":      func(m map[string]any) { sub(m, "HostConfig")["BINDS"] = []string{"/:/h"} },
	"case-variant Image":      func(m map[string]any) { m["image"] = tOtherImg },
	"case-variant label": func(m map[string]any) {
		sub(m, "Labels")["SERVERDOCK.MANAGED"] = "1"
	},
	"case-variant PidsLimit": func(m map[string]any) { sub(m, "HostConfig")["pidslimit"] = 0 },

	// RestartPolicy
	"restart always":      func(m map[string]any) { sub(m, "HostConfig", "RestartPolicy")["Name"] = "always" },
	"restart on-failure":  func(m map[string]any) { sub(m, "HostConfig", "RestartPolicy")["Name"] = "on-failure" },
	"restart empty":       func(m map[string]any) { sub(m, "HostConfig", "RestartPolicy")["Name"] = "" },
	"restart retry count": func(m map[string]any) { sub(m, "HostConfig", "RestartPolicy")["MaximumRetryCount"] = 3 },
	"restart extra key":   func(m map[string]any) { sub(m, "HostConfig", "RestartPolicy")["X"] = 1 },
	"restart not object":  func(m map[string]any) { sub(m, "HostConfig")["RestartPolicy"] = "unless-stopped" },

	// LogConfig
	"log json-file":    func(m map[string]any) { sub(m, "HostConfig", "LogConfig")["Type"] = "json-file" },
	"log none":         func(m map[string]any) { sub(m, "HostConfig", "LogConfig")["Type"] = "none" },
	"log extra option": func(m map[string]any) { sub(m, "HostConfig", "LogConfig", "Config")["labels"] = "x" },
	"log third key":    func(m map[string]any) { sub(m, "HostConfig", "LogConfig", "Config")["tag"] = "x" },
	"log size":         func(m map[string]any) { sub(m, "HostConfig", "LogConfig", "Config")["max-size"] = "1g" },
	"log file":         func(m map[string]any) { sub(m, "HostConfig", "LogConfig", "Config")["max-file"] = "30" },
	"log missing file": func(m map[string]any) { delete(sub(m, "HostConfig", "LogConfig", "Config"), "max-file") },
	"log extra field":  func(m map[string]any) { sub(m, "HostConfig", "LogConfig")["Extra"] = 1 },

	// NetworkMode
	"NetworkMode host":      func(m map[string]any) { sub(m, "HostConfig")["NetworkMode"] = "host" },
	"NetworkMode bridge":    func(m map[string]any) { sub(m, "HostConfig")["NetworkMode"] = "bridge" },
	"NetworkMode none":      func(m map[string]any) { sub(m, "HostConfig")["NetworkMode"] = "none" },
	"NetworkMode container": func(m map[string]any) { sub(m, "HostConfig")["NetworkMode"] = "container:x" },
	"NetworkMode other":     func(m map[string]any) { sub(m, "HostConfig")["NetworkMode"] = "other-net" },
	"NetworkMode empty":     func(m map[string]any) { sub(m, "HostConfig")["NetworkMode"] = "" },

	// PidsLimit
	"PidsLimit 0":      func(m map[string]any) { sub(m, "HostConfig")["PidsLimit"] = 0 },
	"PidsLimit -1":     func(m map[string]any) { sub(m, "HostConfig")["PidsLimit"] = -1 },
	"PidsLimit 4097":   func(m map[string]any) { sub(m, "HostConfig")["PidsLimit"] = 4097 },
	"PidsLimit 4095":   func(m map[string]any) { sub(m, "HostConfig")["PidsLimit"] = 4095 },
	"PidsLimit string": func(m map[string]any) { sub(m, "HostConfig")["PidsLimit"] = "4096" },
	"PidsLimit missing": func(m map[string]any) {
		delete(sub(m, "HostConfig"), "PidsLimit")
	},

	// EndpointsConfig
	"endpoint another network": func(m map[string]any) {
		ec := sub(m, "NetworkingConfig", "EndpointsConfig")
		ec["other"] = ec[tNet]
		delete(ec, tNet)
	},
	"endpoint second network": func(m map[string]any) {
		sub(m, "NetworkingConfig", "EndpointsConfig")["bridge"] = map[string]any{}
	},
	"endpoint no network": func(m map[string]any) { m["NetworkingConfig"] = map[string]any{"EndpointsConfig": map[string]any{}} },
	"endpoint IPv6": func(m map[string]any) {
		sub(m, "NetworkingConfig", "EndpointsConfig", tNet, "IPAMConfig")["IPv6Address"] = "::1"
	},
	"endpoint another IPv4": func(m map[string]any) {
		sub(m, "NetworkingConfig", "EndpointsConfig", tNet, "IPAMConfig")["IPv4Address"] = "10.250.7.3"
	},
	"endpoint no IPAM": func(m map[string]any) { sub(m, "NetworkingConfig", "EndpointsConfig")[tNet] = map[string]any{} },
	"endpoint LinkLocalIPs": func(m map[string]any) {
		sub(m, "NetworkingConfig", "EndpointsConfig", tNet, "IPAMConfig")["LinkLocalIPs"] = []string{"169.254.1.1"}
	},
	"NetworkingConfig extra": func(m map[string]any) { sub(m, "NetworkingConfig")["X"] = 1 },

	// Image
	"Image a tag":        func(m map[string]any) { m["Image"] = "nginx:1" },
	"Image another id":   func(m map[string]any) { m["Image"] = tOtherImg },
	"Image missing":      func(m map[string]any) { delete(m, "Image") },
	"Image not a string": func(m map[string]any) { m["Image"] = 5 },

	// Labels
	"seventh label":  func(m map[string]any) { sub(m, "Labels")["serverdock.extra"] = "1" },
	"user label":     func(m map[string]any) { sub(m, "Labels")["com.example"] = "1" },
	"label missing":  func(m map[string]any) { delete(sub(m, "Labels"), LabelAttempt) },
	"swap a label":   func(m map[string]any) { delete(sub(m, "Labels"), LabelAttempt); sub(m, "Labels")["com.x"] = "1" },
	"instance wrong": func(m map[string]any) { sub(m, "Labels")[LabelInstance] = strings.Repeat("f", 32) },
	"managed 0":      func(m map[string]any) { sub(m, "Labels")[LabelManaged] = "0" },
	"project wrong":  func(m map[string]any) { sub(m, "Labels")[LabelProject] = "4" },
	"service wrong":  func(m map[string]any) { sub(m, "Labels")[LabelService] = "8" },
	"deploy wrong":   func(m map[string]any) { sub(m, "Labels")[LabelDeploy] = "13" },
	"attempt wrong":  func(m map[string]any) { sub(m, "Labels")[LabelAttempt] = "2" },
	"label not id":   func(m map[string]any) { sub(m, "Labels")[LabelDeploy] = "1 2" },
	"label empty":    func(m map[string]any) { sub(m, "Labels")[LabelDeploy] = "" },
	"label number":   func(m map[string]any) { sub(m, "Labels")[LabelDeploy] = 12 },
	"labels null":    func(m map[string]any) { m["Labels"] = nil },

	// ExposedPorts
	"two exposed ports": func(m map[string]any) { sub(m, "ExposedPorts")["9090/tcp"] = map[string]any{} },
	"other port":        func(m map[string]any) { m["ExposedPorts"] = map[string]any{"9090/tcp": map[string]any{}} },
	"udp port":          func(m map[string]any) { m["ExposedPorts"] = map[string]any{"8080/udp": map[string]any{}} },
	"port 0":            func(m map[string]any) { m["ExposedPorts"] = map[string]any{"0/tcp": map[string]any{}} },
	"port 70000":        func(m map[string]any) { m["ExposedPorts"] = map[string]any{"70000/tcp": map[string]any{}} },
	"port leading zero": func(m map[string]any) { m["ExposedPorts"] = map[string]any{"08080/tcp": map[string]any{}} },
	"port value":        func(m map[string]any) { sub(m, "ExposedPorts")["8080/tcp"] = map[string]any{"x": 1} },
	"empty exposed":     func(m map[string]any) { m["ExposedPorts"] = map[string]any{} },
	"null exposed":      func(m map[string]any) { m["ExposedPorts"] = nil },
	"null HostConfig":   func(m map[string]any) { m["HostConfig"] = nil },
	"HostConfig array":  func(m map[string]any) { m["HostConfig"] = []any{} },
}

func TestCheckCreateAgentAcceptsTheAllowedBodyAndRefusesEveryFixture(t *testing.T) {
	exp := goodExp()
	if err := CheckCreateAgent(enc(tree(t, true)), exp); err != nil {
		t.Fatalf("the good body: %v", err)
	}
	// ExposedPorts is optional, and MaximumRetryCount 0 is the zero value.
	m := tree(t, true)
	delete(m, "ExposedPorts")
	sub(m, "HostConfig", "RestartPolicy")["MaximumRetryCount"] = 0
	if err := CheckCreateAgent(enc(m), exp); err != nil {
		t.Fatalf("optional parts: %v", err)
	}
	for name, mod := range refusals {
		m := tree(t, true)
		mod(m)
		if err := CheckCreateAgent(enc(m), exp); err == nil {
			t.Errorf("agent check accepted %q: %s", name, enc(m))
		}
	}
}

func TestCheckCreateControlPlaneRefusesTheSameFixtures(t *testing.T) {
	exp := goodExp()
	if err := CheckCreateControlPlane(enc(tree(t, false)), exp); err != nil {
		t.Fatalf("the good body: %v", err)
	}
	for name, mod := range refusals {
		m := tree(t, false)
		// "instance wrong" and a sixth label do not apply to a five-label body
		// in the same way; apply the mutation and require a refusal anyway.
		mod(m)
		if err := CheckCreateControlPlane(enc(m), exp); err == nil {
			t.Errorf("control-plane check accepted %q: %s", name, enc(m))
		}
	}
}

func TestFiveLabelsForTheControlPlaneSixForTheAgentEachRefusedByTheOther(t *testing.T) {
	exp := goodExp()
	five, six := enc(tree(t, false)), enc(tree(t, true))
	if err := CheckCreateControlPlane(five, exp); err != nil {
		t.Fatal(err)
	}
	if err := CheckCreateAgent(six, exp); err != nil {
		t.Fatal(err)
	}
	if err := CheckCreateControlPlane(six, exp); err == nil {
		t.Error("the control-plane check accepted a body with serverdock.instance")
	}
	if err := CheckCreateAgent(five, exp); err == nil {
		t.Error("the agent check accepted a body without serverdock.instance")
	}
}

func TestCheckCreateRefusesDuplicateKeysAtAnyDepth(t *testing.T) {
	good := string(enc(tree(t, true)))
	for name, b := range map[string]string{
		"top level":     strings.Replace(good, `"Image":`, `"Image":"x","Image":`, 1),
		"nested":        strings.Replace(good, `"Name":"unless-stopped"`, `"Name":"always","Name":"unless-stopped"`, 1),
		"label":         strings.Replace(good, `"serverdock.managed":"1"`, `"serverdock.managed":"1","serverdock.managed":"1"`, 1),
		"pids":          strings.Replace(good, `"PidsLimit":4096`, `"PidsLimit":1,"PidsLimit":4096`, 1),
		"in an array":   strings.Replace(good, `"HostConfig":{`, `"HostConfig":{"CapAdd":[{"a":1,"a":2}],`, 1),
		"endpoint":      strings.Replace(good, `"IPv4Address":"`+tIP+`"`, `"IPv4Address":"10.250.7.3","IPv4Address":"`+tIP+`"`, 1),
		"case variants": strings.Replace(good, `"Image":`, `"image":"x","Image":`, 1),
		"escaped":       strings.Replace(good, `"Image":`, `"\u0049mage":"x","Image":`, 1),
	} {
		if err := CheckCreateAgent([]byte(b), goodExp()); err == nil {
			t.Errorf("%s: accepted %s", name, b)
		}
	}
	// The escaped key really is a JSON escape, and it is refused as a duplicate.
	esc := strings.Replace(good, `"Image":`, `"\u0049mage":"x","Image":`, 1)
	if !strings.Contains(esc, `\u0049`) {
		t.Fatal("the fixture has no JSON escape")
	}
	if err := CheckCreateAgent([]byte(esc), goodExp()); err == nil || !strings.Contains(err.Error(), "duplicate") {
		t.Errorf("escaped duplicate: %v", err)
	}
}

// The agent's real expectations: no label values, no container port. Everything
// in the table is refused except what only a deploy row can judge.
func TestAgentFixtureTableWithTheAgentsRealExpectations(t *testing.T) {
	exp := goodExp()
	exp.Project, exp.Service, exp.Deploy, exp.Attempt, exp.ContainerPort = "", "", "", "", 0
	onlyTheControlPlaneCanJudge := map[string]bool{
		"project wrong": true, "service wrong": true, "deploy wrong": true, "attempt wrong": true, "other port": true,
	}
	if err := CheckCreateAgent(enc(tree(t, true)), exp); err != nil {
		t.Fatal(err)
	}
	for name, mod := range refusals {
		m := tree(t, true)
		mod(m)
		err := CheckCreateAgent(enc(m), exp)
		if onlyTheControlPlaneCanJudge[name] {
			if err != nil {
				t.Errorf("%s: refused: %v", name, err)
			}
			continue
		}
		if err == nil {
			t.Errorf("accepted %q: %s", name, enc(m))
		}
	}
}

func TestReservedNetworksCanNeverBeTheExpectedNetwork(t *testing.T) {
	for _, n := range []string{"host", "bridge", "none", "default", "HOST", "Bridge", "None", "DEFAULT", "container:x", "Container:abc", "CONTAINER:x"} {
		e := goodExp()
		e.Network = n
		m := tree(t, false)
		sub(m, "HostConfig")["NetworkMode"] = n
		ec := sub(m, "NetworkingConfig", "EndpointsConfig")
		ec[n] = ec[tNet]
		delete(ec, tNet)
		if err := CheckCreateControlPlane(enc(m), e); err == nil {
			t.Errorf("control plane accepted network %q", n)
		}
		m6 := tree(t, true)
		sub(m6, "HostConfig")["NetworkMode"] = n
		ec = sub(m6, "NetworkingConfig", "EndpointsConfig")
		ec[n] = ec[tNet]
		delete(ec, tNet)
		if err := CheckCreateAgent(enc(m6), e); err == nil {
			t.Errorf("agent accepted network %q", n)
		}
	}
}

func TestWorkerModeRequiresNoExposedPorts(t *testing.T) {
	e := goodExp()
	e.ContainerPort, e.Worker = 0, true
	m := tree(t, false)
	if err := CheckCreateControlPlane(enc(m), e); err == nil {
		t.Error("a worker body with ExposedPorts was accepted")
	}
	delete(m, "ExposedPorts")
	if err := CheckCreateControlPlane(enc(m), e); err != nil {
		t.Errorf("a worker body without ExposedPorts: %v", err)
	}
	e.ContainerPort = 8080
	if err := CheckCreateControlPlane(enc(m), e); err == nil {
		t.Error("a worker with a container port was accepted as expectations")
	}
}

func TestCheckCreateRefusesMalformedInput(t *testing.T) {
	good := string(enc(tree(t, true)))
	deep := strings.Repeat(`{"a":`, 40) + `1` + strings.Repeat(`}`, 40)
	var many strings.Builder
	many.WriteString(`{`)
	for i := 0; i < 300; i++ {
		if i > 0 {
			many.WriteString(",")
		}
		many.WriteString(`"k` + string(rune('a'+i%26)) + string(rune('a'+i/26)) + `":1`)
	}
	many.WriteString(`}`)
	for name, b := range map[string]string{
		"trailing object": good + `{}`,
		"trailing text":   good + ` x`,
		"not an object":   `[]`,
		"a number":        `5`,
		"null":            `null`,
		"empty":           ``,
		"truncated":       good[:len(good)-3],
		"pids fraction":   strings.Replace(good, `"PidsLimit":4096`, `"PidsLimit":4096.0`, 1),
		"pids exponent":   strings.Replace(good, `"PidsLimit":4096`, `"PidsLimit":4.096e3`, 1),
		"too deep":        strings.Replace(good, `"Labels":`, `"X":`+deep+`,"Labels":`, 1),
		"too many keys":   many.String(),
		"too large":       `{"X":"` + strings.Repeat("a", 70<<10) + `"}`,
	} {
		if err := CheckCreateAgent([]byte(b), goodExp()); err == nil {
			t.Errorf("%s: accepted", name)
		}
	}
}

func TestCheckCreateRefusesIncompleteExpectations(t *testing.T) {
	good := enc(tree(t, true))
	for name, mod := range map[string]func(*CreateExpect){
		"no instance":         func(e *CreateExpect) { e.Instance = "" },
		"short instance":      func(e *CreateExpect) { e.Instance = "abc" },
		"no network":          func(e *CreateExpect) { e.Network = "" },
		"no ip":               func(e *CreateExpect) { e.IP = "" },
		"non-canonical ip":    func(e *CreateExpect) { e.IP = "10.250.007.2" },
		"no image":            func(e *CreateExpect) { e.ImageID = "" },
		"image is a tag":      func(e *CreateExpect) { e.ImageID = "nginx" },
		"no subnet":           func(e *CreateExpect) { e.Subnet = "" },
		"bad project":         func(e *CreateExpect) { e.Project = "p" },
		"bad port":            func(e *CreateExpect) { e.ContainerPort = 70000 },
		"ip outside subnet":   func(e *CreateExpect) { e.IP = "10.250.8.2" },
		"ip is the gateway":   func(e *CreateExpect) { e.IP = "10.250.7.1" },
		"ip is network":       func(e *CreateExpect) { e.IP = "10.250.7.0" },
		"ip is broadcast":     func(e *CreateExpect) { e.IP = "10.250.7.255" },
		"ip in dynamic range": func(e *CreateExpect) { e.IP = "10.250.7.200" },
		"ip in given range":   func(e *CreateExpect) { e.IPRange = "10.250.7.0/26" },
		"ip is given gateway": func(e *CreateExpect) { e.Gateway = tIP },
	} {
		e := goodExp()
		mod(&e)
		if err := CheckCreateAgent(good, e); err == nil {
			t.Errorf("%s: accepted", name)
		}
	}
	// The control plane needs the four label values; the agent does not.
	e := goodExp()
	e.Project = ""
	if err := CheckCreateControlPlane(enc(tree(t, false)), e); err == nil {
		t.Error("control plane accepted without expected label values")
	}
	e = goodExp()
	e.Project, e.Service, e.Deploy, e.Attempt, e.ContainerPort = "", "", "", "", 0
	if err := CheckCreateAgent(good, e); err != nil {
		t.Errorf("agent without label expectations: %v", err)
	}
}

func TestCheckCreateAddressRules(t *testing.T) {
	for _, ip := range []string{"10.250.7.2", "10.250.7.191", "10.250.7.100"} {
		m := tree(t, true)
		sub(m, "NetworkingConfig", "EndpointsConfig", tNet, "IPAMConfig")["IPv4Address"] = ip
		e := goodExp()
		e.IP = ip
		if err := CheckCreateAgent(enc(m), e); err != nil {
			t.Errorf("%s: %v", ip, err)
		}
	}
}

func TestCheckCreateErrorsNeverEchoValues(t *testing.T) {
	m := tree(t, true)
	sub(m, "Labels")["secret-key-xyz"] = "secret-value-xyz"
	sub(m, "HostConfig")["Env"] = "secret-value-xyz"
	m["Image"] = "secret-value-xyz"
	if err := CheckCreateAgent(enc(m), goodExp()); err == nil || strings.Contains(err.Error(), "secret-value-xyz") {
		t.Fatalf("error %v", err)
	}
	for _, b := range []string{`{"A":"secret-value-xyz",}`, `{"a":"secret-value-xyz" "b"}`, `secret-value-xyz`} {
		if err := CheckCreateAgent([]byte(b), goodExp()); err == nil || strings.Contains(err.Error(), "secret-value-xyz") {
			t.Errorf("%q: error %v", b, err)
		}
	}
}

func FuzzCheckCreate(f *testing.F) {
	exp := goodExp()
	good := enc(tree(f, true))
	five := enc(tree(f, false))
	f.Add(good)
	f.Add(five)
	f.Add([]byte(`{}`))
	f.Add([]byte(`{"Image":"x","Image":"y"}`))
	f.Add([]byte(`{"image":"x","HostConfig":{"privileged":true}}`))
	for _, mod := range refusals {
		m := tree(f, true)
		mod(m)
		f.Add(enc(m))
	}
	// The allowed bodies, as canonical JSON, for the property below.
	canon := func(b []byte) string {
		var m map[string]any
		d := json.NewDecoder(strings.NewReader(string(b)))
		d.UseNumber()
		if d.Decode(&m) != nil {
			return ""
		}
		out, _ := json.Marshal(m)
		return string(out)
	}
	m := tree(f, true)
	delete(m, "ExposedPorts")
	noPorts := canon(enc(m))
	m2 := tree(f, true)
	sub(m2, "HostConfig", "RestartPolicy")["MaximumRetryCount"] = 0
	retry0 := canon(enc(m2))
	allowed := map[string]bool{canon(good): true, noPorts: true, retry0: true}
	m3 := tree(f, true)
	delete(m3, "ExposedPorts")
	sub(m3, "HostConfig", "RestartPolicy")["MaximumRetryCount"] = 0
	allowed[canon(enc(m3))] = true
	f.Fuzz(func(t *testing.T, b []byte) {
		err := CheckCreateAgent(b, exp)
		cerr := CheckCreateControlPlane(b, exp)
		if err == nil && cerr == nil {
			t.Fatalf("both the agent and the control-plane check accepted %q", b)
		}
		if err != nil {
			return
		}
		// Whatever the agent check accepts is one of the allowed bodies, up to
		// key order and whitespace: nothing else gets through, and Docker's
		// decoder can read nothing the tree did not show (no duplicate or
		// case-variant key was accepted).
		if !allowed[canon(b)] {
			t.Fatalf("accepted a body outside the allowed shapes: %q", b)
		}
	})
}
