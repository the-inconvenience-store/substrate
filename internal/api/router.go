package api

import (
	"net/http"

	"github.com/substrate/substrate/internal/auth"
	"github.com/substrate/substrate/internal/collection"
	"github.com/substrate/substrate/internal/httpx"
	"github.com/substrate/substrate/internal/record"
	"github.com/substrate/substrate/internal/schema"
	"github.com/substrate/substrate/internal/workspace"
)

// Deps holds the collaborators the API needs.
type Deps struct {
	Workspaces  *workspace.Service
	Collections *collection.Service
	Records     *record.Service
	Schemas     *schema.Service
	AdminToken  string
}

// NewRouter builds the HTTP handler for the whole API.
func NewRouter(d Deps) http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("GET /healthz", func(w http.ResponseWriter, _ *http.Request) {
		httpx.JSON(w, http.StatusOK, map[string]string{"status": "ok"})
	})

	if d.Records == nil {
		return mux // health-only mode (used by the scaffold test)
	}

	h := &handlers{collections: d.Collections, records: d.Records}

	api := http.NewServeMux()
	api.HandleFunc("POST /v1/collections", h.createCollection)
	api.HandleFunc("POST /v1/collections/{collection}/records", h.createRecord)
	api.HandleFunc("GET /v1/collections/{collection}/records/{id}", h.getRecord)
	api.HandleFunc("PATCH /v1/collections/{collection}/records/{id}", h.updateRecord)
	api.HandleFunc("DELETE /v1/collections/{collection}/records/{id}", h.deleteRecord)
	api.HandleFunc("GET /v1/collections/{collection}/records/{id}/history", h.recordHistory)
	api.HandleFunc("POST /v1/collections/{collection}/records/{id}/revert", h.revertRecord)

	sh := &schemaHandlers{h: h, schemas: d.Schemas}
	api.HandleFunc("POST /v1/collections/{collection}/schemas", sh.register)
	api.HandleFunc("GET /v1/collections/{collection}/schemas", sh.list)
	api.HandleFunc("GET /v1/collections/{collection}/schemas/{version}", sh.get)
	// NOTE: Go 1.22+ ServeMux does not allow wildcard segments with a literal suffix
	// (e.g. "{version}:activate" panics). Using subpath form instead.
	api.HandleFunc("POST /v1/collections/{collection}/schemas/{version}/activate", sh.activate)
	api.HandleFunc("POST /v1/collections/{collection}/schemas/{version}/deprecate", sh.deprecate)

	protected := auth.Middleware(d.Workspaces)(api)
	mux.Handle("/v1/", protected)

	admin := &adminHandlers{workspaces: d.Workspaces, token: d.AdminToken}
	mux.HandleFunc("POST /admin/workspaces", admin.createWorkspace)
	mux.HandleFunc("POST /admin/workspaces/{ws}/api-keys", admin.createKey)

	return mux
}
