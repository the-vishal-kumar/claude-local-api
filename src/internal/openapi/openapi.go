// Package openapi generates OpenAPI component schemas by reflecting over Go
// types, so the API documentation is derived from the code rather than
// hand-maintained. It uses only the standard library.
//
// Schema shape is inferred from json struct tags and Go kinds. A few things
// reflection cannot know are read from optional struct tags on the field:
//
//	enum:"a,b,c"     -> schema enum
//	doc:"..."        -> schema description
//	example:"..."    -> schema example
//	format:"..."     -> schema format
//
// Required fields are those whose json tag lacks ",omitempty".
package openapi

import (
	"encoding/json"
	"reflect"
	"strings"
)

var rawMessageType = reflect.TypeOf(json.RawMessage(nil))

// Schemas reflects over each sample value's type and returns an OpenAPI
// components.schemas map keyed by type name. Named struct types referenced by
// the samples (fields, slice elements) are included transitively as $refs.
func Schemas(samples ...any) map[string]any {
	g := &gen{defs: map[string]map[string]any{}}
	for _, s := range samples {
		t := deref(reflect.TypeOf(s))
		if t != nil && t.Kind() == reflect.Struct && t.Name() != "" {
			g.ensure(t.Name(), t)
		}
	}
	out := make(map[string]any, len(g.defs))
	for k, v := range g.defs {
		out[k] = v
	}
	return out
}

type gen struct{ defs map[string]map[string]any }

// ensure generates the named component once (placeholder first to break cycles).
func (g *gen) ensure(name string, t reflect.Type) {
	if _, ok := g.defs[name]; ok {
		return
	}
	g.defs[name] = map[string]any{}
	g.defs[name] = g.object(t)
}

// object builds an object schema for a struct, promoting embedded structs.
func (g *gen) object(t reflect.Type) map[string]any {
	props := map[string]any{}
	var required []string

	var walk func(rt reflect.Type)
	walk = func(rt reflect.Type) {
		for i := 0; i < rt.NumField(); i++ {
			f := rt.Field(i)
			// Promote embedded struct fields (e.g. an embedded *Result) when
			// they have no explicit json name — matches Go's JSON promotion.
			if f.Anonymous && f.Tag.Get("json") == "" {
				if ft := deref(f.Type); ft != nil && ft.Kind() == reflect.Struct {
					walk(ft)
					continue
				}
			}
			if !f.IsExported() {
				continue
			}
			name, omitempty := jsonName(f)
			if name == "-" {
				continue
			}
			if name == "" {
				name = f.Name
			}
			sc := g.schemaFor(f.Type)
			applyTags(sc, f)
			props[name] = sc
			if !omitempty {
				required = append(required, name)
			}
		}
	}
	walk(t)

	obj := map[string]any{"type": "object", "properties": props}
	if len(required) > 0 {
		obj["required"] = required
	}
	return obj
}

// schemaFor maps a Go type to an OpenAPI schema (or a $ref for named structs).
func (g *gen) schemaFor(t reflect.Type) map[string]any {
	t = deref(t)
	if t == nil {
		return map[string]any{}
	}
	if t == rawMessageType {
		return map[string]any{} // arbitrary JSON
	}
	switch t.Kind() {
	case reflect.String:
		return map[string]any{"type": "string"}
	case reflect.Bool:
		return map[string]any{"type": "boolean"}
	case reflect.Int, reflect.Int8, reflect.Int16, reflect.Int32, reflect.Int64,
		reflect.Uint, reflect.Uint8, reflect.Uint16, reflect.Uint32, reflect.Uint64:
		return map[string]any{"type": "integer"}
	case reflect.Float32, reflect.Float64:
		return map[string]any{"type": "number"}
	case reflect.Slice, reflect.Array:
		if t.Elem().Kind() == reflect.Uint8 {
			return map[string]any{"type": "string", "format": "byte"} // []byte -> base64
		}
		return map[string]any{"type": "array", "items": g.schemaFor(t.Elem())}
	case reflect.Map:
		return map[string]any{"type": "object", "additionalProperties": g.schemaFor(t.Elem())}
	case reflect.Struct:
		if name := t.Name(); name != "" {
			g.ensure(name, t)
			return map[string]any{"$ref": "#/components/schemas/" + name}
		}
		return g.object(t)
	default:
		return map[string]any{}
	}
}

// applyTags merges optional doc/enum/example/format tags into a scalar schema.
// $ref schemas are left untouched (OpenAPI 3.0 forbids siblings of $ref).
func applyTags(sc map[string]any, f reflect.StructField) {
	if _, isRef := sc["$ref"]; isRef {
		return
	}
	if v := f.Tag.Get("doc"); v != "" {
		sc["description"] = v
	}
	if v := f.Tag.Get("format"); v != "" {
		sc["format"] = v
	}
	if v := f.Tag.Get("example"); v != "" {
		sc["example"] = v
	}
	if v := f.Tag.Get("enum"); v != "" {
		parts := strings.Split(v, ",")
		vals := make([]any, len(parts))
		for i, p := range parts {
			vals[i] = p
		}
		sc["enum"] = vals
	}
}

// jsonName returns the json field name and whether it is omitempty.
func jsonName(f reflect.StructField) (string, bool) {
	parts := strings.Split(f.Tag.Get("json"), ",")
	omit := false
	for _, p := range parts[1:] {
		if p == "omitempty" {
			omit = true
		}
	}
	return parts[0], omit
}

func deref(t reflect.Type) reflect.Type {
	for t != nil && t.Kind() == reflect.Ptr {
		t = t.Elem()
	}
	return t
}
