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
			if err := s.activateTx(ctx, qtx, cmd.Collection, col.ActiveSchemaVersion, next, cmd.Actor); err != nil {
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

// --- Task 3 stubs (replaced by Task 4) ---

// activateTx sets the collection's active schema version.
// Task 4 replaces this with the full implementation: deprecate prior + write schema_activated event.
func (s *Service) activateTx(ctx context.Context, q *db.Queries, col uuid.UUID, prior pgtype.Int4, version int32, actor string) error {
	return q.SetCollectionActiveVersion(ctx, db.SetCollectionActiveVersionParams{ID: col, ActiveSchemaVersion: pgtype.Int4{Int32: version, Valid: true}})
}

// checkCompatibleTx is a no-op stub; Task 4 implements the real compatibility gate.
func (s *Service) checkCompatibleTx(ctx context.Context, q *db.Queries, col uuid.UUID, priorVersion int32, candidate map[string]any, force bool) error {
	return nil // Task 4 implements the real compatibility gate
}

// --- helpers ---

func textOrNull(s string) pgtype.Text { return pgtype.Text{String: s, Valid: s != ""} }

func normStrings(in []string) []string {
	if in == nil {
		return []string{}
	}
	return in
}
