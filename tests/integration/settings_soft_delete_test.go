//go:build integration
// +build integration

package integration_test

// Five settings taken away are kept, with who took them away and when (V-184
// head 4a, the owner's rule that every delete of a business record is soft).
//
// A project's status rule, its billing and a person's own rate on it, a
// channel's AI memory exclusion and a doc's consent to workspace exports were
// DELETEd when an admin, or the doc's owner, took them away, so who had set
// them, who took them away and when went with them. Now each row stays,
// marked by whoever took it away, and no read sees it: the project's rules
// and the rule a task move checks, the rates a report bills by, whether the
// AI may remember the channel, and whether an export takes the doc. Setting
// it again brings the row back.
//
// Run: go test -tags=integration ./tests/integration/ -run 'TestSettingsTakenAwayAreKeptWithWhoAndWhen|TestSettingsSoftDeleteMigrationUpDownUp' -v

import (
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/google/uuid"

	aiController "github.com/akashc777/OneCamp/controllers/AI"
	approvalsController "github.com/akashc777/OneCamp/controllers/Approvals"
	contentIOController "github.com/akashc777/OneCamp/controllers/ContentIO"
	timeEntryController "github.com/akashc777/OneCamp/controllers/TimeEntry"
	dgraphStruct "github.com/akashc777/OneCamp/models/dgraph"
	approvalsModel "github.com/akashc777/OneCamp/models/postgres/Approvals"
	docExportModels "github.com/akashc777/OneCamp/models/postgres/DocExport"
	rateModel "github.com/akashc777/OneCamp/models/postgres/ProjectRate"
	userModels "github.com/akashc777/OneCamp/models/postgres/User"
	"github.com/akashc777/OneCamp/tests/integration"
)

func TestSettingsTakenAwayAreKeptWithWhoAndWhen(t *testing.T) {
	ctx := context.Background()
	w := newDocsWorld(t, []string{"ada", "maya"}, "ada")
	const live = "0001-01-01T00:00:00Z"
	project, channel := uuid.New(), uuid.NewString()
	ada, maya := w.people["ada"], w.people["maya"]
	// Ada is the project's admin and #launch's moderator; Maya is a member of both.
	w.dg.Mutate(t, []map[string]any{
		{"uid": w.uids["ada"], "user_projects": []map[string]any{{"uid": "_:P"}}},
		{"uid": w.uids["maya"], "user_projects": []map[string]any{{"uid": "_:P"}}},
		{"uid": "_:P", "dgraph.type": "Project", "project_uuid": project.String(), "project_name": "Billed", "project_deleted_at": live,
			"project_members": []map[string]any{{"uid": w.uids["ada"]}, {"uid": w.uids["maya"]}}, "project_admins": []map[string]any{{"uid": w.uids["ada"]}}},
		{"uid": "_:ch", "dgraph.type": "Channel", "ch_uuid": channel, "ch_name": "launch", "ch_deleted_at": live,
			"ch_members": []map[string]any{{"uid": w.uids["ada"]}, {"uid": w.uids["maya"]}}, "ch_moderators": []map[string]any{{"uid": w.uids["ada"]}}},
	})
	as := func(name string) *userModels.UserInfo {
		info := userModels.UserInfo{}
		info.UserPostgresInfo.Id = w.people[name]
		info.UserPostgresInfo.EmailID = w.people[name].String() + "@example.test"
		info.UserDgraphInfo = dgraphStruct.DgraphUser{Uid: w.uids[name], Uuid: w.people[name].String(), UserName: name}
		return &info
	}
	asAda, asMaya := as("ada"), as("maya")
	params := map[string]string{"project_uuid": project.String()}
	// do runs a route as someone and wants 200; it answers the body.
	do := func(t *testing.T, h http.HandlerFunc, method, path string, who *userModels.UserInfo, body string) []byte {
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

	t.Run("a status rule", func(t *testing.T) {
		rule := func() uuid.UUID {
			return takenBy(t, "the rule on done", `status_rules WHERE project_id = $1 AND status = 'done'`, project)
		}
		rules := func() int {
			t.Helper()
			var got struct {
				Data struct {
					Rules []json.RawMessage `json:"rules"`
				} `json:"data"`
			}
			decode(t, do(t, approvalsController.ListStatusRules, http.MethodGet, "/project/"+project.String()+"/statusRules", asAda, ""), &got)
			moving, err := approvalsModel.RulesFor(ctx, project, []string{"done"})
			n, cerr := approvalsModel.CountRules(ctx, project)
			if err != nil || cerr != nil || len(moving) != len(got.Data.Rules) || n != len(got.Data.Rules) {
				t.Fatalf("the project's rules: listed %d, a move into done checks %d (%v), counted %d (%v)", len(got.Data.Rules), len(moving), err, n, cerr)
			}
			return n
		}
		save := func(who string) {
			do(t, approvalsController.SaveStatusRule, http.MethodPost, "/project/"+project.String()+"/statusRules", asAda, `{"status":"done","who":"`+who+`"}`)
		}
		save("admins")
		do(t, approvalsController.DeleteStatusRule, http.MethodPost, "/project/"+project.String()+"/statusRules/delete", asAda, `{"status":"done"}`)
		if by := rule(); by != ada {
			t.Fatalf("the removed rule is marked by %v, want Ada %v", by, ada)
		}
		if n := rules(); n != 0 {
			t.Fatalf("a removed rule still counts: %d", n)
		}
		save("admins")
		if by, n := rule(), rules(); by != uuid.Nil || n != 1 {
			t.Fatalf("setting the rule again: marked by %v, %d rules; want the row back, live", by, n)
		}
		// A rule saved to say nothing (anyone, nothing required) is taken away too.
		save("anyone")
		if by, n := rule(), rules(); by != ada || n != 0 {
			t.Fatalf("a rule saved to say nothing: marked by %v, %d rules; want it kept, marked by Ada", by, n)
		}
	})

	t.Run("a project's billing and a person's own rate", func(t *testing.T) {
		path := "/project/" + project.String() + "/rates"
		type view struct {
			Data struct {
				Set       bool   `json:"set"`
				Currency  string `json:"currency"`
				UpdatedAt string `json:"updated_at"`
				People    []struct {
					UserUUID  string `json:"user_uuid"`
					RateCents int64  `json:"rate_cents"`
				} `json:"people"`
			} `json:"data"`
		}
		read := func() (v view) {
			decode(t, do(t, timeEntryController.ProjectRates, http.MethodGet, path, asAda, ""), &v)
			return v
		}
		save := func(currency string, mayaCents int64, version string) (int, view) {
			t.Helper()
			people := "[]"
			if mayaCents > 0 {
				people = fmt.Sprintf(`[{"user_uuid":%q,"rate_cents":%d}]`, maya, mayaCents)
			}
			rec := call(t, timeEntryController.SetProjectRates, http.MethodPost, path, asAda,
				fmt.Sprintf(`{"currency":%q,"default_rate_cents":8500,"people":%s,"updated_at":%q}`, currency, people, version), params)
			var v view
			if rec.Code == http.StatusOK {
				decode(t, rec.Body.Bytes(), &v)
			}
			return rec.Code, v
		}
		billing := func() uuid.UUID {
			return takenBy(t, "the project's billing", `project_billing WHERE project_uuid = $1`, project)
		}
		mayasRate := func() uuid.UUID {
			return takenBy(t, "Maya's own rate", `project_person_rates WHERE project_uuid = $1 AND user_uuid = $2`, project, maya)
		}

		code, v := save("EUR", 9500, "")
		if code != http.StatusOK || len(v.Data.People) != 1 {
			t.Fatalf("the first save: %d %+v", code, v)
		}
		// Ada takes Maya's own rate away: the row stays, marked by her.
		if code, v = save("EUR", 0, v.Data.UpdatedAt); code != http.StatusOK || len(v.Data.People) != 0 {
			t.Fatalf("taking Maya's rate away: %d %+v", code, v)
		}
		if by := mayasRate(); by != ada {
			t.Fatalf("Maya's removed rate is marked by %v, want Ada %v", by, ada)
		}
		// The rates read (and every report, through rateModel.Get) no longer
		// has her: a save's answer is only what was sent.
		if now := read(); !now.Data.Set || len(now.Data.People) != 0 {
			t.Fatalf("a rate taken away is still read: %+v", now)
		}
		if code, v = save("EUR", 9600, v.Data.UpdatedAt); code != http.StatusOK || len(v.Data.People) != 1 || v.Data.People[0].RateCents != 9600 || mayasRate() != uuid.Nil {
			t.Fatalf("giving Maya a rate again: %d %+v; want her row back, live", code, v)
		}
		before := v.Data.UpdatedAt

		// Ada stops billing the project: its billing and Maya's rate stay, marked.
		do(t, timeEntryController.ClearProjectRates, http.MethodPost, path+"/delete", asAda, "")
		if b, r := billing(), mayasRate(); b != ada || r != ada {
			t.Fatalf("the cleared billing is marked by %v and Maya's rate by %v, want Ada %v for both", b, r, ada)
		}
		if now := read(); now.Data.Set || now.Data.UpdatedAt != "" || len(now.Data.People) != 0 {
			t.Fatalf("a cleared project still reads as billed: %+v", now)
		}
		if b, err := rateModel.Get(project); err != nil || b != nil {
			t.Fatalf("a report still bills a cleared project: %+v %v", b, err)
		}
		// A save from the rates read before the clear finds them gone.
		if code, _ := save("EUR", 9600, before); code != http.StatusConflict || billing() != ada {
			t.Fatalf("a save from before the clear answered %d, want 409 and the billing still cleared", code)
		}
		// Billing it again from a fresh read brings the row back; Maya's old
		// rate stays as it was taken away.
		if code, v = save("USD", 0, ""); code != http.StatusOK || !v.Data.Set || v.Data.Currency != "USD" || len(v.Data.People) != 0 {
			t.Fatalf("billing the project again: %d %+v", code, v)
		}
		if b, r := billing(), mayasRate(); b != uuid.Nil || r != ada {
			t.Fatalf("billing again: the billing is marked by %v (want live) and Maya's rate by %v (want Ada)", b, r)
		}
		if now := read(); !now.Data.Set || len(now.Data.People) != 0 {
			t.Fatalf("billing again reads a rate the clear took away: %+v", now)
		}
	})

	t.Run("a channel's AI memory exclusion", func(t *testing.T) {
		set := func(excluded bool) {
			do(t, aiController.SetChannelMemoryExclusion, http.MethodPost, "/ai/memory/channel-exclusion", asAda,
				fmt.Sprintf(`{"channel_uuid":%q,"excluded":%t}`, channel, excluded))
		}
		excluded := func() bool {
			var got struct {
				Data struct {
					Excluded bool `json:"excluded"`
				} `json:"data"`
			}
			decode(t, do(t, aiController.GetChannelMemoryExclusion, http.MethodGet, "/ai/memory/channel-exclusion?channel="+url.QueryEscape(channel), asAda, ""), &got)
			return got.Data.Excluded
		}
		exclusion := func() uuid.UUID {
			return takenBy(t, "#launch's exclusion", `ai_memory_exclusions WHERE scope_type = 'channel' AND scope_id = $1`, channel)
		}
		set(true)
		set(false)
		if by := exclusion(); by != ada {
			t.Fatalf("the lifted exclusion is marked by %v, want Ada %v", by, ada)
		}
		if excluded() {
			t.Fatal("a lifted exclusion still keeps the AI from remembering the channel")
		}
		set(true)
		if by := exclusion(); by != uuid.Nil || !excluded() {
			t.Fatalf("excluding the channel again: marked by %v; want the row back, live, and the channel excluded", by)
		}
		// Excluding a channel that is already excluded changes nothing: when it
		// was excluded, and by whom, stay as they were.
		since := func() (at time.Time) {
			t.Helper()
			if err := w.env.PG.QueryRow(`SELECT created_at FROM ai_memory_exclusions WHERE scope_type = 'channel' AND scope_id = $1`, channel).Scan(&at); err != nil {
				t.Fatal(err)
			}
			return at
		}
		before := since()
		set(true)
		if after := since(); !after.Equal(before) {
			t.Fatalf("excluding an excluded channel again moved when it was excluded: %v -> %v", before, after)
		}
	})

	t.Run("a doc's consent to workspace exports", func(t *testing.T) {
		plans := w.doc(t, "maya", "Plans", false)
		doc := uuid.MustParse(plans)
		set := func(allow bool) {
			do(t, contentIOController.SetExportConsent, http.MethodPost, "/docs/export-consent", asMaya, fmt.Sprintf(`{"doc_uuid":%q,"allow":%t}`, plans, allow))
		}
		allowed := func() bool {
			t.Helper()
			var got struct {
				Consent struct {
					Allowed bool `json:"allowed"`
				} `json:"consent"`
			}
			decode(t, do(t, contentIOController.GetExportConsent, http.MethodGet, "/docs/export-consent?doc_uuid="+plans, asMaya, ""), &got)
			given, err := docExportModels.Given(ctx, []uuid.UUID{doc})
			if err != nil || (given[doc] == maya) != got.Consent.Allowed {
				t.Fatalf("the doc says allowed %v, but a workspace export finds %v (%v)", got.Consent.Allowed, given, err)
			}
			return got.Consent.Allowed
		}
		consent := func() uuid.UUID {
			return takenBy(t, "Plans' consent", `doc_export_consents WHERE doc_uuid = $1`, doc)
		}
		set(true)
		set(false)
		if by := consent(); by != maya {
			t.Fatalf("the withdrawn consent is marked by %v, want Maya %v", by, maya)
		}
		if allowed() {
			t.Fatal("a withdrawn consent still lets the doc into workspace exports")
		}
		set(true)
		if by := consent(); by != uuid.Nil || !allowed() {
			t.Fatalf("consenting again: marked by %v; want the row back, live, and the doc let in", by)
		}
	})
}

// TestSettingsSoftDeleteMigrationUpDownUp: the migration goes down and up
// again, twice, found by its name so a renumber at landing keeps it. Its down
// takes the marked rows away first, as the schema before it never kept them,
// so none comes back live when the columns go; what is live stays.
func TestSettingsSoftDeleteMigrationUpDownUp(t *testing.T) {
	read := func(dir string) string {
		t.Helper()
		files, err := filepath.Glob("../../migrations/*_settings_soft_delete." + dir + ".sql")
		if err != nil || len(files) != 1 {
			t.Fatalf("the migration's %s file: %v %v", dir, files, err)
		}
		b, err := os.ReadFile(files[0])
		if err != nil {
			t.Fatal(err)
		}
		return string(b)
	}
	up, down := read("up"), read("down")
	env := integration.SetupEnv(t)
	sam := uuid.New()
	if _, err := env.PG.Exec(`INSERT INTO users (id, email_id, username, display_name, created_at, updated_at) VALUES ($1, 'sam@example.test', 'sam', 'Sam', NOW(), NOW())`, sam); err != nil {
		t.Fatal(err)
	}
	kept, gone := uuid.New(), uuid.New()
	// In each table one row is live (kept) and one was taken away (gone).
	for _, q := range []string{
		`INSERT INTO status_rules (project_id, status, updated_by, deleted_at, deleted_by) VALUES ($1, 'done', $3, NULL, NULL), ($2, 'done', $3, NOW(), $3)`,
		`INSERT INTO doc_export_consents (doc_uuid, owner_uuid, deleted_at, deleted_by) VALUES ($1, $3, NULL, NULL), ($2, $3, NOW(), $3)`,
		`INSERT INTO ai_memory_exclusions (scope_type, scope_id, excluded_by, deleted_at, deleted_by) VALUES ('channel', $1::text, $3, NULL, NULL), ('channel', $2::text, $3, NOW(), $3)`,
		`INSERT INTO project_billing (project_uuid, currency, updated_by, deleted_at, deleted_by) VALUES ($1, 'EUR', $3, NULL, NULL), ($2, 'EUR', $3, NOW(), $3)`,
		`INSERT INTO project_person_rates (project_uuid, user_uuid, rate_cents, deleted_at, deleted_by) VALUES ($1, $3, 100, NULL, NULL), ($2, $3, 100, NOW(), $3)`,
	} {
		if _, err := env.PG.Exec(q, kept, gone, sam); err != nil {
			t.Fatalf("%s: %v", q, err)
		}
	}
	// A rate taken away under a billing still live goes with the down too.
	if _, err := env.PG.Exec(`INSERT INTO project_person_rates (project_uuid, user_uuid, rate_cents, deleted_at, deleted_by) VALUES ($1, $2, 100, NOW(), $3)`, kept, uuid.New(), sam); err != nil {
		t.Fatal(err)
	}
	counts := func() (live, marked int) {
		t.Helper()
		for _, q := range []string{
			`SELECT count(*) FILTER (WHERE project_id = $1), count(*) FILTER (WHERE project_id = $2) FROM status_rules`,
			`SELECT count(*) FILTER (WHERE doc_uuid = $1), count(*) FILTER (WHERE doc_uuid = $2) FROM doc_export_consents`,
			`SELECT count(*) FILTER (WHERE scope_id = $1::text), count(*) FILTER (WHERE scope_id = $2::text) FROM ai_memory_exclusions`,
			`SELECT count(*) FILTER (WHERE project_uuid = $1), count(*) FILTER (WHERE project_uuid = $2) FROM project_billing`,
			`SELECT count(*) FILTER (WHERE project_uuid = $1), count(*) FILTER (WHERE project_uuid = $2) FROM project_person_rates`,
		} {
			var l, m int
			if err := env.PG.QueryRow(q, kept, gone).Scan(&l, &m); err != nil {
				t.Fatalf("%s: %v", q, err)
			}
			live, marked = live+l, marked+m
		}
		return live, marked
	}
	for i, step := range []string{down, up, down, up} {
		if _, err := env.PG.Exec(step); err != nil {
			t.Fatalf("step %d: %v", i, err)
		}
		live, marked := counts()
		if live != 5 || marked != 0 {
			t.Fatalf("after step %d: %d live rows (want 5, one a table) and %d marked (want 0: the down takes them away)", i, live, marked)
		}
	}
	var columns int
	if err := env.PG.QueryRow(`SELECT count(*) FROM information_schema.columns
		WHERE table_name IN ('status_rules', 'doc_export_consents', 'ai_memory_exclusions', 'project_billing', 'project_person_rates')
		AND column_name IN ('deleted_at', 'deleted_by')`).Scan(&columns); err != nil || columns != 10 {
		t.Fatalf("after the last up: %d of the 10 columns (%v)", columns, err)
	}
}
