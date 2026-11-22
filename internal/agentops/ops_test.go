package agentops

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"reflect"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/Docker-Hunterpedia/serverdock/internal/agent"
	"github.com/Docker-Hunterpedia/serverdock/internal/agent/dockerapi"
	"github.com/Docker-Hunterpedia/serverdock/internal/policy"
)

func ctx5(t *testing.T) context.Context {
	t.Helper()
	c, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	t.Cleanup(cancel)
	return c
}

func wantCode(t *testing.T, err error, code string) {
	t.Helper()
	if !IsCode(err, code) {
		t.Fatalf("want code %q, got %v", code, err)
	}
}

func TestHandlersRequirePolicyAndInstance(t *testing.T) {
	c := dockerapi.New("/nonexistent")
	defer c.Close()
	for name, d := range map[string]Deps{
		"no docker":       {Instance: testInstance, CheckCreate: policy.CheckCreateAgent},
		"no policy":       {Docker: c, Instance: testInstance},
		"no instance":     {Docker: c, CheckCreate: policy.CheckCreateAgent},
		"short instance":  {Docker: c, Instance: "abc", CheckCreate: policy.CheckCreateAgent},
		"non-hex install": {Docker: c, Instance: strings.Repeat("g", 32), CheckCreate: policy.CheckCreateAgent},
	} {
		if _, err := Handlers(d); err == nil {
			t.Errorf("%s: accepted", name)
		}
	}
}

func TestEveryLocalOpHasAHandlerAndNoRemoteOpDoes(t *testing.T) {
	h, err := Handlers(Deps{Docker: dockerapi.New("/x"), Instance: testInstance, CheckCreate: policy.CheckCreateAgent})
	if err != nil {
		t.Fatal(err)
	}
	for _, sp := range agent.Specs() {
		_, has := h[sp.Name]
		switch {
		case sp.RemoteOK && has:
			t.Errorf("RemoteOK op %s has a local handler", sp.Name)
		case !sp.RemoteOK && sp.Name != "logs" && !has:
			t.Errorf("local-only op %s has no handler", sp.Name)
		case !sp.RemoteOK && sp.Name != "logs" && sp.Name != agent.OpEvents && sp.Timeout <= 0:
			t.Errorf("op %s has no timeout", sp.Name)
		}
	}
	for name := range h {
		sp, ok := agent.SpecFor(name)
		if !ok || sp.RemoteOK {
			t.Errorf("handler %s is not a local-only op in the table", name)
		}
	}
}

// Every type decoded through agent.DecodeArgs must tag every field.
func TestProtocolTypesTagEveryField(t *testing.T) {
	seen := map[reflect.Type]bool{}
	var walk func(ty reflect.Type)
	walk = func(ty reflect.Type) {
		for ty.Kind() == reflect.Pointer || ty.Kind() == reflect.Slice || ty.Kind() == reflect.Array || ty.Kind() == reflect.Map {
			ty = ty.Elem()
		}
		if ty.Kind() != reflect.Struct || seen[ty] {
			return
		}
		seen[ty] = true
		for i := 0; i < ty.NumField(); i++ {
			f := ty.Field(i)
			name, _, _ := strings.Cut(f.Tag.Get("json"), ",")
			if name == "" || name == "-" || f.Anonymous {
				t.Errorf("%s.%s has no json tag name", ty, f.Name)
			}
			walk(f.Type)
		}
	}
	for _, v := range []any{PullArgs{}, PullProgress{}, PullResult{}, ImageInspectArgs{}, ImageInfo{}, CreateArgs{}, CreateResult{},
		IDArgs{}, GraceArgs{}, Empty{}, ContainerState{}, ListResult{}, PortsResult{}, NetworkArgs{}, NetworkResult{}, UsedAddrs{},
		Event{}, HealthArgs{}, HealthResult{}} {
		walk(reflect.TypeOf(v))
	}
	if len(seen) < 15 {
		t.Fatalf("only %d types walked", len(seen))
	}
}

// ---- ownership ----

func TestAgentRefusesToTouchAnUnownedContainer(t *testing.T) {
	r := newRig(t)
	foreign := map[string]*fakeContainer{
		"no labels":         r.fd.add(&fakeContainer{Name: "plain", Labels: map[string]string{"com.example": "x"}}),
		"no serverdock":     r.fd.add(&fakeContainer{Name: "plain2", Labels: map[string]string{}}),
		"another instance":  r.fd.add(&fakeContainer{Name: "other", Labels: ours(map[string]string{LabelInstance: strings.Repeat("f", 32)})}),
		"no instance label": r.fd.add(&fakeContainer{Name: "noinst", Labels: ours(map[string]string{LabelInstance: ""})}),
		"seventh label":     r.fd.add(&fakeContainer{Name: "seven", Labels: ours(map[string]string{"serverdock.extra": "1"})}),
		"missing attempt":   r.fd.add(&fakeContainer{Name: "noatt", Labels: ours(map[string]string{LabelAttempt: ""})}),
		"managed not one":   r.fd.add(&fakeContainer{Name: "man", Labels: ours(map[string]string{LabelManaged: "2"})}),
		"escape in a value": r.fd.add(&fakeContainer{Name: "esc", Labels: ours(map[string]string{LabelService: "7\x1b[2J"})}),
		"over-long value":   r.fd.add(&fakeContainer{Name: "long", Labels: ours(map[string]string{LabelProject: strings.Repeat("p", 300)})}),
		"too many serverdock": r.fd.add(&fakeContainer{Name: "many", Labels: func() map[string]string {
			l := ours(nil)
			for i := 0; i < 17; i++ {
				l[fmt.Sprintf("serverdock.x%d", i)] = "1"
			}
			return l
		}()}),
	}
	ctx := ctx5(t)
	for why, c := range foreign {
		before := c.State
		for op, f := range map[string]func() error{
			"stop":    func() error { return r.cl.ContainerStop(ctx, c.ID, 1) },
			"restart": func() error { return r.cl.ContainerRestart(ctx, c.ID, 1) },
			"remove":  func() error { return r.cl.ContainerRemove(ctx, c.ID) },
			"start":   func() error { return r.cl.ContainerStart(ctx, c.ID) },
		} {
			if err := f(); !IsCode(err, agent.CodeRefusedUnowned) {
				t.Errorf("%s %s: %v", why, op, err)
			}
		}
		if c.State != before {
			t.Errorf("%s: state changed to %s", why, c.State)
		}
	}
	for _, m := range []string{"POST /containers/", "DELETE /containers/"} {
		for _, c := range r.fd.calls {
			if strings.HasPrefix(c, m) && !strings.HasSuffix(c, "/json") {
				t.Errorf("a change reached Docker: %s", c)
			}
		}
	}
	// And inspect says so, without refusing.
	st, err := r.cl.ContainerInspect(ctx, foreign["seventh label"].ID)
	if err != nil || st.Owned {
		t.Errorf("inspect of a container with a seventh label: %+v %v", st, err)
	}
}

func TestOwnershipOpsRefuseShortIDsAndNames(t *testing.T) {
	r := newRig(t)
	c := r.fd.add(&fakeContainer{Name: "sd-p-web-d1-a1", Labels: ours(nil)})
	for _, id := range []string{
		"sd-p-web-d1-a1", c.ID[:12], c.ID[:63], strings.ToUpper(c.ID), c.ID + "0", "", "../" + c.ID[:61], c.ID[:60] + "%2e%2e",
	} {
		for op, args := range map[string]any{
			agent.OpContainerStop:    GraceArgs{ID: id, Grace: 1},
			agent.OpContainerRestart: GraceArgs{ID: id, Grace: 1},
			agent.OpContainerRemove:  IDArgs{ID: id},
			agent.OpContainerStart:   IDArgs{ID: id},
			agent.OpContainerInspect: IDArgs{ID: id},
		} {
			if _, err := r.rawCall(op, args); !IsCode(err, agent.CodeInvalid) {
				t.Errorf("%s %q: %v", op, id, err)
			}
		}
	}
	if n := r.fd.called("GET /containers/") + r.fd.called("POST /containers/") + r.fd.called("DELETE /containers/"); n != 0 {
		t.Errorf("%d Docker calls for refused ids", n)
	}
	// The client refuses before sending, too.
	if err := r.cl.ContainerStop(context.Background(), "sd-p-web-d1-a1", 1); err == nil {
		t.Error("client sent a name")
	}
}

func TestOwnedContainerIsActedOnByItsInspectedID(t *testing.T) {
	r := newRig(t)
	c := r.fd.add(&fakeContainer{Name: "sd-p-web-d1-a1", Labels: ours(nil)})
	ctx := ctx5(t)
	if err := r.cl.ContainerStop(ctx, c.ID, 1); err != nil || c.State != "exited" {
		t.Fatalf("stop: %v %s", err, c.State)
	}
	if err := r.cl.ContainerStart(ctx, c.ID); err != nil || c.State != "running" {
		t.Fatalf("start: %v %s", err, c.State)
	}
	if err := r.cl.ContainerStart(ctx, c.ID); err != nil { // already running is success
		t.Fatalf("start twice: %v", err)
	}
	if err := r.cl.ContainerRestart(ctx, c.ID, 1); err != nil {
		t.Fatalf("restart: %v", err)
	}
	wantCode(t, r.cl.ContainerRemove(ctx, c.ID), agent.CodeConflict) // running: never forced
	_ = r.cl.ContainerStop(ctx, c.ID, 1)
	if err := r.cl.ContainerRemove(ctx, c.ID); err != nil || r.fd.find(c.ID) != nil {
		t.Fatalf("remove: %v", err)
	}
	wantCode(t, r.cl.ContainerRemove(ctx, c.ID), agent.CodeNotFound)
	// Every change used the full id from the inspect.
	for _, call := range r.fd.calls {
		if strings.Contains(call, "sd-p-web") {
			t.Errorf("a change addressed the container by name: %s", call)
		}
	}
	// Grace is range checked.
	wantCode(t, r.cl.ContainerStop(ctx, c.ID, 301), agent.CodeInvalid)
	wantCode(t, r.cl.ContainerRestart(ctx, c.ID, -1), agent.CodeInvalid)
}

func TestContainerInspectReportsStateAndAddresses(t *testing.T) {
	r := newRig(t)
	c := r.fd.add(&fakeContainer{Name: "sd-p-web-d1-a1", Labels: ours(nil), IP: "10.250.7.2", Configured: "10.250.7.2"})
	st, err := r.cl.ContainerInspect(ctx5(t), c.ID)
	if err != nil || st.ID != c.ID || !st.Running || !st.Owned || st.IP != "10.250.7.2" || st.ConfiguredIP != "10.250.7.2" ||
		st.ImageID != imgID || st.Labels[LabelInstance] != testInstance {
		t.Fatalf("%+v %v", st, err)
	}
	_, err = r.cl.ContainerInspect(ctx5(t), strings.Repeat("a", 64))
	wantCode(t, err, agent.CodeNotFound)
}

// ---- create ----

func TestCreateSetsTheInstanceLabelItselfAndSendsTheCheckedBytes(t *testing.T) {
	var got []byte
	var exp CreateExpect
	r := newRig(t, func(d *Deps) {
		d.CheckCreate = func(body []byte, e CreateExpect) error {
			got, exp = append([]byte(nil), body...), e
			return policy.CheckCreateAgent(body, e)
		}
	})
	res, err := r.create(goodCreate())
	if err != nil || res.Existed || !okID(res.ID) {
		t.Fatalf("%+v %v", res, err)
	}
	if len(r.fd.createBodies) != 1 || string(r.fd.createBodies[0]) != string(got) {
		t.Fatalf("the policy saw different bytes than Docker:\n%s\n%s", got, r.fd.createBodies)
	}
	var body struct{ Labels map[string]string }
	_ = json.Unmarshal(got, &body)
	if len(body.Labels) != 6 || body.Labels[LabelInstance] != testInstance {
		t.Fatalf("labels %v", body.Labels)
	}
	if exp.Instance != testInstance || exp.Network != testNet || exp.IP != "10.250.7.2" || exp.ImageID != imgID {
		t.Fatalf("expectations %+v", exp)
	}
}

func TestCreateRefusedByThePolicyHookNeverReachesDocker(t *testing.T) {
	r := newRig(t, func(d *Deps) {
		d.CheckCreate = func([]byte, CreateExpect) error { return fmt.Errorf("nope\x1b[2J") }
	})
	_, err := r.create(goodCreate())
	wantCode(t, err, agent.CodePolicy)
	if strings.ContainsRune(err.Error(), 0x1b) || !strings.Contains(err.Error(), "nope") {
		t.Errorf("error text %q", err)
	}
	if r.fd.called("POST /containers/create") != 0 {
		t.Error("a refused create reached Docker")
	}
}

func TestCreateRefusesCallerSuppliedInstanceOrExtraLabels(t *testing.T) {
	r := newRig(t)
	for name, mod := range map[string]func(*CreateArgs){
		"instance (ours)":     func(a *CreateArgs) { a.Spec.Labels[LabelInstance] = testInstance },
		"instance (another)":  func(a *CreateArgs) { a.Spec.Labels[LabelInstance] = strings.Repeat("e", 32) },
		"extra serverdock":    func(a *CreateArgs) { a.Spec.Labels["serverdock.extra"] = "1" },
		"foreign label":       func(a *CreateArgs) { a.Spec.Labels["com.example.x"] = "1" },
		"upper-case instance": func(a *CreateArgs) { a.Spec.Labels["Serverdock.instance"] = "x" },
	} {
		a := goodCreate()
		mod(&a)
		_, err := r.create(a)
		if !IsCode(err, agent.CodePolicy) {
			t.Errorf("%s: %v", name, err)
		}
	}
	// A missing label is refused by the check on the final body (six exactly).
	a := goodCreate()
	delete(a.Spec.Labels, LabelAttempt)
	_, err := r.create(a)
	wantCode(t, err, agent.CodePolicy)
	if r.fd.called("POST /containers/create") != 0 {
		t.Error("a refused create reached Docker")
	}
}

func TestCreateConflictWithForeignContainerFails(t *testing.T) {
	for name, labels := range map[string]map[string]string{
		"no labels":        {},
		"another instance": ours(map[string]string{LabelInstance: strings.Repeat("f", 32)}),
		"seventh label":    ours(map[string]string{"serverdock.extra": "1"}),
	} {
		r := newRig(t)
		c := r.fd.add(&fakeContainer{Name: "sd-p-web-d1-a1", Labels: labels})
		_, err := r.create(goodCreate())
		if !IsCode(err, agent.CodeConflict) || !strings.Contains(err.Error(), "does not own") {
			t.Errorf("%s: %v", name, err)
		}
		if c.State != "running" || r.fd.called("DELETE") != 0 || r.fd.called("POST /containers/"+c.ID) != 0 {
			t.Errorf("%s: the foreign container was touched", name)
		}
	}
}

func TestCreateConflictWithOwnLabelsButAnotherImageFails(t *testing.T) {
	r := newRig(t)
	c := r.fd.add(&fakeContainer{Name: "sd-p-web-d1-a1", Labels: ours(nil), Image: otherImgID})
	_, err := r.create(goodCreate())
	if !IsCode(err, agent.CodeConflict) || !strings.Contains(err.Error(), "another image") {
		t.Fatalf("%v", err)
	}
	if r.fd.called("DELETE") != 0 || c.State != "running" {
		t.Error("the container was touched")
	}
}

func TestCreateConflictWithAnotherDeployAttemptFails(t *testing.T) {
	r := newRig(t)
	r.fd.add(&fakeContainer{Name: "sd-p-web-d1-a1", Labels: ours(map[string]string{LabelAttempt: "2"})})
	_, err := r.create(goodCreate())
	wantCode(t, err, agent.CodeConflict)
}

func TestCreateConflictWithOwnContainerReturnsExisted(t *testing.T) {
	r := newRig(t)
	c := r.fd.add(&fakeContainer{Name: "sd-p-web-d1-a1", Labels: ours(nil), State: "exited", Configured: "10.250.7.2"})
	res, err := r.create(goodCreate())
	if err != nil || !res.Existed || res.ID != c.ID {
		t.Fatalf("%+v %v", res, err)
	}
	if r.fd.called("DELETE") != 0 || r.fd.called("POST /containers/"+c.ID) != 0 {
		t.Error("an existing container was changed")
	}
	// A repeat after a real create is the same answer.
	r2 := newRig(t)
	first, err := r2.create(goodCreate())
	if err != nil || first.Existed {
		t.Fatalf("%+v %v", first, err)
	}
	second, err := r2.create(goodCreate())
	if err != nil || !second.Existed || second.ID != first.ID {
		t.Fatalf("%+v %v", second, err)
	}
}

func TestCreateRefusesAnAddressOutsideTheStaticRange(t *testing.T) {
	r := newRig(t)
	for ip, why := range map[string]string{
		"10.250.8.2":    "another subnet",
		"10.249.7.2":    "another network",
		"10.250.7.0":    "network address",
		"10.250.7.1":    "gateway",
		"10.250.7.255":  "broadcast",
		"10.250.7.192":  "dynamic range start",
		"10.250.7.200":  "dynamic range",
		"10.250.7.254":  "dynamic range end",
		"10.250.7.02":   "not canonical",
		"::1":           "IPv6",
		"":              "empty",
		"10.250.7.2/24": "a prefix",
		"10.250.7.2 ":   "trailing space",
	} {
		a := goodCreate()
		a.IP = ip
		a.Spec.NetworkingConfig.EndpointsConfig[testNet] = dockerapi.EndpointConfig{IPAMConfig: dockerapi.EndpointIPAM{IPv4Address: ip}}
		_, err := r.create(a)
		if !IsCode(err, agent.CodePolicy) && !IsCode(err, agent.CodeInvalid) {
			t.Errorf("%s (%q): %v", why, ip, err)
		}
	}
	if r.fd.called("POST /containers/create") != 0 {
		t.Error("a refused address reached Docker")
	}
	for _, ip := range []string{"10.250.7.2", "10.250.7.191"} {
		a := goodCreate()
		a.Name = "sd-p-web-d1-a" + fmt.Sprint(len(ip))
		a.Spec.Labels[LabelAttempt] = fmt.Sprint(len(ip))
		a.IP = ip
		a.Spec.NetworkingConfig.EndpointsConfig[testNet] = dockerapi.EndpointConfig{IPAMConfig: dockerapi.EndpointIPAM{IPv4Address: ip}}
		if _, err := r.create(a); err != nil {
			t.Errorf("%s refused: %v", ip, err)
		}
	}
	// The address in the spec must be the one the agent checked.
	a := goodCreate()
	a.Name = "sd-p-web-d2-a1"
	a.Spec.Labels[LabelDeploy] = "2"
	a.Spec.NetworkingConfig.EndpointsConfig[testNet] = dockerapi.EndpointConfig{IPAMConfig: dockerapi.EndpointIPAM{IPv4Address: "10.250.7.9"}}
	_, err := r.create(a)
	wantCode(t, err, agent.CodePolicy)
}

func TestCreateRefusesABadNetworkNameImageOrContainerName(t *testing.T) {
	r := newRig(t)
	for name, mod := range map[string]func(*CreateArgs){
		"host network":         func(a *CreateArgs) { a.Spec.HostConfig.NetworkMode = "host" },
		"another network":      func(a *CreateArgs) { a.Spec.HostConfig.NetworkMode = "serverdock-ffffffff" },
		"image is a tag":       func(a *CreateArgs) { a.Spec.Image = "nginx:1" },
		"image differs":        func(a *CreateArgs) { a.Spec.Image = otherImgID },
		"image_id is a tag":    func(a *CreateArgs) { a.ImageID = "nginx:1" },
		"name without sd-":     func(a *CreateArgs) { a.Name = "web" },
		"name with newline":    func(a *CreateArgs) { a.Name = "sd-p-web-d1-a1\n" },
		"name with slash":      func(a *CreateArgs) { a.Name = "sd-p/web-d1-a1" },
		"name without attempt": func(a *CreateArgs) { a.Name = "sd-p-web-d1" },
		"segment ends in -":    func(a *CreateArgs) { a.Name = "sd-p--web-d1-a1" },
		"double dash":          func(a *CreateArgs) { a.Name = "sd-p-we--b-d1-a1" },
		"trailing dash":        func(a *CreateArgs) { a.Name = "sd-p-web--d1-a1" },
		"deploy != label":      func(a *CreateArgs) { a.Name = "sd-p-web-d2-a1" },
		"attempt != label":     func(a *CreateArgs) { a.Name = "sd-p-web-d1-a2" },
	} {
		a := goodCreate()
		mod(&a)
		if _, err := r.create(a); err == nil {
			t.Errorf("%s accepted", name)
		}
	}
	if r.fd.called("POST /containers/create") != 0 {
		t.Error("a refused create reached Docker")
	}
}

func TestCreateNeedsOurNetwork(t *testing.T) {
	r := newRig(t)
	r.fd.netLabels = map[string]string{LabelInstance: strings.Repeat("9", 32)}
	_, err := r.create(goodCreate())
	wantCode(t, err, agent.CodeRefusedUnowned)
	r.fd.netLabels = nil
	_, err = r.create(goodCreate())
	wantCode(t, err, agent.CodeRefusedUnowned)
	r.fd.netMissing = true
	_, err = r.create(goodCreate())
	wantCode(t, err, agent.CodeNotFound)
	if r.fd.called("POST /containers/create") != 0 {
		t.Error("reached Docker")
	}
}

func TestOpsDecodeArgsStrictly(t *testing.T) {
	r := newRig(t)
	c := r.fd.add(&fakeContainer{Name: "sd-p-web-d1-a1", Labels: ours(nil)})
	raw := func(s string) json.RawMessage { return json.RawMessage(s) }
	for name, tc := range map[string]struct {
		op   string
		args json.RawMessage
	}{
		"unknown key":         {agent.OpContainerStop, raw(`{"id":"` + c.ID + `","grace":1,"force":true}`)},
		"duplicate id":        {agent.OpContainerStop, raw(`{"id":"` + strings.Repeat("a", 64) + `","id":"` + c.ID + `","grace":1}`)},
		"case-variant key":    {agent.OpContainerRemove, raw(`{"ID":"` + c.ID + `"}`)},
		"nested unknown":      {agent.OpContainerCreate, raw(`{"name":"sd-p-web-d1-a1","ip":"10.250.7.2","image_id":"` + imgID + `","spec":{"Image":"` + imgID + `","Privileged":true}}`)},
		"nested host config":  {agent.OpContainerCreate, raw(`{"name":"sd-p-web-d1-a1","ip":"10.250.7.2","image_id":"` + imgID + `","spec":{"HostConfig":{"Privileged":true}}}`)},
		"args on a no-arg op": {agent.OpContainerList, raw(`{"all":true}`)},
		"wrong type":          {agent.OpImagePull, raw(`{"ref":["a"]}`)},
		"events with args":    {agent.OpEvents, raw(`{"x":1}`)},
		"inspect both":        {agent.OpImageInspect, raw(`{"ref":"a:1","image_id":"` + imgID + `"}`)},
		"inspect neither":     {agent.OpImageInspect, raw(`{}`)},
		"health unknown key":  {agent.OpHealthCheck, raw(`{"target":"10.250.7.2:80","kind":"tcp","proxy":"x"}`)},
		"network extra key":   {agent.OpNetworkEnsure, raw(`{"name":"` + testNet + `","subnet":"` + testSubnet + `","icc":true}`)},
	} {
		if _, err := r.rawCall(tc.op, tc.args); !IsCode(err, agent.CodeInvalid) {
			t.Errorf("%s: %v", name, err)
		}
	}
	if n := r.fd.called("POST") + r.fd.called("DELETE"); n != 0 {
		t.Errorf("%d changes reached Docker", n)
	}
}

// ---- images ----

func pullLine(status string, cur, total int) string {
	return fmt.Sprintf(`{"status":%q,"id":"layer1","progressDetail":{"current":%d,"total":%d}}`+"\n", status, cur, total)
}

func TestPullProgressCoalescedToTwoPerSecond(t *testing.T) {
	// The window is injected: an hour makes the test independent of the clock.
	// (The production default is 500 ms, two a second; see defaultProgressEvery.)
	if defaultProgressEvery != 500*time.Millisecond {
		t.Fatal("the default is two lines a second")
	}
	r := newRig(t, func(d *Deps) { d.progressEvery = time.Hour })
	r.fd.addImage("x:1", imgID, "x@sha256:"+strings.Repeat("c", 64))
	r.fd.pull = func(w http.ResponseWriter, req *http.Request) {
		for n := 1; n <= 200; n++ {
			io.WriteString(w, pullLine("Downloading", n*10, 100000))
		}
		io.WriteString(w, `{"status":"Status: Downloaded newer image for x:1"}`+"\n")
	}
	var lines []PullProgress
	res, err := r.cl.ImagePull(ctx5(t), "x:1", 0, func(p PullProgress) { lines = append(lines, p) })
	if err != nil || res.ImageID != imgID {
		t.Fatalf("%+v %v", res, err)
	}
	// 200 Docker lines inside one throttle window: the first, then the final one.
	if len(lines) != 2 {
		t.Fatalf("%d progress lines for 201 Docker lines in one window", len(lines))
	}
	last := lines[len(lines)-1]
	if last.Status != "Status: Downloaded newer image for x:1" || last.Bytes < 1000 {
		t.Errorf("the final line is not the final state: %+v", last)
	}
	if lines[0].Layers != 1 {
		t.Errorf("first line %+v", lines[0])
	}
}

func TestPullStallFails(t *testing.T) {
	r := newRig(t)
	r.fd.pull = func(w http.ResponseWriter, req *http.Request) {
		io.WriteString(w, pullLine("Downloading", 10, 1000))
		w.(http.Flusher).Flush()
		<-req.Context().Done() // then silence
	}
	t0 := time.Now()
	_, err := r.cl.ImagePull(ctx5(t), "x:1", time.Second, nil)
	wantCode(t, err, agent.CodeStalled)
	if d := time.Since(t0); d < 900*time.Millisecond || d > 5*time.Second {
		t.Errorf("stalled after %v", d)
	}
}

func TestPullThatKeepsMakingProgressIsNotStalled(t *testing.T) {
	r := newRig(t)
	r.fd.addImage("x:1", imgID)
	r.fd.pull = func(w http.ResponseWriter, req *http.Request) {
		fl := w.(http.Flusher)
		for n := 1; n <= 6; n++ {
			io.WriteString(w, pullLine("Downloading", n*100, 1000))
			fl.Flush()
			time.Sleep(400 * time.Millisecond) // 2.4 s in all, a 1 s stall limit
		}
		io.WriteString(w, `{"status":"Status: Image is up to date for x:1"}`+"\n")
	}
	if _, err := r.cl.ImagePull(ctx5(t), "x:1", time.Second, nil); err != nil {
		t.Fatal(err)
	}
}

func TestPullRefusedBelowFreeSpace(t *testing.T) {
	var free atomic.Uint64
	free.Store(MinFreeBytes - 1)
	r := newRig(t, func(d *Deps) { d.freeBytes = func(string) (uint64, error) { return free.Load(), nil } })
	r.fd.addImage("x:1", imgID)
	_, err := r.cl.ImagePull(ctx5(t), "x:1", 0, nil)
	wantCode(t, err, agent.CodeDiskFull)
	if r.fd.called("POST /images/create") != 0 {
		t.Error("pulled with too little space")
	}
	free.Store(MinFreeBytes)
	if _, err := r.cl.ImagePull(ctx5(t), "x:1", 0, nil); err != nil {
		t.Fatalf("exactly 1 GiB free: %v", err)
	}
}

func TestPullProceedsWhenFreeSpaceIsUnknown(t *testing.T) {
	r := newRig(t, func(d *Deps) {
		d.freeBytes = func(string) (uint64, error) { return 0, fmt.Errorf("no such directory") }
	})
	r.fd.addImage("x:1", imgID)
	var first []string
	if _, err := r.cl.ImagePull(ctx5(t), "x:1", 0, func(p PullProgress) { first = append(first, p.Status) }); err != nil {
		t.Fatal(err)
	}
	if len(first) == 0 || first[0] != "free disk space unknown" {
		t.Errorf("progress %q", first)
	}
}

func TestPullInspectsTheImageBeforeReportingSuccess(t *testing.T) {
	r := newRig(t)
	// Docker says "up to date" but the image is not there: not a success.
	_, err := r.cl.ImagePull(ctx5(t), "x:1", 0, nil)
	wantCode(t, err, agent.CodeNotFound)
	if r.fd.called("GET /images/x:1/json") != 1 {
		t.Error("no inspect after the pull")
	}
	// The digest of the right repository is picked.
	d1, d2 := "other/img@sha256:"+strings.Repeat("1", 64), "docker.io/library/nginx@sha256:"+strings.Repeat("2", 64)
	r.fd.addImage("nginx:1", imgID, d1, d2)
	res, err := r.cl.ImagePull(ctx5(t), "nginx:1", 0, nil)
	if err != nil || res.ImageID != imgID || res.RepoDigest != d2 {
		t.Fatalf("%+v %v", res, err)
	}
	// Several digests and none for this repository: none is returned.
	r.fd.addImage("a/b:1", imgID, d1, d2)
	res, err = r.cl.ImagePull(ctx5(t), "a/b:1", 0, nil)
	if err != nil || res.RepoDigest != "" {
		t.Fatalf("%+v %v", res, err)
	}
	// One digest, for another repository: none either.
	r.fd.addImage("c/d:1", imgID, d1)
	res, err = r.cl.ImagePull(ctx5(t), "c/d:1", 0, nil)
	if err != nil || res.RepoDigest != "" {
		t.Fatalf("%+v %v", res, err)
	}
}

func TestPullErrorsMapToCodes(t *testing.T) {
	r := newRig(t)
	r.fd.pull = func(w http.ResponseWriter, _ *http.Request) {
		io.WriteString(w, `{"errorDetail":{"message":"write /var/lib/docker/x: no space left on device"},"error":"no space left on device"}`+"\n")
	}
	_, err := r.cl.ImagePull(ctx5(t), "x:1", 0, nil)
	wantCode(t, err, agent.CodeDiskFull)
	r.fd.pull = func(w http.ResponseWriter, _ *http.Request) {
		io.WriteString(w, `{"error":"pull access denied\u001b[2J for x"}`+"\n")
	}
	_, err = r.cl.ImagePull(ctx5(t), "x:1", 0, nil)
	wantCode(t, err, agent.CodeDocker)
	if strings.ContainsRune(err.Error(), 0x1b) || !strings.Contains(err.Error(), "pull access denied[2J for x") {
		t.Errorf("the registry's message does not reach the caller: %q", err)
	}
	for _, msg := range []string{"manifest unknown", "pull access denied for x, repository does not exist"} {
		r.fd.pull = func(w http.ResponseWriter, _ *http.Request) {
			io.WriteString(w, `{"errorDetail":{"message":"`+msg+`"},"error":"`+msg+`"}`+"\n")
		}
		_, err = r.cl.ImagePull(ctx5(t), "x:1", 0, nil)
		if !IsCode(err, agent.CodeDocker) || !strings.Contains(err.Error(), msg) {
			t.Errorf("%q: %v", msg, err)
		}
	}
	// A reference that would pull every tag is refused by the agent.
	for _, ref := range []string{"nginx", "", "x:1;rm", "a b:1", "x:1\n", "x@sha256:abc"} {
		if _, err := r.cl.ImagePull(ctx5(t), ref, 0, nil); !IsCode(err, agent.CodeInvalid) {
			t.Errorf("%q: %v", ref, err)
		}
	}
	for _, s := range []int{-1, MaxStallS + 1} {
		if _, err := r.rawCall(agent.OpImagePull, PullArgs{Ref: "x:1", StallS: s}); !IsCode(err, agent.CodeInvalid) {
			t.Errorf("stall_s %d: %v", s, err)
		}
	}
}

func TestImageInspectIsBoundedAndNeverReturnsLabelValues(t *testing.T) {
	r := newRig(t)
	var digests, ports []string
	for i := 0; i < 20; i++ {
		digests = append(digests, fmt.Sprintf("x@sha256:%064d", i))
		ports = append(ports, fmt.Sprintf("%d/tcp", 1000+i))
	}
	b, _ := json.Marshal(map[string]any{"Id": imgID, "RepoDigests": digests, "Config": map[string]any{
		"ExposedPorts": map[string]any{"8080/tcp": map[string]any{}},
		"Volumes":      map[string]any{"/data": map[string]any{}},
		"Labels":       map[string]any{"serverdock.managed": "secret-value", "serverdock.a": "1", "com.example": "x"},
	}})
	r.fd.images["x:1"] = string(b)
	info, err := r.cl.ImageInspect(ctx5(t), "x:1", false)
	if err != nil || info.ID != imgID || len(info.RepoDigests) != 8 || len(info.ReservedLabelKeys) != 2 ||
		len(info.ExposedPorts) != 1 || len(info.Volumes) != 1 {
		t.Fatalf("%+v %v", info, err)
	}
	r.fd.images[imgID] = string(b)
	if _, err := r.cl.ImageInspect(ctx5(t), imgID, true); err != nil {
		t.Fatal(err)
	}
	if _, err := r.cl.ImageInspect(ctx5(t), "nope:1", false); !IsCode(err, agent.CodeNotFound) {
		t.Fatal(err)
	}
	if _, err := r.rawCall(agent.OpImageInspect, ImageInspectArgs{ImageID: "x:1"}); !IsCode(err, agent.CodeInvalid) {
		t.Fatal(err)
	}
	_ = ports
}

// ---- health ----

// hostRig is a rig whose health probes are dialled to a local server whatever
// the target address (the target is checked against the subnet first).
func healthRig(t *testing.T, h http.Handler) (*rig, *[]string) {
	t.Helper()
	srv := httptest.NewServer(h)
	t.Cleanup(srv.Close)
	var dialled []string
	var mu = make(chan struct{}, 1)
	r := newRig(t, func(d *Deps) {
		d.dial = func(ctx context.Context, network, addr string) (net.Conn, error) {
			mu <- struct{}{}
			dialled = append(dialled, addr)
			<-mu
			return (&net.Dialer{}).DialContext(ctx, "tcp", srv.Listener.Addr().String())
		}
	})
	return r, &dialled
}

func TestHealthCheckRefusesTargetOutsideOurSubnet(t *testing.T) {
	r, dialled := healthRig(t, http.NotFoundHandler())
	for _, target := range []string{
		"127.0.0.1:80", "10.250.8.2:80", "10.250.6.255:80", "192.168.1.5:80", "169.254.169.254:80",
		"172.17.0.1:80", "[::1]:80", "10.250.7.2:0", "10.250.7.2", "localhost:80", "10.250.7.2:80/x", "010.250.7.2:80",
	} {
		for _, kind := range []string{KindHTTP, KindTCP} {
			_, err := r.cl.HealthCheck(ctx5(t), HealthArgs{Target: target, Kind: kind, Path: map[string]string{KindHTTP: "/"}[kind]})
			if !IsCode(err, agent.CodePolicy) && !IsCode(err, agent.CodeInvalid) {
				t.Errorf("%s %s: %v", kind, target, err)
			}
		}
	}
	if len(*dialled) != 0 {
		t.Errorf("dialled %v", *dialled)
	}
}

func TestHealthCheckRefusesNetworkGatewayAndBroadcast(t *testing.T) {
	r, dialled := healthRig(t, http.NotFoundHandler())
	for target, why := range map[string]string{
		"10.250.7.0:80": "network", "10.250.7.1:80": "gateway", "10.250.7.255:80": "broadcast",
		"10.250.7.192:80": "dynamic range start", "10.250.7.230:80": "dynamic range",
	} {
		_, err := r.cl.HealthCheck(ctx5(t), HealthArgs{Target: target, Kind: KindTCP})
		if !IsCode(err, agent.CodePolicy) {
			t.Errorf("%s: %v", why, err)
		}
	}
	if len(*dialled) != 0 {
		t.Errorf("dialled %v", *dialled)
	}
	if res, err := r.cl.HealthCheck(ctx5(t), HealthArgs{Target: "10.250.7.2:80", Kind: KindTCP}); err != nil || !res.OK {
		t.Errorf("a static address: %+v %v", res, err)
	}
}

func TestHealthCheckDoesNotFollowRedirectsOrUseProxyEnv(t *testing.T) {
	var hits atomic.Int32
	var host atomic.Value
	var proxied atomic.Int32
	proxy := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { proxied.Add(1) }))
	defer proxy.Close()
	t.Setenv("HTTP_PROXY", proxy.URL)
	t.Setenv("http_proxy", proxy.URL)
	t.Setenv("ALL_PROXY", proxy.URL)
	t.Setenv("NO_PROXY", "")
	r, dialled := healthRig(t, http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
		hits.Add(1)
		host.Store(req.Host)
		if req.URL.Path == "/start" {
			http.Redirect(w, req, "/elsewhere", http.StatusFound)
			return
		}
		io.WriteString(w, strings.Repeat("x", 100000))
	}))
	res, err := r.cl.HealthCheck(ctx5(t), HealthArgs{Target: "10.250.7.2:8080", Kind: KindHTTP, Path: "/start"})
	if err != nil || res.Status != 302 || !res.OK {
		t.Fatalf("%+v %v", res, err)
	}
	if hits.Load() != 1 || proxied.Load() != 0 {
		t.Errorf("hits %d via proxy %d", hits.Load(), proxied.Load())
	}
	if host.Load() != "10.250.7.2" {
		t.Errorf("Host %v", host.Load())
	}
	if len(*dialled) != 1 || (*dialled)[0] != "10.250.7.2:8080" {
		t.Errorf("dialled %v", *dialled)
	}
	// A big body is read only to 4 KiB and the status still comes back.
	res, err = r.cl.HealthCheck(ctx5(t), HealthArgs{Target: "10.250.7.2:8080", Kind: KindHTTP, Path: "/ok"})
	if err != nil || res.Status != 200 || !res.OK {
		t.Fatalf("%+v %v", res, err)
	}
}

func TestHealthCheckResults(t *testing.T) {
	r, _ := healthRig(t, http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
		switch req.URL.Path {
		case "/500":
			w.WriteHeader(500)
		case "/404":
			w.WriteHeader(404)
		case "/slow":
			time.Sleep(2 * time.Second)
		}
	}))
	for path, want := range map[string]struct {
		ok     bool
		status int
	}{"/": {true, 200}, "/500": {false, 500}, "/404": {false, 404}} {
		res, err := r.cl.HealthCheck(ctx5(t), HealthArgs{Target: "10.250.7.2:80", Kind: KindHTTP, Path: path})
		if err != nil || res.OK != want.ok || res.Status != want.status {
			t.Errorf("%s: %+v %v", path, res, err)
		}
	}
	res, err := r.cl.HealthCheck(ctx5(t), HealthArgs{Target: "10.250.7.2:80", Kind: KindHTTP, Path: "/slow", TimeoutMS: 100})
	if err != nil || res.OK || res.Err != "timeout" || res.MS > 1000 {
		t.Errorf("slow: %+v %v", res, err)
	}
	for name, a := range map[string]HealthArgs{
		"no kind":         {Target: "10.250.7.2:80"},
		"bad kind":        {Target: "10.250.7.2:80", Kind: "exec"},
		"path no slash":   {Target: "10.250.7.2:80", Kind: KindHTTP, Path: "x"},
		"path with space": {Target: "10.250.7.2:80", Kind: KindHTTP, Path: "/a b"},
		"path with CRLF":  {Target: "10.250.7.2:80", Kind: KindHTTP, Path: "/a\r\nX: y"},
		"path too long":   {Target: "10.250.7.2:80", Kind: KindHTTP, Path: "/" + strings.Repeat("a", 256)},
		"path on tcp":     {Target: "10.250.7.2:80", Kind: KindTCP, Path: "/"},
		"timeout too big": {Target: "10.250.7.2:80", Kind: KindTCP, TimeoutMS: MaxHealthMS + 1},
		"negative":        {Target: "10.250.7.2:80", Kind: KindTCP, TimeoutMS: -1},
	} {
		if _, err := r.cl.HealthCheck(ctx5(t), a); !IsCode(err, agent.CodeInvalid) {
			t.Errorf("%s: %v", name, err)
		}
	}
}

func TestHealthCheckTCPRefusedIsAFailedProbeNotAnError(t *testing.T) {
	r := newRig(t, func(d *Deps) {
		d.dial = func(ctx context.Context, network, addr string) (net.Conn, error) {
			l, _ := net.Listen("tcp", "127.0.0.1:0")
			a := l.Addr().String()
			l.Close()
			return (&net.Dialer{}).DialContext(ctx, "tcp", a)
		}
	})
	res, err := r.cl.HealthCheck(ctx5(t), HealthArgs{Target: "10.250.7.2:80", Kind: KindTCP})
	if err != nil || res.OK || res.Err != "connection refused" {
		t.Fatalf("%+v %v", res, err)
	}
}

// ---- list, ports, networks ----

func TestContainerListReturnsStoppedContainersWithTheirConfiguredAddress(t *testing.T) {
	r := newRig(t)
	r.fd.add(&fakeContainer{Name: "sd-p-web-d1-a1", Labels: ours(nil), State: "exited", IP: "", Configured: "10.250.7.9"})
	r.fd.add(&fakeContainer{Name: "sd-p-web-d2-a1", Labels: ours(map[string]string{LabelDeploy: "2"}), IP: "10.250.7.3", Configured: "10.250.7.3"})
	r.fd.add(&fakeContainer{Name: "weird", Labels: ours(map[string]string{"serverdock.extra": "1"}), Configured: "10.250.7.4"})
	r.fd.add(&fakeContainer{Name: "foreign", Labels: map[string]string{"app": "x"}, Configured: "10.250.7.5"})
	r.fd.add(&fakeContainer{Name: "other-install", Labels: ours(map[string]string{LabelInstance: strings.Repeat("f", 32)}), Configured: "10.250.7.6"})
	res, err := r.cl.ContainerList(ctx5(t))
	if err != nil || len(res.Containers) != 3 {
		t.Fatalf("%+v %v", res, err)
	}
	by := map[string]ListedContainer{}
	for _, c := range res.Containers {
		by[c.Name] = c
	}
	stopped := by["sd-p-web-d1-a1"]
	if stopped.State != "exited" || stopped.IP != "" || stopped.ConfiguredIP != "10.250.7.9" || stopped.Unowned ||
		stopped.Labels != (ListedLabels{Project: "3", Service: "7", Deploy: "1", Attempt: "1"}) {
		t.Errorf("stopped: %+v", stopped)
	}
	if c := by["sd-p-web-d2-a1"]; c.IP != "10.250.7.3" || c.Labels.Deploy != "2" {
		t.Errorf("running: %+v", c)
	}
	if w := by["weird"]; !w.Unowned || w.ConfiguredIP != "10.250.7.4" {
		t.Errorf("weird: %+v", w)
	}
	if _, ok := by["foreign"]; ok {
		t.Error("a container without our label is listed")
	}
	if _, ok := by["other-install"]; ok {
		t.Error("another install's container is listed")
	}
}

func TestContainerListNeverReturnsAPartialList(t *testing.T) {
	r := newRig(t)
	for i := 0; i < MaxList+1; i++ {
		r.fd.add(&fakeContainer{Name: fmt.Sprintf("sd-p-web-d%d-a1", i), Labels: ours(map[string]string{LabelDeploy: fmt.Sprint(i)})})
	}
	_, err := r.cl.ContainerList(ctx5(t))
	wantCode(t, err, agent.CodeConflict)
	// Exactly MaxList of ordinary entries still fits the result cap.
	r2 := newRig(t)
	for i := 0; i < MaxList; i++ {
		r2.fd.add(&fakeContainer{Name: fmt.Sprintf("sd-p-web-d%d-a1", i), Labels: ours(map[string]string{LabelDeploy: fmt.Sprint(i)}),
			IP: "10.250.7.2", Configured: "10.250.7.2"})
	}
	res, err := r2.cl.ContainerList(ctx5(t))
	if err != nil || len(res.Containers) != MaxList {
		t.Fatalf("%d %v", len(res.Containers), err)
	}
	// Entries too big to fit the result cap fail as well, never shrink.
	r3 := newRig(t)
	for i := 0; i < MaxList; i++ {
		l := ours(map[string]string{LabelProject: strings.Repeat("p", 64), LabelService: strings.Repeat("s", 64), LabelDeploy: strings.Repeat("9", 64), LabelAttempt: strings.Repeat("9", 64)})
		r3.fd.add(&fakeContainer{Name: strings.Repeat("n", 128) + fmt.Sprint(i), Labels: l, IP: "255.255.255.255", Configured: "255.255.255.255"})
	}
	_, err = r3.cl.ContainerList(ctx5(t))
	wantCode(t, err, agent.CodeConflict)
}

func TestContainerPortsResultCap(t *testing.T) {
	r := newRig(t)
	for i := 0; i < MaxList+50; i++ {
		r.fd.add(&fakeContainer{Name: fmt.Sprintf("%s%d", strings.Repeat("n", 60), i), Labels: map[string]string{},
			Ports: []map[string]any{{"IP": "255.255.255.255", "PrivatePort": 65535, "PublicPort": 65535, "Type": "tcp"}}})
	}
	pr, err := r.cl.ContainerPorts(ctx5(t))
	if err != nil || !pr.Truncated || len(pr.Containers) == 0 || len(pr.Containers) > MaxList {
		t.Fatalf("ports: %d %v truncated=%v", len(pr.Containers), err, pr.Truncated)
	}
}

// container_ports is the exposure check's source: it leaves this install's own
// containers out (matched by the serverdock.instance label, never by name) and
// keeps another install's and every unlabelled one.
func TestContainerPortsExcludesOurOwnContainers(t *testing.T) {
	r := newRig(t)
	pub := []map[string]any{{"IP": "0.0.0.0", "PrivatePort": 80, "PublicPort": 8080, "Type": "tcp"}}
	r.fd.add(&fakeContainer{Name: "ours", Labels: map[string]string{LabelInstance: testInstance}, Ports: pub})
	r.fd.add(&fakeContainer{Name: "sd-looks-ours-d1-a1", Labels: map[string]string{}, Ports: pub}) // a name proves nothing
	r.fd.add(&fakeContainer{Name: "other-install", Labels: map[string]string{LabelInstance: strings.Repeat("f", 32)}, Ports: pub})
	r.fd.add(&fakeContainer{Name: "grafana", Labels: map[string]string{}, Ports: pub})
	res, err := r.cl.ContainerPorts(ctx5(t))
	if err != nil {
		t.Fatal(err)
	}
	var names []string
	for _, c := range res.Containers {
		names = append(names, c.Name)
	}
	if got := strings.Join(names, ","); got != "grafana,other-install,sd-looks-ours-d1-a1" {
		t.Errorf("containers listed: %s", got)
	}
}

func TestContainerPortsDedupesOrdersAndFlagsTruncation(t *testing.T) {
	r := newRig(t)
	p := func(ip string, host int) map[string]any {
		return map[string]any{"IP": ip, "PrivatePort": 80, "PublicPort": host, "Type": "tcp"}
	}
	// Six distinct ports; the all-interfaces one is last in Docker's order, and
	// every port has an IPv4 and an IPv6 entry.
	var ports []map[string]any
	for h := 9001; h <= 9005; h++ {
		ports = append(ports, p("127.0.0.1", h), p("::1", h))
	}
	ports = append(ports, p("0.0.0.0", 9100), p("::", 9100))
	r.fd.add(&fakeContainer{Name: strings.Repeat("n", 100) + "\x1b[31m", Labels: map[string]string{}, Ports: ports})
	r.fd.add(&fakeContainer{Name: "two", Labels: map[string]string{}, Ports: []map[string]any{p("0.0.0.0", 8080), p("::", 8080)}})
	r.fd.add(&fakeContainer{Name: "quiet", Labels: map[string]string{}})
	res, err := r.cl.ContainerPorts(ctx5(t))
	if err != nil || len(res.Containers) != 2 {
		t.Fatalf("%+v %v", res, err)
	}
	by := map[string]PublishingContainer{}
	for _, c := range res.Containers {
		by[c.Name] = c
	}
	two := by["two"]
	if len(two.Ports) != 1 || two.PortsTruncated || two.Ports[0].HostIP != "0.0.0.0" {
		t.Errorf("v4/v6 duplicates not merged: %+v", two)
	}
	var big PublishingContainer
	for n, c := range by {
		if n != "two" {
			big = c
			if len(n) > MaxNameText || strings.ContainsRune(n, 0x1b) {
				t.Errorf("name %q", n)
			}
		}
	}
	if len(big.Ports) != MaxPortsEach || !big.PortsTruncated || big.Ports[0].HostPort != 9100 || big.Ports[0].HostIP != "0.0.0.0" {
		t.Errorf("the exposing port must come first and the cut be flagged: %+v", big)
	}
	for _, q := range big.Ports[1:] {
		if q.HostIP != "127.0.0.1" {
			t.Errorf("loopback entry %+v", q)
		}
	}
}

func TestNetworkEnsureAndInspect(t *testing.T) {
	r := newRig(t)
	r.fd.netMissing = true
	n, err := r.cl.NetworkEnsure(ctx5(t), testNet, testSubnet)
	if err != nil || n.Subnet != testSubnet || n.Gateway != "10.250.7.1" {
		t.Fatalf("%+v %v", n, err)
	}
	if n2, err := r.cl.NetworkEnsure(ctx5(t), testNet, testSubnet); err != nil || n2 != n {
		t.Fatalf("not idempotent: %+v %v", n2, err)
	}
	if r.fd.called("POST /networks/create") != 1 {
		t.Error("created twice")
	}
	// The agent manages one network only.
	_, err = r.cl.NetworkEnsure(ctx5(t), "bridge", testSubnet)
	wantCode(t, err, agent.CodePolicy)
	_, err = r.cl.NetworkEnsure(ctx5(t), testNet, "10.250.7.0/8")
	wantCode(t, err, agent.CodeInvalid)
	_, err = r.cl.NetworkEnsure(ctx5(t), testNet, "10.250.9.0/24") // another subnet than the existing one
	wantCode(t, err, agent.CodeConflict)
	r.fd.netLabels = map[string]string{LabelInstance: strings.Repeat("f", 32)} // not ours
	_, err = r.cl.NetworkEnsure(ctx5(t), testNet, testSubnet)
	wantCode(t, err, agent.CodeConflict)

	r.fd.netLabels = map[string]string{LabelInstance: testInstance}
	r.fd.netUsed = map[string]string{"10.250.7.3": strings.Repeat("a", 64), "10.250.7.2": strings.Repeat("b", 64)}
	u, err := r.cl.NetworkInspect(ctx5(t))
	if err != nil || u.Subnet != testSubnet || u.Gateway != "10.250.7.1" || u.IPRange != "10.250.7.192/26" ||
		!reflect.DeepEqual(u.Used, []string{"10.250.7.2", "10.250.7.3"}) {
		t.Fatalf("%+v %v", u, err)
	}
	r.fd.netLabels = nil
	_, err = r.cl.NetworkInspect(ctx5(t))
	wantCode(t, err, agent.CodeRefusedUnowned)
	r.fd.netMissing = true
	_, err = r.cl.NetworkInspect(ctx5(t))
	wantCode(t, err, agent.CodeNotFound)
}

func TestNetworkInspectRefusesToDropAddresses(t *testing.T) {
	r := newRig(t)
	for i := 0; i < MaxUsedAddrs+1; i++ {
		r.fd.netUsed[fmt.Sprintf("10.250.%d.%d", 8+i/250, 1+i%250)] = fmt.Sprintf("%064x", i)
	}
	_, err := r.cl.NetworkInspect(ctx5(t))
	wantCode(t, err, agent.CodeConflict)
}

// ---- events ----

func eventLine(action, id string) string {
	return fmt.Sprintf(`{"Type":"container","Action":%q,"Actor":{"ID":%q,"Attributes":{"serverdock.service":"7","serverdock.deploy":"3","exitCode":"1"}},"time":1700000000}`+"\n", action, id)
}

func TestEventsOpHasNoTimeoutAndEndsWithDockerStream(t *testing.T) {
	sp, ok := agent.SpecFor(agent.OpEvents)
	if !ok || sp.Timeout != 0 || sp.RemoteOK {
		t.Fatalf("events spec %+v", sp)
	}
	for _, other := range agent.Specs() {
		if other.Name != agent.OpEvents && other.Name != "logs" && other.Timeout == 0 {
			t.Errorf("%s has no timeout", other.Name)
		}
	}
	r := newRig(t)
	release := make(chan struct{})
	r.fd.events = func(w http.ResponseWriter, req *http.Request) {
		io.WriteString(w, eventLine("die", strings.Repeat("a", 64)))
		w.(http.Flusher).Flush()
		select {
		case <-release:
		case <-req.Context().Done():
			return
		}
		io.WriteString(w, eventLine("start", strings.Repeat("b", 64)))
		// then Docker ends the stream
	}
	var got []Event
	ready := make(chan struct{}, 1)
	done := make(chan error, 1)
	go func() {
		done <- r.cl.Events(context.Background(), func() { ready <- struct{}{} }, func(e Event) error { got = append(got, e); return nil })
	}()
	select {
	case <-ready: // the open signal arrives before any event is read
	case err := <-done:
		t.Fatalf("events ended early: %v", err)
	case <-time.After(5 * time.Second):
		t.Fatal("no open signal")
	}
	select {
	case err := <-done:
		t.Fatalf("events ended early: %v", err)
	case <-time.After(300 * time.Millisecond):
	}
	close(release)
	select {
	case err := <-done:
		if err != nil {
			t.Fatalf("the op did not end cleanly with Docker's stream: %v", err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("the op did not end when Docker's stream did")
	}
	if len(got) != 2 || got[0].Action != "die" || got[0].Service != "7" || got[0].Deploy != "3" || got[0].ExitCode != "1" ||
		got[1].Action != "start" || got[1].ID != strings.Repeat("b", 64) || got[0].Time != 1700000000 {
		t.Fatalf("%+v", got)
	}
	// The stream is filtered to this install.
	if r.fd.called("GET /events") != 1 {
		t.Error("one Docker events stream per op")
	}
}

func TestEventsCallbackErrorAndCancelStopTheStream(t *testing.T) {
	r := newRig(t)
	r.fd.events = func(w http.ResponseWriter, req *http.Request) {
		for i := 0; i < 5; i++ {
			io.WriteString(w, eventLine("die", strings.Repeat("a", 64)))
			w.(http.Flusher).Flush()
		}
		<-req.Context().Done()
	}
	stop := fmt.Errorf("enough")
	var n int
	err := r.cl.Events(context.Background(), nil, func(Event) error { n++; return stop })
	if err != stop || n != 1 {
		t.Fatalf("%v after %d events", err, n)
	}
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- r.cl.Events(ctx, nil, func(Event) error { return nil }) }()
	time.Sleep(200 * time.Millisecond)
	cancel()
	select {
	case err := <-done:
		if err == nil {
			t.Error("a cancelled stream reported success")
		}
	case <-time.After(5 * time.Second):
		t.Fatal("cancel did not stop the events op")
	}
}

func TestEventsDockerLineOver64KiBEndsTheOpWithAnError(t *testing.T) {
	r := newRig(t)
	r.fd.events = func(w http.ResponseWriter, req *http.Request) {
		io.WriteString(w, `{"Type":"container","pad":"`+strings.Repeat("x", 70<<10)+`"}`+"\n")
	}
	err := r.cl.Events(ctx5(t), nil, func(Event) error { return nil })
	wantCode(t, err, agent.CodeDocker)
}

func TestCreateRefusesAnImageWithServerdockLabels(t *testing.T) {
	r := newRig(t)
	b, _ := json.Marshal(map[string]any{"Id": imgID, "Config": map[string]any{"Labels": map[string]any{"serverdock.extra": "1"}}})
	r.fd.images[imgID] = string(b)
	_, err := r.create(goodCreate())
	wantCode(t, err, agent.CodePolicy)
	if r.fd.called("POST /containers/create") != 0 {
		t.Error("a spoofing image reached create")
	}
	// An image Docker does not know is not_found, also before create.
	delete(r.fd.images, imgID)
	_, err = r.create(goodCreate())
	wantCode(t, err, agent.CodeNotFound)
}

func TestCreateConflictWithAnotherAddressFails(t *testing.T) {
	r := newRig(t)
	r.fd.add(&fakeContainer{Name: "sd-p-web-d1-a1", Labels: ours(nil), Configured: "10.250.7.9"})
	_, err := r.create(goodCreate())
	if !IsCode(err, agent.CodeConflict) || !strings.Contains(err.Error(), "another address") {
		t.Fatalf("%v", err)
	}
}

func TestCreateModelsTheConfiguredAddressForTheList(t *testing.T) {
	r := newRig(t)
	if _, err := r.create(goodCreate()); err != nil {
		t.Fatal(err)
	}
	res, err := r.cl.ContainerList(ctx5(t))
	if err != nil || len(res.Containers) != 1 || res.Containers[0].ConfiguredIP != "10.250.7.2" || res.Containers[0].IP != "" {
		t.Fatalf("%+v %v", res, err)
	}
}

func TestHealthCheckRefusesAnOversizedResponseHeader(t *testing.T) {
	r, _ := healthRig(t, http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
		w.Header().Set("X-Big", strings.Repeat("a", 64<<10))
	}))
	res, err := r.cl.HealthCheck(ctx5(t), HealthArgs{Target: "10.250.7.2:80", Kind: KindHTTP, Path: "/"})
	if err != nil || res.OK || res.Err != "request failed" {
		t.Fatalf("%+v %v", res, err)
	}
}

func TestStallIsCappedAndShortStallsRoundUp(t *testing.T) {
	r := newRig(t)
	if MaxStallS != 600 {
		t.Fatalf("MaxStallS %d", MaxStallS)
	}
	if _, err := r.rawCall(agent.OpImagePull, PullArgs{Ref: "x:1", StallS: 601}); !IsCode(err, agent.CodeInvalid) {
		t.Fatal(err)
	}
	var got PullArgs
	s := &stub{result: json.RawMessage(`{"image_id":"` + imgID + `"}`), onArgs: func(a any) { got = a.(PullArgs) }}
	for in, want := range map[time.Duration]int{0: 0, time.Millisecond: 1, 1500 * time.Millisecond: 2, 30 * time.Second: 30} {
		if _, err := NewClient(s).ImagePull(context.Background(), "x:1", in, nil); err != nil || got.StallS != want {
			t.Errorf("%v -> %d (want %d) %v", in, got.StallS, want, err)
		}
	}
}

func TestPullRefusesMetadataAndLoopbackRegistriesInTheAgent(t *testing.T) {
	r := newRig(t)
	r.fd.addImage("x:1", imgID)
	for _, ref := range []string{"169.254.169.254/x:1", "metadata.google.internal/x:1", "127.0.0.1:5000/x:1", "localhost/x:1", "localhost4:5000/x:1"} {
		_, err := r.cl.ImagePull(ctx5(t), ref, 0, nil)
		wantCode(t, err, agent.CodePolicy)
	}
	// Forms the Docker client's own reference check already refuses.
	for _, ref := range []string{"[fe80::1]:5000/x:1", "[::ffff:169.254.169.254]/x:1"} {
		if _, err := r.cl.ImagePull(ctx5(t), ref, 0, nil); err == nil {
			t.Errorf("%s accepted", ref)
		}
	}
	if r.fd.called("POST /images/create") != 0 {
		t.Error("a refused registry reached Docker")
	}
	// With the option the loopback registry is allowed; a metadata one never is.
	r2 := newRig(t, func(d *Deps) { d.AllowLoopbackRegistry = true })
	r2.fd.addImage("127.0.0.1:5000/x:1", imgID)
	if _, err := r2.cl.ImagePull(ctx5(t), "127.0.0.1:5000/x:1", 0, nil); err != nil {
		t.Errorf("loopback with the option: %v", err)
	}
	_, err := r2.cl.ImagePull(ctx5(t), "169.254.169.254/x:1", 0, nil)
	wantCode(t, err, agent.CodePolicy)
}
