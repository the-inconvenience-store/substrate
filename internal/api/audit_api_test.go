//go:build integration

package api

import (
	"encoding/json"
	"net/http"
	"testing"
)

func TestAuditOverHTTP(t *testing.T) {
	srv, key, _, _, _ := newGovServer(t)

	resp := doAs(t, "POST", srv.URL+"/v1/collections", key, "agent-1", map[string]any{"name": "orders"})
	resp.Body.Close()
	resp = doAs(t, "POST", srv.URL+"/v1/collections/orders/records", key, "agent-1", map[string]any{"data": map[string]any{"x": 1}})
	if resp.StatusCode != http.StatusCreated {
		t.Fatalf("create = %d", resp.StatusCode)
	}
	resp.Body.Close()

	resp = doAs(t, "GET", srv.URL+"/v1/audit?collection=orders", key, "agent-1", nil)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("audit = %d", resp.StatusCode)
	}
	var page struct {
		Items []struct {
			Type  string          `json:"type"`
			Trace json.RawMessage `json:"trace"`
		} `json:"items"`
		NextCursor string `json:"next_cursor"`
	}
	_ = json.NewDecoder(resp.Body).Decode(&page)
	resp.Body.Close()
	found := false
	for _, it := range page.Items {
		if it.Type == "create" {
			found = true
		}
	}
	if !found {
		t.Fatalf("expected a create event in audit, got %+v", page.Items)
	}

	resp = doAs(t, "GET", srv.URL+"/v1/audit?type=policy_denied", key, "agent-1", nil)
	var empty struct {
		Items []json.RawMessage `json:"items"`
	}
	_ = json.NewDecoder(resp.Body).Decode(&empty)
	resp.Body.Close()
	if len(empty.Items) != 0 {
		t.Fatalf("expected no denials, got %d", len(empty.Items))
	}
}
