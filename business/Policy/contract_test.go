package business

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
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

// The projects programme's policies: members change tasks (A1), team admins
// make projects, and the rest answer today's behaviour until a package reads
// them. Only the two A1 reads are in the admin's list.
func TestTheProjectsPoliciesDefaults(t *testing.T) {
	ctx := context.Background()
	project := Scope{Kind: ScopeProject, ID: "8a7f1d2e-4c3b-4a59-9e10-2f6d8b7c5a41"}
	if Policies.Choice(ctx, ProjectsEditWho) != "members" || Policies.ChoiceIn(ctx, ProjectsEditWho, project) != "members" {
		t.Error("members change a project's tasks unless it says otherwise")
	}
	if Policies.Choice(ctx, ProjectsCreateWho) != "team_admins" || Policies.Choice(ctx, ProjectsGuests) != "follow" {
		t.Error("team admins make projects, and guests follow the workspace, as before")
	}
	if Policies.BoolIn(ctx, ProjectsTriage, project) || Policies.BoolIn(ctx, ProjectsWIPHard, project) || Policies.Bool(ctx, ProjectsAITriage) {
		t.Error("no triage, no hard limits and no AI triage yet")
	}
	if Policies.IntIn(ctx, ProjectsAutoCloseDays, project) != 0 {
		t.Error("nothing closes by itself yet")
	}
	for _, p := range []ChoicePolicy{ProjectsEditWho, ProjectsCreateWho, ProjectsGuests} {
		if !p.Allows(p.Default) {
			t.Errorf("%s's default %q isn't one of its choices", p.Key, p.Default)
		}
	}
	listed := map[string]Info{}
	for _, in := range Admin.ListAll(ctx) {
		if strings.HasPrefix(in.Key, "projects.") {
			listed[in.Key] = in
		}
	}
	if len(listed) != 2 || listed[ProjectsEditWho.Key].Value != "members" || listed[ProjectsCreateWho.Key].Value != "team_admins" {
		t.Errorf("the admin's list: %+v", listed)
	}
	if _, err := Admin.InfoIn(ctx, ProjectsEditWho.Key, project); err != nil {
		t.Errorf("a project's own Who can change tasks: %v", err)
	}
	if _, err := Admin.InfoIn(ctx, ProjectsCreateWho.Key, project); err == nil {
		t.Error("who makes projects is the workspace's, not a project's")
	}
}
