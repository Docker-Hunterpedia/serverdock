package agentops

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"io"
	"log/slog"
	"net"
	"net/http"
	"net/http/httptest"
	"net/netip"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/Docker-Hunterpedia/serverdock/internal/agent"
	"github.com/Docker-Hunterpedia/serverdock/internal/agent/dockerapi"
	"github.com/Docker-Hunterpedia/serverdock/internal/hub"
)

var quiet = slog.New(slog.NewTextHandler(io.Discard, nil))

const (
	testInstance = "0123456789abcdef0123456789abcdef"
	testSubnet   = "10.250.7.0/24"
	imgID        = "sha256:aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"
	otherImgID   = "sha256:bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb"
)

var testNet = NetworkName(testInstance)

type fakeContainer struct {
	ID, Name, Image string
	Labels          map[string]string
	State           string
	IP, Configured  string
	Ports           []map[string]any
}

// fakeDocker is a small stateful dockerd on a unix socket.
type fakeDocker struct {
	mu           sync.Mutex
	calls        []string
	createBodies [][]byte
	containers   []*fakeContainer
	images       map[string]string // reference or id -> inspect JSON
	netMissing   bool
	netLabels    map[string]string
	netUsed      map[string]string // ip -> container id
	dataRoot     string
	pull         func(w http.ResponseWriter, r *http.Request)
	events       func(w http.ResponseWriter, r *http.Request)
	listExtra    []map[string]any // raw list entries appended as they are
}

func containerID(name string) string {
	h := sha256.Sum256([]byte(name))
	return hex.EncodeToString(h[:])
}

func (f *fakeDocker) record(s string) { f.mu.Lock(); f.calls = append(f.calls, s); f.mu.Unlock() }

func (f *fakeDocker) called(prefix string) int {
	f.mu.Lock()
	defer f.mu.Unlock()
	n := 0
	for _, c := range f.calls {
		if strings.HasPrefix(c, prefix) {
			n++
		}
	}
	return n
}

func (f *fakeDocker) add(c *fakeContainer) *fakeContainer {
	f.mu.Lock()
	defer f.mu.Unlock()
	if c.ID == "" {
		c.ID = containerID(c.Name)
	}
	if c.State == "" {
		c.State = "running"
	}
	if c.Image == "" {
		c.Image = imgID
	}
	f.containers = append(f.containers, c)
	return c
}

func ours(extra map[string]string) map[string]string {
	l := map[string]string{
		LabelManaged: "1", LabelInstance: testInstance, LabelProject: "p", LabelService: "7", LabelDeploy: "1", LabelAttempt: "1",
	}
	for k, v := range extra {
		if v == "" {
			delete(l, k)
		} else {
			l[k] = v
		}
	}
	return l
}

func (f *fakeDocker) addImage(ref, id string, digests ...string) {
	b, _ := json.Marshal(map[string]any{
		"Id": id, "RepoDigests": digests,
		"Config": map[string]any{"ExposedPorts": map[string]any{"8080/tcp": map[string]any{}}},
	})
	f.mu.Lock()
	f.images[ref], f.images[id] = string(b), string(b)
	f.mu.Unlock()
}

func (f *fakeDocker) find(idOrName string) *fakeContainer {
	for _, c := range f.containers {
		if c.ID == idOrName || c.Name == idOrName {
			return c
		}
	}
	return nil
}

func (f *fakeDocker) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	if r.URL.Path == "/_ping" {
		w.Header().Set("API-Version", "1.45")
		io.WriteString(w, "OK")
		return
	}
	_, path, _ := strings.Cut(strings.TrimPrefix(r.URL.Path, "/v"), "/")
	path = "/" + path
	body, _ := io.ReadAll(io.LimitReader(r.Body, 1<<20))
	f.record(r.Method + " " + path)
	jerr := func(code int, msg string) {
		w.WriteHeader(code)
		json.NewEncoder(w).Encode(map[string]string{"message": msg})
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	switch {
	case path == "/info":
		json.NewEncoder(w).Encode(map[string]string{"DockerRootDir": f.dataRoot})
	case path == "/images/create":
		f.mu.Unlock()
		f.pull(w, r)
		f.mu.Lock()
	case strings.HasPrefix(path, "/images/") && strings.HasSuffix(path, "/json"):
		ref := strings.TrimSuffix(strings.TrimPrefix(path, "/images/"), "/json")
		if j, ok := f.images[ref]; ok {
			io.WriteString(w, j)
			return
		}
		jerr(404, "no such image")
	case strings.HasPrefix(path, "/networks/") && r.Method == http.MethodGet:
		if f.netMissing || strings.TrimPrefix(path, "/networks/") != testNet {
			jerr(404, "no such network")
			return
		}
		used := map[string]any{}
		for ip, id := range f.netUsed {
			used[id] = map[string]string{"IPv4Address": ip + "/24"}
		}
		json.NewEncoder(w).Encode(map[string]any{
			"Id": "net1", "Name": testNet, "Driver": "bridge", "Labels": f.netLabels,
			"Options":    map[string]string{"com.docker.network.bridge.enable_icc": "false"},
			"IPAM":       map[string]any{"Config": []map[string]string{{"Subnet": testSubnet, "IPRange": "10.250.7.192/26", "Gateway": "10.250.7.1"}}},
			"Containers": used,
		})
	case path == "/networks/create":
		f.netMissing = false
		var req struct{ Labels map[string]string }
		_ = json.Unmarshal(body, &req)
		f.netLabels = req.Labels
		w.WriteHeader(201)
		io.WriteString(w, `{"Id":"net1"}`)
	case path == "/containers/create":
		name := r.URL.Query().Get("name")
		if f.find(name) != nil {
			jerr(409, "Conflict. The container name is already in use")
			return
		}
		f.createBodies = append(f.createBodies, body)
		var req struct {
			Image  string
			Labels map[string]string
		}
		_ = json.Unmarshal(body, &req)
		c := &fakeContainer{Name: name, ID: containerID(name), Image: req.Image, Labels: req.Labels, State: "created"}
		f.containers = append(f.containers, c)
		w.WriteHeader(201)
		json.NewEncoder(w).Encode(map[string]string{"Id": c.ID})
	case path == "/containers/json":
		var flt struct{ Label []string }
		_ = json.Unmarshal([]byte(r.URL.Query().Get("filters")), &flt)
		out := []map[string]any{}
	next:
		for _, c := range f.containers {
			for _, l := range flt.Label {
				k, v, hasV := strings.Cut(l, "=")
				if got, ok := c.Labels[k]; !ok || (hasV && got != v) {
					continue next
				}
			}
			if r.URL.Query().Get("all") != "1" && c.State != "running" {
				continue
			}
			out = append(out, map[string]any{
				"Id": c.ID, "Names": []string{"/" + c.Name}, "ImageID": c.Image, "State": c.State, "Labels": c.Labels, "Ports": c.Ports,
				"NetworkSettings": map[string]any{"Networks": map[string]any{testNet: map[string]any{
					"IPAddress": c.IP, "IPAMConfig": map[string]string{"IPv4Address": c.Configured}}}},
			})
		}
		out = append(out, f.listExtra...)
		json.NewEncoder(w).Encode(out)
	case strings.HasPrefix(path, "/containers/"):
		rest := strings.TrimPrefix(path, "/containers/")
		idOrName, action, _ := strings.Cut(rest, "/")
		c := f.find(idOrName)
		if c == nil {
			jerr(404, "no such container")
			return
		}
		switch {
		case action == "json":
			running := c.State == "running"
			json.NewEncoder(w).Encode(map[string]any{
				"Id": c.ID, "Name": "/" + c.Name, "Image": c.Image, "RestartCount": 0,
				"State":      map[string]any{"Status": c.State, "Running": running},
				"Config":     map[string]any{"Labels": c.Labels},
				"HostConfig": map[string]string{"NetworkMode": testNet},
				"NetworkSettings": map[string]any{"Networks": map[string]any{testNet: map[string]any{
					"IPAddress": c.IP, "IPAMConfig": map[string]string{"IPv4Address": c.Configured}}}},
			})
		case action == "start":
			if c.State == "running" {
				w.WriteHeader(304)
				return
			}
			c.State = "running"
			w.WriteHeader(204)
		case action == "stop":
			c.State = "exited"
			w.WriteHeader(204)
		case action == "restart":
			c.State = "running"
			w.WriteHeader(204)
		case action == "" && r.Method == http.MethodDelete:
			if c.State == "running" {
				jerr(409, "You cannot remove a running container")
				return
			}
			for i, x := range f.containers {
				if x == c {
					f.containers = append(f.containers[:i], f.containers[i+1:]...)
					break
				}
			}
			w.WriteHeader(204)
		default:
			jerr(400, "unsupported")
		}
	case path == "/events":
		f.mu.Unlock()
		f.events(w, r)
		f.mu.Lock()
	default:
		jerr(404, "unknown path "+path)
	}
}

// rig is an agent (real Serve over a pipe, ModeLocal) with a hub session in
// front of it, talking to a fake dockerd.
type rig struct {
	t    *testing.T
	fd   *fakeDocker
	sess *hub.Session
	cl   *Client
	deps Deps
}

func newFake() *fakeDocker {
	f := &fakeDocker{
		images:    map[string]string{},
		netLabels: map[string]string{LabelInstance: testInstance},
		netUsed:   map[string]string{},
		dataRoot:  "/var/lib/docker",
	}
	f.pull = func(w http.ResponseWriter, r *http.Request) {
		io.WriteString(w, `{"status":"Status: Image is up to date for x"}`+"\n")
	}
	f.events = func(w http.ResponseWriter, r *http.Request) {}
	return f
}

func newRig(t *testing.T, mods ...func(*Deps)) *rig {
	t.Helper()
	fd := newFake()
	dir, err := os.MkdirTemp("", "sdo")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { os.RemoveAll(dir) })
	sock := filepath.Join(dir, "d.sock")
	ln, err := net.Listen("unix", sock)
	if err != nil {
		t.Fatal(err)
	}
	srv := &httptest.Server{Listener: ln, Config: &http.Server{Handler: fd}}
	srv.Start()
	t.Cleanup(srv.Close)
	docker := dockerapi.New(sock)
	t.Cleanup(docker.Close)

	deps := Deps{Docker: docker, Instance: testInstance, CheckCreate: InterimCheckCreate, Log: quiet,
		freeBytes: func(string) (uint64, error) { return 100 << 30, nil }}
	for _, m := range mods {
		m(&deps)
	}
	sess := attachAgent(t, docker, deps)
	return &rig{t: t, fd: fd, sess: sess, cl: NewClient(sess), deps: deps}
}

// goodCreate is a valid create request for the rig's network.
func goodCreate() CreateArgs {
	return CreateArgs{
		Name: "sd-p-web-d1-a1", IP: "10.250.7.2", ImageID: imgID,
		Spec: dockerapi.CreateSpec{
			Image:        imgID,
			Labels:       map[string]string{LabelManaged: "1", LabelProject: "p", LabelService: "7", LabelDeploy: "1", LabelAttempt: "1"},
			ExposedPorts: map[string]struct{}{"8080/tcp": {}},
			HostConfig: dockerapi.HostConfig{
				RestartPolicy: dockerapi.RestartPolicy{Name: "unless-stopped"},
				LogConfig:     dockerapi.LogConfig{Type: "local", Config: map[string]string{"max-size": "10m", "max-file": "3"}},
				NetworkMode:   testNet, PidsLimit: 4096,
			},
			NetworkingConfig: dockerapi.NetworkingConfig{EndpointsConfig: map[string]dockerapi.EndpointConfig{
				testNet: {IPAMConfig: dockerapi.EndpointIPAM{IPv4Address: "10.250.7.2"}}}},
		},
	}
}

func (r *rig) create(a CreateArgs) (CreateResult, error) {
	return r.cl.ContainerCreate(context.Background(), a.Name, a.Spec, a.IP, a.ImageID)
}

// rawCall sends a request the typed client would not (to test the agent's
// refusals of hostile input).
func (r *rig) rawCall(op string, args any) (json.RawMessage, error) {
	return r.sess.Call(context.Background(), op, args, nil)
}

func mustURL(s string) *url.URL { u, _ := url.Parse(s); return u }

func newNetInfo(subnet string) dockerapi.NetworkInfo {
	gw, rng, _ := networkAddrsForTest(subnet)
	return dockerapi.NetworkInfo{Subnet: subnet, Gateway: gw, IPRange: rng}
}

func networkAddrsForTest(subnet string) (string, string, error) {
	p := netip.MustParsePrefix(subnet)
	r, err := dockerapi.DynamicRange(subnet)
	return p.Addr().Next().String(), r.String(), err
}

func mustAddr(t *testing.T, s string) netip.Addr {
	t.Helper()
	a, err := netip.ParseAddr(s)
	if err != nil {
		t.Fatal(err)
	}
	return a
}

// attachAgent serves the handlers over an in-memory pipe (the real agent.Serve
// in ModeLocal) and returns the hub session in front of it, handshake done.
func attachAgent(t *testing.T, docker *dockerapi.Client, deps Deps) *hub.Session {
	t.Helper()
	ops, err := Handlers(deps)
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	h := hub.New(ctx, "", quiet)
	a, b := net.Pipe()
	var wg sync.WaitGroup
	wg.Go(func() { _ = h.Attach(ctx, hub.LocalID, a, 0) })
	wg.Go(func() {
		_ = agent.Serve(ctx, b, agent.Config{Logs: docker, Version: "t", Mode: agent.ModeLocal, Ops: ops}, 0, quiet)
	})
	t.Cleanup(func() { cancel(); wg.Wait() })
	var sess *hub.Session
	for i := 0; i < 500 && sess == nil; i++ {
		if s, ok := h.Get(hub.LocalID); ok {
			sess = s
			break
		}
		time.Sleep(10 * time.Millisecond)
	}
	if sess == nil {
		t.Fatal("agent did not attach")
	}
	select {
	case <-sess.HelloDone():
	case <-time.After(10 * time.Second):
		t.Fatal("no hello")
	}
	return sess
}
