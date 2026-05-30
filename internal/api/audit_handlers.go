package api

import (
	"net/http"
	"strconv"
	"time"

	"github.com/google/uuid"

	"github.com/substrate/substrate/internal/apierr"
	"github.com/substrate/substrate/internal/audit"
	"github.com/substrate/substrate/internal/auth"
	"github.com/substrate/substrate/internal/httpx"
)

type auditHandlers struct {
	h     *handlers
	audit *audit.Service
}

func (ah *auditHandlers) list(w http.ResponseWriter, r *http.Request) {
	ws := auth.WorkspaceFrom(r.Context())
	v := r.URL.Query()
	var f audit.Filter
	if name := v.Get("collection"); name != "" {
		c, err := ah.h.resolveCollection(r, name)
		if err != nil {
			writeErr(w, err)
			return
		}
		f.Collection = &c.ID
	}
	if rec := v.Get("record"); rec != "" {
		id, err := uuid.Parse(rec)
		if err != nil {
			writeErr(w, apierr.New(apierr.BadRequest, "invalid record id"))
			return
		}
		f.Record = &id
	}
	f.Actor = v.Get("actor")
	f.Type = v.Get("type")
	if s := v.Get("since"); s != "" {
		ts, err := time.Parse(time.RFC3339, s)
		if err != nil {
			writeErr(w, apierr.New(apierr.BadRequest, "since must be RFC3339"))
			return
		}
		f.Since = &ts
	}
	if u := v.Get("until"); u != "" {
		ts, err := time.Parse(time.RFC3339, u)
		if err != nil {
			writeErr(w, apierr.New(apierr.BadRequest, "until must be RFC3339"))
			return
		}
		f.Until = &ts
	}
	if l := v.Get("limit"); l != "" {
		n, err := strconv.Atoi(l)
		if err != nil {
			writeErr(w, apierr.New(apierr.BadRequest, "limit must be an integer"))
			return
		}
		f.Limit = n
	}
	f.Cursor = v.Get("cursor")

	items, next, err := ah.audit.List(r.Context(), ws, f)
	if err != nil {
		writeErr(w, err)
		return
	}
	if items == nil {
		items = []audit.Entry{}
	}
	httpx.JSON(w, http.StatusOK, map[string]any{"items": items, "next_cursor": next})
}
