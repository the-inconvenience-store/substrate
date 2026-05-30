// Package projection rebuilds and advances the records current-state projection.
package projection

import "encoding/json"

// schemaDefaults holds a schema's top-level property defaults, pre-extracted from
// the schema JSON so a backfill run can parse the schema once (parseDefaults) and
// then apply the defaults cheaply to each record (apply). The raw JSON of each
// default is kept and unmarshalled per record so records never alias a shared
// reference value.
type schemaDefaults map[string]json.RawMessage

// parseDefaults extracts the top-level properties[*].default entries from a JSON
// schema. Returns nil when the schema does not parse or declares no defaults
// (ranging over a nil map is a safe no-op in apply). Only top-level
// properties[*].default are considered (v0 scope).
func parseDefaults(schemaRaw []byte) schemaDefaults {
	var doc struct {
		Properties map[string]struct {
			Default json.RawMessage `json:"default"`
		} `json:"properties"`
	}
	if err := json.Unmarshal(schemaRaw, &doc); err != nil || len(doc.Properties) == 0 {
		return nil
	}
	var d schemaDefaults
	for name, prop := range doc.Properties {
		if len(prop.Default) == 0 {
			continue
		}
		if d == nil {
			d = make(schemaDefaults, len(doc.Properties))
		}
		d[name] = prop.Default
	}
	return d
}

// apply returns data with the defaults filled in for any missing keys, plus
// whether anything changed. It copies-on-write: the input map is never mutated.
func (d schemaDefaults) apply(data map[string]any) (map[string]any, bool) {
	out := data
	changed := false
	for name, raw := range d {
		if _, present := data[name]; present {
			continue
		}
		var v any
		if err := json.Unmarshal(raw, &v); err != nil {
			continue
		}
		if !changed {
			out = make(map[string]any, len(data)+1)
			for k, val := range data {
				out[k] = val
			}
			changed = true
		}
		out[name] = v
	}
	return out, changed
}

// applyDefaults parses schemaRaw and applies its top-level defaults to data in a
// single call — equivalent to parseDefaults(schemaRaw).apply(data). When advancing
// many records under one schema, prefer parseDefaults once + apply per record so
// the schema is not re-parsed for every record.
func applyDefaults(schemaRaw []byte, data map[string]any) (map[string]any, bool) {
	return parseDefaults(schemaRaw).apply(data)
}
