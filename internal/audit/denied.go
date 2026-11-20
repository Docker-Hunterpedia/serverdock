package audit

import (
	"context"
	"fmt"
	"sync"
	"time"
)

const (
	deniedPerWindow = 10
	deniedWindow    = time.Minute
	deniedMaxKeys   = 4096
)

// deniedLimiter rate-limits denied rows per actor + action: at most 10 per
// minute are written; the rest are counted and written as one "N denied
// attempts suppressed" row when the window rolls over (lazily, on the next
// denied attempt for that key, or on Flush). No timer.
type deniedLimiter struct {
	l  *Logger
	mu sync.Mutex
	m  map[string]*deniedWindowState
}

type deniedWindowState struct {
	start      time.Time
	written    int
	suppressed int
	proto      Entry // the last suppressed attempt, for the summary row
}

func (d *deniedLimiter) init(l *Logger) { d.l, d.m = l, map[string]*deniedWindowState{} }

// Denied records a denied attempt (phase denied), subject to the per-actor,
// per-action rate limit. It returns whether a row was written for this attempt.
func (l *Logger) Denied(ctx context.Context, e Entry) (bool, error) {
	e.Phase, e.Result, e.IntentID = PhaseDenied, ResultDenied, 0
	return l.denied.record(ctx, e)
}

// Flush writes the pending "suppressed" summaries (call at shutdown).
func (l *Logger) Flush(ctx context.Context) error {
	d := &l.denied
	d.mu.Lock()
	var pending []Entry
	for k, s := range d.m {
		if s.suppressed > 0 {
			pending = append(pending, s.summary())
		}
		delete(d.m, k)
	}
	d.mu.Unlock()
	var first error
	for _, e := range pending {
		if _, err := l.Append(ctx, e); err != nil && first == nil {
			first = err
		}
	}
	return first
}

func (s *deniedWindowState) summary() Entry {
	e := s.proto
	e.Error = fmt.Sprintf("%d denied attempts suppressed", s.suppressed)
	e.ArgsJSON = "{}"
	return e
}

func (d *deniedLimiter) record(ctx context.Context, e Entry) (bool, error) {
	key := e.ActorKind + "\x00" + e.ActorID + "\x00" + e.Action
	now := d.l.now()
	d.mu.Lock()
	s := d.m[key]
	var flush *Entry
	if s != nil && now.Sub(s.start) >= deniedWindow {
		if s.suppressed > 0 {
			sum := s.summary()
			flush = &sum
		}
		delete(d.m, key)
		s = nil
	}
	if s == nil {
		if len(d.m) >= deniedMaxKeys {
			d.evictLocked(now)
		}
		s = &deniedWindowState{start: now}
		d.m[key] = s
	}
	write := s.written < deniedPerWindow
	if write {
		s.written++
	} else {
		s.suppressed++
		s.proto = e
	}
	d.mu.Unlock()
	if flush != nil {
		if _, err := d.l.Append(ctx, *flush); err != nil {
			return false, err
		}
	}
	if !write {
		return false, nil
	}
	_, err := d.l.Append(ctx, e)
	return err == nil, err
}

// evictLocked makes room in a full table: expired windows go first, then
// arbitrary ones. A dropped window loses its suppressed count; that needs
// thousands of distinct actor+action pairs being denied inside one minute, which
// authenticated actors do not produce.
func (d *deniedLimiter) evictLocked(now time.Time) {
	for k, s := range d.m {
		if now.Sub(s.start) >= deniedWindow {
			delete(d.m, k)
		}
	}
	for k := range d.m {
		if len(d.m) < deniedMaxKeys {
			break
		}
		delete(d.m, k)
	}
}
