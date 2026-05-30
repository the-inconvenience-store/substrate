package policy

import (
	"testing"

	"github.com/google/uuid"
)

func ruleID(n byte) uuid.UUID { return uuid.UUID{15: n} }

func TestSelectPrecedence(t *testing.T) {
	colA := uuid.UUID{0: 1}
	colB := uuid.UUID{0: 2}
	req := Request{Actor: "alice", Collection: colA, Operation: OpCreate}

	tests := []struct {
		name     string
		rules    []Rule
		wantOK   bool
		wantEff  string
		wantRule *uuid.UUID
	}{
		{
			name:   "no rules -> no decision",
			rules:  nil,
			wantOK: false,
		},
		{
			name:   "non-applicable rule ignored",
			rules:  []Rule{{ID: ruleID(1), Actor: "bob", CollectionWildcard: true, Operation: "*", Effect: "deny"}},
			wantOK: false,
		},
		{
			name:    "wildcard allow applies",
			rules:   []Rule{{ID: ruleID(2), Actor: "*", CollectionWildcard: true, Operation: "*", Effect: "allow"}},
			wantOK:  true,
			wantEff: "allow",
		},
		{
			name: "most-specific allow beats broad deny",
			rules: []Rule{
				{ID: ruleID(3), Actor: "*", CollectionWildcard: true, Operation: "*", Effect: "deny"},
				{ID: ruleID(4), Actor: "alice", Collection: colA, Operation: OpCreate, Effect: "allow"},
			},
			wantOK:   true,
			wantEff:  "allow",
			wantRule: ptr(ruleID(4)),
		},
		{
			name: "deny-overrides at equal specificity",
			rules: []Rule{
				{ID: ruleID(5), Actor: "alice", CollectionWildcard: true, Operation: "*", Effect: "allow"},
				{ID: ruleID(6), Actor: "*", Collection: colA, Operation: "*", Effect: "deny"},
			},
			wantOK:  true,
			wantEff: "deny",
		},
		{
			name: "collection mismatch excludes rule",
			rules: []Rule{
				{ID: ruleID(7), Actor: "*", Collection: colB, Operation: "*", Effect: "deny"},
			},
			wantOK: false,
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			dec, ok := Select(tc.rules, req)
			if ok != tc.wantOK {
				t.Fatalf("ok = %v, want %v", ok, tc.wantOK)
			}
			if !ok {
				return
			}
			if dec.Effect != tc.wantEff {
				t.Fatalf("effect = %q, want %q", dec.Effect, tc.wantEff)
			}
			if tc.wantRule != nil && (dec.MatchedRule == nil || *dec.MatchedRule != *tc.wantRule) {
				t.Fatalf("matched rule = %v, want %v", dec.MatchedRule, tc.wantRule)
			}
		})
	}
}

func TestDefaultDecision(t *testing.T) {
	if d := DefaultDecision("deny"); d.Effect != "deny" || d.Reason != "default_deny" {
		t.Fatalf("deny default = %+v", d)
	}
	if d := DefaultDecision("allow"); d.Effect != "allow" || d.Reason != "default_allow" {
		t.Fatalf("allow default = %+v", d)
	}
	if d := DefaultDecision("anything-else"); d.Effect != "allow" {
		t.Fatalf("unknown mode should default allow, got %+v", d)
	}
}

func ptr(id uuid.UUID) *uuid.UUID { return &id }
