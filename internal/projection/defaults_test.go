package projection

import "testing"

func TestApplyDefaults(t *testing.T) {
	schema := []byte(`{"type":"object","properties":{"a":{"type":"string"},"b":{"type":"string","default":"x"},"c":{"type":"integer","default":7}}}`)

	out, changed := applyDefaults(schema, map[string]any{"a": "hi"})
	if !changed {
		t.Fatal("expected changed=true")
	}
	if out["b"] != "x" {
		t.Fatalf("b = %v, want x", out["b"])
	}
	if got, ok := out["c"].(float64); !ok || got != 7 {
		t.Fatalf("c = %v, want 7", out["c"])
	}
	if out["a"] != "hi" {
		t.Fatalf("a mutated: %v", out["a"])
	}

	out2, _ := applyDefaults(schema, map[string]any{"b": "keep"})
	if out2["b"] != "keep" {
		t.Fatalf("b overwritten: %v", out2["b"])
	}

	if _, changed := applyDefaults([]byte(`{"properties":{"a":{"type":"string"}}}`), map[string]any{"a": "x"}); changed {
		t.Fatal("expected changed=false")
	}

	if _, changed := applyDefaults([]byte(`not json`), map[string]any{"a": 1}); changed {
		t.Fatal("expected changed=false for bad schema")
	}
}
