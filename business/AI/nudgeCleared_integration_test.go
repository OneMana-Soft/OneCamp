//go:build integration

package business

// A nudge closed anywhere is named live (V-577, main's notification audit
// H2). The "cleared" event carried only the open count, so an open tab kept a
// resolved nudge listed, and counted, until it reloaded. The nudges the engine
// closes for a person in one pass are now told in ONE "cleared" on their own
// activity topic, named in nudge_ids, with the open count after.
//
// Run: go test -tags=integration ./business/AI/ -run TestEveryNudgeTheEngineClosesIsNamedLive -v

import (
	"context"
	"encoding/json"
	"slices"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/akashc777/OneCamp/helpers"
	"github.com/akashc777/OneCamp/initializers/postgresInit"
	mqttStruct "github.com/akashc777/OneCamp/models/mqtt"
	nudgeModels "github.com/akashc777/OneCamp/models/postgres/WorkspaceNudge"
	"github.com/akashc777/OneCamp/tests/integration"
)

func TestEveryNudgeTheEngineClosesIsNamedLive(t *testing.T) {
	ctx := context.Background()
	env := integration.SetupEnv(t)
	if err := postgresInit.ConnectPostgres(ctx, env.DSN); err != nil {
		t.Fatal(err)
	}
	broker := integration.RecordMqtt(t)

	owner, maya := uuid.New(), uuid.New()
	for name, id := range map[string]uuid.UUID{"owner": owner, "maya": maya} {
		if _, err := env.PG.Exec(`INSERT INTO users (id, email_id, username, display_name, created_at, updated_at)
			VALUES ($1, $2, $3, $4, NOW(), NOW())`, id, id.String()+"@example.test", name, name); err != nil {
			t.Fatal(err)
		}
	}
	nudge := func(t *testing.T, whom uuid.UUID, key string) string {
		t.Helper()
		var id string
		if err := env.PG.QueryRow(`INSERT INTO workspace_nudges (user_id, kind, title, body, source_type, source_id, status, priority, dedup_key)
			VALUES ($1, 'overdue_commitment', 'Overdue commitment', 'You committed to it.', 'memory', $2, 'open', 1, $3)
			RETURNING id`, whom, uuid.NewString(), key).Scan(&id); err != nil {
			t.Fatal(err)
		}
		return id
	}
	type cleared struct {
		NudgeIDs  []string `json:"nudge_ids"`
		OpenCount int      `json:"open_count"`
	}
	// clearedFor is each "cleared" told on whom's own activity topic since
	// the last call.
	seen := map[uuid.UUID]int{}
	clearedFor := func(t *testing.T, whom uuid.UUID) []cleared {
		t.Helper()
		if busy := helpers.SettleBackground(10 * time.Second); len(busy) > 0 {
			t.Fatalf("still at work: %v", busy)
		}
		var out []cleared
		msgs := broker.Messages(helpers.GetMqttTopicForUserActivity(whom.String()), mqttStruct.MESSAGE_AI_NUDGE)
		for _, raw := range msgs[seen[whom]:] {
			var m struct {
				Action string `json:"action"`
				cleared
			}
			if err := json.Unmarshal(raw, &m); err != nil {
				t.Fatal(err)
			}
			if m.Action == "cleared" {
				out = append(out, m.cleared)
			}
		}
		seen[whom] = len(msgs)
		return out
	}
	// ids is the one cleared's names, sorted; more than one cleared fails.
	ids := func(t *testing.T, cs []cleared) []string {
		t.Helper()
		if len(cs) != 1 {
			t.Fatalf("told %+v; want ONE cleared", cs)
		}
		out := slices.Clone(cs[0].NudgeIDs)
		slices.Sort(out)
		return out
	}

	kept := nudge(t, owner, "memory:kept")
	gone := nudge(t, owner, "memory:gone")
	alsoGone := nudge(t, owner, "memory:also-gone")
	mayas := nudge(t, maya, "memory:mayas")

	t.Run("a pass names each nudge whose signal no longer holds, in one cleared", func(t *testing.T) {
		applyUserNudges(ctx, owner, []candidateNudge{{userID: owner, kind: nudgeModels.KindOverdueCommitment,
			title: "Overdue commitment", body: "You committed to it.", sourceType: "memory", priority: 1, dedupKey: "memory:kept"}})
		got := clearedFor(t, owner)
		want := []string{alsoGone, gone}
		slices.Sort(want)
		if !slices.Equal(ids(t, got), want) {
			t.Errorf("the pass told the owner %+v; want one cleared naming each nudge it closed, %v", got, want)
		}
		if got[0].OpenCount != 1 {
			t.Errorf("the cleared says %d open; want 1, the kept one", got[0].OpenCount)
		}
		if c := clearedFor(t, maya); len(c) != 0 {
			t.Errorf("maya was told %+v; want nothing: none of hers closed", c)
		}
	})

	t.Run("a person whose every signal resolved hears each one by name", func(t *testing.T) {
		reconcileResolvedUsers(ctx, map[uuid.UUID][]candidateNudge{maya: nil})
		if got := ids(t, clearedFor(t, owner)); !slices.Equal(got, []string{kept}) {
			t.Errorf("the owner was told %v; want their last open nudge, %s", got, kept)
		}
		if c := clearedFor(t, maya); len(c) != 0 {
			t.Errorf("maya was told %+v; want nothing: she had candidates this pass", c)
		}
	})

	t.Run("a memory item settled closes its nudge by name", func(t *testing.T) {
		var source string
		if err := env.PG.QueryRow(`SELECT source_id FROM workspace_nudges WHERE id = $1`, mayas).Scan(&source); err != nil {
			t.Fatal(err)
		}
		if _, err := env.PG.Exec(`UPDATE workspace_nudges SET dedup_key = 'memory:'||source_id WHERE id = $1`, mayas); err != nil {
			t.Fatal(err)
		}
		ClearNudgeForMemoryItem(ctx, source)
		got := clearedFor(t, maya)
		if len(got) != 1 || !slices.Equal(got[0].NudgeIDs, []string{mayas}) || got[0].OpenCount != 0 {
			t.Errorf("maya was told %+v; want one cleared naming %s, with 0 open", got, mayas)
		}
	})

	// V-581 (C11, CR-B16 M1): the settle at the head of each pass closes the
	// nudges whose commitment no longer needs its person, and names them too.
	t.Run("the settle names each nudge it supersedes, in one cleared per person", func(t *testing.T) {
		lee := uuid.New()
		if _, err := env.PG.Exec(`INSERT INTO users (id, email_id, username, display_name, created_at, updated_at)
			VALUES ($1, $2, 'lee', 'lee', NOW(), NOW())`, lee, lee.String()+"@example.test"); err != nil {
			t.Fatal(err)
		}
		// quietQuestion is whom's urgent open question, last raised 15 days
		// ago: the settle quiets it and never closes it.
		quietQuestion := func(t *testing.T, whom uuid.UUID) {
			t.Helper()
			if _, err := env.PG.Exec(`INSERT INTO workspace_nudges (user_id, kind, title, body, source_type, source_id, status, priority, dedup_key, created_at, updated_at)
				VALUES ($1, $2, 'A question waits', 'Someone asked you.', 'memory', $3, 'open', 1, $4, NOW() - interval '15 days', NOW() - interval '15 days')`,
				whom, nudgeModels.KindStaleQuestion, uuid.NewString(), "memory:q-"+whom.String()); err != nil {
				t.Fatal(err)
			}
		}
		// Each commitment behind these is gone: none needs its person now.
		first, second := nudge(t, owner, "memory:settle-1"), nudge(t, owner, "memory:settle-2")
		hers := nudge(t, maya, "memory:settle-mayas")
		quietQuestion(t, owner)
		quietQuestion(t, lee)

		settleOpenNudges(ctx, time.Now())

		got := clearedFor(t, owner)
		want := []string{first, second}
		slices.Sort(want)
		if !slices.Equal(ids(t, got), want) {
			t.Errorf("the settle told the owner %+v; want one cleared naming each nudge it superseded, %v", got, want)
		}
		if got[0].OpenCount != 1 {
			t.Errorf("the owner's cleared says %d open; want 1, the quiet question", got[0].OpenCount)
		}
		if got := clearedFor(t, maya); !slices.Equal(ids(t, got), []string{hers}) || got[0].OpenCount != 0 {
			t.Errorf("maya was told %+v; want one cleared naming %s, with 0 open", got, hers)
		}
		if got := clearedFor(t, lee); len(got) != 1 || len(got[0].NudgeIDs) != 0 || got[0].OpenCount != 1 {
			t.Errorf("lee, whose question only went quiet, was told %+v; want one cleared naming none, with 1 open", got)
		}
	})
}
