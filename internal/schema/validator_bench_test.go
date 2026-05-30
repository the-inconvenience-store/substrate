package schema

import (
	"context"
	"testing"

	"github.com/google/uuid"
)

// stubResolver satisfies the unexported activeResolver interface with a fixed
// in-memory schema, so the validator never touches a database.
type stubResolver struct{ s ActiveSchema }

func (r stubResolver) GetActive(ctx context.Context, col uuid.UUID) (ActiveSchema, error) {
	return r.s, nil
}

var benchSchemaRaw = []byte(`{
  "type": "object",
  "required": ["name", "age"],
  "properties": {
    "name":   {"type": "string", "minLength": 1, "maxLength": 120},
    "age":    {"type": "integer", "minimum": 0, "maximum": 130},
    "email":  {"type": "string", "format": "email"},
    "active": {"type": "boolean"},
    "score":  {"type": "number"},
    "tags":   {"type": "array", "items": {"type": "string"}}
  }
}`)

func benchPayload() map[string]any {
	return map[string]any{
		"name": "Ada Lovelace", "age": 36, "email": "ada@example.com",
		"active": true, "score": 99.5, "tags": []any{"x", "y", "z"},
	}
}

func BenchmarkValidateWrite(b *testing.B) {
	v := NewValidator(stubResolver{s: ActiveSchema{Version: 1, Raw: benchSchemaRaw}})
	col := uuid.New()
	data := benchPayload()
	// Warm the compiled-schema cache so we measure the steady-state validate path.
	if _, err := v.ValidateWrite(context.Background(), col, data); err != nil {
		b.Fatalf("warmup: %v", err)
	}
	b.ReportAllocs()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		if _, err := v.ValidateWrite(context.Background(), col, data); err != nil {
			b.Fatal(err)
		}
	}
}

func BenchmarkClassify(b *testing.B) {
	current := map[string]any{
		"type":     "object",
		"required": []any{"name"},
		"properties": map[string]any{
			"name": map[string]any{"type": "string", "maxLength": float64(120)},
			"age":  map[string]any{"type": "integer", "minimum": float64(0)},
		},
	}
	candidate := map[string]any{
		"type":     "object",
		"required": []any{"name", "age"}, // add-required => breaking
		"properties": map[string]any{
			"name":  map[string]any{"type": "string", "maxLength": float64(80)}, // tighten => breaking
			"age":   map[string]any{"type": "integer", "minimum": float64(0)},
			"email": map[string]any{"type": "string"}, // new optional => non-breaking
		},
	}
	b.ReportAllocs()
	b.ResetTimer()
	var sink int
	for i := 0; i < b.N; i++ {
		sink = len(Classify(current, candidate))
	}
	_ = sink
}
