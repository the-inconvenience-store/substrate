package projection

import (
	"context"
	"errors"
	"fmt"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/substrate/substrate/internal/db"
)

// Replayer rebuilds the records projection from the authoritative events stream.
type Replayer struct {
	pool *pgxpool.Pool
	q    *db.Queries
}

func NewReplayer(pool *pgxpool.Pool) *Replayer { return &Replayer{pool: pool, q: db.New(pool)} }

// RebuildRecord reconstructs a single record's projection row from its latest event.
// Returns false when the record has no events. Idempotent.
func (r *Replayer) RebuildRecord(ctx context.Context, ws, col, id uuid.UUID) (bool, error) {
	ev, err := r.q.GetLatestRecordEvent(ctx, db.GetLatestRecordEventParams{
		WorkspaceID: ws, CollectionID: col, RecordID: id,
	})
	if errors.Is(err, pgx.ErrNoRows) {
		return false, nil
	}
	if err != nil {
		return false, fmt.Errorf("latest event: %w", err)
	}
	status := "active"
	if ev.Type == "delete" {
		status = "deleted"
	}
	data := ev.StateAfter
	if len(data) == 0 {
		data = []byte("{}")
	}
	if err := r.q.UpsertRecordProjection(ctx, db.UpsertRecordProjectionParams{
		ID: id, CollectionID: col, WorkspaceID: ws,
		Data: data, Revision: ev.Revision, Status: status,
		Actor: ev.Actor, SchemaVersion: ev.SchemaVersion,
	}); err != nil {
		return false, fmt.Errorf("upsert projection: %w", err)
	}
	return true, nil
}

// RebuildCollection rebuilds every record in a collection from its events and returns
// the number rebuilt. Lifecycle pseudo-events are excluded by the underlying query.
func (r *Replayer) RebuildCollection(ctx context.Context, ws, col uuid.UUID) (int, error) {
	ids, err := r.q.ListRecordIDsInCollection(ctx, db.ListRecordIDsInCollectionParams{
		WorkspaceID: ws, CollectionID: col,
	})
	if err != nil {
		return 0, fmt.Errorf("list record ids: %w", err)
	}
	n := 0
	for _, id := range ids {
		ok, err := r.RebuildRecord(ctx, ws, col, id)
		if err != nil {
			return n, err
		}
		if ok {
			n++
		}
	}
	return n, nil
}
