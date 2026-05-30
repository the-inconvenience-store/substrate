package record

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgtype"

	"github.com/substrate/substrate/internal/apierr"
	"github.com/substrate/substrate/internal/db"
	"github.com/substrate/substrate/internal/policy"
	"github.com/substrate/substrate/internal/store"
)

// HistoryEntry is one event in a record's timeline.
type HistoryEntry struct {
	Revision  int64          `json:"revision"`
	Type      string         `json:"type"`
	Actor     string         `json:"actor"`
	State     map[string]any `json:"state"`
	CreatedAt time.Time      `json:"created_at"`
}

// AsOf selects a point in a record's history. Exactly one field should be set;
// precedence is Revision, then EventID, then Timestamp.
type AsOf struct {
	Revision  int64
	EventID   uuid.UUID
	Timestamp time.Time
}

// History returns the ordered event stream for a record.
// Returns NotFound if the record has no events.
func (s *Service) History(ctx context.Context, ws, col, id uuid.UUID, actor string) ([]HistoryEntry, error) {
	if _, err := s.authorize(ctx, policy.Request{
		Workspace: ws, Actor: actor, Collection: col, Target: id, Operation: policy.OpRead,
	}); err != nil {
		return nil, err
	}
	rows, err := s.q.ListRecordEvents(ctx, db.ListRecordEventsParams{
		WorkspaceID: ws, CollectionID: col, RecordID: id,
	})
	if err != nil {
		return nil, fmt.Errorf("history query: %w", err)
	}
	out := make([]HistoryEntry, 0, len(rows))
	for _, r := range rows {
		h := HistoryEntry{
			Revision:  r.Revision,
			Type:      r.Type,
			Actor:     r.Actor.String,
			CreatedAt: r.CreatedAt.Time,
		}
		if len(r.StateAfter) > 0 {
			_ = json.Unmarshal(r.StateAfter, &h.State)
		}
		out = append(out, h)
	}
	if len(out) == 0 {
		return nil, apierr.New(apierr.NotFound, "record not found")
	}
	return out, nil
}

// GetAsOf resolves the record's state at the requested point in time.
func (s *Service) GetAsOf(ctx context.Context, ws, col, id uuid.UUID, at AsOf, actor string) (Record, error) {
	if _, err := s.authorize(ctx, policy.Request{
		Workspace: ws, Actor: actor, Collection: col, Target: id, Operation: policy.OpRead,
	}); err != nil {
		return Record{}, err
	}
	state, rev, status, err := s.resolveAsOf(ctx, s.q, ws, col, id, at)
	if err != nil {
		return Record{}, err
	}
	return Record{ID: id, Collection: col, Data: state, Revision: rev, Status: status}, nil
}

// Revert appends a new forward "revert" event restoring the record to a prior point.
// The record is re-activated if it was soft-deleted. Revision bumps by 1.
func (s *Service) Revert(ctx context.Context, ws, col, id uuid.UUID, to AsOf, actor string) (Record, error) {
	dec, aerr := s.authorize(ctx, policy.Request{
		Workspace: ws, Actor: actor, Collection: col, Target: id, Operation: policy.OpUpdate,
	})
	if aerr != nil {
		return Record{}, aerr
	}
	var rec Record
	err := store.WithTx(ctx, s.pool, func(tx pgx.Tx) error {
		qtx := s.q.WithTx(tx)
		state, _, _, err := s.resolveAsOf(ctx, qtx, ws, col, id, to)
		if err != nil {
			return err
		}
		current, err := qtx.GetAnyRecordRevisionForUpdate(ctx, db.GetAnyRecordRevisionForUpdateParams{
			WorkspaceID: ws, CollectionID: col, ID: id,
		})
		if errors.Is(err, pgx.ErrNoRows) {
			return apierr.New(apierr.NotFound, "record not found")
		}
		if err != nil {
			return err
		}
		next := current + 1
		if err := appendEvent(ctx, qtx, eventRow{
			Workspace: ws, Collection: col, RecordID: id,
			Type: "revert", Revision: next, State: state, Actor: actor,
			Trace: s.policyTrace(dec, policy.OpUpdate),
		}); err != nil {
			return err
		}
		if err := qtx.RevertRecordData(ctx, db.RevertRecordDataParams{
			WorkspaceID: ws, CollectionID: col, ID: id,
			Data: mustJSON(state), Revision: next,
		}); err != nil {
			return err
		}
		rec = Record{ID: id, Collection: col, Data: state, Revision: next, Status: "active", Actor: actor}
		return nil
	})
	if err != nil {
		return Record{}, err
	}
	return rec, nil
}

// resolveAsOf reads the record's state at the requested point, using q (which may be the
// pool-bound or a tx-bound *db.Queries).
func (s *Service) resolveAsOf(ctx context.Context, q *db.Queries, ws, col, id uuid.UUID, at AsOf) (map[string]any, int64, string, error) {
	var (
		raw []byte
		rev int64
		typ string
		err error
	)
	switch {
	case at.Revision > 0:
		row, e := q.GetStateAtRevision(ctx, db.GetStateAtRevisionParams{
			WorkspaceID: ws, CollectionID: col, RecordID: id, Revision: at.Revision,
		})
		raw, rev, typ, err = row.StateAfter, row.Revision, row.Type, e
	case at.EventID != uuid.Nil:
		row, e := q.GetStateAtEvent(ctx, db.GetStateAtEventParams{
			WorkspaceID: ws, CollectionID: col, RecordID: id, ID: at.EventID,
		})
		raw, rev, typ, err = row.StateAfter, row.Revision, row.Type, e
	default:
		row, e := q.GetStateAtTimestamp(ctx, db.GetStateAtTimestampParams{
			WorkspaceID: ws, CollectionID: col, RecordID: id,
			CreatedAt: pgtype.Timestamptz{Time: at.Timestamp, Valid: true},
		})
		raw, rev, typ, err = row.StateAfter, row.Revision, row.Type, e
	}
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, 0, "", apierr.New(apierr.NotFound, "no state at requested point")
	}
	if err != nil {
		return nil, 0, "", fmt.Errorf("resolve as-of: %w", err)
	}
	state := map[string]any{}
	if len(raw) > 0 {
		_ = json.Unmarshal(raw, &state)
	}
	status := "active"
	if typ == "delete" {
		status = "deleted"
	}
	return state, rev, status, nil
}
