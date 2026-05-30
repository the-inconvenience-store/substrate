// Package policy implements Substrate's declarative allow/deny governance plane.
package policy

import "github.com/google/uuid"

// Operation tokens a rule may target (besides "*").
const (
	OpCreate          = "create"
	OpRead            = "read"
	OpUpdate          = "update"
	OpDelete          = "delete"
	OpRegisterSchema  = "register_schema"
	OpActivateSchema  = "activate_schema"
	OpDeprecateSchema = "deprecate_schema"
	// OpBackfill gates the backfill-management surface: triggering a manual
	// backfill and toggling a collection's auto_backfill flag.
	OpBackfill = "backfill"
)

// ValidOperations is the set of operation tokens accepted on rule creation.
var ValidOperations = map[string]bool{
	OpCreate: true, OpRead: true, OpUpdate: true, OpDelete: true,
	OpRegisterSchema: true, OpActivateSchema: true, OpDeprecateSchema: true,
	OpBackfill: true,
}

// Rule is one loaded policy row. CollectionWildcard is true when collection_id is NULL.
type Rule struct {
	ID                 uuid.UUID
	Actor              string // exact actor or "*"
	Collection         uuid.UUID
	CollectionWildcard bool
	Operation          string // operation token or "*"
	Effect             string // "allow" | "deny"
}

// Request is one authorization question.
type Request struct {
	Workspace  uuid.UUID
	Actor      string
	Collection uuid.UUID
	Target     uuid.UUID // record id recorded on the denial event; uuid.Nil when none
	Operation  string
}

// Decision is the evaluation result.
type Decision struct {
	Effect      string     // "allow" | "deny"
	MatchedRule *uuid.UUID // nil when defaulted
	Reason      string     // "rule" | "default_allow" | "default_deny" | "no_evaluator"
}

// Allowed reports whether the decision permits the operation.
func (d Decision) Allowed() bool { return d.Effect == "allow" }

func (r Rule) applies(req Request) bool {
	if r.Actor != "*" && r.Actor != req.Actor {
		return false
	}
	if !r.CollectionWildcard && r.Collection != req.Collection {
		return false
	}
	if r.Operation != "*" && r.Operation != req.Operation {
		return false
	}
	return true
}

// specificity counts concrete (non-wildcard) dimensions, range 0..3.
func (r Rule) specificity() int {
	n := 0
	if r.Actor != "*" {
		n++
	}
	if !r.CollectionWildcard {
		n++
	}
	if r.Operation != "*" {
		n++
	}
	return n
}

// Select applies precedence: highest specificity wins; deny-overrides at equal
// specificity. Returns ok=false when no rule applies (caller uses the default mode).
func Select(rules []Rule, req Request) (Decision, bool) {
	best := -1
	var winner *Rule
	for i := range rules {
		if !rules[i].applies(req) {
			continue
		}
		s := rules[i].specificity()
		switch {
		case s > best:
			best, winner = s, &rules[i]
		case s == best && winner != nil && winner.Effect != "deny" && rules[i].Effect == "deny":
			winner = &rules[i]
		}
	}
	if winner == nil {
		return Decision{}, false
	}
	id := winner.ID
	return Decision{Effect: winner.Effect, MatchedRule: &id, Reason: "rule"}, true
}

// DefaultDecision maps a workspace policy_mode to a defaulted decision.
func DefaultDecision(mode string) Decision {
	if mode == "deny" {
		return Decision{Effect: "deny", Reason: "default_deny"}
	}
	return Decision{Effect: "allow", Reason: "default_allow"}
}
