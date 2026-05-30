package policy

import (
	"testing"

	"github.com/google/uuid"
)

// buildRules returns n deterministic rules: a spread of wildcard/specific actor,
// collection, and operation dimensions, ending with one exact-match allow rule
// for the benchmarked request so Select must scan the whole slice.
func buildRules(n int, col uuid.UUID) []Rule {
	rules := make([]Rule, 0, n)
	ops := []string{OpCreate, OpRead, OpUpdate, OpDelete}
	for i := 0; i < n-1; i++ {
		rules = append(rules, Rule{
			ID:                 uuid.New(),
			Actor:              "other-actor",
			Collection:         uuid.New(),
			CollectionWildcard: i%2 == 0,
			Operation:          ops[i%len(ops)],
			Effect:             "deny",
		})
	}
	rules = append(rules, Rule{
		ID: uuid.New(), Actor: "agent-1", Collection: col,
		CollectionWildcard: false, Operation: OpUpdate, Effect: "allow",
	})
	return rules
}

func benchmarkSelect(b *testing.B, n int) {
	b.Helper()
	col := uuid.New()
	rules := buildRules(n, col)
	req := Request{Actor: "agent-1", Collection: col, Operation: OpUpdate}
	b.ReportAllocs()
	b.ResetTimer()
	var sink Decision
	for i := 0; i < b.N; i++ {
		d, _ := Select(rules, req)
		sink = d
	}
	_ = sink
}

func BenchmarkSelect_Small(b *testing.B) { benchmarkSelect(b, 5) }
func BenchmarkSelect_Large(b *testing.B) { benchmarkSelect(b, 50) }
