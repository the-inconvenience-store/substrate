package policy

import (
	"context"
	"errors"
	"fmt"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgtype"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/substrate/substrate/internal/apierr"
	"github.com/substrate/substrate/internal/db"
)

// PolicyRule is one rule as returned by the API.
type PolicyRule struct {
	ID         uuid.UUID `json:"id"`
	Actor      string    `json:"actor"`
	Collection string    `json:"collection"` // collection name, or "*"
	Operation  string    `json:"operation"`
	Effect     string    `json:"effect"`
}

// CreateRuleCmd is the input for creating a rule. CollectionID nil ⇒ any collection.
type CreateRuleCmd struct {
	Workspace      uuid.UUID
	Actor          string
	Operation      string
	Effect         string
	CollectionID   *uuid.UUID
	CollectionName string // echoed back; "*" when CollectionID is nil
}

// Service is the rule-management facade (separate from the Engine evaluator).
type Service struct {
	pool *pgxpool.Pool
	q    *db.Queries
}

func NewService(pool *pgxpool.Pool) *Service { return &Service{pool: pool, q: db.New(pool)} }

// CreateRule validates and inserts a rule.
func (s *Service) CreateRule(ctx context.Context, cmd CreateRuleCmd) (PolicyRule, error) {
	actor := cmd.Actor
	if actor == "" {
		actor = "*"
	}
	op := cmd.Operation
	if op == "" {
		op = "*"
	}
	if op != "*" && !ValidOperations[op] {
		return PolicyRule{}, apierr.New(apierr.BadRequest, "unknown operation token")
	}
	if cmd.Effect != "allow" && cmd.Effect != "deny" {
		return PolicyRule{}, apierr.New(apierr.BadRequest, "effect must be 'allow' or 'deny'")
	}
	cid := pgtype.UUID{}
	colName := "*"
	if cmd.CollectionID != nil {
		cid = pgtype.UUID{Bytes: *cmd.CollectionID, Valid: true}
		colName = cmd.CollectionName
	}
	row, err := s.q.InsertPolicy(ctx, db.InsertPolicyParams{
		ID: uuid.New(), WorkspaceID: cmd.Workspace, Actor: actor,
		CollectionID: cid, Operation: op, Effect: cmd.Effect,
	})
	if err != nil {
		return PolicyRule{}, fmt.Errorf("insert policy: %w", err)
	}
	return PolicyRule{ID: row.ID, Actor: row.Actor, Collection: colName, Operation: row.Operation, Effect: row.Effect}, nil
}

// ListRules returns all rules for a workspace.
func (s *Service) ListRules(ctx context.Context, ws uuid.UUID) ([]PolicyRule, error) {
	rows, err := s.q.ListPolicies(ctx, ws)
	if err != nil {
		return nil, fmt.Errorf("list policies: %w", err)
	}
	out := make([]PolicyRule, 0, len(rows))
	for _, r := range rows {
		name := "*"
		if r.CollectionName.Valid {
			name = r.CollectionName.String
		}
		out = append(out, PolicyRule{ID: r.ID, Actor: r.Actor, Collection: name, Operation: r.Operation, Effect: r.Effect})
	}
	return out, nil
}

// DeleteRule removes a rule scoped to the workspace.
func (s *Service) DeleteRule(ctx context.Context, ws, id uuid.UUID) error {
	n, err := s.q.DeletePolicy(ctx, db.DeletePolicyParams{ID: id, WorkspaceID: ws})
	if err != nil {
		return fmt.Errorf("delete policy: %w", err)
	}
	if n == 0 {
		return apierr.New(apierr.NotFound, "policy not found")
	}
	return nil
}

// DefaultMode returns the workspace's current default policy mode.
func (s *Service) DefaultMode(ctx context.Context, ws uuid.UUID) (string, error) {
	mode, err := s.q.GetWorkspacePolicyMode(ctx, ws)
	if errors.Is(err, pgx.ErrNoRows) {
		return "", apierr.New(apierr.NotFound, "workspace not found")
	}
	if err != nil {
		return "", fmt.Errorf("get mode: %w", err)
	}
	return mode, nil
}
