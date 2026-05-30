// Package schema provides the schema registry, JSON Schema validation, and the
// compatibility classifier for Substrate's typed collections.
package schema

import "fmt"

// Change is one structural difference between two schemas.
type Change struct {
	Path     string `json:"path"`
	Kind     string `json:"kind"`
	Breaking bool   `json:"breaking"`
}

// Classify returns the structural changes from current to candidate. A change is
// Breaking if migrating existing data validated by current could fail candidate.
// Constructs it cannot confidently analyze are reported as breaking (conservative).
func Classify(current, candidate map[string]any) []Change {
	var out []Change
	classifyNode(current, candidate, "$", &out)
	return out
}

func classifyNode(cur, cand map[string]any, path string, out *[]Change) {
	// Unanalyzable combinators / refs on either side -> conservative breaking.
	for _, k := range []string{"$ref", "anyOf", "oneOf", "allOf", "not", "patternProperties"} {
		if _, ok := cur[k]; ok {
			if !equalJSON(cur[k], cand[k]) {
				*out = append(*out, Change{Path: path, Kind: "unanalyzable:" + k, Breaking: true})
				return
			}
		}
		if _, ok := cand[k]; ok {
			if !equalJSON(cur[k], cand[k]) {
				*out = append(*out, Change{Path: path, Kind: "unanalyzable:" + k, Breaking: true})
				return
			}
		}
	}

	classifyType(cur["type"], cand["type"], path, out)
	classifyEnum(cur["enum"], cand["enum"], path, out)
	classifyBounds(cur, cand, path, out)
	classifyRequired(cur["required"], cand["required"], path, out)
	classifyProperties(cur, cand, path, out)
	classifyItems(cur["items"], cand["items"], path, out)
}

func classifyType(cur, cand any, path string, out *[]Change) {
	if cur == nil || cand == nil || equalJSON(cur, cand) {
		return
	}
	curSet := typeSet(cur)
	candSet := typeSet(cand)
	// Narrowing: some accepted type is no longer accepted (and not a safe widen).
	for t := range curSet {
		if !candSet[t] && !(t == "integer" && candSet["number"]) {
			*out = append(*out, Change{Path: path + ".type", Kind: "narrow-type", Breaking: true})
			return
		}
	}
	*out = append(*out, Change{Path: path + ".type", Kind: "widen-type", Breaking: false})
}

func classifyEnum(cur, cand any, path string, out *[]Change) {
	if cur == nil {
		return // no prior constraint -> candidate adding enum is a tighten only if cur unconstrained
	}
	curVals, ok1 := cur.([]any)
	candVals, ok2 := cand.([]any)
	if !ok1 {
		return
	}
	if !ok2 {
		// enum removed entirely (relaxed) -> not breaking
		*out = append(*out, Change{Path: path + ".enum", Kind: "remove-enum-constraint", Breaking: false})
		return
	}
	candSet := map[string]bool{}
	for _, v := range candVals {
		candSet[fmt.Sprintf("%v", v)] = true
	}
	for _, v := range curVals {
		if !candSet[fmt.Sprintf("%v", v)] {
			*out = append(*out, Change{Path: path + ".enum", Kind: "remove-enum-value", Breaking: true})
			return
		}
	}
	if len(candVals) > len(curVals) {
		*out = append(*out, Change{Path: path + ".enum", Kind: "add-enum-value", Breaking: false})
	}
}

func classifyBounds(cur, cand map[string]any, path string, out *[]Change) {
	// Tightening numeric/length bounds is breaking; loosening or removing is not.
	type bound struct {
		key     string
		tighten func(c, n float64) bool // true if n is stricter than c
	}
	bounds := []bound{
		{"minimum", func(c, n float64) bool { return n > c }},
		{"minLength", func(c, n float64) bool { return n > c }},
		{"minItems", func(c, n float64) bool { return n > c }},
		{"maximum", func(c, n float64) bool { return n < c }},
		{"maxLength", func(c, n float64) bool { return n < c }},
		{"maxItems", func(c, n float64) bool { return n < c }},
	}
	for _, b := range bounds {
		cv, cok := asFloat(cur[b.key])
		nv, nok := asFloat(cand[b.key])
		switch {
		case nok && !cok:
			*out = append(*out, Change{Path: path + "." + b.key, Kind: "add-bound", Breaking: true})
		case cok && nok && b.tighten(cv, nv):
			*out = append(*out, Change{Path: path + "." + b.key, Kind: "tighten-bound", Breaking: true})
		}
	}
	// pattern / format: any change is conservatively breaking.
	for _, k := range []string{"pattern", "format"} {
		if _, ok := cand[k]; ok && !equalJSON(cur[k], cand[k]) {
			*out = append(*out, Change{Path: path + "." + k, Kind: "change-" + k, Breaking: true})
		}
	}
}

func classifyRequired(cur, cand any, path string, out *[]Change) {
	curSet := strSet(cur)
	candSet := strSet(cand)
	for f := range candSet {
		if !curSet[f] {
			*out = append(*out, Change{Path: path + ".required." + f, Kind: "add-required", Breaking: true})
		}
	}
	for f := range curSet {
		if !candSet[f] {
			*out = append(*out, Change{Path: path + ".required." + f, Kind: "remove-required", Breaking: true})
		}
	}
}

func classifyProperties(cur, cand map[string]any, path string, out *[]Change) {
	curProps, _ := cur["properties"].(map[string]any)
	candProps, _ := cand["properties"].(map[string]any)
	candRequired := strSet(cand["required"])
	for name, cp := range curProps {
		np, ok := candProps[name]
		if !ok {
			// property dropped from schema: only breaking if it was required (caught by required diff);
			// dropping an optional property definition is not breaking.
			continue
		}
		cpm, _ := cp.(map[string]any)
		npm, _ := np.(map[string]any)
		if cpm != nil && npm != nil {
			classifyNode(cpm, npm, path+"."+name, out)
		}
	}
	for name := range candProps {
		if _, ok := curProps[name]; !ok && candRequired[name] {
			*out = append(*out, Change{Path: path + "." + name, Kind: "add-required-property", Breaking: true})
		}
	}
}

func classifyItems(cur, cand any, path string, out *[]Change) {
	cm, ok1 := cur.(map[string]any)
	nm, ok2 := cand.(map[string]any)
	if ok1 && ok2 {
		classifyNode(cm, nm, path+".items", out)
	}
}

// --- small helpers ---

func typeSet(v any) map[string]bool {
	s := map[string]bool{}
	switch t := v.(type) {
	case string:
		s[t] = true
	case []any:
		for _, e := range t {
			if es, ok := e.(string); ok {
				s[es] = true
			}
		}
	}
	return s
}

func strSet(v any) map[string]bool {
	s := map[string]bool{}
	if arr, ok := v.([]any); ok {
		for _, e := range arr {
			if es, ok := e.(string); ok {
				s[es] = true
			}
		}
	}
	return s
}

func asFloat(v any) (float64, bool) {
	switch n := v.(type) {
	case float64:
		return n, true
	case int:
		return float64(n), true
	case int64:
		return float64(n), true
	}
	return 0, false
}

func equalJSON(a, b any) bool {
	return fmt.Sprintf("%v", a) == fmt.Sprintf("%v", b)
}
