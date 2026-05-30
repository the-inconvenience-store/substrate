package schema

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
	"github.com/substrate/substrate/internal/store"
)

// bytesReader adapts a byte slice to the io.Reader the jsonschema API expects.
// NOTE: This is a temporary copy kept here so Task 3 compiles without Task 5's
// validator.go. When Task 5 lands and adds the canonical bytesReader in validator.go
// (same package), this copy must be removed to avoid a duplicate-function error.
func bytesReader(b []byte) *bytes.Reader { return bytes.NewReader(b) }

// SchemaVersion is a registered, immutable schema version.
type SchemaVersion struct {
	Version       int            `json:"version"`
	Lifecycle     string         `json:"lifecycle"`
	IndexedFields []string       `json:"indexed_fields"`
	JSONSchema    map[string]any `json:"json_schema,omitempty"`
}

// RegisterCmd registers a new schema version.
type RegisterCmd struct {
	Workspace     uuid.UUID
	Collection    uuid.UUID
	Actor         string
	JSONSchema    map[string]any
	IndexedFields []string
	Activate      bool   // register + activate in one call
	Force         bool   // allow a breaking change on activation
	Rationale     string // recorded when Force is used
}

// ActiveSchema is the active version's number plus its raw document.
type ActiveSchema struct {
	Version int
	Raw     []byte
}

// Service manages the schema registry.
type Service struct {
	pool *pgxpool.Pool
	q    *db.Queries
}

func New(pool *pgxpool.Pool) *Service { return &Service{pool: pool, q: db.New(pool)} }

// compileSchema validates that doc is a usable JSON Schema (draft 2020-12).
func compileSchema(doc map[string]any) error {
	raw, err := json.Marshal(doc)
	if err != nil {
		return apierr.New(apierr.SchemaInvalid, "schema is not JSON-encodable")
	}
	c := jsonschema.NewCompiler()
	parsed, err := jsonschema.UnmarshalJSON(bytesReader(raw))
	if err != nil {
		return apierr.New(apierr.SchemaInvalid, "schema is not valid JSON")
	}
	const resID = "substrate://schema.json"
	if err := c.AddResource(resID, parsed); err != nil {
		return apierr.New(apierr.SchemaInvalid, fmt.Sprintf("invalid schema: %v", err))
	}
	if _, err := c.Compile(resID); err != nil {
		return apierr.New(apierr.SchemaInvalid, fmt.Sprintf("invalid schema: %v", err))
	}
	return nil
}

// Register inserts a new immutable version. First schema auto-activates; otherwise
// it is a draft unless Activate is set.
func (s *Service) Register(ctx context.Context, cmd RegisterCmd) (SchemaVersion, error) {
	if err := compileSchema(cmd.JSONSchema); err != nil {
		return SchemaVersion{}, err
	}
	rawSchema, _ := json.Marshal(cmd.JSONSchema)
	idxRaw, _ := json.Marshal(normStrings(cmd.IndexedFields))

	var result SchemaVersion
	err := store.WithTx(ctx, s.pool, func(tx pgx.Tx) error {
		qtx := s.q.WithTx(tx)
		col, err := qtx.LockCollection(ctx, cmd.Collection)
		if errors.Is(err, pgx.ErrNoRows) {
			return apierr.New(apierr.NotFound, "collection not found")
		}
		if err != nil {
			return err
		}
		isFirst := !col.ActiveSchemaVersion.Valid
		next, err := qtx.NextSchemaVersion(ctx, cmd.Collection)
		if err != nil {
			return err
		}
		lifecycle := "draft"
		if isFirst || cmd.Activate {
			lifecycle = "active"
		}
		// Compatibility gate when activating over an existing active version.
		if !isFirst && cmd.Activate {
			if err := s.checkCompatibleTx(ctx, qtx, cmd.Collection, col.ActiveSchemaVersion.Int32, cmd.JSONSchema, cmd.Force); err != nil {
				return err
			}
		}
		row, err := qtx.InsertSchema(ctx, db.InsertSchemaParams{
			ID: uuid.New(), CollectionID: cmd.Collection, WorkspaceID: cmd.Workspace,
			Version: next, JsonSchema: rawSchema, Lifecycle: lifecycle,
			IndexedFields: idxRaw, Rationale: textOrNull(cmd.Rationale),
			CreatedBy: textOrNull(cmd.Actor),
		})
		if err != nil {
			return err
		}
		if lifecycle == "active" {
			if err := s.activateTx(ctx, qtx, cmd.Collection, col.WorkspaceID, col.ActiveSchemaVersion, next, cmd.Actor); err != nil {
				return err
			}
		}
		result = SchemaVersion{Version: int(row.Version), Lifecycle: lifecycle, IndexedFields: normStrings(cmd.IndexedFields)}
		return nil
	})
	if err != nil {
		return SchemaVersion{}, err
	}
	return result, nil
}

// GetActive returns the collection's active schema version + raw document.
func (s *Service) GetActive(ctx context.Context, col uuid.UUID) (ActiveSchema, error) {
	row, err := s.q.GetActiveSchema(ctx, col)
	if errors.Is(err, pgx.ErrNoRows) {
		return ActiveSchema{}, apierr.New(apierr.NotFound, "no active schema")
	}
	if err != nil {
		return ActiveSchema{}, fmt.Errorf("get active schema: %w", err)
	}
	return ActiveSchema{Version: int(row.Version), Raw: row.JsonSchema}, nil
}

// Get returns one version including its full document.
func (s *Service) Get(ctx context.Context, col uuid.UUID, version int) (SchemaVersion, error) {
	row, err := s.q.GetSchema(ctx, db.GetSchemaParams{CollectionID: col, Version: int32(version)})
	if errors.Is(err, pgx.ErrNoRows) {
		return SchemaVersion{}, apierr.New(apierr.NotFound, "schema version not found")
	}
	if err != nil {
		return SchemaVersion{}, fmt.Errorf("get schema: %w", err)
	}
	var doc map[string]any
	_ = json.Unmarshal(row.JsonSchema, &doc)
	var idx []string
	_ = json.Unmarshal(row.IndexedFields, &idx)
	return SchemaVersion{Version: int(row.Version), Lifecycle: row.Lifecycle, IndexedFields: idx, JSONSchema: doc}, nil
}

// List returns all versions (without full documents).
func (s *Service) List(ctx context.Context, col uuid.UUID) ([]SchemaVersion, error) {
	rows, err := s.q.ListSchemas(ctx, col)
	if err != nil {
		return nil, fmt.Errorf("list schemas: %w", err)
	}
	out := make([]SchemaVersion, 0, len(rows))
	for _, r := range rows {
		var idx []string
		_ = json.Unmarshal(r.IndexedFields, &idx)
		out = append(out, SchemaVersion{Version: int(r.Version), Lifecycle: r.Lifecycle, IndexedFields: idx})
	}
	return out, nil
}

// checkCompatibleTx loads the prior active schema and runs Classify. Returns
// schema_incompatible if any breaking change is detected (unless force is true).
func (s *Service) checkCompatibleTx(ctx context.Context, q *db.Queries, col uuid.UUID, priorVersion int32, candidate map[string]any, force bool) error {
	if force {
		return nil
	}
	prior, err := q.GetSchema(ctx, db.GetSchemaParams{CollectionID: col, Version: priorVersion})
	if err != nil {
		return fmt.Errorf("load prior schema: %w", err)
	}
	var priorDoc map[string]any
	if err := json.Unmarshal(prior.JsonSchema, &priorDoc); err != nil {
		return fmt.Errorf("decode prior schema: %w", err)
	}
	changes := Classify(priorDoc, candidate)
	var breaking []Change
	for _, c := range changes {
		if c.Breaking {
			breaking = append(breaking, c)
		}
	}
	if len(breaking) > 0 {
		return apierr.New(apierr.SchemaIncompatible, "breaking schema change requires force").
			WithDetails(map[string]any{"breaking_changes": breaking})
	}
	return nil
}

// activateTx deprecates the previously-active version, marks the target active,
// moves the collection pointer, and appends a schema_activated event.
func (s *Service) activateTx(ctx context.Context, q *db.Queries, col uuid.UUID, ws uuid.UUID, prior pgtype.Int4, version int32, actor string) error {
	// Deprecate the previously-active version if any and different.
	if prior.Valid && prior.Int32 != version {
		if err := q.SetSchemaLifecycle(ctx, db.SetSchemaLifecycleParams{
			CollectionID: col, Version: prior.Int32, Lifecycle: "deprecated",
		}); err != nil {
			return err
		}
	}
	if err := q.SetSchemaLifecycle(ctx, db.SetSchemaLifecycleParams{
		CollectionID: col, Version: version, Lifecycle: "active",
	}); err != nil {
		return err
	}
	if err := q.SetCollectionActiveVersion(ctx, db.SetCollectionActiveVersionParams{
		ID: col, ActiveSchemaVersion: pgtype.Int4{Int32: version, Valid: true},
	}); err != nil {
		return err
	}
	return appendSchemaEvent(ctx, q, col, ws, "schema_activated", int64(version), actor)
}

// Activate makes an existing draft/deprecated version the active one.
func (s *Service) Activate(ctx context.Context, ws, col uuid.UUID, version int, actor string, force bool, rationale string) error {
	return store.WithTx(ctx, s.pool, func(tx pgx.Tx) error {
		qtx := s.q.WithTx(tx)
		c, err := qtx.LockCollection(ctx, col)
		if errors.Is(err, pgx.ErrNoRows) {
			return apierr.New(apierr.NotFound, "collection not found")
		}
		if err != nil {
			return err
		}
		target, err := qtx.GetSchema(ctx, db.GetSchemaParams{CollectionID: col, Version: int32(version)})
		if errors.Is(err, pgx.ErrNoRows) {
			return apierr.New(apierr.NotFound, "schema version not found")
		}
		if err != nil {
			return err
		}
		if c.ActiveSchemaVersion.Valid && c.ActiveSchemaVersion.Int32 != int32(version) {
			var candidate map[string]any
			if err := json.Unmarshal(target.JsonSchema, &candidate); err != nil {
				return fmt.Errorf("decode candidate: %w", err)
			}
			if err := s.checkCompatibleTx(ctx, qtx, col, c.ActiveSchemaVersion.Int32, candidate, force); err != nil {
				return err
			}
		}
		return s.activateTx(ctx, qtx, col, c.WorkspaceID, c.ActiveSchemaVersion, int32(version), actor)
	})
}

// Deprecate marks a non-active version deprecated.
func (s *Service) Deprecate(ctx context.Context, col uuid.UUID, version int) error {
	return store.WithTx(ctx, s.pool, func(tx pgx.Tx) error {
		qtx := s.q.WithTx(tx)
		c, err := qtx.LockCollection(ctx, col)
		if errors.Is(err, pgx.ErrNoRows) {
			return apierr.New(apierr.NotFound, "collection not found")
		}
		if err != nil {
			return err
		}
		if c.ActiveSchemaVersion.Valid && c.ActiveSchemaVersion.Int32 == int32(version) {
			return apierr.New(apierr.Conflict, "cannot deprecate the active version; activate another first")
		}
		return qtx.SetSchemaLifecycle(ctx, db.SetSchemaLifecycleParams{
			CollectionID: col, Version: int32(version), Lifecycle: "deprecated",
		})
	})
}

// appendSchemaEvent records a schema lifecycle change on the events timeline.
// The event is collection-scoped: record_id is the collection id; revision is the schema version.
func appendSchemaEvent(ctx context.Context, q *db.Queries, col uuid.UUID, ws uuid.UUID, typ string, version int64, actor string) error {
	return q.AppendEvent(ctx, db.AppendEventParams{
		ID: uuid.New(), WorkspaceID: ws, CollectionID: col, RecordID: col,
		Type: typ, Revision: version, StateAfter: []byte("{}"),
		Actor: textOrNull(actor), IdempotencyKey: textOrNull(""),
	})
}

// --- helpers ---

func textOrNull(s string) pgtype.Text { return pgtype.Text{String: s, Valid: s != ""} }

func normStrings(in []string) []string {
	if in == nil {
		return []string{}
	}
	return in
}
