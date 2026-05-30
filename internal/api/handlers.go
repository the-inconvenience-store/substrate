package api

import (
	"encoding/json"
	"net/http"
	"strconv"
	"time"

	"github.com/google/uuid"

	"github.com/substrate/substrate/internal/apierr"
	"github.com/substrate/substrate/internal/auth"
	"github.com/substrate/substrate/internal/collection"
	"github.com/substrate/substrate/internal/httpx"
	"github.com/substrate/substrate/internal/projection"
	"github.com/substrate/substrate/internal/query"
	"github.com/substrate/substrate/internal/record"
	"github.com/substrate/substrate/internal/workspace"
)

func writeErr(w http.ResponseWriter, err error) {
	e, ok := apierr.As(err)
	if !ok {
		e = apierr.New(apierr.Internal, "internal error")
	}
	httpx.JSON(w, e.HTTPStatus(), httpx.Envelope{
		Error: httpx.EnvelopeError{Code: string(e.Code), Message: e.Message, Details: e.Details},
	})
}

// resolveCollection looks up a collection by name within the request's workspace.
func (h *handlers) resolveCollection(r *http.Request, name string) (collection.Collection, error) {
	ws := auth.WorkspaceFrom(r.Context())
	return h.collections.GetByName(r.Context(), ws, name)
}

type handlers struct {
	collections *collection.Service
	records     *record.Service
}

// --- collections ---

func (h *handlers) createCollection(w http.ResponseWriter, r *http.Request) {
	var body struct {
		Name         string `json:"name"`
		Level        string `json:"level"`
		AutoBackfill bool   `json:"auto_backfill"`
	}
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
		writeErr(w, apierr.New(apierr.BadRequest, "invalid json"))
		return
	}
	c, err := h.collections.Create(r.Context(), auth.WorkspaceFrom(r.Context()), body.Name)
	if err != nil {
		writeErr(w, err)
		return
	}
	if body.AutoBackfill {
		if err := h.collections.SetAutoBackfill(r.Context(), c.WorkspaceID, c.ID, true); err != nil {
			writeErr(w, err)
			return
		}
		c.AutoBackfill = true
	}
	httpx.JSON(w, http.StatusCreated, c)
}

// --- records ---

func (h *handlers) createRecord(w http.ResponseWriter, r *http.Request) {
	c, err := h.resolveCollection(r, r.PathValue("collection"))
	if err != nil {
		writeErr(w, err)
		return
	}
	var body struct {
		Data map[string]any `json:"data"`
	}
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
		writeErr(w, apierr.New(apierr.BadRequest, "invalid json"))
		return
	}
	rec, err := h.records.Create(r.Context(), record.CreateCmd{
		Workspace: c.WorkspaceID, Collection: c.ID, Actor: auth.ActorFrom(r.Context()),
		Data: body.Data, IdempotencyKey: r.Header.Get("Idempotency-Key"),
	})
	if err != nil {
		writeErr(w, err)
		return
	}
	setETag(w, rec.Revision)
	httpx.JSON(w, http.StatusCreated, rec)
}

func (h *handlers) listRecords(w http.ResponseWriter, r *http.Request) {
	c, err := h.resolveCollection(r, r.PathValue("collection"))
	if err != nil {
		writeErr(w, err)
		return
	}
	v := r.URL.Query()
	q, err := query.Parse(v["filter"], v.Get("sort"), v.Get("limit"), v.Get("cursor"))
	if err != nil {
		writeErr(w, err)
		return
	}
	items, next, err := h.records.List(r.Context(), c.WorkspaceID, c.ID, auth.ActorFrom(r.Context()), q)
	if err != nil {
		writeErr(w, err)
		return
	}
	if items == nil {
		items = []record.Record{}
	}
	httpx.JSON(w, http.StatusOK, map[string]any{"items": items, "next_cursor": next})
}

func (h *handlers) getRecord(w http.ResponseWriter, r *http.Request) {
	c, err := h.resolveCollection(r, r.PathValue("collection"))
	if err != nil {
		writeErr(w, err)
		return
	}
	id, err := uuid.Parse(r.PathValue("id"))
	if err != nil {
		writeErr(w, apierr.New(apierr.BadRequest, "invalid id"))
		return
	}
	if asOf := r.URL.Query().Get("as_of"); asOf != "" {
		at, perr := parseAsOf(asOf)
		if perr != nil {
			writeErr(w, perr)
			return
		}
		rec, err := h.records.GetAsOf(r.Context(), c.WorkspaceID, c.ID, id, at, auth.ActorFrom(r.Context()))
		if err != nil {
			writeErr(w, err)
			return
		}
		httpx.JSON(w, http.StatusOK, rec)
		return
	}
	rec, err := h.records.Get(r.Context(), c.WorkspaceID, c.ID, id, auth.ActorFrom(r.Context()))
	if err != nil {
		writeErr(w, err)
		return
	}
	setETag(w, rec.Revision)
	httpx.JSON(w, http.StatusOK, rec)
}

func (h *handlers) updateRecord(w http.ResponseWriter, r *http.Request) {
	c, err := h.resolveCollection(r, r.PathValue("collection"))
	if err != nil {
		writeErr(w, err)
		return
	}
	id, err := uuid.Parse(r.PathValue("id"))
	if err != nil {
		writeErr(w, apierr.New(apierr.BadRequest, "invalid id"))
		return
	}
	rev, err := parseIfMatch(r.Header.Get("If-Match"))
	if err != nil {
		writeErr(w, err)
		return
	}
	var body struct {
		Data map[string]any `json:"data"`
	}
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
		writeErr(w, apierr.New(apierr.BadRequest, "invalid json"))
		return
	}
	rec, err := h.records.Update(r.Context(), record.UpdateCmd{
		Workspace: c.WorkspaceID, Collection: c.ID, ID: id, ExpectedRevision: rev,
		Actor: auth.ActorFrom(r.Context()), Data: body.Data,
		IdempotencyKey: r.Header.Get("Idempotency-Key"),
	})
	if err != nil {
		writeErr(w, err)
		return
	}
	setETag(w, rec.Revision)
	httpx.JSON(w, http.StatusOK, rec)
}

func (h *handlers) deleteRecord(w http.ResponseWriter, r *http.Request) {
	c, err := h.resolveCollection(r, r.PathValue("collection"))
	if err != nil {
		writeErr(w, err)
		return
	}
	id, err := uuid.Parse(r.PathValue("id"))
	if err != nil {
		writeErr(w, apierr.New(apierr.BadRequest, "invalid id"))
		return
	}
	if err := h.records.Delete(r.Context(), c.WorkspaceID, c.ID, id, auth.ActorFrom(r.Context())); err != nil {
		writeErr(w, err)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

func (h *handlers) recordHistory(w http.ResponseWriter, r *http.Request) {
	c, err := h.resolveCollection(r, r.PathValue("collection"))
	if err != nil {
		writeErr(w, err)
		return
	}
	id, err := uuid.Parse(r.PathValue("id"))
	if err != nil {
		writeErr(w, apierr.New(apierr.BadRequest, "invalid id"))
		return
	}
	hist, err := h.records.History(r.Context(), c.WorkspaceID, c.ID, id, auth.ActorFrom(r.Context()))
	if err != nil {
		writeErr(w, err)
		return
	}
	httpx.JSON(w, http.StatusOK, hist)
}

func (h *handlers) revertRecord(w http.ResponseWriter, r *http.Request) {
	c, err := h.resolveCollection(r, r.PathValue("collection"))
	if err != nil {
		writeErr(w, err)
		return
	}
	id, err := uuid.Parse(r.PathValue("id"))
	if err != nil {
		writeErr(w, apierr.New(apierr.BadRequest, "invalid id"))
		return
	}
	var body struct {
		To string `json:"to"`
	}
	_ = json.NewDecoder(r.Body).Decode(&body)
	at, perr := parseAsOf(body.To)
	if perr != nil {
		writeErr(w, perr)
		return
	}
	rec, err := h.records.Revert(r.Context(), c.WorkspaceID, c.ID, id, at, auth.ActorFrom(r.Context()))
	if err != nil {
		writeErr(w, err)
		return
	}
	setETag(w, rec.Revision)
	httpx.JSON(w, http.StatusOK, rec)
}

// --- helpers ---

func setETag(w http.ResponseWriter, rev int64) {
	w.Header().Set("ETag", strconv.Quote(strconv.FormatInt(rev, 10)))
}

func parseIfMatch(h string) (int64, error) {
	if h == "" {
		return 0, apierr.New(apierr.BadRequest, "If-Match header required for update")
	}
	unq, err := strconv.Unquote(h)
	if err != nil {
		unq = h
	}
	rev, err := strconv.ParseInt(unq, 10, 64)
	if err != nil {
		return 0, apierr.New(apierr.BadRequest, "invalid If-Match revision")
	}
	return rev, nil
}

func parseAsOf(s string) (record.AsOf, error) {
	if rev, err := strconv.ParseInt(s, 10, 64); err == nil {
		return record.AsOf{Revision: rev}, nil
	}
	if id, err := uuid.Parse(s); err == nil {
		return record.AsOf{EventID: id}, nil
	}
	if ts, err := parseTime(s); err == nil {
		return record.AsOf{Timestamp: ts}, nil
	}
	return record.AsOf{}, apierr.New(apierr.BadRequest, "as_of must be a revision, event id, or RFC3339 timestamp")
}

func parseTime(s string) (time.Time, error) {
	return time.Parse(time.RFC3339, s)
}

// --- admin handlers ---

type adminHandlers struct {
	workspaces *workspace.Service
	token      string
	replayer   *projection.Replayer
}

func (a *adminHandlers) authed(r *http.Request) bool {
	return a.token != "" && r.Header.Get("X-Admin-Token") == a.token
}

func (a *adminHandlers) createWorkspace(w http.ResponseWriter, r *http.Request) {
	if !a.authed(r) {
		writeErr(w, apierr.New(apierr.Unauthorized, "invalid admin token"))
		return
	}
	var body struct {
		Name string `json:"name"`
	}
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
		writeErr(w, apierr.New(apierr.BadRequest, "invalid json"))
		return
	}
	ws, err := a.workspaces.CreateWorkspace(r.Context(), body.Name)
	if err != nil {
		writeErr(w, err)
		return
	}
	httpx.JSON(w, http.StatusCreated, ws)
}

func (a *adminHandlers) createKey(w http.ResponseWriter, r *http.Request) {
	if !a.authed(r) {
		writeErr(w, apierr.New(apierr.Unauthorized, "invalid admin token"))
		return
	}
	wsID, err := uuid.Parse(r.PathValue("ws"))
	if err != nil {
		writeErr(w, apierr.New(apierr.BadRequest, "invalid workspace id"))
		return
	}
	var body struct {
		Label string `json:"label"`
	}
	_ = json.NewDecoder(r.Body).Decode(&body)
	plaintext, id, err := a.workspaces.CreateAPIKey(r.Context(), wsID, body.Label)
	if err != nil {
		writeErr(w, err)
		return
	}
	httpx.JSON(w, http.StatusCreated, map[string]any{"id": id, "key": plaintext})
}
