package policy

import (
	"context"
	"encoding/json"
	"fmt"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgtype"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/substrate/substrate/internal/apierr"
	"github.com/substrate/substrate/internal/db"
)

// Evaluator authorizes an operation. On deny it records a policy_denied event and
// returns an apierr.Forbidden error alongside the (deny) decision.
type Evaluator interface {
	Authorize(ctx context.Context, req Request) (Decision, error)
}

// Engine is the in-process rule evaluator backed by the policies table.
type Engine struct {
	pool *pgxpool.Pool
	q    *db.Queries
}

// NewEngine builds the evaluator over a connection pool.
func NewEngine(pool *pgxpool.Pool) *Engine {
	return &Engine{pool: pool, q: db.New(pool)}
}

// Evaluate computes the decision with no side effects.
func (e *Engine) Evaluate(ctx context.Context, req Request) (Decision, error) {
	rows, err := e.q.ListPoliciesForRequest(ctx, db.ListPoliciesForRequestParams{
		WorkspaceID:  req.Workspace,
		CollectionID: pgtype.UUID{Bytes: req.Collection, Valid: true},
	})
	if err != nil {
		return Decision{}, fmt.Errorf("load policies: %w", err)
	}
	rules := make([]Rule, 0, len(rows))
	for _, r := range rows {
		rule := Rule{ID: r.ID, Actor: r.Actor, Operation: r.Operation, Effect: r.Effect}
		if r.CollectionID.Valid {
			rule.Collection = uuid.UUID(r.CollectionID.Bytes)
		} else {
			rule.CollectionWildcard = true
		}
		rules = append(rules, rule)
	}
	if dec, ok := Select(rules, req); ok {
		return dec, nil
	}
	mode, err := e.q.GetWorkspacePolicyMode(ctx, req.Workspace)
	if err != nil {
		return Decision{}, fmt.Errorf("load policy mode: %w", err)
	}
	return DefaultDecision(mode), nil
}

// Authorize evaluates and, on deny, writes a policy_denied event then returns Forbidden.
func (e *Engine) Authorize(ctx context.Context, req Request) (Decision, error) {
	dec, err := e.Evaluate(ctx, req)
	if err != nil {
		return Decision{}, err
	}
	if dec.Allowed() {
		return dec, nil
	}
	if err := e.recordDenial(ctx, req, dec); err != nil {
		return Decision{}, err
	}
	return dec, apierr.New(apierr.Forbidden, "operation denied by policy").
		WithDetails(denialDetails(dec))
}

func (e *Engine) recordDenial(ctx context.Context, req Request, dec Decision) error {
	err := e.q.AppendEvent(ctx, db.AppendEventParams{
		ID: uuid.New(), WorkspaceID: req.Workspace, CollectionID: req.Collection,
		RecordID: req.Target, Type: "policy_denied", Revision: 0,
		StateAfter:     nil,
		Actor:          pgtype.Text{String: req.Actor, Valid: req.Actor != ""},
		IdempotencyKey: pgtype.Text{},
		Trace:          dec.TraceJSON(req.Operation),
	})
	if err != nil {
		return fmt.Errorf("record denial: %w", err)
	}
	return nil
}

// TraceJSON renders the decision for the events.trace column.
func (d Decision) TraceJSON(op string) []byte {
	m := map[string]any{"effect": d.Effect, "reason": d.Reason, "operation": op}
	if d.MatchedRule != nil {
		m["matched_rule"] = d.MatchedRule.String()
	}
	b, _ := json.Marshal(m)
	return b
}

func denialDetails(d Decision) map[string]any {
	m := map[string]any{"reason": d.Reason}
	if d.MatchedRule != nil {
		m["matched_rule"] = d.MatchedRule.String()
	}
	return m
}
