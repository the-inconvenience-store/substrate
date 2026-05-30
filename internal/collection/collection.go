package collection

import (
	"context"
	"errors"
	"fmt"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/substrate/substrate/internal/apierr"
	"github.com/substrate/substrate/internal/db"
)

// Collection is a flexible (v0) object type within a workspace.
type Collection struct {
	ID           uuid.UUID `json:"id"`
	WorkspaceID  uuid.UUID `json:"workspace_id"`
	Name         string    `json:"name"`
	Level        string    `json:"level"`
	AutoBackfill bool      `json:"auto_backfill"`
}

// Service manages collections.
type Service struct {
	pool *pgxpool.Pool
	q    *db.Queries
}

func New(pool *pgxpool.Pool) *Service { return &Service{pool: pool, q: db.New(pool)} }

func (s *Service) Create(ctx context.Context, ws uuid.UUID, name string) (Collection, error) {
	if name == "" {
		return Collection{}, apierr.New(apierr.BadRequest, "collection name required")
	}
	row, err := s.q.CreateCollection(ctx, db.CreateCollectionParams{
		ID: uuid.New(), WorkspaceID: ws, Name: name, Level: "flexible",
	})
	if err != nil {
		var pgErr *pgconn.PgError
		if errors.As(err, &pgErr) && pgErr.Code == "23505" {
			return Collection{}, apierr.New(apierr.Conflict, "collection name already exists")
		}
		return Collection{}, fmt.Errorf("insert collection: %w", err)
	}
	return Collection{ID: row.ID, WorkspaceID: row.WorkspaceID, Name: row.Name, Level: row.Level}, nil
}

// SetAutoBackfill toggles a collection's opt-in auto-backfill flag.
func (s *Service) SetAutoBackfill(ctx context.Context, ws, col uuid.UUID, enabled bool) error {
	if err := s.q.SetAutoBackfill(ctx, db.SetAutoBackfillParams{WorkspaceID: ws, ID: col, AutoBackfill: enabled}); err != nil {
		return fmt.Errorf("set auto_backfill: %w", err)
	}
	return nil
}

func (s *Service) GetByName(ctx context.Context, ws uuid.UUID, name string) (Collection, error) {
	row, err := s.q.GetCollectionByName(ctx, db.GetCollectionByNameParams{
		WorkspaceID: ws, Name: name,
	})
	if errors.Is(err, pgx.ErrNoRows) {
		return Collection{}, apierr.New(apierr.NotFound, "collection not found")
	}
	if err != nil {
		return Collection{}, fmt.Errorf("get collection: %w", err)
	}
	return Collection{ID: row.ID, WorkspaceID: row.WorkspaceID, Name: row.Name, Level: row.Level, AutoBackfill: row.AutoBackfill}, nil
}
