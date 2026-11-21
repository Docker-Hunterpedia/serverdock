package main

import (
	"bytes"
	"strings"
	"testing"

	"github.com/Docker-Hunterpedia/serverdock/web"
)

func TestRun(t *testing.T) {
	cases := []struct {
		args   []string
		code   int
		outHas string
		errHas string
	}{
		{[]string{"version"}, 0, "serverdock dev ui=" + web.UI, ""},
		{nil, 2, "", "usage:"},
		{[]string{"bogus"}, 2, "", "unknown command"},
		{[]string{"help"}, 0, "usage:", ""},
	}
	for _, c := range cases {
		var out, errb bytes.Buffer
		if got := run(c.args, &out, &errb); got != c.code {
			t.Errorf("%v: code %d want %d", c.args, got, c.code)
		}
		if !strings.Contains(out.String(), c.outHas) || !strings.Contains(errb.String(), c.errHas) {
			t.Errorf("%v: out=%q err=%q", c.args, out.String(), errb.String())
		}
	}
}
