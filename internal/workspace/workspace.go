package workspace

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"errors"
	"fmt"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgtype"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/substrate/substrate/internal/apierr"
	"github.com/substrate/substrate/internal/db"
)

// Workspace is a tenant boundary.
type Workspace struct {
	ID         uuid.UUID `json:"id"`
	Name       string    `json:"name"`
	PolicyMode string    `json:"policy_mode"`
}

// Service manages workspaces and their API keys.
type Service struct {
	pool *pgxpool.Pool
	q    *db.Queries
}

func New(pool *pgxpool.Pool) *Service { return &Service{pool: pool, q: db.New(pool)} }

func (s *Service) CreateWorkspace(ctx context.Context, name string) (Workspace, error) {
	row, err := s.q.CreateWorkspace(ctx, db.CreateWorkspaceParams{
		ID: uuid.New(), Name: name, PolicyMode: "allow",
	})
	if err != nil {
		return Workspace{}, fmt.Errorf("insert workspace: %w", err)
	}
	return Workspace{ID: row.ID, Name: row.Name, PolicyMode: row.PolicyMode}, nil
}

// CreateAPIKey returns the plaintext key (shown once) and the stored key id.
func (s *Service) CreateAPIKey(ctx context.Context, ws uuid.UUID, label string) (string, uuid.UUID, error) {
	raw := make([]byte, 32)
	if _, err := rand.Read(raw); err != nil {
		return "", uuid.Nil, fmt.Errorf("rand: %w", err)
	}
	plaintext := "sk_" + base64.RawURLEncoding.EncodeToString(raw)
	sum := sha256.Sum256([]byte(plaintext))
	id, err := s.q.CreateAPIKey(ctx, db.CreateAPIKeyParams{
		ID:          uuid.New(),
		WorkspaceID: ws,
		Hash:        sum[:],
		Label:       pgtype.Text{String: label, Valid: label != ""},
	})
	if err != nil {
		return "", uuid.Nil, fmt.Errorf("insert key: %w", err)
	}
	return plaintext, id, nil
}

// VerifyKey resolves a plaintext key to its workspace, or returns an Unauthorized error.
func (s *Service) VerifyKey(ctx context.Context, plaintext string) (uuid.UUID, error) {
	sum := sha256.Sum256([]byte(plaintext))
	ws, err := s.q.GetWorkspaceIDByAPIKeyHash(ctx, sum[:])
	if errors.Is(err, pgx.ErrNoRows) {
		return uuid.Nil, apierr.New(apierr.Unauthorized, "invalid api key")
	}
	if err != nil {
		return uuid.Nil, fmt.Errorf("verify: %w", err)
	}
	return ws, nil
}
