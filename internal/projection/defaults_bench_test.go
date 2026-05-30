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

// BenchmarkApplyDefaults measures the per-record default-application hot path as
// the backfiller runs it: the schema's defaults are parsed once (here, in setup),
// then applied to each record. This mirrors Backfiller.Run, which parses defaults
// once per run rather than per record. (Before that change the per-record op also
// re-parsed the schema; the committed baseline reflects that older, larger cost.)
func BenchmarkApplyDefaults(b *testing.B) {
	dflts := parseDefaults(benchDefaultsSchema)
	// data is missing every defaulted key, forcing the copy-on-write path.
	data := map[string]any{"name": "widget"}
	b.ReportAllocs()
	b.ResetTimer()
	var changed bool
	for i := 0; i < b.N; i++ {
		_, changed = dflts.apply(data)
	}
	_ = changed
}

// BenchmarkParseDefaults measures the one-time, per-run schema-defaults parse that
// BenchmarkApplyDefaults now hoists out of the per-record loop.
func BenchmarkParseDefaults(b *testing.B) {
	b.ReportAllocs()
	b.ResetTimer()
	var d schemaDefaults
	for i := 0; i < b.N; i++ {
		d = parseDefaults(benchDefaultsSchema)
	}
	_ = d
}
