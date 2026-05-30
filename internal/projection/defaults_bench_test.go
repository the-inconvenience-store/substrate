package projection

import "testing"

var benchDefaultsSchema = []byte(`{
  "type": "object",
  "properties": {
    "status":   {"type": "string", "default": "active"},
    "priority": {"type": "integer", "default": 0},
    "tags":     {"type": "array", "default": []},
    "meta":     {"type": "object", "default": {"v": 1}},
    "name":     {"type": "string"}
  }
}`)

func BenchmarkApplyDefaults(b *testing.B) {
	// data is missing every defaulted key, forcing the copy-on-write path.
	data := map[string]any{"name": "widget"}
	b.ReportAllocs()
	b.ResetTimer()
	var changed bool
	for i := 0; i < b.N; i++ {
		_, changed = applyDefaults(benchDefaultsSchema, data)
	}
	_ = changed
}
