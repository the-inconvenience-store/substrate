package httpx

import (
	"net/http"

	"github.com/substrate/substrate/internal/jsonx"
)

// JSON writes v as a JSON body with the given status code.
func JSON(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	if v != nil {
		_ = jsonx.NewEncoder(w).Encode(v)
	}
}

// Envelope is the standard error body shape.
type Envelope struct {
	Error EnvelopeError `json:"error"`
}

// EnvelopeError is the inner error object.
type EnvelopeError struct {
	Code    string         `json:"code"`
	Message string         `json:"message"`
	Details map[string]any `json:"details,omitempty"`
}
