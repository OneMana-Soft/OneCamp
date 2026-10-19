//go:build integration
// +build integration

package integration_test

// The one query grammar's names resolve as the asker, against real stores
// (business/SearchQuery; design/QUERY_STACK_CONTRACT.md §4.6): a channel by
// #name only when the asker may read it (a member, or anyone for a public
// one, through the Access Resolver's channel rule), a person by @handle, and
// in:@person as the asker's own DM. A private channel the asker isn't in
// reads exactly as one that isn't there: the same words and the same note,
// through the route too. A spec written back for someone else names only
// what they may see.
//
// Run: go test -tags=integration ./tests/integration/ -run TestQueryNamesResolveAsTheAsker -v

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/google/uuid"

	accessBusiness "github.com/akashc777/OneCamp/business/Access"
	_ "github.com/akashc777/OneCamp/business/Conversation" // registers the channel rule, as the server links it
	querySpec "github.com/akashc777/OneCamp/business/QuerySpec"
	searchQuery "github.com/akashc777/OneCamp/business/SearchQuery"
	queryController "github.com/akashc777/OneCamp/controllers/SearchQuery"
	userDomain "github.com/akashc777/OneCamp/domain/User"
	"github.com/akashc777/OneCamp/helpers"
	"github.com/akashc777/OneCamp/initializers/postgresInit"
	userModels "github.com/akashc777/OneCamp/models/postgres/User"
	"github.com/akashc777/OneCamp/tests/integration"
)

func TestQueryNamesResolveAsTheAsker(t *testing.T) {
	ctx := context.Background()
	t.Setenv("CONVERSATION_ACCESS_SOURCE", "graph")
	env := integration.SetupEnv(t)
	if err := postgresInit.ConnectPostgres(ctx, env.DSN); err != nil {
		t.Fatal(err)
	}
	integration.SetupRedis(t)
	integration.StubMqttClient(t)
	dg := integration.SetupDgraph(t)

	sam, maya := uuid.New(), uuid.New()
	launch, leadership, general := uuid.New(), uuid.New(), uuid.New()
	for name, id := range map[string]uuid.UUID{"sam": sam, "maya": maya} {
		if _, err := env.PG.Exec(`INSERT INTO users (id, email_id, username, display_name, created_at, updated_at)
			VALUES ($1, $2 || '@acme.test', $2, $2, NOW(), NOW())`, id, name); err != nil {
			t.Fatal(err)
		}
	}
	for name, ch := range map[string]struct {
		id      uuid.UUID
		private bool
	}{"launch": {launch, true}, "leadership": {leadership, true}, "general": {general, false}} {
		if _, err := env.PG.Exec(`INSERT INTO channels (id, ch_name, ch_private) VALUES ($1, $2, $3)`, ch.id, name, ch.private); err != nil {
			t.Fatal(err)
		}
	}
	live := "0001-01-01T00:00:00Z"
	dg.Mutate(t, []map[string]any{
		{"uid": "_:sam", "dgraph.type": "User", "user_uuid": sam.String(), "user_name": "sam"},
		{"uid": "_:maya", "dgraph.type": "User", "user_uuid": maya.String(), "user_name": "maya"},
		{"uid": "_:launch", "dgraph.type": "Channel", "ch_uuid": launch.String(), "ch_name": "launch",
			"ch_private": true, "ch_deleted_at": live, "ch_members": []map[string]any{{"uid": "_:sam"}}},
		{"uid": "_:leadership", "dgraph.type": "Channel", "ch_uuid": leadership.String(), "ch_name": "leadership",
			"ch_private": true, "ch_deleted_at": live, "ch_members": []map[string]any{{"uid": "_:maya"}}},
		{"uid": "_:general", "dgraph.type": "Channel", "ch_uuid": general.String(), "ch_name": "general",
			"ch_private": false, "ch_deleted_at": live},
	})

	infoOf := func(id uuid.UUID) userModels.UserInfo {
		t.Helper()
		pg, err := userDomain.GetActiveUserWithAdminFlagByUserUUID(ctx, id)
		if err != nil || pg == nil {
			t.Fatalf("load %s: %v", id, err)
		}
		gr, err := userDomain.GetDgraphUserInfoByUUID(ctx, id.String())
		if err != nil || gr == nil {
			t.Fatalf("load %s's node: %v", id, err)
		}
		return userModels.UserInfo{UserPostgresInfo: *pg, UserDgraphInfo: *gr}
	}
	samInfo, mayaInfo := infoOf(sam), infoOf(maya)
	asker := func(u userModels.UserInfo) searchQuery.Asker {
		return searchQuery.Asker{Subject: accessBusiness.SubjectOf(&u), Email: u.UserPostgresInfo.EmailID}
	}
	scope := querySpec.Scope{Kind: querySpec.ScopeSearch}
	only := func(t *testing.T, spec querySpec.Spec) querySpec.Filter {
		t.Helper()
		if spec.Filter == nil || spec.Filter.Op != "" {
			t.Fatalf("want one condition, got %+v", spec.Filter)
		}
		return *spec.Filter
	}

	t.Run("a channel the asker is in, and a public one, resolve to their ids", func(t *testing.T) {
		for text, want := range map[string]uuid.UUID{"in:#launch": launch, "in:#general": general} {
			spec, issues := searchQuery.Build(ctx, asker(samInfo), text, scope, searchQuery.ModeSearch)
			c := only(t, spec)
			if len(issues) > 0 || c.Field != "in" || c.Operator != querySpec.OperatorIs || len(c.Values) != 1 ||
				c.Values[0].ID != want.String() || c.Values[0].Type != "channel" {
				t.Errorf("%s for sam: %+v %+v", text, c, issues)
			}
		}
	})

	t.Run("a private channel the asker isn't in reads as one that isn't there", func(t *testing.T) {
		hidden, hiddenIssues := searchQuery.Build(ctx, asker(samInfo), "in:#leadership", scope, searchQuery.ModeSearch)
		missing, missingIssues := searchQuery.Build(ctx, asker(samInfo), "in:#nowhere", scope, searchQuery.ModeSearch)
		h, m := only(t, hidden), only(t, missing)
		if h.Field != "text" || h.Values[0].Text != "in:#leadership" || m.Field != "text" {
			t.Errorf("both are searched as words: %+v / %+v", h, m)
		}
		if len(hiddenIssues) != 1 || len(missingIssues) != 1 || hiddenIssues[0].Code != missingIssues[0].Code ||
			strings.Replace(hiddenIssues[0].Message, "#leadership", "#X", 1) != strings.Replace(missingIssues[0].Message, "#nowhere", "#X", 1) {
			t.Errorf("they differ: %+v / %+v", hiddenIssues, missingIssues)
		}
		// Maya is in it: for her it resolves.
		spec, issues := searchQuery.Build(ctx, asker(mayaInfo), "in:#leadership", scope, searchQuery.ModeSearch)
		if c := only(t, spec); len(issues) > 0 || c.Values[0].ID != leadership.String() {
			t.Errorf("for maya: %+v %+v", c, issues)
		}
	})

	t.Run("a person by handle, and the asker's own DM with them", func(t *testing.T) {
		spec, issues := searchQuery.Build(ctx, asker(samInfo), "from:@MAYA", scope, searchQuery.ModeSearch)
		if c := only(t, spec); len(issues) > 0 || c.Operator != querySpec.OperatorIn || c.Values[0].ID != maya.String() {
			t.Errorf("from:@MAYA: %+v %+v", c, issues)
		}
		spec, issues = searchQuery.Build(ctx, asker(samInfo), "in:@maya", scope, searchQuery.ModeSearch)
		if c := only(t, spec); len(issues) > 0 || c.Values[0].ID != maya.String() || c.Values[0].Type != searchQuery.TypeDMWith {
			t.Errorf("in:@maya: %+v %+v", c, issues)
		}
		if _, issues := searchQuery.Build(ctx, asker(samInfo), "from:@nobody", scope, searchQuery.ModeList); searchQuery.Runs(issues) {
			t.Errorf("a handle nobody has, in a list: %+v", issues)
		}
	})

	t.Run("a spec written back names only what its reader may see", func(t *testing.T) {
		spec, _ := searchQuery.Build(ctx, asker(samInfo), "in:#launch from:@maya", scope, searchQuery.ModeSearch)
		if got := searchQuery.Format(ctx, asker(samInfo), spec); got != "in:#launch from:@maya" {
			t.Errorf("for sam: %q", got)
		}
		if got := searchQuery.Format(ctx, asker(mayaInfo), spec); got != `in:"(hidden)" from:@maya` {
			t.Errorf("for maya, who isn't in #launch: %q", got)
		}
	})

	t.Run("through the route, the same answer for hidden and missing", func(t *testing.T) {
		post := func(text string) map[string]any {
			t.Helper()
			body, _ := json.Marshal(map[string]any{"text": text})
			r := httptest.NewRequest(http.MethodPost, "/query/parse", strings.NewReader(string(body)))
			r = r.WithContext(context.WithValue(r.Context(), helpers.UserInfoContextKey, samInfo))
			rec := httptest.NewRecorder()
			queryController.ParseQuery(rec, r)
			if rec.Code != http.StatusOK {
				t.Fatalf("%s: %d %s", text, rec.Code, rec.Body)
			}
			var out struct {
				Data map[string]any `json:"data"`
			}
			if err := json.Unmarshal(rec.Body.Bytes(), &out); err != nil {
				t.Fatal(err)
			}
			return out.Data
		}
		// The same length, so places and tokens line up and only the name differs.
		hidden, missing := post("in:#leadership"), post("in:#readership")
		if hidden["runs"] != true || missing["runs"] != true {
			t.Errorf("a search runs as words: %v / %v", hidden["runs"], missing["runs"])
		}
		strip := func(d map[string]any, name string) string {
			raw, _ := json.Marshal(d)
			return strings.ReplaceAll(string(raw), name, "X")
		}
		if strip(hidden, "leadership") != strip(missing, "readership") {
			t.Errorf("the answers differ beyond the name:\n  %s\n  %s", strip(hidden, "leadership"), strip(missing, "readership"))
		}
	})
}
