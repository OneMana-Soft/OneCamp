//go:build integration

package integration_test

// Call Quality Telemetry (calls A5, #56) on Postgres 12: someone in the live
// call keeps their own samples and nobody else's; why the call stuttered is
// read per person; the install's day is rolled up once; the migration goes
// down and up again.

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"

	callQualityBusiness "github.com/akashc777/OneCamp/business/CallQuality"
	callStateBusiness "github.com/akashc777/OneCamp/business/CallState"
	callsController "github.com/akashc777/OneCamp/controllers/Calls"
	"github.com/akashc777/OneCamp/initializers/livekitInit"
	"github.com/akashc777/OneCamp/initializers/postgresInit"
	dgraphStruct "github.com/akashc777/OneCamp/models/dgraph"
	"github.com/akashc777/OneCamp/tests/integration"
)

func callQualityMigration(t *testing.T, dir string) string {
	t.Helper()
	_, here, _, _ := runtime.Caller(0)
	matches, _ := filepath.Glob(filepath.Join(filepath.Dir(here), "..", "..", "migrations", "*_call_quality."+dir+".sql"))
	if len(matches) != 1 {
		t.Fatalf("want one call_quality %s migration, found %v", dir, matches)
	}
	b, err := os.ReadFile(matches[0])
	if err != nil {
		t.Fatal(err)
	}
	return string(b)
}

// The quality migration goes down and up again: its two tables go and come back.
func TestCallQualityMigrationUpDownUp(t *testing.T) {
	env := integration.SetupEnv(t)
	exists := func(table string) bool {
		var ok bool
		if err := env.PG.QueryRow(`SELECT to_regclass($1) IS NOT NULL`, table).Scan(&ok); err != nil {
			t.Fatal(err)
		}
		return ok
	}
	if !exists("call_quality_samples") || !exists("call_quality_daily") {
		t.Fatal("the tables aren't there after the migrations")
	}
	if _, err := env.PG.Exec(callQualityMigration(t, "down")); err != nil {
		t.Fatalf("down: %v", err)
	}
	if exists("call_quality_samples") || exists("call_quality_daily") {
		t.Error("the tables outlived the down step")
	}
	if _, err := env.PG.Exec(callQualityMigration(t, "up")); err != nil {
		t.Fatalf("up again: %v", err)
	}
	if !exists("call_quality_samples") || !exists("call_quality_daily") {
		t.Error("the tables didn't come back")
	}
}

// Maya, in the call, keeps her samples; Ben, who left minutes ago, keeps none. Maya runs
// the call and reads why it stuttered for each person. Yesterday's samples
// are rolled up into one row, once.
func TestCallQualityIsKeptReadAndRolledUp(t *testing.T) {
	env := integration.SetupEnv(t)
	ctx := context.Background()
	if err := postgresInit.ConnectPostgres(ctx, env.DSN); err != nil {
		t.Fatal(err)
	}
	session, room := uuid.New(), uuid.NewString()
	if _, err := env.PG.Exec(`INSERT INTO call_sessions (id, room, kind, resource_kind, resource_id, started_at) VALUES ($1, $2, 'huddle', 'channel', $2, NOW() - interval '10 minutes')`, session, room); err != nil {
		t.Fatal(err)
	}
	if _, err := env.PG.Exec(`INSERT INTO call_attendance (session_id, identity, name, role, joined_at, last_joined_at, left_at) VALUES
		($1, '0x1', 'Maya Chen', 'host', NOW() - interval '10 minutes', NOW() - interval '10 minutes', NULL),
		($1, '0x2', 'Ben', 'participant', NOW() - interval '10 minutes', NOW() - interval '10 minutes', NOW() - interval '5 minutes')`, session); err != nil {
		t.Fatal(err)
	}
	maya, ben := &dgraphStruct.DgraphUser{Uid: "0x1"}, &dgraphStruct.DgraphUser{Uid: "0x2"}
	lossy := []callQualityBusiness.Sample{{AgoMs: 30_000, RttMs: 90, LossPct: 9}, {AgoMs: 20_000, RttMs: 95, LossPct: 11}, {AgoMs: 10_000, RttMs: 80}}

	if n, err := callQualityBusiness.Record(ctx, maya, session.String(), lossy); err != nil || n != 3 {
		t.Fatalf("Maya's samples: %d, %v", n, err)
	}
	if _, err := callQualityBusiness.Record(ctx, ben, session.String(), lossy); !errors.Is(err, callQualityBusiness.ErrNotInCall) {
		t.Errorf("Ben, who left longer ago than the grace: %v", err)
	}
	var kept int
	if err := env.PG.QueryRow(`SELECT count(*) FROM call_quality_samples WHERE session_id = $1`, session).Scan(&kept); err != nil || kept != 3 {
		t.Fatalf("kept %d, %v", kept, err)
	}
	// A batch sent again, its answer lost, adds nothing: one sample per
	// person per 10 s. Its samples sit mid-way through their 10 s, so the
	// two sends a moment apart land in the same ones.
	mid := time.Now().UnixMilli()%10_000 + 5_000
	again := []callQualityBusiness.Sample{{AgoMs: mid + 40_000, RttMs: 70}, {AgoMs: mid + 50_000, RttMs: 75}}
	if n, err := callQualityBusiness.Record(ctx, maya, session.String(), again); err != nil || n != 2 {
		t.Fatalf("a new batch: %d, %v", n, err)
	}
	if n, err := callQualityBusiness.Record(ctx, maya, session.String(), again); err != nil || n != 0 {
		t.Errorf("the batch sent again wrote %d, %v", n, err)
	}
	if err := env.PG.QueryRow(`SELECT count(*) FROM call_quality_samples WHERE session_id = $1`, session).Scan(&kept); err != nil || kept != 5 {
		t.Fatalf("kept %d after the resend, %v", kept, err)
	}

	// Why adds each person's samples up in the database: the median round
	// trip of 70, 75, 80, 90 and 95, the 95th percentile of loss, and two
	// of five held back by the network.
	why, err := callQualityBusiness.Why(ctx, maya, false, session.String())
	if err != nil || len(why) != 1 || why[0].Name != "Maya Chen" || why[0].Verdict != callQualityBusiness.VerdictNetwork || why[0].Samples != 5 {
		t.Fatalf("why it stuttered: %+v, %v", why, err)
	}
	if j := why[0]; j.RttMs != 80 || j.LossPct != 11 || j.NetworkShare != 0.4 {
		t.Errorf("the numbers: %+v", j)
	}
	if _, err := callQualityBusiness.Why(ctx, ben, false, session.String()); !errors.Is(err, callQualityBusiness.ErrNotAllowed) {
		t.Errorf("Ben reading it: %v", err)
	}

	// Yesterday's samples (written as the store keeps them), rolled up once.
	now := time.Now()
	y := time.Date(now.UTC().Year(), now.UTC().Month(), now.UTC().Day(), 12, 0, 0, 0, time.UTC).AddDate(0, 0, -1)
	if _, err := env.PG.Exec(`INSERT INTO call_quality_samples (session_id, identity, at, rtt_ms, loss_pct, limited) VALUES
		($1, '0x1', $2, 100, 0, ''), ($1, '0x1', $2::timestamptz + interval '10 seconds', 300, 8, ''), ($1, '0x9', $2, 50, 0, 'cpu'), ($1, '0x9', $2::timestamptz + interval '10 seconds', 60, 0, '')`, session, y); err != nil {
		t.Fatal(err)
	}
	wrote, err := callQualityBusiness.RollUpDays(ctx, now)
	if err != nil || wrote < 1 {
		t.Fatalf("rolled up %d, %v", wrote, err)
	}
	if again, err := callQualityBusiness.RollUpDays(ctx, now); err != nil || again != 0 {
		t.Errorf("a second pass wrote %d, %v", again, err)
	}
	days, err := callQualityBusiness.Days(ctx, 7, now)
	if err != nil || len(days) == 0 {
		t.Fatalf("the days: %+v, %v", days, err)
	}
	d := days[len(days)-1]
	if d.Day.Format(time.DateOnly) != y.Format(time.DateOnly) || d.Calls != 1 || d.People != 2 || d.Samples != 4 || d.CPUShare != 0.25 || d.NetworkShare != 0.25 {
		t.Errorf("yesterday: %+v", d)
	}
}

// The batch Maya's app sends as she leaves lands after the call ended, as the
// last one out (V-973): it is kept on Postgres, but nothing from after she
// left, once only, and never for an earlier call in the room she left long
// ago. A room's latest call is found going on or over.
func TestCallQualityLeavingBatchIsKept(t *testing.T) {
	env := integration.SetupEnv(t)
	ctx := context.Background()
	if err := postgresInit.ConnectPostgres(ctx, env.DSN); err != nil {
		t.Fatal(err)
	}
	earlier, latest, room := uuid.New(), uuid.New(), uuid.NewString()
	if _, err := env.PG.Exec(`INSERT INTO call_sessions (id, room, kind, resource_kind, resource_id, started_at, ended_at) VALUES
		($1, $3, 'huddle', 'channel', $3, NOW() - interval '30 minutes', NOW() - interval '20 minutes'),
		($2, $3, 'huddle', 'channel', $3, NOW() - interval '10 minutes', NOW() - interval '30 seconds')`, earlier, latest, room); err != nil {
		t.Fatal(err)
	}
	if _, err := env.PG.Exec(`INSERT INTO call_attendance (session_id, identity, name, role, joined_at, last_joined_at, left_at) VALUES
		($1, '0x1', 'Maya Chen', 'host', NOW() - interval '30 minutes', NOW() - interval '30 minutes', NOW() - interval '20 minutes'),
		($2, '0x1', 'Maya Chen', 'host', NOW() - interval '10 minutes', NOW() - interval '10 minutes', NOW() - interval '30 seconds')`, earlier, latest); err != nil {
		t.Fatal(err)
	}

	s, err := callStateBusiness.LatestInRoom(ctx, room)
	if err != nil || s == nil || s.ID != latest.String() || s.Live || s.Person("0x1") == nil {
		t.Fatalf("the room's latest call: %+v, %v", s, err)
	}
	if s, err := callStateBusiness.LatestInRoom(ctx, uuid.NewString()); err != nil || s != nil {
		t.Errorf("a room with no call: %+v, %v", s, err)
	}

	maya := &dgraphStruct.DgraphUser{Uid: "0x1"}
	// Two samples from before she left, mid-way through their 10 s so that
	// the batch sent again a moment later lands in the same ones, and one
	// from 20 s after she left.
	mid := time.Now().UnixMilli()%10_000 + 5_000
	leaving := []callQualityBusiness.Sample{{AgoMs: mid + 40_000, RttMs: 90}, {AgoMs: mid + 30_000, RttMs: 95}, {AgoMs: 10_000, RttMs: 80}}
	if n, err := callQualityBusiness.Record(ctx, maya, latest.String(), leaving); err != nil || n != 2 {
		t.Fatalf("Maya's leaving batch: %d, %v", n, err)
	}
	if n, err := callQualityBusiness.Record(ctx, maya, latest.String(), leaving); err != nil || n != 0 {
		t.Errorf("the batch sent again wrote %d, %v", n, err)
	}
	if _, err := callQualityBusiness.Record(ctx, maya, earlier.String(), []callQualityBusiness.Sample{{AgoMs: 21 * 60_000}}); !errors.Is(err, callQualityBusiness.ErrNotInCall) {
		t.Errorf("the earlier call she left 20 minutes ago: %v", err)
	}
	var kept int
	if err := env.PG.QueryRow(`SELECT count(*) FROM call_quality_samples WHERE session_id IN ($1, $2)`, earlier, latest).Scan(&kept); err != nil || kept != 2 {
		t.Errorf("kept %d, %v", kept, err)
	}
}

// A guest's last batch, sent as the call goes from their screen, reaches the
// guest route with the call token they joined with after the call already
// ended (they were the last one out), and is kept: their page knows its
// token, not the call's session, so the route reads the room's latest call,
// over or not (CODE-REVIEWER-B84 F1). A token for a room with no call keeps
// nothing.
func TestCallQualityGuestLeavingBatchIsKept(t *testing.T) {
	env := integration.SetupEnv(t)
	ctx := context.Background()
	if err := postgresInit.ConnectPostgres(ctx, env.DSN); err != nil {
		t.Fatal(err)
	}
	integration.SetupLiveKit(t)
	meeting, session, gia := uuid.NewString(), uuid.New(), "guest-"+uuid.NewString()+"-g1"
	room := "meet-" + meeting
	if _, err := env.PG.Exec(`INSERT INTO call_sessions (id, room, kind, resource_kind, resource_id, started_at, ended_at, ended_reason) VALUES ($1, $2, 'meeting', 'meeting', $3, NOW() - interval '10 minutes', NOW() - interval '30 seconds', 'last_left')`, session, room, meeting); err != nil {
		t.Fatal(err)
	}
	if _, err := env.PG.Exec(`INSERT INTO call_attendance (session_id, identity, name, role, joined_at, last_joined_at, left_at) VALUES ($1, $2, 'Guest: Gia', 'guest', NOW() - interval '10 minutes', NOW() - interval '10 minutes', NOW() - interval '30 seconds')`, session, gia); err != nil {
		t.Fatal(err)
	}
	// As the web sends it: POST /guest/calls/quality, the call token as a
	// bearer, the samples as JSON.
	type answer struct {
		Code string `json:"code"`
		Data struct {
			Kept int `json:"kept"`
		} `json:"data"`
	}
	send := func(room, body string) (int, answer) {
		token, err := livekitInit.LiveKitService.GenerateGuestToken(room, gia, "Guest: Gia", "", livekitInit.CallPublishSources)
		if err != nil {
			t.Fatal(err)
		}
		r := httptest.NewRequest(http.MethodPost, "/guest/calls/quality", strings.NewReader(body))
		r.Header.Set("Authorization", "Bearer "+token)
		w := httptest.NewRecorder()
		callsController.GuestRecordCallQuality(w, r)
		var a answer
		if err := json.Unmarshal(w.Body.Bytes(), &a); err != nil {
			t.Fatalf("the answer %q: %v", w.Body.String(), err)
		}
		return w.Code, a
	}

	// Two samples from before she left, and one from 20 s after she left.
	leaving := `{"samples":[{"ago_ms":50000,"rtt_ms":90},{"ago_ms":40000,"rtt_ms":95},{"ago_ms":10000,"rtt_ms":80}]}`
	if code, a := send(room, leaving); code != http.StatusOK || a.Data.Kept != 2 {
		t.Fatalf("Gia's leaving batch after the call ended: %d %+v", code, a)
	}
	if code, a := send("meet-"+uuid.NewString(), leaving); code != http.StatusNotFound || a.Code != "not_in_call" {
		t.Errorf("a token for a room with no call: %d %+v", code, a)
	}
	var kept int
	if err := env.PG.QueryRow(`SELECT count(*) FROM call_quality_samples WHERE session_id = $1 AND identity = $2`, session, gia).Scan(&kept); err != nil || kept != 2 {
		t.Errorf("kept %d, %v", kept, err)
	}
}

// A call's own samples go 30 days on, once their day is rolled up; the
// install's days stay (CODE-REVIEWER: nothing deleted them).
func TestCallQualitySamplesGoOnceRolledUp(t *testing.T) {
	env := integration.SetupEnv(t)
	ctx := context.Background()
	if err := postgresInit.ConnectPostgres(ctx, env.DSN); err != nil {
		t.Fatal(err)
	}
	session := uuid.New()
	now := time.Now()
	old, recent := now.AddDate(0, 0, -40).Truncate(time.Second), now.AddDate(0, 0, -2).Truncate(time.Second)
	if _, err := env.PG.Exec(`INSERT INTO call_quality_samples (session_id, identity, at, rtt_ms) VALUES
		($1, '0x1', $2, 80), ($1, '0x1', $2::timestamptz + interval '10 seconds', 90), ($1, '0x1', $3, 70)`, session, old, recent); err != nil {
		t.Fatal(err)
	}
	if _, err := callQualityBusiness.RollUpDays(ctx, now); err != nil {
		t.Fatal(err)
	}
	n, err := callQualityBusiness.PurgeSamples(ctx, now)
	if err != nil || n < 2 {
		t.Fatalf("purged %d, %v", n, err)
	}
	var left []time.Time
	rows, err := env.PG.Query(`SELECT at FROM call_quality_samples WHERE session_id = $1`, session)
	if err != nil {
		t.Fatal(err)
	}
	defer rows.Close()
	for rows.Next() {
		var at time.Time
		if err := rows.Scan(&at); err != nil {
			t.Fatal(err)
		}
		left = append(left, at)
	}
	if len(left) != 1 || !left[0].Equal(recent) {
		t.Errorf("left %v, want only the one from two days ago", left)
	}
	var days int
	if err := env.PG.QueryRow(`SELECT count(*) FROM call_quality_daily WHERE day = $1::date`, old.UTC().Format(time.DateOnly)).Scan(&days); err != nil || days != 1 {
		t.Errorf("the old day's row: %d, %v", days, err)
	}
}
