package projection

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgtype"
	"github.com/jackc/pgx/v5/pgxpool"
	jsonschema "github.com/santhosh-tekuri/jsonschema/v6"

	"github.com/substrate/substrate/internal/apierr"
	"github.com/substrate/substrate/internal/db"
	"github.com/substrate/substrate/internal/schema"
	"github.com/substrate/substrate/internal/store"
)

const defaultBatch = 200

// SchemaResolver is the slice of the schema registry the backfiller needs.
type SchemaResolver interface {
	GetActive(ctx context.Context, col uuid.UUID) (schema.ActiveSchema, error)
}

// Report summarizes a backfill run.
type Report struct {
	Scanned   int `json:"scanned"`
	Migrated  int `json:"migrated"`
	Skipped   int `json:"skipped"`
	Remaining int `json:"remaining"`
}

// Backfiller advances active records toward a collection's active schema version.
type Backfiller struct {
	pool   *pgxpool.Pool
	q      *db.Queries
	schema SchemaResolver
}

func NewBackfiller(pool *pgxpool.Pool, sr SchemaResolver) *Backfiller {
	return &Backfiller{pool: pool, q: db.New(pool), schema: sr}
}

// Run advances every active record below the active version, in bounded keyset batches,
// applying schema defaults and re-validating. Invalid records are skipped (never altered).
func (b *Backfiller) Run(ctx context.Context, ws, col uuid.UUID, batch int) (Report, error) {
	var rep Report
	active, err := b.schema.GetActive(ctx, col)
	if err != nil {
		if e, ok := apierr.As(err); ok && e.Code == apierr.NotFound {
			return rep, nil // flexible collection: nothing to advance
		}
		return rep, fmt.Errorf("get active schema: %w", err)
	}
	compiled, err := compileSchema(active.Raw)
	if err != nil {
		return rep, err
	}
	if batch <= 0 || batch > defaultBatch {
		batch = defaultBatch
	}
	activeVer := pgtype.Int4{Int32: int32(active.Version), Valid: true}

	after := uuid.UUID{} // keyset cursor by id; uuid.Nil starts before all ids
	for {
		rows, err := b.q.ListRecordsBelowVersion(ctx, db.ListRecordsBelowVersionParams{
			CollectionID: col, SchemaVersion: activeVer, ID: after, Limit: int32(batch),
		})
		if err != nil {
			return rep, fmt.Errorf("list below version: %w", err)
		}
		if len(rows) == 0 {
			break
		}
		for _, row := range rows {
			rep.Scanned++
			migrated, err := b.migrateOne(ctx, ws, col, row.ID, int32(active.Version), active.Raw, compiled)
			if err != nil {
				return rep, err
			}
			if migrated {
				rep.Migrated++
			} else {
				rep.Skipped++
			}
			after = row.ID
		}
		if len(rows) < batch {
			break
		}
	}

	remaining, err := b.q.CountRecordsBelowVersion(ctx, db.CountRecordsBelowVersionParams{
		CollectionID: col, SchemaVersion: activeVer,
	})
	if err != nil {
		return rep, fmt.Errorf("count remaining: %w", err)
	}
	rep.Remaining = int(remaining)
	return rep, nil
}

func (b *Backfiller) migrateOne(ctx context.Context, ws, col, id uuid.UUID, activeVer int32, schemaRaw []byte, compiled *jsonschema.Schema) (bool, error) {
	var migrated bool
	err := store.WithTx(ctx, b.pool, func(tx pgx.Tx) error {
		qtx := b.q.WithTx(tx)
		row, err := qtx.GetRecordForUpdate(ctx, db.GetRecordForUpdateParams{WorkspaceID: ws, CollectionID: col, ID: id})
		if errors.Is(err, pgx.ErrNoRows) {
			return nil // deleted/gone since the scan
		}
		if err != nil {
			return err
		}
		if row.SchemaVersion.Valid && row.SchemaVersion.Int32 >= activeVer {
			return nil // advanced by a concurrent write
		}
		var data map[string]any
		if err := json.Unmarshal(row.Data, &data); err != nil {
			return fmt.Errorf("decode data: %w", err)
		}
		if data == nil {
			data = map[string]any{}
		}
		migratedData, _ := applyDefaults(schemaRaw, data)
		if err := compiled.Validate(migratedData); err != nil {
			return nil // invalid under active schema: skip, leave untouched
		}
		next := row.Revision + 1
		raw, err := json.Marshal(migratedData)
		if err != nil {
			return fmt.Errorf("encode migrated: %w", err)
		}
		sysActor := pgtype.Text{String: "system:backfill", Valid: true}
		sv := pgtype.Int4{Int32: activeVer, Valid: true}
		if err := qtx.AppendEvent(ctx, db.AppendEventParams{
			ID: uuid.New(), WorkspaceID: ws, CollectionID: col, RecordID: id,
			Type: "migration", Revision: next, StateAfter: raw,
			Actor: sysActor, IdempotencyKey: pgtype.Text{}, Trace: nil, SchemaVersion: sv,
		}); err != nil {
			return fmt.Errorf("append migration event: %w", err)
		}
		if err := qtx.UpdateRecordData(ctx, db.UpdateRecordDataParams{
			WorkspaceID: ws, CollectionID: col, ID: id,
			Data: raw, Revision: next, Actor: sysActor, SchemaVersion: sv,
		}); err != nil {
			return fmt.Errorf("update record: %w", err)
		}
		migrated = true
		return nil
	})
	return migrated, err
}

func compileSchema(raw []byte) (*jsonschema.Schema, error) {
	parsed, err := jsonschema.UnmarshalJSON(bytes.NewReader(raw))
	if err != nil {
		return nil, fmt.Errorf("parse active schema: %w", err)
	}
	c := jsonschema.NewCompiler()
	const id = "substrate://backfill.json"
	if err := c.AddResource(id, parsed); err != nil {
		return nil, fmt.Errorf("add active schema: %w", err)
	}
	sch, err := c.Compile(id)
	if err != nil {
		return nil, fmt.Errorf("compile active schema: %w", err)
	}
	return sch, nil
}
