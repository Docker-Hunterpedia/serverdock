package memstat

import (
	"strings"
	"testing"
)

const sample = `Name:	serverdock
VmPeak:	  123456 kB
VmHWM:	   30000 kB
VmRSS:	   25000 kB
RssAnon:	   15000 kB
RssFile:	   10000 kB
Threads:	8
`

func TestParseStatus(t *testing.T) {
	p, err := ParseStatus(strings.NewReader(sample))
	if err != nil {
		t.Fatal(err)
	}
	want := Proc{VmRSS: 25000 * 1024, VmHWM: 30000 * 1024, RssAnon: 15000 * 1024, RssFile: 10000 * 1024}
	if p != want {
		t.Fatalf("got %+v want %+v", p, want)
	}
}

func TestParseStatusErrors(t *testing.T) {
	for _, in := range []string{"VmRSS:\n", "VmRSS: abc kB\n", "VmRSS: 5 MB\n"} {
		if _, err := ParseStatus(strings.NewReader(in)); err == nil {
			t.Errorf("expected error for %q", in)
		}
	}
}

func TestParseStatusMissingKeys(t *testing.T) {
	p, err := ParseStatus(strings.NewReader("Name: x\n"))
	if err != nil || p != (Proc{}) {
		t.Fatalf("got %+v, %v", p, err)
	}
}

func TestRead(t *testing.T) {
	s := Read()
	if s.Goroutines < 1 || s.HeapAlloc == 0 || s.Sys == 0 {
		t.Fatalf("implausible snapshot %+v", s)
	}
}
