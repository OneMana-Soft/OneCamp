package middleware

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/akashc777/OneCamp/helpers"
	capabilityModels "github.com/akashc777/OneCamp/models/postgres/Capability"
	models "github.com/akashc777/OneCamp/models/postgres/User"
)

// A refusal says why (QA SH-02, 14 Oct 2026): a member who couldn't invite got
// a 403 with no body, and the web told them to check their connection. The
// capability's refusal names the condition, the capability and the words; the
// admin-only gate's names its condition and its words.
func TestARefusalSaysWhy(t *testing.T) {
	serve := func(h http.Handler, user models.UserInfo) (int, map[string]any) {
		t.Helper()
		req := httptest.NewRequest(http.MethodPost, "/invitations", nil)
		req = req.WithContext(context.WithValue(req.Context(), helpers.UserInfoContextKey, user))
		rec := httptest.NewRecorder()
		h.ServeHTTP(rec, req)
		var body map[string]any
		if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
			t.Fatalf("the refusal has no JSON body: %q", rec.Body.String())
		}
		return rec.Code, body
	}
	never := http.HandlerFunc(func(http.ResponseWriter, *http.Request) { t.Fatal("a refused request reached the handler") })
	// A bot never holds a delegated capability, and is refused before any policy lookup.
	bot := models.UserInfo{UserPostgresInfo: models.User{IsBot: true}}

	code, body := serve(RequireCapability(capabilityModels.CapInvitationCreate)(never), bot)
	if code != http.StatusForbidden || body["code"] != CapabilityRequired || body["capability"] != capabilityModels.CapInvitationCreate {
		t.Errorf("capability refusal: %d %v", code, body)
	}
	if body["msg"] != "Only admins, or people an admin allows, can invite people. Ask an admin." {
		t.Errorf("capability refusal says %q", body["msg"])
	}

	code, body = serve(VerifyAdminAuthOnlyPostgres(never), models.UserInfo{})
	if code != http.StatusForbidden || body["code"] != AdminRequired || body["msg"] != "Only admins can do this. Ask an admin." {
		t.Errorf("admin refusal: %d %v", code, body)
	}
}
