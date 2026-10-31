//go:build integration
// +build integration

package integration_test

// Each member brings their own Slack DMs (chat item 2: a Slack team moves in an
// afternoon). A team on Slack's free or Pro plan exports its public channels
// alone, so the admin imports those, and each member brings their own DMs,
// group DMs and private channels with their own token. What keeps it theirs is
// checked here against real Postgres, Redis and Dgraph, with a fake Slack that
// serves the acme workspace as Slack answers Bo's token, and lists him one more
// private channel than Slack would: Ada and Cy's #hiring, which he isn't in.
//
//   - The admin's public import and Bo's read together bring everything the
//     whole export brings (cameAcross), each conversation with exactly its
//     Slack members: the admin in none of Bo's, nobody else added to one.
//   - Bo brings only what he is in: #hiring is left out, its history never
//     asked for and its name in no note; no public channel is read again.
//   - Bo is #leads' member, not its manager: Ada made it in Slack.
//   - The admins see that it happened, never what's in it: an audit record of
//     who and how many; the import history without plan, digest, notes or
//     names, in both its screens; and no plan, run or retry of it by hand.
//   - The token is kept nowhere. Someone else's token, an app's, and a second
//     read while one is under way are refused before anything is read.
//
// Run: go test -tags=integration ./tests/integration/ -run TestEachMemberBringsTheirOwnSlackDMs -v

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"slices"
	"sort"
	"strings"
	"testing"
	"time"

	"github.com/go-chi/chi/v5"
	"github.com/google/uuid"

	slackBusiness "github.com/akashc777/OneCamp/business/SlackImport"
	importController "github.com/akashc777/OneCamp/controllers/Import"
	slackImportController "github.com/akashc777/OneCamp/controllers/SlackImport"
	"github.com/akashc777/OneCamp/helpers"
	redisInit "github.com/akashc777/OneCamp/initializers/redis"
	slackapi "github.com/akashc777/OneCamp/services/Slack"
	"github.com/akashc777/OneCamp/tests/integration"
)

func TestEachMemberBringsTheirOwnSlackDMs(t *testing.T) {
	const (
		boToken  = "xoxp-acme-bo-0123456789"
		adaToken = "xoxp-acme-ada-9876543210"
		appToken = "xoxp-acme-app-5555555555"
	)
	ctx := context.Background()

	// The admin's import: the export a free or Pro team gets, its public
	// channels alone.
	public := map[string]bool{"users.json": true, "channels.json": true, "general": true, "launch": true, "old-launch": true}
	w := newAcmeWorld(t, map[string][]byte{"raw.zip": zipTreeWhere(t, acmeDir, func(rel string) bool {
		return public[strings.SplitN(rel, "/", 2)[0]]
	})})
	e, admin := w.e, w.admin
	team := e.job(t, "slack", "Acme", "planned", "slackImport/acme/raw.zip")
	if _, err := e.PG.Exec(`UPDATE import_jobs SET options = '{"skip_subtypes": false}' WHERE id = $1`, team); err != nil {
		t.Fatal(err)
	}
	arc, done, err := slackBusiness.OpenStagedZipFromMinIO(ctx, "slackImport/acme/raw.zip")
	if err != nil {
		t.Fatalf("open the export: %v", err)
	}
	if _, err := slackBusiness.BuildPlan(ctx, team, arc); err != nil {
		t.Fatalf("plan: %v", err)
	}
	done()
	slackBusiness.RunImport(ctx, team, &admin)
	if status, _ := e.state(t, team); status != "completed" {
		t.Fatalf("the team's import ended %q", status)
	}

	// Slack as it answers Bo's token, and Ada's, and an app's.
	slack := newSlackFromExport(t, acmeDir, boToken, "U0BO00002")
	slack.people[adaToken] = "U0ADA0001"
	slack.people[appToken], slack.bots[appToken] = "U0GHBOT09", true
	slack.kinds["private_channel"] = append(slack.kinds["private_channel"], map[string]any{
		"id": "G0HIRING1", "name": "hiring", "created": 1759000500, "creator": "U0ADA0001", "is_channel": true, "is_private": true})
	slack.members["G0HIRING1"] = []any{"U0ADA0001", "U0CY00003"}
	slack.history["G0HIRING1"] = []any{map[string]any{"ts": "1759310300.000900", "user": "U0ADA0001", "text": "Interview notes for the designer role"}}
	srv := httptest.NewServer(slack)
	t.Cleanup(srv.Close)
	was := slackapi.Base
	slackapi.Base = srv.URL + "/api/"
	t.Cleanup(func() { slackapi.Base = was })

	bo := signedIn(t, w.people["bo"])
	as := func(who any) context.Context { return context.WithValue(ctx, helpers.UserInfoContextKey, who) }
	call := func(h http.HandlerFunc, who context.Context, job uuid.UUID, body string) (int, map[string]any) {
		t.Helper()
		rc := chi.NewRouteContext()
		rc.URLParams.Add("jobId", job.String())
		rec := httptest.NewRecorder()
		h(rec, httptest.NewRequest(http.MethodPost, "/", strings.NewReader(body)).WithContext(context.WithValue(who, chi.RouteCtxKey, rc)))
		var out map[string]any
		_ = json.Unmarshal(rec.Body.Bytes(), &out)
		return rec.Code, out
	}
	// A member may start three reads a minute; each step here starts afresh.
	freshMinute := func() {
		t.Helper()
		keys, err := redisInit.RedisClient.Keys(ctx, "*slack-import:rate*").Result()
		if err == nil && len(keys) > 0 {
			err = redisInit.RedisClient.Del(ctx, keys...).Err()
		}
		if err != nil {
			t.Fatal(err)
		}
	}
	jobs := func() int {
		var n int
		if err := e.PG.QueryRow(`SELECT count(*) FROM import_jobs`).Scan(&n); err != nil {
			t.Fatal(err)
		}
		return n
	}

	// Refused before anything is read: someone else's token, an app's (by its
	// prefix, or by Slack's word), and no job is made for any of them.
	before, asked := jobs(), len(slack.asked)
	for _, c := range []struct {
		token string
		code  int
		says  string
	}{
		{adaToken, http.StatusForbidden, "not_your_token"},
		{"xoxb-acme-app-1", http.StatusBadRequest, "bad_token"},
		{appToken, http.StatusBadRequest, "bad_token"},
	} {
		if code, out := call(slackImportController.HandleMyRead, as(*bo), uuid.Nil, `{"token": "`+c.token+`"}`); code != c.code || out["code"] != c.says {
			t.Errorf("Bo bringing %s: %d %v, want %d %s", c.token, code, out, c.code, c.says)
		}
	}
	// A fourth within the minute is refused unread, his own token or not.
	if code, out := call(slackImportController.HandleMyRead, as(*bo), uuid.Nil, `{"token": "`+boToken+`"}`); code != http.StatusTooManyRequests {
		t.Errorf("a fourth read within the minute: %d %v", code, out)
	}
	if after := jobs(); after != before {
		t.Errorf("a refused read made %d jobs", after-before)
	}
	for _, m := range slack.asked[asked:] {
		if m != "auth.test" {
			t.Errorf("a refused read asked Slack %s", m)
		}
	}
	freshMinute()

	// Bo brings his own.
	asked = len(slack.asked)
	code, out := call(slackImportController.HandleMyRead, as(*bo), uuid.Nil, `{"token": "`+boToken+`"}`)
	if code != http.StatusAccepted {
		t.Fatalf("Bo's read: %d %v", code, out)
	}
	read, err := uuid.Parse(fmt.Sprint(out["job_id"]))
	if err != nil {
		t.Fatalf("Bo's read's job: %v", out)
	}
	start := time.Now()
	for deadline := time.Now().Add(2 * time.Minute); ; {
		integration.Settle(t)
		status, _ := e.state(t, read)
		if status == "completed" || status == "failed" || status == "cancelled" {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("Bo's read is still %s", status)
		}
		time.Sleep(200 * time.Millisecond)
	}
	// Everything the whole export brings is here, each conversation with
	// exactly its Slack members, and nobody opens one they weren't in.
	notes := w.cameAcross(t, read, time.Since(start))
	for code := range notes {
		if code != "" && !strings.HasPrefix(code, "CHANNEL") {
			t.Logf("Bo's read noted %s", code)
		}
	}

	// Only what Bo is in, and none of the public channels again.
	var options, by string
	if err := e.PG.QueryRow(`SELECT options::text, triggered_by::text FROM import_jobs WHERE id = $1`, read).Scan(&options, &by); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(options, `"member_dms": true`) || by != bo.UserPostgresInfo.Id.String() {
		t.Errorf("Bo's read is %s by %s", options, by)
	}
	for _, of := range slack.of[asked:] {
		if strings.Contains(of, "public_channel") || of == "conversations.history G0HIRING1" || of == "pins.list G0HIRING1" {
			t.Errorf("Bo's read asked Slack for %q", of)
		}
	}
	var hiring int
	if err := e.PG.QueryRow(`SELECT count(*) FROM import_errors WHERE import_id = $1 AND (message LIKE '%hiring%' OR source_id = 'G0HIRING1')`, read).Scan(&hiring); err != nil || hiring != 0 {
		t.Errorf("a note names #hiring to Bo: %d %v", hiring, err)
	}
	var graph struct {
		Channels []struct {
			Name    string `json:"ch_name"`
			Members []struct {
				Name string `json:"user_name"`
			} `json:"ch_members"`
			Moderators []struct {
				Name string `json:"user_name"`
			} `json:"ch_moderators"`
		} `json:"channels"`
	}
	raw := w.dg.Query(t, `{ channels(func: has(ch_uuid)) { ch_name ch_members { user_name } ch_moderators { user_name } } }`)
	if err := json.Unmarshal(raw, &graph); err != nil {
		t.Fatalf("%s: %v", raw, err)
	}
	for _, c := range graph.Channels {
		var mods []string
		for _, m := range c.Moderators {
			mods = append(mods, m.Name)
		}
		sort.Strings(mods)
		switch c.Name {
		case "hiring":
			t.Errorf("#hiring came across with Bo's read: %+v", c)
		case "leads":
			if fmt.Sprint(mods) != "[ada]" {
				t.Errorf("#leads' managers %v, want Ada, who made it in Slack", mods)
			}
		}
	}

	// The admins see that it happened: the audit log says who, and how many of
	// each, and names nothing.
	var summary, meta, actor string
	for deadline := time.Now().Add(10 * time.Second); ; time.Sleep(100 * time.Millisecond) {
		err := e.PG.QueryRow(`SELECT summary, metadata::text, actor_id::text FROM admin_audit_log WHERE action = 'slack_import.member_read'`).Scan(&summary, &meta, &actor)
		if err == nil || time.Now().After(deadline) {
			if err != nil {
				t.Fatalf("no audit record of Bo's read: %v", err)
			}
			break
		}
	}
	var counts struct {
		Job      string `json:"job_id"`
		DMs      int    `json:"dms"`
		GroupDMs int    `json:"group_dms"`
		Private  int    `json:"private_channels"`
	}
	_ = json.Unmarshal([]byte(meta), &counts)
	if actor != bo.UserPostgresInfo.Id.String() || counts.Job != read.String() || counts.DMs != 1 || counts.GroupDMs != 1 || counts.Private != 1 {
		t.Errorf("the audit record: %s by %s, %s", summary, actor, meta)
	}
	for _, word := range []string{"leads", "hiring", "Group of three", "engineers", "D0ADABO01", boToken} {
		if strings.Contains(summary+meta, word) {
			t.Errorf("the audit record holds %q: %s %s", word, summary, meta)
		}
	}

	// The import history, in both its screens, shows the admin the read
	// without what's in it; Bo sees all of it in his own.
	asAdmin := w.asAdmin()
	_, list := call(slackImportController.HandleListJobs, asAdmin, uuid.Nil, ``)
	var shown map[string]any
	for _, j := range list["jobs"].([]any) {
		if v := j.(map[string]any); v["id"] == read.String() {
			shown = v
		}
	}
	_, one := call(importController.HandleGetJob, asAdmin, read, ``)
	for screen, v := range map[string]map[string]any{"the Slack import's history": shown, "the import page": one} {
		if v == nil {
			t.Errorf("%s doesn't show Bo's read", screen)
			continue
		}
		progress, _ := json.Marshal(v["progress"])
		if v["plan"] != nil || v["digest"] != nil || strings.Contains(string(progress), "leads") || strings.Contains(string(progress), "channels") {
			t.Errorf("%s shows the admin what's in Bo's read: plan %v, digest %v, progress %s", screen, v["plan"] != nil, v["digest"], progress)
		}
		if !strings.Contains(string(progress), `"dms":1`) {
			t.Errorf("%s doesn't show how many conversations Bo brought: %s", screen, progress)
		}
	}
	if shown != nil && shown["member_name"] != "bo" {
		t.Errorf("the Slack import's history doesn't say whose DMs they are: %v", shown["member_name"])
	}
	for screen, h := range map[string]http.HandlerFunc{"the Slack import's": slackImportController.HandleListErrors, "the import page's": importController.HandleListErrors} {
		if _, out := call(h, asAdmin, read, ``); out["hidden"] != true || len(out["errors"].([]any)) != 0 {
			t.Errorf("%s notes show the admin Bo's read's: %v", screen, out)
		}
	}
	rc := chi.NewRouteContext()
	rec := httptest.NewRecorder()
	slackImportController.HandleMyReads(rec, httptest.NewRequest(http.MethodGet, "/", nil).WithContext(context.WithValue(as(*bo), chi.RouteCtxKey, rc)))
	var mine struct {
		Reads []struct {
			ID   string `json:"id"`
			Plan any    `json:"plan"`
		} `json:"reads"`
		TeamImported bool `json:"team_imported"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &mine); err != nil || !mine.TeamImported || len(mine.Reads) != 1 || mine.Reads[0].ID != read.String() || mine.Reads[0].Plan == nil {
		t.Errorf("Bo's own reads: %d %s", rec.Code, rec.Body.String())
	}

	// Nobody plans, runs or retries it by hand: it ran as Bo, and an admin's
	// run would bring it in as the admin.
	for name, h := range map[string]http.HandlerFunc{
		"plan": slackImportController.HandlePlan, "run": slackImportController.HandleRun,
		"the import page's plan": importController.HandlePlan, "the import page's run": importController.HandleRun,
		"retry": importController.HandleRetryFailedChunks,
	} {
		if code, out := call(h, asAdmin, read, `{"options": {"member_dms": false}}`); code != http.StatusConflict || out["code"] != "member_read" {
			t.Errorf("the admin's %s of Bo's read: %d %v", name, code, out)
		}
	}
	if err := e.PG.QueryRow(`SELECT options::text FROM import_jobs WHERE id = $1`, read).Scan(&options); err != nil || !strings.Contains(options, `"member_dms": true`) {
		t.Errorf("Bo's read's options became %s", options)
	}

	// A second read while one is under way is refused, and so is stopping a
	// read that isn't his.
	waiting := uuid.New()
	if _, err := e.PG.Exec(`INSERT INTO import_jobs (id, provider, source_workspace_name, source, status, triggered_by, options)
		VALUES ($1, 'slack', 'Acme', 'api', 'pending', $2, '{"member_dms": true}')`, waiting, bo.UserPostgresInfo.Id); err != nil {
		t.Fatal(err)
	}
	if code, out := call(slackImportController.HandleMyRead, as(*bo), uuid.Nil, `{"token": "`+boToken+`"}`); code != http.StatusConflict || out["code"] != "under_way" {
		t.Errorf("a second read while one is under way: %d %v", code, out)
	}
	ada := signedIn(t, w.people["ada"])
	if code, out := call(slackImportController.HandleStopMyRead, as(*ada), waiting, ``); code != http.StatusNotFound || out["code"] != "not_yours" {
		t.Errorf("Ada stopping Bo's read: %d %v", code, out)
	}
	if code, out := call(slackImportController.HandleStopMyRead, as(*bo), waiting, ``); code != http.StatusOK {
		t.Errorf("Bo stopping his own read: %d %v", code, out)
	}
	if status, _ := e.state(t, waiting); status != "cancelled" {
		t.Errorf("Bo's stopped read is %s", status)
	}

	// The token is kept nowhere: not on a job, not in a note, not in the audit log.
	for _, q := range []string{
		`SELECT count(*) FROM import_jobs WHERE row_to_json(import_jobs)::text LIKE '%' || $1 || '%'`,
		`SELECT count(*) FROM import_errors WHERE row_to_json(import_errors)::text LIKE '%' || $1 || '%'`,
		`SELECT count(*) FROM admin_audit_log WHERE row_to_json(admin_audit_log)::text LIKE '%' || $1 || '%'`,
	} {
		for _, token := range []string{boToken, adaToken} {
			var kept int
			if err := e.PG.QueryRow(q, token).Scan(&kept); err != nil || kept != 0 {
				t.Errorf("%s: the token is kept (%d, %v)", q, kept, err)
			}
		}
	}
	if !slices.Contains(slack.asked, "conversations.history") {
		t.Error("Bo's read read no history at all")
	}
}
