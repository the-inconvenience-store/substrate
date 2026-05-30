//go:build integration

package api

import (
	"encoding/json"
	"net/http"
	"testing"
)

func TestProjectionSurfaceEndToEnd(t *testing.T) {
	srv, key, _, _ := newProjServer(t)

	resp := doAs(t, "POST", srv.URL+"/v1/collections", key, "agent-1", map[string]any{"name": "orders", "auto_backfill": true})
	if resp.StatusCode != http.StatusCreated {
		t.Fatalf("collection = %d", resp.StatusCode)
	}
	var coll struct {
		AutoBackfill bool `json:"auto_backfill"`
	}
	json.NewDecoder(resp.Body).Decode(&coll)
	resp.Body.Close()
	if !coll.AutoBackfill {
		t.Fatalf("auto_backfill not set at create: %+v", coll)
	}

	resp = doAs(t, "POST", srv.URL+"/v1/collections/orders/schemas", key, "agent-1", map[string]any{
		"json_schema": map[string]any{"type": "object", "properties": map[string]any{"a": map[string]any{"type": "string"}}},
		"activate":    true,
	})
	if resp.StatusCode != http.StatusCreated {
		t.Fatalf("schema = %d", resp.StatusCode)
	}
	resp.Body.Close()

	resp = doAs(t, "POST", srv.URL+"/v1/collections/orders/records", key, "agent-1", map[string]any{"data": map[string]any{"a": "x"}})
	resp.Body.Close()

	resp = doAs(t, "POST", srv.URL+"/v1/collections/orders/schemas", key, "agent-1", map[string]any{
		"json_schema": map[string]any{"type": "object", "properties": map[string]any{
			"a": map[string]any{"type": "string"}, "b": map[string]any{"type": "string", "default": "filled"},
		}},
		"activate": true,
	})
	if resp.StatusCode != http.StatusCreated {
		t.Fatalf("v2 = %d", resp.StatusCode)
	}
	resp.Body.Close()

	resp = doAs(t, "POST", srv.URL+"/v1/collections/orders/backfill", key, "agent-1", nil)
	var rep struct{ Migrated int }
	json.NewDecoder(resp.Body).Decode(&rep)
	resp.Body.Close()
	if rep.Migrated != 1 {
		t.Fatalf("migrated = %d, want 1", rep.Migrated)
	}
}
