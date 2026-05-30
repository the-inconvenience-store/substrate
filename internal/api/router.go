package api

import (
	"net/http"

	"github.com/substrate/substrate/internal/httpx"
)

// Deps holds the collaborators handlers need. Fields are added by later tasks.
type Deps struct{}

// NewRouter builds the HTTP handler for the whole API.
func NewRouter(d Deps) http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("GET /healthz", func(w http.ResponseWriter, _ *http.Request) {
		httpx.JSON(w, http.StatusOK, map[string]string{"status": "ok"})
	})
	return mux
}
