//go:build integration
// +build integration

package integration_test

// Only someone who may share a doc, board, table, channel or project manages
// its guest links. Maya, a member, can't make one, sees none of the owner's,
// and can't turn one off, publish it or take it off the web. What she can't
// see is not found to her: a doc she can't read, a private table, a project
// she isn't in. What she sees but may not share refuses her: a doc she reads
// but doesn't own, a board she reads but can't change, a table she sees but
// doesn't manage, and a channel or a project she's in but doesn't run. The
// owner's links stand through all of it.
//
// Run: go test -tags=integration ./tests/integration/ -run TestOnlyWhoMayShareSomethingManagesItsGuestLinks -v

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/go-chi/chi/v5"
	"github.com/google/uuid"

	tableBusiness "github.com/akashc777/OneCamp/business/DataTable"
	docBusiness "github.com/akashc777/OneCamp/business/Doc"
	guestBusiness "github.com/akashc777/OneCamp/business/Guest"
	settingsBusiness "github.com/akashc777/OneCamp/business/Settings"
	guestController "github.com/akashc777/OneCamp/controllers/Guest"
	"github.com/akashc777/OneCamp/helpers"
	dgraphStruct "github.com/akashc777/OneCamp/models/dgraph"
	tableModel "github.com/akashc777/OneCamp/models/postgres/DataTable"
	guestModel "github.com/akashc777/OneCamp/models/postgres/Guest"
	userModels "github.com/akashc777/OneCamp/models/postgres/User"
)

func TestOnlyWhoMayShareSomethingManagesItsGuestLinks(t *testing.T) {
	env, dg := liveSessionStores(t)
	ctx := context.Background()
	people := livePeople(t, env, "owner", "maya")
	board, channel, project, hiddenProject, team := uuid.NewString(), uuid.NewString(), uuid.NewString(), uuid.NewString(), uuid.NewString()
	in := func(names ...string) []map[string]any {
		var out []map[string]any
		for _, n := range names {
			out = append(out, map[string]any{"uid": "_:" + n})
		}
		return out
	}
	live := "0001-01-01T00:00:00Z"
	uids := dg.Mutate(t, []map[string]any{
		{"uid": "_:owner", "dgraph.type": "User", "user_uuid": people["owner"].String(), "user_name": "owner"},
		{"uid": "_:maya", "dgraph.type": "User", "user_uuid": people["maya"].String(), "user_name": "maya"},
		{"uid": "_:b", "dgraph.type": "Board", "board_uuid": board, "board_title": "Launch retro", "board_private": false,
			"board_deleted_at": live, "board_created_by": in("owner")[0]},
		{"uid": "_:c", "dgraph.type": "Channel", "ch_uuid": channel, "ch_name": "launch", "ch_private": false,
			"ch_members": in("owner", "maya"), "ch_moderators": in("owner")},
		{"uid": "_:p", "dgraph.type": "Project", "project_uuid": project, "project_name": "Website", "project_deleted_at": live,
			"project_members": in("owner", "maya"), "project_admins": in("owner"), "project_created_by": in("owner")[0]},
		{"uid": "_:hp", "dgraph.type": "Project", "project_uuid": hiddenProject, "project_name": "Payroll", "project_deleted_at": live,
			"project_members": in("owner"), "project_admins": in("owner"), "project_created_by": in("owner")[0]},
	})
	for _, q := range []struct {
		sql  string
		args []any
	}{
		{`INSERT INTO teams (id, team_name, created_by) VALUES ($1, 'Web', $2)`, []any{team, people["owner"]}},
		{`INSERT INTO projects (id, project_name, team_id, created_by) VALUES ($1, 'Website', $2, $3), ($4, 'Payroll', $2, $3)`,
			[]any{project, team, people["owner"], hiddenProject}},
	} {
		if _, err := env.PG.Exec(q.sql, q.args...); err != nil {
			t.Fatalf("%s: %v", q.sql, err)
		}
	}
	if err := settingsBusiness.SetGuestAccessEnabled(true); err != nil {
		t.Fatal(err)
	}
	userOf := func(name string) userModels.UserInfo {
		var u userModels.UserInfo
		u.UserPostgresInfo.Id = people[name]
		u.UserDgraphInfo = dgraphStruct.DgraphUser{Uid: uids[name], Uuid: people[name].String(), UserName: name}
		return u
	}
	owner, maya := userOf("owner"), userOf("maya")
	doc := func(title string, private bool) string {
		t.Helper()
		d, err := docBusiness.CreateDocInTree(ctx, &owner.UserDgraphInfo, docBusiness.NewDoc{Title: title, Private: &private}, nil)
		if err != nil {
			t.Fatal(err)
		}
		return d.Uuid
	}
	hidden, open := doc("Pay review", true), doc("Launch notes", false)
	table := func(name, visibility string) string {
		t.Helper()
		tb, err := tableBusiness.CreateTable(ctx, tableBusiness.TableInput{Name: name, Visibility: visibility}, tableBusiness.Actor{UserID: people["owner"]})
		if err != nil {
			t.Fatal(err)
		}
		return tb.Id.String()
	}
	privateTable, sharedTable := table("Salaries", tableModel.VisibilityPrivate), table("Vendors", tableModel.VisibilityWorkspace)
	link := func(kind, id string) uuid.UUID {
		t.Helper()
		g, err := guestBusiness.CreateResourceGrant(ctx, people["owner"], kind, id, "view", time.Hour, false)
		if err != nil {
			t.Fatal(err)
		}
		return g.GrantID
	}

	m := chi.NewRouter()
	m.Get("/guest/links", guestController.ListResourceGuestLinks)
	m.Post("/guest/links", guestController.CreateResourceGuestLink)
	m.Post("/guest/links/{id}/revoke", guestController.RevokeResourceGuestLink)
	m.Put("/guest/links/{id}/publish", guestController.PublishGuestLink)
	m.Delete("/guest/links/{id}/publish", guestController.UnpublishGuestLink)
	as := func(who userModels.UserInfo, method, target, body string) *httptest.ResponseRecorder {
		r := httptest.NewRequest(method, target, strings.NewReader(body))
		w := httptest.NewRecorder()
		m.ServeHTTP(w, r.WithContext(context.WithValue(r.Context(), helpers.UserInfoContextKey, who)))
		return w
	}
	listed := func(w *httptest.ResponseRecorder) int {
		t.Helper()
		var body struct {
			Data []json.RawMessage `json:"data"`
		}
		if err := json.Unmarshal(w.Body.Bytes(), &body); err != nil {
			t.Fatalf("the list: %v %s", err, w.Body)
		}
		return len(body.Data)
	}

	for _, c := range []struct {
		what, kind, id string
		want           int
	}{
		{"a doc she can't read", "doc", hidden, http.StatusNotFound},
		{"a doc she reads but doesn't own", "doc", open, http.StatusForbidden},
		{"a board she reads but can't change", "board", board, http.StatusForbidden},
		{"a private table", "table", privateTable, http.StatusNotFound},
		{"a table she sees but doesn't manage", "table", sharedTable, http.StatusForbidden},
		{"a channel she's in but doesn't run", "channel", channel, http.StatusForbidden},
		{"a project she isn't in", "project", hiddenProject, http.StatusNotFound},
		{"a project she's in but doesn't run", "project", project, http.StatusForbidden},
	} {
		t.Run(c.what, func(t *testing.T) {
			grant := link(c.kind, c.id)
			made := as(maya, http.MethodPost, "/guest/links", `{"resource_type":"`+c.kind+`","resource_id":"`+c.id+`","capability":"view","ttl_hours":1}`)
			if made.Code != c.want {
				t.Errorf("she made a link: %d %s", made.Code, made.Body)
			}
			list := as(maya, http.MethodGet, "/guest/links?resource_type="+c.kind+"&resource_id="+c.id, "")
			if c.kind == "doc" {
				// A doc lists only the caller's own links to someone who may
				// not share it, so she sees none of the owner's.
				if list.Code != http.StatusOK || listed(list) != 0 {
					t.Errorf("she listed the owner's links: %d %s", list.Code, list.Body)
				}
			} else if list.Code != c.want {
				t.Errorf("she listed the %s's links: %d %s", c.kind, list.Code, list.Body)
			}
			if w := as(maya, http.MethodPost, "/guest/links/"+grant.String()+"/revoke", ""); w.Code != c.want {
				t.Errorf("she turned the owner's link off: %d %s", w.Code, w.Body)
			}
			if c.kind == "doc" {
				for _, method := range []string{http.MethodPut, http.MethodDelete} {
					if w := as(maya, method, "/guest/links/"+grant.String()+"/publish", `{}`); w.Code != c.want {
						t.Errorf("%s publish of the owner's link: %d %s", method, w.Code, w.Body)
					}
				}
			}
			g, err := guestModel.GetByID(ctx, grant)
			if err != nil || g == nil || g.RevokedAt != nil || g.Slug != nil {
				t.Errorf("the owner's link after her tries: %+v %v", g, err)
			}
			if own := as(owner, http.MethodGet, "/guest/links?resource_type="+c.kind+"&resource_id="+c.id, ""); own.Code != http.StatusOK || listed(own) != 1 {
				t.Errorf("the owner lists their link: %d %s", own.Code, own.Body)
			}
		})
	}
}
