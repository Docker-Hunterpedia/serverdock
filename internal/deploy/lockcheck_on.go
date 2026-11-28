//go:build testhooks

package deploy

import (
	"bytes"
	"fmt"
	"runtime"
	"strconv"
	"strings"
	"sync"
)

// The test-only lock-order checker (plan 0004 step 10, "Lock order"). Three locks
// exist: the per-service lock, the allocation lock and the global route lock. They
// are only ever taken in the order service, allocation, route, never in reverse,
// and a goroutine never holds two services' locks. Every acquisition in the
// package reports itself here; a goroutine that takes a lock while holding one of a
// higher rank (or a second service lock) is recorded, and the package's TestMain
// fails the whole test run if anything was recorded. The checker keys on goroutine
// ids read from runtime.Stack: acceptable only because this file is built with
// -tags testhooks.

type lockKind int

const (
	lockService lockKind = 1
	lockAlloc   lockKind = 2
	lockRoute   lockKind = 3
)

func (k lockKind) String() string {
	switch k {
	case lockService:
		return "service lock"
	case lockAlloc:
		return "allocation lock"
	case lockRoute:
		return "route lock"
	}
	return "unknown lock"
}

// held is one lock a goroutine holds: its kind, the object it belongs to (a
// ServiceLocks, an Allocator, a Routes: two instances never collide) and, for a
// service lock, the service id.
type held struct {
	kind lockKind
	inst any
	key  int64
}

type lockChecker struct {
	mu         sync.Mutex
	exempt     map[uint64]int
	byG        map[uint64][]held
	owner      map[held]uint64
	violations []string
}

var checker = &lockChecker{byG: map[uint64][]held{}, owner: map[held]uint64{}}

func stackNow() []byte {
	var b [2048]byte
	return b[:runtime.Stack(b[:], false)]
}

func goid() uint64 {
	var b [64]byte
	n := runtime.Stack(b[:], false)
	b2 := bytes.TrimPrefix(b[:n], []byte("goroutine "))
	if i := bytes.IndexByte(b2, ' '); i > 0 {
		id, _ := strconv.ParseUint(string(b2[:i]), 10, 64)
		return id
	}
	return 0
}

func lockTaken(kind lockKind, inst any, key int64)    { checker.taken(kind, inst, key) }
func lockReleased(kind lockKind, inst any, key int64) { checker.released(kind, inst, key) }

func (c *lockChecker) taken(kind lockKind, inst any, key int64) {
	g := goid()
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.exempt[g] > 0 {
		return
	}
	for _, h := range c.byG[g] {
		switch {
		case h.kind > kind:
			c.violations = append(c.violations, fmt.Sprintf("goroutine %d took the %s while holding the %s (the order is service, allocation, route)\n%s", g, kind, h.kind, stackNow()))
		case h.kind == kind && kind == lockService:
			c.violations = append(c.violations, fmt.Sprintf("goroutine %d took the lock of service %d while holding the lock of service %d\n%s", g, key, h.key, stackNow()))
		case h.kind == kind:
			c.violations = append(c.violations, fmt.Sprintf("goroutine %d took the %s twice\n%s", g, kind, stackNow()))
		}
	}
	h := held{kind, inst, key}
	c.byG[g] = append(c.byG[g], h)
	c.owner[h] = g
}

func (c *lockChecker) released(kind lockKind, inst any, key int64) {
	c.mu.Lock()
	defer c.mu.Unlock()
	h := held{kind, inst, key}
	g, ok := c.owner[h]
	if !ok {
		return
	}
	delete(c.owner, h)
	l := c.byG[g]
	for i := len(l) - 1; i >= 0; i-- {
		if l[i] == h {
			l = append(l[:i], l[i+1:]...)
			break
		}
	}
	if len(l) == 0 {
		delete(c.byG, g)
	} else {
		c.byG[g] = l
	}
}

// lockOrderReport lists every violation seen so far ("" when there is none).
func lockOrderReport() string {
	checker.mu.Lock()
	defer checker.mu.Unlock()
	return strings.Join(checker.violations, "\n")
}

// lockCheckExempt excludes the calling goroutine from the checker until the returned
// function runs (see the shipped build's no-op).
func lockCheckExempt() func() {
	g := goid()
	checker.mu.Lock()
	if checker.exempt == nil {
		checker.exempt = map[uint64]int{}
	}
	checker.exempt[g]++
	checker.mu.Unlock()
	return func() {
		checker.mu.Lock()
		checker.exempt[g]--
		checker.mu.Unlock()
	}
}
