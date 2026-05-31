package openapi

import (
	"encoding/json"
	"testing"
)

type child struct {
	X bool `json:"x"`
}

type sample struct {
	Name   string          `json:"name"`
	Count  int64           `json:"count,omitempty"`
	Mode   string          `json:"mode,omitempty" enum:"a,b" doc:"the mode"`
	Blob   []byte          `json:"blob,omitempty"`
	Child  child           `json:"child,omitempty"`
	Raw    json.RawMessage `json:"raw,omitempty"`
	Hidden string          `json:"-"`
	secret string
}

func TestSchemas(t *testing.T) {
	schemas := Schemas(sample{})

	samp, ok := schemas["sample"].(map[string]any)
	if !ok {
		t.Fatalf("no 'sample' schema: %v", schemas)
	}
	props := samp["properties"].(map[string]any)

	// Field name + type mapping.
	if props["name"].(map[string]any)["type"] != "string" {
		t.Errorf("name type = %v", props["name"])
	}
	if props["count"].(map[string]any)["type"] != "integer" {
		t.Errorf("count type = %v", props["count"])
	}
	// []byte -> base64 string.
	blob := props["blob"].(map[string]any)
	if blob["type"] != "string" || blob["format"] != "byte" {
		t.Errorf("blob schema = %v", blob)
	}
	// enum + doc tags.
	mode := props["mode"].(map[string]any)
	if mode["description"] != "the mode" {
		t.Errorf("mode description = %v", mode["description"])
	}
	if e, _ := mode["enum"].([]any); len(e) != 2 || e[0] != "a" || e[1] != "b" {
		t.Errorf("mode enum = %v", mode["enum"])
	}
	// json.RawMessage -> arbitrary JSON ({}).
	if raw := props["raw"].(map[string]any); len(raw) != 0 {
		t.Errorf("raw schema = %v, want {}", raw)
	}
	// Named struct field -> $ref, and the referenced component exists.
	if ref := props["child"].(map[string]any)["$ref"]; ref != "#/components/schemas/child" {
		t.Errorf("child ref = %v", ref)
	}
	if _, ok := schemas["child"]; !ok {
		t.Error("transitive 'child' schema missing")
	}
	// json:"-" and unexported fields excluded.
	if _, ok := props["Hidden"]; ok {
		t.Error("json:\"-\" field should be excluded")
	}
	if _, ok := props["secret"]; ok {
		t.Error("unexported field should be excluded")
	}
	// required = fields without omitempty.
	reqd, _ := samp["required"].([]string)
	if len(reqd) != 1 || reqd[0] != "name" {
		t.Errorf("required = %v, want [name]", reqd)
	}
}
