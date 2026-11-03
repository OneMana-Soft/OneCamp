//go:build integration
// +build integration

// An integration disconnected is kept, with who disconnected it and when, and
// its credentials go (V-184 head 5b, the owner's rule that every delete of a
// business record is soft).
//
// Disconnecting the org's GitHub or a person's Google Calendar (unlinked from
// the calendar or disconnected from the connectors page, the same row)
// DELETEd the integration's row. Now the row stays, marked by who disconnected
// it, with both tokens, the sync token, the webhook address and the token's
// expiry cleared, and nothing reads or writes it: the integration's lookup,
// the token refresh and the task-sync switch. Connecting again makes a new
// row, and the one kept stays as it was disconnected. The admin audit log
// records who disconnected the org's GitHub.
//
// Run: go test -tags=integration ./tests/integration/ -run 'TestAnIntegrationDisconnectedIsKeptWithWhoAndWhen|TestIntegrationsSoftDeleteMigrationUpDownUp' -v
package integration_test

import (
	"context"
	"database/sql"
	"fmt"
	"net/http"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/google/uuid"

	connectorController "github.com/akashc777/OneCamp/controllers/Connector"
	githubController "github.com/akashc777/OneCamp/controllers/GitHub"
	integrationController "github.com/akashc777/OneCamp/controllers/Integration"
	integrationDomain "github.com/akashc777/OneCamp/domain/Integration"
	dgraphStruct "github.com/akashc777/OneCamp/models/dgraph"
	userModels "github.com/akashc777/OneCamp/models/postgres/User"
	"github.com/akashc777/OneCamp/tests/integration"
)

func TestAnIntegrationDisconnectedIsKeptWithWhoAndWhen(t *testing.T) {
	ctx := context.Background()
	w := newDocsWorld(t, []string{"ada", "maya"}, "ada")
	as := func(name string) *userModels.UserInfo {
		info := userModels.UserInfo{}
		info.UserPostgresInfo.Id = w.people[name]
		info.UserPostgresInfo.EmailID = w.people[name].String() + "@example.test"
		info.UserDgraphInfo = dgraphStruct.DgraphUser{Uid: w.uids[name], Uuid: w.people[name].String(), UserName: name}
		return &info
	}
	asAda, asMaya := as("ada"), as("maya")
	str := func(s string) *string { return &s }
	exp := time.Now().Add(time.Hour)

	type row struct {
		at, expires                       sql.NullTime
		by                                uuid.NullUUID
		access, refresh, sync, hook, meta sql.NullString
		taskSync                          bool
	}
	// rows reads every row an integration has, the live ones and the kept.
	rows := func(t *testing.T, entityType string, entityID uuid.UUID, provider string) (live []row, kept []row) {
		t.Helper()
		rs, err := w.env.PG.Query(`SELECT deleted_at, expires_at, deleted_by, access_token, refresh_token, sync_token, webhook_url, metadata::text, task_sync_enabled
			FROM integrations WHERE entity_type = $1 AND entity_id = $2 AND provider = $3 ORDER BY created_at, id`, entityType, entityID, provider)
		if err != nil {
			t.Fatal(err)
		}
		defer rs.Close()
		for rs.Next() {
			var r row
			if err := rs.Scan(&r.at, &r.expires, &r.by, &r.access, &r.refresh, &r.sync, &r.hook, &r.meta, &r.taskSync); err != nil {
				t.Fatal(err)
			}
			if r.at.Valid {
				kept = append(kept, r)
			} else {
				live = append(live, r)
			}
		}
		if err := rs.Err(); err != nil {
			t.Fatal(err)
		}
		return live, kept
	}
	cleared := func(t *testing.T, r row, by uuid.UUID) {
		t.Helper()
		if !r.by.Valid || r.by.UUID != by {
			t.Fatalf("the kept integration is marked by %v; want %s", r.by, by)
		}
		if r.access.Valid || r.refresh.Valid || r.sync.Valid || r.hook.Valid || r.expires.Valid {
			t.Fatalf("the kept integration still holds credentials (access %v, refresh %v, sync %v, webhook %v, expiry %v); want them gone",
				r.access.Valid, r.refresh.Valid, r.sync.Valid, r.hook.Valid, r.expires.Valid)
		}
	}

	t.Run("the org's GitHub, disconnected by an admin, is kept, marked by them, its tokens gone, and nothing reads or writes it", func(t *testing.T) {
		if err := integrationDomain.UpsertIntegration(ctx, "org", uuid.Nil, "github", str("gho-1"), str("ghr-1"), str("https://hooks.example.test/1"), nil, &exp); err != nil {
			t.Fatal(err)
		}
		if _, err := w.env.PG.Exec(`UPDATE integrations SET sync_token = 'sync-1' WHERE entity_type = 'org' AND provider = 'github'`); err != nil {
			t.Fatal(err)
		}
		// Maya's GitHub identity: a person's row, which the org's disconnect leaves alone.
		mayaMeta := fmt.Sprintf(`{"github_login":"maya-gh","user_uuid":%q}`, w.people["maya"].String())
		if err := integrationDomain.UpsertIntegration(ctx, "user", w.people["maya"], "github", nil, nil, nil, &mayaMeta, nil); err != nil {
			t.Fatal(err)
		}

		if rec := call(t, githubController.HandleDisconnect, http.MethodPost, "/admin/github/disconnect", asAda, "", nil); rec.Code != http.StatusOK {
			t.Fatalf("disconnecting GitHub answered %d: %s", rec.Code, rec.Body.String())
		}
		live, kept := rows(t, "org", uuid.Nil, "github")
		if len(live) != 0 || len(kept) != 1 {
			t.Fatalf("after the disconnect: %d live, %d kept; want 0 and 1", len(live), len(kept))
		}
		cleared(t, kept[0], w.people["ada"])
		// The admin audit log says who disconnected it.
		auditedBy(t, w.env.PG, "integration.github_disconnect", w.people["ada"])
		if got, err := integrationDomain.GetIntegration(ctx, "org", uuid.Nil, "github"); err != nil || got != nil {
			t.Fatalf("the integration's lookup still finds the disconnected GitHub: %+v (%v)", got, err)
		}
		// A late token refresh and the task-sync switch leave the kept row as it is.
		if err := integrationDomain.UpdateIntegrationToken(ctx, "org", uuid.Nil, "github", str("gho-late"), str("ghr-late"), &exp); err != nil {
			t.Fatal(err)
		}
		if err := integrationDomain.UpdateTaskSyncEnabled(ctx, "org", uuid.Nil, "github", true); err != nil {
			t.Fatal(err)
		}
		live, kept = rows(t, "org", uuid.Nil, "github")
		if len(live) != 0 || len(kept) != 1 || kept[0].taskSync {
			t.Fatalf("after a late refresh and the sync switch: %d live, %d kept (%+v); want the kept row untouched", len(live), len(kept), kept)
		}
		cleared(t, kept[0], w.people["ada"])
		if m, err := integrationDomain.GetIntegrationMetadataByGitHubLogin(ctx, "maya-gh"); err != nil || m == nil {
			t.Fatalf("Maya's GitHub identity is gone after the org's disconnect: %v (%v)", m, err)
		}
		if m, err := integrationDomain.GetIntegrationMetadataByEntityID(ctx, w.people["maya"]); err != nil || m == nil {
			t.Fatalf("Maya's GitHub identity isn't found by her id after the org's disconnect: %v (%v)", m, err)
		}

		// Connected again, it's a new row; the kept one stays as it was disconnected.
		if err := integrationDomain.UpsertIntegration(ctx, "org", uuid.Nil, "github", str("gho-2"), nil, nil, nil, nil); err != nil {
			t.Fatal(err)
		}
		live, kept = rows(t, "org", uuid.Nil, "github")
		if len(live) != 1 || len(kept) != 1 {
			t.Fatalf("after connecting again: %d live, %d kept; want 1 and 1", len(live), len(kept))
		}
		cleared(t, kept[0], w.people["ada"])
		got, err := integrationDomain.GetIntegration(ctx, "org", uuid.Nil, "github")
		if err != nil || got == nil || got.AccessToken == nil || *got.AccessToken != "gho-2" || got.RefreshToken != nil {
			t.Fatalf("the GitHub connected again reads %+v (%v); want the new token and no refresh token carried from the kept row", got, err)
		}
	})

	t.Run("a person's Google Calendar, unlinked, is kept and marked by them, and another person's stays connected", func(t *testing.T) {
		for _, name := range []string{"ada", "maya"} {
			if err := integrationDomain.UpsertIntegration(ctx, "user", w.people[name], "google_calendar", str("ya29-"+name), str("1//r-"+name), nil, str(`{"calendar":"primary"}`), &exp); err != nil {
				t.Fatal(err)
			}
		}
		if rec := call(t, integrationController.UnlinkGoogleCalendar, http.MethodPost, "/integrations/google-calendar/unlink", asMaya, "", nil); rec.Code != http.StatusOK {
			t.Fatalf("unlinking Maya's Google Calendar answered %d: %s", rec.Code, rec.Body.String())
		}
		live, kept := rows(t, "user", w.people["maya"], "google_calendar")
		if len(live) != 0 || len(kept) != 1 {
			t.Fatalf("after Maya's unlink: %d live, %d kept; want 0 and 1", len(live), len(kept))
		}
		cleared(t, kept[0], w.people["maya"])
		if !kept[0].meta.Valid {
			t.Fatal("the kept integration lost its metadata; want what was connected kept")
		}
		if got, err := integrationDomain.GetIntegration(ctx, "user", w.people["maya"], "google_calendar"); err != nil || got != nil {
			t.Fatalf("the lookup still finds Maya's unlinked Google Calendar: %+v (%v)", got, err)
		}
		if live, kept := rows(t, "user", w.people["ada"], "google_calendar"); len(live) != 1 || len(kept) != 0 || !live[0].access.Valid {
			t.Fatalf("Ada's Google Calendar: %d live, %d kept; want hers connected, untouched", len(live), len(kept))
		}

		// Unlinking again marks nothing more: the kept row keeps its time.
		at := kept[0].at.Time
		if rec := call(t, integrationController.UnlinkGoogleCalendar, http.MethodPost, "/integrations/google-calendar/unlink", asMaya, "", nil); rec.Code != http.StatusOK {
			t.Fatalf("unlinking again answered %d: %s", rec.Code, rec.Body.String())
		}
		if live, kept := rows(t, "user", w.people["maya"], "google_calendar"); len(live) != 0 || len(kept) != 1 || !kept[0].at.Time.Equal(at) {
			t.Fatalf("after unlinking again: %d live, %d kept; want the one kept, marked when it was first unlinked", len(live), len(kept))
		}
	})

	t.Run("a person's Google Calendar disconnected from the connectors page is kept the same way, beside the ones kept before", func(t *testing.T) {
		// The connectors page's Google Calendar is the calendar's own row
		// (provider google_calendar): its disconnect keeps it as the unlink
		// does, and leaves every row kept before as it was.
		maya := w.people["maya"]
		_, before := rows(t, "user", maya, "google_calendar")
		if err := integrationDomain.UpsertIntegration(ctx, "user", maya, "google_calendar", str("ya29-maya-2"), str("1//r-maya-2"), nil, str(`{"calendar":"primary"}`), &exp); err != nil {
			t.Fatal(err)
		}
		if rec := call(t, connectorController.Disconnect, http.MethodPost, "/connectors/google_calendar/disconnect", asMaya, "", map[string]string{"provider": "google_calendar"}); rec.Code != http.StatusOK {
			t.Fatalf("disconnecting Maya's Google Calendar from the connectors page answered %d: %s", rec.Code, rec.Body.String())
		}
		live, kept := rows(t, "user", maya, "google_calendar")
		if len(live) != 0 || len(kept) != len(before)+1 {
			t.Fatalf("after the connectors page's disconnect: %d live, %d kept; want 0 and %d", len(live), len(kept), len(before)+1)
		}
		for _, k := range kept {
			cleared(t, k, maya)
		}
		for _, b := range before {
			found := false
			for _, k := range kept {
				found = found || k.at.Time.Equal(b.at.Time)
			}
			if !found {
				t.Fatalf("the Google Calendar kept at %v is gone after the connectors page's disconnect; want it kept as it was", b.at.Time)
			}
		}
		if live, _ := rows(t, "user", w.people["ada"], "google_calendar"); len(live) != 1 || !live[0].access.Valid {
			t.Fatalf("Ada's Google Calendar: %d live; want hers connected, untouched", len(live))
		}
	})

	t.Run("a person's GitHub identity isn't the connectors page's to disconnect, and once disconnected neither GitHub lookup finds it", func(t *testing.T) {
		// Maya's GitHub identity (provider github) is what githubSync reads to
		// credit her work. The connectors page lists github_connector, never
		// github, so a hand-made disconnect of github, or of any name it
		// doesn't list, answers 404 and changes nothing (V-281).
		maya := w.people["maya"]
		mayaMeta := fmt.Sprintf(`{"github_login":"maya-gh","user_uuid":%q}`, maya.String())
		if err := integrationDomain.UpsertIntegration(ctx, "user", maya, "github", nil, nil, nil, &mayaMeta, nil); err != nil {
			t.Fatal(err)
		}
		for _, provider := range []string{"github", "nope"} {
			rec := call(t, connectorController.Disconnect, http.MethodPost, "/connectors/"+provider+"/disconnect", asMaya, "", map[string]string{"provider": provider})
			if rec.Code != http.StatusNotFound {
				t.Fatalf("disconnecting %q from the connectors page answered %d: %s; want 404", provider, rec.Code, rec.Body.String())
			}
		}
		if live, kept := rows(t, "user", maya, "github"); len(live) != 1 || len(kept) != 0 {
			t.Fatalf("Maya's GitHub identity after the refused disconnects: %d live, %d kept; want 1 and 0", len(live), len(kept))
		}
		if m, err := integrationDomain.GetIntegrationMetadataByGitHubLogin(ctx, "maya-gh"); err != nil || m == nil {
			t.Fatalf("Maya's GitHub identity isn't found by her login after the refused disconnects: %v (%v)", m, err)
		}

		// Disconnected (kept, marked by her), neither lookup finds it, so
		// GitHub activity is no longer credited to her.
		if err := integrationDomain.DeleteIntegration(ctx, "user", maya, "github", maya); err != nil {
			t.Fatal(err)
		}
		live, kept := rows(t, "user", maya, "github")
		if len(live) != 0 || len(kept) != 1 || !kept[0].meta.Valid {
			t.Fatalf("after disconnecting Maya's GitHub identity: %d live, %d kept; want 0 and 1, its metadata kept", len(live), len(kept))
		}
		cleared(t, kept[0], maya)
		if m, err := integrationDomain.GetIntegrationMetadataByGitHubLogin(ctx, "maya-gh"); err != nil || m != nil {
			t.Fatalf("the GitHub lookup by login finds Maya's disconnected identity: %v (%v); want nothing", m, err)
		}
		if m, err := integrationDomain.GetIntegrationMetadataByEntityID(ctx, maya); err != nil || m != nil {
			t.Fatalf("the GitHub lookup by id finds Maya's disconnected identity: %v (%v); want nothing", m, err)
		}
	})
}

// auditedBy waits until the admin audit log holds one row of action recorded
// for who, filed under integrations.
func auditedBy(t *testing.T, db *sql.DB, action string, who uuid.UUID) {
	t.Helper()
	for deadline := time.Now().Add(10 * time.Second); ; time.Sleep(100 * time.Millisecond) {
		var n int
		var category string
		if err := db.QueryRow(`SELECT COUNT(*), COALESCE(MIN(category), '') FROM admin_audit_log WHERE action = $1 AND actor_id = $2`, action, who).Scan(&n, &category); err != nil {
			t.Fatal(err)
		}
		switch {
		case n > 1:
			t.Fatalf("the admin audit log has %d %s rows for %s; want one", n, action, who)
		case n == 1 && category != "integration":
			t.Fatalf("%s is filed under %q; want integration", action, category)
		case n == 1:
			return
		case time.Now().After(deadline):
			t.Fatalf("the admin audit log has no %s row for %s; want the disconnect recorded", action, who)
		}
	}
}

func TestIntegrationsSoftDeleteMigrationUpDownUp(t *testing.T) {
	// The head's three files, in the order they go up.
	names := []string{"integrations_soft_delete", "integrations_live_idx", "integrations_unique_while_live"}
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
	sam := uuid.New()
	// One integration connected and one disconnected under the same key, which
	// only the partial unique allows.
	exec(`INSERT INTO integrations (entity_type, entity_id, provider, access_token, metadata, deleted_at, deleted_by) VALUES
		('user', $1, 'google_calendar', 'live', '{"kept":true}', NULL, NULL), ('user', $1, 'google_calendar', NULL, '{"kept":false}', NOW(), $1)`, sam)
	counts := func() (live, marked int) {
		t.Helper()
		if err := env.PG.QueryRow(`SELECT count(*) FILTER (WHERE metadata->>'kept' = 'true'), count(*) FILTER (WHERE metadata->>'kept' = 'false') FROM integrations WHERE entity_id = $1`, sam).Scan(&live, &marked); err != nil {
			t.Fatal(err)
		}
		return live, marked
	}
	fullUnique := func() bool {
		t.Helper()
		var n int
		if err := env.PG.QueryRow(`SELECT count(*) FROM pg_constraint WHERE conname = 'integrations_entity_type_entity_id_provider_key'`).Scan(&n); err != nil {
			t.Fatal(err)
		}
		return n == 1
	}
	if fullUnique() {
		t.Fatal("at head, the full unique on (entity_type, entity_id, provider) is still there")
	}
	for _, q := range down {
		exec(q)
	}
	if live, marked := counts(); live != 1 || marked != 0 {
		t.Fatalf("after the downs: %d live, %d marked; want the connected integration kept and the disconnected one gone", live, marked)
	}
	if !fullUnique() {
		t.Fatal("after the downs, the full unique on (entity_type, entity_id, provider) isn't back")
	}
	var cols int
	if err := env.PG.QueryRow(`SELECT count(*) FROM information_schema.columns WHERE table_name = 'integrations' AND column_name IN ('deleted_at', 'deleted_by')`).Scan(&cols); err != nil || cols != 0 {
		t.Fatalf("after the downs integrations has %d of its mark columns (%v); want none", cols, err)
	}
	for _, q := range up {
		exec(q)
	}
	if fullUnique() {
		t.Fatal("after the ups again, the full unique on (entity_type, entity_id, provider) is still there")
	}
	exec(`INSERT INTO integrations (entity_type, entity_id, provider, metadata, deleted_at, deleted_by) VALUES ('user', $1, 'google_calendar', '{"kept":false}', NOW(), $1)`, sam)
	if _, err := env.PG.Exec(`INSERT INTO integrations (entity_type, entity_id, provider) VALUES ('user', $1, 'google_calendar')`, sam); err == nil {
		t.Fatal("after the ups again, a second connected Google Calendar went in for one person; want the partial unique to refuse it")
	}
	if live, marked := counts(); live != 1 || marked != 1 {
		t.Fatalf("after the ups again: %d live, %d marked; want 1 and 1", live, marked)
	}
}
