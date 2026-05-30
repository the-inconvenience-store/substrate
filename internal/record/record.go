package record

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/pgtype"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/substrate/substrate/internal/apierr"
	"github.com/substrate/substrate/internal/db"
	"github.com/substrate/substrate/internal/store"
)

// Record is the current materialized state of an object.
type Record struct {
	ID         uuid.UUID      `json:"id"`
	Collection uuid.UUID      `json:"collection_id"`
	Data       map[string]any `json:"data"`
	Revision   int64          `json:"revision"`
	Status     string         `json:"status"`
	Actor      string         `json:"actor"`
}

// CreateCmd is the input for creating a record.
type CreateCmd struct {
	Workspace      uuid.UUID
	Collection     uuid.UUID
	Actor          string
	Data           map[string]any
	IdempotencyKey string
}

// UpdateCmd is the input for replacing a record's data.
type UpdateCmd struct {
	Workspace        uuid.UUID
	Collection       uuid.UUID
	ID               uuid.UUID
	ExpectedRevision int64
	Actor            string
	Data             map[string]any
	IdempotencyKey   string
}

// Service performs record mutations and reads.
type Service struct {
	pool *pgxpool.Pool
	q    *db.Queries
}

func New(pool *pgxpool.Pool) *Service { return &Service{pool: pool, q: db.New(pool)} }

func (s *Service) Create(ctx context.Context, cmd CreateCmd) (Record, error) {
	rec := Record{
		ID: uuid.New(), Collection: cmd.Collection, Data: cmd.Data,
		Revision: 1, Status: "active", Actor: cmd.Actor,
	}
	if rec.Data == nil {
		rec.Data = map[string]any{}
	}
	err := store.WithTx(ctx, s.pool, func(tx pgx.Tx) error {
		qtx := s.q.WithTx(tx)
		if cmd.IdempotencyKey != "" {
			if replayed, ok, err := lookupReplay(ctx, qtx, cmd.Workspace, cmd.IdempotencyKey); err != nil {
				return err
			} else if ok {
				rec = replayed
				return nil
			}
		}
		if err := appendEvent(ctx, qtx, eventRow{
			Workspace: cmd.Workspace, Collection: cmd.Collection, RecordID: rec.ID,
			Type: "create", Revision: 1, State: rec.Data, Actor: cmd.Actor,
			IdempotencyKey: cmd.IdempotencyKey,
		}); err != nil {
			return err
		}
		return qtx.InsertRecord(ctx, db.InsertRecordParams{
			ID: rec.ID, CollectionID: cmd.Collection, WorkspaceID: cmd.Workspace,
			Data: mustJSON(rec.Data), Revision: 1, Actor: textOrNull(cmd.Actor),
		})
	})
	if err != nil {
		// Concurrent idempotency conflict: the transaction was rolled back and the
		// winning row is now visible. Re-query outside the aborted transaction.
		if errors.Is(err, errIdempotencyConflict) && cmd.IdempotencyKey != "" {
			replayed, ok, rerr := lookupReplay(ctx, s.q, cmd.Workspace, cmd.IdempotencyKey)
			if rerr != nil {
				return Record{}, fmt.Errorf("replay after conflict: %w", rerr)
			}
			if ok {
				return replayed, nil
			}
		}
		return Record{}, err
	}
	return rec, nil
}

func (s *Service) Get(ctx context.Context, ws, col, id uuid.UUID) (Record, error) {
	row, err := s.q.GetActiveRecord(ctx, db.GetActiveRecordParams{
		WorkspaceID: ws, CollectionID: col, ID: id,
	})
	if errors.Is(err, pgx.ErrNoRows) {
		return Record{}, apierr.New(apierr.NotFound, "record not found")
	}
	if err != nil {
		return Record{}, fmt.Errorf("get record: %w", err)
	}
	rec := Record{
		ID: row.ID, Collection: row.CollectionID, Revision: row.Revision,
		Status: row.Status, Actor: row.Actor.String,
	}
	if err := json.Unmarshal(row.Data, &rec.Data); err != nil {
		return Record{}, fmt.Errorf("decode data: %w", err)
	}
	return rec, nil
}

func (s *Service) Update(ctx context.Context, cmd UpdateCmd) (Record, error) {
	var rec Record
	err := store.WithTx(ctx, s.pool, func(tx pgx.Tx) error {
		qtx := s.q.WithTx(tx)
		if cmd.IdempotencyKey != "" {
			if replayed, ok, err := lookupReplay(ctx, qtx, cmd.Workspace, cmd.IdempotencyKey); err != nil {
				return err
			} else if ok {
				rec = replayed
				return nil
			}
		}
		current, err := qtx.GetRecordRevisionForUpdate(ctx, db.GetRecordRevisionForUpdateParams{
			WorkspaceID: cmd.Workspace, CollectionID: cmd.Collection, ID: cmd.ID,
		})
		if errors.Is(err, pgx.ErrNoRows) {
			return apierr.New(apierr.NotFound, "record not found")
		}
		if err != nil {
			return err
		}
		if current != cmd.ExpectedRevision {
			return apierr.New(apierr.Conflict, "revision mismatch").
				WithDetails(map[string]any{"expected": cmd.ExpectedRevision, "current": current})
		}
		next := current + 1
		if cmd.Data == nil {
			cmd.Data = map[string]any{}
		}
		if err := appendEvent(ctx, qtx, eventRow{
			Workspace: cmd.Workspace, Collection: cmd.Collection, RecordID: cmd.ID,
			Type: "update", Revision: next, State: cmd.Data, Actor: cmd.Actor,
			IdempotencyKey: cmd.IdempotencyKey,
		}); err != nil {
			return err
		}
		if err := qtx.UpdateRecordData(ctx, db.UpdateRecordDataParams{
			WorkspaceID: cmd.Workspace, CollectionID: cmd.Collection, ID: cmd.ID,
			Data: mustJSON(cmd.Data), Revision: next, Actor: textOrNull(cmd.Actor),
		}); err != nil {
			return err
		}
		rec = Record{ID: cmd.ID, Collection: cmd.Collection, Data: cmd.Data,
			Revision: next, Status: "active", Actor: cmd.Actor}
		return nil
	})
	if err != nil {
		// Concurrent idempotency conflict: the transaction was rolled back and the
		// winning row is now visible. Re-query outside the aborted transaction.
		if errors.Is(err, errIdempotencyConflict) && cmd.IdempotencyKey != "" {
			replayed, ok, rerr := lookupReplay(ctx, s.q, cmd.Workspace, cmd.IdempotencyKey)
			if rerr != nil {
				return Record{}, fmt.Errorf("replay after conflict: %w", rerr)
			}
			if ok {
				return replayed, nil
			}
		}
		return Record{}, err
	}
	return rec, nil
}

func (s *Service) Delete(ctx context.Context, ws, col, id uuid.UUID, actor string) error {
	return store.WithTx(ctx, s.pool, func(tx pgx.Tx) error {
		qtx := s.q.WithTx(tx)
		row, err := qtx.GetRecordForUpdate(ctx, db.GetRecordForUpdateParams{
			WorkspaceID: ws, CollectionID: col, ID: id,
		})
		if errors.Is(err, pgx.ErrNoRows) {
			return apierr.New(apierr.NotFound, "record not found")
		}
		if err != nil {
			return err
		}
		next := row.Revision + 1
		var data map[string]any
		_ = json.Unmarshal(row.Data, &data)
		if err := appendEvent(ctx, qtx, eventRow{
			Workspace: ws, Collection: col, RecordID: id,
			Type: "delete", Revision: next, State: data, Actor: actor,
		}); err != nil {
			return err
		}
		return qtx.SoftDeleteRecord(ctx, db.SoftDeleteRecordParams{
			WorkspaceID: ws, CollectionID: col, ID: id, Revision: next,
		})
	})
}

// --- internal helpers ---

type eventRow struct {
	Workspace      uuid.UUID
	Collection     uuid.UUID
	RecordID       uuid.UUID
	Type           string
	Revision       int64
	State          map[string]any
	Actor          string
	IdempotencyKey string
}

// errIdempotencyConflict is a sentinel returned by appendEvent when the INSERT
// fails specifically on the idempotency unique index (SQLSTATE 23505). The
// caller is responsible for performing a post-rollback replay lookup.
var errIdempotencyConflict = errors.New("idempotency key conflict")

func appendEvent(ctx context.Context, q *db.Queries, e eventRow) error {
	err := q.AppendEvent(ctx, db.AppendEventParams{
		ID: uuid.New(), WorkspaceID: e.Workspace, CollectionID: e.Collection,
		RecordID: e.RecordID, Type: e.Type, Revision: e.Revision,
		StateAfter: mustJSON(e.State), Actor: textOrNull(e.Actor),
		IdempotencyKey: textOrNull(e.IdempotencyKey),
	})
	if err != nil {
		var pgErr *pgconn.PgError
		if errors.As(err, &pgErr) && pgErr.Code == "23505" {
			if e.IdempotencyKey != "" {
				// Concurrent duplicate: the winning transaction already committed
				// this idempotency key. Signal the caller to replay outside the
				// now-aborted transaction.
				return errIdempotencyConflict
			}
			return apierr.New(apierr.Conflict, "duplicate idempotency key")
		}
		return fmt.Errorf("append event: %w", err)
	}
	return nil
}

// lookupReplay returns the materialized record for a prior idempotency key, if any.
func lookupReplay(ctx context.Context, q *db.Queries, ws uuid.UUID, key string) (Record, bool, error) {
	row, err := q.GetReplayEvent(ctx, db.GetReplayEventParams{
		WorkspaceID: ws, IdempotencyKey: textOrNull(key),
	})
	if errors.Is(err, pgx.ErrNoRows) {
		return Record{}, false, nil
	}
	if err != nil {
		return Record{}, false, fmt.Errorf("replay lookup: %w", err)
	}
	rec := Record{
		ID: row.RecordID, Collection: row.CollectionID, Revision: row.Revision,
		Status: "active", Actor: row.Actor.String,
	}
	if row.Type == "delete" {
		rec.Status = "deleted"
	}
	if err := json.Unmarshal(row.StateAfter, &rec.Data); err != nil {
		return Record{}, false, fmt.Errorf("decode replay: %w", err)
	}
	return rec, true, nil
}

func mustJSON(v any) []byte {
	b, err := json.Marshal(v)
	if err != nil {
		panic(fmt.Sprintf("marshal: %v", err))
	}
	return b
}

// textOrNull maps a Go string to a pgtype.Text, treating "" as SQL NULL.
func textOrNull(s string) pgtype.Text {
	return pgtype.Text{String: s, Valid: s != ""}
}
