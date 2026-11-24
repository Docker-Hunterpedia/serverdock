//go:build testhooks

package deploy

import (
	"os"
	"path/filepath"
	"testing"
	"time"
)

// With -tags testhooks (what internal/testbin builds the server with) a named
// pause point writes its marker file and blocks; every other name returns.
func TestPausePointBlocksOnlyWhereNamed(t *testing.T) {
	if !hooksCompiled {
		t.Fatal("hooksCompiled is false in a testhooks build")
	}
	pauseRelease = make(chan struct{})
	t.Cleanup(func() { close(pauseRelease) }) // frees the paused goroutine; the variable stays set
	dir := t.TempDir()
	t.Setenv("SERVERDOCK_TESTHOOK_PAUSE", "health:before,pull:after")
	t.Setenv("SERVERDOCK_TESTHOOK_DIR", dir)
	other := make(chan struct{})
	go func() { pausePoint("start:before"); close(other) }()
	select {
	case <-other:
	case <-time.After(2 * time.Second):
		t.Fatal("an unnamed point blocked")
	}
	paused := make(chan struct{})
	go func() { pausePoint("health:before"); close(paused) }() // blocks for good (the test process ends)
	deadline := time.Now().Add(2 * time.Second)
	for {
		if _, err := os.Stat(filepath.Join(dir, "paused-health-before")); err == nil {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("no marker file")
		}
		time.Sleep(5 * time.Millisecond)
	}
	select {
	case <-paused:
		t.Fatal("the named point returned")
	case <-time.After(50 * time.Millisecond):
	}
}
