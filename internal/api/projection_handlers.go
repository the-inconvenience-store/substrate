package api

import (
	"encoding/json"
	"net/http"

	"github.com/google/uuid"

	"github.com/substrate/substrate/internal/apierr"
	"github.com/substrate/substrate/internal/httpx"
	"github.com/substrate/substrate/internal/projection"
)

type projectionHandlers struct {
	h          *handlers
	backfiller *projection.Backfiller
}

func (p *projectionHandlers) backfill(w http.ResponseWriter, r *http.Request) {
	c, err := p.h.resolveCollection(r, r.PathValue("collection"))
	if err != nil {
		writeErr(w, err)
		return
	}
	rep, err := p.backfiller.Run(r.Context(), c.WorkspaceID, c.ID, 0)
	if err != nil {
		writeErr(w, err)
		return
	}
	httpx.JSON(w, http.StatusOK, rep)
}

func (p *projectionHandlers) setAutoBackfill(w http.ResponseWriter, r *http.Request) {
	c, err := p.h.resolveCollection(r, r.PathValue("collection"))
	if err != nil {
		writeErr(w, err)
		return
	}
	var body struct {
		Enabled bool `json:"enabled"`
	}
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
		writeErr(w, apierr.New(apierr.BadRequest, "invalid json"))
		return
	}
	if err := p.h.collections.SetAutoBackfill(r.Context(), c.WorkspaceID, c.ID, body.Enabled); err != nil {
		writeErr(w, err)
		return
	}
	httpx.JSON(w, http.StatusOK, map[string]any{"collection": c.Name, "auto_backfill": body.Enabled})
}

// replay is admin-token gated (method on adminHandlers).
func (a *adminHandlers) replay(w http.ResponseWriter, r *http.Request) {
	if !a.authed(r) {
		writeErr(w, apierr.New(apierr.Unauthorized, "invalid admin token"))
		return
	}
	var body struct {
		WorkspaceID  string `json:"workspace_id"`
		CollectionID string `json:"collection_id"`
		RecordID     string `json:"record_id"`
	}
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
		writeErr(w, apierr.New(apierr.BadRequest, "invalid json"))
		return
	}
	ws, err := uuid.Parse(body.WorkspaceID)
	if err != nil {
		writeErr(w, apierr.New(apierr.BadRequest, "invalid workspace_id"))
		return
	}
	col, err := uuid.Parse(body.CollectionID)
	if err != nil {
		writeErr(w, apierr.New(apierr.BadRequest, "invalid collection_id"))
		return
	}
	if body.RecordID != "" {
		id, err := uuid.Parse(body.RecordID)
		if err != nil {
			writeErr(w, apierr.New(apierr.BadRequest, "invalid record_id"))
			return
		}
		ok, err := a.replayer.RebuildRecord(r.Context(), ws, col, id)
		if err != nil {
			writeErr(w, err)
			return
		}
		n := 0
		if ok {
			n = 1
		}
		httpx.JSON(w, http.StatusOK, map[string]any{"rebuilt": n})
		return
	}
	n, err := a.replayer.RebuildCollection(r.Context(), ws, col)
	if err != nil {
		writeErr(w, err)
		return
	}
	httpx.JSON(w, http.StatusOK, map[string]any{"rebuilt": n})
}
