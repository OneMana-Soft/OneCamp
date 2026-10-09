//go:build integration

package business

// A Slack import leaves the channels the way the team had them: Slack's
// #general goes into the workspace's own while that holds only its welcome
// post, channels archived in Slack are archived once their history is in, and
// the importing admin leaves the private channels they weren't in, handing
// each to whoever made it in Slack.
//
// Run: go test -tags=integration ./business/SlackImport/ -run TestASlackImportsChannelsAreTheTeams -v

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/google/uuid"
	"github.com/opensearch-project/opensearch-go/v4"
	"github.com/opensearch-project/opensearch-go/v4/opensearchapi"

	channelAdapter "github.com/akashc777/OneCamp/adapter/Channel"
	channelBusiness "github.com/akashc777/OneCamp/business/Channel"
	userDomain "github.com/akashc777/OneCamp/domain/User"
	"github.com/akashc777/OneCamp/helpers"
	"github.com/akashc777/OneCamp/initializers/dgraphInit"
	"github.com/akashc777/OneCamp/initializers/opensearchInit"
	"github.com/akashc777/OneCamp/initializers/postgresInit"
	guestModel "github.com/akashc777/OneCamp/models/postgres/Guest"
	importModels "github.com/akashc777/OneCamp/models/postgres/SlackImport"
	userModels "github.com/akashc777/OneCamp/models/postgres/User"
	"github.com/akashc777/OneCamp/tests/integration"
)

func TestASlackImportsChannelsAreTheTeams(t *testing.T) {
	ctx := context.WithValue(context.Background(), helpers.BulkImportContextKey, true)
	env := integration.SetupEnv(t)
	if err := postgresInit.ConnectPostgres(ctx, env.DSN); err != nil {
		t.Fatal(err)
	}
	integration.StubMqttClient(t)
	dg := integration.SetupDgraph(t)
	search := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		fmt.Fprint(w, `{"took":1,"errors":false,"items":[]}`)
	}))
	defer search.Close()
	client, err := opensearchapi.NewClient(opensearchapi.Config{Client: opensearch.Config{Addresses: []string{search.URL}}})
	if err != nil {
		t.Fatal(err)
	}
	opensearchInit.OpenSearchClient = client

	admin, ada, bo := uuid.New(), uuid.New(), uuid.New()
	for id, email := range map[uuid.UUID]string{admin: "admin@acme.test", ada: "ada@acme.test", bo: "bo@acme.test"} {
		if _, err := env.PG.Exec(`INSERT INTO users (id, email_id, username) VALUES ($1, $2, $3)`, id, email, email[:len(email)-10]); err != nil {
			t.Fatal(err)
		}
	}
	dg.Mutate(t, []map[string]any{
		{"uid": "_:admin", "dgraph.type": "User", "user_uuid": admin.String(), "user_name": "admin"},
		{"uid": "_:ada", "dgraph.type": "User", "user_uuid": ada.String(), "user_name": "ada"},
		{"uid": "_:bo", "dgraph.type": "User", "user_uuid": bo.String(), "user_name": "bo"},
	})
	pg, err := userDomain.GetActiveUserWithAdminFlagByUserUUID(ctx, admin)
	if err != nil || pg == nil {
		t.Fatalf("load admin: %v", err)
	}
	node, err := userDomain.GetDgraphUserInfoByUUID(ctx, admin.String())
	if err != nil || node == nil {
		t.Fatalf("load admin's node: %v", err)
	}
	adminInfo := &userModels.UserInfo{UserPostgresInfo: *pg, UserDgraphInfo: *node}

	// The workspace's own #general, with its welcome post.
	cerr, general := channelBusiness.CreateChannel(ctx, &channelAdapter.InputCreateChannel{ChannelName: "general"}, adminInfo)
	if cerr != nil {
		t.Fatal(cerr)
	}
	if _, err := env.PG.Exec(`INSERT INTO posts (id, post_channel, created_by) VALUES ($1, $2, $3)`, uuid.New(), general, admin); err != nil {
		t.Fatal(err)
	}

	job := uuid.New()
	if _, err := env.PG.Exec(`INSERT INTO import_jobs (id, provider, source_workspace_name, source, status, triggered_by)
		VALUES ($1, 'slack', 'Acme', 'export_zip', 'running', $2)`, job, admin); err != nil {
		t.Fatal(err)
	}
	for slackID, id := range map[string]uuid.UUID{"UADMIN": admin, "UADA": ada, "UBO": bo} {
		if err := importModels.UpsertIdMappingWithOwnership(ctx, job, importModels.EntityUser, slackID, id, nil, nil, false); err != nil {
			t.Fatal(err)
		}
	}

	public := []SlackChannel{
		{ID: "CGEN", Name: "general", IsGeneral: true, Members: []string{"UADMIN", "UADA", "UBO"}},
		{ID: "COLD", Name: "old-launch", IsArchived: true, Creator: "UADA", Members: []string{"UADA"}},
	}
	private := []SlackChannel{
		{ID: "GLEAD", Name: "leadership", Creator: "UBO", Members: []string{"UADA", "UBO"}},
		{ID: "GMINE", Name: "admins", Creator: "UADMIN", Members: []string{"UADMIN", "UADA"}},
	}
	stats, err := resolveChannels(ctx, job, "Acme", public, private, adminInfo, "")
	if err != nil {
		t.Fatal(err)
	}

	mapped := func(slackID string) (uuid.UUID, bool) {
		t.Helper()
		var id uuid.UUID
		var owned bool
		if err := env.PG.QueryRow(`SELECT onecamp_uuid, created_by_this_import FROM import_id_map
			WHERE import_id = $1 AND entity_type = 'channel' AND source_id = $2`, job, slackID).Scan(&id, &owned); err != nil {
			t.Fatalf("%s: %v", slackID, err)
		}
		return id, owned
	}
	count := func(channel uuid.UUID, edge string, user uuid.UUID) int {
		t.Helper()
		q := fmt.Sprintf(`{ q(func: eq(ch_uuid, %q)) { n: count(%s @filter(eq(user_uuid, %q))) } }`, channel.String(), edge, user.String())
		resp, err := dgraphInit.DgraphClient.NewReadOnlyTxn().Query(context.Background(), q)
		if err != nil {
			t.Fatal(err)
		}
		var out struct {
			Q []struct {
				N int `json:"n"`
			} `json:"q"`
		}
		if err := json.Unmarshal(resp.Json, &out); err != nil || len(out.Q) != 1 {
			t.Fatalf("%s: %s %v", q, resp.Json, err)
		}
		return out.Q[0].N
	}

	// 1. Slack's #general went into the workspace's own, not beside it, and
	// a rollback won't take the channel.
	if id, owned := mapped("CGEN"); id != general || owned {
		t.Errorf("Slack's #general: %s (owned %v), want the workspace's %s", id, owned, general)
	}
	if stats.IntoGeneral != "general" {
		t.Errorf("stats: %+v", stats)
	}
	var beside int
	_ = env.PG.QueryRow(`SELECT count(*) FROM channels WHERE ch_name LIKE 'general-from-slack%'`).Scan(&beside)
	if beside != 0 {
		t.Errorf("%d #general-from-slack channels", beside)
	}
	if count(general, "ch_members", ada) != 1 || count(general, "ch_members", bo) != 1 {
		t.Error("#general's Slack members joined it")
	}

	// 2. The admin left the private channel they weren't in, handing it to
	// whoever made it in Slack; they stayed in the one they were in.
	lead, owned := mapped("GLEAD")
	if !owned {
		t.Fatal("the private channel was made by the import")
	}
	if count(lead, "ch_members", admin) != 0 || count(lead, "ch_moderators", admin) != 0 {
		t.Error("the admin is still in a private channel they weren't in")
	}
	if count(lead, "ch_moderators", bo) != 1 || count(lead, "ch_members", ada) != 1 {
		t.Error("the private channel went to its Slack creator, with its members")
	}
	mine, _ := mapped("GMINE")
	if count(mine, "ch_members", admin) != 1 || count(mine, "ch_moderators", admin) != 1 {
		t.Error("the admin left a private channel they were in")
	}

	// 3. Archived in Slack, archived here once the history is in.
	old, _ := mapped("COLD")
	archived := func(id uuid.UUID) bool {
		t.Helper()
		var yes bool
		if err := env.PG.QueryRow(`SELECT deleted_at IS NOT NULL FROM channels WHERE id = $1`, id).Scan(&yes); err != nil {
			t.Fatal(err)
		}
		return yes
	}
	if archived(old) {
		t.Fatal("archived before its history came in")
	}
	archiveArchivedChannels(ctx, job)
	if !archived(old) || archived(general) || archived(lead) {
		t.Errorf("after the archive pass: old %v, general %v, leadership %v", archived(old), archived(general), archived(lead))
	}

	// 4. A #general shared with guests isn't poured into: its guests would
	// see the team's whole Slack history.
	grant, err := guestModel.CreateGrant(ctx, []byte("hash-of-a-link"), guestModel.ResourceChannel, general.String(), guestModel.CapabilityView, admin, nil)
	if err != nil {
		t.Fatal(err)
	}
	if _, ok := seededGeneral(ctx); ok || !generalSharedWithGuests(ctx) {
		t.Error("a #general with a guest link would be folded into")
	}
	if err := guestModel.Revoke(ctx, grant); err != nil {
		t.Fatal(err)
	}
	if _, ok := seededGeneral(ctx); !ok {
		t.Error("its link turned off, #general can take Slack's again")
	}

	// 5. A #general the team has written in is theirs: Slack's would go
	// beside it.
	if _, err := env.PG.Exec(`INSERT INTO posts (id, post_channel, created_by) VALUES ($1, $2, $3)`, uuid.New(), general, ada); err != nil {
		t.Fatal(err)
	}
	if _, ok := seededGeneral(ctx); ok {
		t.Error("a #general with the team's posts in it would be folded into")
	}
}
