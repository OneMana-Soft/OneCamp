//go:build integration
// +build integration

package integration_test

// A link between a Slack channel and a OneCamp channel, which an admin takes
// away from the workspace's setup, is kept, with who took it away and when
// (V-184 head 4b, the owner's rule that every delete of a business record is
// soft).
//
// The link was DELETEd when an admin removed it, so which channels the
// workspace bridged went with it. Now the row stays, marked by the admin who
// took it away, and no read sees it: the bridge's links. Linking either side
// again makes a new row: each side of a link is unique among live rows only.
// The edition with AI keeps a custom AI provider and a model on its allowlist
// the same way; their tables, and the migrations the test below runs down and
// up, are shared.
//
// Run: go test -tags=integration ./tests/integration/ -run 'TestAdminConfigTakenAwayIsKeptWithWhoAndWhen|TestAdminConfigSoftDeleteMigrationUpDownUp' -v

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"net/http"
	"os"
	"path/filepath"
	"testing"

	"github.com/google/uuid"

	slackBridgeController "github.com/akashc777/OneCamp/controllers/SlackBridge"
	dgraphStruct "github.com/akashc777/OneCamp/models/dgraph"
	slackModel "github.com/akashc777/OneCamp/models/postgres/SlackBridge"
	userModels "github.com/akashc777/OneCamp/models/postgres/User"
	"github.com/akashc777/OneCamp/tests/integration"
)

func TestAdminConfigTakenAwayIsKeptWithWhoAndWhen(t *testing.T) {
	ctx := context.Background()
	w := newDocsWorld(t, []string{"ada", "maya"}, "ada")
	ada := w.people["ada"]
	as := func(name string) *userModels.UserInfo {
		info := userModels.UserInfo{}
		info.UserPostgresInfo.Id = w.people[name]
		info.UserPostgresInfo.EmailID = w.people[name].String() + "@example.test"
		info.UserDgraphInfo = dgraphStruct.DgraphUser{Uid: w.uids[name], Uuid: w.people[name].String(), UserName: name}
		return &info
	}
	asAda := as("ada")
	// do runs a route as someone and wants 200; it answers the body.
	do := func(t *testing.T, h http.HandlerFunc, method, path string, who *userModels.UserInfo, body string, params map[string]string) []byte {
		t.Helper()
		rec := call(t, h, method, path, who, body, params)
		if rec.Code != http.StatusOK {
			t.Fatalf("%s %s answered %d: %s", method, path, rec.Code, rec.Body)
		}
		return rec.Body.Bytes()
	}
	decode := func(t *testing.T, b []byte, v any) {
		t.Helper()
		if err := json.Unmarshal(b, v); err != nil {
			t.Fatalf("%s: %v", b, err)
		}
	}
	// takenBy is who took the row away, for a row that is kept, marked: the
	// zero id while it is live. A row that is gone fails the test.
	takenBy := func(t *testing.T, what, from string, args ...any) uuid.UUID {
		t.Helper()
		var at sql.NullTime
		var by uuid.NullUUID
		if err := w.env.PG.QueryRow(`SELECT deleted_at, deleted_by FROM `+from, args...).Scan(&at, &by); err != nil {
			t.Fatalf("%s: %v; want the row kept, marked by who took it away", what, err)
		}
		if at.Valid != by.Valid {
			t.Fatalf("%s: deleted_at %v but deleted_by %v: a row taken away says when and by whom", what, at, by)
		}
		return by.UUID
	}
	type listed struct {
		ID    string `json:"id"`
		Label string `json:"label"`
	}
	has := func(rows []listed, id string) bool {
		for _, r := range rows {
			if r.ID == id {
				return true
			}
		}
		return false
	}

	t.Run("a link between a Slack channel and a OneCamp channel", func(t *testing.T) {
		if err := slackModel.SaveBridge(ctx, slackModel.Bridge{TeamID: "T1", TeamName: "Acme", BotTokenEnc: "sealed", SigningSecretEnc: "sealed"}); err != nil {
			t.Fatal(err)
		}
		general, design := uuid.New(), uuid.New()
		link := uuid.New()
		if err := slackModel.CreateLink(ctx, slackModel.Link{ID: link, SlackChannelID: "C1", SlackChannelName: "general", ChannelUUID: general, CreatedBy: &ada}); err != nil {
			t.Fatal(err)
		}
		// A message bridged across the pair: the id map is derived, it goes with the link.
		if _, err := w.env.PG.Exec(`INSERT INTO slack_bridge_messages (slack_channel_id, slack_ts, channel_uuid, origin) VALUES ('C1', '1700000000.000100', $1, 'slack')`, general); err != nil {
			t.Fatal(err)
		}
		links := func(t *testing.T) []listed {
			t.Helper()
			var out struct {
				Data struct {
					Links []listed `json:"links"`
				} `json:"data"`
			}
			decode(t, do(t, slackBridgeController.GetStatus, "GET", "/admin/slack-bridge", asAda, "", nil), &out)
			return out.Data.Links
		}
		if !has(links(t), link.String()) {
			t.Fatal("the bridge doesn't list the link before it is taken away")
		}
		params := map[string]string{"id": link.String()}

		do(t, slackBridgeController.DeleteLink, "DELETE", "/admin/slack-bridge/links/"+link.String(), asAda, "", params)

		if by := takenBy(t, "the link", `slack_bridge_links WHERE id = $1`, link); by != ada {
			t.Fatalf("the link was taken away by %s, want Ada (%s)", by, ada)
		}
		var mapped int
		if err := w.env.PG.QueryRow(`SELECT count(*) FROM slack_bridge_messages WHERE slack_channel_id = 'C1'`).Scan(&mapped); err != nil || mapped != 0 {
			t.Fatalf("%d bridged messages of the pair still mapped (%v), want none", mapped, err)
		}
		if has(links(t), link.String()) {
			t.Fatal("the bridge still lists the link taken away")
		}
		if rec := call(t, slackBridgeController.DeleteLink, "DELETE", "/admin/slack-bridge/links/"+link.String(), asAda, "", params); rec.Code != http.StatusNotFound {
			t.Fatalf("taking the link away again answered %d (%s), want 404", rec.Code, rec.Body)
		}

		// Each side can be linked again, in a new link; a live link still holds both of its sides.
		slackAgain, channelAgain := uuid.New(), uuid.New()
		if err := slackModel.CreateLink(ctx, slackModel.Link{ID: slackAgain, SlackChannelID: "C1", SlackChannelName: "general", ChannelUUID: design, CreatedBy: &ada}); err != nil {
			t.Fatalf("linking Slack's #general again: %v", err)
		}
		if err := slackModel.CreateLink(ctx, slackModel.Link{ID: channelAgain, SlackChannelID: "C2", SlackChannelName: "random", ChannelUUID: general, CreatedBy: &ada}); err != nil {
			t.Fatalf("linking the OneCamp channel again: %v", err)
		}
		if err := slackModel.CreateLink(ctx, slackModel.Link{ID: uuid.New(), SlackChannelID: "C1", ChannelUUID: uuid.New()}); !errors.Is(err, slackModel.ErrLinkTaken) {
			t.Fatalf("a second live link for Slack's #general: %v, want ErrLinkTaken", err)
		}
		if err := slackModel.CreateLink(ctx, slackModel.Link{ID: uuid.New(), SlackChannelID: "C3", ChannelUUID: general}); !errors.Is(err, slackModel.ErrLinkTaken) {
			t.Fatalf("a second live link for the OneCamp channel: %v, want ErrLinkTaken", err)
		}
		if ls := links(t); !has(ls, slackAgain.String()) || !has(ls, channelAgain.String()) || has(ls, link.String()) || len(ls) != 2 {
			t.Fatalf("the bridge lists %v; want the two links made again and not the one taken away", ls)
		}
	})
}

func TestAdminConfigSoftDeleteMigrationUpDownUp(t *testing.T) {
	// The head's six files, in the order they go up.
	names := []string{
		"admin_config_soft_delete",
		"ai_providers_label_live_idx",
		"ai_authorized_models_live_idx",
		"slack_links_slack_channel_live_idx",
		"slack_links_channel_live_idx",
		"admin_config_unique_while_live",
	}
	read := func(name, dir string) string {
		t.Helper()
		files, err := filepath.Glob("../../migrations/*_" + name + "." + dir + ".sql")
		if err != nil || len(files) != 1 {
			t.Fatalf("migration %s's %s file: %v %v", name, dir, files, err)
		}
		b, err := os.ReadFile(files[0])
		if err != nil {
			t.Fatal(err)
		}
		return string(b)
	}
	var up, down []string
	for i := range names {
		up = append(up, read(names[i], "up"))
		down = append(down, read(names[len(names)-1-i], "down"))
	}
	env := integration.SetupEnv(t)
	exec := func(q string, args ...any) {
		t.Helper()
		if _, err := env.PG.Exec(q, args...); err != nil {
			t.Fatalf("%s: %v", q, err)
		}
	}
	ada, kept, gone, channel := uuid.New(), uuid.New(), uuid.New(), uuid.New()
	// In each table one row is live and one taken away under the same unique
	// name, which only the partial uniques allow.
	exec(`INSERT INTO ai_providers (id, kind, label, deleted_at, deleted_by) VALUES ($1, 'openai_compatible', 'Gateway', NULL, NULL), ($2, 'openai_compatible', 'gateway', NOW(), $3)`, kept, gone, ada)
	exec(`INSERT INTO ai_authorized_models (provider_id, model, label, deleted_at, deleted_by) VALUES ($1, 'm', 'kept', NULL, NULL), ($1, 'm', 'gone', NOW(), $2), ($3, 'm', 'gone', NOW(), $2)`, kept, ada, gone)
	exec(`INSERT INTO slack_bridge_links (id, slack_channel_id, channel_uuid, slack_channel_name, deleted_at, deleted_by) VALUES ($1, 'C1', $2, 'kept', NULL, NULL), ($3, 'C1', $2, 'gone', NOW(), $4)`, uuid.New(), channel, uuid.New(), ada)

	// counts reads by key, not by deleted_at, which the down takes away.
	counts := func() (live, marked int) {
		t.Helper()
		for _, c := range []struct {
			q    string
			args []any
		}{
			{`SELECT count(*) FILTER (WHERE id = $1), count(*) FILTER (WHERE id = $2) FROM ai_providers`, []any{kept, gone}},
			{`SELECT count(*) FILTER (WHERE label = 'kept' AND provider_id = $1), count(*) FILTER (WHERE label = 'gone' AND provider_id IN ($1, $2)) FROM ai_authorized_models`, []any{kept, gone}},
			{`SELECT count(*) FILTER (WHERE slack_channel_name = 'kept'), count(*) FILTER (WHERE slack_channel_name = 'gone') FROM slack_bridge_links`, nil},
		} {
			var l, m int
			if err := env.PG.QueryRow(c.q, c.args...).Scan(&l, &m); err != nil {
				t.Fatalf("%s: %v", c.q, err)
			}
			live, marked = live+l, marked+m
		}
		return live, marked
	}
	// indexes counts the full uniques and the partial ones that stand.
	indexes := func() (full, partial int) {
		t.Helper()
		if err := env.PG.QueryRow(`SELECT
			count(*) FILTER (WHERE indexname IN ('uq_ai_providers_label', 'ai_authorized_models_provider_id_model_key', 'slack_bridge_links_slack_channel_id_key', 'slack_bridge_links_channel_uuid_key')),
			count(*) FILTER (WHERE indexname IN ('uq_ai_providers_label_live', 'uq_ai_authorized_models_live', 'uq_slack_bridge_links_slack_channel_live', 'uq_slack_bridge_links_channel_live')
				AND indexdef LIKE '%WHERE (deleted_at IS NULL)')
			FROM pg_indexes WHERE tablename IN ('ai_providers', 'ai_authorized_models', 'slack_bridge_links')`).Scan(&full, &partial); err != nil {
			t.Fatal(err)
		}
		return full, partial
	}
	if full, partial := indexes(); full != 0 || partial != 4 {
		t.Fatalf("after the migrations: %d full uniques (want 0) and %d partial (want 4)", full, partial)
	}
	for i := 0; i < 2; i++ {
		for j, q := range down {
			if _, err := env.PG.Exec(q); err != nil {
				t.Fatalf("round %d, down %s: %v", i, names[len(names)-1-j], err)
			}
		}
		if live, marked := counts(); live != 3 || marked != 0 {
			t.Fatalf("round %d, after the downs: %d live rows (want 3, one a table) and %d taken away (want 0: the down deletes them)", i, live, marked)
		}
		if full, partial := indexes(); full != 4 || partial != 0 {
			t.Fatalf("round %d, after the downs: %d full uniques (want 4) and %d partial (want 0)", i, full, partial)
		}
		for j, q := range up {
			if _, err := env.PG.Exec(q); err != nil {
				t.Fatalf("round %d, up %s: %v", i, names[j], err)
			}
		}
		if live, marked := counts(); live != 3 || marked != 0 {
			t.Fatalf("round %d, after the ups: %d live rows (want 3) and %d taken away (want 0)", i, live, marked)
		}
		if full, partial := indexes(); full != 0 || partial != 4 {
			t.Fatalf("round %d, after the ups: %d full uniques (want 0) and %d partial (want 4)", i, full, partial)
		}
	}
	var columns int
	if err := env.PG.QueryRow(`SELECT count(*) FROM information_schema.columns
		WHERE table_name IN ('ai_providers', 'ai_authorized_models', 'slack_bridge_links')
		AND column_name IN ('deleted_at', 'deleted_by')`).Scan(&columns); err != nil || columns != 6 {
		t.Fatalf("after the last up: %d of the 6 columns (%v)", columns, err)
	}
	// Up again, a name taken away is free and a live one still holds.
	exec(`INSERT INTO ai_providers (id, kind, label, deleted_at, deleted_by) VALUES ($1, 'openai_compatible', 'GATEWAY', NOW(), $2)`, uuid.New(), ada)
	if _, err := env.PG.Exec(`INSERT INTO ai_providers (id, kind, label) VALUES ($1, 'openai_compatible', 'GATEWAY')`, uuid.New()); err == nil {
		t.Fatal("a second live provider labelled Gateway went in; want the live label unique")
	}
}
