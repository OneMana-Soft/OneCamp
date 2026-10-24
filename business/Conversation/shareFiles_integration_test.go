//go:build integration

package business

import (
	"context"
	"slices"
	"testing"
	"time"

	fileAccessBusiness "github.com/akashc777/OneCamp/business/FileAccess"
	"github.com/akashc777/OneCamp/helpers"
	"github.com/akashc777/OneCamp/initializers/postgresInit"
	dgraphStruct "github.com/akashc777/OneCamp/models/dgraph"
	"github.com/akashc777/OneCamp/tests/integration"
	"github.com/google/uuid"
)

// A share copies a message's files only when they are files of the
// conversation it copies from, each on the object its row there names. B's
// DM with C holds B's message listing B's own upload to the DM under another
// key (A's), and A's file from A's private channel by its id and key; C's
// reply lists A's file and C's own upload. Sharing the DM into a group with D
// gives the group a file row for each file the copies keep: D, who was never
// in the DM, opens B's and C's files through the group, and A's through none.
func TestAShareCopiesOnlyTheFilesOfTheConversationItCopies(t *testing.T) {
	ctx := context.Background()
	env := integration.SetupEnv(t)
	if err := postgresInit.ConnectPostgres(ctx, env.DSN); err != nil {
		t.Fatal(err)
	}
	integration.SetupRedis(t)
	integration.StubMqttClient(t)
	integration.SetupOpenSearch(t)
	dg := integration.SetupDgraph(t)

	a, b, c, d, private := uuid.New(), uuid.New(), uuid.New(), uuid.New(), uuid.New()
	for i, u := range []uuid.UUID{a, b, c, d} {
		if _, err := env.PG.Exec(`INSERT INTO users (id, email_id) VALUES ($1, $2)`, u, []string{"a", "b", "c", "d"}[i]+"@example.test"); err != nil {
			t.Fatal(err)
		}
	}
	dm := helpers.GetGroupingId(b.String(), c.String())
	group, err := helpers.GenerateGroupID([]string{b.String(), c.String(), d.String()})
	if err != nil {
		t.Fatal(err)
	}
	conv := uuid.New()
	if _, err := env.PG.Exec(`INSERT INTO conversations (id, kind, ref) VALUES ($1, 'group', $2)`, conv, group); err != nil {
		t.Fatal(err)
	}
	file := func(by uuid.UUID, srcKey, srcValue, key string) uuid.UUID {
		id := uuid.New()
		if _, err := env.PG.Exec(`INSERT INTO attachments (id, obj_key, src_key, src_value, created_by) VALUES ($1, $2, $3, $4, $5)`, id, key, srcKey, srcValue, by); err != nil {
			t.Fatal(err)
		}
		return id
	}
	salaries := a.String() + "/salaries.xlsx"
	theirs := file(a, "channel", private.String(), salaries)
	bees := file(b, "chat", dm, b.String()+"/plan.png")
	cees := file(c, "chat", dm, c.String()+"/notes.pdf")

	const live = "0001-01-01T00:00:00Z"
	at := time.Date(2026, 10, 1, 10, 0, 0, 0, time.UTC)
	entry := func(id uuid.UUID, key string) map[string]any {
		return map[string]any{"dgraph.type": "Attachment", "attachment_uuid": id.String(), "attachment_obj_key": key, "attachment_file_name": "file"}
	}
	person := func(name string, id uuid.UUID) map[string]any {
		return map[string]any{"uid": "_:" + name, "dgraph.type": "User", "user_uuid": id.String(), "user_name": name, "user_deleted_at": live}
	}
	uids := dg.Mutate(t, []map[string]any{
		person("a", a), person("b", b), person("c", c), person("d", d),
		{"uid": "_:dm", "dgraph.type": "Dm", "dm_grouping_id": dm, "dm_participants": []map[string]any{{"uid": "_:b"}, {"uid": "_:c"}},
			"dm_chats": []map[string]any{{"dgraph.type": "Chat", "chat_uuid": uuid.NewString(), "chat_body_text": "<p>the plan</p>",
				"chat_created_at": at.Format(time.RFC3339), "chat_deleted_at": live,
				"chat_from": map[string]any{"uid": "_:b"}, "chat_dm": map[string]any{"uid": "_:dm"},
				"chat_attachments": []map[string]any{entry(bees, salaries), entry(theirs, salaries)},
				"chat_comments": []map[string]any{{"dgraph.type": "Comment", "comment_uuid": uuid.NewString(), "comment_text": "<p>and mine</p>",
					"comment_created_at": at.Add(time.Minute).Format(time.RFC3339), "comment_deleted_at": live,
					"comment_by":          map[string]any{"uid": "_:c"},
					"comment_attachments": []map[string]any{entry(theirs, salaries), entry(cees, c.String()+"/notes.pdf")}}},
			}}},
		{"uid": "_:group", "dgraph.type": "Dm", "dm_grouping_id": group,
			"dm_participants": []map[string]any{{"uid": "_:b"}, {"uid": "_:c"}, {"uid": "_:d"}}},
	})

	people := []*dgraphStruct.DgraphUser{{Uid: uids["b"], Uuid: b.String()}, {Uid: uids["c"], Uuid: c.String()}, {Uid: uids["d"], Uuid: d.String()}}
	origTarget, origFinish := targetOf, shareFinish
	t.Cleanup(func() { targetOf, shareFinish = origTarget, origFinish })
	targetOf = func(context.Context, sharePayload) (copyTarget, error) {
		return &groupTarget{conv: conv, groupingID: group, people: people}, nil
	}
	shareFinish = func(context.Context, copyTarget, sharePayload) error { return nil }

	if err := shareRun(ctx, sharePayload{From: dm, ToKind: KindGroup, To: group, By: b.String(), Until: time.Now().Add(time.Hour), Cap: shareCap}); err != nil {
		t.Fatal(err)
	}

	rows, err := env.PG.Query(`SELECT id, obj_key FROM attachments WHERE src_key = 'grpChat' AND src_value = $1`, group)
	if err != nil {
		t.Fatal(err)
	}
	defer rows.Close()
	var keys []string
	for rows.Next() {
		var id uuid.UUID
		var key string
		if err := rows.Scan(&id, &key); err != nil {
			t.Fatal(err)
		}
		keys = append(keys, key)
		got, err := fileAccessBusiness.ReadFile(ctx, d.String(), fileAccessBusiness.ConversationOf(group), id.String())
		if err != nil {
			t.Errorf("D can't open the group's file %s (%s): %v", id, key, err)
		} else if got.ObjKey == salaries {
			t.Errorf("D opens A's file through the group's file %s", id)
		}
	}
	if err := rows.Err(); err != nil {
		t.Fatal(err)
	}
	want := []string{b.String() + "/plan.png", c.String() + "/notes.pdf"}
	slices.Sort(keys)
	slices.Sort(want)
	if !slices.Equal(keys, want) {
		t.Errorf("the group's files are on %v; want B's and C's own uploads to the DM: %v", keys, want)
	}
}
