// Package memstat reports process memory from /proc/self/status and the Go
// runtime. It is a convenience for debug output only; the bench measures from
// outside the process.
package memstat

import (
	"bufio"
	"fmt"
	"io"
	"os"
	"runtime"
	"strconv"
	"strings"
)

// Proc holds values from /proc/<pid>/status, in bytes.
type Proc struct {
	VmRSS   uint64
	VmHWM   uint64
	RssAnon uint64
	RssFile uint64
}

// Snapshot combines /proc values (zero off Linux) with Go runtime stats.
type Snapshot struct {
	Proc       Proc   `json:"proc"`
	HeapAlloc  uint64 `json:"heap_alloc"`
	HeapSys    uint64 `json:"heap_sys"`
	HeapInuse  uint64 `json:"heap_inuse"`
	StackInuse uint64 `json:"stack_inuse"`
	Sys        uint64 `json:"sys"`
	NumGC      uint32 `json:"num_gc"`
	Goroutines int    `json:"goroutines"`
}

// ParseStatus parses the contents of a /proc/<pid>/status file.
func ParseStatus(r io.Reader) (Proc, error) {
	var p Proc
	sc := bufio.NewScanner(r)
	for sc.Scan() {
		key, val, ok := strings.Cut(sc.Text(), ":")
		if !ok {
			continue
		}
		var dst *uint64
		switch key {
		case "VmRSS":
			dst = &p.VmRSS
		case "VmHWM":
			dst = &p.VmHWM
		case "RssAnon":
			dst = &p.RssAnon
		case "RssFile":
			dst = &p.RssFile
		default:
			continue
		}
		v, err := parseKB(val)
		if err != nil {
			return Proc{}, fmt.Errorf("memstat: %s: %w", key, err)
		}
		*dst = v
	}
	return p, sc.Err()
}

func parseKB(s string) (uint64, error) {
	fields := strings.Fields(s)
	if len(fields) == 0 {
		return 0, fmt.Errorf("empty value")
	}
	n, err := strconv.ParseUint(fields[0], 10, 64)
	if err != nil {
		return 0, err
	}
	if len(fields) > 1 && fields[1] != "kB" {
		return 0, fmt.Errorf("unexpected unit %q", fields[1])
	}
	return n * 1024, nil
}

// Read returns the current snapshot. /proc errors (non-Linux) leave Proc zero.
func Read() Snapshot {
	var s Snapshot
	if f, err := os.Open("/proc/self/status"); err == nil {
		s.Proc, _ = ParseStatus(f)
		f.Close()
	}
	var m runtime.MemStats
	runtime.ReadMemStats(&m)
	s.HeapAlloc, s.HeapSys, s.HeapInuse = m.HeapAlloc, m.HeapSys, m.HeapInuse
	s.StackInuse, s.Sys, s.NumGC = m.StackInuse, m.Sys, m.NumGC
	s.Goroutines = runtime.NumGoroutine()
	return s
}
