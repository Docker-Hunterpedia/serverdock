package main

import (
	"bufio"
	"context"
	"fmt"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"testing"
	"time"

	"github.com/Docker-Hunterpedia/serverdock/internal/agent/proxy"
	"github.com/Docker-Hunterpedia/serverdock/internal/api"
	"github.com/Docker-Hunterpedia/serverdock/internal/limits"
	"github.com/Docker-Hunterpedia/serverdock/internal/pki"
	"github.com/Docker-Hunterpedia/serverdock/internal/testbin"
)

func TestControlSocketModes(t *testing.T) {
	sock := shortSock(t)
	addrs, stop := startServer(t, serverConfig{Data: newData(t), ControlSocket: sock})
	if addrs.Control != sock {
		t.Fatalf("Control = %q", addrs.Control)
	}
	if st, err := os.Lstat(sock); err != nil || st.Mode().Perm() != 0o600 || st.Mode()&os.ModeSocket == 0 {
		t.Fatalf("socket %v %v", st, err)
	}
	if st, err := os.Stat(filepath.Dir(sock)); err != nil || st.Mode().Perm() != 0o700 {
		t.Fatalf("socket dir %v %v", st, err)
	}
	// A request without the proxy's single X-Forwarded-For is refused with 400.
	resp, err := unixClient(sock).Get("http://cp/healthz")
	if err != nil || resp.StatusCode != http.StatusBadRequest {
		t.Fatalf("no XFF: %v %v", resp, err)
	}
	resp.Body.Close()
	if err := stop(); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Lstat(sock); err == nil {
		t.Error("socket file left behind after shutdown")
	}
}

func TestListenPrivateSocketRules(t *testing.T) {
	base := sockDir(t)

	// A group/world accessible directory is refused (and not tightened).
	open := filepath.Join(base, "open")
	os.Mkdir(open, 0o755)
	os.Chmod(open, 0o755)
	if _, err := listenPrivateSocket(filepath.Join(open, "c.sock")); err == nil || !strings.Contains(err.Error(), "chmod 700") {
		t.Errorf("0755 directory accepted: %v", err)
	}

	// A directory owned by someone else is refused (portable: pretend to be another uid).
	own := filepath.Join(base, "own")
	os.Mkdir(own, 0o700)
	if _, err := listenPrivate(filepath.Join(own, "c.sock"), os.Geteuid()+1); err == nil || !strings.Contains(err.Error(), "owned by uid") {
		t.Errorf("foreign-owned directory accepted: %v", err)
	}

	// A path over the OS limit is refused with a clear message, before anything is created.
	long := filepath.Join(base, strings.Repeat("d", maxSocketPath()), "c.sock")
	if _, err := listenPrivateSocket(long); err == nil || !strings.Contains(err.Error(), "too long") {
		t.Errorf("long path: %v", err)
	}
	if _, err := os.Stat(filepath.Join(base, strings.Repeat("d", maxSocketPath()))); err == nil {
		t.Error("a directory was created for a path that is too long")
	}
	if _, err := listenPrivateSocket("relative.sock"); err == nil {
		t.Error("relative path accepted")
	}

	// A regular file at the socket path is not replaced; a stale socket is.
	priv := filepath.Join(base, "priv")
	os.Mkdir(priv, 0o700)
	os.WriteFile(filepath.Join(priv, "c.sock"), []byte("x"), 0o600)
	if _, err := listenPrivateSocket(filepath.Join(priv, "c.sock")); err == nil {
		t.Error("a regular file was replaced")
	}
	os.Remove(filepath.Join(priv, "c.sock"))
	stale, _ := net.Listen("unix", filepath.Join(priv, "c.sock"))
	stale.(*net.UnixListener).SetUnlinkOnClose(false)
	stale.Close()
	ln, err := listenPrivateSocket(filepath.Join(priv, "c.sock"))
	if err != nil {
		t.Fatalf("stale socket: %v", err)
	}
	ln.Close()

	// A missing directory is created 0700 even under a loose umask.
	defer syscall.Umask(syscall.Umask(0))
	nested := filepath.Join(base, "a", "b", "c.sock")
	ln, err = listenPrivateSocket(nested)
	if err != nil {
		t.Fatal(err)
	}
	ln.Close()
	for _, d := range []string{"a", filepath.Join("a", "b")} {
		if st, err := os.Stat(filepath.Join(base, d)); err != nil || st.Mode().Perm() != 0o700 {
			t.Errorf("%s: %v %v", d, st, err)
		}
	}
}

func TestControlAndDebugPathsMustDiffer(t *testing.T) {
	sock := filepath.Join(sockDir(t), "x.sock")
	err := runServer(context.Background(), serverConfig{Data: newData(t), ControlSocket: sock, DebugSocket: sock}, quietLog(), nil)
	if err == nil || !strings.Contains(err.Error(), "different paths") {
		t.Fatalf("%v", err)
	}
	if _, err := os.Lstat(sock); err == nil {
		t.Error("a socket was created before the check")
	}
}

// The 257th connection waits until a slot is free (the cap is 256, kept from M1).
func TestControlSocketConnectionCap(t *testing.T) {
	addrs, stop := startServer(t, serverConfig{Data: newData(t), ControlSocket: shortSock(t)})
	defer stop()
	req := "GET /healthz HTTP/1.1\r\nHost: cp\r\nX-Forwarded-For: 127.0.0.1\r\n\r\n"
	roundTrip := func(c net.Conn, d time.Duration) (string, error) {
		c.SetDeadline(time.Now().Add(d))
		if _, err := c.Write([]byte(req)); err != nil {
			return "", err
		}
		return bufio.NewReader(c).ReadString('\n')
	}
	var conns []net.Conn
	defer func() {
		for _, c := range conns {
			c.Close()
		}
	}()
	for i := 0; i < controlMaxConns; i++ {
		c, err := net.Dial("unix", addrs.Control)
		if err != nil {
			t.Fatal(err)
		}
		conns = append(conns, c)
		// A full request and response proves the server accepted this connection.
		if line, err := roundTrip(c, 5*time.Second); err != nil || !strings.HasPrefix(line, "HTTP/1.1 200") {
			t.Fatalf("connection %d: %q %v", i, line, err)
		}
	}
	over, err := net.Dial("unix", addrs.Control)
	if err != nil {
		t.Fatal(err)
	}
	defer over.Close()
	if line, err := roundTrip(over, 400*time.Millisecond); err == nil {
		t.Fatalf("connection %d was served while %d are open: %q", controlMaxConns+1, controlMaxConns, line)
	}
	conns[0].Close() // frees a slot; the waiting connection is accepted and answered
	over.SetDeadline(time.Now().Add(5 * time.Second))
	if line, err := bufio.NewReader(over).ReadString('\n'); err != nil || !strings.HasPrefix(line, "HTTP/1.1 200") {
		t.Fatalf("waiting connection not served after a slot was freed: %q %v", line, err)
	}
}

// Real Caddy in front of the socket: a client-supplied X-Forwarded-For never
// reaches the handler. It sees exactly one value, the real peer.
func TestForgedXFFOnSocketIgnoredByCaddy(t *testing.T) { forgedXFF(t, false) }

// The same through the production path: the HTTPS panel config (TLS on :443,
// proxied to the socket), not the plain-HTTP one.
func TestForgedXFFOnSocketIgnoredByCaddyOverHTTPS(t *testing.T) { forgedXFF(t, true) }

func forgedXFF(t *testing.T, https bool) {
	sock := shortSock(t)
	ln, err := listenPrivateSocket(sock)
	if err != nil {
		t.Fatal(err)
	}
	var mu sync.Mutex
	var seen []string
	var key string
	h := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		seen, key = r.Header.Values("X-Forwarded-For"), limits.ClientIP(r)
		mu.Unlock()
		fmt.Fprint(w, "ok")
	})
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- api.Serve(ctx, ln, h, time.Second, api.WithTrustedProxy(nil)) }()
	defer func() { cancel(); <-done }()

	listen := testbin.FreeAddr(t)
	cfg := proxy.Config{DataDir: newData(t), Listen: listen, Upstream: proxy.UnixUpstream(sock)}
	scheme, client := "http", http.DefaultClient
	if https {
		pkiDir := filepath.Join(newData(t), "pki")
		leaf, err := pki.EnsurePanelLeaf(pki.PanelOptions{Dir: pkiDir})
		if err != nil {
			t.Fatal(err)
		}
		httpsAddr := testbin.FreeAddr(t)
		cfg.Panel = &proxy.Panel{Listen: httpsAddr, CertFile: leaf.CertFile}
		scheme, listen, client = "https", httpsAddr, insecureClient()
		defer client.CloseIdleConnections()
	}
	if err := proxy.Load(cfg); err != nil {
		t.Fatal(err)
	}
	defer proxy.Stop()

	forged := map[string][]string{
		"one value":      {"6.6.6.6"},
		"two header":     {"6.6.6.6", "7.7.7.7"},
		"list":           {"6.6.6.6, 7.7.7.7"},
		"loopback fake":  {"127.0.0.1"},
		"junk":           {"not-an-ip"},
		"empty":          {""},
		"v6":             {"2001:db8::6"},
		"with real hops": {"10.0.0.1, 10.0.0.2, 6.6.6.6"},
	}
	for name, vals := range forged {
		var resp *http.Response
		for i := 0; i < 50; i++ { // Caddy may need a moment after Load
			req, _ := http.NewRequest("GET", scheme+"://"+listen+"/", nil)
			for _, v := range vals {
				req.Header.Add("X-Forwarded-For", v)
			}
			if resp, err = client.Do(req); err == nil {
				break
			}
			time.Sleep(100 * time.Millisecond)
		}
		if err != nil {
			t.Fatalf("%s: %v", name, err)
		}
		resp.Body.Close()
		mu.Lock()
		got, k := seen, key
		mu.Unlock()
		if resp.StatusCode != 200 || len(got) != 1 || got[0] != "127.0.0.1" || k != "127.0.0.1" {
			t.Errorf("%s: status %d, handler saw %q, key %q; want one value 127.0.0.1", name, resp.StatusCode, got, k)
		}
	}
}

// The control plane has no TCP listener: the flag is gone, and (on Linux, where
// /proc shows it) the running process listens on no TCP port except Caddy's
// public one.
func TestNoTCPControlListener(t *testing.T) {
	if testing.Short() {
		t.Skip("builds the binary")
	}
	if res := testbin.Run(t, testbin.Options{Args: []string{"server", "--internal", "127.0.0.1:7070"}}); res.Code != 2 || !strings.Contains(res.Stderr, "not defined") {
		t.Fatalf("--internal still exists: exit %d\n%s", res.Code, res.Stderr)
	}

	data := filepath.Join(testbin.ShortDir(t), "data")
	p := testbin.Start(t, testbin.Options{Args: []string{"server", "--data", data, "--proxy=false"}})
	if err := p.WaitLog("control plane listening", 30*time.Second); err != nil {
		t.Fatal(err)
	}
	// The default socket is DATA/run/cp.sock, 0600 in a 0700 directory.
	sock := filepath.Join(data, "run", "cp.sock")
	if st, err := os.Lstat(sock); err != nil || st.Mode().Perm() != 0o600 || st.Mode()&os.ModeSocket == 0 {
		t.Fatalf("default socket: %v %v", st, err)
	}
	if st, err := os.Stat(filepath.Dir(sock)); err != nil || st.Mode().Perm() != 0o700 {
		t.Fatalf("socket dir: %v %v", st, err)
	}
	resp, err := cpGet(sock, "/healthz")
	if err != nil || resp.StatusCode != 200 {
		t.Fatalf("healthz on the default socket: %v %v", resp, err)
	}
	resp.Body.Close()
	ports, ok := listeningTCPPorts(p.Cmd.Process.Pid)
	if !ok {
		t.Log("no /proc: skipping the listening-socket scan (the Linux root run covers it)")
	} else if len(ports) != 0 {
		t.Errorf("--proxy=false server listens on TCP ports %v", ports)
	}
	p.Stop()

	// With the proxy: only Caddy's two public listeners (the :80 redirect and the panel).
	listen, httpsListen := testbin.FreeAddr(t), testbin.FreeAddr(t)
	p2 := testbin.Start(t, testbin.Options{Args: []string{"server", "--data", data, "--listen", listen, "--https-listen", httpsListen}})
	if err := p2.WaitLog("control plane listening", 30*time.Second); err != nil {
		t.Fatal(err)
	}
	if err := p2.WaitLog("proxy listening", 30*time.Second); err != nil {
		t.Fatal(err)
	}
	if ports, ok := listeningTCPPorts(p2.Cmd.Process.Pid); ok {
		_, p80, _ := net.SplitHostPort(listen)
		_, p443, _ := net.SplitHostPort(httpsListen)
		got := map[int]bool{}
		for _, p := range ports {
			got[p] = true
		}
		if len(got) != 2 || !got[atoiOr0(p80)] || !got[atoiOr0(p443)] {
			t.Errorf("listening TCP ports %v, want only the proxy's %s and %s", ports, p80, p443)
		}
	}
	p2.Stop()
}

func atoiOr0(s string) int { n, _ := strconv.Atoi(s); return n }

// listeningTCPPorts returns the TCP ports the process listens on, read from
// /proc (Linux). ok is false where /proc is not available.
func listeningTCPPorts(pid int) (ports []int, ok bool) {
	fds, err := os.ReadDir(fmt.Sprintf("/proc/%d/fd", pid))
	if err != nil {
		return nil, false
	}
	inodes := map[string]bool{}
	for _, fd := range fds {
		if l, err := os.Readlink(fmt.Sprintf("/proc/%d/fd/%s", pid, fd.Name())); err == nil && strings.HasPrefix(l, "socket:[") {
			inodes[strings.TrimSuffix(strings.TrimPrefix(l, "socket:["), "]")] = true
		}
	}
	for _, f := range []string{"tcp", "tcp6"} {
		b, err := os.ReadFile(fmt.Sprintf("/proc/%d/net/%s", pid, f))
		if err != nil {
			return nil, false
		}
		for _, line := range strings.Split(string(b), "\n")[1:] {
			c := strings.Fields(line)
			if len(c) < 10 || c[3] != "0A" || !inodes[c[9]] { // 0A = LISTEN
				continue
			}
			if i := strings.LastIndex(c[1], ":"); i >= 0 {
				if n, err := strconv.ParseInt(c[1][i+1:], 16, 32); err == nil {
					ports = append(ports, int(n))
				}
			}
		}
	}
	return ports, true
}

// Two servers with different DATA but the same --control-socket: the second must
// not take the path from the first (it is a live socket), and the first keeps
// answering.
func TestSecondServerCannotStealALiveControlSocket(t *testing.T) {
	sock := shortSock(t)
	addrs, stop := startServer(t, serverConfig{Data: newData(t), ControlSocket: sock})
	before, _ := os.Lstat(sock)
	err := runServer(context.Background(), serverConfig{Data: newData(t), ControlSocket: sock}, quietLog(), nil)
	if err == nil || !strings.Contains(err.Error(), "in use by another process") {
		t.Fatalf("second server: %v", err)
	}
	if after, err := os.Lstat(sock); err != nil || !os.SameFile(before, after) {
		t.Fatalf("the first server's socket changed: %v", err)
	}
	resp, err := cpGet(addrs.Control, "/healthz")
	if err != nil || resp.StatusCode != 200 {
		t.Fatalf("first server stopped answering: %v %v", resp, err)
	}
	resp.Body.Close()
	if err := stop(); err != nil {
		t.Fatal(err)
	}
}

// A listener closes (and unlinks) only its own socket: if the path was taken
// over meanwhile, the newcomer's socket stays.
func TestListenerClosesOnlyItsOwnSocket(t *testing.T) {
	sock := filepath.Join(sockDir(t), "d", "c.sock")
	ln, err := listenPrivateSocket(sock)
	if err != nil {
		t.Fatal(err)
	}
	os.Remove(sock) // something else takes over the path
	other, err := net.Listen("unix", sock)
	if err != nil {
		t.Fatal(err)
	}
	defer other.Close()
	ln.Close()
	if st, err := os.Lstat(sock); err != nil || st.Mode()&os.ModeSocket == 0 {
		t.Fatalf("the other socket was removed: %v", err)
	}
	// Untouched path: removed on Close.
	sock2 := filepath.Join(sockDir(t), "d", "c.sock")
	ln2, _ := listenPrivateSocket(sock2)
	ln2.Close()
	if _, err := os.Lstat(sock2); err == nil {
		t.Fatal("own socket left behind")
	}
}
