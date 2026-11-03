//go:build integration
// +build integration

package integration_test

// Three things an admin takes away from the workspace's setup are kept, with
// who took them away and when (V-184 head 4b, the owner's rule that every
// delete of a business record is soft).
//
// A custom AI provider, a model on the AI allowlist and a link between a Slack
// channel and a OneCamp channel were DELETEd when an admin removed them, so
// which endpoint and models the workspace used, which models it allowed and
// which channels it bridged went with them. Now each row stays, marked by the
// admin who took it away, and no read sees it: the admin's AI setup and
// allowlist, a member's model picker and the pick they made, the settings
// that named the provider, and the bridge's links. A provider taken away
// takes its key with it (cleared) and its models (marked by the same admin),
// as its foreign keys' CASCADE and SET NULL did. Adding one again makes a new
// row: a provider's label, a model on its provider and each side of a link
// are unique among live rows only.
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

	aiController "github.com/akashc777/OneCamp/controllers/AI"
	slackBridgeController "github.com/akashc777/OneCamp/controllers/SlackBridge"
	"github.com/akashc777/OneCamp/initializers/postgresInit"
	dgraphStruct "github.com/akashc777/OneCamp/models/dgraph"
	slackModel "github.com/akashc777/OneCamp/models/postgres/SlackBridge"
	userModels "github.com/akashc777/OneCamp/models/postgres/User"
	"github.com/akashc777/OneCamp/tests/integration"
)

func TestAdminConfigTakenAwayIsKeptWithWhoAndWhen(t *testing.T) {
	t.Setenv("AI_CONFIG_KEK", "integration-ai-kek-0123456789abcdef")
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
	asAda, asMaya := as("ada"), as("maya")
	// do runs a route as someone and wants 200; it answers the body.
	do := func(t *testing.T, h http.HandlerFunc, method, path string, who *userModels.UserInfo, body string, params map[string]string) []byte {
		t.Helper()
		rec := call(t, h, method, path, who, body, params)
		if rec.Code != http.StatusOK {
			t.Fatalf("%s %s answered %d: %s", method, path, rec.Code, rec.Body)
		}
		return rec.Body.Bytes()
	}
	// refused runs a route that must not go through, because what it names
	// was taken away (or a live row holds its name): anything but 200.
	refused := func(t *testing.T, h http.HandlerFunc, method, path string, who *userModels.UserInfo, body string, params map[string]string) {
		t.Helper()
		if rec := call(t, h, method, path, who, body, params); rec.Code == http.StatusOK {
			t.Fatalf("%s %s answered 200 (%s); want it refused", method, path, rec.Body)
		}
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
	// fingerprint is what the AI setup's reload on every replica watches
	// (business/AI/aiConfigReconcile.go): taking a provider or a model away
	// has to move it, or the other replicas keep serving what was taken away.
	fingerprint := func(t *testing.T) string {
		t.Helper()
		fp, err := postgresInit.RowsFingerprint(ctx, "ai_settings", "ai_providers", "ai_authorized_models")
		if err != nil {
			t.Fatal(err)
		}
		return fp
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
	type setup struct {
		Providers            []listed `json:"providers"`
		VisionProviderID     string   `json:"vision_provider_id"`
		CodePRChatProviderID string   `json:"code_pr_chat_provider_id"`
	}
	aiSetup := func(t *testing.T) setup {
		t.Helper()
		var out struct {
			Data setup `json:"data"`
		}
		decode(t, do(t, aiController.GetAIConfig, "GET", "/admin/ai/config", asAda, "", nil), &out)
		return out.Data
	}
	allowlist := func(t *testing.T) []listed {
		t.Helper()
		var out struct {
			Data []listed `json:"data"`
		}
		decode(t, do(t, aiController.ListAuthorizedModels, "GET", "/admin/ai/authorized-models", asAda, "", nil), &out)
		return out.Data
	}
	// picker is a member's own model picker: what it offers and their pick
	// ("" for the workspace default).
	picker := func(t *testing.T, who *userModels.UserInfo) ([]listed, string) {
		t.Helper()
		var out struct {
			Data struct {
				Models   []listed `json:"models"`
				Selected string   `json:"selected_model_id"`
			} `json:"data"`
		}
		decode(t, do(t, aiController.ListMyModels, "GET", "/ai/models", who, "", nil), &out)
		return out.Data.Models, out.Data.Selected
	}
	addProvider := func(t *testing.T, label, key string) string {
		t.Helper()
		var out struct {
			Data listed `json:"data"`
		}
		decode(t, do(t, aiController.CreateProvider, "POST", "/admin/ai/providers", asAda,
			`{"label":"`+label+`","base_url":"https://models.example.com/v1","api_key":"`+key+`"}`, nil), &out)
		return out.Data.ID
	}
	authorize := func(t *testing.T, providerID, model string) string {
		t.Helper()
		var out struct {
			Data listed `json:"data"`
		}
		decode(t, do(t, aiController.AuthorizeModel, "POST", "/admin/ai/authorized-models", asAda,
			`{"provider_id":"`+providerID+`","model":"`+model+`","label":"`+model+`"}`, nil), &out)
		return out.Data.ID
	}

	t.Run("a custom AI provider, with its key and its models", func(t *testing.T) {
		p := addProvider(t, "Own model", "sk-own")
		m := authorize(t, p, "own-large")
		// The settings name it for images and for code runs (vision has no
		// foreign key; code runs' had ON DELETE SET NULL).
		if _, err := w.env.PG.Exec(`UPDATE ai_settings SET vision_provider_id = $1, vision_model = 'own-vision',
			code_pr_chat_provider_id = $1, code_pr_chat_model = 'own-large' WHERE id = 1`, p); err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() {
			_, _ = w.env.PG.Exec(`UPDATE ai_settings SET vision_provider_id = NULL, vision_model = '',
				code_pr_chat_provider_id = NULL, code_pr_chat_model = '' WHERE id = 1`)
		})
		// Choosing it for images goes through while it is live (its catalog
		// can't be read here, which the check lets pass), so the same write
		// refused after the take-away is refused for the take-away alone.
		do(t, aiController.SetVisionModel, "POST", "/admin/ai/vision-model", asAda, `{"provider_id":"`+p+`","model":"own-vision"}`, nil)
		do(t, aiController.SetMyModel, "POST", "/ai/model-preference", asMaya, `{"model_id":"`+m+`"}`, nil)
		if _, pick := picker(t, asMaya); pick != m {
			t.Fatalf("Maya's pick is %q before the take-away, want %s", pick, m)
		}
		before := fingerprint(t)

		do(t, aiController.DeleteProvider, "DELETE", "/admin/ai/providers/"+p, asAda, "", map[string]string{"providerId": p})

		if by := takenBy(t, "the provider", `ai_providers WHERE id = $1`, p); by != ada {
			t.Fatalf("the provider was taken away by %s, want Ada (%s)", by, ada)
		}
		var keyGone bool
		var label, baseURL string
		if err := w.env.PG.QueryRow(`SELECT api_key_enc IS NULL, label, base_url FROM ai_providers WHERE id = $1`, p).Scan(&keyGone, &label, &baseURL); err != nil ||
			!keyGone || label != "Own model" || baseURL != "https://models.example.com/v1" {
			t.Fatalf("the kept provider: key cleared %v, label %q, endpoint %q (%v); want its key gone and which endpoint it was on record", keyGone, label, baseURL, err)
		}
		if by := takenBy(t, "its model", `ai_authorized_models WHERE id = $1`, m); by != ada {
			t.Fatalf("the provider's model was taken away by %s, want Ada (%s) with the provider", by, ada)
		}
		if fingerprint(t) == before {
			t.Fatal("the AI setup's fingerprint did not move: the other replicas would keep serving the provider taken away")
		}
		s := aiSetup(t)
		if has(s.Providers, p) {
			t.Fatal("the AI setup still lists the provider taken away")
		}
		if s.VisionProviderID != "" || s.CodePRChatProviderID != "" {
			t.Fatalf("the settings still name the provider taken away (images %q, code runs %q); want none, as its SET NULL left them", s.VisionProviderID, s.CodePRChatProviderID)
		}
		if has(allowlist(t), m) {
			t.Fatal("the allowlist still has the model of the provider taken away")
		}
		if models, pick := picker(t, asMaya); pick != "" || has(models, m) {
			t.Fatalf("Maya's picker: pick %q, offers the model %v; want the workspace default, the model gone", pick, has(models, m))
		}
		refused(t, aiController.UpdateProvider, "PATCH", "/admin/ai/providers/"+p, asAda, `{"label":"Renamed"}`, map[string]string{"providerId": p})
		refused(t, aiController.DeleteProvider, "DELETE", "/admin/ai/providers/"+p, asAda, "", map[string]string{"providerId": p})
		refused(t, aiController.AuthorizeModel, "POST", "/admin/ai/authorized-models", asAda, `{"provider_id":"`+p+`","model":"own-small"}`, nil)
		// The settings writes read the provider through GetProvider, which sees
		// live providers only: nothing else stops one from naming a provider
		// taken away (images, code runs, chat and embeddings alike).
		refused(t, aiController.SetVisionModel, "POST", "/admin/ai/vision-model", asAda, `{"provider_id":"`+p+`","model":"own-vision"}`, nil)
		var under int
		if err := w.env.PG.QueryRow(`SELECT count(*) FROM ai_authorized_models WHERE provider_id = $1 AND model = 'own-small'`, p).Scan(&under); err != nil || under != 0 {
			t.Fatalf("%d models authorized under the provider taken away (%v), want none", under, err)
		}

		// Its label again is a new provider; a second live one by that label,
		// in any case, is still refused.
		again := addProvider(t, "Own model", "sk-own-2")
		if again == p {
			t.Fatal("adding the label again brought back the provider taken away, want a new one")
		}
		if !has(aiSetup(t).Providers, again) {
			t.Fatal("the AI setup doesn't list the provider added again")
		}
		refused(t, aiController.CreateProvider, "POST", "/admin/ai/providers", asAda, `{"label":"own MODEL","base_url":"https://models.example.com/v1"}`, nil)

		// A provider with no model on the allowlist: its own mark is all that
		// moves the fingerprint, as no model's mark goes with it.
		bare := addProvider(t, "Bare", "sk-bare")
		before = fingerprint(t)
		do(t, aiController.DeleteProvider, "DELETE", "/admin/ai/providers/"+bare, asAda, "", map[string]string{"providerId": bare})
		if by := takenBy(t, "the provider with no models", `ai_providers WHERE id = $1`, bare); by != ada {
			t.Fatalf("the provider with no models was taken away by %s, want Ada (%s)", by, ada)
		}
		if fingerprint(t) == before {
			t.Fatal("taking away a provider with no models did not move the AI setup's fingerprint: the other replicas would keep serving it")
		}
	})

	t.Run("a model on the allowlist", func(t *testing.T) {
		p := addProvider(t, "Team gateway", "")
		m := authorize(t, p, "gw-small")
		if again := authorize(t, p, "gw-small"); again != m {
			t.Fatalf("authorizing a live model again made %s, want the same entry %s", again, m)
		}
		do(t, aiController.SetMyModel, "POST", "/ai/model-preference", asMaya, `{"model_id":"`+m+`"}`, nil)
		before := fingerprint(t)
		params := map[string]string{"id": m}

		do(t, aiController.RevokeAuthorizedModel, "DELETE", "/admin/ai/authorized-models/"+m, asAda, "", params)

		if by := takenBy(t, "the model", `ai_authorized_models WHERE id = $1`, m); by != ada {
			t.Fatalf("the model was revoked by %s, want Ada (%s)", by, ada)
		}
		if fingerprint(t) == before {
			t.Fatal("the AI setup's fingerprint did not move: the other replicas would keep serving the model revoked")
		}
		if has(allowlist(t), m) {
			t.Fatal("the allowlist still has the model revoked")
		}
		if models, pick := picker(t, asMaya); pick != "" || has(models, m) {
			t.Fatalf("Maya's picker: pick %q, offers the model %v; want the workspace default, the model gone", pick, has(models, m))
		}
		refused(t, aiController.SetMyModel, "POST", "/ai/model-preference", asMaya, `{"model_id":"`+m+`"}`, nil)
		refused(t, aiController.SetAuthorizedModelEnabled, "POST", "/admin/ai/authorized-models/"+m+"/enabled", asAda, `{"enabled":false}`, params)
		refused(t, aiController.SetAuthorizedModelLimits, "POST", "/admin/ai/authorized-models/"+m+"/limits", asAda, `{"context_window_tokens":8192,"max_output_tokens":1024}`, params)
		refused(t, aiController.RevokeAuthorizedModel, "DELETE", "/admin/ai/authorized-models/"+m, asAda, "", params)

		// Authorizing it again is a new entry; the one revoked stays as it was.
		again := authorize(t, p, "gw-small")
		if again == m {
			t.Fatal("authorizing the model again brought back the entry revoked, want a new one")
		}
		if by := takenBy(t, "the entry revoked", `ai_authorized_models WHERE id = $1`, m); by != ada {
			t.Fatalf("authorizing the model again changed the entry revoked: taken away by %s, want Ada (%s)", by, ada)
		}
		if !has(allowlist(t), again) {
			t.Fatal("the allowlist doesn't have the model authorized again")
		}
	})

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
