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
	"github.com/substrate/substrate/internal/store"
	"github.com/substrate/substrate/internal/workspace"
)

func newTestServer(t *testing.T) (*httptest.Server, string) {
	t.Helper()
	pool := store.NewTestPool(t)
	ws := workspace.New(pool)
	w, err := ws.CreateWorkspace(t.Context(), "acme")
	if err != nil {
		t.Fatalf("workspace: %v", err)
	}
	key, _, err := ws.CreateAPIKey(t.Context(), w.ID, "test")
	if err != nil {
		t.Fatalf("key: %v", err)
	}
	srv := httptest.NewServer(NewRouter(Deps{
		Workspaces:  ws,
		Collections: collection.New(pool),
		Records:     record.New(pool, nil),
	}))
	t.Cleanup(srv.Close)
	return srv, key
}

func do(t *testing.T, method, url, key string, body any) *http.Response {
	t.Helper()
	var buf bytes.Buffer
	if body != nil {
		_ = json.NewEncoder(&buf).Encode(body)
	}
	req, _ := http.NewRequest(method, url, &buf)
	req.Header.Set("Authorization", "Bearer "+key)
	req.Header.Set("X-Substrate-Actor", "agent-1")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("%s %s: %v", method, url, err)
	}
	return resp
}

func TestRecordLifecycleOverHTTP(t *testing.T) {
	srv, key := newTestServer(t)

	// Create collection.
	resp := do(t, "POST", srv.URL+"/v1/collections", key, map[string]any{"name": "trips", "level": "flexible"})
	if resp.StatusCode != http.StatusCreated {
		t.Fatalf("create collection status = %d", resp.StatusCode)
	}
	resp.Body.Close()

	// Create record.
	resp = do(t, "POST", srv.URL+"/v1/collections/trips/records", key, map[string]any{"data": map[string]any{"dest": "Tokyo"}})
	if resp.StatusCode != http.StatusCreated {
		t.Fatalf("create record status = %d", resp.StatusCode)
	}
	var created struct {
		ID       string `json:"id"`
		Revision int64  `json:"revision"`
	}
	_ = json.NewDecoder(resp.Body).Decode(&created)
	resp.Body.Close()
	if created.Revision != 1 {
		t.Fatalf("revision = %d, want 1", created.Revision)
	}

	// Read it back.
	resp = do(t, "GET", srv.URL+"/v1/collections/trips/records/"+created.ID, key, nil)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("get status = %d", resp.StatusCode)
	}
	if etag := resp.Header.Get("ETag"); etag != `"1"` {
		t.Fatalf("etag = %q, want \"1\"", etag)
	}
	resp.Body.Close()

	// Update with If-Match.
	req, _ := http.NewRequest("PATCH", srv.URL+"/v1/collections/trips/records/"+created.ID, bytes.NewBufferString(`{"data":{"dest":"Kyoto"}}`))
	req.Header.Set("Authorization", "Bearer "+key)
	req.Header.Set("If-Match", `"1"`)
	resp, _ = http.DefaultClient.Do(req)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("update status = %d", resp.StatusCode)
	}
	resp.Body.Close()

	// History has 2 entries.
	resp = do(t, "GET", srv.URL+"/v1/collections/trips/records/"+created.ID+"/history", key, nil)
	var hist []map[string]any
	_ = json.NewDecoder(resp.Body).Decode(&hist)
	resp.Body.Close()
	if len(hist) != 2 {
		t.Fatalf("history len = %d, want 2", len(hist))
	}

	// Unauthorized without key.
	bad, _ := http.NewRequest("GET", srv.URL+"/v1/collections/trips/records/"+created.ID, nil)
	resp, _ = http.DefaultClient.Do(bad)
	if resp.StatusCode != http.StatusUnauthorized {
		t.Fatalf("no-key status = %d, want 401", resp.StatusCode)
	}
	resp.Body.Close()
}
