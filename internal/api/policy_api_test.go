//go:build integration

package api

import (
	"bytes"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/substrate/substrate/internal/audit"
	"github.com/substrate/substrate/internal/collection"
	"github.com/substrate/substrate/internal/policy"
	"github.com/substrate/substrate/internal/record"
	"github.com/substrate/substrate/internal/store"
	"github.com/substrate/substrate/internal/workspace"
)

func newGovServer(t *testing.T) (*httptest.Server, string, string, uuid.UUID, *pgxpool.Pool) {
	t.Helper()
	pool := store.NewTestPool(t)
	wsSvc := workspace.New(pool)
	w, err := wsSvc.CreateWorkspace(t.Context(), "acme")
	if err != nil {
		t.Fatalf("workspace: %v", err)
	}
	key, _, err := wsSvc.CreateAPIKey(t.Context(), w.ID, "test")
	if err != nil {
		t.Fatalf("key: %v", err)
	}
	const adminToken = "admin-secret"
	engine := policy.NewEngine(pool)
	srv := httptest.NewServer(NewRouter(Deps{
		Workspaces:  wsSvc,
		Collections: collection.New(pool),
		Records:     record.New(pool, nil).WithEvaluator(engine),
		Policies:    policy.NewService(pool),
		Audit:       audit.New(pool),
		AdminToken:  adminToken,
	}))
	t.Cleanup(srv.Close)
	return srv, key, adminToken, w.ID, pool
}

func doAs(t *testing.T, method, url, key, actor string, body any) *http.Response {
	t.Helper()
	var buf bytes.Buffer
	if body != nil {
		_ = json.NewEncoder(&buf).Encode(body)
	}
	req, _ := http.NewRequest(method, url, &buf)
	req.Header.Set("Authorization", "Bearer "+key)
	if actor != "" {
		req.Header.Set("X-Substrate-Actor", actor)
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("%s %s: %v", method, url, err)
	}
	return resp
}

func TestPolicyRuleCRUDAndEnforcement(t *testing.T) {
	srv, key, _, _, _ := newGovServer(t)

	resp := doAs(t, "POST", srv.URL+"/v1/collections", key, "agent-1", map[string]any{"name": "orders"})
	if resp.StatusCode != http.StatusCreated {
		t.Fatalf("collection = %d", resp.StatusCode)
	}
	resp.Body.Close()

	resp = doAs(t, "POST", srv.URL+"/v1/policies", key, "agent-1", map[string]any{
		"collection": "orders", "operation": "create", "effect": "deny",
	})
	if resp.StatusCode != http.StatusCreated {
		t.Fatalf("policy create = %d", resp.StatusCode)
	}
	var rule struct {
		ID         string `json:"id"`
		Collection string `json:"collection"`
		Effect     string `json:"effect"`
	}
	_ = json.NewDecoder(resp.Body).Decode(&rule)
	resp.Body.Close()
	if rule.Collection != "orders" || rule.Effect != "deny" {
		t.Fatalf("rule = %+v", rule)
	}

	resp = doAs(t, "GET", srv.URL+"/v1/policies", key, "agent-1", nil)
	var listed struct {
		DefaultMode string           `json:"default_mode"`
		Rules       []map[string]any `json:"rules"`
	}
	_ = json.NewDecoder(resp.Body).Decode(&listed)
	resp.Body.Close()
	if listed.DefaultMode != "allow" || len(listed.Rules) != 1 {
		t.Fatalf("listed = %+v", listed)
	}

	resp = doAs(t, "POST", srv.URL+"/v1/collections/orders/records", key, "agent-1", map[string]any{"data": map[string]any{"x": 1}})
	if resp.StatusCode != http.StatusForbidden {
		t.Fatalf("denied create = %d, want 403", resp.StatusCode)
	}
	var env struct {
		Error struct {
			Code string `json:"code"`
		} `json:"error"`
	}
	_ = json.NewDecoder(resp.Body).Decode(&env)
	resp.Body.Close()
	if env.Error.Code != "policy_denied" {
		t.Fatalf("code = %q", env.Error.Code)
	}

	resp = doAs(t, "DELETE", srv.URL+"/v1/policies/"+rule.ID, key, "agent-1", nil)
	if resp.StatusCode != http.StatusNoContent {
		t.Fatalf("delete = %d", resp.StatusCode)
	}
	resp.Body.Close()

	resp = doAs(t, "POST", srv.URL+"/v1/collections/orders/records", key, "agent-1", map[string]any{"data": map[string]any{"x": 1}})
	if resp.StatusCode != http.StatusCreated {
		t.Fatalf("allowed create = %d, want 201", resp.StatusCode)
	}
	resp.Body.Close()
}

func TestSetPolicyModeAdmin(t *testing.T) {
	srv, key, adminToken, ws, _ := newGovServer(t)

	req, _ := http.NewRequest("PUT", srv.URL+"/admin/workspaces/"+ws.String()+"/policy-mode", bytes.NewBufferString(`{"mode":"deny"}`))
	req.Header.Set("X-Admin-Token", adminToken)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("put: %v", err)
	}
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("mode flip = %d", resp.StatusCode)
	}
	resp.Body.Close()

	resp = doAs(t, "POST", srv.URL+"/v1/collections", key, "agent-1", map[string]any{"name": "orders"})
	resp.Body.Close()

	resp = doAs(t, "POST", srv.URL+"/v1/collections/orders/records", key, "agent-1", map[string]any{"data": map[string]any{}})
	if resp.StatusCode != http.StatusForbidden {
		t.Fatalf("default-deny create = %d, want 403", resp.StatusCode)
	}
	resp.Body.Close()

	resp = doAs(t, "GET", srv.URL+"/v1/policies", key, "agent-1", nil)
	var listed struct {
		DefaultMode string `json:"default_mode"`
	}
	_ = json.NewDecoder(resp.Body).Decode(&listed)
	resp.Body.Close()
	if listed.DefaultMode != "deny" {
		t.Fatalf("default_mode = %q", listed.DefaultMode)
	}
}
