//go:build integration
// +build integration

package integration_test

// Can the demo's shared visitor archive or delete the demo's own content?
//
// Everyone who opens the demo is the same account, and the seeder makes the
// demo's channels, projects, team and docs as that account, so the visitor
// owns them. One visitor archiving #engineering took it from every visitor
// after them until the nightly reset. business/DemoGuard keeps what existed
// when the demo was last seeded; this runs the real handlers against a real
// graph and database: the demo's own things are refused with code "demo" and
// left as they were, the visitor's own things still archive and delete, and
// nobody else is affected.
//
// Run: go test -tags=integration ./tests/integration/ -run TestDemoSeeded -v

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/opensearch-project/opensearch-go/v4"
	"github.com/opensearch-project/opensearch-go/v4/opensearchapi"

	channelAdapter "github.com/akashc777/OneCamp/adapter/Channel"
	docAdapter "github.com/akashc777/OneCamp/adapter/Doc"
	projectAdapter "github.com/akashc777/OneCamp/adapter/Project"
	teamAdapter "github.com/akashc777/OneCamp/adapter/Team"
	demoGuard "github.com/akashc777/OneCamp/business/DemoGuard"
	docBusiness "github.com/akashc777/OneCamp/business/Doc"
	channelController "github.com/akashc777/OneCamp/controllers/Channel"
	docController "github.com/akashc777/OneCamp/controllers/Doc"
	projectController "github.com/akashc777/OneCamp/controllers/Project"
	teamController "github.com/akashc777/OneCamp/controllers/Team"
	"github.com/akashc777/OneCamp/helpers"
	"github.com/akashc777/OneCamp/initializers/opensearchInit"
	"github.com/akashc777/OneCamp/initializers/postgresInit"
	dgraphStruct "github.com/akashc777/OneCamp/models/dgraph"
	userModels "github.com/akashc777/OneCamp/models/postgres/User"
)

func TestDemoSeededContentStaysForEveryVisitor(t *testing.T) {
	dg := wireStores(t)
	ctx := context.Background()
	pg := postgresInit.DBConn.SqlDB

	// Archiving and deleting index in the background; give it somewhere to go.
	search := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		fmt.Fprint(w, `{"took":1,"errors":false,"items":[],"updated":0,"failures":[]}`)
	}))
	defer search.Close()
	client, err := opensearchapi.NewClient(opensearchapi.Config{Client: opensearch.Config{Addresses: []string{search.URL}}})
	if err != nil {
		t.Fatal(err)
	}
	opensearchInit.OpenSearchClient = client

	t.Setenv("DEMO_MODE", "true")
	t.Setenv("DEMO_USER_EMAIL", "visitor@demo.example")

	// The demo was seeded an hour ago. Its own things are from just before;
	// the visitor's are from this morning, after it.
	seededAt := time.Now().Add(-time.Hour).UTC()
	theDemos := seededAt.Add(-10 * time.Minute).Format(time.RFC3339)
	visitors := seededAt.Add(30 * time.Minute).Format(time.RFC3339)
	if err := demoGuard.MarkSeeded(seededAt); err != nil {
		t.Fatalf("recording the seeding: %v", err)
	}

	people := map[string]struct {
		id    uuid.UUID
		email string
	}{
		"visitor": {uuid.New(), "visitor@demo.example"},
		"owner":   {uuid.New(), "owner@company.example"},
	}
	var nodes []map[string]any
	for name, p := range people {
		if _, err := pg.ExecContext(ctx, `INSERT INTO users (id, email_id, username, display_name, created_at, updated_at)
			VALUES ($1, $2, $3, $4, NOW(), NOW())`, p.id, p.email, name, name); err != nil {
			t.Fatal(err)
		}
		nodes = append(nodes, map[string]any{"uid": "_:" + name, "dgraph.type": "User", "user_uuid": p.id.String(), "user_name": name})
	}

	ids := map[string]uuid.UUID{}
	for _, k := range []string{"seededDoc", "ownDoc", "ownersDoc", "seededCh", "ownCh", "team", "seededProject", "ownProject"} {
		ids[k] = uuid.New()
	}
	doc := func(key, by, created string) map[string]any {
		return map[string]any{"uid": "_:" + key, "dgraph.type": "Doc", "doc_uuid": ids[key].String(), "doc_title": key,
			"doc_private": false, "doc_deleted_at": "0001-01-01T00:00:00Z", "doc_created_at": created,
			"doc_created_by": map[string]any{"uid": "_:" + by}}
	}
	channel := func(key, created string) map[string]any {
		if _, err := pg.ExecContext(ctx, `INSERT INTO channels (id, ch_name, ch_private, created_by) VALUES ($1, $2, false, $3)`,
			ids[key], key, people["visitor"].id); err != nil {
			t.Fatal(err)
		}
		return map[string]any{"uid": "_:" + key, "dgraph.type": "Channel", "ch_uuid": ids[key].String(), "ch_name": key,
			"ch_private": false, "ch_deleted_at": "0001-01-01T00:00:00Z", "ch_created_at": created,
			"ch_members": []map[string]any{{"uid": "_:visitor"}}, "ch_moderators": []map[string]any{{"uid": "_:visitor"}}}
	}
	if _, err := pg.ExecContext(ctx, `INSERT INTO teams (id, team_name, created_by) VALUES ($1, 'Launch', $2)`, ids["team"], people["visitor"].id); err != nil {
		t.Fatal(err)
	}
	project := func(key, created string) map[string]any {
		if _, err := pg.ExecContext(ctx, `INSERT INTO projects (id, project_name, team_id, created_by) VALUES ($1, $2, $3, $4)`,
			ids[key], key, ids["team"], people["visitor"].id); err != nil {
			t.Fatal(err)
		}
		return map[string]any{"uid": "_:" + key, "dgraph.type": "Project", "project_uuid": ids[key].String(), "project_name": key,
			"project_deleted_at": "0001-01-01T00:00:00Z", "project_created_at": created,
			"project_team": map[string]any{"uid": "_:team"}, "project_admins": []map[string]any{{"uid": "_:visitor"}}}
	}
	nodes = append(nodes,
		doc("seededDoc", "visitor", theDemos), doc("ownDoc", "visitor", visitors), doc("ownersDoc", "owner", theDemos),
		channel("seededCh", theDemos), channel("ownCh", visitors),
		map[string]any{"uid": "_:team", "dgraph.type": "Team", "team_uuid": ids["team"].String(), "team_name": "Launch",
			"team_deleted_at": "0001-01-01T00:00:00Z", "team_created_at": theDemos,
			"team_admins": []map[string]any{{"uid": "_:visitor"}}, "team_members": []map[string]any{{"uid": "_:visitor"}}},
		project("seededProject", theDemos), project("ownProject", visitors),
	)
	uids := dg.Mutate(t, nodes)

	as := func(name string, admin bool) userModels.UserInfo {
		var u userModels.UserInfo
		u.UserPostgresInfo.Id = people[name].id
		u.UserPostgresInfo.EmailID = people[name].email
		u.UserPostgresInfo.IsAdmin = admin
		u.UserDgraphInfo = dgraphStruct.DgraphUser{Uid: uids[name], Uuid: people[name].id.String(), UserName: name}
		return u
	}
	call := func(who userModels.UserInfo, handler http.HandlerFunc, body any) (int, map[string]any) {
		t.Helper()
		raw, _ := json.Marshal(body)
		rec := httptest.NewRecorder()
		req := httptest.NewRequest(http.MethodPost, "/", bytes.NewReader(raw)).
			WithContext(context.WithValue(ctx, helpers.UserInfoContextKey, who))
		handler(rec, req)
		var out map[string]any
		_ = json.Unmarshal(rec.Body.Bytes(), &out)
		return rec.Code, out
	}
	refused := func(what string, code int, out map[string]any) {
		t.Helper()
		if code != http.StatusForbidden || out["code"] != "demo" {
			t.Errorf("%s: got %d %v, want 403 with code \"demo\"", what, code, out)
		}
		if msg, _ := out["msg"].(string); msg != helpers.DemoSeededMsg {
			t.Errorf("%s: says %q, not why", what, msg)
		}
	}
	allowed := func(what string, code int, out map[string]any) {
		t.Helper()
		if code != http.StatusOK {
			t.Errorf("%s: got %d %v, want 200", what, code, out)
		}
	}
	docGone := func(key string) bool {
		t.Helper()
		d, err := docBusiness.GetBasicDgraphDocByUUID(ctx, ids[key].String(), uids["visitor"])
		if err != nil || d == nil {
			t.Fatalf("reading %s back: %v", key, err)
		}
		return d.DeletedAt != nil && d.DeletedAt.After(time.Date(1970, 1, 1, 0, 0, 0, 0, time.UTC))
	}
	rowGone := func(table string, key string) bool {
		t.Helper()
		var deleted *time.Time
		if err := pg.QueryRowContext(ctx, `SELECT deleted_at FROM `+table+` WHERE id = $1`, ids[key]).Scan(&deleted); err != nil {
			t.Fatalf("reading %s back: %v", key, err)
		}
		return deleted != nil
	}
	visitor := as("visitor", false)
	archive := func(key string) channelAdapter.UpdateChannelInfo {
		return channelAdapter.UpdateChannelInfo{ChannelUuid: ids[key].String(), ChannelName: key, ChannelArchived: true}
	}

	// Docs.
	code, out := call(visitor, docController.DeleteDoc, docAdapter.InputUpdateDoc{DocId: ids["seededDoc"].String()})
	refused("the visitor deleting the demo's doc", code, out)
	if docGone("seededDoc") {
		t.Error("the demo's doc was deleted anyway")
	}
	code, out = call(visitor, docController.DeleteDoc, docAdapter.InputUpdateDoc{DocId: ids["ownDoc"].String()})
	allowed("the visitor deleting a doc they made", code, out)
	if !docGone("ownDoc") {
		t.Error("the visitor's own doc was not deleted")
	}

	// Channels, archived from the edit dialog.
	code, out = call(visitor, channelController.UpdateChannelInfo, archive("seededCh"))
	refused("the visitor archiving the demo's channel", code, out)
	if rowGone("channels", "seededCh") {
		t.Error("the demo's channel was archived anyway")
	}
	code, out = call(visitor, channelController.UpdateChannelInfo, archive("ownCh"))
	allowed("the visitor archiving a channel they made", code, out)
	if !rowGone("channels", "ownCh") {
		t.Error("the visitor's own channel was not archived")
	}
	// Editing the demo's channel without archiving it is still editing.
	code, out = call(visitor, channelController.UpdateChannelInfo, channelAdapter.UpdateChannelInfo{
		ChannelUuid: ids["seededCh"].String(), ChannelName: "seededCh", ChannelAbout: "What the launch team talks about"})
	allowed("the visitor editing the demo's channel", code, out)

	// Projects.
	code, out = call(visitor, projectController.ArchiveProject, projectAdapter.AddOrRemoveProjectInput{ProjectUuid: ids["seededProject"].String()})
	refused("the visitor archiving the demo's project", code, out)
	if rowGone("projects", "seededProject") {
		t.Error("the demo's project was archived anyway")
	}
	code, out = call(visitor, projectController.ArchiveProject, projectAdapter.AddOrRemoveProjectInput{ProjectUuid: ids["ownProject"].String()})
	allowed("the visitor archiving a project they made", code, out)
	if !rowGone("projects", "ownProject") {
		t.Error("the visitor's own project was not archived")
	}

	// The team. Archiving teams is for admins, which the visitor is not; were
	// somebody to make it one, the demo's team still stays.
	code, out = call(as("visitor", true), teamController.ArchiveTeam, teamAdapter.CreateOrUpdateTeamInput{Uuid: ids["team"].String()})
	refused("the visitor, made an admin, archiving the demo's team", code, out)
	if rowGone("teams", "team") {
		t.Error("the demo's team was archived anyway")
	}

	// The people who run the demo are not the shared visitor.
	code, out = call(as("owner", false), docController.DeleteDoc, docAdapter.InputUpdateDoc{DocId: ids["ownersDoc"].String()})
	allowed("the demo's own team deleting their doc", code, out)

	// And on any other server, nothing is kept from anybody.
	t.Setenv("DEMO_MODE", "")
	code, out = call(visitor, docController.DeleteDoc, docAdapter.InputUpdateDoc{DocId: ids["seededDoc"].String()})
	allowed("the same account on a server that is not the demo", code, out)
}
