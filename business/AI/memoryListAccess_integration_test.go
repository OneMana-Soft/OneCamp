//go:build integration

package business

// V-586: the memory list and the briefing showed a person their own memory
// items from a private channel, a project or a DM they could no longer read.
// memoryModels.List matched an item on its scope OR its owner, so ownership
// alone carried an item out of the place it lives. The same OR reached the
// assistant's memory context and a catch-up's own items. Now every memory
// reader keeps only what its reader can read now, through the one check
// memory items have (memoryScopes), applied in the query before the page is
// cut, so a page is never short while readable items remain. An item that
// lives nowhere (no channel, project or DM) is its owner's personal note:
// there is no place to have left, so its owner alone reads it, and never
// through an agent reading for someone else.
//
// Run: go test -tags=integration ./business/AI/ -run TestTheMemoryListShowsOnlyWhatItsReaderCanRead -v

import (
	"context"
	"slices"
	"strings"
	"testing"

	"github.com/google/uuid"

	adapter "github.com/akashc777/OneCamp/adapter/AI"
	userDomain "github.com/akashc777/OneCamp/domain/User"
	"github.com/akashc777/OneCamp/initializers/postgresInit"
	userModels "github.com/akashc777/OneCamp/models/postgres/User"
	ai "github.com/akashc777/OneCamp/services/AI"
	"github.com/akashc777/OneCamp/tests/integration"
)

func TestTheMemoryListShowsOnlyWhatItsReaderCanRead(t *testing.T) {
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
	if _, err := env.PG.Exec(`UPDATE ai_settings SET enabled = true, memory_layer_enabled = true`); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _, _ = env.PG.Exec(`UPDATE ai_settings SET enabled = false, memory_layer_enabled = false`) })

	// Where everyone is now. The member is in #general, the website project
	// and a DM with peer; they were removed from #private and left the launch
	// project. Peer is only in that DM. Ann is only in her DM with bob, which
	// was never the member's.
	general, private := uuid.New(), uuid.New()
	website, launch := uuid.New(), uuid.New()
	ourDM := member.String() + " " + peer.String()
	theirDM := ann.String() + " " + bob.String()
	zero := "0001-01-01T00:00:00Z"
	dg.Mutate(t, []map[string]any{
		{"uid": "_:member", "dgraph.type": "User", "user_uuid": member.String(), "user_name": "member",
			"user_channels": []map[string]any{{"uid": "_:general", "dgraph.type": "Channel", "ch_uuid": general.String(), "ch_name": "general", "ch_deleted_at": zero}},
			"user_projects": []map[string]any{{"uid": "_:website", "dgraph.type": "Project", "project_uuid": website.String(), "project_name": "website", "project_deleted_at": zero}},
			"user_dms": []map[string]any{{"uid": "_:dm", "dgraph.type": "Dm", "dm_grouping_id": ourDM,
				"dm_participants": []map[string]any{{"uid": "_:member"}, {"uid": "_:peer"}}}}},
		{"uid": "_:peer", "dgraph.type": "User", "user_uuid": peer.String(), "user_name": "peer",
			"user_dms": []map[string]any{{"uid": "_:dm"}}},
		{"uid": "_:ann", "dgraph.type": "User", "user_uuid": ann.String(), "user_name": "ann",
			"user_dms": []map[string]any{{"uid": "_:theirdm", "dgraph.type": "Dm", "dm_grouping_id": theirDM,
				"dm_participants": []map[string]any{{"uid": "_:ann"}, {"uid": "_:bob", "dgraph.type": "User", "user_uuid": bob.String(), "user_name": "bob"}}}}},
	})
	reader := func(id uuid.UUID) *userModels.UserInfo {
		t.Helper()
		profile, err := userDomain.GetActiveDgraphUserInfoByUUID(ctx, id.String())
		if err != nil || profile == nil {
			t.Fatalf("the profile of %s: %v", id, err)
		}
		u := &userModels.UserInfo{UserDgraphInfo: *profile}
		u.UserPostgresInfo.Id = id
		return u
	}

	// write files one open commitment, overdue a day (the home's attention list
	// shows only overdue ones), by what it says, owned by owner in one
	// place, made age ago.
	write := func(owner uuid.UUID, content, column, scope, age string) {
		t.Helper()
		if _, err := env.PG.Exec(`INSERT INTO workspace_memory_items
			(kind, content, status, owner_user_id, `+column+`, due_at, dedup_hash, created_at, updated_at)
			VALUES ('commitment', $1, 'open', $2, $3, NOW() - interval '1 day', $4, NOW() - $5::interval, NOW() - $5::interval)`,
			content, owner, scope, uuid.NewString(), age); err != nil {
			t.Fatal(err)
		}
	}
	write(member, "Send the general numbers", "channel_uuid", general.String(), "1 hour")
	write(member, "Send the private salary sheet", "channel_uuid", private.String(), "1 hour")
	write(member, "Review the website copy", "project_uuid", website.String(), "1 hour")
	write(member, "Ship the launch plan", "project_uuid", launch.String(), "1 hour")
	write(member, "Call peer about the venue", "chat_grp_id", ourDM, "1 hour")
	write(member, "Book ann and bob's flights", "chat_grp_id", theirDM, "1 hour")
	write(peer, "Peer books the room", "chat_grp_id", ourDM, "1 hour")
	// The member's personal note: it lives nowhere.
	if _, err := env.PG.Exec(`INSERT INTO workspace_memory_items
		(kind, content, status, owner_user_id, due_at, dedup_hash, created_at, updated_at)
		VALUES ('commitment', 'Tidy my own notes', 'open', $1, NOW() - interval '1 day', $2, NOW() - interval '1 hour', NOW() - interval '1 hour')`,
		member, uuid.NewString()); err != nil {
		t.Fatal(err)
	}

	all := []string{"Book ann and bob's flights", "Call peer about the venue", "Peer books the room", "Review the website copy",
		"Send the general numbers", "Send the private salary sheet", "Ship the launch plan", "Tidy my own notes"}
	// The member's own items they can read now, and everything they can read now.
	theirOwn := []string{"Call peer about the venue", "Review the website copy", "Send the general numbers", "Tidy my own notes"}
	theyRead := []string{"Call peer about the venue", "Peer books the room", "Review the website copy", "Send the general numbers", "Tidy my own notes"}

	// says lists which of the eight commitments a text quotes.
	says := func(texts ...string) []string {
		text := strings.Join(texts, "\n")
		var got []string
		for _, c := range all {
			if strings.Contains(text, c) {
				got = append(got, c)
			}
		}
		return got
	}
	listed := func(u *userModels.UserInfo, limit int) (*adapter.MemoryListResponse, []string) {
		t.Helper()
		resp, err := ListWorkspaceMemory(ctx, u, nil, nil, limit, "")
		if err != nil {
			t.Fatal(err)
		}
		var texts []string
		for _, it := range resp.Items {
			texts = append(texts, it.Content)
		}
		return resp, texts
	}
	briefed := func(u *userModels.UserInfo) []string {
		t.Helper()
		b, err := GetBriefing(ctx, u)
		if err != nil {
			t.Fatal(err)
		}
		var texts []string
		for _, it := range b.OpenItems {
			texts = append(texts, it.Content)
		}
		return texts
	}
	me := reader(member)

	t.Run("the memory list shows a member only what they can read now, and counts what it shows", func(t *testing.T) {
		resp, texts := listed(me, 50)
		if got := says(texts...); !slices.Equal(got, theyRead) {
			t.Errorf("the member's memory list shows %q; want only %q: they were removed from #private, left the launch project and were never in ann and bob's DM", got, theyRead)
		}
		if resp.Counts["commitment"] != len(theyRead) {
			t.Errorf("the list counts %d open commitments; it shows %d", resp.Counts["commitment"], len(theyRead))
		}
	})

	t.Run("a DM's items are seen by its people only", func(t *testing.T) {
		if _, texts := listed(reader(peer), 50); !slices.Equal(says(texts...), []string{"Call peer about the venue", "Peer books the room"}) {
			t.Errorf("peer's memory list shows %q; want their DM's two items", says(texts...))
		}
		if _, texts := listed(reader(ann), 50); !slices.Equal(says(texts...), []string{"Book ann and bob's flights"}) {
			t.Errorf("ann's memory list shows %q; want her DM's one item", says(texts...))
		}
	})

	t.Run("a personal note is its owner's alone, never an agent's reading for someone else", func(t *testing.T) {
		if _, texts := listed(reader(peer), 50); slices.Contains(says(texts...), "Tidy my own notes") {
			t.Error("peer's memory list shows the member's personal note")
		}
		// The member's agent, asked by peer: only what both can read, and none
		// of the member's own notes.
		resp, err := ListWorkspaceMemory(ai.WithRunRequester(ctx, peer.String(), member.String()), me, nil, nil, 50, "")
		if err != nil {
			t.Fatal(err)
		}
		var texts []string
		for _, it := range resp.Items {
			texts = append(texts, it.Content)
		}
		if got := says(texts...); !slices.Equal(got, []string{"Call peer about the venue", "Peer books the room"}) {
			t.Errorf("the member's agent, reading for peer, shows %q; want only their DM's two items", got)
		}
	})

	t.Run("the briefing's open items are the member's own, from where they are now", func(t *testing.T) {
		if got := says(briefed(me)...); !slices.Equal(got, theirOwn) {
			t.Errorf("the member's briefing shows %q; want only %q", got, theirOwn)
		}
	})

	t.Run("the assistant's memory context holds only what the member can read now", func(t *testing.T) {
		channels, projects := getAccessibleResourceUUIDs(me)
		var texts []string
		for _, it := range fetchMemoryItems(ctx, me, channels, projects) {
			texts = append(texts, it.Content)
		}
		if got := says(texts...); !slices.Equal(got, theyRead) {
			t.Errorf("the assistant's memory context holds %q; want only %q", got, theyRead)
		}
	})

	t.Run("a catch-up lists the member's own items in that place only", func(t *testing.T) {
		got := says(catchUpOwnedItems(ctx, me, adapter.CatchUpRequest{ScopeType: catchUpScopeChannel, ChannelUUID: general.String()}))
		if !slices.Equal(got, []string{"Send the general numbers"}) {
			t.Errorf("a catch-up on #general lists %q; want only the member's item there", got)
		}
		if got := says(catchUpOwnedItems(ctx, me, adapter.CatchUpRequest{ScopeType: catchUpScopeChannel, ChannelUUID: private.String()})); len(got) != 0 {
			t.Errorf("a catch-up on #private, which the member was removed from, lists %q", got)
		}
	})

	t.Run("a page is never short while readable items remain", func(t *testing.T) {
		// A dozen newer items of the member's from places they left: a page cut
		// before the check would hold only these and come back short or empty.
		for i := 0; i < 12; i++ {
			write(member, "Left behind "+uuid.NewString(), "channel_uuid", private.String(), "1 minute")
		}
		if _, texts := listed(me, 3); len(texts) != 3 || len(says(texts...)) != 3 {
			t.Errorf("a list page of 3 holds %q; want 3 the member can read", texts)
		}
		if b := briefed(me); len(b) != len(theirOwn) || !slices.Equal(says(b...), theirOwn) {
			t.Errorf("the briefing shows %q; want %q", b, theirOwn)
		}
		var titles []string
		for _, it := range gatherMemoryAttentionItems(ctx, me) {
			titles = append(titles, it.Title)
		}
		if len(titles) != len(theirOwn) || !slices.Equal(says(titles...), theirOwn) {
			t.Errorf("the attention list shows %q; want %q", titles, theirOwn)
		}
	})
}
