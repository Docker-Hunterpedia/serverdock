package actions

import (
	"encoding/json"
	"fmt"
	"reflect"
	"strings"
	"time"
)

// SchemaOf reflects t into a JSON Schema (2020-12 subset, as OpenAPI 3.1 uses
// it). It is run on every registered action's input and output type, so a type
// the reflector does not understand fails a test, not a request.
//
//   - struct: an object with properties from the json tags, required for the
//     fields that are neither omitempty nor pointers, additionalProperties false
//     (the registry decodes strictly); anonymous struct fields are inlined;
//   - string, bool, integers (minimum 0 for unsigned), floats, slices, arrays and
//     pointers (type [T, "null"]); []byte is a base64 string; time.Time is a
//     date-time string; string-keyed maps are additionalProperties; interfaces and
//     json.RawMessage are {};
//   - a named string type with Enum() []string gets an enum;
//   - the sd:"secret" tag adds writeOnly and format password, and doc:"..." a
//     description;
//   - anything else (channels, funcs, non-string map keys, complex numbers) is an
//     error.
func SchemaOf(t reflect.Type) (map[string]any, error) { return schemaOf(t, 0) }

type enumer interface{ Enum() []string }

// The reflected types are computed in functions, not package variables: nothing here runs at load.
func timeType() reflect.Type { return reflect.TypeFor[time.Time]() }
func rawType() reflect.Type  { return reflect.TypeFor[json.RawMessage]() }

func schemaOf(t reflect.Type, depth int) (map[string]any, error) {
	if depth > 16 {
		return nil, fmt.Errorf("type %s nests too deeply (or is recursive)", t)
	}
	switch {
	case t == rawType():
		return map[string]any{}, nil
	case t == timeType():
		return map[string]any{"type": "string", "format": "date-time"}, nil
	case t.Kind() == reflect.Pointer:
		inner, err := schemaOf(t.Elem(), depth+1)
		if err != nil {
			return nil, err
		}
		return nullable(inner), nil
	}
	if e, ok := reflect.Zero(t).Interface().(enumer); ok && t.Kind() == reflect.String {
		vals := e.Enum()
		list := make([]any, len(vals))
		for i, v := range vals {
			list[i] = v
		}
		return map[string]any{"type": "string", "enum": list}, nil
	}
	switch t.Kind() {
	case reflect.String:
		return map[string]any{"type": "string"}, nil
	case reflect.Bool:
		return map[string]any{"type": "boolean"}, nil
	case reflect.Int, reflect.Int8, reflect.Int16, reflect.Int32, reflect.Int64:
		return map[string]any{"type": "integer"}, nil
	case reflect.Uint, reflect.Uint8, reflect.Uint16, reflect.Uint32, reflect.Uint64:
		return map[string]any{"type": "integer", "minimum": 0}, nil
	case reflect.Float32, reflect.Float64:
		return map[string]any{"type": "number"}, nil
	case reflect.Interface:
		return map[string]any{}, nil
	case reflect.Slice, reflect.Array:
		if t.Elem().Kind() == reflect.Uint8 && t.Kind() == reflect.Slice {
			return map[string]any{"type": "string", "contentEncoding": "base64"}, nil
		}
		item, err := schemaOf(t.Elem(), depth+1)
		if err != nil {
			return nil, err
		}
		out := map[string]any{"type": "array", "items": item}
		if t.Kind() == reflect.Array {
			out["minItems"], out["maxItems"] = t.Len(), t.Len()
		}
		return out, nil
	case reflect.Map:
		if t.Key().Kind() != reflect.String {
			return nil, fmt.Errorf("map with non-string key %s", t.Key())
		}
		v, err := schemaOf(t.Elem(), depth+1)
		if err != nil {
			return nil, err
		}
		return map[string]any{"type": "object", "additionalProperties": v}, nil
	case reflect.Struct:
		return structSchema(t, depth)
	}
	return nil, fmt.Errorf("unsupported type %s (%s)", t, t.Kind())
}

func nullable(s map[string]any) map[string]any {
	if ty, ok := s["type"].(string); ok {
		out := map[string]any{}
		for k, v := range s {
			out[k] = v
		}
		out["type"] = []any{ty, "null"}
		return out
	}
	return map[string]any{"oneOf": []any{s, map[string]any{"type": "null"}}}
}

func structSchema(t reflect.Type, depth int) (map[string]any, error) {
	props := map[string]any{}
	var required []any
	var walk func(t reflect.Type) error
	walk = func(t reflect.Type) error {
		for i := 0; i < t.NumField(); i++ {
			f := t.Field(i)
			tag := f.Tag.Get("json")
			if tag == "-" {
				continue
			}
			name, opts, _ := strings.Cut(tag, ",")
			if f.Anonymous && name == "" && f.Type.Kind() == reflect.Struct {
				if err := walk(f.Type); err != nil {
					return err
				}
				continue
			}
			if !f.IsExported() {
				continue
			}
			if name == "" {
				name = f.Name
			}
			fs, err := schemaOf(f.Type, depth+1)
			if err != nil {
				return fmt.Errorf("field %s.%s: %w", t.Name(), f.Name, err)
			}
			fs = copyMap(fs)
			if f.Tag.Get("sd") == "secret" {
				fs["writeOnly"], fs["format"] = true, "password"
			}
			if d := f.Tag.Get("doc"); d != "" {
				fs["description"] = d
			}
			props[name] = fs
			if f.Type.Kind() != reflect.Pointer && !strings.Contains(","+opts+",", ",omitempty,") {
				required = append(required, name)
			}
		}
		return nil
	}
	if err := walk(t); err != nil {
		return nil, err
	}
	out := map[string]any{"type": "object", "properties": props, "additionalProperties": false}
	if len(required) > 0 {
		out["required"] = required
	}
	return out, nil
}

func copyMap(m map[string]any) map[string]any {
	out := make(map[string]any, len(m)+2)
	for k, v := range m {
		out[k] = v
	}
	return out
}
