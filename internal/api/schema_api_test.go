//go:build integration

package api

import (
	"bytes"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/substrate/substrate/internal/collection"
	"github.com/substrate/substrate/internal/record"
	"github.com/substrate/substrate/internal/schema"
	"github.com/substrate/substrate/internal/store"
	"github.com/substrate/substrate/internal/workspace"
)

func newSchemaServer(t *testing.T) (*httptest.Server, string) {
	t.Helper()
	pool := store.NewTestPool(t)
	ws := workspace.New(pool)
	w, _ := ws.CreateWorkspace(t.Context(), "acme")
	key, _, _ := ws.CreateAPIKey(t.Context(), w.ID, "test")
	reg := schema.New(pool)
	srv := httptest.NewServer(NewRouter(Deps{
		Workspaces:  ws,
		Collections: collection.New(pool),
		Records:     record.New(pool, schema.NewValidator(reg)),
		Schemas:     reg,
	}))
	t.Cleanup(srv.Close)
	return srv, key
}

func sreq(t *testing.T, method, url, key, body string) *http.Response {
	t.Helper()
	req, _ := http.NewRequest(method, url, bytes.NewBufferString(body))
	req.Header.Set("Authorization", "Bearer "+key)
	req.Header.Set("X-Substrate-Actor", "agent-1")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("%s %s: %v", method, url, err)
	}
	return resp
}

func TestSchemaLifecycleOverHTTP(t *testing.T) {
	srv, key := newSchemaServer(t)
	base := srv.URL + "/v1/collections/people"

	// Create collection (flexible).
	sreq(t, "POST", srv.URL+"/v1/collections", key, `{"name":"people"}`).Body.Close()

	// Register + activate first schema (auto-activates anyway).
	resp := sreq(t, "POST", base+"/schemas", key,
		`{"json_schema":{"type":"object","properties":{"name":{"type":"string"}},"required":["name"]}}`)
	if resp.StatusCode != http.StatusCreated {
		t.Fatalf("register status = %d", resp.StatusCode)
	}
	resp.Body.Close()

	// Valid typed record create -> 201.
	resp = sreq(t, "POST", base+"/records", key, `{"data":{"name":"Ada"}}`)
	if resp.StatusCode != http.StatusCreated {
		t.Fatalf("valid create status = %d", resp.StatusCode)
	}
	resp.Body.Close()

	// Invalid typed record create -> 422.
	resp = sreq(t, "POST", base+"/records", key, `{"data":{"name":123}}`)
	if resp.StatusCode != http.StatusUnprocessableEntity {
		t.Fatalf("invalid create status = %d, want 422", resp.StatusCode)
	}
	resp.Body.Close()

	// Register v2 breaking (adds required age), then activate -> 409.
	resp = sreq(t, "POST", base+"/schemas", key,
		`{"json_schema":{"type":"object","properties":{"name":{"type":"string"},"age":{"type":"integer"}},"required":["name","age"]}}`)
	var reg struct{ Version int `json:"version"` }
	_ = json.NewDecoder(resp.Body).Decode(&reg)
	resp.Body.Close()

	// NOTE: using /activate subpath because Go 1.22+ ServeMux does not allow
	// wildcard segments with a literal suffix (e.g. {version}:activate panics).
	resp = sreq(t, "POST", base+"/schemas/2/activate", key, `{}`)
	if resp.StatusCode != http.StatusConflict {
		t.Fatalf("breaking activate status = %d, want 409", resp.StatusCode)
	}
	resp.Body.Close()

	// Force activate -> 200/204.
	resp = sreq(t, "POST", base+"/schemas/2/activate", key, `{"force":true,"rationale":"intended"}`)
	if resp.StatusCode != http.StatusOK && resp.StatusCode != http.StatusNoContent {
		t.Fatalf("forced activate status = %d", resp.StatusCode)
	}
	resp.Body.Close()
}
