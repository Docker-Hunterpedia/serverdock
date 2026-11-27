// Command drive is the bench's API client (plan 0004 step 15). It signs the bench server in
// (first-run setup), creates exposed services from a test image, triggers deploys and waits
// until they finish, so bench/run.sh can measure a server that has real apps. It is bench
// tooling: nothing in the product imports it.
//
//	drive -addr 127.0.0.1:18443 -state FILE setup -token-file FILE
//	drive -addr ... -state FILE expose -n 10 -image REF
//	drive -addr ... -state FILE deploy -services bench/app-1,bench/app-2
//	drive -addr x -state x freeports
//
// The session (cookie and CSRF token) is kept in the -state file (0600); nothing secret is
// printed. Only the standard library.
package main

import (
	"bytes"
	"crypto/hmac"
	"crypto/sha1"
	"crypto/tls"
	"encoding/base32"
	"encoding/binary"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"net"
	"net/http"
	"os"
	"strconv"
	"strings"
	"time"
)

const (
	username = "admin"
	password = "correct horse battery staple" // the bench server is throwaway
	project  = "bench"
)

type client struct {
	addr string
	http *http.Client
	st   state
}

// state is the saved session.
type state struct {
	Cookie string `json:"cookie"` // the Cookie header value
	CSRF   string `json:"csrf"`
}

func main() {
	if err := run(os.Args[1:]); err != nil {
		fmt.Fprintln(os.Stderr, "drive:", err)
		os.Exit(1)
	}
}

func run(args []string) error {
	fs := flag.NewFlagSet("drive", flag.ContinueOnError)
	addr := fs.String("addr", "", "the server's HTTPS address, host:port")
	stateFile := fs.String("state", "", "session file")
	if err := fs.Parse(args); err != nil {
		return err
	}
	if fs.NArg() < 1 || (fs.Arg(0) != "freeports" && (*addr == "" || *stateFile == "")) {
		return errors.New("usage: drive -addr HOST:PORT -state FILE setup|expose|deploy [flags]")
	}
	c := &client{addr: *addr, http: &http.Client{Timeout: 30 * time.Second,
		Transport: &http.Transport{TLSClientConfig: &tls.Config{InsecureSkipVerify: true}}}} // the panel's leaf is self-signed
	cmd, rest := fs.Arg(0), fs.Args()[1:]
	if cmd == "freeports" { // no server needed: prints two free TCP ports, one per line
		for i := 0; i < 2; i++ {
			p, err := freePort()
			if err != nil {
				return err
			}
			fmt.Println(p)
		}
		return nil
	}
	if cmd != "setup" {
		b, err := os.ReadFile(*stateFile)
		if err != nil {
			return fmt.Errorf("no session (run setup first): %w", err)
		}
		if err := json.Unmarshal(b, &c.st); err != nil {
			return err
		}
	}
	switch cmd {
	case "setup":
		sf := flag.NewFlagSet("setup", flag.ContinueOnError)
		tokenFile := sf.String("token-file", "", "file with the setup token")
		if err := sf.Parse(rest); err != nil {
			return err
		}
		return c.setup(*tokenFile, *stateFile)
	case "expose":
		sf := flag.NewFlagSet("expose", flag.ContinueOnError)
		n := sf.Int("n", 1, "services to create (1 to 100)")
		image := sf.String("image", "", "image reference of the app (listens on 8080)")
		timeout := sf.Duration("timeout", 5*time.Minute, "how long to wait for all of them")
		if err := sf.Parse(rest); err != nil {
			return err
		}
		return c.expose(*n, *image, *timeout)
	case "deploy":
		sf := flag.NewFlagSet("deploy", flag.ContinueOnError)
		svcs := sf.String("services", "", "comma-separated <project>/<service> names, deployed at once")
		timeout := sf.Duration("timeout", 3*time.Minute, "how long to wait for them to finish")
		if err := sf.Parse(rest); err != nil {
			return err
		}
		return c.deploy(strings.Split(*svcs, ","), *timeout)
	}
	return fmt.Errorf("unknown command %q", cmd)
}

// post sends a JSON POST. A non-2xx answer becomes an error carrying the API's code and
// message (never the request, which may hold credentials).
func (c *client) post(path string, in, out any) error {
	var body io.Reader = http.NoBody
	if in != nil {
		b, err := json.Marshal(in)
		if err != nil {
			return err
		}
		body = bytes.NewReader(b)
	}
	req, err := http.NewRequest("POST", "https://"+c.addr+path, body)
	if err != nil {
		return err
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Origin", "https://"+c.addr)
	if c.st.CSRF != "" {
		req.Header.Set("X-SD-CSRF", c.st.CSRF)
	}
	if c.st.Cookie != "" {
		req.Header.Set("Cookie", c.st.Cookie)
	}
	resp, err := c.http.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	b, _ := io.ReadAll(io.LimitReader(resp.Body, 4<<20))
	if resp.StatusCode/100 != 2 {
		var e struct {
			Error struct{ Code, Message string } `json:"error"`
		}
		_ = json.Unmarshal(b, &e)
		return fmt.Errorf("%s: %d %s: %s", path, resp.StatusCode, e.Error.Code, e.Error.Message)
	}
	// A new session cookie, if the server set one.
	var cookies []string
	for _, ck := range resp.Cookies() {
		cookies = append(cookies, ck.Name+"="+ck.Value)
	}
	if len(cookies) > 0 {
		c.st.Cookie = strings.Join(cookies, "; ")
	}
	if out == nil {
		return nil
	}
	// Action answers are {"data": ..., "audit": ..., "request_id": ...}; the auth and setup
	// routes answer the bare object.
	var env struct {
		Data json.RawMessage `json:"data"`
	}
	if strings.HasPrefix(path, "/api/v1/actions/") && json.Unmarshal(b, &env) == nil && env.Data != nil {
		b = env.Data
	}
	return json.Unmarshal(b, out)
}

func totp(secret string) (string, error) { return totpAt(secret, time.Now()) }

func totpAt(secret string, now time.Time) (string, error) {
	s := strings.ToUpper(strings.TrimRight(secret, "="))
	key, err := base32.StdEncoding.WithPadding(base32.NoPadding).DecodeString(s)
	if err != nil {
		return "", err
	}
	var msg [8]byte
	binary.BigEndian.PutUint64(msg[:], uint64(now.Unix()/30))
	h := hmac.New(sha1.New, key)
	h.Write(msg[:])
	sum := h.Sum(nil)
	o := sum[len(sum)-1] & 15
	return fmt.Sprintf("%06d", (binary.BigEndian.Uint32(sum[o:o+4])&0x7fffffff)%1000000), nil
}

// setup runs the first-run flow; setup/complete starts the session.
func (c *client) setup(tokenFile, stateFile string) error {
	tb, err := os.ReadFile(tokenFile)
	if err != nil {
		return err
	}
	token := strings.TrimSpace(string(tb))
	var en struct {
		Secret       string `json:"secret"`
		EnrollmentID string `json:"enrollment_id"`
	}
	if err := c.post("/api/v1/setup/enroll", map[string]string{"token": token, "username": username}, &en); err != nil {
		return err
	}
	code, err := totp(en.Secret)
	if err != nil {
		return err
	}
	var done struct {
		CSRF string `json:"csrf"`
	}
	if err := c.post("/api/v1/setup/complete", map[string]string{"token": token, "enrollment_id": en.EnrollmentID,
		"username": username, "password": password, "code": code}, &done); err != nil {
		return err
	}
	if done.CSRF == "" || c.st.Cookie == "" {
		return errors.New("setup/complete returned no session")
	}
	c.st.CSRF = done.CSRF
	b, _ := json.Marshal(c.st)
	if err := os.WriteFile(stateFile, b, 0o600); err != nil {
		return err
	}
	// A job refuses to run when the password or the authenticator changed in the same second
	// it was queued (auth.RefreshJobActor fails closed on a tie): let the second pass.
	time.Sleep(2 * time.Second)
	return nil
}

func freePort() (int, error) {
	ln, err := net.Listen("tcp", ":0")
	if err != nil {
		return 0, err
	}
	defer ln.Close()
	return ln.Addr().(*net.TCPAddr).Port, nil
}

type deployRef struct {
	Service string `json:"service"`
	Deploy  int    `json:"deploy"`
}

// expose creates n services (bench/app-1 ...) of image on free public ports, queues each
// first deploy, and waits until every deploy succeeded and every public port answers 2xx.
func (c *client) expose(n int, image string, timeout time.Duration) error {
	if n < 1 || n > 100 || image == "" {
		return errors.New("expose: -n 1..100 and -image are required")
	}
	t0 := time.Now()
	type svc struct {
		name string
		port int
		dep  int
	}
	var all []svc
	used := map[int]bool{}
	for i := 1; i <= n; i++ {
		var port int
		for port == 0 || used[port] || port < 1024 {
			p, err := freePort()
			if err != nil {
				return err
			}
			port = p
		}
		used[port] = true
		name := "app-" + strconv.Itoa(i)
		var out struct {
			Deploy *deployRef `json:"deploy"`
		}
		err := c.post("/api/v1/actions/services.create", map[string]any{"project": project, "name": name, "image_ref": image,
			"container_port": 8080, "public_port": port, "deploy": true, "health": map[string]any{"kind": "http", "path": "/", "passes": 1}}, &out)
		if err != nil {
			return err
		}
		if out.Deploy == nil {
			return fmt.Errorf("services.create %s queued no deploy", name)
		}
		all = append(all, svc{project + "/" + name, port, out.Deploy.Deploy})
	}
	for _, s := range all {
		if err := c.waitDeploy(s.name, s.dep, t0.Add(timeout)); err != nil {
			return err
		}
	}
	web := &http.Client{Timeout: 5 * time.Second}
	for _, s := range all {
		for {
			resp, err := web.Get("http://127.0.0.1:" + strconv.Itoa(s.port) + "/")
			if err == nil {
				_, _ = io.Copy(io.Discard, resp.Body)
				resp.Body.Close()
				if resp.StatusCode/100 == 2 {
					break
				}
			}
			if time.Now().After(t0.Add(timeout)) {
				return fmt.Errorf("%s: port %d does not answer 2xx", s.name, s.port)
			}
			time.Sleep(500 * time.Millisecond)
		}
	}
	fmt.Printf("exposed %d services, every public port answers 2xx, after %.1f s\n", n, time.Since(t0).Seconds())
	return nil
}

// waitDeploy polls deploys.get until the deploy is finished; only "succeeded" is success.
func (c *client) waitDeploy(service string, number int, deadline time.Time) error {
	for {
		var out struct {
			Deploy struct {
				Status string `json:"status"`
				Stage  string `json:"stage"`
				Reason string `json:"reason"`
			} `json:"deploy"`
		}
		if err := c.post("/api/v1/actions/deploys.get", map[string]any{"service": service, "number": number}, &out); err != nil {
			return err
		}
		switch out.Deploy.Status {
		case "succeeded":
			return nil
		case "failed", "cancelled", "superseded":
			return fmt.Errorf("%s deploy %d: %s (%s)", service, number, out.Deploy.Status, out.Deploy.Reason)
		}
		if time.Now().After(deadline) {
			return fmt.Errorf("%s deploy %d: still %s/%s at the deadline", service, number, out.Deploy.Status, out.Deploy.Stage)
		}
		time.Sleep(500 * time.Millisecond)
	}
}

// deploy queues one deploy per service at once and waits until all have succeeded.
func (c *client) deploy(services []string, timeout time.Duration) error {
	if len(services) == 0 || services[0] == "" {
		return errors.New("deploy: -services is required")
	}
	t0 := time.Now()
	refs := make([]deployRef, 0, len(services))
	for _, s := range services {
		var out deployRef
		if err := c.post("/api/v1/actions/services.deploy", map[string]any{"service": s}, &out); err != nil {
			return err
		}
		refs = append(refs, out)
	}
	for _, r := range refs {
		if err := c.waitDeploy(r.Service, r.Deploy, t0.Add(timeout)); err != nil {
			return err
		}
	}
	fmt.Printf("deployed %d service(s) at once, all succeeded after %.1f s\n", len(refs), time.Since(t0).Seconds())
	return nil
}
