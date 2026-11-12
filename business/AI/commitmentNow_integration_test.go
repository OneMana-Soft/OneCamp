//go:build integration

package business

// V-581 (C11): the owner's home said "1 task overdue" with no overdue task.
// The one thing behind it was a memory commitment ("Xnnnxxmmx", keyboard
// noise) due in June: the attention list called every open commitment that
// wasn't due today or later "Overdue commitment", undated ones too, however
// old, and the nudge engine raised the same ones as urgent.
//
// Now ONE predicate says when a commitment needs its person now: open, live,
// dated, due before today, and overdue for at most 14 days. The attention
// list and the nudge engine both use it; an older one is stale, and the
// memory page asks of it "Still relevant?".
//
// Run: go test -tags=integration ./business/AI/ -run TestACommitmentNeedsMeNowOnlyWhileRecentlyOverdue -v

import (
	"context"
	"encoding/json"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"

	userDomain "github.com/akashc777/OneCamp/domain/User"
	"github.com/akashc777/OneCamp/initializers/postgresInit"
	userModels "github.com/akashc777/OneCamp/models/postgres/User"
	memoryModels "github.com/akashc777/OneCamp/models/postgres/WorkspaceMemory"
	nudgeModels "github.com/akashc777/OneCamp/models/postgres/WorkspaceNudge"
	"github.com/akashc777/OneCamp/tests/integration"
)

func TestACommitmentNeedsMeNowOnlyWhileRecentlyOverdue(t *testing.T) {
	ctx := context.Background()
	env := integration.SetupEnv(t)
	if err := postgresInit.ConnectPostgres(ctx, env.DSN); err != nil {
		t.Fatal(err)
	}
	dg := integration.SetupDgraph(t)
	integration.StubMqttClient(t)

	owner := uuid.New()
	if _, err := env.PG.Exec(`INSERT INTO users (id, email_id, username, display_name, created_at, updated_at)
		VALUES ($1, 'owner@example.test', 'owner', 'owner', NOW(), NOW())`, owner); err != nil {
		t.Fatal(err)
	}
	channel := uuid.New()
	zero := "0001-01-01T00:00:00Z"
	dg.Mutate(t, []map[string]any{{"uid": "_:owner", "dgraph.type": "User", "user_uuid": owner.String(), "user_name": "owner",
		"user_channels": []map[string]any{{"uid": "_:ch", "dgraph.type": "Channel", "ch_uuid": channel.String(), "ch_name": "launch", "ch_deleted_at": zero}}}})

	// Each of the owner's commitments in #launch: due when (SQL), in status,
	// deleted when said, last touched a month ago.
	write := func(content, due, status string, deleted bool) uuid.UUID {
		t.Helper()
		var id uuid.UUID
		if err := env.PG.QueryRow(`INSERT INTO workspace_memory_items
			(kind, content, status, owner_user_id, channel_uuid, due_at, dedup_hash, deleted_at, created_at, updated_at)
			VALUES ('commitment', $1, $2, $3, $4, `+due+`, $5, CASE WHEN $6 THEN NOW() END, NOW() - interval '30 days', NOW() - interval '30 days')
			RETURNING id`, content, status, owner, channel, uuid.NewString(), deleted).Scan(&id); err != nil {
			t.Fatal(err)
		}
		return id
	}
	ids := map[string]uuid.UUID{
		"undated":          write("Look into the pricing page", "NULL", "open", false),
		"due today":        write("Send the deck", "NOW()", "open", false),
		"overdue 1 day":    write("Book the venue", "NOW() - interval '1 day'", "open", false),
		"overdue 14 days":  write("Write the launch post", "NOW() - interval '14 days'", "open", false),
		"overdue 15 days":  write("Xnnnxxmmx", "NOW() - interval '15 days'", "open", false),
		"overdue, deleted": write("Order the stickers", "NOW() - interval '1 day'", "open", true),
		"overdue, done":    write("Call the printer", "NOW() - interval '1 day'", "resolved", false),
	}
	needsMe := []string{"overdue 1 day", "overdue 14 days"}
	nameOf := func(id string) string {
		for name, v := range ids {
			if v.String() == id {
				return name
			}
		}
		return "unknown " + id
	}

	// asOwner is the owner as a request has them: with where they are now.
	asOwner := func(t *testing.T) *userModels.UserInfo {
		t.Helper()
		profile, err := userDomain.GetActiveDgraphUserInfoByUUID(ctx, owner.String())
		if err != nil || profile == nil {
			t.Fatalf("the owner's profile: %v", err)
		}
		me := &userModels.UserInfo{UserDgraphInfo: *profile}
		me.UserPostgresInfo.Id = owner
		return me
	}

	t.Run("the home's attention list holds only recently overdue commitments", func(t *testing.T) {
		var got []string
		for _, it := range gatherMemoryAttentionItems(ctx, asOwner(t)) {
			if it.Source == "commitment" {
				got = append(got, nameOf(strings.TrimPrefix(it.URL, "/app/ai/memory?item=")))
			}
		}
		slices.Sort(got)
		if !slices.Equal(got, needsMe) {
			t.Errorf("the attention list holds the commitments %q; want %q (undated is pending, due today isn't overdue, over 14 days is stale)", got, needsMe)
		}
	})

	t.Run("the engine nudges only of recently overdue commitments", func(t *testing.T) {
		var got []string
		for _, c := range gatherMemoryNudges(ctx, time.Now()) {
			if c.userID == owner && c.kind == nudgeModels.KindOverdueCommitment {
				got = append(got, nameOf(c.sourceID))
			}
		}
		slices.Sort(got)
		if !slices.Equal(got, needsMe) {
			t.Errorf("the engine would raise overdue-commitment nudges for %q; want %q", got, needsMe)
		}
	})

	t.Run("the memory page marks a stale commitment, and only it", func(t *testing.T) {
		// The page as the owner reads it: their open commitments from where
		// they are now (V-586's scoped read).
		page, err := ListWorkspaceMemory(ctx, asOwner(t), []string{memoryModels.KindCommitment}, []string{memoryModels.StatusOpen}, 50, "")
		if err != nil {
			t.Fatal(err)
		}
		if len(page.Items) != 5 {
			t.Errorf("the memory page lists %d open commitments; want the owner's 5 live ones", len(page.Items))
		}
		var stale []string
		for _, v := range page.Items {
			raw, err := json.Marshal(v)
			if err != nil {
				t.Fatal(err)
			}
			if strings.Contains(string(raw), `"stale":true`) {
				stale = append(stale, nameOf(v.ID))
			}
		}
		if want := []string{"overdue 15 days"}; !slices.Equal(stale, want) {
			t.Errorf("the memory page marks %q stale (\"Still relevant?\"); want %q", stale, want)
		}
	})
}
