//go:build integration

package api

import (
	"encoding/json"
	"net/http"
	"testing"
)

func TestListRecordsHTTP(t *testing.T) {
	srv, key := newTestServer(t)

	// Create collection.
	resp := do(t, "POST", srv.URL+"/v1/collections", key, map[string]any{"name": "items", "level": "flexible"})
	if resp.StatusCode != http.StatusCreated {
		t.Fatalf("create collection status = %d", resp.StatusCode)
	}
	resp.Body.Close()

	// Create 3 records: open, open, closed.
	for _, status := range []string{"open", "open", "closed"} {
		resp = do(t, "POST", srv.URL+"/v1/collections/items/records", key, map[string]any{"data": map[string]any{"status": status}})
		if resp.StatusCode != http.StatusCreated {
			t.Fatalf("create record status = %d", resp.StatusCode)
		}
		resp.Body.Close()
	}

	// List with filter status:eq:open.
	resp = do(t, "GET", srv.URL+"/v1/collections/items/records?filter=status:eq:open&limit=10", key, nil)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("list status = %d, want 200", resp.StatusCode)
	}
	var out struct {
		Items      []map[string]any `json:"items"`
		NextCursor string           `json:"next_cursor"`
	}
	_ = json.NewDecoder(resp.Body).Decode(&out)
	resp.Body.Close()
	if len(out.Items) != 2 {
		t.Fatalf("items len = %d, want 2", len(out.Items))
	}
}

func TestListRecordsBadFilterHTTP(t *testing.T) {
	srv, key := newTestServer(t)

	resp := do(t, "POST", srv.URL+"/v1/collections", key, map[string]any{"name": "items", "level": "flexible"})
	if resp.StatusCode != http.StatusCreated {
		t.Fatalf("create collection status = %d", resp.StatusCode)
	}
	resp.Body.Close()

	resp = do(t, "GET", srv.URL+"/v1/collections/items/records?filter=bad-syntax", key, nil)
	if resp.StatusCode != http.StatusBadRequest {
		t.Fatalf("bad filter status = %d, want 400", resp.StatusCode)
	}
	resp.Body.Close()
}
