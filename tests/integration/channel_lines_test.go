//go:build integration
// +build integration

package integration_test

// A channel's history says what happened to it (chat B28), against real
// Postgres 12, Redis and Dgraph, through the Resource Events bus: a join and a
// rename in a small channel are lines at their time, as their actor; a join in
// a public channel of more than 20 people isn't, until the workspace turns
// chat.lines.joins_large on; a line is no row in posts, so nothing counts it.
//
// Run: go test -tags=integration ./tests/integration/ -run 'TestAChannelsHistorySaysWhatHappenedToIt' -v

import (
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"sort"
	"strings"
	"testing"
	"time"

	channelAdapter "github.com/akashc777/OneCamp/adapter/Channel"
	channelBusiness "github.com/akashc777/OneCamp/business/Channel"
	_ "github.com/akashc777/OneCamp/business/Conversation" // the chat.lines sink
	"github.com/akashc777/OneCamp/helpers"
	"github.com/akashc777/OneCamp/initializers/postgresInit"
	dgraphStruct "github.com/akashc777/OneCamp/models/dgraph"
	"github.com/akashc777/OneCamp/tests/integration"
	"github.com/google/uuid"
)

func TestAChannelsHistorySaysWhatHappenedToIt(t *testing.T) {
	// A sink logs what it couldn't write and returns: the test keeps those
	// lines, so a line that's missing says why (errorLines, doc_read_gone_quiet_test.go).
	logged := &errorLines{}
	was := helpers.Logger
	helpers.Logger = slog.New(logged)
	t.Cleanup(func() { helpers.Logger = was })

	ctx := context.Background()
	env := integration.SetupEnv(t)
	if err := postgresInit.ConnectPostgres(ctx, env.DSN); err != nil {
		t.Fatal(err)
	}
	integration.SetupRedis(t)
	integration.StubMqttClient(t)
	dg := integration.SetupDgraph(t)
	t.Cleanup(func() { helpers.SettleBackground(10 * time.Second) })

	const live = "0001-01-01T00:00:00Z"
	maya, jonas := uuid.New(), uuid.New()
	small, big := uuid.New(), uuid.New()
	nodes := []map[string]any{
		{"uid": "_:maya", "dgraph.type": "User", "user_uuid": maya.String(), "user_name": "Maya", "user_deleted_at": live},
		{"uid": "_:jonas", "dgraph.type": "User", "user_uuid": jonas.String(), "user_name": "Jonas", "user_deleted_at": live},
	}
	crowd := []map[string]any{{"uid": "_:maya"}, {"uid": "_:jonas"}}
	for i := 0; i < 23; i++ {
		nodes = append(nodes, map[string]any{"uid": fmt.Sprintf("_:p%d", i), "dgraph.type": "User", "user_uuid": uuid.NewString(), "user_name": fmt.Sprintf("p%d", i), "user_deleted_at": live})
		crowd = append(crowd, map[string]any{"uid": fmt.Sprintf("_:p%d", i)})
	}
	nodes = append(nodes,
		map[string]any{"dgraph.type": "Channel", "ch_uuid": small.String(), "ch_name": "q4-launch", "ch_private": false, "ch_deleted_at": live,
			"ch_members": []map[string]any{{"uid": "_:maya"}, {"uid": "_:jonas"}}},
		map[string]any{"dgraph.type": "Channel", "ch_uuid": big.String(), "ch_name": "general", "ch_private": false, "ch_deleted_at": live,
			"ch_members": crowd},
	)
	dg.Mutate(t, nodes)

	lines := func(channel uuid.UUID) []string {
		t.Helper()
		helpers.SettleBackground(10 * time.Second)
		var got struct {
			C []struct {
				Posts []struct {
					Text   string `json:"post_text"`
					System string `json:"post_system"`
					By     *struct {
						UUID string `json:"user_uuid"`
					} `json:"post_by"`
					At *time.Time `json:"post_created_at"`
				} `json:"ch_posts"`
			} `json:"c"`
		}
		q := fmt.Sprintf(`{ c(func: eq(ch_uuid, %q)) { ch_posts @filter(eq(post_kind, "system")) { post_text post_system post_created_at post_by { user_uuid } } } }`, channel)
		if err := json.Unmarshal(dg.Query(t, q), &got); err != nil {
			t.Fatal(err)
		}
		var out []string
		for _, c := range got.C {
			for _, p := range c.Posts {
				var l struct {
					Kind  string `json:"kind"`
					Actor string `json:"actor"`
				}
				_ = json.Unmarshal([]byte(p.System), &l)
				if p.By == nil || p.By.UUID != l.Actor {
					t.Errorf("a line written as %v, its actor %s", p.By, l.Actor)
				}
				out = append(out, l.Kind+" "+p.Text)
			}
		}
		sort.Strings(out)
		return out
	}

	// Jonas joins the small channel, Maya renames it.
	channelBusiness.EmitMembership(ctx, true, small.String(), jonas.String(), jonas.String())
	before := &dgraphStruct.DgraphChannel{Uuid: small.String(), Name: "q4-launch"}
	zero := time.Time{}
	before.DeletedAt = &zero
	channelBusiness.EmitChannelChanges(ctx, maya.String(), before, &channelAdapter.UpdateChannelInfo{ChannelName: "launch"}, channelBusiness.PrivacyChange(0))
	want := []string{
		"channel.joined <p>Jonas joined.</p>",
		"channel.renamed <p>Maya renamed the channel to #launch (was #q4-launch).</p>",
	}
	if got := lines(small); fmt.Sprint(got) != fmt.Sprint(want) {
		t.Fatalf("the small channel's lines: %q, want %q (logged: %q)", got, want, logged.take())
	}

	// A join in a public channel of 25 isn't a line.
	channelBusiness.EmitMembership(ctx, true, big.String(), jonas.String(), jonas.String())
	if got := lines(big); len(got) != 0 {
		t.Fatalf("a join in a channel of 25 was a line: %q", got)
	}

	// The writers' errors, not the stubbed broker's ("not Connected").
	var failed []string
	for _, e := range logged.take() {
		if !strings.Contains(e, "mqtt") {
			failed = append(failed, e)
		}
	}
	if len(failed) != 0 {
		t.Errorf("the lines logged errors: %q", failed)
	}

	// A line is no post row: nothing counts it unread.
	var rows int
	if err := env.PG.QueryRow(`SELECT COUNT(*) FROM posts WHERE post_channel IN ($1, $2)`, small, big).Scan(&rows); err != nil || rows != 0 {
		t.Fatalf("%d post rows for the lines (%v)", rows, err)
	}
}
