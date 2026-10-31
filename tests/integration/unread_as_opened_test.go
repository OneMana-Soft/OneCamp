//go:build integration
// +build integration

package integration_test

// How many were unread as a DM or a group opened (chat B27), against real
// Postgres 12 and Dgraph. Reading a conversation's latest page marks it read,
// so the sidebar's count, asked for as the conversation opens, could come
// after that mark and say none, and the line where someone left off was never
// drawn. The page says the count itself, read just before its mark: what
// the reader hadn't read, never their own, and nothing in a conversation they
// never opened. Read again, it says none.
//
// Run: go test -tags=integration ./tests/integration/ -run 'TestTheLatestPageSaysWhatWasUnread' -v

import (
	"context"
	"testing"
	"time"

	chatBusiness "github.com/akashc777/OneCamp/business/Chat"
	lastseenBusiness "github.com/akashc777/OneCamp/business/LastSeenChat"
	chatDomain "github.com/akashc777/OneCamp/domain/Chat"
	"github.com/akashc777/OneCamp/helpers"
	"github.com/akashc777/OneCamp/initializers/postgresInit"
	"github.com/akashc777/OneCamp/tests/integration"
	"github.com/google/uuid"
)

func TestTheLatestPageSaysWhatWasUnread(t *testing.T) {
	ctx := context.Background()
	env := integration.SetupEnv(t)
	if err := postgresInit.ConnectPostgres(ctx, env.DSN); err != nil {
		t.Fatal(err)
	}
	integration.SetupRedis(t)
	dg := integration.SetupDgraph(t)
	integration.StubMqttClient(t)
	t.Cleanup(func() { helpers.SettleBackground(10 * time.Second) })

	ids := map[string]uuid.UUID{"ada": uuid.New(), "bea": uuid.New(), "cal": uuid.New()}
	for name, id := range ids {
		if _, err := env.PG.Exec(`INSERT INTO users (id, email_id, username, display_name, created_at, updated_at)
			VALUES ($1, $2, $3, $4, NOW(), NOW())`, id, id.String()+"@example.test", name, name); err != nil {
			t.Fatal(err)
		}
	}
	dm := helpers.GetGroupingId(ids["ada"].String(), ids["bea"].String())
	neverOpened := helpers.GetGroupingId(ids["ada"].String(), ids["cal"].String())
	group := "fedcba9876543210fedcba9876543210"
	one := func(name string) map[string]any { return map[string]any{"uid": "_:" + name} }
	nodes := []map[string]any{}
	for name, id := range ids {
		nodes = append(nodes, map[string]any{"uid": "_:" + name, "dgraph.type": "User", "user_uuid": id.String(), "user_name": name})
	}
	nodes = append(nodes,
		map[string]any{"dgraph.type": "Dm", "dm_grouping_id": dm, "dm_participants": []map[string]any{one("ada"), one("bea")}},
		map[string]any{"dgraph.type": "Dm", "dm_grouping_id": neverOpened, "dm_participants": []map[string]any{one("ada"), one("cal")}},
		map[string]any{"dgraph.type": "Dm", "dm_grouping_id": group, "dm_participants": []map[string]any{one("ada"), one("bea"), one("cal")}},
	)
	dg.Mutate(t, nodes)

	// Ada read each conversation an hour ago; since then Bea wrote twice in
	// the DM and Ada once; in the group, Bea and Cal once each and Ada once.
	// Cal wrote to Ada too, in a DM she has never opened.
	t0 := time.Now().Add(-time.Hour)
	for _, grp := range []string{dm, group} {
		if err := lastseenBusiness.CreateOrUpdateLastSeenChat(ctx, grp, ids["ada"], t0); err != nil {
			t.Fatal(err)
		}
	}
	chat := func(grp, by string, at time.Time) {
		if _, err := env.PG.Exec(`INSERT INTO chats (id, created_by, grp_id, created_at, updated_at) VALUES ($1, $2, $3, $4, $4)`, uuid.New(), ids[by], grp, at); err != nil {
			t.Fatal(err)
		}
	}
	chat(dm, "bea", t0.Add(time.Second))
	chat(dm, "bea", t0.Add(2*time.Second))
	chat(dm, "ada", t0.Add(3*time.Second))
	chat(group, "bea", t0.Add(time.Second))
	chat(group, "cal", t0.Add(2*time.Second))
	chat(group, "ada", t0.Add(3*time.Second))
	chat(neverOpened, "cal", t0.Add(time.Second))

	unread := func(what string, page chatBusiness.ChatPagination, err error) int {
		t.Helper()
		if err != nil {
			t.Fatalf("%s: %v", what, err)
		}
		if page.Unread == nil {
			t.Fatalf("%s: the page says nothing of what was unread", what)
		}
		return *page.Unread
	}

	// The DM: Bea's two, not Ada's own; read again, none; the sidebar agrees.
	page, err := chatBusiness.GetDgraphLatestChatFromDgraph(ctx, ids["ada"], ids["bea"].String())
	if n := unread("the DM", page, err); n != 2 {
		t.Errorf("the DM as it opened: %d unread, want 2", n)
	}
	page, err = chatBusiness.GetDgraphLatestChatFromDgraph(ctx, ids["ada"], ids["bea"].String())
	if n := unread("the DM again", page, err); n != 0 {
		t.Errorf("the DM read again: %d unread, want 0", n)
	}
	counts, err := chatDomain.GetLatestChatMessageCountByUserID(ctx, ids["ada"])
	if err != nil || counts[dm] == nil || counts[dm].ChatCount != 0 {
		t.Errorf("the sidebar's count after: %+v, %v", counts[dm], err)
	}

	// The group the same way.
	page, err = chatBusiness.GetDgraphLatestGroupChatFromDgraph(ctx, ids["ada"], group)
	if n := unread("the group", page, err); n != 2 {
		t.Errorf("the group as it opened: %d unread, want 2", n)
	}
	page, err = chatBusiness.GetDgraphLatestGroupChatFromDgraph(ctx, ids["ada"], group)
	if n := unread("the group again", page, err); n != 0 {
		t.Errorf("the group read again: %d unread, want 0", n)
	}

	// A DM never opened has no line, as the sidebar counts it.
	page, err = chatBusiness.GetDgraphLatestChatFromDgraph(ctx, ids["ada"], ids["cal"].String())
	if n := unread("the DM never opened", page, err); n != 0 {
		t.Errorf("a DM never opened: %d unread, want 0", n)
	}
}
