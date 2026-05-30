// Package projection rebuilds and advances the records current-state projection.
package projection

import "encoding/json"

// applyDefaults returns data with the schema's top-level defaults filled in for any
// missing keys, plus whether anything changed. It copies-on-write: the input map is
// never mutated. Only top-level `properties[*].default` are applied (v0 scope).
func applyDefaults(schemaRaw []byte, data map[string]any) (map[string]any, bool) {
	var doc struct {
		Properties map[string]struct {
			Default json.RawMessage `json:"default"`
		} `json:"properties"`
	}
	if err := json.Unmarshal(schemaRaw, &doc); err != nil || len(doc.Properties) == 0 {
		return data, false
	}
	out := data
	changed := false
	for name, prop := range doc.Properties {
		if len(prop.Default) == 0 {
			continue
		}
		if _, present := data[name]; present {
			continue
		}
		var v any
		if err := json.Unmarshal(prop.Default, &v); err != nil {
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
