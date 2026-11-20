package actions

import (
	"context"
	"encoding/json"
	"fmt"
	"go/ast"
	"go/parser"
	"go/token"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func mustPanic(t *testing.T, name string, f func()) {
	t.Helper()
	defer func() {
		if recover() == nil {
			t.Errorf("%s: Register did not panic", name)
		}
	}()
	f()
}

func exec0[I any](context.Context, Call, I) (string, error) { return "", nil }

type okIn struct{ Name string }

func (okIn) Validate() error { return nil }

func okDef() Def[okIn, string] {
	return Def[okIn, string]{Name: "t.ok", Summary: "x", Risk: RiskRead, Scope: ScopeRead, Execute: exec0[okIn]}
}

func newReg(t *testing.T) *Registry { return newEnv(t).reg }

func TestRegisterPanicsOnScopeRiskMismatch(t *testing.T) {
	tg := func(okIn) string { return "t" }
	cases := map[string]func(*Def[okIn, string]){
		"mutate with read scope":  func(d *Def[okIn, string]) { d.Risk, d.Scope, d.Target = RiskMutate, ScopeRead, tg },
		"destructive with write":  func(d *Def[okIn, string]) { d.Risk, d.Scope, d.Target = RiskDestructive, ScopeWrite, tg },
		"sensitive with write":    func(d *Def[okIn, string]) { d.Sensitive, d.Scope, d.Target = true, ScopeWrite, tg },
		"missing risk":            func(d *Def[okIn, string]) { d.Risk = 0 },
		"missing scope":           func(d *Def[okIn, string]) { d.Scope = 0 },
		"bad name":                func(d *Def[okIn, string]) { d.Name = "Bad Name" },
		"name without an area":    func(d *Def[okIn, string]) { d.Name = "verb" },
		"no summary":              func(d *Def[okIn, string]) { d.Summary = "" },
		"no execute":              func(d *Def[okIn, string]) { d.Execute = nil },
		"mutate without a target": func(d *Def[okIn, string]) { d.Risk, d.Scope = RiskMutate, ScopeWrite },
		"webhook on a read":       func(d *Def[okIn, string]) { d.WebhookAllowed, d.Target = true, tg },
		"webhook on a destructive": func(d *Def[okIn, string]) {
			d.Risk, d.Scope, d.WebhookAllowed, d.Target = RiskDestructive, ScopeAdmin, true, tg
		},
		"webhook and session only": func(d *Def[okIn, string]) {
			d.Risk, d.Scope, d.WebhookAllowed, d.SessionOnly, d.Target = RiskMutate, ScopeWrite, true, true, tg
		},
	}
	for name, mod := range cases {
		d := okDef()
		mod(&d)
		mustPanic(t, name, func() { Register(newReg(t), d) })
	}
	// And the valid shapes register.
	r := newReg(t)
	Register(r, okDef())
	mustPanic(t, "duplicate", func() { Register(r, okDef()) })
}

type withMap struct{ M map[string]string }

func (withMap) Validate() error { return nil }

type withAny struct{ V any }

func (withAny) Validate() error { return nil }

type withRaw struct{ R json.RawMessage }

func (withRaw) Validate() error { return nil }

type nested struct {
	Inner struct{ Deep []map[string]any }
}

func (nested) Validate() error { return nil }

type ptrAny struct{ P *any }

func (ptrAny) Validate() error { return nil }

type tagged struct {
	Secret map[string]string `sd:"secret"`
	Audit  json.RawMessage   `sd:"audit"`
	Plain  string
	Num    int
}

func (tagged) Validate() error { return nil }

type hiddenMap struct{ Extra map[string]string }

type embedsHidden struct {
	hiddenMap // unexported embedded struct: its exported field is still input
	Name      string
}

func (embedsHidden) Validate() error { return nil }

type badTag struct {
	X string `sd:"public"`
}

func (badTag) Validate() error { return nil }

func TestRegisterPanicsOnUntaggedFreeFormFields(t *testing.T) {
	reg := func(t *testing.T) *Registry { return newReg(t) }
	mustPanic(t, "map", func() {
		Register(reg(t), Def[withMap, string]{Name: "t.map", Summary: "x", Risk: RiskRead, Scope: ScopeRead, Execute: exec0[withMap]})
	})
	mustPanic(t, "any", func() {
		Register(reg(t), Def[withAny, string]{Name: "t.any", Summary: "x", Risk: RiskRead, Scope: ScopeRead, Execute: exec0[withAny]})
	})
	mustPanic(t, "RawMessage", func() {
		Register(reg(t), Def[withRaw, string]{Name: "t.raw", Summary: "x", Risk: RiskRead, Scope: ScopeRead, Execute: exec0[withRaw]})
	})
	mustPanic(t, "nested slice of maps", func() {
		Register(reg(t), Def[nested, string]{Name: "t.nested", Summary: "x", Risk: RiskRead, Scope: ScopeRead, Execute: exec0[nested]})
	})
	mustPanic(t, "pointer to any", func() {
		Register(reg(t), Def[ptrAny, string]{Name: "t.ptr", Summary: "x", Risk: RiskRead, Scope: ScopeRead, Execute: exec0[ptrAny]})
	})
	mustPanic(t, "map inside an unexported embedded struct", func() {
		Register(reg(t), Def[embedsHidden, string]{Name: "t.emb", Summary: "x", Risk: RiskRead, Scope: ScopeRead, Execute: exec0[embedsHidden]})
	})
	mustPanic(t, "unknown sd tag", func() {
		Register(reg(t), Def[badTag, string]{Name: "t.tag", Summary: "x", Risk: RiskRead, Scope: ScopeRead, Execute: exec0[badTag]})
	})
	// Tagged free-form fields are accepted.
	Register(reg(t), Def[tagged, string]{Name: "t.tagged", Summary: "x", Risk: RiskRead, Scope: ScopeRead, Execute: exec0[tagged]})
}

// ---- docs/security.md "Unattended destructive actions" ----

const unattendedHeading = "## Unattended destructive actions"

// unlistedUnattendedDestructive returns the destructive actions with a System
// reason whose name does not appear (in backticks) in the doc's "Unattended
// destructive actions" section.
func unlistedUnattendedDestructive(list []Info, doc string) []string {
	_, rest, ok := strings.Cut(doc, unattendedHeading)
	section := ""
	if ok {
		section, _, _ = strings.Cut(rest, "\n## ")
	}
	var missing []string
	for _, a := range list {
		if a.Risk == RiskDestructive && a.System != "" && !strings.Contains(section, "`"+a.Name+"`") {
			missing = append(missing, a.Name)
		}
	}
	return missing
}

func TestEveryUnattendedDestructiveActionIsListedInTheThreatModel(t *testing.T) {
	doc, err := os.ReadFile(filepath.Join("..", "..", "docs", "security.md"))
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(doc), unattendedHeading) {
		t.Fatalf("docs/security.md has no %q section", unattendedHeading)
	}
	// Every built-in action (and, when a milestone adds more, they register here
	// too: add them to this registry).
	r := New(newEnv(t).log, quiet) // a registry with only the built-ins
	RegisterBuiltins(r, Deps{})
	if m := unlistedUnattendedDestructive(r.List(), string(doc)); len(m) != 0 {
		t.Errorf("destructive actions with a System reason are not listed under %q in docs/security.md: %v", unattendedHeading, m)
	}
	for _, a := range r.List() {
		if a.Risk == RiskDestructive && a.System != "" {
			t.Errorf("built-in %s is destructive and unattended: security must review it first", a.Name)
		}
	}
}

func TestUnattendedDestructiveCheckCatchesAMissingEntry(t *testing.T) {
	e := newEnv(t) // has t.destroy_sys (destructive, System set) and t.destroy (no System)
	list := e.reg.List()
	doc := "# Threat model\n\n## Unattended destructive actions\n\nThey are reviewed.\n\n- `other.action` because.\n\n## Debug access\n\n`t.destroy_sys` mentioned in the wrong section.\n"
	if m := unlistedUnattendedDestructive(list, doc); len(m) != 1 || m[0] != "t.destroy_sys" {
		t.Fatalf("missing = %v, want t.destroy_sys (a mention in another section does not count)", m)
	}
	doc = strings.Replace(doc, "- `other.action` because.", "- `t.destroy_sys` retention sweep: expires old backups past the retention window.", 1)
	if m := unlistedUnattendedDestructive(list, doc); len(m) != 0 {
		t.Fatalf("a listed action was reported missing: %v", m)
	}
	if m := unlistedUnattendedDestructive(list, "no section at all"); len(m) != 1 {
		t.Errorf("no section: %v", m)
	}
}

// ---- API and MCP can never reach the system actor ----

// internal/api and internal/mcp must not refer to the SystemInvoker or the
// executor marker. (internal/mcp arrives in M9; the test covers it when it exists.)
func TestAPIAndMCPDoNotReferenceTheSystemInvoker(t *testing.T) {
	forbidden := map[string]bool{"SystemInvoker": true, "InvokeAs": true, "KindSystem": true}
	for _, pkg := range []string{"api", "mcp"} {
		dir := filepath.Join("..", pkg)
		files, _ := filepath.Glob(filepath.Join(dir, "*.go"))
		for _, f := range files {
			af, err := parser.ParseFile(token.NewFileSet(), f, nil, 0)
			if err != nil {
				t.Fatal(err)
			}
			ast.Inspect(af, func(n ast.Node) bool {
				if id, ok := n.(*ast.Ident); ok && forbidden[id.Name] {
					t.Errorf("%s refers to %s", f, id.Name)
				}
				return true
			})
		}
	}
	// Self-check: the walker does see these names in this package.
	af, _ := parser.ParseFile(token.NewFileSet(), "system.go", nil, 0)
	var seen bool
	ast.Inspect(af, func(n ast.Node) bool {
		if id, ok := n.(*ast.Ident); ok && id.Name == "SystemInvoker" {
			seen = true
		}
		return true
	})
	if !seen {
		t.Fatal("the identifier walk is broken")
	}
	// No exported way to build a system actor.
	for _, name := range []string{"SystemActor", "NewSystemActor"} {
		if _, err := os.Stat(name); err == nil {
			t.Error(name)
		}
	}
}

// ---- fuzz: arbitrary input never panics and never executes invalid input ----

func FuzzInvokeInput(f *testing.F) {
	for _, s := range []string{`{"name":"x"}`, `{}`, `null`, ``, `[]`, `{"name":"x","secret":"s"}`, `{"name":"x"}{"name":"y"}`,
		`{"name":"\u0000"}`, `{"name":` + strings.Repeat(`"a`, 100), `{"name":"x","extra":{}}`, `"str"`, `123`} {
		f.Add([]byte(s))
	}
	f.Fuzz(func(t *testing.T, b []byte) {
		fe := newFuzzEnv(t)
		u := UserActor("u", ScopeAdmin, fe.fresh())
		before := fe.execs["t.mutate"].Load()
		res, err := fe.reg.Invoke(ctx, u, "t.mutate", json.RawMessage(b), Options{})
		ran := fe.execs["t.mutate"].Load() - before
		if err == nil && ran != 1 {
			t.Fatalf("succeeded without executing: %v", res)
		}
		if err != nil && ran != 0 && !strings.Contains(err.Error(), "audit") {
			t.Fatalf("failed but executed: %v", err)
		}
		_ = fmt.Sprint(res)
	})
}

// The registry walk: every registered action (here the built-ins plus the test
// table) satisfies the rules Register enforces, as seen through List.
func TestWalkEveryAction(t *testing.T) {
	e := newEnv(t)
	RegisterBuiltins(e.reg, Deps{})
	list := e.reg.List()
	if len(list) < 11 {
		t.Fatalf("only %d actions", len(list))
	}
	seen := map[string]bool{}
	for _, a := range list {
		if seen[a.Name] || !nameRe.MatchString(a.Name) || a.Summary == "" {
			t.Errorf("bad action %q", a.Name)
		}
		seen[a.Name] = true
		switch a.Risk {
		case RiskMutate:
			if a.Scope < ScopeWrite {
				t.Errorf("%s: mutate below write scope", a.Name)
			}
		case RiskDestructive:
			if a.Scope < ScopeAdmin {
				t.Errorf("%s: destructive below admin scope", a.Name)
			}
		}
		if a.Sensitive && a.Scope < ScopeAdmin {
			t.Errorf("%s: sensitive below admin scope", a.Name)
		}
		if a.WebhookOK && (a.Risk != RiskMutate || a.Sensitive || a.SessionOnly) {
			t.Errorf("%s: bad webhook flag", a.Name)
		}
		if err := checkFreeForm(a.InputType, ""); err != nil {
			t.Errorf("%s: %v", a.Name, err)
		}
	}
	for _, want := range []string{"system.info", "system.docker", "audit.verify"} {
		if !seen[want] {
			t.Errorf("built-in %s missing", want)
		}
	}
}

// A type that encodes itself would be recorded as it marshals, bypassing the
// field-level redaction, so a secret inside one is refused at Register.
type cred struct {
	User string `json:"user"`
	Pass string `json:"pass" sd:"secret"`
}

func (c cred) MarshalJSON() ([]byte, error) { // value receiver: the reviewer's reproduction
	return json.Marshal(map[string]string{"user": c.User, "pass": c.Pass})
}

type ptrCred struct {
	Pass string `sd:"secret"`
}

func (c *ptrCred) MarshalText() ([]byte, error) { return []byte(c.Pass), nil }

type wrapsCred struct {
	Name  string
	Creds []cred
}

func (wrapsCred) Validate() error { return nil }

type deepCred struct {
	Outer struct{ Inner map[string]*ptrCred }
}

func (deepCred) Validate() error { return nil }

type safeMarshaler struct {
	When time.Time // marshals itself, but holds no secret
	Name string
}

func (safeMarshaler) Validate() error { return nil }

func TestRegisterRefusesASecretInsideASelfMarshallingType(t *testing.T) {
	mustPanic(t, "value-receiver MarshalJSON", func() {
		Register(newReg(t), Def[wrapsCred, string]{Name: "t.cred", Summary: "x", Risk: RiskRead, Scope: ScopeRead, Execute: exec0[wrapsCred]})
	})
	mustPanic(t, "pointer-receiver MarshalText, nested in a map in a struct", func() {
		Register(newReg(t), Def[deepCred, string]{Name: "t.deepcred", Summary: "x", Risk: RiskRead, Scope: ScopeRead, Execute: exec0[deepCred]})
	})
	// A marshaler without secrets (time.Time) is fine.
	Register(newReg(t), Def[safeMarshaler, string]{Name: "t.safe", Summary: "x", Risk: RiskRead, Scope: ScopeRead, Execute: exec0[safeMarshaler]})
	// And the message says why.
	defer func() {
		if r := recover(); r == nil || !strings.Contains(fmt.Sprint(r), "encodes itself") {
			t.Errorf("panic message: %v", r)
		}
	}()
	Register(newReg(t), Def[wrapsCred, string]{Name: "t.cred2", Summary: "x", Risk: RiskRead, Scope: ScopeRead, Execute: exec0[wrapsCred]})
}
