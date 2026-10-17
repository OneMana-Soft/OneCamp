package business

import (
	"context"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/akashc777/OneCamp/helpers"
	userModels "github.com/akashc777/OneCamp/models/postgres/User"
)

// Every policy's default is what the server does today with no policy at all, so
// reading one, or putting a route behind one, changes nothing until A6.
func TestDefaultsAreTodaysBehaviour(t *testing.T) {
	ctx := context.Background()
	if !Policies.Bool(ctx, DocsExportMembers) || !Policies.Bool(ctx, DocsPublishMembers) {
		t.Error("members export and publish today; the defaults must say so")
	}
	if Policies.Int(ctx, GuestLinkMaxDays) != 0 {
		t.Error("guest links have no cap of their own today")
	}
	if Policies.List(ctx, EmbedAllowlist) != nil {
		t.Error("no embed list is configured today")
	}

	reached := false
	h := Policies.Require(DocsExportMembers)(http.HandlerFunc(func(http.ResponseWriter, *http.Request) { reached = true }))
	h.ServeHTTP(httptest.NewRecorder(), httptest.NewRequest(http.MethodPost, "/doc/export", nil))
	if !reached {
		t.Error("a route behind a policy that is on reaches its handler")
	}
}

// A policy that is off refuses members and lets admins through.
func TestAnOffPolicyRefusesMembersNotAdmins(t *testing.T) {
	off := BoolPolicy{Key: "test.off", Default: false}
	for _, admin := range []bool{false, true} {
		reached := false
		h := Policies.Require(off)(http.HandlerFunc(func(http.ResponseWriter, *http.Request) { reached = true }))
		var u userModels.UserInfo
		u.UserPostgresInfo.IsAdmin = admin
		req := httptest.NewRequest(http.MethodPost, "/x", nil)
		req = req.WithContext(context.WithValue(req.Context(), helpers.UserInfoContextKey, u))
		rec := httptest.NewRecorder()
		h.ServeHTTP(rec, req)
		if reached != admin {
			t.Errorf("admin %v: reached the handler %v", admin, reached)
		}
		if !admin && rec.Code != http.StatusForbidden {
			t.Errorf("a member is refused with 403, got %d", rec.Code)
		}
	}
}
