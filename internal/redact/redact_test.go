package redact

import (
	"errors"
	"strings"
	"testing"
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
