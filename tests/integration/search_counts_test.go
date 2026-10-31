//go:build integration
// +build integration

package integration_test

// Search's counts are the index's honest count, uncapped (decision D-23 in
// design/docs-vs-notion/PROGRAM.md), and these are its guards, against a real
// OpenSearch mapped as the server maps it.
//
// An outsider's count never moves: a private channel filling with matches
// changes nothing for someone who was never in it, in any kind's count or in
// all. Access taken away comes off the count within the index's refresh
// window, and the page never shows the item at any point: the graph has the
// last word on every page from the moment the change returns.
//
// Run: go test -tags=integration ./tests/integration/ -run 'TestAnOutsidersCountNeverMoves|TestTakenAccessLeavesTheCountAndNeverShows' -v

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"reflect"
	"sort"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/opensearch-project/opensearch-go/v4/opensearchapi"

	accessBusiness "github.com/akashc777/OneCamp/business/Access"
	docBusiness "github.com/akashc777/OneCamp/business/Doc"
	globalSearchBusiness "github.com/akashc777/OneCamp/business/GlobalSearch"
	globalSearchController "github.com/akashc777/OneCamp/controllers/GlobalSearch"
	queryController "github.com/akashc777/OneCamp/controllers/SearchQuery"
	postDomain "github.com/akashc777/OneCamp/domain/Post"
	"github.com/akashc777/OneCamp/helpers"
	"github.com/akashc777/OneCamp/initializers/opensearchInit"
	"github.com/akashc777/OneCamp/initializers/postgresInit"
	dgraphStruct "github.com/akashc777/OneCamp/models/dgraph"
	openSearchStruct "github.com/akashc777/OneCamp/models/openSearch"
	userModels "github.com/akashc777/OneCamp/models/postgres/User"
	"github.com/akashc777/OneCamp/tests/integration"
	"github.com/akashc777/OneCamp/tests/searchprobe"
)

func TestAnOutsidersCountNeverMoves(t *testing.T) {
	ctx := context.Background()
	// A search's task hits are checked against who may see them in Postgres
	// (models/taskread), and every hit against its row there (liveHits.go).
	env := integration.ConnectServerPostgres(t)
	search := integration.SetupOpenSearch(t)
	if err := opensearchInit.ConnectOpenSearch(&opensearchInit.OpenSearchConfig{Host: search.URL}); err != nil {
		t.Fatalf("create the indices: %v", err)
	}

	outsider, insider := uuid.NewString(), uuid.NewString()
	general, private := uuid.NewString(), uuid.NewString()
	now := time.Now().Unix()
	exec := func(q string, args ...any) {
		t.Helper()
		if _, err := env.PG.Exec(q, args...); err != nil {
			t.Fatalf("%s: %v", q, err)
		}
	}
	for _, u := range []string{outsider, insider} {
		exec(`INSERT INTO users (id, email_id, username, display_name, created_at, updated_at) VALUES ($1, $2, $3, $4, NOW(), NOW())`, u, u+"@example.test", u, u)
	}
	exec(`INSERT INTO channels (id, ch_name, ch_private) VALUES ($1, 'general', false)`, general)
	exec(`INSERT INTO channels (id, ch_name, ch_private) VALUES ($1, 'launch', true)`, private)
	// write posts n matches in a channel, as a post is written, and makes
	// them searchable at once.
	write := func(channel, body string, n int) {
		t.Helper()
		for i := 0; i < n; i++ {
			post := uuid.NewString()
			exec(`INSERT INTO posts (id, post_channel, created_by) VALUES ($1, $2, $3)`, post, channel, insider)
			postDomain.CreatePostWithAttachmentsInOpenSearch(&openSearchStruct.OpenSearchPost{
				Uuid: post, PostBody: fmt.Sprintf("%s %d", body, i), PostByUserUuid: insider, PostByUserFullName: "Ana Insider",
				PostChannelUuid: channel, PostCreatedAt: now - int64(i), PostUpdatedAt: now - int64(i),
			}, nil)
		}
		if _, err := opensearchInit.OpenSearchClient.Indices.Refresh(ctx, &opensearchapi.IndicesRefreshReq{Indices: []string{"posts"}}); err != nil {
			t.Fatalf("refreshing the posts index: %v", err)
		}
	}
	outside := globalSearchBusiness.Reach{UserUUID: outsider, Email: outsider + "@example.test", Channels: []string{general}}
	inside := globalSearchBusiness.Reach{UserUUID: insider, Email: insider + "@example.test", Channels: []string{general, private}}

	write(general, "rollout plan", 3)
	before := searchprobe.SearchAs(t, ctx, outside, "rollout", globalSearchBusiness.Page{})
	if before.Total != 3 || before.Totals[globalSearchBusiness.KindMessage] != 3 {
		t.Fatalf("the outsider counts %d (%v) in #general; want its 3", before.Total, before.Totals)
	}

	// The private channel fills with matches the outsider was never in.
	write(private, "rollout secret", 30)
	if got := searchprobe.SearchAs(t, ctx, inside, "rollout", globalSearchBusiness.Page{}); got.Total != 33 || !got.HasMore {
		t.Fatalf("someone in both counts %d (more: %v); want all 33, so the index holds the private ones", got.Total, got.HasMore)
	}
	for _, page := range []globalSearchBusiness.Page{{}, {Kinds: []string{globalSearchBusiness.KindMessage}}, {Sort: globalSearchBusiness.SortNewest}} {
		after := searchprobe.SearchAs(t, ctx, outside, "rollout", page)
		if after.Total != before.Total || !reflect.DeepEqual(after.Totals, before.Totals) {
			t.Errorf("the outsider's count moved (%+v): %d %v, was %d %v", page, after.Total, after.Totals, before.Total, before.Totals)
		}
		for _, h := range after.Page {
			if h.Post != nil && h.Post.PostChannelUuid == private {
				t.Errorf("the outsider was shown %q", h.Post.PostBody)
			}
		}
	}
	// A word only the private posts hold counts nothing at all.
	if got := searchprobe.SearchAs(t, ctx, outside, "secret", globalSearchBusiness.Page{}); got.Total != 0 || len(got.Totals) != 0 {
		t.Errorf("a word only the private channel holds counts %d (%v) for the outsider", got.Total, got.Totals)
	}

	// The search box's route and the grammar's (POST /query/search) answer
	// each of them with the same hits and counts: the new route reaches
	// nothing the old one doesn't.
	type answer struct {
		Data struct {
			Page []struct {
				Post *struct {
					Uuid string `json:"post_id"`
				} `json:"post"`
			} `json:"page"`
			Totals map[string]int `json:"totals"`
			Total  int            `json:"total"`
		} `json:"data"`
	}
	ask := func(h http.HandlerFunc, r globalSearchBusiness.Reach, in map[string]string) (ids []string, totals map[string]int, total int) {
		t.Helper()
		var channels []*dgraphStruct.DgraphChannel
		for _, c := range r.Channels {
			channels = append(channels, &dgraphStruct.DgraphChannel{Uuid: c})
		}
		as := context.WithValue(ctx, helpers.UserInfoContextKey, userModels.UserInfo{
			UserPostgresInfo: userModels.User{Id: uuid.MustParse(r.UserUUID), EmailID: r.Email},
			UserDgraphInfo:   dgraphStruct.DgraphUser{Uuid: r.UserUUID, Channels: channels},
		})
		body, _ := json.Marshal(in)
		rec := httptest.NewRecorder()
		h(rec, httptest.NewRequest(http.MethodPost, "/", bytes.NewReader(body)).WithContext(as))
		if rec.Code != http.StatusOK {
			t.Fatalf("%v: %d %s", in, rec.Code, rec.Body.String())
		}
		var out answer
		if err := json.Unmarshal(rec.Body.Bytes(), &out); err != nil {
			t.Fatal(err)
		}
		for _, h := range out.Data.Page {
			if h.Post != nil {
				ids = append(ids, h.Post.Uuid)
			}
		}
		sort.Strings(ids)
		return ids, out.Data.Totals, out.Data.Total
	}
	for name, r := range map[string]globalSearchBusiness.Reach{"the outsider": outside, "someone in both": inside} {
		boxIDs, boxTotals, boxTotal := ask(globalSearchController.GetUnifiedGlobalSearch, r, map[string]string{"global_search_text": "rollout"})
		ids, totals, total := ask(queryController.SearchQuery, r, map[string]string{"q": "rollout"})
		if len(ids) == 0 || !reflect.DeepEqual(ids, boxIDs) || !reflect.DeepEqual(totals, boxTotals) || total != boxTotal {
			t.Errorf("%s: the grammar's route found %v (%v, %d), the search box's %v (%v, %d)", name, ids, totals, total, boxIDs, boxTotals, boxTotal)
		}
	}
}

func TestTakenAccessLeavesTheCountAndNeverShows(t *testing.T) {
	ctx := context.Background()
	env := integration.SetupEnv(t)
	if err := postgresInit.ConnectPostgres(ctx, env.DSN); err != nil {
		t.Fatal(err)
	}
	integration.StubMqttClient(t)
	dg := integration.SetupDgraph(t)
	search := integration.SetupOpenSearch(t)
	if err := opensearchInit.ConnectOpenSearch(&opensearchInit.OpenSearchConfig{Host: search.URL}); err != nil {
		t.Fatalf("create the indices: %v", err)
	}

	people := map[string]uuid.UUID{"owner": uuid.New(), "teammate": uuid.New()}
	var nodes []map[string]any
	for name, id := range people {
		if _, err := env.PG.Exec(`INSERT INTO users (id, email_id, username, display_name, created_at, updated_at) VALUES ($1, $2, $3, $4, NOW(), NOW())`,
			id, id.String()+"@example.test", name, name); err != nil {
			t.Fatal(err)
		}
		nodes = append(nodes, map[string]any{"uid": "_:" + name, "dgraph.type": "User", "user_uuid": id.String(), "user_name": name})
	}
	team := uuid.NewString()
	nodes = append(nodes, map[string]any{"uid": "_:team", "dgraph.type": "Team", "team_uuid": team, "team_name": "Finance",
		"team_deleted_at": "0001-01-01T00:00:00Z", "team_members": []map[string]any{{"uid": "_:teammate"}}})
	uids := dg.Mutate(t, nodes)
	owner := &dgraphStruct.DgraphUser{Uid: uids["owner"], Uuid: people["owner"].String(), UserName: "owner"}
	priv := true
	budget, err := docBusiness.CreateDocInTree(ctx, owner, docBusiness.NewDoc{Title: "Quarterly budget", Private: &priv}, nil)
	if err != nil {
		t.Fatal(err)
	}
	asOwner := accessBusiness.Subject{UserUID: uids["owner"], UserUUID: people["owner"].String()}
	resource := accessBusiness.Resource{Kind: accessBusiness.KindDoc, UUID: budget.Uuid}
	if _, err := accessBusiness.Apply(ctx, asOwner, accessBusiness.Change{
		Resource: resource, Add: []accessBusiness.Grant{{Kind: accessBusiness.PrincipalTeam, UUID: team, Level: accessBusiness.LevelView}},
	}); err != nil {
		t.Fatal(err)
	}

	mate := globalSearchBusiness.Reach{UserUUID: people["teammate"].String(), Email: people["teammate"].String() + "@example.test", Teams: []string{team}}
	shown := func(r globalSearchBusiness.Result) bool {
		for _, h := range r.Page {
			if h.Doc != nil && h.Doc.Uuid == budget.Uuid {
				return true
			}
		}
		return false
	}
	// The page of every kind, which would show the doc, and the tasks' page,
	// which can't, so its doc count is the index's own count, untouched by
	// any page's check.
	everything := globalSearchBusiness.Page{}
	tasksOnly := globalSearchBusiness.Page{Kinds: []string{globalSearchBusiness.KindTask}}
	deadline := time.Now().Add(30 * time.Second)
	for {
		r := searchprobe.SearchAs(t, ctx, mate, "quarterly budget", everything)
		if shown(r) && r.Totals[globalSearchBusiness.KindDoc] == 1 {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("the team's member never found the budget: %d in all, %v", r.Total, r.Totals)
		}
		time.Sleep(200 * time.Millisecond)
	}
	if r := searchprobe.SearchAs(t, ctx, mate, "quarterly budget", tasksOnly); r.Totals[globalSearchBusiness.KindDoc] != 1 {
		t.Fatalf("the tasks' page counts %v; want the doc counted beside it", r.Totals)
	}

	// The team loses the doc. From the moment that returns, no page shows it.
	if _, err := accessBusiness.Apply(ctx, asOwner, accessBusiness.Change{
		Resource: resource, Remove: []accessBusiness.Grant{{Kind: accessBusiness.PrincipalTeam, UUID: team}},
	}); err != nil {
		t.Fatal(err)
	}
	taken := time.Now()
	for i := 0; i < 5; i++ {
		if r := searchprobe.SearchAs(t, ctx, mate, "quarterly budget", everything); shown(r) || r.Totals[globalSearchBusiness.KindDoc] != 0 {
			t.Fatalf("%v after the change: the page shows the doc (%v) or counts it (%v)", time.Since(taken), shown(r), r.Totals)
		}
	}
	// The index's own count too: the change wrote the entry, and search saw
	// it, before it returned (docs C2-1, revocation first). The tasks' page
	// can't show the doc, so its doc count is the index's alone.
	if r := searchprobe.SearchAs(t, ctx, mate, "quarterly budget", tasksOnly); r.Totals[globalSearchBusiness.KindDoc] != 0 {
		t.Fatalf("%v after the change the index still counts the doc: %v", time.Since(taken), r.Totals)
	}
	if r := searchprobe.SearchAs(t, ctx, mate, "quarterly budget", everything); shown(r) || r.Total != 0 {
		t.Errorf("once written: the page shows the doc (%v), or counts %d", shown(r), r.Total)
	}
}

// countsOnlyWhatShows: on a search's last page each kind counts exactly the
// hits of it the page shows, so nothing kept off the page (a doc, a board, a
// file or a task the asker may not open) is in any count.
func countsOnlyWhatShows(t *testing.T, what string, p globalSearchBusiness.GlobalSearchPagination) {
	t.Helper()
	if p.HasMore {
		t.Fatalf("%s: more than one page; the world is meant to fit one", what)
	}
	shown := map[string]int{}
	for _, h := range p.Page {
		switch {
		case h.Doc != nil:
			shown[globalSearchBusiness.KindDoc]++
		case h.Board != nil:
			shown[globalSearchBusiness.KindBoard]++
		case h.Attachment != nil:
			shown[globalSearchBusiness.KindFile]++
		case h.Task != nil:
			shown[globalSearchBusiness.KindTask]++
		case h.Chat != nil, h.Post != nil, h.Comment != nil:
			shown[globalSearchBusiness.KindMessage]++
		case h.User != nil:
			shown[globalSearchBusiness.KindPerson]++
		case h.Project != nil:
			shown[globalSearchBusiness.KindProject]++
		case h.Channel != nil:
			shown[globalSearchBusiness.KindChannel]++
		case h.Team != nil:
			shown[globalSearchBusiness.KindTeam]++
		case h.Event != nil:
			shown[globalSearchBusiness.KindEvent]++
		}
	}
	counted := map[string]int{}
	for kind, n := range p.Totals {
		if n != 0 {
			counted[kind] = n
		}
	}
	if !reflect.DeepEqual(counted, shown) || p.Total != len(p.Page) {
		t.Errorf("%s: counts %v (%d in all) beside a page of %v (%d)", what, counted, p.Total, shown, len(p.Page))
	}
}
