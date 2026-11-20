package redact

import (
	"errors"
	"strings"
	"testing"
	"time"
)

type in struct {
	Name     string            `json:"name"`
	Password string            `json:"password" sd:"secret"`
	Env      map[string]string `json:"env" sd:"secret"`
	Notes    map[string]any    `json:"notes" sd:"audit"`
	Keys     []string          `json:"keys" sd:"secret"`
	Port     int               `json:"port"`
	PIN      int               `json:"pin" sd:"secret"`
	Nested   struct {
		Token string `json:"token" sd:"secret"`
		Label string `json:"label"`
	} `json:"nested"`
	Skip string `json:"-"`
}

func sample() in {
	i := in{Name: "web", Password: "hunter2-SENTINEL", Env: map[string]string{"DB": "postgres://u:ENVSECRET@h"}, Keys: []string{"k1-SECRET", "k2-SECRET"},
		Notes: map[string]any{"why": "because"}, Port: 80, PIN: 424242, Skip: "x"}
	i.Nested.Token, i.Nested.Label = "nested-SECRET", "ok"
	return i
}

func TestForInputCollectsSecretValues(t *testing.T) {
	r := ForInput(sample())
	for _, s := range []string{"hunter2-SENTINEL", "ENVSECRET", "k1-SECRET", "k2-SECRET", "nested-SECRET", "424242"} {
		if !strings.Contains(strings.Join(r.values, "|"), s) {
			t.Errorf("secret %q not collected: %v", s, r.values)
		}
	}
	if got := r.String("failed: dial postgres://u:postgres://u:ENVSECRET@h with hunter2-SENTINEL and k1-SECRET"); strings.Contains(got, "SECRET") || strings.Contains(got, "SENTINEL") {
		t.Errorf("not redacted: %s", got)
	}
	if got := r.Err(errors.New("bad hunter2-SENTINEL")); got != "bad "+Placeholder {
		t.Errorf("Err = %q", got)
	}
	if r.Err(nil) != "" || (*Redactor)(nil).String("x") != "x" {
		t.Error("nil handling")
	}
	// A pointer to the input works too.
	s := sample()
	if got := ForInput(&s).String("hunter2-SENTINEL"); got != Placeholder {
		t.Errorf("pointer input: %q", got)
	}
	if ForInput(nil).String("abc") != "abc" || ForInput(42).String("abc") != "abc" {
		t.Error("non-struct input")
	}
}

func TestArgsJSONRedactsSecretsAndKeepsTheRest(t *testing.T) {
	got, err := ArgsJSON(sample())
	if err != nil {
		t.Fatal(err)
	}
	for _, leak := range []string{"hunter2-SENTINEL", "ENVSECRET", "k1-SECRET", "nested-SECRET", "424242"} {
		if strings.Contains(got, leak) {
			t.Errorf("args leak %q: %s", leak, got)
		}
	}
	for _, keep := range []string{`"name":"web"`, `"port":80`, `"label":"ok"`, `"why":"because"`, `"password":"[redacted]"`} {
		if !strings.Contains(got, keep) {
			t.Errorf("args missing %s: %s", keep, got)
		}
	}
	if strings.Contains(got, `"Skip"`) || strings.Contains(got, `"x"`) {
		t.Errorf("json:\"-\" field recorded: %s", got)
	}
}

func TestLongestSecretFirst(t *testing.T) {
	r := New("abc", "abcdef")
	if got := r.String("x abcdef y"); got != "x "+Placeholder+" y" {
		t.Errorf("got %q", got)
	}
	if got := New("").String("keep"); got != "keep" {
		t.Error("empty secret must be ignored")
	}
}

type EnvVar struct {
	Key   string `json:"key"`
	Value string `json:"value" sd:"secret"`
}

type hidden struct {
	Token string `json:"token" sd:"secret"`
	Label string `json:"label"`
}

type deep struct {
	Vars    []EnvVar          `json:"vars"`
	ByName  map[string]EnvVar `json:"by_name"`
	Ptr     *EnvVar           `json:"ptr"`
	Arr     [2]EnvVar         `json:"arr"`
	PtrS    []*EnvVar         `json:"ptr_s"`
	Any     any               `json:"any" sd:"audit"`
	hidden                    // an unexported embedded struct: encoding/json still emits its exported fields
	Created time.Time         `json:"created"`
}

func deepSample() deep {
	d := deep{
		Vars:    []EnvVar{{"A", "SLICE-SECRET-1"}, {"B", "SLICE-SECRET-2"}},
		ByName:  map[string]EnvVar{"x": {"X", "MAP-SECRET"}},
		Ptr:     &EnvVar{"P", "PTR-SECRET"},
		Arr:     [2]EnvVar{{"R0", "ARRAY-SECRET-0"}, {"R1", "ARRAY-SECRET-1"}},
		PtrS:    []*EnvVar{{"Q", "PTRSLICE-SECRET"}, nil},
		Any:     map[string]any{"list": []EnvVar{{"I", "IFACE-SECRET"}}},
		Created: time.Date(2026, 11, 20, 1, 2, 3, 0, time.UTC),
	}
	d.hidden = hidden{Token: "EMBEDDED-SECRET", Label: "embedded-label"}
	return d
}

var deepSentinels = []string{"SLICE-SECRET-1", "SLICE-SECRET-2", "MAP-SECRET", "PTR-SECRET", "ARRAY-SECRET-0", "ARRAY-SECRET-1",
	"PTRSLICE-SECRET", "IFACE-SECRET", "EMBEDDED-SECRET"}

func TestNestedSecretsAreFoundAndRedacted(t *testing.T) {
	d := deepSample()
	r := ForInput(d)
	text := "failed: " + strings.Join(deepSentinels, " | ")
	if got := r.String(text); anyContains(got, deepSentinels) {
		t.Errorf("not redacted: %s", got)
	}
	args, err := ArgsJSON(d)
	if err != nil {
		t.Fatal(err)
	}
	if anyContains(args, deepSentinels) {
		t.Fatalf("args leak a nested secret: %s", args)
	}
	for _, keep := range []string{`"key":"A"`, `"key":"B"`, `"key":"X"`, `"key":"R1"`, `"label":"embedded-label"`, `"created":"2026-11-20T01:02:03Z"`, `"value":"[redacted]"`} {
		if !strings.Contains(args, keep) {
			t.Errorf("args lost %s: %s", keep, args)
		}
	}
	if strings.Contains(args, `"hidden"`) {
		t.Errorf("the embedded struct was not flattened like encoding/json does: %s", args)
	}
	// A pointer to the input, and the input behind an interface, work the same.
	if anyContains(ForInput(&d).String(text), deepSentinels) {
		t.Error("pointer input")
	}
	var iface any = d
	if anyContains(ForInput(iface).String(text), deepSentinels) {
		t.Error("interface input")
	}
}

func TestCyclesAndDepthAreBounded(t *testing.T) {
	type node struct {
		Next *node
		S    string `sd:"secret"`
	}
	n := &node{S: "CYCLE-SECRET"}
	n.Next = n
	if got := ForInput(n).String("x CYCLE-SECRET y"); got != "x "+Placeholder+" y" {
		t.Errorf("cycle: %q", got)
	}
	if _, err := ArgsJSON(n); err != nil {
		t.Errorf("cycle in ArgsJSON: %v", err)
	}
}

func anyContains(s string, subs []string) bool {
	for _, x := range subs {
		if strings.Contains(s, x) {
			return true
		}
	}
	return false
}
