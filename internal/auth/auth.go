package auth

import (
	"context"
	"net/http"
	"strings"

	"github.com/google/uuid"

	"github.com/substrate/substrate/internal/apierr"
	"github.com/substrate/substrate/internal/httpx"
)

type ctxKey int

const (
	wsKey ctxKey = iota
	actorKey
)

// Verifier resolves a plaintext API key to a workspace id.
type Verifier interface {
	VerifyKey(ctx context.Context, plaintext string) (uuid.UUID, error)
}

// Middleware authenticates requests via API key and pins workspace + actor on the context.
func Middleware(v Verifier) func(http.Handler) http.Handler {
	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			key := bearer(r)
			if key == "" {
				writeUnauthorized(w, "missing api key")
				return
			}
			ws, err := v.VerifyKey(r.Context(), key)
			if err != nil {
				writeUnauthorized(w, "invalid api key")
				return
			}
			actor := r.Header.Get("X-Substrate-Actor")
			if actor == "" {
				actor = "anonymous"
			}
			ctx := context.WithValue(r.Context(), wsKey, ws)
			ctx = context.WithValue(ctx, actorKey, actor)
			next.ServeHTTP(w, r.WithContext(ctx))
		})
	}
}

func bearer(r *http.Request) string {
	if h := r.Header.Get("Authorization"); strings.HasPrefix(h, "Bearer ") {
		return strings.TrimPrefix(h, "Bearer ")
	}
	return r.Header.Get("X-Api-Key")
}

func writeUnauthorized(w http.ResponseWriter, msg string) {
	e := apierr.New(apierr.Unauthorized, msg)
	httpx.JSON(w, e.HTTPStatus(), httpx.Envelope{
		Error: httpx.EnvelopeError{Code: string(e.Code), Message: e.Message},
	})
}

// WorkspaceFrom returns the workspace id pinned by Middleware (uuid.Nil if absent).
func WorkspaceFrom(ctx context.Context) uuid.UUID {
	ws, _ := ctx.Value(wsKey).(uuid.UUID)
	return ws
}

// ActorFrom returns the actor pinned by Middleware ("" if absent).
func ActorFrom(ctx context.Context) string {
	a, _ := ctx.Value(actorKey).(string)
	return a
}
