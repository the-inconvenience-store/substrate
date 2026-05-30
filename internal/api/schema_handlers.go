package api

import (
	"encoding/json"
	"net/http"
	"strconv"

	"github.com/substrate/substrate/internal/apierr"
	"github.com/substrate/substrate/internal/auth"
	"github.com/substrate/substrate/internal/httpx"
	"github.com/substrate/substrate/internal/schema"
)

type schemaHandlers struct {
	h       *handlers
	schemas *schema.Service
}

func (sh *schemaHandlers) register(w http.ResponseWriter, r *http.Request) {
	c, err := sh.h.resolveCollection(r, r.PathValue("collection"))
	if err != nil {
		writeErr(w, err)
		return
	}
	var body struct {
		JSONSchema    map[string]any `json:"json_schema"`
		IndexedFields []string       `json:"indexed_fields"`
		Activate      bool           `json:"activate"`
		Force         bool           `json:"force"`
		Rationale     string         `json:"rationale"`
	}
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
		writeErr(w, apierr.New(apierr.BadRequest, "invalid json"))
		return
	}
	out, err := sh.schemas.Register(r.Context(), schema.RegisterCmd{
		Workspace: c.WorkspaceID, Collection: c.ID, Actor: auth.ActorFrom(r.Context()),
		JSONSchema: body.JSONSchema, IndexedFields: body.IndexedFields,
		Activate: body.Activate, Force: body.Force, Rationale: body.Rationale,
	})
	if err != nil {
		writeErr(w, err)
		return
	}
	httpx.JSON(w, http.StatusCreated, out)
}

func (sh *schemaHandlers) list(w http.ResponseWriter, r *http.Request) {
	c, err := sh.h.resolveCollection(r, r.PathValue("collection"))
	if err != nil {
		writeErr(w, err)
		return
	}
	out, err := sh.schemas.List(r.Context(), c.ID)
	if err != nil {
		writeErr(w, err)
		return
	}
	httpx.JSON(w, http.StatusOK, out)
}

func (sh *schemaHandlers) get(w http.ResponseWriter, r *http.Request) {
	c, err := sh.h.resolveCollection(r, r.PathValue("collection"))
	if err != nil {
		writeErr(w, err)
		return
	}
	ver, err := strconv.Atoi(r.PathValue("version"))
	if err != nil {
		writeErr(w, apierr.New(apierr.BadRequest, "invalid version"))
		return
	}
	out, err := sh.schemas.Get(r.Context(), c.ID, ver)
	if err != nil {
		writeErr(w, err)
		return
	}
	httpx.JSON(w, http.StatusOK, out)
}

func (sh *schemaHandlers) activate(w http.ResponseWriter, r *http.Request) {
	c, err := sh.h.resolveCollection(r, r.PathValue("collection"))
	if err != nil {
		writeErr(w, err)
		return
	}
	ver, err := strconv.Atoi(r.PathValue("version"))
	if err != nil {
		writeErr(w, apierr.New(apierr.BadRequest, "invalid version"))
		return
	}
	var body struct {
		Force     bool   `json:"force"`
		Rationale string `json:"rationale"`
	}
	_ = json.NewDecoder(r.Body).Decode(&body)
	if err := sh.schemas.Activate(r.Context(), c.WorkspaceID, c.ID, ver, auth.ActorFrom(r.Context()), body.Force, body.Rationale); err != nil {
		writeErr(w, err)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

func (sh *schemaHandlers) deprecate(w http.ResponseWriter, r *http.Request) {
	c, err := sh.h.resolveCollection(r, r.PathValue("collection"))
	if err != nil {
		writeErr(w, err)
		return
	}
	ver, err := strconv.Atoi(r.PathValue("version"))
	if err != nil {
		writeErr(w, apierr.New(apierr.BadRequest, "invalid version"))
		return
	}
	if err := sh.schemas.Deprecate(r.Context(), c.ID, ver); err != nil {
		writeErr(w, err)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}
