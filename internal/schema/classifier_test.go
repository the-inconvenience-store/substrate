package schema

import "testing"

func obj(props map[string]any, required ...string) map[string]any {
	m := map[string]any{"type": "object", "properties": props}
	if len(required) > 0 {
		r := make([]any, len(required))
		for i, s := range required {
			r[i] = s
		}
		m["required"] = r
	}
	return m
}

func hasBreaking(cs []Change) bool {
	for _, c := range cs {
		if c.Breaking {
			return true
		}
	}
	return false
}

func TestClassify_AddOptionalField_NotBreaking(t *testing.T) {
	cur := obj(map[string]any{"a": map[string]any{"type": "string"}}, "a")
	cand := obj(map[string]any{
		"a": map[string]any{"type": "string"},
		"b": map[string]any{"type": "string"},
	}, "a")
	if hasBreaking(Classify(cur, cand)) {
		t.Fatal("adding an optional field must not be breaking")
	}
}

func TestClassify_RemoveRequiredField_Breaking(t *testing.T) {
	cur := obj(map[string]any{
		"a": map[string]any{"type": "string"},
		"b": map[string]any{"type": "string"},
	}, "a", "b")
	cand := obj(map[string]any{"a": map[string]any{"type": "string"}}, "a")
	if !hasBreaking(Classify(cur, cand)) {
		t.Fatal("removing a required field must be breaking")
	}
}

func TestClassify_AddRequired_Breaking(t *testing.T) {
	cur := obj(map[string]any{"a": map[string]any{"type": "string"}}, "a")
	cand := obj(map[string]any{
		"a": map[string]any{"type": "string"},
		"b": map[string]any{"type": "string"},
	}, "a", "b")
	if !hasBreaking(Classify(cur, cand)) {
		t.Fatal("promoting a field to required must be breaking")
	}
}

func TestClassify_NarrowType_Breaking(t *testing.T) {
	cur := obj(map[string]any{"a": map[string]any{"type": "number"}}, "a")
	cand := obj(map[string]any{"a": map[string]any{"type": "integer"}}, "a")
	if !hasBreaking(Classify(cur, cand)) {
		t.Fatal("number->integer must be breaking")
	}
}

func TestClassify_WidenType_NotBreaking(t *testing.T) {
	cur := obj(map[string]any{"a": map[string]any{"type": "integer"}}, "a")
	cand := obj(map[string]any{"a": map[string]any{"type": "number"}}, "a")
	if hasBreaking(Classify(cur, cand)) {
		t.Fatal("integer->number must not be breaking")
	}
}

func TestClassify_AddEnumValue_NotBreaking(t *testing.T) {
	cur := obj(map[string]any{"a": map[string]any{"enum": []any{"x"}}}, "a")
	cand := obj(map[string]any{"a": map[string]any{"enum": []any{"x", "y"}}}, "a")
	if hasBreaking(Classify(cur, cand)) {
		t.Fatal("adding an enum value must not be breaking")
	}
}

func TestClassify_RemoveEnumValue_Breaking(t *testing.T) {
	cur := obj(map[string]any{"a": map[string]any{"enum": []any{"x", "y"}}}, "a")
	cand := obj(map[string]any{"a": map[string]any{"enum": []any{"x"}}}, "a")
	if !hasBreaking(Classify(cur, cand)) {
		t.Fatal("removing an enum value must be breaking")
	}
}

func TestClassify_NestedRemoveRequired_Breaking(t *testing.T) {
	// "x" is removed from both properties and required in the nested object.
	// Removing a required property entirely (case 2) must be breaking.
	cur := obj(map[string]any{
		"a": obj(map[string]any{"x": map[string]any{"type": "string"}}, "x"),
	}, "a")
	cand := obj(map[string]any{
		"a": obj(map[string]any{} /* x removed from properties */),
	}, "a")
	if !hasBreaking(Classify(cur, cand)) {
		t.Fatal("nested removal of a required property must be breaking")
	}
}

func TestClassify_RelaxRequiredKeepingProperty_NotBreaking(t *testing.T) {
	// "b" stays in properties but is removed from required — a constraint
	// relaxation. Data that previously omitted "b" now validates; data that
	// included "b" still validates. This must NOT be breaking.
	cur := obj(map[string]any{
		"a": map[string]any{"type": "string"},
		"b": map[string]any{"type": "string"},
	}, "a", "b")
	cand := obj(map[string]any{
		"a": map[string]any{"type": "string"},
		"b": map[string]any{"type": "string"},
	}, "a") // b kept as property, just no longer required
	if hasBreaking(Classify(cur, cand)) {
		t.Fatal("removing a field from required while keeping the property definition must not be breaking")
	}
}

func TestClassify_AmbiguousConstruct_Breaking(t *testing.T) {
	cur := obj(map[string]any{"a": map[string]any{"type": "string"}}, "a")
	cand := obj(map[string]any{"a": map[string]any{"$ref": "#/$defs/Foo"}}, "a")
	if !hasBreaking(Classify(cur, cand)) {
		t.Fatal("switching to an unanalyzable construct must be conservatively breaking")
	}
}
