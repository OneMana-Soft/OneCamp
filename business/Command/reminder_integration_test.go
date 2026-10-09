//go:build integration

package business

// /remind into a channel, against Postgres, Dgraph and the real send rules: a
// reminder is set only where its owner may post, "#here" included (that id
// comes with the request); it posts when it fires; and once its owner can't
// post there any more, it neither posts nor fires again.
// Run: go test -tags=integration ./business/Command/ -run TestChannelReminders -v

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	schedulerBusiness "github.com/akashc777/OneCamp/business/Scheduler"
	jobDomain "github.com/akashc777/OneCamp/domain/ScheduledJob"
	userDomain "github.com/akashc777/OneCamp/domain/User"
	"github.com/akashc777/OneCamp/initializers/dgraphInit"
	"github.com/akashc777/OneCamp/initializers/opensearchInit"
	"github.com/akashc777/OneCamp/initializers/postgresInit"
	jobModel "github.com/akashc777/OneCamp/models/postgres/ScheduledJob"
	userModels "github.com/akashc777/OneCamp/models/postgres/User"
	"github.com/akashc777/OneCamp/tests/integration"
	"github.com/dgraph-io/dgo/v230/protos/api"
	"github.com/google/uuid"
	"github.com/opensearch-project/opensearch-go/v4"
	"github.com/opensearch-project/opensearch-go/v4/opensearchapi"
)

func TestChannelRemindersFollowThePostRules(t *testing.T) {
	ctx := context.Background()
	env := integration.SetupEnv(t)
	if err := postgresInit.ConnectPostgres(ctx, env.DSN); err != nil {
		t.Fatal(err)
	}
	integration.StubMqttClient(t)
	dg := integration.SetupDgraph(t)

	// A post is indexed in the background; give it somewhere to go.
	search := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		fmt.Fprint(w, `{"took":1,"errors":false,"items":[]}`)
	}))
	defer search.Close()
	client, err := opensearchapi.NewClient(opensearchapi.Config{Client: opensearch.Config{Addresses: []string{search.URL}}})
	if err != nil {
		t.Fatal(err)
	}
	opensearchInit.OpenSearchClient = client

	sam := uuid.New()
	if _, err := env.PG.Exec(`INSERT INTO users (id, email_id, username, display_name, created_at, updated_at)
		VALUES ($1, $2, 'sam', 'Sam', NOW(), NOW())`, sam, sam.String()+"@example.test"); err != nil {
		t.Fatal(err)
	}
	general, news, old, secret := uuid.New(), uuid.New(), uuid.New(), uuid.New()
	for name, ch := range map[string]uuid.UUID{"general": general, "news": news, "old": old, "secret": secret} {
		if _, err := env.PG.Exec(`INSERT INTO channels (id, ch_name, ch_private) VALUES ($1, $2, $3)`, ch, name, name == "secret"); err != nil {
			t.Fatal(err)
		}
	}
	live := "0001-01-01T00:00:00Z"
	member := []map[string]any{{"uid": "_:sam"}}
	uids := dg.Mutate(t, []map[string]any{
		{"uid": "_:sam", "dgraph.type": "User", "user_uuid": sam.String(), "user_name": "sam"},
		{"uid": "_:general", "dgraph.type": "Channel", "ch_uuid": general.String(), "ch_name": "general",
			"ch_private": false, "ch_deleted_at": live, "ch_members": member},
		{"uid": "_:news", "dgraph.type": "Channel", "ch_uuid": news.String(), "ch_name": "news",
			"ch_private": false, "ch_deleted_at": live, "ch_post_policy": "admins_only", "ch_members": member},
		{"uid": "_:old", "dgraph.type": "Channel", "ch_uuid": old.String(), "ch_name": "old",
			"ch_private": false, "ch_deleted_at": "2026-01-01T00:00:00Z", "ch_members": member},
		{"uid": "_:secret", "dgraph.type": "Channel", "ch_uuid": secret.String(), "ch_name": "secret",
			"ch_private": true, "ch_deleted_at": live},
	})

	pg, err := userDomain.GetActiveUserWithAdminFlagByUserUUID(ctx, sam)
	if err != nil || pg == nil {
		t.Fatalf("load sam: %v", err)
	}
	gr, err := userDomain.GetDgraphUserInfoByUUID(ctx, sam.String())
	if err != nil || gr == nil {
		t.Fatalf("load sam's node: %v", err)
	}
	samInfo := userModels.UserInfo{UserPostgresInfo: *pg, UserDgraphInfo: *gr}

	remind := func(channel uuid.UUID, text string) string {
		t.Helper()
		resp, err := handleRemind(ctx, CommandContext{User: samInfo, Command: "remind", Text: text, ChannelID: &channel, Timezone: "UTC"})
		if err != nil || resp == nil {
			t.Fatalf("remind: %v", err)
		}
		return resp.Text
	}
	jobs := func(status string) []*jobModel.ScheduledJob {
		t.Helper()
		js, err := jobDomain.ListByUser(ctx, sam, jobModel.JobTypeReminder, []string{status}, 25)
		if err != nil {
			t.Fatal(err)
		}
		return js
	}
	posts := func(ch uuid.UUID) (n int) {
		t.Helper()
		if err := env.PG.QueryRow(`SELECT count(*) FROM posts WHERE post_channel = $1`, ch).Scan(&n); err != nil {
			t.Fatal(err)
		}
		return n
	}

	// "#here" is the channel the command came from, which is anything the
	// request names: it is checked like a channel named outright.
	for _, c := range []struct {
		name string
		ch   uuid.UUID
	}{
		{"an announcement channel they don't run", news},
		{"an archived channel", old},
		{"a private channel they're not in", secret},
		{"a channel that doesn't exist", uuid.New()},
	} {
		if got := remind(c.ch, "#here ship it every day"); !strings.Contains(got, "Couldn't find a channel") {
			t.Errorf("a reminder in %s: %q", c.name, got)
		}
	}
	if got := remind(general, "#news ship it every day"); !strings.Contains(got, "Couldn't find a channel") {
		t.Errorf("a reminder in an announcement channel, by name: %q", got)
	}
	if n := len(jobs(jobModel.StatusPending)); n != 0 {
		t.Fatalf("%d reminders were set where they can't post", n)
	}

	// Their own channel: set, and posted when it fires.
	if got := remind(general, "#here ship it every day"); !strings.Contains(got, "Okay") {
		t.Fatalf("a reminder in their own channel: %q", got)
	}
	set := jobs(jobModel.StatusPending)
	if len(set) != 1 {
		t.Fatalf("%d reminders set, want 1", len(set))
	}
	var p reminderPayload
	if err := json.Unmarshal([]byte(set[0].Payload), &p); err != nil || p.TargetID != general.String() {
		t.Fatalf("the reminder is for %q (%v), want %s", p.TargetID, err, general)
	}
	schedulerBusiness.RunClaimed(ctx, set[0])
	if n := posts(general); n != 1 {
		t.Fatalf("the reminder made %d posts, want 1", n)
	}
	if again := jobs(jobModel.StatusPending); len(again) != 1 {
		t.Fatalf("a repeating reminder that posted isn't due again: %d pending", len(again))
	}

	// Sam leaves the channel: the next time, it doesn't post, and it stops.
	raw, _ := json.Marshal(map[string]any{"uid": uids["general"], "ch_members": []map[string]any{{"uid": uids["sam"]}}})
	if _, err := dgraphInit.DgraphClient.NewTxn().Mutate(ctx, &api.Mutation{DeleteJson: raw, CommitNow: true}); err != nil {
		t.Fatal(err)
	}
	due := jobs(jobModel.StatusPending)
	if len(due) != 1 {
		t.Fatalf("%d pending, want 1", len(due))
	}
	schedulerBusiness.RunClaimed(ctx, due[0])
	if n := posts(general); n != 1 {
		t.Fatalf("a reminder posted in a channel its owner left: %d posts", n)
	}
	if n := len(jobs(jobModel.StatusPending)); n != 0 {
		t.Fatalf("a refused reminder is still due: %d pending", n)
	}
	stopped := jobs(jobModel.StatusFailed)
	if len(stopped) != 1 || stopped[0].LastError == nil || !strings.HasPrefix(*stopped[0].LastError, "Not posted") {
		t.Fatalf("the refused reminder wasn't stopped with its reason: %+v", stopped)
	}
}
