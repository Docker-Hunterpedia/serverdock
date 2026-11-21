package agentops

import (
	"encoding/json"
	"strings"
	"testing"
)

func goodBody(t testing.TB) (map[string]any, CreateExpect) {
	t.Helper()
	spec := goodCreate().Spec
	spec.Labels[LabelInstance] = testInstance
	b, err := spec.Marshal()
	if err != nil {
		t.Fatal(err)
	}
	var m map[string]any
	if err := json.Unmarshal(b, &m); err != nil {
		t.Fatal(err)
	}
	return m, CreateExpect{Instance: testInstance, Network: testNet, IP: "10.250.7.2", ImageID: imgID}
}

func sub(m map[string]any, path ...string) map[string]any {
	for _, p := range path {
		m = m[p].(map[string]any)
	}
	return m
}

func TestInterimCheckCreateAcceptsOnlyTheAllowedShape(t *testing.T) {
	m, exp := goodBody(t)
	b, _ := json.Marshal(m)
	if err := InterimCheckCreate(b, exp); err != nil {
		t.Fatalf("the good body: %v", err)
	}
	// ExposedPorts is optional.
	delete(m, "ExposedPorts")
	b, _ = json.Marshal(m)
	if err := InterimCheckCreate(b, exp); err != nil {
		t.Fatalf("no ExposedPorts: %v", err)
	}
	ep := func(m map[string]any) map[string]any { return sub(m, "NetworkingConfig", "EndpointsConfig", testNet) }
	for name, mod := range map[string]func(m map[string]any){
		"Privileged":            func(m map[string]any) { sub(m, "HostConfig")["Privileged"] = true },
		"PidMode host":          func(m map[string]any) { sub(m, "HostConfig")["PidMode"] = "host" },
		"CapAdd":                func(m map[string]any) { sub(m, "HostConfig")["CapAdd"] = []string{"SYS_ADMIN"} },
		"Binds":                 func(m map[string]any) { sub(m, "HostConfig")["Binds"] = []string{"/:/host"} },
		"Mounts":                func(m map[string]any) { sub(m, "HostConfig")["Mounts"] = []any{} },
		"PortBindings":          func(m map[string]any) { sub(m, "HostConfig")["PortBindings"] = map[string]any{} },
		"SecurityOpt":           func(m map[string]any) { sub(m, "HostConfig")["SecurityOpt"] = []string{"seccomp=unconfined"} },
		"Devices":               func(m map[string]any) { sub(m, "HostConfig")["Devices"] = []any{} },
		"top-level Env":         func(m map[string]any) { m["Env"] = []string{"A=1"} },
		"top-level Cmd":         func(m map[string]any) { m["Cmd"] = []string{"sh"} },
		"top-level Entrypoint":  func(m map[string]any) { m["Entrypoint"] = []string{"sh"} },
		"top-level User":        func(m map[string]any) { m["User"] = "root" },
		"Hostname":              func(m map[string]any) { m["Hostname"] = "x" },
		"NetworkMode host":      func(m map[string]any) { sub(m, "HostConfig")["NetworkMode"] = "host" },
		"NetworkMode bridge":    func(m map[string]any) { sub(m, "HostConfig")["NetworkMode"] = "bridge" },
		"NetworkMode container": func(m map[string]any) { sub(m, "HostConfig")["NetworkMode"] = "container:x" },
		"PidsLimit 0":           func(m map[string]any) { sub(m, "HostConfig")["PidsLimit"] = 0 },
		"PidsLimit -1":          func(m map[string]any) { sub(m, "HostConfig")["PidsLimit"] = -1 },
		"PidsLimit 4097":        func(m map[string]any) { sub(m, "HostConfig")["PidsLimit"] = 4097 },
		"restart always":        func(m map[string]any) { sub(m, "HostConfig", "RestartPolicy")["Name"] = "always" },
		"restart on-failure":    func(m map[string]any) { sub(m, "HostConfig", "RestartPolicy")["Name"] = "on-failure" },
		"restart retry count":   func(m map[string]any) { sub(m, "HostConfig", "RestartPolicy")["MaximumRetryCount"] = 3 },
		"log driver":            func(m map[string]any) { sub(m, "HostConfig", "LogConfig")["Type"] = "json-file" },
		"log extra option":      func(m map[string]any) { sub(m, "HostConfig", "LogConfig", "Config")["labels"] = "x" },
		"log size":              func(m map[string]any) { sub(m, "HostConfig", "LogConfig", "Config")["max-size"] = "1g" },
		"image is a tag":        func(m map[string]any) { m["Image"] = "nginx:1" },
		"image is another id":   func(m map[string]any) { m["Image"] = otherImgID },
		"seventh label":         func(m map[string]any) { sub(m, "Labels")["serverdock.extra"] = "1" },
		"label missing":         func(m map[string]any) { delete(sub(m, "Labels"), LabelAttempt) },
		"instance not ours":     func(m map[string]any) { sub(m, "Labels")[LabelInstance] = strings.Repeat("f", 32) },
		"managed not 1":         func(m map[string]any) { sub(m, "Labels")[LabelManaged] = "0" },
		"label value bad":       func(m map[string]any) { sub(m, "Labels")[LabelService] = "a b" },
		"label value empty":     func(m map[string]any) { sub(m, "Labels")[LabelDeploy] = "" },
		"foreign label swap": func(m map[string]any) {
			delete(sub(m, "Labels"), LabelAttempt)
			sub(m, "Labels")["com.x"] = "1"
		},
		"second network": func(m map[string]any) { sub(m, "NetworkingConfig", "EndpointsConfig")["bridge"] = map[string]any{} },
		"another network": func(m map[string]any) {
			ec := sub(m, "NetworkingConfig", "EndpointsConfig")
			ec["other"] = ec[testNet]
			delete(ec, testNet)
		},
		"aliases":      func(m map[string]any) { ep(m)["Aliases"] = []string{"db"} },
		"ipv6 address": func(m map[string]any) { sub(ep(m), "IPAMConfig")["IPv6Address"] = "::1" },
		"another ipv4": func(m map[string]any) { sub(ep(m), "IPAMConfig")["IPv4Address"] = "10.250.7.3" },
		"links":        func(m map[string]any) { ep(m)["Links"] = []string{"x"} },
		"two exposed ports": func(m map[string]any) {
			sub(m, "ExposedPorts")["9090/tcp"] = map[string]any{}
		},
		"udp port": func(m map[string]any) {
			delete(sub(m, "ExposedPorts"), "8080/tcp")
			sub(m, "ExposedPorts")["53/udp"] = map[string]any{}
		},
		"port 0": func(m map[string]any) {
			delete(sub(m, "ExposedPorts"), "8080/tcp")
			sub(m, "ExposedPorts")["0/tcp"] = map[string]any{}
		},
		"port 70000": func(m map[string]any) {
			delete(sub(m, "ExposedPorts"), "8080/tcp")
			sub(m, "ExposedPorts")["70000/tcp"] = map[string]any{}
		},
		"exposed port value":  func(m map[string]any) { sub(m, "ExposedPorts")["8080/tcp"] = map[string]any{"x": 1} },
		"empty exposed ports": func(m map[string]any) { m["ExposedPorts"] = map[string]any{} },
		"null host config":    func(m map[string]any) { m["HostConfig"] = nil },
	} {
		m, exp := goodBody(t)
		mod(m)
		b, _ := json.Marshal(m)
		if err := InterimCheckCreate(b, exp); err == nil {
			t.Errorf("%s: accepted %s", name, b)
		}
	}
}

func TestInterimCheckCreateRefusesBadInput(t *testing.T) {
	m, exp := goodBody(t)
	good, _ := json.Marshal(m)
	for name, b := range map[string]string{
		"duplicate key":        strings.Replace(string(good), `"PidsLimit":4096`, `"PidsLimit":4096,"PidsLimit":1`, 1),
		"duplicate nested":     strings.Replace(string(good), `"Name":"unless-stopped"`, `"Name":"always","Name":"unless-stopped"`, 1),
		"duplicate label":      strings.Replace(string(good), `"serverdock.managed":"1"`, `"serverdock.managed":"1","serverdock.managed":"1"`, 1),
		"trailing data":        string(good) + `{}`,
		"not an object":        `[]`,
		"empty":                ``,
		"pids as a string":     strings.Replace(string(good), `"PidsLimit":4096`, `"PidsLimit":"4096"`, 1),
		"pids with a fraction": strings.Replace(string(good), `"PidsLimit":4096`, `"PidsLimit":4096.0`, 1),
	} {
		if err := InterimCheckCreate([]byte(b), exp); err == nil {
			t.Errorf("%s: accepted", name)
		}
	}
	for name, e := range map[string]CreateExpect{
		"no instance": {Network: exp.Network, IP: exp.IP, ImageID: exp.ImageID},
		"no network":  {Instance: exp.Instance, IP: exp.IP, ImageID: exp.ImageID},
		"no ip":       {Instance: exp.Instance, Network: exp.Network, ImageID: exp.ImageID},
		"no image":    {Instance: exp.Instance, Network: exp.Network, IP: exp.IP},
	} {
		if err := InterimCheckCreate(good, e); err == nil {
			t.Errorf("%s: accepted", name)
		}
	}
	// An error never echoes a value from the body.
	m2, exp2 := goodBody(t)
	sub(m2, "Labels")["secret-key-xyz"] = "secret-value-xyz"
	b, _ := json.Marshal(m2)
	if err := InterimCheckCreate(b, exp2); err != nil && strings.Contains(err.Error(), "secret-value-xyz") {
		t.Errorf("error echoes a value: %v", err)
	}
}

func FuzzInterimCheckCreate(f *testing.F) {
	m, exp := goodBody(f)
	good, _ := json.Marshal(m)
	f.Add(good)
	f.Add([]byte(`{}`))
	f.Add([]byte(`{"Image":"x","Image":"y"}`))
	var want map[string]any
	_ = json.Unmarshal(good, &want)
	wb, _ := json.Marshal(want)
	f.Fuzz(func(t *testing.T, b []byte) {
		if InterimCheckCreate(b, exp) != nil {
			return
		}
		// Whatever passes is the allowed body up to key order and whitespace.
		var got map[string]any
		if json.Unmarshal(b, &got) != nil {
			t.Fatalf("accepted undecodable input %q", b)
		}
		gb, _ := json.Marshal(got)
		if string(gb) != string(wb) {
			t.Fatalf("accepted a body that differs from the allowed shape: %s", b)
		}
	})
}

func TestStaticAddrOnABiggerSubnet(t *testing.T) {
	n := newNetInfo("10.250.16.0/20")
	for ip, ok := range map[string]bool{
		"10.250.16.2": true, "10.250.28.255": false, "10.250.16.1": false, "10.250.16.0": false, "10.250.31.255": false,
		"10.250.16.3": true, "10.250.15.9": false, "10.250.28.0": false, "10.250.27.254": true,
	} {
		err := StaticAddr(n, mustAddr(t, ip))
		if (err == nil) != ok {
			t.Errorf("%s: err=%v want ok=%v", ip, err, ok)
		}
	}
}
