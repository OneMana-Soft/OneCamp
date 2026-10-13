//go:build integration
// +build integration

package integration_test

// The Inbox badge counts what the Inbox shows that is new, and only the Inbox
// clears it, against a real Postgres and Dgraph.
//
// The badge counted the stored mentions, comments and reactions since the
// last look with none of the feed's filters: a mention in a private channel
// the reader isn't in (the @ list offers everyone), in a message since
// deleted, or a reaction to a deleted message raised it over an Inbox with
// nothing new (TN-BUG-5). And every read of the feed marked it seen, Home's
// included, so opening the app cleared the badge with replies and reactions
// still unseen (TN-BUG-11). The badge now counts the feed itself, and only
// the explicit "seen" call moves the last look.
//
// Run: go test -tags=integration ./tests/integration/ -run TestTheInboxBadgeCountsWhatTheInboxShows -v

import (
	"context"
	"testing"
	"time"

	"github.com/google/uuid"

	activityBusiness "github.com/akashc777/OneCamp/business/Activity"
	_ "github.com/akashc777/OneCamp/business/Board" // hands the feed the board read rule
	_ "github.com/akashc777/OneCamp/business/Doc"   // hands the feed the doc read rule
	"github.com/akashc777/OneCamp/initializers/postgresInit"
	"github.com/akashc777/OneCamp/tests/integration"
)

func TestTheInboxBadgeCountsWhatTheInboxShows(t *testing.T) {
	ctx := context.Background()
	env := integration.SetupEnv(t)
	if err := postgresInit.ConnectPostgres(ctx, env.DSN); err != nil {
		t.Fatal(err)
	}
	integration.StubMqttClient(t)
	dg := integration.SetupDgraph(t)

	people := map[string]uuid.UUID{"reader": uuid.New(), "maya": uuid.New(), "jonas": uuid.New()}
	var nodes []map[string]any
	for name, id := range people {
		if _, err := env.PG.Exec(`INSERT INTO users (id, email_id, username, display_name, created_at, updated_at)
			VALUES ($1, $2, $3, $4, NOW(), NOW())`, id, id.String()+"@example.test", name, name); err != nil {
			t.Fatal(err)
		}
		nodes = append(nodes, map[string]any{"uid": "_:" + name, "dgraph.type": "User", "user_uuid": id.String(), "user_name": name})
	}
	reader := people["reader"]
	// The reader last looked an hour ago.
	if _, err := env.PG.Exec(`INSERT INTO last_seen_activity (user_id, user_last_seen) VALUES ($1, NOW() - interval '1 hour')`, reader); err != nil {
		t.Fatal(err)
	}

	const live = "0001-01-01T00:00:00Z"
	now := time.Now().UTC()
	at := func(ago time.Duration) string { return now.Add(-ago).Format(time.RFC3339Nano) }
	nodes = append(nodes,
		map[string]any{"uid": "_:launch", "dgraph.type": "Channel", "ch_uuid": uuid.NewString(), "ch_name": "launch",
			"ch_private": false, "ch_deleted_at": live, "ch_members": []map[string]any{{"uid": "_:reader"}, {"uid": "_:maya"}, {"uid": "_:jonas"}}},
		map[string]any{"uid": "_:leadership", "dgraph.type": "Channel", "ch_uuid": uuid.NewString(), "ch_name": "leadership",
			"ch_private": true, "ch_deleted_at": live, "ch_members": []map[string]any{{"uid": "_:maya"}}},
	)
	// post is a message in a channel, by someone, deleted or not.
	post := func(key, channel, by, text string, ago time.Duration, deleted bool) {
		deletedAt := live
		if deleted {
			deletedAt = at(ago - time.Second)
		}
		nodes = append(nodes, map[string]any{"uid": "_:" + key, "dgraph.type": "Post", "post_uuid": uuid.NewString(),
			"post_text": text, "post_by": map[string]any{"uid": "_:" + by}, "post_channel": map[string]any{"uid": "_:" + channel},
			"post_created_at": at(ago), "post_deleted_at": deletedAt})
	}
	// mention names the reader in a post, as CreatePost stores one.
	mention := func(key, postKey string, ago time.Duration) {
		nodes = append(nodes, map[string]any{"uid": "_:" + key, "dgraph.type": "Mention",
			"mention_users": []map[string]any{{"uid": "_:reader"}}, "mention_post": map[string]any{"uid": "_:" + postKey},
			"mention_created_at": at(ago)})
	}
	// reaction is someone's emoji on the reader's post.
	reaction := func(key, postKey, by string, ago time.Duration) {
		nodes = append(nodes, map[string]any{"uid": "_:" + key, "dgraph.type": "Reaction", "reaction_emoji_id": "tada",
			"reaction_added_by": map[string]any{"uid": "_:" + by}, "reaction_on_content_added_by": map[string]any{"uid": "_:reader"},
			"reaction_added_at": at(ago)})
	}

	// Since the last look: a mention the feed shows, and three it doesn't.
	post("p-shown", "launch", "maya", "@reader can you check the numbers?", 50*time.Minute, false)
	mention("m-shown", "p-shown", 50*time.Minute)
	post("p-private", "leadership", "maya", "@reader the reorg", 40*time.Minute, false)
	mention("m-private", "p-private", 40*time.Minute)
	post("p-deleted", "launch", "maya", "@reader wrong thread", 30*time.Minute, true)
	mention("m-deleted", "p-deleted", 30*time.Minute)
	// A reaction to the reader's message the feed shows, and one to a
	// message they deleted.
	post("p-mine", "launch", "reader", "shipped", 25*time.Minute, false)
	reaction("r-shown", "p-mine", "maya", 20*time.Minute)
	post("p-mine-gone", "launch", "reader", "typo", 15*time.Minute, true)
	reaction("r-deleted", "p-mine-gone", "jonas", 10*time.Minute)
	// Before the last look: seen already.
	post("p-old", "launch", "maya", "@reader last week's numbers", 2*time.Hour, false)
	mention("m-old", "p-old", 2*time.Hour)
	uids := dg.Mutate(t, nodes)
	dg.Mutate(t, []map[string]any{
		{"uid": uids["p-mine"], "post_reactions": []map[string]any{{"uid": uids["r-shown"]}}},
		{"uid": uids["p-mine-gone"], "post_reactions": []map[string]any{{"uid": uids["r-deleted"]}}},
	})

	badge := func(t *testing.T) uint64 {
		t.Helper()
		n, err := activityBusiness.UnreadActivityCount(ctx, reader, uids["reader"])
		if err != nil {
			t.Fatalf("counting the badge: %v", err)
		}
		return n
	}
	// newInFeed is how many of the feed's items are newer than the last look.
	newInFeed := func(t *testing.T) int {
		t.Helper()
		var seen time.Time
		if err := env.PG.QueryRow(`SELECT user_last_seen FROM last_seen_activity WHERE user_id = $1`, reader).Scan(&seen); err != nil {
			t.Fatal(err)
		}
		p, err := activityBusiness.GetUnifiedActivity(ctx, reader, uids["reader"], time.Now().Add(time.Minute), 50)
		if err != nil {
			t.Fatalf("reading the feed: %v", err)
		}
		n := 0
		for _, it := range p.Activities {
			if when, err := time.Parse(time.RFC3339Nano, it.Time); err == nil && when.After(seen) {
				n++
			}
		}
		return n
	}

	t.Run("the badge counts what the feed shows, not what it hides", func(t *testing.T) {
		if got := badge(t); got != 2 {
			t.Errorf("the badge says %d; want 2: the mention in #launch and the reaction to the reader's message, "+
				"not the mention in a private channel they aren't in, in a deleted message, or a reaction to a deleted one", got)
		}
		if got, want := badge(t), uint64(newInFeed(t)); got != want {
			t.Errorf("the badge says %d and the feed shows %d new", got, want)
		}
	})

	t.Run("reading the feed marks nothing seen", func(t *testing.T) {
		// Home reads the first page on every load, and the Inbox reads more.
		for range 3 {
			newInFeed(t)
		}
		if got := badge(t); got != 2 {
			t.Errorf("after the feed was read, the badge says %d; want 2 still", got)
		}
	})

	t.Run("the Inbox marking it seen clears the badge", func(t *testing.T) {
		if err := activityBusiness.MarkActivitySeen(ctx, reader); err != nil {
			t.Fatalf("marking the feed seen: %v", err)
		}
		if got := badge(t); got != 0 {
			t.Errorf("after the Inbox marked the feed seen, the badge says %d; want 0", got)
		}
	})
}
