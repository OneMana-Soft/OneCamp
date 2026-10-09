package jira

import (
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	importProvider "github.com/akashc777/OneCamp/business/Import/provider"
)

func TestAPickedJiraProjectIsTheOnlyOneImported(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/rest/api/3/project/search" {
			t.Errorf("asked %s", r.URL.Path)
		}
		fmt.Fprint(w, `{"isLast":true,"values":[{"id":"1","key":"ENG","name":"Engineering"},{"id":"2","key":"OPS","name":"Operations"}]}`)
	}))
	defer srv.Close()
	p := New()
	keys := func(opts importProvider.JobOptions) (string, error) {
		projects, err := p.projectsInScope(context.Background(), "tok", srv.URL, opts)
		var out []string
		for _, pr := range projects {
			out = append(out, pr.Key)
		}
		return strings.Join(out, ","), err
	}

	if got, err := keys(importProvider.JobOptions{"project_key": "OPS"}); err != nil || got != "OPS" {
		t.Errorf("picked OPS: %q %v", got, err)
	}
	// A job from before the pick had its own key.
	if got, err := keys(importProvider.JobOptions{"discover_id": "eng"}); err != nil || got != "ENG" {
		t.Errorf("picked eng the old way: %q %v", got, err)
	}
	if got, err := keys(importProvider.JobOptions{}); err != nil || got != "ENG,OPS" {
		t.Errorf("every project: %q %v", got, err)
	}
	// A project the account can't see any more is an error, not everything.
	if got, err := keys(importProvider.JobOptions{"project_key": "GONE"}); err == nil || got != "" || !strings.Contains(err.Error(), "GONE isn't visible") {
		t.Errorf("a project that's gone: %q %v", got, err)
	}
}

// A picked project brings its own people (who can be assigned its issues,
// and who its issues name), not every account on the site, and only its own
// category as a team.
func TestAPickedJiraProjectBringsOnlyItsPeopleAndCategory(t *testing.T) {
	siteListed, issuesWalked := false, false
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/rest/api/3/users/search":
			siteListed = true
			fmt.Fprint(w, `[{"accountId":"s1","accountType":"atlassian","displayName":"Someone else"}]`)
		case "/rest/api/3/projectCategory":
			t.Error("the site's categories were asked for a picked project")
			fmt.Fprint(w, `[]`)
		case "/rest/api/3/project/search":
			fmt.Fprint(w, `{"isLast":true,"values":[{"id":"1","key":"ENG","name":"Engineering","projectCategory":{"id":"c1","name":"Product"}},{"id":"2","key":"OPS","name":"Operations","projectCategory":{"id":"c2","name":"Platform"}}]}`)
		case "/rest/api/3/user/assignable/search":
			if r.URL.Query().Get("project") != "OPS" {
				t.Errorf("assignable people of %q", r.URL.Query().Get("project"))
			}
			fmt.Fprint(w, `[{"accountId":"a1","accountType":"atlassian","displayName":"Ada"}]`)
		case "/rest/api/3/search/jql":
			issuesWalked = true
			if !strings.Contains(r.URL.Query().Get("jql"), `project = "OPS"`) {
				t.Errorf("issues of %q", r.URL.Query().Get("jql"))
			}
			// Only an issue's own text needs it rendered; people don't.
			if r.URL.Query().Get("expand") != "" {
				t.Errorf("people read with expand=%s", r.URL.Query().Get("expand"))
			}
			fmt.Fprint(w, `{"isLast":true,"issues":[{"id":"10","key":"OPS-1","fields":{
				"assignee":{"accountId":"b2","accountType":"atlassian","displayName":"Bo"},
				"reporter":{"accountId":"a1","accountType":"atlassian","displayName":"Ada"},
				"creator":{"accountId":"bot","accountType":"app","displayName":"Automation"},
				"comment":{"comments":[{"author":{"accountId":"c3","accountType":"atlassian","displayName":"Cy"}}]}}}]}`)
		default:
			t.Errorf("asked %s", r.URL.Path)
		}
	}))
	defer srv.Close()
	p := New()
	ctx := context.Background()
	picked := importProvider.JobOptions{"project_key": "OPS"}

	// Counted for the plan, while the admin waits: who can be assigned its
	// issues, without walking them all.
	planned, err := p.usersInScope(ctx, "tok", srv.URL, picked, false)
	if err != nil || len(planned) != 1 || planned[0].AccountID != "a1" || issuesWalked {
		t.Errorf("a picked project's people counted for the plan: %+v %v (issues walked: %v)", planned, err, issuesWalked)
	}

	users, err := p.usersInScope(ctx, "tok", srv.URL, picked, true)
	var ids []string
	for _, u := range users {
		ids = append(ids, u.AccountID)
	}
	if err != nil || strings.Join(ids, ",") != "a1,b2,c3" || siteListed {
		t.Errorf("a picked project's people: %v %v (site listed: %v)", ids, err, siteListed)
	}
	cats, err := p.categoriesInScope(ctx, "tok", srv.URL, picked)
	if err != nil || len(cats) != 1 || cats[0].ID != "c2" {
		t.Errorf("a picked project's category: %+v %v", cats, err)
	}

	// Every project: the site's people, as before.
	if users, err := p.usersInScope(ctx, "tok", srv.URL, importProvider.JobOptions{}, true); err != nil || len(users) != 1 || users[0].AccountID != "s1" || !siteListed {
		t.Errorf("every project's people: %+v %v", users, err)
	}
}
