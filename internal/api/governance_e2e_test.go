//go:build integration

package api

import (
	"encoding/json"
	"net/http"
	"testing"
)

func TestGovernanceEndToEnd(t *testing.T) {
	srv, key, _, _, _ := newGovServer(t)

	resp := doAs(t, "POST", srv.URL+"/v1/collections", key, "agent-1", map[string]any{"name": "orders"})
	resp.Body.Close()

	// Deny mallory's create on orders.
	resp = doAs(t, "POST", srv.URL+"/v1/policies", key, "agent-1", map[string]any{
		"actor": "mallory", "collection": "orders", "operation": "create", "effect": "deny",
	})
	if resp.StatusCode != http.StatusCreated {
		t.Fatalf("rule = %d", resp.StatusCode)
	}
	resp.Body.Close()

	// mallory denied.
	resp = doAs(t, "POST", srv.URL+"/v1/collections/orders/records", key, "mallory", map[string]any{"data": map[string]any{}})
	if resp.StatusCode != http.StatusForbidden {
		t.Fatalf("mallory = %d, want 403", resp.StatusCode)
	}
	resp.Body.Close()

	// alice allowed (rule is actor-specific to mallory; alice falls through to default_allow).
	resp = doAs(t, "POST", srv.URL+"/v1/collections/orders/records", key, "alice", map[string]any{"data": map[string]any{}})
	if resp.StatusCode != http.StatusCreated {
		t.Fatalf("alice = %d, want 201", resp.StatusCode)
	}
	resp.Body.Close()

	// Exactly one policy_denied, for mallory.
	resp = doAs(t, "GET", srv.URL+"/v1/audit?type=policy_denied", key, "agent-1", nil)
	var denied struct {
		Items []struct {
			Actor string `json:"actor"`
		} `json:"items"`
	}
	_ = json.NewDecoder(resp.Body).Decode(&denied)
	resp.Body.Close()
	if len(denied.Items) != 1 || denied.Items[0].Actor != "mallory" {
		t.Fatalf("denials = %+v", denied.Items)
	}

	// alice's create event carries an allow trace.
	resp = doAs(t, "GET", srv.URL+"/v1/audit?actor=alice", key, "agent-1", nil)
	var alicePage struct {
		Items []struct {
			Type  string         `json:"type"`
			Trace map[string]any `json:"trace"`
		} `json:"items"`
	}
	_ = json.NewDecoder(resp.Body).Decode(&alicePage)
	resp.Body.Close()
	var createTrace map[string]any
	for _, it := range alicePage.Items {
		if it.Type == "create" {
			createTrace = it.Trace
		}
	}
	if createTrace == nil || createTrace["effect"] != "allow" {
		t.Fatalf("alice create trace = %+v", createTrace)
	}
}
