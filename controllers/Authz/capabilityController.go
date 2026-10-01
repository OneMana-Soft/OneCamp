package controllers

// Admin HTTP handlers for the generic capability-permission policies. Mounted
// under the admin router (gated by VerifyAdminAuthOnlyPostgres) — only admins
// decide which capabilities are delegated to members. Every change is
// audit-logged.

import (
	"encoding/json"
	"net/http"

	auditBusiness "github.com/akashc777/OneCamp/business/AdminAudit"
	authz "github.com/akashc777/OneCamp/business/Authz"
	"github.com/akashc777/OneCamp/helpers"
	userModels "github.com/akashc777/OneCamp/models/postgres/User"
)

// ListCapabilityPolicies handles GET /admin/capabilities
func ListCapabilityPolicies(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	policies, err := authz.ListPolicies(ctx)
	if err != nil {
		helpers.LogErrorWithContext(ctx, "controllers/ListCapabilityPolicies err: %+v", err)
		helpers.WriteJSON(w, http.StatusInternalServerError, helpers.Envolope{"msg": "failed to load permissions"})
		return
	}
	helpers.WriteJSON(w, http.StatusOK, helpers.Envolope{"data": policies})
}

// SetCapabilityPolicy handles POST /admin/capabilities
func SetCapabilityPolicy(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	userInfo := ctx.Value(helpers.UserInfoContextKey).(userModels.UserInfo)

	var req struct {
		Capability string `json:"capability"`
		Policy     string `json:"policy"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		helpers.WriteJSON(w, http.StatusBadRequest, helpers.Envolope{"msg": "invalid request body"})
		return
	}

	if err := authz.SetPolicy(ctx, req.Capability, req.Policy, userInfo); err != nil {
		helpers.WriteJSON(w, http.StatusBadRequest, helpers.Envolope{"msg": err.Error()})
		return
	}

	auditBusiness.Record(r, "capability.policy.update", auditBusiness.CategorySettings,
		"Updated permission policy: "+req.Capability+" → "+req.Policy,
		map[string]interface{}{"capability": req.Capability, "policy": req.Policy})
	helpers.WriteJSON(w, http.StatusOK, helpers.Envolope{"msg": "updated"})
}

// GetMyCapabilities handles GET /me/capabilities — returns the set of
// delegatable capabilities the CURRENT user may exercise, so the FE can show
// or hide features (e.g. the Automations nav entry) without guessing. Member-
// accessible (any authenticated user); reusable by every capability-gated
// feature.
func GetMyCapabilities(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	userInfo, ok := ctx.Value(helpers.UserInfoContextKey).(userModels.UserInfo)
	if !ok {
		helpers.WriteJSON(w, http.StatusUnauthorized, helpers.Envolope{"msg": "unauthorized"})
		return
	}
	out := authz.MyCapabilities(ctx, &userInfo)
	helpers.WriteJSON(w, http.StatusOK, helpers.Envolope{"data": out})
}
