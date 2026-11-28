//go:build !testhooks

package deploy

// The lock-order checker (plan 0004 step 10) exists only in a testhooks build. In
// the shipped build these are empty functions the compiler removes.

// lockKind ranks the three locks of the deadlock rule: service lock, then
// allocation lock, then route lock. They are only ever taken in that order.
type lockKind int

const (
	lockService lockKind = 1
	lockAlloc   lockKind = 2
	lockRoute   lockKind = 3
)

func lockTaken(lockKind, any, int64)    {}
func lockReleased(lockKind, any, int64) {}

// lockOrderReport is "" in a shipped build.
func lockOrderReport() string { return "" }

// lockCheckExempt excludes the calling goroutine from the checker until the returned
// function runs. Only a test of the lock types themselves (taking two services' locks
// to test the API, or one allocator's lock inside another's to interleave them) uses it.
func lockCheckExempt() func() { return func() {} }
