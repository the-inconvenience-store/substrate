// Package audit reads the workspace event stream with filters and keyset pagination.
package audit

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"strconv"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/substrate/substrate/internal/apierr"
)

const (
	defaultLimit = 50
	maxLimit     = 200
)

// Filter narrows the audit stream. Zero-valued fields are ignored.
type Filter struct {
	Collection *uuid.UUID
	Record     *uuid.UUID
	Actor      string
	Type       string
	Since      *time.Time
	Until      *time.Time
	Limit      int
	Cursor     string // opaque; wraps the last seen seq
}

// Entry is one audit row.
type Entry struct {
	Seq        int64           `json:"seq"`
	ID         uuid.UUID       `json:"id"`
	Type       string          `json:"type"`
	Collection uuid.UUID       `json:"collection_id"`
	Record     uuid.UUID       `json:"record_id"`
	Revision   int64           `json:"revision"`
	Actor      string          `json:"actor"`
	Trace      json.RawMessage `json:"trace,omitempty"`
	State      json.RawMessage `json:"state_after,omitempty"`
	CreatedAt  time.Time       `json:"created_at"`
}

// Service reads audit events.
type Service struct{ pool *pgxpool.Pool }

func New(pool *pgxpool.Pool) *Service { return &Service{pool: pool} }

func encodeCursor(seq int64) string {
	return base64.RawURLEncoding.EncodeToString([]byte(strconv.FormatInt(seq, 10)))
}

func decodeCursor(tok string) (int64, error) {
	raw, err := base64.RawURLEncoding.DecodeString(tok)
	if err != nil {
		return 0, apierr.New(apierr.BadRequest, "invalid cursor")
	}
	seq, err := strconv.ParseInt(string(raw), 10, 64)
	if err != nil {
		return 0, apierr.New(apierr.BadRequest, "invalid cursor")
	}
	return seq, nil
}

// List returns a page of audit entries (newest first) plus a next cursor ("" when last page).
func (s *Service) List(ctx context.Context, ws uuid.UUID, f Filter) ([]Entry, string, error) {
	limit := f.Limit
	if limit <= 0 {
		limit = defaultLimit
	}
	if limit > maxLimit {
		limit = maxLimit
	}

	sql := `SELECT seq, id, type, collection_id, record_id, revision, actor, trace, state_after, created_at
FROM events WHERE workspace_id = $1`
	args := []any{ws}
	add := func(cond string, v any) {
		args = append(args, v)
		sql += fmt.Sprintf(" AND %s $%d", cond, len(args))
	}
	if f.Collection != nil {
		add("collection_id =", *f.Collection)
	}
	if f.Record != nil {
		add("record_id =", *f.Record)
	}
	if f.Actor != "" {
		add("actor =", f.Actor)
	}
	if f.Type != "" {
		add("type =", f.Type)
	}
	if f.Since != nil {
		add("created_at >=", *f.Since)
	}
	if f.Until != nil {
		add("created_at <=", *f.Until)
	}
	if f.Cursor != "" {
		seq, err := decodeCursor(f.Cursor)
		if err != nil {
			return nil, "", err
		}
		add("seq <", seq)
	}
	sql += fmt.Sprintf(" ORDER BY seq DESC LIMIT %d", limit+1)

	rows, err := s.pool.Query(ctx, sql, args...)
	if err != nil {
		return nil, "", fmt.Errorf("audit query: %w", err)
	}
	defer rows.Close()

	var out []Entry
	for rows.Next() {
		var (
			e     Entry
			actor *string
			trace []byte
			state []byte
		)
		if err := rows.Scan(&e.Seq, &e.ID, &e.Type, &e.Collection, &e.Record, &e.Revision, &actor, &trace, &state, &e.CreatedAt); err != nil {
			return nil, "", fmt.Errorf("scan audit: %w", err)
		}
		if actor != nil {
			e.Actor = *actor
		}
		e.Trace = json.RawMessage(trace)
		e.State = json.RawMessage(state)
		out = append(out, e)
	}
	if err := rows.Err(); err != nil {
		return nil, "", fmt.Errorf("iterate audit: %w", err)
	}

	next := ""
	if len(out) > limit {
		last := out[limit-1]
		out = out[:limit]
		next = encodeCursor(last.Seq)
	}
	return out, next, nil
}
