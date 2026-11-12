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
	"encoding/json"
	"fmt"
	"slices"
	"testing"
	"time"

	"github.com/google/uuid"

	activityBusiness "github.com/akashc777/OneCamp/business/Activity"
	_ "github.com/akashc777/OneCamp/business/Board"  // hands the feed the board read rule
	_ "github.com/akashc777/OneCamp/business/Doc"    // hands the feed the doc read rule
	_ "github.com/akashc777/OneCamp/business/Follow" // hands the feed its notices
	readStateBusiness "github.com/akashc777/OneCamp/business/ReadState"
	"github.com/akashc777/OneCamp/helpers"
	"github.com/akashc777/OneCamp/initializers/postgresInit"
	mqttStruct "github.com/akashc777/OneCamp/models/mqtt"
	userModels "github.com/akashc777/OneCamp/models/postgres/User"
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
		p, err := activityBusiness.GetUnifiedActivity(ctx, activityBusiness.FeedQuery{User: reader, DgraphID: uids["reader"], Before: time.Now().Add(time.Minute), Limit: 50})
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

// The owner's bug (5 Nov): the badge stayed, even on clicking Inbox, and
// nothing in the Inbox said which item it counted. One rule now says what is
// unread, the badge counts the Inbox's default view by it (the feed with its
// notices: C4), each item the Inbox lists carries it (C8), reading an item in
// its channel takes it off (C7), and opening the Inbox answers the badge after
// the write and tells the person's own tabs and devices (C6).
//
// Run: go test -tags=integration ./tests/integration/ -run TestInboxBadgeIsTheInboxsUnreadItems -v
func TestInboxBadgeIsTheInboxsUnreadItems(t *testing.T) {
	ctx := context.Background()
	env := integration.SetupEnv(t)
	if err := postgresInit.ConnectPostgres(ctx, env.DSN); err != nil {
		t.Fatal(err)
	}
	broker := &noticeBroker{}
	integration.UseMqttClient(t, broker)
	dg := integration.SetupDgraph(t)

	reader, maya := uuid.New(), uuid.New()
	for name, id := range map[string]uuid.UUID{"reader": reader, "maya": maya} {
		if _, err := env.PG.Exec(`INSERT INTO users (id, email_id, username, display_name, created_at, updated_at)
			VALUES ($1, $2, $3, $4, NOW(), NOW())`, id, id.String()+"@example.test", name, name); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := env.PG.Exec(`INSERT INTO last_seen_activity (user_id, user_last_seen) VALUES ($1, NOW() - interval '1 hour')`, reader); err != nil {
		t.Fatal(err)
	}
	const live = "0001-01-01T00:00:00Z"
	now := time.Now().UTC()
	at := func(ago time.Duration) string { return now.Add(-ago).Format(time.RFC3339Nano) }
	launch, ops := uuid.NewString(), uuid.NewString()
	members := []map[string]any{{"uid": "_:reader"}, {"uid": "_:maya"}}
	uids := dg.Mutate(t, []map[string]any{
		{"uid": "_:reader", "dgraph.type": "User", "user_uuid": reader.String(), "user_name": "reader"},
		{"uid": "_:maya", "dgraph.type": "User", "user_uuid": maya.String(), "user_name": "maya"},
		{"uid": "_:launch", "dgraph.type": "Channel", "ch_uuid": launch, "ch_name": "launch", "ch_private": false, "ch_deleted_at": live, "ch_members": members},
		{"uid": "_:ops", "dgraph.type": "Channel", "ch_uuid": ops, "ch_name": "ops", "ch_private": false, "ch_deleted_at": live, "ch_members": members},
		// A mention in #launch since the last look, unread there.
		{"uid": "_:p-launch", "dgraph.type": "Post", "post_uuid": uuid.NewString(), "post_text": "@reader the numbers?",
			"post_by": map[string]any{"uid": "_:maya"}, "post_channel": map[string]any{"uid": "_:launch"}, "post_created_at": at(40 * time.Minute), "post_deleted_at": live},
		{"uid": "_:m-launch", "dgraph.type": "Mention", "mention_users": []map[string]any{{"uid": "_:reader"}},
			"mention_post": map[string]any{"uid": "_:p-launch"}, "mention_created_at": at(40 * time.Minute)},
		// A mention in #ops since the last look, which they read in #ops since.
		{"uid": "_:p-ops", "dgraph.type": "Post", "post_uuid": uuid.NewString(), "post_text": "@reader rota",
			"post_by": map[string]any{"uid": "_:maya"}, "post_channel": map[string]any{"uid": "_:ops"}, "post_created_at": at(30 * time.Minute), "post_deleted_at": live},
		{"uid": "_:m-ops", "dgraph.type": "Mention", "mention_users": []map[string]any{{"uid": "_:reader"}},
			"mention_post": map[string]any{"uid": "_:p-ops"}, "mention_created_at": at(30 * time.Minute)},
	})
	for name, id := range map[string]string{"launch": launch, "ops": ops} {
		if _, err := env.PG.Exec(`INSERT INTO channels (id, ch_name, ch_private) VALUES ($1, $2, false)`, id, name); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := env.PG.Exec(`INSERT INTO last_seen_channel (user_id, channel_id, user_last_seen) VALUES ($1, $2, NOW() - interval '10 minutes')`, reader, ops); err != nil {
		t.Fatal(err)
	}
	notice := func(kind string, ago time.Duration) {
		t.Helper()
		if _, err := env.PG.Exec(`INSERT INTO resource_notices (id, user_uuid, kind, resource_kind, resource_uuid, reason, dedup_key, created_at)
			VALUES ($1, $2, $3, 'doc', $4, 'subject', $5, $6)`, uuid.New(), reader, kind, uuid.NewString(), uuid.NewString(), now.Add(-ago)); err != nil {
			t.Fatal(err)
		}
	}
	// Two notices since the last look, and one before it.
	notice("doc.shared", 20*time.Minute)
	notice("doc.mentioned", 15*time.Minute)
	notice("doc.shared", 3*time.Hour)

	badge := func(t *testing.T) uint64 {
		t.Helper()
		n, err := activityBusiness.UnreadActivityCount(ctx, reader, uids["reader"])
		if err != nil {
			t.Fatalf("counting the badge: %v", err)
		}
		return n
	}
	// marked is what the Inbox's first page (the feed with notices) draws a
	// dot on.
	marked := func(t *testing.T) []string {
		t.Helper()
		p, err := activityBusiness.GetUnifiedActivity(ctx, activityBusiness.FeedQuery{User: reader, DgraphID: uids["reader"], Before: time.Now().Add(time.Minute), Limit: 20, Notices: true})
		if err != nil {
			t.Fatalf("reading the Inbox: %v", err)
		}
		var out []string
		for _, it := range p.Activities {
			if it.Unread {
				out = append(out, it.ActivityType)
			}
		}
		return out
	}

	t.Run("the badge is the Inbox's unread items, notices counted and what was read in its channel not", func(t *testing.T) {
		if got := badge(t); got != 3 {
			t.Errorf("the badge says %d; want 3: the mention in #launch and the two new notices (C4), not the mention read in #ops (C7) or the old notice", got)
		}
		if got := marked(t); len(got) != 3 || got[0] != "NOTICE" || got[1] != "NOTICE" || got[2] != "MENTION" {
			t.Errorf("the Inbox marks %v unread; want the badge's three, newest first: the two notices, then the mention (C8)", got)
		}
	})

	t.Run("opening the Inbox answers 0 and tells their own tabs and devices", func(t *testing.T) {
		who := &userModels.UserInfo{}
		who.UserPostgresInfo.Id = reader
		who.UserDgraphInfo.Uuid, who.UserDgraphInfo.Uid = reader.String(), uids["reader"]
		n, err := readStateBusiness.InboxSeen(ctx, who)
		if err != nil || n == nil || *n != 0 {
			t.Fatalf("opening the Inbox answered %v, %v; want 0", n, err)
		}
		if busy := helpers.SettleBackground(10 * time.Second); len(busy) > 0 {
			t.Fatalf("still at work: %v", busy)
		}
		if got := badge(t); got != 0 {
			t.Errorf("after opening the Inbox the badge says %d; want 0", got)
		}
		if got := marked(t); len(got) != 0 {
			t.Errorf("after opening the Inbox it still marks %v unread", got)
		}
		mine, theirs := broker.inboxMoves(helpers.GetMqttTopicForUserActivity(reader.String())), broker.inboxMoves(helpers.GetMqttTopicForUserActivity(maya.String()))
		if len(mine) != 1 || mine[0].Unread != 0 || mine[0].Seen == "" || len(theirs) != 0 {
			t.Errorf("opening the Inbox told %+v on their own topic and %+v on maya's; want one read.moved inbox, seen, unread 0, on theirs alone", mine, theirs)
		}
	})

	t.Run("what arrives after counts, and only that", func(t *testing.T) {
		notice("doc.shared", -time.Second) // a moment from now: after the last look
		if got := badge(t); got != 1 {
			t.Errorf("a notice after the Inbox was opened makes the badge %d; want 1", got)
		}
	})
}

// inboxMoves is each read.moved of kind inbox published on topic.
func (b *noticeBroker) inboxMoves(topic string) []readStateBusiness.MovedEvent {
	b.mu.Lock()
	defer b.mu.Unlock()
	var out []readStateBusiness.MovedEvent
	for _, m := range b.got {
		var msg struct {
			Type int8                         `json:"type"`
			Data readStateBusiness.MovedEvent `json:"data"`
		}
		if m.topic == topic && json.Unmarshal(m.payload, &msg) == nil && msg.Type == mqttStruct.MESSAGE_READ_MOVED && msg.Data.Kind == readStateBusiness.KindInbox {
			out = append(out, msg.Data)
		}
	}
	return out
}

// The badge runs on every sidebar load, so it stays cheap at size (A2): with
// 10,000 items in the feed and 1,000 notices, nothing new since the last look
// costs the stored count alone, and 150 new ones a page each of the feed and
// the notices, cut at the cap (100: "99+"). The times are logged for the
// review; the bound only catches a disaster.
//
// Run: go test -tags=integration ./tests/integration/ -run TestInboxBadgeCostAt10kFeedItems -v
func TestInboxBadgeCostAt10kFeedItems(t *testing.T) {
	ctx := context.Background()
	env := integration.SetupEnv(t)
	if err := postgresInit.ConnectPostgres(ctx, env.DSN); err != nil {
		t.Fatal(err)
	}
	integration.StubMqttClient(t)
	dg := integration.SetupDgraph(t)

	reader, maya := uuid.New(), uuid.New()
	for name, id := range map[string]uuid.UUID{"reader": reader, "maya": maya} {
		if _, err := env.PG.Exec(`INSERT INTO users (id, email_id, username, display_name, created_at, updated_at)
			VALUES ($1, $2, $3, $4, NOW(), NOW())`, id, id.String()+"@example.test", name, name); err != nil {
			t.Fatal(err)
		}
	}
	launch := uuid.NewString()
	if _, err := env.PG.Exec(`INSERT INTO channels (id, ch_name, ch_private) VALUES ($1, 'launch', false)`, launch); err != nil {
		t.Fatal(err)
	}
	const live = "0001-01-01T00:00:00Z"
	now := time.Now().UTC()
	nodes := []map[string]any{
		{"uid": "_:reader", "dgraph.type": "User", "user_uuid": reader.String(), "user_name": "reader"},
		{"uid": "_:maya", "dgraph.type": "User", "user_uuid": maya.String(), "user_name": "maya"},
		{"uid": "_:launch", "dgraph.type": "Channel", "ch_uuid": launch, "ch_name": "launch", "ch_private": false, "ch_deleted_at": live,
			"ch_members": []map[string]any{{"uid": "_:reader"}, {"uid": "_:maya"}}},
	}
	const posts, old, fresh = 100, 10_000, 150
	for p := range posts {
		nodes = append(nodes, map[string]any{"uid": fmt.Sprintf("_:p%d", p), "dgraph.type": "Post", "post_uuid": uuid.NewString(),
			"post_text": "@reader", "post_by": map[string]any{"uid": "_:maya"}, "post_channel": map[string]any{"uid": "_:launch"},
			"post_created_at": now.Add(-48 * time.Hour).Format(time.RFC3339Nano), "post_deleted_at": live})
	}
	mention := func(i int, ago time.Duration) map[string]any {
		return map[string]any{"uid": fmt.Sprintf("_:m%d", i), "dgraph.type": "Mention", "mention_users": []map[string]any{{"uid": "_:reader"}},
			"mention_post": map[string]any{"uid": fmt.Sprintf("_:p%d", i%posts)}, "mention_created_at": now.Add(-ago).Format(time.RFC3339Nano)}
	}
	for i := range old {
		nodes = append(nodes, mention(i, 2*time.Hour+time.Duration(i)*time.Second))
	}
	uids := dg.Mutate(t, nodes)
	if _, err := env.PG.Exec(`INSERT INTO resource_notices (id, user_uuid, kind, resource_kind, resource_uuid, reason, dedup_key, created_at)
		SELECT md5(random()::text || g::text)::uuid, $1, 'doc.shared', 'doc', md5(random()::text || g::text), 'subject', md5(random()::text || g::text), NOW() - interval '3 hours' - g * interval '1 second'
		  FROM generate_series(1, 1000) g`, reader); err != nil {
		t.Fatal(err)
	}
	if _, err := env.PG.Exec(`INSERT INTO last_seen_activity (user_id, user_last_seen) VALUES ($1, NOW() - interval '1 hour')`, reader); err != nil {
		t.Fatal(err)
	}

	// timed is the badge's median time over five reads, and its count.
	timed := func(t *testing.T) (time.Duration, uint64) {
		t.Helper()
		var took []time.Duration
		var n uint64
		for range 5 {
			start := time.Now()
			got, err := activityBusiness.UnreadActivityCount(ctx, reader, uids["reader"])
			if err != nil {
				t.Fatalf("counting the badge: %v", err)
			}
			took, n = append(took, time.Since(start)), got
		}
		slices.Sort(took)
		return took[2], n
	}

	quiet, n := timed(t)
	if n != 0 {
		t.Fatalf("with nothing new the badge says %d", n)
	}
	var more []map[string]any
	for i := range fresh {
		m := mention(old+i, time.Duration(i+1)*time.Second)
		m["mention_post"] = map[string]any{"uid": uids[fmt.Sprintf("p%d", i%posts)]}
		m["mention_users"] = []map[string]any{{"uid": uids["reader"]}}
		more = append(more, m)
	}
	dg.Mutate(t, more)
	if _, err := env.PG.Exec(`INSERT INTO resource_notices (id, user_uuid, kind, resource_kind, resource_uuid, reason, dedup_key, created_at)
		SELECT md5(random()::text || g::text)::uuid, $1, 'doc.shared', 'doc', md5(random()::text || g::text), 'subject', md5(random()::text || g::text), NOW() - g * interval '1 second'
		  FROM generate_series(1, 20) g`, reader); err != nil {
		t.Fatal(err)
	}
	busy, n := timed(t)
	if n != 100 {
		t.Errorf("150 new mentions and 20 new notices make the badge %d; want the cap, 100", n)
	}
	t.Logf("badge at %d feed items and 1,000 notices: nothing new %v, %d new mentions and 20 notices %v (median of 5)", old+fresh, quiet, fresh, busy)
	if quiet > 2*time.Second || busy > 2*time.Second {
		t.Errorf("the badge took %v with nothing new and %v with %d new; want well under 2 s each", quiet, busy, fresh)
	}
}
