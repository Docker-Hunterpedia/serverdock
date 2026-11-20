// Package redact keeps secrets out of audit rows and logs. In M1 it knows the
// values of the current call's `sd:"secret"` input fields; M3b adds the secret
// store's known values.
package redact

import (
	"encoding"
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

var (
	marshalerType     = reflect.TypeFor[json.Marshaler]()
	textMarshalerType = reflect.TypeFor[encoding.TextMarshaler]()
)

// maxDepth bounds the walk (cyclic pointers, absurdly nested input).
const maxDepth = 32

// secretValues collects every value held by an sd:"secret" field anywhere in v:
// through pointers and interfaces, slices and arrays, map values, nested and
// embedded structs (exported or not).
func secretValues(v reflect.Value) []string { return walkSecrets(v, 0) }

func walkSecrets(v reflect.Value, depth int) []string {
	if depth > maxDepth {
		return nil
	}
	for v.IsValid() && (v.Kind() == reflect.Pointer || v.Kind() == reflect.Interface) {
		if v.IsNil() {
			return nil
		}
		v = v.Elem()
	}
	if !v.IsValid() {
		return nil
	}
	var out []string
	switch v.Kind() {
	case reflect.Struct:
		t := v.Type()
		for i := 0; i < t.NumField(); i++ {
			f := t.Field(i)
			if f.Tag.Get("sd") == TagSecret {
				out = append(out, stringsIn(v.Field(i), 0)...)
				continue
			}
			out = append(out, walkSecrets(v.Field(i), depth+1)...)
		}
	case reflect.Slice, reflect.Array:
		if v.Type().Elem().Kind() == reflect.Uint8 {
			return nil
		}
		for i := 0; i < v.Len(); i++ {
			out = append(out, walkSecrets(v.Index(i), depth+1)...)
		}
	case reflect.Map:
		it := v.MapRange()
		for it.Next() {
			out = append(out, walkSecrets(it.Value(), depth+1)...)
		}
	}
	return out
}

// stringsIn returns every scalar under v as text (a tagged secret may be a
// string, a number, a list, a map or a whole struct).
func stringsIn(v reflect.Value, depth int) []string {
	if depth > maxDepth {
		return nil
	}
	for v.IsValid() && (v.Kind() == reflect.Pointer || v.Kind() == reflect.Interface) {
		if v.IsNil() {
			return nil
		}
		v = v.Elem()
	}
	if !v.IsValid() {
		return nil
	}
	switch v.Kind() {
	case reflect.Slice, reflect.Array:
		if v.Type().Elem().Kind() == reflect.Uint8 {
			return []string{string(v.Bytes())}
		}
		var out []string
		for i := 0; i < v.Len(); i++ {
			out = append(out, stringsIn(v.Index(i), depth+1)...)
		}
		return out
	case reflect.Map:
		var out []string
		it := v.MapRange()
		for it.Next() {
			out = append(out, stringsIn(it.Value(), depth+1)...)
		}
		return out
	case reflect.Struct:
		var out []string
		for i := 0; i < v.NumField(); i++ {
			out = append(out, stringsIn(v.Field(i), depth+1)...)
		}
		return out
	}
	return []string{fmt.Sprint(leaf(v))}
}

// leaf returns a scalar's Go value without Value.Interface, which panics for
// values reached through unexported fields.
func leaf(v reflect.Value) any {
	switch v.Kind() {
	case reflect.String:
		return v.String()
	case reflect.Bool:
		return v.Bool()
	case reflect.Int, reflect.Int8, reflect.Int16, reflect.Int32, reflect.Int64:
		return v.Int()
	case reflect.Uint, reflect.Uint8, reflect.Uint16, reflect.Uint32, reflect.Uint64, reflect.Uintptr:
		return v.Uint()
	case reflect.Float32, reflect.Float64:
		return v.Float()
	}
	return nil
}

// ArgsJSON renders an action input for the audit row: secret fields become
// "[redacted]", everything else is recorded as the input's JSON would show it.
func ArgsJSON(in any) (string, error) {
	b, err := json.Marshal(scrub(reflect.ValueOf(in)))
	return string(b), err
}

func scrub(v reflect.Value) any { return scrubAt(v, 0) }

func scrubAt(v reflect.Value, depth int) any {
	if depth > maxDepth {
		return nil
	}
	for v.IsValid() && (v.Kind() == reflect.Pointer || v.Kind() == reflect.Interface) {
		if v.IsNil() {
			return nil
		}
		v = v.Elem()
	}
	if !v.IsValid() {
		return nil
	}
	switch v.Kind() {
	case reflect.Struct:
		// A type that encodes itself (time.Time, ...) is opaque: record it as it
		// would be marshalled.
		if v.CanInterface() && (v.Type().Implements(marshalerType) || v.Type().Implements(textMarshalerType)) {
			return v.Interface()
		}
		out := map[string]any{}
		scrubStruct(out, v, depth)
		return out
	case reflect.Slice, reflect.Array:
		if v.Type().Elem().Kind() == reflect.Uint8 {
			return v.Bytes() // encoded as base64 by encoding/json, like the input itself
		}
		if v.Kind() == reflect.Slice && v.IsNil() {
			return nil
		}
		out := make([]any, v.Len())
		for i := range out {
			out[i] = scrubAt(v.Index(i), depth+1)
		}
		return out
	case reflect.Map:
		if v.IsNil() {
			return nil
		}
		out := map[string]any{}
		it := v.MapRange()
		for it.Next() {
			out[fmt.Sprint(leaf(it.Key()))] = scrubAt(it.Value(), depth+1)
		}
		return out
	}
	return leaf(v)
}

// scrubStruct adds v's fields to out, flattening embedded structs the way
// encoding/json does (an embedded struct without a json name contributes its
// fields directly, exported or not).
func scrubStruct(out map[string]any, v reflect.Value, depth int) {
	t := v.Type()
	for i := 0; i < t.NumField(); i++ {
		f := t.Field(i)
		tag, _, _ := strings.Cut(f.Tag.Get("json"), ",")
		if tag == "-" {
			continue
		}
		if f.Anonymous && tag == "" && f.Tag.Get("sd") != TagSecret {
			fv := v.Field(i)
			for fv.Kind() == reflect.Pointer && !fv.IsNil() {
				fv = fv.Elem()
			}
			if fv.Kind() == reflect.Struct {
				scrubStruct(out, fv, depth+1)
				continue
			}
		}
		if !f.IsExported() {
			continue
		}
		name := f.Name
		if tag != "" {
			name = tag
		}
		if f.Tag.Get("sd") == TagSecret {
			out[name] = Placeholder
			continue
		}
		out[name] = scrubAt(v.Field(i), depth+1)
	}
}
