// Package testbin builds the real serverdock binary once per test process and
// runs it as a subprocess. Behaviour that is process-wide (the umask, file
// modes, exit codes, the data-directory lock, running as another uid) cannot be
// tested through runServer in-process, so those tests use this harness.
//
// It is for tests only: nothing outside *_test.go files imports it.
//
// A package that uses it must call Init and Cleanup from TestMain (Bin fails
// otherwise), so no sdbin* directory is ever left in the temp directory.
//
// Options.Uid (running as another user) works on Linux only, and needs root.
// ShortDir creates a directory owned by the test's user with mode 0700: when the
// test runs as root and the binary as another uid, that uid cannot enter it, so
// give such a server a socket path inside its own (chowned) DATA instead.
package testbin

import (
	"bytes"
	"errors"
	"fmt"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"syscall"
	"testing"
	"time"
)

var (
	initialised bool
	buildOnce   sync.Once
	buildDir    string
	buildErr    error
)

// Bin returns the path of the built serverdock binary, building it on first
// use (CGO off, so it behaves like the shipped one). The build directory is
// world-searchable so tests can run the binary as another uid.
func Bin(t testing.TB) string {
	t.Helper()
	if !initialised {
		t.Fatal("testbin: call testbin.Init() and defer testbin.Cleanup() in this package's TestMain")
	}
	buildOnce.Do(func() {
		var root string
		if root, buildErr = moduleRoot(); buildErr != nil {
			return
		}
		if buildDir, buildErr = os.MkdirTemp("", "sdbin"); buildErr != nil {
			return
		}
		if buildErr = os.Chmod(buildDir, 0o755); buildErr != nil {
			return
		}
		// -tags testhooks compiles the deploy runner's pause points (SERVERDOCK_TESTHOOK_*)
		// into this binary and no other: a shipped build has none (plan 0004 step 8a).
		cmd := exec.Command("go", "build", "-trimpath", "-tags", "testhooks", "-o", filepath.Join(buildDir, "serverdock"), "./cmd/serverdock")
		cmd.Dir = root
		cmd.Env = append(os.Environ(), "CGO_ENABLED=0")
		if out, err := cmd.CombinedOutput(); err != nil {
			buildErr = fmt.Errorf("go build ./cmd/serverdock: %v\n%s", err, out)
		}
	})
	if buildErr != nil {
		t.Fatalf("testbin: %v", buildErr)
	}
	return filepath.Join(buildDir, "serverdock")
}

// Init records that the package's TestMain will call Cleanup. Call it first in TestMain.
func Init() { initialised = true }

// Cleanup removes the built binary. Call it from TestMain after m.Run.
func Cleanup() {
	if buildDir != "" {
		os.RemoveAll(buildDir)
	}
}

func moduleRoot() (string, error) {
	dir, err := os.Getwd()
	if err != nil {
		return "", err
	}
	for {
		if _, err := os.Stat(filepath.Join(dir, "go.mod")); err == nil {
			return dir, nil
		}
		parent := filepath.Dir(dir)
		if parent == dir {
			return "", errors.New("go.mod not found above the test's directory")
		}
		dir = parent
	}
}

// Options describes one run of the binary.
type Options struct {
	// Args are the command line after the binary name, for example
	// {"server", "--data", dir, ...}.
	Args []string
	// Uid, if non-zero, runs the binary as that uid and gid (needs root).
	Uid int
	// Env is added to the environment (KEY=value).
	Env []string
}

func (o Options) command(t testing.TB) *exec.Cmd {
	t.Helper()
	args := o.Args
	if len(args) > 0 && args[0] == "server" && !hasFlag(args, "--docker-socket") {
		// The server creates the app network in Docker at start (network_ensure):
		// a test binary must never do that on the machine's real Docker, so it gets
		// a socket that does not exist unless the test names one.
		args = append(append([]string(nil), args...), "--docker-socket", "/nonexistent/serverdock-test-docker.sock")
	}
	cmd := exec.Command(Bin(t), args...)
	cmd.Env = append(os.Environ(), o.Env...)
	if o.Uid != 0 {
		if os.Geteuid() != 0 {
			t.Fatal("testbin: Uid needs root")
		}
		cmd.SysProcAttr = &syscall.SysProcAttr{Credential: &syscall.Credential{Uid: uint32(o.Uid), Gid: uint32(o.Uid)}}
	}
	return cmd
}

// Result is a finished run.
type Result struct {
	Stdout, Stderr string
	Code           int
}

// Run runs the binary to completion (60 s limit).
func Run(t testing.TB, o Options) Result {
	t.Helper()
	cmd := o.command(t)
	var out, errb bytes.Buffer
	cmd.Stdout, cmd.Stderr = &out, &errb
	if err := cmd.Start(); err != nil {
		t.Fatalf("testbin: %v", err)
	}
	done := make(chan error, 1)
	go func() { done <- cmd.Wait() }()
	select {
	case err := <-done:
		res := Result{Stdout: out.String(), Stderr: errb.String()}
		var ee *exec.ExitError
		if errors.As(err, &ee) {
			res.Code = ee.ExitCode()
		} else if err != nil {
			t.Fatalf("testbin: %v", err)
		}
		return res
	case <-time.After(60 * time.Second):
		_ = cmd.Process.Kill()
		<-done
		t.Fatalf("testbin: %v timed out\n%s%s", o.Args, out.String(), errb.String())
	}
	return Result{}
}

// Proc is a running binary.
type Proc struct {
	Cmd  *exec.Cmd
	log  *syncBuf
	done chan struct{}
	err  error
	t    testing.TB
}

type syncBuf struct {
	mu sync.Mutex
	b  bytes.Buffer
}

func (s *syncBuf) Write(p []byte) (int, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.b.Write(p)
}

func (s *syncBuf) String() string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.b.String()
}

// Start starts the binary in the background; stdout and stderr are both
// collected in Logs. The process is stopped (SIGTERM, then kill) when the test
// ends if the test has not stopped it itself.
func Start(t testing.TB, o Options) *Proc {
	t.Helper()
	cmd := o.command(t)
	p := &Proc{Cmd: cmd, log: &syncBuf{}, done: make(chan struct{}), t: t}
	cmd.Stdout, cmd.Stderr = p.log, p.log
	if err := cmd.Start(); err != nil {
		t.Fatalf("testbin: %v", err)
	}
	go func() { p.err = cmd.Wait(); close(p.done) }()
	t.Cleanup(func() { p.Stop() })
	return p
}

// Logs returns everything the process has written so far.
func (p *Proc) Logs() string { return p.log.String() }

// WaitLog waits until the output contains sub. It fails if the process exits
// first or d passes.
func (p *Proc) WaitLog(sub string, d time.Duration) error {
	deadline := time.Now().Add(d)
	for {
		if strings.Contains(p.log.String(), sub) {
			return nil
		}
		select {
		case <-p.done:
			if strings.Contains(p.log.String(), sub) {
				return nil
			}
			return fmt.Errorf("process exited before logging %q:\n%s", sub, p.log.String())
		case <-time.After(20 * time.Millisecond):
		}
		if time.Now().After(deadline) {
			return fmt.Errorf("timed out waiting for %q:\n%s", sub, p.log.String())
		}
	}
}

// Wait waits up to d for the process to exit on its own and returns its exit code.
func (p *Proc) Wait(d time.Duration) (int, error) {
	select {
	case <-p.done:
		return exitCode(p.err), nil
	case <-time.After(d):
		return -1, errors.New("process still running")
	}
}

// Stop sends SIGTERM, waits up to 10 s (then kills) and returns the exit code.
func (p *Proc) Stop() int {
	select {
	case <-p.done:
	default:
		_ = p.Cmd.Process.Signal(syscall.SIGTERM)
		select {
		case <-p.done:
		case <-time.After(10 * time.Second):
			_ = p.Cmd.Process.Kill()
			<-p.done
		}
	}
	return exitCode(p.err)
}

func exitCode(err error) int {
	var ee *exec.ExitError
	if errors.As(err, &ee) {
		return ee.ExitCode()
	}
	if err != nil {
		return -1
	}
	return 0
}

// ShortDir returns a new empty directory with a short path (unix socket paths
// are limited to about 104 bytes, and t.TempDir paths are too long on macOS).
// Its mode is 0700.
func ShortDir(t testing.TB) string {
	t.Helper()
	d, err := os.MkdirTemp("", "sd")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { os.RemoveAll(d) })
	return d
}

// FreeAddr returns a loopback TCP address that was free a moment ago.
func FreeAddr(t testing.TB) string {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer ln.Close()
	return ln.Addr().String()
}

// hasFlag reports whether args names the flag (as "--name" or "--name=value").
func hasFlag(args []string, name string) bool {
	for _, a := range args {
		if a == name || strings.HasPrefix(a, name+"=") {
			return true
		}
	}
	return false
}
