package record

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/pgtype"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/substrate/substrate/internal/apierr"
	"github.com/substrate/substrate/internal/db"
	"github.com/substrate/substrate/internal/policy"
	"github.com/substrate/substrate/internal/query"
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

// Validator validates a record body against a collection's active schema and
// returns the schema version to stamp (0 for flexible collections).
type Validator interface {
	ValidateWrite(ctx context.Context, collectionID uuid.UUID, data map[string]any) (int, error)
}

// Service performs record mutations and reads.
type Service struct {
	pool      *pgxpool.Pool
	q         *db.Queries
	validator Validator
	eval      policy.Evaluator
}

// New builds a record service. validator may be nil (flexible-only; no validation).
func New(pool *pgxpool.Pool, validator Validator) *Service {
	return &Service{pool: pool, q: db.New(pool), validator: validator}
}

// WithEvaluator wires an optional policy evaluator. nil ⇒ no enforcement.
func (s *Service) WithEvaluator(e policy.Evaluator) *Service { s.eval = e; return s }

// authorize runs the policy check for an operation. With no evaluator it allows.
func (s *Service) authorize(ctx context.Context, req policy.Request) (policy.Decision, error) {
	if s.eval == nil {
		return policy.Decision{Effect: "allow", Reason: "no_evaluator"}, nil
	}
	return s.eval.Authorize(ctx, req)
}

// policyTrace returns the trace bytes for an allowed event, or nil when no
// evaluator is wired (preserving the pre-policy NULL-trace behavior).
func (s *Service) policyTrace(dec policy.Decision, op string) []byte {
	if s.eval == nil {
		return nil
	}
	return dec.TraceJSON(op)
}

// schemaVersionParam validates (if a validator is configured) and returns the
// pgtype.Int4 to stamp on the record. version 0 -> NULL (flexible / grandfathered).
func (s *Service) schemaVersionParam(ctx context.Context, col uuid.UUID, data map[string]any) (pgtype.Int4, error) {
	if s.validator == nil {
		return pgtype.Int4{}, nil
	}
	ver, err := s.validator.ValidateWrite(ctx, col, data)
	if err != nil {
		return pgtype.Int4{}, err
	}
	if ver == 0 {
		return pgtype.Int4{}, nil
	}
	return pgtype.Int4{Int32: int32(ver), Valid: true}, nil
}

func (s *Service) Create(ctx context.Context, cmd CreateCmd) (Record, error) {
	dec, err := s.authorize(ctx, policy.Request{
		Workspace: cmd.Workspace, Actor: cmd.Actor, Collection: cmd.Collection,
		Target: uuid.Nil, Operation: policy.OpCreate,
	})
	if err != nil {
		return Record{}, err
	}
	rec := Record{
		ID: uuid.New(), Collection: cmd.Collection, Data: cmd.Data,
		Revision: 1, Status: "active", Actor: cmd.Actor,
	}
	if rec.Data == nil {
		rec.Data = map[string]any{}
	}
	sv, err := s.schemaVersionParam(ctx, cmd.Collection, rec.Data)
	if err != nil {
		return Record{}, err
	}
	err = store.WithTx(ctx, s.pool, func(tx pgx.Tx) error {
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
			Trace:          s.policyTrace(dec, policy.OpCreate),
			SchemaVersion:  sv,
		}); err != nil {
			return err
		}
		return qtx.InsertRecord(ctx, db.InsertRecordParams{
			ID: rec.ID, CollectionID: cmd.Collection, WorkspaceID: cmd.Workspace,
			Data: mustJSON(rec.Data), Revision: 1, Actor: textOrNull(cmd.Actor),
			SchemaVersion: sv,
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

func (s *Service) Get(ctx context.Context, ws, col, id uuid.UUID, actor string) (Record, error) {
	if _, err := s.authorize(ctx, policy.Request{
		Workspace: ws, Actor: actor, Collection: col, Target: id, Operation: policy.OpRead,
	}); err != nil {
		return Record{}, err
	}
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

// List runs a parsed list query within a workspace+collection and returns the
// page of records plus an opaque next_cursor ("" when the page is the last one).
func (s *Service) List(ctx context.Context, ws, col uuid.UUID, actor string, q query.ListQuery) ([]Record, string, error) {
	if _, err := s.authorize(ctx, policy.Request{
		Workspace: ws, Actor: actor, Collection: col, Target: uuid.Nil, Operation: policy.OpRead,
	}); err != nil {
		return nil, "", err
	}
	sqlText, valueArgs, err := query.Build(q)
	if err != nil {
		return nil, "", err
	}
	args := append([]any{ws, col}, valueArgs...)
	rows, err := s.pool.Query(ctx, sqlText, args...)
	if err != nil {
		return nil, "", fmt.Errorf("list query: %w", err)
	}
	defer rows.Close()

	type scanned struct {
		rec     Record
		created time.Time
		sortKey string
	}
	var all []scanned
	for rows.Next() {
		var (
			sc      scanned
			rawData []byte
			actor   pgtype.Text
		)
		if err := rows.Scan(&sc.rec.ID, &sc.rec.Collection, &rawData, &sc.rec.Revision,
			&sc.rec.Status, &actor, &sc.created, &sc.sortKey); err != nil {
			return nil, "", fmt.Errorf("scan row: %w", err)
		}
		sc.rec.Actor = actor.String
		if err := json.Unmarshal(rawData, &sc.rec.Data); err != nil {
			return nil, "", fmt.Errorf("decode data: %w", err)
		}
		all = append(all, sc)
	}
	if err := rows.Err(); err != nil {
		return nil, "", fmt.Errorf("iterate rows: %w", err)
	}

	next := ""
	if len(all) > q.Limit {
		last := all[q.Limit-1]
		all = all[:q.Limit]
		next = query.NextCursor(q, last.sortKey, last.rec.ID.String())
	}

	items := make([]Record, len(all))
	for i, sc := range all {
		items[i] = sc.rec
	}
	return items, next, nil
}

func (s *Service) Update(ctx context.Context, cmd UpdateCmd) (Record, error) {
	dec, aerr := s.authorize(ctx, policy.Request{
		Workspace: cmd.Workspace, Actor: cmd.Actor, Collection: cmd.Collection,
		Target: cmd.ID, Operation: policy.OpUpdate,
	})
	if aerr != nil {
		return Record{}, aerr
	}
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
		sv, verr := s.schemaVersionParam(ctx, cmd.Collection, cmd.Data)
		if verr != nil {
			return verr
		}
		if err := appendEvent(ctx, qtx, eventRow{
			Workspace: cmd.Workspace, Collection: cmd.Collection, RecordID: cmd.ID,
			Type: "update", Revision: next, State: cmd.Data, Actor: cmd.Actor,
			IdempotencyKey: cmd.IdempotencyKey,
			Trace:          s.policyTrace(dec, policy.OpUpdate),
			SchemaVersion:  sv,
		}); err != nil {
			return err
		}
		if err := qtx.UpdateRecordData(ctx, db.UpdateRecordDataParams{
			WorkspaceID: cmd.Workspace, CollectionID: cmd.Collection, ID: cmd.ID,
			Data: mustJSON(cmd.Data), Revision: next, Actor: textOrNull(cmd.Actor),
			SchemaVersion: sv,
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
	dec, err := s.authorize(ctx, policy.Request{
		Workspace: ws, Actor: actor, Collection: col, Target: id, Operation: policy.OpDelete,
	})
	if err != nil {
		return err
	}
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
			Trace:         s.policyTrace(dec, policy.OpDelete),
			SchemaVersion: row.SchemaVersion,
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
	Trace          []byte
	SchemaVersion  pgtype.Int4
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
		Trace:          e.Trace,
		SchemaVersion:  e.SchemaVersion,
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
