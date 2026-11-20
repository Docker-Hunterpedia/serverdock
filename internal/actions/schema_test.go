package actions

import (
	"encoding/json"
	"reflect"
	"strings"
	"testing"
	"time"
)

type level string

func (level) Enum() []string { return []string{"low", "high"} }

type inner struct {
	N uint16 `json:"n"`
}

type embedded struct {
	Base string `json:"base"`
}

type kitchen struct {
	embedded
	Name      string            `json:"name" doc:"what it is"`
	Pass      string            `json:"pass" sd:"secret"`
	Count     int               `json:"count,omitempty"`
	Ratio     float64           `json:"ratio"`
	On        bool              `json:"on"`
	Opt       *string           `json:"opt"`
	PtrStruct *inner            `json:"ptr_struct,omitempty"`
	List      []inner           `json:"list"`
	Fixed     [3]int8           `json:"fixed"`
	Tags      map[string]string `json:"tags,omitempty"`
	When      time.Time         `json:"when"`
	Level     level             `json:"level"`
	Any       any               `json:"any"`
	Raw       json.RawMessage   `json:"raw,omitempty"`
	Blob      []byte            `json:"blob,omitempty"`
	Skipped   string            `json:"-"`
	hidden    string
}

func mustSchema(t *testing.T, typ reflect.Type) map[string]any {
	t.Helper()
	s, err := SchemaOf(typ)
	if err != nil {
		t.Fatal(err)
	}
	return s
}

func js(v any) string { b, _ := json.Marshal(v); return string(b) }

func TestSchemaOfEveryKind(t *testing.T) {
	cases := map[string]struct {
		typ  reflect.Type
		want string
	}{
		"string":    {reflect.TypeFor[string](), `{"type":"string"}`},
		"bool":      {reflect.TypeFor[bool](), `{"type":"boolean"}`},
		"int":       {reflect.TypeFor[int64](), `{"type":"integer"}`},
		"uint":      {reflect.TypeFor[uint32](), `{"minimum":0,"type":"integer"}`},
		"float":     {reflect.TypeFor[float32](), `{"type":"number"}`},
		"time":      {reflect.TypeFor[time.Time](), `{"format":"date-time","type":"string"}`},
		"pointer":   {reflect.TypeFor[*string](), `{"type":["string","null"]}`},
		"ptr-obj":   {reflect.TypeFor[*inner](), `{"additionalProperties":false,"properties":{"n":{"minimum":0,"type":"integer"}},"required":["n"],"type":["object","null"]}`},
		"slice":     {reflect.TypeFor[[]string](), `{"items":{"type":"string"},"type":"array"}`},
		"array":     {reflect.TypeFor[[2]bool](), `{"items":{"type":"boolean"},"maxItems":2,"minItems":2,"type":"array"}`},
		"bytes":     {reflect.TypeFor[[]byte](), `{"contentEncoding":"base64","type":"string"}`},
		"map":       {reflect.TypeFor[map[string]int](), `{"additionalProperties":{"type":"integer"},"type":"object"}`},
		"interface": {reflect.TypeFor[any](), `{}`},
		"raw":       {reflect.TypeFor[json.RawMessage](), `{}`}, // any JSON value
		"enum":      {reflect.TypeFor[level](), `{"enum":["low","high"],"type":"string"}`},
		"none":      {reflect.TypeFor[None](), `{"additionalProperties":false,"properties":{},"type":"object"}`},
	}
	for name, c := range cases {
		if got := js(mustSchema(t, c.typ)); got != c.want {
			t.Errorf("%s:\n got %s\nwant %s", name, got, c.want)
		}
	}
}

func TestSchemaOfAStruct(t *testing.T) {
	s := mustSchema(t, reflect.TypeFor[kitchen]())
	props := s["properties"].(map[string]any)
	if s["additionalProperties"] != false || s["type"] != "object" {
		t.Fatalf("%v", s)
	}
	for _, skipped := range []string{"Skipped", "hidden", "-"} {
		if _, ok := props[skipped]; ok {
			t.Errorf("%s is in the schema", skipped)
		}
	}
	if _, ok := props["base"]; !ok {
		t.Error("an embedded struct was not inlined")
	}
	pass := props["pass"].(map[string]any)
	if pass["writeOnly"] != true || pass["format"] != "password" {
		t.Errorf("secret field: %v", pass)
	}
	if props["name"].(map[string]any)["description"] != "what it is" {
		t.Errorf("doc tag: %v", props["name"])
	}
	req := map[string]bool{}
	for _, r := range s["required"].([]any) {
		req[r.(string)] = true
	}
	// Required: not omitempty and not a pointer.
	for _, want := range []string{"base", "name", "pass", "ratio", "on", "list", "fixed", "when", "level", "any"} {
		if !req[want] {
			t.Errorf("%s should be required", want)
		}
	}
	for _, not := range []string{"count", "opt", "ptr_struct", "tags", "raw", "blob"} {
		if req[not] {
			t.Errorf("%s should not be required", not)
		}
	}
	if opt := props["opt"].(map[string]any); !reflect.DeepEqual(opt["type"], []any{"string", "null"}) {
		t.Errorf("opt: %v", opt)
	}
}

func TestSchemaOfRefusesWhatItCannotDescribe(t *testing.T) {
	type bad1 struct{ C chan int }
	type bad2 struct{ F func() }
	type bad3 struct{ M map[int]string }
	type bad4 struct{ Z complex128 }
	type rec struct {
		Next *rec `json:"next"`
	}
	for name, typ := range map[string]reflect.Type{"chan": reflect.TypeFor[bad1](), "func": reflect.TypeFor[bad2](), "int key": reflect.TypeFor[bad3](),
		"complex": reflect.TypeFor[bad4](), "chan direct": reflect.TypeFor[chan int](), "recursive": reflect.TypeFor[rec]()} {
		if _, err := SchemaOf(typ); err == nil {
			t.Errorf("%s was accepted", name)
		}
	}
	if _, err := SchemaOf(reflect.TypeFor[bad1]()); err == nil || !strings.Contains(err.Error(), "bad1.C") {
		t.Errorf("the error does not name the field: %v", err)
	}
}

// Every built-in action's input and output reflect.
func TestEveryBuiltinActionReflects(t *testing.T) {
	r := New(newEnv(t).log, quiet)
	RegisterBuiltins(r, Deps{PanelCert: func() PanelCert { return PanelCert{} }})
	if len(r.List()) < 4 {
		t.Fatalf("%d builtins", len(r.List()))
	}
	for _, a := range r.List() {
		if a.OutputType == nil || a.InputType == nil {
			t.Fatalf("%s lacks a type", a.Name)
		}
		for what, typ := range map[string]reflect.Type{"input": a.InputType, "output": a.OutputType} {
			if _, err := SchemaOf(typ); err != nil {
				t.Errorf("%s %s: %v", a.Name, what, err)
			}
		}
	}
	if info, ok := r.Lookup("system.info"); !ok || info.OutputType == nil {
		t.Errorf("Lookup: %+v %v", info, ok)
	}
	if _, ok := r.Lookup("nope.nothing"); ok {
		t.Error("Lookup found a missing action")
	}
}
