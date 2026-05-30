package api

import (
	"encoding/json"
	"net/http"

	"github.com/google/uuid"

	"github.com/substrate/substrate/internal/apierr"
	"github.com/substrate/substrate/internal/auth"
	"github.com/substrate/substrate/internal/httpx"
	"github.com/substrate/substrate/internal/policy"
)

type policyHandlers struct {
	h        *handlers
	policies *policy.Service
}

func (ph *policyHandlers) create(w http.ResponseWriter, r *http.Request) {
	ws := auth.WorkspaceFrom(r.Context())
	var body struct {
		Actor      string `json:"actor"`
		Collection string `json:"collection"`
		Operation  string `json:"operation"`
		Effect     string `json:"effect"`
	}
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
		writeErr(w, apierr.New(apierr.BadRequest, "invalid json"))
		return
	}
	cmd := policy.CreateRuleCmd{Workspace: ws, Actor: body.Actor, Operation: body.Operation, Effect: body.Effect}
	if body.Collection != "" && body.Collection != "*" {
		c, err := ph.h.resolveCollection(r, body.Collection)
		if err != nil {
			writeErr(w, err)
			return
		}
		cmd.CollectionID = &c.ID
		cmd.CollectionName = c.Name
	}
	rule, err := ph.policies.CreateRule(r.Context(), cmd)
	if err != nil {
		writeErr(w, err)
		return
	}
	httpx.JSON(w, http.StatusCreated, rule)
}

func (ph *policyHandlers) list(w http.ResponseWriter, r *http.Request) {
	ws := auth.WorkspaceFrom(r.Context())
	rules, err := ph.policies.ListRules(r.Context(), ws)
	if err != nil {
		writeErr(w, err)
		return
	}
	mode, err := ph.policies.DefaultMode(r.Context(), ws)
	if err != nil {
		writeErr(w, err)
		return
	}
	if rules == nil {
		rules = []policy.PolicyRule{}
	}
	httpx.JSON(w, http.StatusOK, map[string]any{"default_mode": mode, "rules": rules})
}

func (ph *policyHandlers) delete(w http.ResponseWriter, r *http.Request) {
	ws := auth.WorkspaceFrom(r.Context())
	id, err := uuid.Parse(r.PathValue("id"))
	if err != nil {
		writeErr(w, apierr.New(apierr.BadRequest, "invalid id"))
		return
	}
	if err := ph.policies.DeleteRule(r.Context(), ws, id); err != nil {
		writeErr(w, err)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

// setPolicyMode is admin-token gated (wired on the admin handler).
func (a *adminHandlers) setPolicyMode(w http.ResponseWriter, r *http.Request) {
	if !a.authed(r) {
		writeErr(w, apierr.New(apierr.Unauthorized, "invalid admin token"))
		return
	}
	wsID, err := uuid.Parse(r.PathValue("ws"))
	if err != nil {
		writeErr(w, apierr.New(apierr.BadRequest, "invalid workspace id"))
		return
	}
	var body struct {
		Mode string `json:"mode"`
	}
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
		writeErr(w, apierr.New(apierr.BadRequest, "invalid json"))
		return
	}
	if err := a.workspaces.SetPolicyMode(r.Context(), wsID, body.Mode); err != nil {
		writeErr(w, err)
		return
	}
	httpx.JSON(w, http.StatusOK, map[string]any{"workspace_id": wsID, "policy_mode": body.Mode})
}
