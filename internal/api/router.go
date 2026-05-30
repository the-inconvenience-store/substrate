package api

import (
	"net/http"

	"github.com/substrate/substrate/internal/auth"
	"github.com/substrate/substrate/internal/collection"
	"github.com/substrate/substrate/internal/httpx"
	"github.com/substrate/substrate/internal/record"
	"github.com/substrate/substrate/internal/workspace"
)

// Deps holds the collaborators the API needs.
type Deps struct {
	Workspaces  *workspace.Service
	Collections *collection.Service
	Records     *record.Service
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

	protected := auth.Middleware(d.Workspaces)(api)
	mux.Handle("/v1/", protected)
	return mux
}
