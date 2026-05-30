//go:build integration

package api

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/substrate/substrate/internal/collection"
	"github.com/substrate/substrate/internal/record"
	"github.com/substrate/substrate/internal/store"
	"github.com/substrate/substrate/internal/workspace"
)

func jsonBody(s string) *strings.Reader { return strings.NewReader(s) }

func TestAdminBootstrap(t *testing.T) {
	pool := store.NewTestPool(t)
	srv := httptest.NewServer(NewRouter(Deps{
		Workspaces:  workspace.New(pool),
		Collections: collection.New(pool),
		Records:     record.New(pool, nil),
		AdminToken:  "secret",
	}))
	defer srv.Close()

	// Wrong token rejected.
	req, _ := http.NewRequest("POST", srv.URL+"/admin/workspaces", nil)
	req.Header.Set("X-Admin-Token", "nope")
	resp, _ := http.DefaultClient.Do(req)
	if resp.StatusCode != http.StatusUnauthorized {
		t.Fatalf("bad token status = %d, want 401", resp.StatusCode)
	}
	resp.Body.Close()

	// Create workspace.
	req, _ = http.NewRequest("POST", srv.URL+"/admin/workspaces", jsonBody(`{"name":"acme"}`))
	req.Header.Set("X-Admin-Token", "secret")
	resp, _ = http.DefaultClient.Do(req)
	if resp.StatusCode != http.StatusCreated {
		t.Fatalf("create ws status = %d", resp.StatusCode)
	}
	var ws struct {
		ID string `json:"id"`
	}
	_ = json.NewDecoder(resp.Body).Decode(&ws)
	resp.Body.Close()

	// Create key.
	req, _ = http.NewRequest("POST", srv.URL+"/admin/workspaces/"+ws.ID+"/api-keys", jsonBody(`{"label":"default"}`))
	req.Header.Set("X-Admin-Token", "secret")
	resp, _ = http.DefaultClient.Do(req)
	if resp.StatusCode != http.StatusCreated {
		t.Fatalf("create key status = %d", resp.StatusCode)
	}
	var key struct {
		Key string `json:"key"`
	}
	_ = json.NewDecoder(resp.Body).Decode(&key)
	resp.Body.Close()
	if key.Key == "" {
		t.Fatal("expected plaintext key in response")
	}
}
