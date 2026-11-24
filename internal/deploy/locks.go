package deploy

import (
	"context"
	"sync"
)

// ServiceLocks is the per-service in-memory lock of plan 0004 ("Jobs and the
// scheduler", "Lock order"): the deploy runner holds a service's lock for the
// whole job, and the step 10 resync takes it with TryLock only, so it never waits
// behind a job. (The database already allows one running job per service; this lock
// is what makes the resync's "is a job running here" check race-free.)
//
// Lock order: service lock, then the allocator's lock, then the route lock. Never
// the reverse, and a goroutine never holds two services' locks.
//
// Entries exist only while a lock is held or awaited, so the map is bounded by
// the number of services in use at once (at most 2 running jobs plus the resync).
type ServiceLocks struct {
	mu sync.Mutex
	m  map[int64]*svcLock
}

type svcLock struct {
	ch   chan struct{} // capacity 1: a token in it means held
	refs int           // holders and waiters
}

// Lock waits for the service's lock or for ctx. It returns the unlock function.
func (l *ServiceLocks) Lock(ctx context.Context, service int64) (unlock func(), err error) {
	e := l.ref(service)
	select {
	case e.ch <- struct{}{}:
		return l.unlocker(service, e), nil
	case <-ctx.Done():
		l.unref(service, e)
		return nil, context.Cause(ctx)
	}
}

// TryLock takes the lock if it is free; it never waits.
func (l *ServiceLocks) TryLock(service int64) (unlock func(), ok bool) {
	e := l.ref(service)
	select {
	case e.ch <- struct{}{}:
		return l.unlocker(service, e), true
	default:
		l.unref(service, e)
		return nil, false
	}
}

func (l *ServiceLocks) ref(service int64) *svcLock {
	l.mu.Lock()
	defer l.mu.Unlock()
	if l.m == nil {
		l.m = map[int64]*svcLock{}
	}
	e := l.m[service]
	if e == nil {
		e = &svcLock{ch: make(chan struct{}, 1)}
		l.m[service] = e
	}
	e.refs++
	return e
}

func (l *ServiceLocks) unref(service int64, e *svcLock) {
	l.mu.Lock()
	defer l.mu.Unlock()
	if e.refs--; e.refs == 0 {
		delete(l.m, service)
	}
}

func (l *ServiceLocks) unlocker(service int64, e *svcLock) func() {
	var once sync.Once
	return func() {
		once.Do(func() {
			<-e.ch
			l.unref(service, e)
		})
	}
}

// Len is the number of entries (for tests: it must return to 0).
func (l *ServiceLocks) Len() int {
	l.mu.Lock()
	defer l.mu.Unlock()
	return len(l.m)
}
