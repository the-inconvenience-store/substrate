package auth

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/google/uuid"
)

type fakeVerifier struct {
	ws  uuid.UUID
	err error
}

func (f fakeVerifier) VerifyKey(_ context.Context, _ string) (uuid.UUID, error) {
	return f.ws, f.err
}

func TestMiddleware_ValidKey(t *testing.T) {
	ws := uuid.New()
	mw := Middleware(fakeVerifier{ws: ws})
	var gotWS uuid.UUID
	var gotActor string
	next := http.HandlerFunc(func(_ http.ResponseWriter, r *http.Request) {
		gotWS = WorkspaceFrom(r.Context())
		gotActor = ActorFrom(r.Context())
	})
	req := httptest.NewRequest("GET", "/", nil)
	req.Header.Set("Authorization", "Bearer sk_abc")
	req.Header.Set("X-Substrate-Actor", "agent-7")
	rec := httptest.NewRecorder()

	mw(next).ServeHTTP(rec, req)

	if gotWS != ws {
		t.Errorf("workspace = %s, want %s", gotWS, ws)
	}
	if gotActor != "agent-7" {
		t.Errorf("actor = %q, want agent-7", gotActor)
	}
}

func TestMiddleware_MissingKey(t *testing.T) {
	mw := Middleware(fakeVerifier{err: errors.New("nope")})
	next := http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		t.Fatal("next should not run")
		_ = w
	})
	req := httptest.NewRequest("GET", "/", nil)
	rec := httptest.NewRecorder()
	mw(next).ServeHTTP(rec, req)
	if rec.Code != http.StatusUnauthorized {
		t.Errorf("status = %d, want 401", rec.Code)
	}
}
