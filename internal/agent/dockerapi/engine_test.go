package dockerapi

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"
)

// recorded is one request the fake engine saw.
type recorded struct {
	Method, Path, Query string
	Body                []byte
}

// engine is a programmable fake dockerd: every versioned request goes to h and
// is recorded.
type engine struct {
	mu   sync.Mutex
	reqs []recorded
	h    func(w http.ResponseWriter, r *http.Request)
}

func (e *engine) last() recorded {
	e.mu.Lock()
	defer e.mu.Unlock()
	if len(e.reqs) == 0 {
		return recorded{}
	}
	return e.reqs[len(e.reqs)-1]
}

func (e *engine) count() int { e.mu.Lock(); defer e.mu.Unlock(); return len(e.reqs) }

// startEngine serves h on a unix socket (with /_ping and the /v<ver> prefix
// handled) and returns a client with a short deadline of 300 ms.
func startEngine(t *testing.T, h func(w http.ResponseWriter, r *http.Request)) (*Client, *engine) {
	t.Helper()
	e := &engine{h: h}
	fd := &fakeDocker{api: "1.45"}
	dir, err := os.MkdirTemp("", "sd")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { os.RemoveAll(dir) })
	sock := filepath.Join(dir, "d.sock")
	ln, err := net.Listen("unix", sock)
	if err != nil {
		t.Fatal(err)
	}
	inner := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(io.LimitReader(r.Body, 1<<20))
		e.mu.Lock()
		e.reqs = append(e.reqs, recorded{r.Method, r.URL.Path, r.URL.RawQuery, body})
		e.mu.Unlock()
		r.Body = io.NopCloser(bytes.NewReader(body))
		e.h(w, r)
	})
	srv := &httptest.Server{Listener: ln, Config: &http.Server{Handler: fd.versioned(inner)}}
	srv.Start()
	t.Cleanup(srv.Close)
	c := New(sock)
	c.short = 300 * time.Millisecond
	c.slack = 300 * time.Millisecond
	t.Cleanup(c.Close)
	return c, e
}

func reply(status int, body string) func(http.ResponseWriter, *http.Request) {
	return func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(status)
		io.WriteString(w, body)
	}
}

const (
	id64   = "0123456789abcdef0123456789abcdef0123456789abcdef0123456789abcdef"
	otherN = "serverdock-aabbccdd"
)

func goodSpec() CreateSpec {
	return CreateSpec{
		Image:        "sha256:" + id64,
		Labels:       map[string]string{"serverdock.managed": "1"},
		ExposedPorts: map[string]struct{}{"8080/tcp": {}},
		HostConfig: HostConfig{
			RestartPolicy: RestartPolicy{Name: "unless-stopped"},
			LogConfig:     LogConfig{Type: "local", Config: map[string]string{"max-size": "10m", "max-file": "3"}},
			NetworkMode:   otherN,
			PidsLimit:     4096,
		},
		NetworkingConfig: NetworkingConfig{EndpointsConfig: map[string]EndpointConfig{
			otherN: {IPAMConfig: EndpointIPAM{IPv4Address: "10.250.0.2"}},
		}},
	}
}

func okVerify([]byte) error { return nil }

// ops lists every new method as a closure with its expected success status.
type op struct {
	name string
	ok   int    // success status and body
	body string // success body
	run  func(c *Client) error
}

func allOps() []op {
	ctx := context.Background()
	return []op{
		{"ImageInspect", 200, `{"Id":"sha256:` + id64 + `"}`, func(c *Client) error { _, err := c.ImageInspect(ctx, "nginx:1"); return err }},
		{"ContainerCreate", 201, `{"Id":"` + id64 + `"}`, func(c *Client) error {
			_, err := c.ContainerCreate(ctx, "sd-a-b-d1-a1", goodSpec(), okVerify)
			return err
		}},
		{"ContainerStart", 204, ``, func(c *Client) error { return c.ContainerStart(ctx, id64) }},
		{"ContainerRestart", 204, ``, func(c *Client) error { return c.ContainerRestart(ctx, id64, 0) }},
		{"ContainerRemove", 204, ``, func(c *Client) error { return c.ContainerRemove(ctx, id64) }},
		{"ContainerInspect", 200, `{"Id":"` + id64 + `","Name":"/x"}`, func(c *Client) error { _, err := c.ContainerInspect(ctx, id64); return err }},
		{"ContainerList", 200, `[]`, func(c *Client) error { _, err := c.ContainerList(ctx, ListOptions{All: true}); return err }},
		{"ContainerPorts", 200, `[]`, func(c *Client) error { _, err := c.ContainerPorts(ctx); return err }},
		{"NetworkInspect", 200, `{"Id":"n1","Name":"` + otherN + `"}`, func(c *Client) error { _, err := c.NetworkInspect(ctx, otherN); return err }},
		{"DataRoot", 200, `{"DockerRootDir":"/var/lib/docker"}`, func(c *Client) error { _, err := c.DataRoot(ctx); return err }},
		{"ImagePull", 200, `{"status":"Status: Image is up to date for nginx:1"}` + "\r\n", func(c *Client) error { return c.ImagePull(ctx, "nginx:1", nil) }},
		{"Events", 200, ``, func(c *Client) error { return c.Events(ctx, nil, func(Event) error { return nil }) }},
	}
}

func TestEachMethodSucceedsOnItsOKStatus(t *testing.T) {
	for _, o := range allOps() {
		t.Run(o.name, func(t *testing.T) {
			c, _ := startEngine(t, reply(o.ok, o.body))
			if err := o.run(c); err != nil {
				t.Fatal(err)
			}
		})
	}
}

func TestEachMethodMapsErrorStatuses(t *testing.T) {
	cases := []struct {
		status int
		want   error
	}{{404, ErrNotFound}, {409, ErrConflict}, {304, ErrNotModified}}
	for _, o := range allOps() {
		for _, k := range cases {
			t.Run(o.name+"/"+http.StatusText(k.status), func(t *testing.T) {
				c, _ := startEngine(t, reply(k.status, `{"message":"nope"}`))
				err := o.run(c)
				if !errors.Is(err, k.want) {
					t.Fatalf("got %v, want %v", err, k.want)
				}
				var ae *APIError
				if !errors.As(err, &ae) || ae.Status != k.status {
					t.Fatalf("not an APIError with status %d: %v", k.status, err)
				}
			})
		}
		t.Run(o.name+"/500", func(t *testing.T) {
			c, _ := startEngine(t, reply(500, "{\"message\":\"boom\\u001b[31m \\u202e done\"}"))
			err := o.run(c)
			var ae *APIError
			if !errors.As(err, &ae) || ae.Status != 500 {
				t.Fatalf("got %v", err)
			}
			if errors.Is(err, ErrNotFound) || errors.Is(err, ErrConflict) || errors.Is(err, ErrNotModified) {
				t.Fatalf("a 500 matched a sentinel: %v", err)
			}
			if strings.ContainsAny(ae.Msg, "\x1b‮") || !strings.Contains(ae.Msg, "boom") {
				t.Fatalf("message not cleaned: %q", ae.Msg)
			}
		})
	}
}

func TestEachMethodHonoursItsDeadlineOnAHangingHeader(t *testing.T) {
	for _, o := range allOps() {
		t.Run(o.name, func(t *testing.T) {
			release := make(chan struct{})
			t.Cleanup(func() { close(release) })
			c, _ := startEngine(t, func(w http.ResponseWriter, r *http.Request) {
				select {
				case <-release:
				case <-r.Context().Done():
				}
			})
			t0 := time.Now()
			err := o.run(c)
			if !errors.Is(err, context.DeadlineExceeded) {
				t.Fatalf("got %v", err)
			}
			if time.Since(t0) > 3*time.Second {
				t.Fatalf("took %v", time.Since(t0))
			}
		})
	}
}

func TestRequestShapes(t *testing.T) {
	ctx := context.Background()
	check := func(name string, o op, method, path, query string, body func(t *testing.T, b []byte)) {
		t.Run(name, func(t *testing.T) {
			c, e := startEngine(t, reply(o.ok, o.body))
			if err := o.run(c); err != nil {
				t.Fatal(err)
			}
			r := e.last()
			if r.Method != method || r.Path != path || r.Query != query {
				t.Fatalf("got %s %s ?%s", r.Method, r.Path, r.Query)
			}
			if body != nil {
				body(t, r.Body)
			} else if len(r.Body) != 0 {
				t.Fatalf("unexpected body %q", r.Body)
			}
		})
	}
	byName := map[string]op{}
	for _, o := range allOps() {
		byName[o.name] = o
	}
	check("start", byName["ContainerStart"], "POST", "/containers/"+id64+"/start", "", nil)
	check("restart", byName["ContainerRestart"], "POST", "/containers/"+id64+"/restart", "t=0", nil)
	check("remove never forces and drops anonymous volumes", byName["ContainerRemove"], "DELETE", "/containers/"+id64, "v=1&force=0", nil)
	check("pull", byName["ImagePull"], "POST", "/images/create", "fromImage=nginx%3A1", nil)
	check("image inspect", byName["ImageInspect"], "GET", "/images/nginx:1/json", "", nil)
	check("info", byName["DataRoot"], "GET", "/info", "", nil)
	check("create", byName["ContainerCreate"], "POST", "/containers/create", "name=sd-a-b-d1-a1", func(t *testing.T, b []byte) {
		want, _ := goodSpec().Marshal()
		if !bytes.Equal(b, want) {
			t.Fatalf("body %s", b)
		}
	})
	t.Run("list filters", func(t *testing.T) {
		c, e := startEngine(t, reply(200, `[]`))
		if _, err := c.ContainerList(ctx, ListOptions{All: true, Labels: []string{"serverdock.instance=abc"}}); err != nil {
			t.Fatal(err)
		}
		if q := e.last().Query; !strings.Contains(q, "all=1") || !strings.Contains(q, "filters=%7B%22label%22%3A%5B%22serverdock.instance%3Dabc%22%5D%7D") {
			t.Fatalf("query %s", q)
		}
	})
	t.Run("events filter", func(t *testing.T) {
		c, e := startEngine(t, reply(200, ``))
		if err := c.Events(ctx, []string{"serverdock.instance=abc"}, func(Event) error { return nil }); err != nil {
			t.Fatal(err)
		}
		r := e.last()
		if r.Path != "/events" || !strings.Contains(r.Query, "%22type%22%3A%5B%22container%22%5D") || !strings.Contains(r.Query, "serverdock.instance%3Dabc") {
			t.Fatalf("%+v", r)
		}
	})
}

func TestInputsAreValidatedBeforeAnyRequest(t *testing.T) {
	ctx := context.Background()
	c, e := startEngine(t, reply(200, `{}`))
	for _, bad := range []string{"", "a/../b", "a b", "a?x=1", "a#b", "a%2fb", "-x", "/x", "a//b", "a\nb", strings.Repeat("a", 400)} {
		if _, err := c.ImageInspect(ctx, bad); err == nil {
			t.Errorf("ImageInspect accepted %q", bad)
		}
		if err := c.ImagePull(ctx, bad, nil); err == nil {
			t.Errorf("ImagePull accepted %q", bad)
		}
	}
	for _, bad := range []string{"", "abc", id64[:12], strings.ToUpper(id64), id64 + "0", "../" + id64[3:], "my-container"} {
		if c.ContainerStart(ctx, bad) == nil || c.ContainerRestart(ctx, bad, 1) == nil || c.ContainerRemove(ctx, bad) == nil {
			t.Errorf("a mutating call accepted id %q", bad)
		}
	}
	if c.ContainerRestart(ctx, id64, -1) == nil || c.ContainerRestart(ctx, id64, 4000) == nil {
		t.Error("grace out of range accepted")
	}
	if _, err := c.ContainerInspect(ctx, "a/b"); err == nil {
		t.Error("inspect accepted a path")
	}
	for _, bad := range []string{"", "x y", "../x", "a/b"} {
		if _, err := c.ContainerCreate(ctx, bad, goodSpec(), okVerify); err == nil {
			t.Errorf("create accepted name %q", bad)
		}
		if _, err := c.NetworkInspect(ctx, bad); err == nil {
			t.Errorf("network inspect accepted %q", bad)
		}
	}
	if _, err := c.ContainerList(ctx, ListOptions{Labels: []string{"a\nb"}}); err == nil {
		t.Error("list accepted a newline in a filter")
	}
	if err := c.Events(ctx, []string{""}, nil); err == nil {
		t.Error("events accepted an empty filter")
	}
	if n := e.count(); n != 0 {
		t.Fatalf("%d requests reached the engine", n)
	}
}

func TestContainerCreateVerifiesTheExactBytesFirst(t *testing.T) {
	ctx := context.Background()
	c, e := startEngine(t, reply(201, `{"Id":"`+id64+`"}`))
	if _, err := c.ContainerCreate(ctx, "n", goodSpec(), nil); err == nil {
		t.Fatal("create without verify")
	}
	refuse := errors.New("policy says no")
	if _, err := c.ContainerCreate(ctx, "n", goodSpec(), func([]byte) error { return refuse }); !errors.Is(err, refuse) {
		t.Fatalf("got %v", err)
	}
	if e.count() != 0 {
		t.Fatal("a refused body was sent")
	}
	var seen []byte
	if _, err := c.ContainerCreate(ctx, "n", goodSpec(), func(b []byte) error { seen = append([]byte(nil), b...); return nil }); err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(seen, e.last().Body) {
		t.Fatalf("verify saw %s but Docker got %s", seen, e.last().Body)
	}
	// A reply with a bad id is refused.
	c2, _ := startEngine(t, reply(201, `{"Id":"../x"}`))
	if _, err := c2.ContainerCreate(ctx, "n", goodSpec(), okVerify); err == nil {
		t.Fatal("accepted a bad id")
	}
}

// The create body type holds only the fields the plan's policy allows: adding a
// field to it must come with a policy change, so this list is the review point.
func TestCreateBodyTypeHasOnlyAllowedFields(t *testing.T) {
	b, err := goodSpec().Marshal()
	if err != nil {
		t.Fatal(err)
	}
	var tree map[string]any
	if err := json.Unmarshal(b, &tree); err != nil {
		t.Fatal(err)
	}
	var paths []string
	var walk func(prefix string, v any)
	walk = func(prefix string, v any) {
		m, ok := v.(map[string]any)
		if !ok {
			paths = append(paths, prefix)
			return
		}
		for k, c := range m {
			// map-valued leaves keyed by data (labels, network names, ports)
			switch prefix + "/" + k {
			case "/Labels", "/ExposedPorts", "/HostConfig/LogConfig/Config", "/NetworkingConfig/EndpointsConfig":
				paths = append(paths, prefix+"/"+k)
				continue
			}
			walk(prefix+"/"+k, c)
		}
	}
	walk("", tree)
	want := map[string]bool{
		"/Image": true, "/Labels": true, "/ExposedPorts": true,
		"/HostConfig/RestartPolicy/Name": true, "/HostConfig/LogConfig/Type": true,
		"/HostConfig/LogConfig/Config": true, "/HostConfig/NetworkMode": true, "/HostConfig/PidsLimit": true,
		"/NetworkingConfig/EndpointsConfig": true,
	}
	got := map[string]bool{}
	for _, p := range paths {
		got[p] = true
		if !want[p] {
			t.Errorf("create body has the field %s, which the policy does not allow", p)
		}
	}
	for p := range want {
		if !got[p] {
			t.Errorf("expected field %s is missing from the marshaled spec", p)
		}
	}
	// The endpoint carries the one allowed key.
	ep := tree["NetworkingConfig"].(map[string]any)["EndpointsConfig"].(map[string]any)[otherN].(map[string]any)
	if len(ep) != 1 || len(ep["IPAMConfig"].(map[string]any)) != 1 {
		t.Errorf("endpoint config has extra keys: %v", ep)
	}
}

func TestContainerInspectAndListDecode(t *testing.T) {
	ctx := context.Background()
	c, _ := startEngine(t, func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/containers/json" {
			io.WriteString(w, `{"Id":"`+id64+`","Name":"/sd-a-b-d1-a1","Image":"sha256:`+id64+`","RestartCount":2,
			 "State":{"Status":"running","Running":true,"Restarting":false,"ExitCode":0,"OOMKilled":false,"StartedAt":"2026-11-21T10:00:00.5Z"},
			 "Config":{"Labels":{"serverdock.managed":"1"}},"HostConfig":{"NetworkMode":"`+otherN+`"},
			 "NetworkSettings":{"Networks":{"`+otherN+`":{"IPAddress":"10.250.0.2","IPAMConfig":{"IPv4Address":"10.250.0.2"}}}}}`)
			return
		}
		io.WriteString(w, `[{"Id":"`+id64+`","Names":["/web"],"ImageID":"sha256:x","State":"exited","Labels":{"a":"b"},
		 "Ports":[{"IP":"0.0.0.0","PrivatePort":80,"PublicPort":8080,"Type":"tcp"},{"PrivatePort":9,"Type":"tcp"}],
		 "NetworkSettings":{"Networks":{"`+otherN+`":{"IPAddress":"","IPAMConfig":{"IPv4Address":"10.250.0.9"}}}}}]`)
	})
	info, err := c.ContainerInspect(ctx, id64)
	if err != nil {
		t.Fatal(err)
	}
	if info.Name != "sd-a-b-d1-a1" || !info.State.Running || info.RestartCount != 2 || info.State.StartedAt.IsZero() ||
		info.Networks[otherN].ConfiguredIPv4 != "10.250.0.2" || info.Labels["serverdock.managed"] != "1" || info.NetworkMode != otherN {
		t.Fatalf("%+v", info)
	}
	list, err := c.ContainerList(ctx, ListOptions{All: true})
	if err != nil || len(list) != 1 {
		t.Fatalf("%v %v", list, err)
	}
	// A stopped container still shows the address it holds.
	if list[0].Networks[otherN].ConfiguredIPv4 != "10.250.0.9" || list[0].Networks[otherN].IPAddress != "" || list[0].Names[0] != "web" {
		t.Fatalf("%+v", list[0])
	}
	ports, err := c.ContainerPorts(ctx)
	if err != nil || len(ports) != 1 || ports[0].Name != "web" || len(ports[0].Ports) != 1 || ports[0].Ports[0].HostPort != 8080 {
		t.Fatalf("%+v %v", ports, err)
	}
	// An inspect with a bad id in the reply is refused.
	c2, _ := startEngine(t, reply(200, `{"Id":"x"}`))
	if _, err := c2.ContainerInspect(ctx, id64); err == nil {
		t.Fatal("accepted a bad id")
	}
}

func TestImageInspectNeverReturnsLabelValues(t *testing.T) {
	c, _ := startEngine(t, reply(200, `{"Id":"sha256:`+id64+`","RepoDigests":["a@sha256:1"],
	 "Config":{"ExposedPorts":{"8080/tcp":{},"80/tcp":{}},"Volumes":{"/data":{}},
	 "Labels":{"serverdock.managed":"secret-looking value","ServerDock.x":"y","other":"z"}}}`))
	info, err := c.ImageInspect(context.Background(), "x:1")
	if err != nil {
		t.Fatal(err)
	}
	if len(info.ReservedLabelKeys) != 2 || info.ReservedLabelKeys[0] != "ServerDock.x" || info.ReservedLabelKeys[1] != "serverdock.managed" {
		t.Fatalf("%v", info.ReservedLabelKeys)
	}
	if strings.Contains(strings.Join(info.ReservedLabelKeys, ","), "secret") {
		t.Fatal("a label value leaked")
	}
	if len(info.ExposedPorts) != 2 || info.ExposedPorts[0] != "80/tcp" || len(info.Volumes) != 1 {
		t.Fatalf("%+v", info)
	}
}

func TestPullProgress(t *testing.T) {
	lines := strings.Join([]string{
		`{"status":"Pulling from library/x","id":"1"}`,
		`{"status":"Pulling fs layer","id":"aaa","progressDetail":{}}`,
		`{"status":"Pulling fs layer","id":"bbb","progressDetail":{}}`,
		`{"status":"Downloading","id":"aaa","progressDetail":{"current":50,"total":100}}`,
		`{"status":"Downloading","id":"bbb","progressDetail":{"current":10,"total":400}}`,
		`{"status":"Pull complete","id":"aaa","progressDetail":{}}`,
		`{"status":"Digest: sha256:abc"}`,
		`{"status":"Status: Downloaded newer image for x:1"}`,
	}, "\r\n") + "\r\n"
	c, _ := startEngine(t, reply(200, lines))
	var last PullProgress
	var n int
	if err := c.ImagePull(context.Background(), "x:1", func(p PullProgress) { last = p; n++ }); err != nil {
		t.Fatal(err)
	}
	if n != 8 {
		t.Errorf("%d callbacks", n)
	}
	if last.Layers != 2 || last.LayersDone != 1 || last.Bytes != 110 || last.BytesTotal != 500 || !strings.HasPrefix(last.Status, "Status: Downloaded") {
		t.Fatalf("%+v", last)
	}
}

func TestPullErrorLines(t *testing.T) {
	ctx := context.Background()
	t.Run("errorDetail", func(t *testing.T) {
		c, _ := startEngine(t, reply(200, `{"status":"x"}`+"\n"+`{"errorDetail":{"message":"manifest unknown\u001b[2J"},"error":"manifest unknown"}`+"\n"))
		err := c.ImagePull(ctx, "x:1", nil)
		if err == nil || !strings.Contains(err.Error(), "manifest unknown") || strings.Contains(err.Error(), "\x1b") || errors.Is(err, ErrDiskFull) {
			t.Fatalf("%v", err)
		}
	})
	t.Run("ENOSPC in the stream", func(t *testing.T) {
		c, _ := startEngine(t, reply(200, `{"errorDetail":{"message":"write /var/lib/docker/tmp/x: no space left on device"},"error":"write /var/lib/docker/tmp/x: no space left on device"}`+"\n"))
		if err := c.ImagePull(ctx, "x:1", nil); !errors.Is(err, ErrDiskFull) {
			t.Fatalf("%v", err)
		}
	})
	t.Run("ENOSPC as an HTTP error", func(t *testing.T) {
		c, _ := startEngine(t, reply(500, `{"message":"mkdir /var/lib/docker/x: no space left on device"}`))
		if err := c.ImagePull(ctx, "x:1", nil); !errors.Is(err, ErrDiskFull) {
			t.Fatalf("%v", err)
		}
	})
	t.Run("bad line", func(t *testing.T) {
		c, _ := startEngine(t, reply(200, "not json\n"))
		if err := c.ImagePull(ctx, "x:1", nil); err == nil {
			t.Fatal("accepted garbage")
		}
	})
	t.Run("line over 64 KiB", func(t *testing.T) {
		c, _ := startEngine(t, reply(200, `{"status":"`+strings.Repeat("a", MaxJSONLine)+`"}`+"\n"))
		if err := c.ImagePull(ctx, "x:1", nil); !errors.Is(err, ErrLineTooLong) {
			t.Fatalf("%v", err)
		}
	})
	t.Run("a line of exactly the cap is fine", func(t *testing.T) {
		term := "Status: Downloaded newer image for "
		pad := MaxJSONLine - len(`{"status":""}`) - len(term)
		c, _ := startEngine(t, reply(200, `{"status":"`+term+strings.Repeat("a", pad)+`"}`+"\r\n"))
		if err := c.ImagePull(ctx, "x:1", nil); err != nil {
			t.Fatalf("%v", err)
		}
	})
}

func TestPullStatusAndLayerCountAreBounded(t *testing.T) {
	var b strings.Builder
	for i := 0; i < maxLayers+100; i++ {
		b.WriteString(`{"status":"Pulling fs layer","id":"l` + string(rune('a'+i%26)) + strings.Repeat("x", i%7) + string(rune('0'+i%10)) + `-` + itoa(i) + `"}` + "\n")
	}
	b.WriteString(`{"status":"` + strings.Repeat("s", 5000) + `"}` + "\n" + `{"status":"Status: Downloaded newer image for x:1"}` + "\n")
	c, _ := startEngine(t, reply(200, b.String()))
	var last PullProgress
	if err := c.ImagePull(context.Background(), "x:1", func(p PullProgress) { last = p }); err != nil {
		t.Fatal(err)
	}
	if last.Layers != maxLayers || len(last.Status) > maxErrMsg {
		t.Fatalf("layers %d status %d bytes", last.Layers, len(last.Status))
	}
}

func itoa(i int) string {
	b, _ := json.Marshal(i)
	return string(b)
}

func TestClosedStreams(t *testing.T) {
	ctx := context.Background()
	half := func(w http.ResponseWriter, _ *http.Request) { // a line without its newline, then EOF
		io.WriteString(w, `{"status":"Down`)
	}
	c, _ := startEngine(t, half)
	if err := c.ImagePull(ctx, "x:1", nil); !errors.Is(err, io.ErrUnexpectedEOF) {
		t.Fatalf("pull: %v", err)
	}
	if err := c.Events(ctx, nil, func(Event) error { return nil }); !errors.Is(err, io.ErrUnexpectedEOF) {
		t.Fatalf("events: %v", err)
	}
	// A server that drops the connection mid-stream.
	c2, _ := startEngine(t, func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Length", "1000")
		io.WriteString(w, `{"Action":"die"}`+"\n")
		w.(http.Flusher).Flush()
	})
	if err := c2.Events(ctx, nil, func(Event) error { return nil }); err == nil {
		t.Fatal("a truncated stream ended cleanly")
	}
}

func TestEventsDecodeFilterAndStop(t *testing.T) {
	ev := func(typ, action, id, extra string) string {
		return `{"Type":"` + typ + `","Action":"` + action + `","Time":1700000000,"Actor":{"ID":"` + id + `","Attributes":{"serverdock.service":"7","serverdock.deploy":"12",` + extra + `"name":"x"}}}` + "\n"
	}
	body := ev("container", "die", id64, `"exitCode":"137",`) + ev("network", "connect", id64, "") + ev("container", "start", "short", "") +
		"\n" + ev("container", "start", id64, "")
	c, _ := startEngine(t, reply(200, body))
	var got []Event
	if err := c.Events(context.Background(), nil, func(e Event) error { got = append(got, e); return nil }); err != nil {
		t.Fatal(err)
	}
	if len(got) != 2 || got[0] != (Event{Action: "die", ID: id64, Service: "7", Deploy: "12", ExitCode: "137", Time: 1700000000}) || got[1].Action != "start" {
		t.Fatalf("%+v", got)
	}
	stop := errors.New("stop")
	if err := c.Events(context.Background(), nil, func(Event) error { return stop }); !errors.Is(err, stop) {
		t.Fatalf("%v", err)
	}
}

func TestEventsLineOver64KiBIsAnError(t *testing.T) {
	big := `{"Type":"container","Action":"die","Actor":{"ID":"` + id64 + `","Attributes":{"x":"` + strings.Repeat("a", MaxJSONLine) + `"}}}` + "\n"
	c, _ := startEngine(t, reply(200, `{"Type":"container","Action":"start","Actor":{"ID":"`+id64+`"}}`+"\n"+big))
	var n int
	err := c.Events(context.Background(), nil, func(Event) error { n++; return nil })
	if !errors.Is(err, ErrLineTooLong) || n != 1 {
		t.Fatalf("n=%d err=%v", n, err)
	}
}

func TestEventsEndsWhenTheContextIsCancelled(t *testing.T) {
	c, _ := startEngine(t, func(w http.ResponseWriter, r *http.Request) {
		w.Write([]byte(`{"Type":"container","Action":"start","Actor":{"ID":"` + id64 + `"}}` + "\n"))
		w.(http.Flusher).Flush()
		<-r.Context().Done()
	})
	ctx, cancel := context.WithCancel(context.Background())
	got := make(chan struct{}, 1)
	done := make(chan error, 1)
	go func() {
		done <- c.Events(ctx, nil, func(Event) error { got <- struct{}{}; return nil })
	}()
	<-got
	time.Sleep(c.short + 100*time.Millisecond) // longer than the header deadline: an idle stream is fine
	cancel()
	select {
	case err := <-done:
		if !errors.Is(err, context.Canceled) {
			t.Fatalf("%v", err)
		}
	case <-time.After(3 * time.Second):
		t.Fatal("Events did not stop")
	}
}

func TestNetworkAddrs(t *testing.T) {
	for _, tc := range []struct{ subnet, gw, rng string }{
		{"10.250.0.0/24", "10.250.0.1", "10.250.0.192/26"},
		{"10.250.4.0/22", "10.250.4.1", "10.250.7.0/24"},
		{"10.250.0.0/20", "10.250.0.1", "10.250.12.0/22"},
		{"192.168.0.0/26", "192.168.0.1", "192.168.0.48/28"},
	} {
		gw, r, err := networkAddrs(tc.subnet)
		if err != nil || gw.String() != tc.gw || r.String() != tc.rng {
			t.Errorf("%s: %v %v %v", tc.subnet, gw, r, err)
		}
	}
	for _, bad := range []string{"", "10.250.0.5/24", "10.250.0.0/27", "10.0.0.0/15", "fd00::/64", "x", "10.250.0.0"} {
		if _, _, err := networkAddrs(bad); err == nil {
			t.Errorf("accepted %q", bad)
		}
	}
}

func netInspectJSON(name, subnet string, labels map[string]string) string {
	l, _ := json.Marshal(labels)
	return `{"Id":"nid","Name":"` + name + `","Driver":"bridge","Options":{"com.docker.network.bridge.enable_icc":"false"},"Labels":` + string(l) + `,"IPAM":{"Config":[{"Subnet":"` + subnet + `","Gateway":"10.250.0.1","IPRange":"10.250.0.192/26"}]},
	 "Containers":{"` + id64 + `":{"IPv4Address":"10.250.0.5/24"}}}`
}

func TestNetworkEnsure(t *testing.T) {
	ctx := context.Background()
	ours := map[string]string{"serverdock.instance": "abc"}
	spec := NetworkSpec{Name: otherN, Subnet: "10.250.0.0/24", Labels: ours}

	t.Run("creates with an explicit gateway and IPRange", func(t *testing.T) {
		created := false
		c, e := startEngine(t, func(w http.ResponseWriter, r *http.Request) {
			switch {
			case r.Method == "GET" && !created:
				http.NotFound(w, r)
			case r.Method == "POST":
				created = true
				w.WriteHeader(201)
				io.WriteString(w, `{"Id":"nid"}`)
			default:
				io.WriteString(w, netInspectJSON(otherN, "10.250.0.0/24", ours))
			}
		})
		info, err := c.NetworkEnsure(ctx, spec)
		if err != nil || info.Subnet != "10.250.0.0/24" || info.Used["10.250.0.5"] != id64 {
			t.Fatalf("%+v %v", info, err)
		}
		var post recorded
		for _, r := range e.reqs {
			if r.Method == "POST" {
				post = r
			}
		}
		var got createNetwork
		if err := json.Unmarshal(post.Body, &got); err != nil || post.Path != "/networks/create" {
			t.Fatalf("%v %+v", err, post)
		}
		cfg := got.IPAM.Config[0]
		if cfg.Subnet != "10.250.0.0/24" || cfg.Gateway != "10.250.0.1" || cfg.IPRange != "10.250.0.192/26" ||
			got.Driver != "bridge" || got.Options["com.docker.network.bridge.enable_icc"] != "false" || got.Labels["serverdock.instance"] != "abc" {
			t.Fatalf("%s", post.Body)
		}
	})
	t.Run("idempotent: an existing network is not recreated", func(t *testing.T) {
		c, e := startEngine(t, func(w http.ResponseWriter, r *http.Request) {
			io.WriteString(w, netInspectJSON(otherN, "10.250.0.0/24", map[string]string{"serverdock.instance": "abc", "extra": "1"}))
		})
		for i := 0; i < 2; i++ {
			if _, err := c.NetworkEnsure(ctx, spec); err != nil {
				t.Fatal(err)
			}
		}
		for _, r := range e.reqs {
			if r.Method != "GET" {
				t.Fatalf("unexpected %s", r.Method)
			}
		}
	})
	t.Run("refuses a same-name network without our label", func(t *testing.T) {
		c, e := startEngine(t, reply(200, netInspectJSON(otherN, "10.250.0.0/24", map[string]string{"other": "x"})))
		if _, err := c.NetworkEnsure(ctx, spec); !errors.Is(err, ErrForeignNetwork) {
			t.Fatalf("%v", err)
		}
		if e.last().Method != "GET" {
			t.Fatal("touched a foreign network")
		}
		c2, _ := startEngine(t, reply(200, netInspectJSON(otherN, "10.250.0.0/24", map[string]string{"serverdock.instance": "someone-else"})))
		if _, err := c2.NetworkEnsure(ctx, spec); !errors.Is(err, ErrForeignNetwork) {
			t.Fatalf("another install's network: %v", err)
		}
	})
	t.Run("refuses another subnet", func(t *testing.T) {
		c, _ := startEngine(t, reply(200, netInspectJSON(otherN, "10.251.0.0/24", ours)))
		if _, err := c.NetworkEnsure(ctx, spec); !errors.Is(err, ErrSubnetMismatch) {
			t.Fatalf("%v", err)
		}
	})
	t.Run("a create that loses a race re-inspects", func(t *testing.T) {
		var gets int
		c, _ := startEngine(t, func(w http.ResponseWriter, r *http.Request) {
			if r.Method == "POST" {
				w.WriteHeader(409)
				io.WriteString(w, `{"message":"exists"}`)
				return
			}
			gets++
			if gets == 1 {
				http.NotFound(w, r)
				return
			}
			io.WriteString(w, netInspectJSON(otherN, "10.250.0.0/24", ours))
		})
		if _, err := c.NetworkEnsure(ctx, spec); err != nil {
			t.Fatal(err)
		}
	})
	t.Run("inspect does not accept a prefix match", func(t *testing.T) {
		c, _ := startEngine(t, reply(200, netInspectJSON("other-name", "10.250.0.0/24", ours)))
		if _, err := c.NetworkInspect(ctx, otherN); !errors.Is(err, ErrNotFound) {
			t.Fatalf("%v", err)
		}
	})
	t.Run("bad specs", func(t *testing.T) {
		c, e := startEngine(t, reply(200, `{}`))
		for _, s := range []NetworkSpec{
			{Name: "bad name", Subnet: "10.250.0.0/24", Labels: ours},
			{Name: otherN, Subnet: "10.250.0.7/24", Labels: ours},
			{Name: otherN, Subnet: "10.250.0.0/24"},
		} {
			if _, err := c.NetworkEnsure(ctx, s); err == nil {
				t.Errorf("accepted %+v", s)
			}
		}
		if e.count() != 0 {
			t.Fatal("a request was sent")
		}
	})
}

func TestJSONLines(t *testing.T) {
	j := newJSONLines(strings.NewReader("a\r\n\r\n\nb\n"))
	for _, want := range []string{"a", "b"} {
		if l, err := j.next(); err != nil || string(l) != want {
			t.Fatalf("%q %v", l, err)
		}
	}
	if _, err := j.next(); !errors.Is(err, io.EOF) {
		t.Fatalf("%v", err)
	}
}

func FuzzPullLines(f *testing.F) {
	for _, s := range []string{
		`{"status":"Downloading","id":"a","progressDetail":{"current":1,"total":2}}`, `{"error":"x"}`, `{"errorDetail":{"message":"no space left on device"}}`,
		`{"status":"x","id":"a","progressDetail":{"current":-5,"total":9223372036854775807}}`, `[]`, `null`, ``, `{"id":1}`,
	} {
		f.Add([]byte(s + "\n"))
	}
	f.Fuzz(func(t *testing.T, b []byte) {
		c, _ := startEngine(t, func(w http.ResponseWriter, _ *http.Request) { w.Write(b) })
		_ = c.ImagePull(context.Background(), "x:1", func(p PullProgress) {
			if p.Layers > maxLayers || len(p.Status) > maxErrMsg || p.LayersDone > p.Layers {
				t.Fatalf("%+v", p)
			}
		})
	})
}

func FuzzEventLines(f *testing.F) {
	for _, s := range []string{
		`{"Type":"container","Action":"die","Actor":{"ID":"` + id64 + `","Attributes":{"exitCode":"1"}}}`, `{}`, `[]`, `{"Actor":1}`, ``,
	} {
		f.Add([]byte(s + "\n"))
	}
	f.Fuzz(func(t *testing.T, b []byte) {
		c, _ := startEngine(t, func(w http.ResponseWriter, _ *http.Request) { w.Write(b) })
		_ = c.Events(context.Background(), nil, func(e Event) error {
			if !ValidFullID(e.ID) || len(e.Action) > 64 {
				t.Fatalf("%+v", e)
			}
			return nil
		})
	})
}

func TestPullRefusesAReferenceThatPullsEveryTag(t *testing.T) {
	ctx := context.Background()
	c, e := startEngine(t, reply(200, `{"status":"Status: Image is up to date for x"}`+"\n"))
	for _, bad := range []string{"nginx", "a/b", "127.0.0.1:5000/app", "localhost:5000/a/b", "nginx:", "a/b:", "a@sha256:abc", "a@sha256:" + id64[:63], "a:1@sha256:" + id64 + "@sha256:" + id64, "a:b:c"} {
		if err := c.ImagePull(ctx, bad, nil); err == nil {
			t.Errorf("ImagePull accepted %q", bad)
		}
	}
	if e.count() != 0 {
		t.Fatalf("%d requests reached the engine", e.count())
	}
	for _, good := range []string{"nginx:1", "a/b:latest", "127.0.0.1:5000/app:1", "a/b@sha256:" + id64, "a/b:1@sha256:" + id64, "localhost:5000/a/b:2"} {
		if err := c.ImagePull(ctx, good, nil); err != nil {
			t.Errorf("ImagePull refused %q: %v", good, err)
		}
	}
	// Inspect still takes a bare name (it names one local image).
	if err := validRefOnly("nginx"); err != nil {
		t.Error(err)
	}
}

func validRefOnly(ref string) error {
	if !validRef(ref) {
		return errors.New("validRef refused " + ref)
	}
	return nil
}

func TestPullNeedsACompletionStatus(t *testing.T) {
	c, _ := startEngine(t, reply(200, `{"status":"Pulling from library/x","id":"1"}`+"\n"+`{"status":"Digest: sha256:`+id64+`"}`+"\n"))
	if err := c.ImagePull(context.Background(), "x:1", nil); err == nil {
		t.Fatal("a stream cut before its status was a success")
	}
	c2, _ := startEngine(t, reply(200, ``))
	if err := c2.ImagePull(context.Background(), "x:1", nil); err == nil {
		t.Fatal("an empty stream was a success")
	}
}

func TestPullBytesComeFromDownloadingLinesOnly(t *testing.T) {
	lines := strings.Join([]string{
		`{"status":"Pulling fs layer","id":"a","progressDetail":{"current":999,"total":999}}`,
		`{"status":"Downloading","id":"a","progressDetail":{"current":40,"total":100}}`,
		`{"status":"Extracting","id":"a","progressDetail":{"current":1,"total":5}}`,     // must not move the totals
		`{"status":"Downloading","id":"a","progressDetail":{"current":30,"total":100}}`, // never backwards
		`{"status":"Download complete","id":"a"}`,
		`{"status":"Extracting","id":"a","progressDetail":{"current":2,"total":50}}`,
		`{"status":"Pull complete","id":"a"}`,
		`{"status":"Status: Downloaded newer image for x:1"}`,
	}, "\n") + "\n"
	c, _ := startEngine(t, reply(200, lines))
	var seq []PullProgress
	if err := c.ImagePull(context.Background(), "x:1", func(p PullProgress) { seq = append(seq, p) }); err != nil {
		t.Fatal(err)
	}
	var prev int64
	for i, p := range seq {
		if p.Bytes < prev {
			t.Errorf("step %d: bytes went backwards: %d -> %d", i, prev, p.Bytes)
		}
		if p.Bytes > p.BytesTotal {
			t.Errorf("step %d: %d of %d", i, p.Bytes, p.BytesTotal)
		}
		prev = p.Bytes
	}
	if seq[0].BytesTotal != 0 {
		t.Errorf("a non-Downloading line set a total: %+v", seq[0])
	}
	if seq[2].BytesTotal != 100 || seq[2].Bytes != 40 {
		t.Errorf("Extracting moved the totals: %+v", seq[2])
	}
	last := seq[len(seq)-1]
	if last.Bytes != 100 || last.BytesTotal != 100 || last.LayersDone != 1 {
		t.Errorf("%+v", last)
	}
}

func TestEventsFilterNamesTheActions(t *testing.T) {
	c, e := startEngine(t, reply(200, ``))
	if err := c.Events(context.Background(), nil, func(Event) error { return nil }); err != nil {
		t.Fatal(err)
	}
	q, _ := url.ParseQuery(e.last().Query)
	var f map[string][]string
	if err := json.Unmarshal([]byte(q.Get("filters")), &f); err != nil {
		t.Fatal(err)
	}
	got := strings.Join(f["event"], ",")
	if got != "start,stop,die,oom,destroy,restart,kill" {
		t.Fatalf("event filter %q", got)
	}
	for _, a := range f["event"] {
		if strings.HasPrefix(a, "exec_") || strings.HasPrefix(a, "health") {
			t.Errorf("filter includes %s", a)
		}
	}
}

// Headers followed by a stalled error body do not hold the caller.
func TestStreamErrorBodyIsReadUnderTheShortDeadline(t *testing.T) {
	release := make(chan struct{})
	t.Cleanup(func() { close(release) })
	c, _ := startEngine(t, func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Length", "100")
		w.WriteHeader(500)
		w.(http.Flusher).Flush()
		select {
		case <-release:
		case <-r.Context().Done():
		}
	})
	t0 := time.Now()
	err := c.Events(context.Background(), nil, func(Event) error { return nil })
	var ae *APIError
	if !errors.As(err, &ae) || ae.Status != 500 || time.Since(t0) > 3*time.Second {
		t.Fatalf("%v after %v", err, time.Since(t0))
	}
}

func TestNetworkEnsureRefusesChangedSettings(t *testing.T) {
	ctx := context.Background()
	ours := map[string]string{"serverdock.instance": "abc"}
	spec := NetworkSpec{Name: otherN, Subnet: "10.250.0.0/24", Labels: ours}
	base := netInspectJSON(otherN, "10.250.0.0/24", ours)
	for name, mod := range map[string]func(string) string{
		"driver": func(s string) string { return strings.Replace(s, `"Driver":"bridge"`, `"Driver":"macvlan"`, 1) },
		"gateway": func(s string) string {
			return strings.Replace(s, `"Gateway":"10.250.0.1"`, `"Gateway":"10.250.0.2"`, 1)
		},
		"range": func(s string) string {
			return strings.Replace(s, `"IPRange":"10.250.0.192/26"`, `"IPRange":"10.250.0.128/25"`, 1)
		},
		"no range": func(s string) string { return strings.Replace(s, `,"IPRange":"10.250.0.192/26"`, ``, 1) },
		"icc":      func(s string) string { return strings.Replace(s, `enable_icc":"false"`, `enable_icc":"true"`, 1) },
		"no icc": func(s string) string {
			return strings.Replace(s, `"com.docker.network.bridge.enable_icc":"false"`, ``, 1)
		},
	} {
		body := mod(base)
		if body == base {
			t.Fatalf("%s: the mutation changed nothing", name)
		}
		c, e := startEngine(t, reply(200, body))
		if _, err := c.NetworkEnsure(ctx, spec); !errors.Is(err, ErrNetworkSettings) {
			t.Errorf("%s: %v", name, err)
		}
		if e.last().Method != "GET" {
			t.Errorf("%s: touched the network", name)
		}
	}
}

func TestNetworkCreateOverlapIsTyped(t *testing.T) {
	c, _ := startEngine(t, func(w http.ResponseWriter, r *http.Request) {
		if r.Method == "GET" {
			http.NotFound(w, r)
			return
		}
		w.WriteHeader(403)
		io.WriteString(w, `{"message":"Pool overlaps with other one on this address space"}`)
	})
	_, err := c.NetworkEnsure(context.Background(), NetworkSpec{Name: otherN, Subnet: "10.250.0.0/24", Labels: map[string]string{"serverdock.instance": "abc"}})
	if !errors.Is(err, ErrSubnetOverlap) || !strings.Contains(err.Error(), "overlaps") {
		t.Fatalf("%v", err)
	}
}

func TestImageInspectBoundsAndCleansItsLists(t *testing.T) {
	var ports, labels []string
	for i := 0; i < 100; i++ {
		ports = append(ports, `"`+itoa(1000+i)+`/tcp":{}`)
		labels = append(labels, `"serverdock.k`+itoa(i)+`":"v"`)
	}
	long := strings.Repeat("v", 1000)
	body := `{"Id":"sha256:` + id64 + `","RepoDigests":["a\u001b[2J@sha256:1","` + long + `"],
	 "Config":{"ExposedPorts":{` + strings.Join(ports, ",") + `},"Volumes":{"/` + long + `":{}},"Labels":{` + strings.Join(labels, ",") + `}}}`
	c, _ := startEngine(t, reply(200, body))
	info, err := c.ImageInspect(context.Background(), "x:1")
	if err != nil {
		t.Fatal(err)
	}
	if len(info.ExposedPorts) != maxImageList || len(info.ReservedLabelKeys) != maxImageList || !info.HasReservedLabel {
		t.Fatalf("%d ports, %d reserved, flag %v", len(info.ExposedPorts), len(info.ReservedLabelKeys), info.HasReservedLabel)
	}
	if strings.ContainsRune(info.RepoDigests[0], 0x1b) || len(info.RepoDigests[1]) > maxImageText || len(info.Volumes[0]) > maxImageText {
		t.Fatalf("not cleaned or capped: %q", info.RepoDigests)
	}
	for _, bad := range []string{"x", "sha256:abc", "sha256:" + strings.ToUpper(id64), id64} {
		c2, _ := startEngine(t, reply(200, `{"Id":"`+bad+`"}`))
		if _, err := c2.ImageInspect(context.Background(), "x:1"); err == nil {
			t.Errorf("accepted image id %q", bad)
		}
	}
	// No labels at all: the flag is false.
	c3, _ := startEngine(t, reply(200, `{"Id":"sha256:`+id64+`","Config":{"Labels":{"a":"b"}}}`))
	if i, _ := c3.ImageInspect(context.Background(), "x:1"); i.HasReservedLabel {
		t.Error("flag set without a reserved label")
	}
}

func TestContainerLabelsAreOnlyOurs(t *testing.T) {
	ctx := context.Background()
	c, _ := startEngine(t, func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/containers/json" {
			io.WriteString(w, `[{"Id":"short","Names":["/x"],"Labels":{"serverdock.service":"1"}},
			 {"Id":"`+id64+`","Names":["/y"],"State":"running","Labels":{"serverdock.service":"7\u001b[2J","com.example.secret":"s3cret","traefik.x":"y"}}]`)
			return
		}
		io.WriteString(w, `{"Id":"`+id64+`","Name":"/y","Config":{"Labels":{"serverdock.deploy":"1","com.example.secret":"s3cret"}}}`)
	})
	list, err := c.ContainerList(ctx, ListOptions{All: true})
	if err != nil || len(list) != 1 || list[0].ID != id64 {
		t.Fatalf("%+v %v", list, err)
	}
	// A value that CleanText would change is dropped, never rewritten.
	if _, ok := list[0].Labels["serverdock.service"]; ok || len(list[0].Labels) != 0 || !list[0].LabelsDropped {
		t.Fatalf("labels %q dropped=%v", list[0].Labels, list[0].LabelsDropped)
	}
	info, err := c.ContainerInspect(ctx, id64)
	if err != nil || len(info.Labels) != 1 || info.Labels["serverdock.deploy"] != "1" {
		t.Fatalf("%+v %v", info, err)
	}
}

func TestOurLabelsDropAndReport(t *testing.T) {
	long := map[string]string{}
	for i := 0; i < 17; i++ {
		long["serverdock.k"+itoa(i)] = "v"
	}
	cases := []struct {
		name    string
		in      map[string]string
		keep    int
		dropped bool
	}{
		{"clean", map[string]string{"serverdock.a": "1", "other": "x"}, 1, false},
		{"dirty value dropped", map[string]string{"serverdock.a": "1\u001b[2J", "serverdock.b": "2"}, 1, true},
		{"bidi value dropped", map[string]string{"serverdock.a": "a\u202eb"}, 0, true},
		{"over-long value dropped", map[string]string{"serverdock.a": strings.Repeat("x", 257)}, 0, true},
		{"max-length value kept", map[string]string{"serverdock.a": strings.Repeat("x", 256)}, 1, false},
		{"over-long key dropped", map[string]string{"serverdock." + strings.Repeat("k", 60): "1"}, 0, true},
		{"seventeen labels", long, 16, true},
		{"non-serverdock never counts", map[string]string{"a": "\u001b", "b": strings.Repeat("x", 999)}, 0, false},
	}
	for _, tc := range cases {
		got, dropped := ourLabels(tc.in)
		if len(got) != tc.keep || dropped != tc.dropped {
			t.Errorf("%s: kept %d dropped=%v, want %d %v", tc.name, len(got), dropped, tc.keep, tc.dropped)
		}
	}
	// The flag reaches inspect and list results.
	c, _ := startEngine(t, func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/containers/json" {
			io.WriteString(w, `[{"Id":"`+id64+`","Names":["/y"],"Labels":{"serverdock.service":"a\u001bb"}}]`)
			return
		}
		io.WriteString(w, `{"Id":"`+id64+`","Name":"/y","Config":{"Labels":{"serverdock.deploy":"a\u202eb"}}}`)
	})
	info, err := c.ContainerInspect(context.Background(), id64)
	if err != nil || !info.LabelsDropped || len(info.Labels) != 0 {
		t.Fatalf("%+v %v", info, err)
	}
	list, err := c.ContainerList(context.Background(), ListOptions{All: true})
	if err != nil || len(list) != 1 || !list[0].LabelsDropped {
		t.Fatalf("%+v %v", list, err)
	}
}
