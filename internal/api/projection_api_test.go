//go:build integration

package api

import (
	"bytes"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/google/uuid"

	"github.com/substrate/substrate/internal/collection"
	"github.com/substrate/substrate/internal/policy"
	"github.com/substrate/substrate/internal/projection"
	"github.com/substrate/substrate/internal/query"
	"github.com/substrate/substrate/internal/record"
	"github.com/substrate/substrate/internal/schema"
	"github.com/substrate/substrate/internal/store"
	"github.com/substrate/substrate/internal/workspace"
)

func newProjServer(t *testing.T) (*httptest.Server, string, string, uuid.UUID) {
	t.Helper()
	pool := store.NewTestPool(t)
	wsSvc := workspace.New(pool)
	w, err := wsSvc.CreateWorkspace(t.Context(), "acme")
	if err != nil {
		t.Fatalf("ws: %v", err)
	}
	key, _, err := wsSvc.CreateAPIKey(t.Context(), w.ID, "test")
	if err != nil {
		t.Fatalf("key: %v", err)
	}
	const adminToken = "admin-secret"
	reg := schema.NewWithIndexer(pool, query.NewIndexer(pool))
	engine := policy.NewEngine(pool)
	srv := httptest.NewServer(NewRouter(Deps{
		Workspaces:  wsSvc,
		Collections: collection.New(pool),
		Records:     record.New(pool, schema.NewValidator(reg)),
		Schemas:     reg,
		Policies:    policy.NewService(pool),
		Backfiller:  projection.NewBackfiller(pool, reg),
		Replayer:    projection.NewReplayer(pool),
		Evaluator:   engine,
		AdminToken:  adminToken,
	}))
	t.Cleanup(srv.Close)
	return srv, key, adminToken, w.ID
}

func mustJSONReq(t *testing.T, method, url string, body any) *http.Request {
	t.Helper()
	b, _ := json.Marshal(body)
	req, err := http.NewRequest(method, url, bytes.NewReader(b))
	if err != nil {
		t.Fatalf("req: %v", err)
	}
	req.Header.Set("Content-Type", "application/json")
	return req
}

func TestBackfillAndReplayOverHTTP(t *testing.T) {
	srv, key, adminToken, ws := newProjServer(t)

	resp := doAs(t, "POST", srv.URL+"/v1/collections", key, "agent-1", map[string]any{"name": "orders"})
	var coll struct {
		ID string `json:"id"`
	}
	json.NewDecoder(resp.Body).Decode(&coll)
	resp.Body.Close()

	resp = doAs(t, "POST", srv.URL+"/v1/collections/orders/schemas", key, "agent-1", map[string]any{
		"json_schema": map[string]any{"type": "object", "properties": map[string]any{"a": map[string]any{"type": "string"}}},
		"activate":    true,
	})
	if resp.StatusCode != http.StatusCreated {
		t.Fatalf("v1 = %d", resp.StatusCode)
	}
	resp.Body.Close()

	resp = doAs(t, "POST", srv.URL+"/v1/collections/orders/records", key, "agent-1", map[string]any{"data": map[string]any{"a": "x"}})
	if resp.StatusCode != http.StatusCreated {
		t.Fatalf("record = %d", resp.StatusCode)
	}
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
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("backfill = %d", resp.StatusCode)
	}
	var rep struct{ Migrated, Skipped, Remaining int }
	json.NewDecoder(resp.Body).Decode(&rep)
	resp.Body.Close()
	if rep.Migrated != 1 || rep.Remaining != 0 {
		t.Fatalf("report = %+v", rep)
	}

	resp = doAs(t, "GET", srv.URL+"/v1/collections/orders/records", key, "agent-1", nil)
	var page struct {
		Items []struct {
			Data map[string]any `json:"data"`
		} `json:"items"`
	}
	json.NewDecoder(resp.Body).Decode(&page)
	resp.Body.Close()
	if len(page.Items) != 1 || page.Items[0].Data["b"] != "filled" {
		t.Fatalf("list after backfill = %+v", page.Items)
	}

	req := mustJSONReq(t, "POST", srv.URL+"/admin/replay", map[string]any{"workspace_id": ws.String(), "collection_id": coll.ID})
	req.Header.Set("X-Admin-Token", adminToken)
	rresp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("replay: %v", err)
	}
	var rr struct{ Rebuilt int }
	json.NewDecoder(rresp.Body).Decode(&rr)
	rresp.Body.Close()
	if rresp.StatusCode != http.StatusOK || rr.Rebuilt != 1 {
		t.Fatalf("replay status=%d rebuilt=%d", rresp.StatusCode, rr.Rebuilt)
	}

	resp = doAs(t, "POST", srv.URL+"/v1/collections/orders/auto-backfill", key, "agent-1", map[string]any{"enabled": true})
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("toggle = %d", resp.StatusCode)
	}
	resp.Body.Close()
}

func TestBackfillDeniedByPolicy(t *testing.T) {
	srv, key, _, _ := newProjServer(t)

	resp := doAs(t, "POST", srv.URL+"/v1/collections", key, "agent-1", map[string]any{"name": "orders"})
	if resp.StatusCode != http.StatusCreated {
		t.Fatalf("collection = %d", resp.StatusCode)
	}
	resp.Body.Close()

	// Deny the backfill-management operation on this collection.
	resp = doAs(t, "POST", srv.URL+"/v1/policies", key, "agent-1", map[string]any{
		"collection": "orders", "operation": "backfill", "effect": "deny",
	})
	if resp.StatusCode != http.StatusCreated {
		t.Fatalf("policy create = %d", resp.StatusCode)
	}
	resp.Body.Close()

	// Manual backfill is now forbidden.
	resp = doAs(t, "POST", srv.URL+"/v1/collections/orders/backfill", key, "agent-1", nil)
	if resp.StatusCode != http.StatusForbidden {
		t.Fatalf("backfill = %d, want 403", resp.StatusCode)
	}
	var env struct {
		Error struct {
			Code string `json:"code"`
		} `json:"error"`
	}
	json.NewDecoder(resp.Body).Decode(&env)
	resp.Body.Close()
	if env.Error.Code != "policy_denied" {
		t.Fatalf("code = %q, want policy_denied", env.Error.Code)
	}

	// The auto-backfill toggle is gated by the same operation.
	resp = doAs(t, "POST", srv.URL+"/v1/collections/orders/auto-backfill", key, "agent-1", map[string]any{"enabled": true})
	if resp.StatusCode != http.StatusForbidden {
		t.Fatalf("toggle = %d, want 403", resp.StatusCode)
	}
	resp.Body.Close()

	// A denial event was recorded on the audit timeline (type policy_denied).
	// (Audit endpoint is not wired in newProjServer; presence is covered by policy tests.)
}
