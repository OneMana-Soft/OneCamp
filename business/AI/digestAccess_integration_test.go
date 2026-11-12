//go:build integration

package business

// NOTIF-H5: the daily memory-digest email quoted memory items from a private
// channel, a project or a DM its reader could no longer read: the digest
// gathered each person's items by owner and never asked, at send time,
// whether they still belonged where each item lives. A member removed from a
// private channel kept getting its text by email. Now the digest (the daily
// one and the test one), like the home's attention list, keeps only what its
// reader can read at send time, through the one check memory items have
// (memoryItemVisibleTo, in its batch form).
//
// Run: go test -tags=integration ./business/AI/ -run TestTheDigestQuotesOnlyWhatItsReaderCanRead -v

import (
	"context"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/google/uuid"

	userDomain "github.com/akashc777/OneCamp/domain/User"
	"github.com/akashc777/OneCamp/initializers/postgresInit"
	userModels "github.com/akashc777/OneCamp/models/postgres/User"
	"github.com/akashc777/OneCamp/tests/integration"
)

func TestTheDigestQuotesOnlyWhatItsReaderCanRead(t *testing.T) {
	ctx := context.Background()
	env := integration.SetupEnv(t)
	if err := postgresInit.ConnectPostgres(ctx, env.DSN); err != nil {
		t.Fatal(err)
	}
	dg := integration.SetupDgraph(t)
	integration.StubMqttClient(t)

	member, peer, ann, bob := uuid.New(), uuid.New(), uuid.New(), uuid.New()
	for name, id := range map[string]uuid.UUID{"member": member, "peer": peer, "ann": ann, "bob": bob} {
		if _, err := env.PG.Exec(`INSERT INTO users (id, email_id, username, display_name, created_at, updated_at)
			VALUES ($1, $2, $3, $4, NOW(), NOW())`, id, id.String()+"@example.test", name, name); err != nil {
			t.Fatal(err)
		}
	}
	// The member wants a digest every day.
	if _, err := env.PG.Exec(`INSERT INTO users_notification_preferences (user_id, email_digest_frequency, unsubscribe_token)
		VALUES ($1, 'daily', $2)`, member, uuid.NewString()); err != nil {
		t.Fatal(err)
	}

	// Where the member is now: #general and the website project, and a DM
	// with peer. They were removed from #private and left the launch
	// project; ann and bob's DM was never theirs.
	general, private := uuid.New(), uuid.New()
	website, launch := uuid.New(), uuid.New()
	ourDM := member.String() + " " + peer.String()
	theirDM := ann.String() + " " + bob.String()
	zero := "0001-01-01T00:00:00Z"
	dg.Mutate(t, []map[string]any{{"uid": "_:member", "dgraph.type": "User", "user_uuid": member.String(), "user_name": "member",
		"user_channels": []map[string]any{{"uid": "_:general", "dgraph.type": "Channel", "ch_uuid": general.String(), "ch_name": "general", "ch_deleted_at": zero}},
		"user_projects": []map[string]any{{"uid": "_:website", "dgraph.type": "Project", "project_uuid": website.String(), "project_name": "website", "project_deleted_at": zero}},
		"user_dms": []map[string]any{{"uid": "_:dm", "dgraph.type": "Dm", "dm_grouping_id": ourDM,
			"dm_participants": []map[string]any{{"uid": "_:member"}, {"uid": "_:peer", "dgraph.type": "User", "user_uuid": peer.String(), "user_name": "peer"}}}},
	}})

	// The member's overdue commitments, one in each place, by what it says.
	write := func(content, column, scope string) {
		t.Helper()
		if _, err := env.PG.Exec(`INSERT INTO workspace_memory_items
			(kind, content, status, owner_user_id, `+column+`, due_at, dedup_hash)
			VALUES ('commitment', $1, 'open', $2, $3, NOW() - interval '1 day', $4)`, content, member, scope, uuid.NewString()); err != nil {
			t.Fatal(err)
		}
	}
	write("Send the general numbers", "channel_uuid", general.String())
	write("Send the private salary sheet", "channel_uuid", private.String())
	write("Review the website copy", "project_uuid", website.String())
	write("Ship the launch plan", "project_uuid", launch.String())
	write("Call peer about the venue", "chat_grp_id", ourDM)
	write("Book ann and bob's flights", "chat_grp_id", theirDM)
	readable := []string{"Call peer about the venue", "Review the website copy", "Send the general numbers"}
	unreadable := []string{"Book ann and bob's flights", "Send the private salary sheet", "Ship the launch plan"}

	// says lists which of the six commitments a text quotes.
	says := func(text string) []string {
		var got []string
		for _, c := range append(slices.Clone(readable), unreadable...) {
			if strings.Contains(text, c) {
				got = append(got, c)
			}
		}
		slices.Sort(got)
		return got
	}

	t.Run("the daily digest quotes only what its reader can read now", func(t *testing.T) {
		var mu sync.Mutex
		sent := map[string]string{}
		was := sendMemoryDigest
		sendMemoryDigest = func(to, subject, body string) { mu.Lock(); sent[to] += subject + "\n" + body; mu.Unlock() }
		t.Cleanup(func() { sendMemoryDigest = was })

		runDigest(ctx, time.Now())

		mu.Lock()
		defer mu.Unlock()
		if got := says(sent[member.String()]); !slices.Equal(got, readable) {
			t.Errorf("the member's digest quotes %q; want only %q: they were removed from #private, left the launch project and were never in ann and bob's DM", got, readable)
		}
		if !strings.Contains(sent[member.String()], "You have 3 open items") {
			t.Errorf("the member's digest counts what it can't show: %q", firstLine(sent[member.String()]))
		}
	})

	t.Run("the test digest quotes only what its reader can read now", func(t *testing.T) {
		if _, err := env.PG.Exec(`UPDATE ai_settings SET enabled = true, memory_layer_enabled = true`); err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() { _, _ = env.PG.Exec(`UPDATE ai_settings SET enabled = false, memory_layer_enabled = false`) })
		var body string
		was := sendMemoryDigestNow
		sendMemoryDigestNow = func(_ context.Context, _ string, subject, b string) int { body = subject + "\n" + b; return 1 }
		t.Cleanup(func() { sendMemoryDigestNow = was })

		if err := SendTestMemoryDigest(ctx, member.String()); err != nil {
			t.Fatal(err)
		}
		if got := says(body); !slices.Equal(got, readable) {
			t.Errorf("the member's test digest quotes %q; want only %q", got, readable)
		}
	})

	t.Run("the home's attention list shows only what its reader can read now", func(t *testing.T) {
		profile, err := userDomain.GetActiveDgraphUserInfoByUUID(ctx, member.String())
		if err != nil || profile == nil {
			t.Fatalf("the member's profile: %v", err)
		}
		me := &userModels.UserInfo{UserDgraphInfo: *profile}
		me.UserPostgresInfo.Id = member
		var titles []string
		for _, it := range gatherMemoryAttentionItems(ctx, me) {
			titles = append(titles, it.Title)
		}
		if got := says(strings.Join(titles, "\n")); !slices.Equal(got, readable) {
			t.Errorf("the member's attention list shows %q; want only %q", got, readable)
		}
	})
}

// firstLine is a text's first line.
func firstLine(s string) string {
	line, _, _ := strings.Cut(s, "\n")
	return line
}
