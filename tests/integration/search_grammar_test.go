//go:build integration
// +build integration

package integration_test

// The one grammar searches the index (docs C2-1), against a real OpenSearch
// mapped as the server maps it, as the asker: from:, in: (a channel by #name
// or by name, a DM, a project or a team by name), type:, title:, a phrase, a
// -word, dates (made and changed, in the asker's zone), counts and pages. The
// posts index is made first as an install before the searchable copies had
// it, with an entry in it, so the one-time pass is seen to bring that entry
// into from: filters.
//
// Run: go test -tags=integration ./tests/integration/ -run TestTheGrammarSearchesTheIndex -v

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"net/url"
	"sort"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/opensearch-project/opensearch-go/v4/opensearchapi"

	accessBusiness "github.com/akashc777/OneCamp/business/Access"
	_ "github.com/akashc777/OneCamp/business/Conversation" // registers the channel rule, as the server links it
	globalSearchBusiness "github.com/akashc777/OneCamp/business/GlobalSearch"
	querySpec "github.com/akashc777/OneCamp/business/QuerySpec"
	searchQuery "github.com/akashc777/OneCamp/business/SearchQuery"
	globalSearchController "github.com/akashc777/OneCamp/controllers/GlobalSearch"
	v1Controller "github.com/akashc777/OneCamp/controllers/V1"
	channelDomain "github.com/akashc777/OneCamp/domain/Channel"
	chatDomain "github.com/akashc777/OneCamp/domain/Chat"
	postDomain "github.com/akashc777/OneCamp/domain/Post"
	projectDomain "github.com/akashc777/OneCamp/domain/Project"
	taskDomain "github.com/akashc777/OneCamp/domain/Task"
	userDomain "github.com/akashc777/OneCamp/domain/User"
	"github.com/akashc777/OneCamp/helpers"
	"github.com/akashc777/OneCamp/initializers/opensearchInit"
	"github.com/akashc777/OneCamp/initializers/postgresInit"
	dgraphStruct "github.com/akashc777/OneCamp/models/dgraph"
	openSearchStruct "github.com/akashc777/OneCamp/models/openSearch"
	userModels "github.com/akashc777/OneCamp/models/postgres/User"
	"github.com/akashc777/OneCamp/tests/integration"
)

// postsBeforeCopies is the posts index as installs made it before the
// searchable copies (initializers/opensearchInit, before docs C2-1).
const postsBeforeCopies = `{
	"settings": { "index": { "number_of_shards": 1, "number_of_replicas": 0 } },
	"mappings": {
		"properties": {
			"post_body": { "type": "text", "index": true },
			"post_by_user_id": { "type": "keyword", "index": false },
			"post_by_user_full_name": { "type": "text", "index": true },
			"post_by_profile": { "type": "keyword", "index": false },
			"post_id": { "type": "keyword", "index": false },
			"post_ch_id": { "type": "keyword", "index": true},
			"post_ch_name": { "type": "text", "index": true},
			"created_date": { "type": "date", "format": "epoch_second", "index": true },
			"updated_date": { "type": "date", "format": "epoch_second", "index": false },
			"deleted_date": { "type": "date", "format": "epoch_second", "index": true }
		}
	}
}`

func TestTheGrammarSearchesTheIndex(t *testing.T) {
	ctx := context.Background()
	t.Setenv("CONVERSATION_ACCESS_SOURCE", "graph")
	env := integration.SetupEnv(t)
	if err := postgresInit.ConnectPostgres(ctx, env.DSN); err != nil {
		t.Fatal(err)
	}
	integration.SetupRedis(t)
	integration.StubMqttClient(t)
	dg := integration.SetupDgraph(t)
	search := integration.SetupOpenSearch(t)
	exec := func(q string, args ...any) {
		t.Helper()
		if _, err := env.PG.Exec(q, args...); err != nil {
			t.Fatalf("%s: %v", q, err)
		}
	}
	at := func(s string) int64 {
		t.Helper()
		tm, err := time.Parse(time.RFC3339, s)
		if err != nil {
			t.Fatal(err)
		}
		return tm.Unix()
	}

	// Sam is in #launch (private) and #general, the Q4 launch project and
	// the Ops team; Ana writes in both channels and has a DM with Sam.
	sam, ana := uuid.New(), uuid.New()
	launch, general := uuid.New(), uuid.New()
	team, project := uuid.New(), uuid.New()
	for name, id := range map[string]uuid.UUID{"sam": sam, "ana": ana} {
		exec(`INSERT INTO users (id, email_id, username, display_name, created_at, updated_at) VALUES ($1, $2 || '@acme.test', $2, $2, NOW(), NOW())`, id, name)
	}
	exec(`INSERT INTO channels (id, ch_name, ch_private) VALUES ($1, 'launch', true)`, launch)
	exec(`INSERT INTO channels (id, ch_name, ch_private) VALUES ($1, 'general', false)`, general)
	exec(`INSERT INTO teams (id, team_name, created_by) VALUES ($1, 'Ops', $2)`, team, sam)
	exec(`INSERT INTO projects (id, project_name, team_id, created_by) VALUES ($1, 'Q4 launch', $2, $3)`, project, team, sam)
	live := "0001-01-01T00:00:00Z"
	dg.Mutate(t, []map[string]any{
		{"uid": "_:sam", "dgraph.type": "User", "user_uuid": sam.String(), "user_name": "sam",
			"user_projects": []map[string]any{{"uid": "_:project"}}, "user_teams": []map[string]any{{"uid": "_:team"}}},
		{"uid": "_:ana", "dgraph.type": "User", "user_uuid": ana.String(), "user_name": "ana"},
		{"uid": "_:launch", "dgraph.type": "Channel", "ch_uuid": launch.String(), "ch_name": "launch",
			"ch_private": true, "ch_deleted_at": live, "ch_members": []map[string]any{{"uid": "_:sam"}, {"uid": "_:ana"}}},
		{"uid": "_:general", "dgraph.type": "Channel", "ch_uuid": general.String(), "ch_name": "general",
			"ch_private": false, "ch_deleted_at": live, "ch_members": []map[string]any{{"uid": "_:sam"}, {"uid": "_:ana"}}},
		{"uid": "_:team", "dgraph.type": "Team", "team_uuid": team.String(), "team_name": "Ops",
			"team_deleted_at": live, "team_members": []map[string]any{{"uid": "_:sam"}}},
		{"uid": "_:project", "dgraph.type": "Project", "project_uuid": project.String(), "project_name": "Q4 launch",
			"project_deleted_at": live, "project_members": []map[string]any{{"uid": "_:sam"}}},
	})

	// The install before the copies: its posts index, and one post in it.
	names := map[string]string{}
	old := uuid.New()
	names[old.String()] = "old"
	exec(`INSERT INTO posts (id, post_channel, created_by) VALUES ($1, $2, $3)`, old, launch, sam)
	if _, err := opensearchInit.OpenSearchClient.Indices.Create(ctx, opensearchapi.IndicesCreateReq{Index: "posts", Body: strings.NewReader(postsBeforeCopies)}); err != nil {
		t.Fatalf("the posts index as it was made: %v", err)
	}
	oldEntry, _ := json.Marshal(&openSearchStruct.OpenSearchPost{Uuid: old.String(), PostBody: "launch budget approved", PostByUserUuid: sam.String(),
		PostChannelUuid: launch.String(), PostCreatedAt: at("2026-09-20T10:00:00Z"), PostUpdatedAt: at("2026-09-20T10:00:00Z")})
	if _, err := opensearchInit.OpenSearchClient.Index(ctx, opensearchapi.IndexReq{Index: "posts", DocumentID: old.String(),
		Body: strings.NewReader(string(oldEntry)), Params: opensearchapi.IndexParams{Refresh: "true"}}); err != nil {
		t.Fatalf("the old post: %v", err)
	}
	// The server starts: the other indices are made, and the copies mapped.
	if err := opensearchInit.ConnectOpenSearch(&opensearchInit.OpenSearchConfig{Host: search.URL}); err != nil {
		t.Fatalf("connect: %v", err)
	}
	if !opensearchInit.QueryFieldReady("posts", "post_by_user_id.q") || !opensearchInit.QueryFieldReady("posts", "updated_date.q") {
		t.Fatal("the posts index made before the copies didn't take them")
	}

	// What's written after.
	post := func(name, body string, by, channel uuid.UUID, created, updated string) {
		t.Helper()
		id := uuid.New()
		names[id.String()] = name
		exec(`INSERT INTO posts (id, post_channel, created_by) VALUES ($1, $2, $3)`, id, channel, by)
		postDomain.CreatePostWithAttachmentsInOpenSearch(&openSearchStruct.OpenSearchPost{Uuid: id.String(), PostBody: body, PostByUserUuid: by.String(),
			PostChannelUuid: channel.String(), PostCreatedAt: at(created), PostUpdatedAt: at(updated)}, nil)
	}
	post("checklist", "launch checklist ready", sam, launch, "2026-10-01T12:00:00Z", "2026-10-01T12:00:00Z")
	post("party", "launch party photos", ana, launch, "2026-10-10T12:00:00Z", "2026-10-15T09:00:00Z")
	post("pricing", "pricing page draft", sam, launch, "2026-10-12T12:00:00Z", "2026-10-12T12:00:00Z")
	post("retro", "launch retro notes", ana, general, "2026-10-05T12:00:00Z", "2026-10-05T12:00:00Z")
	post("eve", "launch eve dinner", ana, general, "2026-09-30T20:00:00Z", "2026-09-30T20:00:00Z")
	dm, task := uuid.New(), uuid.New()
	names[dm.String()], names[task.String()], names[launch.String()], names[project.String()] = "dm", "task", "#launch", "project"
	exec(`INSERT INTO chats (id, grp_id, created_by) VALUES ($1, $2, $3)`, dm, helpers.GetGroupingId(sam.String(), ana.String()), ana)
	chatDomain.CreateChatWithAttachmentsInOpenSearch(&openSearchStruct.OpenSearchChat{Uuid: dm.String(), ChatBody: "launch dates confirmed?", ChatByUserUuid: ana.String(),
		ChatParticipants: []*openSearchStruct.OpenSearchChatParticipants{{Uuid: sam.String(), Name: "sam"}, {Uuid: ana.String(), Name: "ana"}},
		ChatGrpId:        helpers.GetGroupingId(sam.String(), ana.String()),
		ChatCreatedAt:    at("2026-10-02T12:00:00Z"), ChatUpdatedAt: at("2026-10-02T12:00:00Z")}, nil)
	exec(`INSERT INTO tasks (id, project_id, created_by) VALUES ($1, $2, $3)`, task, project, sam)
	taskDomain.CreateTaskWithAttachmentsInOpensearch(&openSearchStruct.OpenSearchTask{Uuid: task.String(), TaskName: "Launch checklist", TaskStatus: "inProgress",
		TaskProjectUuid: project.String(), TaskCreatedByUserUuid: sam.String(), TaskCreatedAt: at("2026-10-03T12:00:00Z"), TaskUpdatedAt: at("2026-10-03T12:00:00Z")}, nil, nil)
	channelDomain.CreateChannelInOpenSearch(&openSearchStruct.OpenSearchChannel{Uuid: launch.String(), ChannelName: "launch", ChannelCreatedAt: at("2026-09-01T12:00:00Z")})
	projectDomain.CreateProjectInOpenSearch(&openSearchStruct.OpenSearchProject{Uuid: project.String(), ProjectName: "Q4 launch", ProjectTeamUuid: team.String(),
		ProjectCreatedAt: at("2026-09-01T12:00:00Z")})
	refresh := func() {
		t.Helper()
		if _, err := opensearchInit.OpenSearchClient.Indices.Refresh(ctx, &opensearchapi.IndicesRefreshReq{}); err != nil {
			t.Fatalf("refresh: %v", err)
		}
	}
	refresh()

	pgSam, err := userDomain.GetActiveUserWithAdminFlagByUserUUID(ctx, sam)
	if err != nil || pgSam == nil {
		t.Fatalf("load sam: %v", err)
	}
	grSam, err := userDomain.GetDgraphUserInfoByUUID(ctx, sam.String())
	if err != nil || grSam == nil {
		t.Fatalf("load sam's node: %v", err)
	}
	info := userModels.UserInfo{UserPostgresInfo: *pgSam, UserDgraphInfo: *grSam}
	who := searchQuery.Asker{Subject: accessBusiness.SubjectOf(&info), Email: pgSam.EmailID}
	reach := globalSearchBusiness.Reach{UserUUID: sam.String(), Email: pgSam.EmailID,
		Channels: []string{launch.String(), general.String()}, Projects: []string{project.String()}, Teams: []string{team.String()}}

	run := func(text string, page globalSearchBusiness.Page) globalSearchBusiness.Result {
		t.Helper()
		spec, issues := searchQuery.Build(ctx, who, text, querySpec.Scope{Kind: querySpec.ScopeSearch}, searchQuery.ModeSearch)
		for _, is := range issues {
			t.Errorf("%q: %s", text, is.Message)
		}
		r, err := globalSearchBusiness.Search(ctx, reach, spec, page)
		if err != nil {
			t.Fatalf("%q: %v", text, err)
		}
		for _, n := range r.Notes {
			t.Errorf("%q: %s", text, n.Message)
		}
		return r
	}
	found := func(r globalSearchBusiness.Result) string {
		var out []string
		for _, h := range r.Page {
			id := ""
			switch {
			case h.Post != nil:
				id = h.Post.Uuid
			case h.Chat != nil:
				id = h.Chat.Uuid
			case h.Task != nil:
				id = h.Task.Uuid
			case h.Channel != nil:
				id = h.Channel.Uuid
			case h.Project != nil:
				id = h.Project.Uuid
			}
			name := names[id]
			if name == "" {
				name = "?" + h.Type
			}
			out = append(out, name)
		}
		sort.Strings(out)
		return strings.Join(out, " ")
	}
	expect := func(text string, page globalSearchBusiness.Page, want string) {
		t.Helper()
		if got := found(run(text, page)); got != want {
			t.Errorf("%q finds [%s]; want [%s]", text, got, want)
		}
	}
	all := globalSearchBusiness.Page{}

	// The old post has no copy of who wrote it until the pass runs.
	expect("launch from:@sam", all, "checklist task")
	if err := globalSearchBusiness.BackfillQueryCopies(ctx); err != nil {
		t.Fatalf("the pass: %v", err)
	}
	refresh()
	expect("launch from:@sam", all, "checklist old task")
	expect("launch -from:@sam", all, "#launch dm eve party project retro")

	everything := run("launch", all)
	if got := found(everything); got != "#launch checklist dm eve old party project retro task" {
		t.Errorf("launch finds [%s]", got)
	}
	if everything.Total != 9 || everything.Totals["message"] != 6 || everything.Totals["task"] != 1 || everything.Totals["channel"] != 1 || everything.Totals["project"] != 1 {
		t.Errorf("launch counts %d %v", everything.Total, everything.Totals)
	}
	expect("launch in:#launch", all, "checklist old party")
	expect("launch in:general", all, "eve retro")
	expect(`launch in:"Q4 launch"`, all, "task")
	expect("launch in:Ops", all, "project")
	expect("launch in:@ana", all, "dm")
	expect("launch type:task", all, "task")
	expect("type:message launch", all, "checklist dm eve old party retro")
	expect("title:checklist", all, "task")
	expect(`"launch party"`, all, "party")
	expect("launch -party", all, "#launch checklist dm eve old project retro task")
	expect("launch after:2026-10-04", all, "party retro")
	expect("launch updated:2026-10-15", all, "party")
	expect("launch on:2026-10-01", all, "checklist")
	expect("launch on:2026-10-01", globalSearchBusiness.Page{Zone: time.FixedZone("IST", 5*3600+1800)}, "checklist eve")
	expect("launch ORDER BY created", all, "#launch checklist dm eve old party project retro task")

	// Pages of four go through all nine once, each counting nine, and the
	// last one ends there.
	seen := map[string]int{}
	after := ""
	for i := 0; i < 4; i++ {
		r := run("launch", globalSearchBusiness.Page{Size: 4, After: after, Sort: globalSearchBusiness.SortNewest})
		if r.Total != 9 {
			t.Errorf("page %d counts %d", i+1, r.Total)
		}
		for _, name := range strings.Fields(found(r)) {
			seen[name]++
		}
		if !r.HasMore {
			if i != 2 || r.Next != "" {
				t.Errorf("the pages ended after %d (next %q); want 3", i+1, r.Next)
			}
			break
		}
		after = r.Next
	}
	if len(seen) != 9 {
		t.Errorf("the pages held %d of the 9: %v", len(seen), seen)
	}
	for name, n := range seen {
		if n != 1 {
			t.Errorf("%s shows %d times", name, n)
		}
	}

	// The app's search (/search) and the public API's (/v1/search) read their
	// text as the person asking, as the query route does: /search names them
	// by the request (accessBusiness.RequestSubject), the API by its token
	// (accessBusiness.SubjectFor). Both built the asker from a uuid alone,
	// with no graph uid, so a team named in the text was found for no one and
	// "launch in:Ops" narrowed nothing, even for Sam. Ana isn't in Ops: naming
	// it still finds her nothing of its project, through either.
	signedIn := func(who uuid.UUID, channels, projects, teams []uuid.UUID) context.Context {
		t.Helper()
		pg, err := userDomain.GetActiveUserWithAdminFlagByUserUUID(ctx, who)
		if err != nil || pg == nil {
			t.Fatalf("load %s: %v", who, err)
		}
		gr, err := userDomain.GetDgraphUserInfoByUUID(ctx, who.String())
		if err != nil || gr == nil {
			t.Fatalf("load %s's node: %v", who, err)
		}
		// The places sign-in loads for them, as the reach above.
		for _, id := range channels {
			gr.Channels = append(gr.Channels, &dgraphStruct.DgraphChannel{Uuid: id.String()})
		}
		for _, id := range projects {
			gr.Projects = append(gr.Projects, &dgraphStruct.DgraphProject{Uuid: id.String()})
		}
		for _, id := range teams {
			gr.Teams = append(gr.Teams, &dgraphStruct.DgraphTeam{Uuid: id.String()})
		}
		return context.WithValue(ctx, helpers.UserInfoContextKey, userModels.UserInfo{UserPostgresInfo: *pg, UserDgraphInfo: *gr})
	}
	asSam := signedIn(sam, []uuid.UUID{launch, general}, []uuid.UUID{project}, []uuid.UUID{team})
	asAna := signedIn(ana, []uuid.UUID{launch, general}, nil, nil)
	searches := map[string]func(as context.Context, text string) string{
		"/search": func(as context.Context, text string) string {
			raw, _ := json.Marshal(map[string]string{"global_search_text": text})
			rec := httptest.NewRecorder()
			globalSearchController.GetUnifiedGlobalSearch(rec, httptest.NewRequest(http.MethodPost, "/", bytes.NewReader(raw)).WithContext(as))
			if rec.Code != http.StatusOK {
				t.Fatalf("/search %q: %d %s", text, rec.Code, rec.Body)
			}
			return rec.Body.String()
		},
		"/v1/search": func(as context.Context, text string) string {
			rec := httptest.NewRecorder()
			v1Controller.SearchV1(rec, httptest.NewRequest(http.MethodGet, "/?q="+url.QueryEscape(text), nil).WithContext(as))
			if rec.Code != http.StatusOK {
				t.Fatalf("/v1/search %q: %d %s", text, rec.Code, rec.Body)
			}
			return rec.Body.String()
		},
	}
	// named is which of the entries above an answer names.
	named := func(body string) string {
		var out []string
		for id, name := range names {
			if strings.Contains(body, id) {
				out = append(out, name)
			}
		}
		sort.Strings(out)
		return strings.Join(out, " ")
	}
	for route, search := range searches {
		if got := named(search(asSam, "launch in:Ops")); got != "project" {
			t.Errorf("%s: \"launch in:Ops\" as Sam names [%s]; want [project]", route, got)
		}
		if got := named(search(asAna, "launch in:Ops")); strings.Contains(got, "project") || strings.Contains(got, "task") {
			t.Errorf("%s: \"launch in:Ops\" as Ana, who isn't in Ops, names [%s]; want nothing of its project", route, got)
		}
	}
}
