//go:build integration
// +build integration

package integration_test

// Every change to who is in the workspace, and who runs it, is in the admin
// audit log (audit A6), against real Postgres 12 and Dgraph. Making or
// removing an admin, switching someone's account off or back on, invitations,
// and what the directory does through SCIM wrote no row: only an INFO line.
//
// Run: go test -tags=integration ./tests/integration/ -run 'TestEveryMembershipChangeIsAudited|TestWhatTheDirectoryDoesIsAudited' -v

import (
	"bytes"
	"context"
	"database/sql"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/go-chi/chi/v5"
	"github.com/google/uuid"

	userBusiness "github.com/akashc777/OneCamp/business/User"
	scimController "github.com/akashc777/OneCamp/controllers/Scim"
	userController "github.com/akashc777/OneCamp/controllers/User"
	"github.com/akashc777/OneCamp/helpers"
	customMiddleware "github.com/akashc777/OneCamp/middleware"
	userModels "github.com/akashc777/OneCamp/models/postgres/User"
)

// audited waits until the admin audit log holds want rows of action, all by
// the security category, and answers its rows' actor kinds.
func audited(t *testing.T, db *sql.DB, action string, want int) []string {
	t.Helper()
	for deadline := time.Now().Add(10 * time.Second); ; time.Sleep(100 * time.Millisecond) {
		rows, err := db.Query(`SELECT category, COALESCE(actor_kind, '') FROM admin_audit_log WHERE action = $1`, action)
		if err != nil {
			t.Fatal(err)
		}
		var kinds []string
		for rows.Next() {
			var category, kind string
			if err := rows.Scan(&category, &kind); err != nil {
				t.Fatal(err)
			}
			if category != "security" {
				t.Fatalf("%s is filed under %q", action, category)
			}
			kinds = append(kinds, kind)
		}
		rows.Close()
		if len(kinds) == want {
			return kinds
		}
		if time.Now().After(deadline) {
			t.Fatalf("the audit log has %d %s rows, want %d", len(kinds), action, want)
		}
	}
}

// act runs handler as the owner with body, the route's params and a session
// request, as the router would, and answers its status.
func act(t *testing.T, lr *landReady, handler http.HandlerFunc, method string, body any, params ...string) int {
	t.Helper()
	raw, _ := json.Marshal(body)
	ctx := context.WithValue(lr.ctx, helpers.UserInfoContextKey, lr.owner)
	if len(params) > 0 {
		rc := chi.NewRouteContext()
		for i := 0; i+1 < len(params); i += 2 {
			rc.URLParams.Add(params[i], params[i+1])
		}
		ctx = context.WithValue(ctx, chi.RouteCtxKey, rc)
	}
	rec := httptest.NewRecorder()
	handler(rec, httptest.NewRequest(method, "/", bytes.NewReader(raw)).WithContext(ctx))
	if rec.Code >= 300 {
		t.Fatalf("%d %s", rec.Code, rec.Body.String())
	}
	return rec.Code
}

func TestEveryMembershipChangeIsAudited(t *testing.T) {
	t.Setenv("RESEND_API_KEY", "")
	t.Setenv("APP_SECRET_KEK", "admin-audit-membership-integration-kek")
	lr := setupLandReady(t)
	memberID, _, err := userBusiness.CreateUserWithMethod(lr.ctx, "dana@example.test", "Dana", nil, userModels.AuthMethodEmail, false)
	if err != nil {
		t.Fatal(err)
	}
	dana := map[string]string{"user_uuid": memberID.String()}

	t.Run("an admin made and taken away", func(t *testing.T) {
		act(t, lr, userController.CreateAdmin, http.MethodPost, dana)
		audited(t, lr.env.PG, "admin.granted", 1)
		act(t, lr, userController.RemoveAdmin, http.MethodPost, dana)
		audited(t, lr.env.PG, "admin.revoked", 1)
	})
	t.Run("an account switched off and back on", func(t *testing.T) {
		act(t, lr, userController.DeactivateUser, http.MethodPost, dana)
		audited(t, lr.env.PG, "member.deactivated", 1)
		act(t, lr, userController.ActivateUser, http.MethodPost, dana)
		audited(t, lr.env.PG, "member.reactivated", 1)
	})
	t.Run("an invitation made, sent again and withdrawn", func(t *testing.T) {
		kim := map[string]string{"email": "kim@example.test"}
		act(t, lr, userController.AddInvitation, http.MethodPost, kim)
		audited(t, lr.env.PG, "invitation.created", 1)
		act(t, lr, userController.ResendInvitation, http.MethodPost, kim)
		audited(t, lr.env.PG, "invitation.resent", 1)
		act(t, lr, userController.DeleteInvitation, http.MethodDelete, nil, "email", "kim@example.test")
		audited(t, lr.env.PG, "invitation.deleted", 1)
	})
}

func TestWhatTheDirectoryDoesIsAudited(t *testing.T) {
	lr := setupLandReady(t)

	t.Run("a directory credential made and revoked", func(t *testing.T) {
		raw, _ := json.Marshal(map[string]any{"name": "Okta", "expires_in_days": 30})
		rec := httptest.NewRecorder()
		ctx := context.WithValue(lr.ctx, helpers.UserInfoContextKey, lr.owner)
		scimController.CreateScimTokenHandler(rec, httptest.NewRequest(http.MethodPost, "/", bytes.NewReader(raw)).WithContext(ctx))
		var made struct {
			Data struct {
				Token struct {
					ID string `json:"id"`
				} `json:"token"`
			} `json:"data"`
		}
		if err := json.Unmarshal(rec.Body.Bytes(), &made); err != nil || rec.Code != http.StatusOK || made.Data.Token.ID == "" {
			t.Fatalf("making a credential: %d %s", rec.Code, rec.Body.String())
		}
		audited(t, lr.env.PG, "scim_token.create", 1)
		act(t, lr, scimController.RevokeScimTokenHandler, http.MethodPost, nil, "id", made.Data.Token.ID)
		audited(t, lr.env.PG, "scim_token.revoke", 1)
	})

	t.Run("someone the directory adds, switches off, on, and removes", func(t *testing.T) {
		raw, _ := json.Marshal(map[string]string{"name": "Okta"})
		rec := httptest.NewRecorder()
		scimController.CreateScimTokenHandler(rec, httptest.NewRequest(http.MethodPost, "/", bytes.NewReader(raw)).WithContext(context.WithValue(lr.ctx, helpers.UserInfoContextKey, lr.owner)))
		var made struct {
			Data struct {
				Secret string `json:"plaintext"`
			} `json:"data"`
		}
		if err := json.Unmarshal(rec.Body.Bytes(), &made); err != nil || made.Data.Secret == "" {
			t.Fatalf("making a credential: %d %s", rec.Code, rec.Body.String())
		}
		// Through the credential's own check, as a directory's calls come.
		scim := func(handler http.HandlerFunc, method, body string, id string) *httptest.ResponseRecorder {
			t.Helper()
			r := httptest.NewRequest(method, "/", strings.NewReader(body)).WithContext(lr.ctx)
			r.Header.Set("Authorization", "Bearer "+made.Data.Secret)
			r.Header.Set("Content-Type", "application/scim+json")
			if id != "" {
				rc := chi.NewRouteContext()
				rc.URLParams.Add("id", id)
				r = r.WithContext(context.WithValue(r.Context(), chi.RouteCtxKey, rc))
			}
			rec := httptest.NewRecorder()
			customMiddleware.VerifyScimToken(handler).ServeHTTP(rec, r)
			return rec
		}
		created := scim(scimController.CreateScimUser, http.MethodPost,
			`{"schemas":["urn:ietf:params:scim:schemas:core:2.0:User"],"userName":"lee@example.test","emails":[{"value":"lee@example.test","primary":true}],"active":true}`, "")
		var user struct {
			ID string `json:"id"`
		}
		if err := json.Unmarshal(created.Body.Bytes(), &user); err != nil || created.Code != http.StatusCreated || uuid.Validate(user.ID) != nil {
			t.Fatalf("the directory adding Lee: %d %s", created.Code, created.Body.String())
		}
		patch := func(active bool) {
			body, _ := json.Marshal(map[string]any{"schemas": []string{"urn:ietf:params:scim:api:messages:2.0:PatchOp"},
				"Operations": []map[string]any{{"op": "replace", "value": map[string]bool{"active": active}}}})
			if rec := scim(scimController.PatchScimUser, http.MethodPatch, string(body), user.ID); rec.Code != http.StatusOK {
				t.Fatalf("active=%v: %d %s", active, rec.Code, rec.Body.String())
			}
		}
		patch(false)
		patch(true)
		if rec := scim(scimController.DeleteScimUser, http.MethodDelete, "", user.ID); rec.Code != http.StatusNoContent {
			t.Fatalf("the directory removing Lee: %d %s", rec.Code, rec.Body.String())
		}
		// The workspace acting on its own, through the credential, named beside the admin who made it.
		for _, action := range []string{"scim.user.created", "scim.user.deactivated", "scim.user.reactivated", "scim.user.deleted"} {
			if kinds := audited(t, lr.env.PG, action, 1); kinds[0] != "system" {
				t.Errorf("%s was written as %q, want the system", action, kinds[0])
			}
		}
		var actor *uuid.UUID
		if err := lr.env.PG.QueryRow(`SELECT actor_id FROM admin_audit_log WHERE action = 'scim.user.created'`).Scan(&actor); err != nil || actor == nil || *actor != lr.owner.UserPostgresInfo.Id {
			t.Errorf("the directory's row names %v (%v), want the admin who made the credential", actor, err)
		}
	})
}
