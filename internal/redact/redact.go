// Package redact keeps secrets out of audit rows and logs. In M1 it knows the
// values of the current call's `sd:"secret"` input fields; M3b adds the secret
// store's known values.
package redact

import (
	"encoding/json"
	"fmt"
	"reflect"
	"sort"
	"strings"
)

// Placeholder replaces a secret.
const Placeholder = "[redacted]"

// Tag names used in `sd:"..."` struct tags.
const (
	TagSecret = "secret" // value is redacted everywhere it would be recorded
	TagAudit  = "audit"  // a free-form field explicitly safe to record
)

// Redactor replaces known secret values in free text.
type Redactor struct{ values []string }

// New returns a Redactor for the given secret values (empty ones are ignored).
func New(values ...string) *Redactor {
	r := &Redactor{}
	for _, v := range values {
		if v != "" {
			r.values = append(r.values, v)
		}
	}
	// Longest first, so a secret that contains another is replaced whole.
	sort.Slice(r.values, func(i, j int) bool { return len(r.values[i]) > len(r.values[j]) })
	return r
}

// ForInput returns a Redactor holding every string value found in in's
// `sd:"secret"` fields (strings, string slices, and string-valued maps).
func ForInput(in any) *Redactor { return New(secretValues(reflect.ValueOf(in))...) }

// String replaces every known secret value in s.
func (r *Redactor) String(s string) string {
	if r == nil {
		return s
	}
	for _, v := range r.values {
		s = strings.ReplaceAll(s, v, Placeholder)
	}
	return s
}

// Err returns err's text with secrets removed ("" for nil).
func (r *Redactor) Err(err error) string {
	if err == nil {
		return ""
	}
	return r.String(err.Error())
}

func secretValues(v reflect.Value) []string {
	for v.IsValid() && (v.Kind() == reflect.Pointer || v.Kind() == reflect.Interface) {
		if v.IsNil() {
			return nil
		}
		v = v.Elem()
	}
	if !v.IsValid() || v.Kind() != reflect.Struct {
		return nil
	}
	var out []string
	t := v.Type()
	for i := 0; i < t.NumField(); i++ {
		f := t.Field(i)
		if !f.IsExported() {
			continue
		}
		if f.Tag.Get("sd") == TagSecret {
			out = append(out, stringsIn(v.Field(i))...)
			continue
		}
		out = append(out, secretValues(v.Field(i))...) // nested structs
	}
	return out
}

func stringsIn(v reflect.Value) []string {
	for v.Kind() == reflect.Pointer || v.Kind() == reflect.Interface {
		if v.IsNil() {
			return nil
		}
		v = v.Elem()
	}
	switch v.Kind() {
	case reflect.String:
		return []string{v.String()}
	case reflect.Slice, reflect.Array:
		if v.Type().Elem().Kind() == reflect.Uint8 {
			return []string{string(v.Bytes())}
		}
		var out []string
		for i := 0; i < v.Len(); i++ {
			out = append(out, stringsIn(v.Index(i))...)
		}
		return out
	case reflect.Map:
		var out []string
		for _, k := range v.MapKeys() {
			out = append(out, stringsIn(v.MapIndex(k))...)
		}
		return out
	case reflect.Struct:
		var out []string
		for i := 0; i < v.NumField(); i++ {
			if v.Type().Field(i).IsExported() {
				out = append(out, stringsIn(v.Field(i))...)
			}
		}
		return out
	}
	return []string{fmt.Sprint(v.Interface())} // numbers and bools are still secrets when tagged
}

// ArgsJSON renders an action input for the audit row: secret fields become
// "[redacted]", everything else is recorded as the input's JSON would show it.
func ArgsJSON(in any) (string, error) {
	b, err := json.Marshal(scrub(reflect.ValueOf(in)))
	return string(b), err
}

func scrub(v reflect.Value) any {
	for v.IsValid() && (v.Kind() == reflect.Pointer || v.Kind() == reflect.Interface) {
		if v.IsNil() {
			return nil
		}
		v = v.Elem()
	}
	if !v.IsValid() {
		return nil
	}
	if v.Kind() != reflect.Struct {
		return v.Interface()
	}
	out := map[string]any{}
	t := v.Type()
	for i := 0; i < t.NumField(); i++ {
		f := t.Field(i)
		if !f.IsExported() {
			continue
		}
		name := f.Name
		if tag, _, _ := strings.Cut(f.Tag.Get("json"), ","); tag == "-" {
			continue
		} else if tag != "" {
			name = tag
		}
		if f.Tag.Get("sd") == TagSecret {
			out[name] = Placeholder
			continue
		}
		out[name] = scrub(v.Field(i))
	}
	return out
}
