//go:build integration
// +build integration

package integration_test

// After an import the team arrives: the admin who ran it is told it finished,
// offered the people who came across (only those who can really be invited,
// with the plan's room shown), and the invitations go out through the
// workspace's own invitation endpoint, which still refuses a full plan.
//
// Run: go test -tags=integration ./tests/integration/ -run TestAfterAnImportTheTeamIsOffered -v

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/go-chi/chi/v5"
	"github.com/google/uuid"

	importBusiness "github.com/akashc777/OneCamp/business/Import"
	importController "github.com/akashc777/OneCamp/controllers/Import"
	userController "github.com/akashc777/OneCamp/controllers/User"
	"github.com/akashc777/OneCamp/helpers"
	"github.com/akashc777/OneCamp/initializers/postgresInit"
	userModels "github.com/akashc777/OneCamp/models/postgres/User"
	"github.com/akashc777/OneCamp/tests/integration"
)

func TestAfterAnImportTheTeamIsOffered(t *testing.T) {
	ctx := context.Background()
	env := integration.SetupEnv(t)
	if err := postgresInit.ConnectPostgres(ctx, env.DSN); err != nil {
		t.Fatal(err)
	}
	old := helpers.SeatLimit
	t.Cleanup(func() { helpers.SeatLimit = old })
	helpers.SeatLimit = "5"

	exec := func(q string, args ...any) {
		t.Helper()
		if _, err := env.PG.Exec(q, args...); err != nil {
			t.Fatalf("%s: %v", q, err)
		}
	}
	type person struct {
		email, name                  string
		external, bot, gone, invited bool
		sourceBot                    bool
	}
	admin, other := uuid.New(), uuid.New()
	exec(`INSERT INTO users (id, email_id, username) VALUES ($1, 'admin@acme.test', 'admin'), ($2, 'other@acme.test', 'other')`, admin, other)

	job, othersJob, refused := uuid.New(), uuid.New(), uuid.New()
	exec(`INSERT INTO import_jobs (id, provider, source_workspace_name, source, status, started_at, completed_at, triggered_by)
		VALUES ($1, 'asana', 'Acme', 'api', 'completed', NOW() - interval '1 hour', NOW() - interval '1 minute', $2),
		       ($3, 'jira', 'Acme', 'api', 'completed', NOW() - interval '1 hour', NOW() - interval '1 minute', $4),
		       ($5, 'trello', 'Board', 'api', 'failed', NULL, NOW(), $2)`,
		job, admin, othersJob, other, refused)

	people := []person{
		{email: "sam@acme.test", name: "Sam"}, // a member the import matched by address
		{email: "priya@acme.test", name: "Priya", external: true},
		{email: "jo@acme.test", external: true}, // no name in the source
		{email: "kim@acme.test", name: "Kim", external: true},
		{email: "lee@acme.test", name: "Lee", external: true, invited: true},
		{email: "asana-import-acme-77@no-reply.local", name: "Nomail", external: true},
		{email: "deploy@acme.test", name: "Deploy", external: true, sourceBot: true},
		{email: "ci@acme.test", name: "CI", external: true, bot: true},
		{email: "gone@acme.test", name: "Gone", external: true, gone: true},
	}
	for i, p := range people {
		id := uuid.New()
		var deleted any
		if p.gone {
			deleted = time.Now()
		}
		var name any
		if p.name != "" {
			name = p.name
		}
		exec(`INSERT INTO users (id, email_id, display_name, is_external, is_bot, deleted_at) VALUES ($1, $2, $3, $4, $5, $6)`,
			id, p.email, name, p.external, p.bot, deleted)
		md, _ := json.Marshal(map[string]any{"matched_by": "external", "is_bot": p.sourceBot})
		exec(`INSERT INTO import_id_map (import_id, entity_type, source_id, onecamp_uuid, metadata) VALUES ($1, 'user', $2, $3, $4)`,
			job, "src-"+string(rune('a'+i)), id, md)
		if p.invited {
			exec(`INSERT INTO invitations (email, invited_by, status) VALUES ($1, $2, 'sent')`, p.email, admin)
		}
	}
	// The same mailbox under a second source account is still one person.
	exec(`INSERT INTO import_id_map (import_id, entity_type, source_id, onecamp_uuid)
		SELECT $1, 'user', 'src-twin', id FROM users WHERE email_id = 'priya@acme.test'`, job)

	asAdmin := context.WithValue(ctx, helpers.UserInfoContextKey, userModels.UserInfo{
		UserPostgresInfo: userModels.User{Id: admin, EmailID: "admin@acme.test", IsAdmin: true},
	})
	call := func(h http.HandlerFunc, method string, jobID uuid.UUID, body any) (int, []byte) {
		t.Helper()
		raw, _ := json.Marshal(body)
		req := httptest.NewRequest(method, "/", bytes.NewReader(raw))
		rc := chi.NewRouteContext()
		rc.URLParams.Add("jobId", jobID.String())
		req = req.WithContext(context.WithValue(asAdmin, chi.RouteCtxKey, rc))
		rec := httptest.NewRecorder()
		h(rec, req)
		return rec.Code, rec.Body.Bytes()
	}
	offer := func() importBusiness.ImportPeople {
		t.Helper()
		code, body := call(importController.HandleImportPeople, http.MethodGet, job, nil)
		if code != http.StatusOK {
			t.Fatalf("people: %d %s", code, body)
		}
		var out importBusiness.ImportPeople
		if err := json.Unmarshal(body, &out); err != nil {
			t.Fatal(err)
		}
		return out
	}
	emails := func(o importBusiness.ImportPeople) (out []string) {
		for _, p := range o.People {
			out = append(out, p.Email)
		}
		return out
	}

	// 1. Offered: the three placeholders with their own address, once each,
	// in name order. Counted, not offered: the member, the one already
	// invited, the one with a made-up address, the one deactivated here. Bots
	// are neither.
	got := offer()
	if want := []string{"jo@acme.test", "kim@acme.test", "priya@acme.test"}; len(got.People) != 3 ||
		emails(got)[0] != want[0] || emails(got)[1] != want[1] || emails(got)[2] != want[2] {
		t.Fatalf("offered %v, want %v", emails(got), want)
	}
	if got.People[0].Name != "jo" {
		t.Errorf("someone without a name is offered under their address's name, got %q", got.People[0].Name)
	}
	if got.AlreadyMembers != 1 || got.AlreadyInvited != 1 || got.NoEmail != 1 || got.Left != 1 {
		t.Errorf("counts: %+v", got)
	}
	// Three members (the two admins and Sam) of five: room for two.
	if got.Seats.Used != 3 || got.Seats.Limit != 5 || got.Seats.Left == nil || *got.Seats.Left != 2 {
		t.Errorf("seats: %+v", got.Seats)
	}

	// 2. The admin who ran it is told it finished, with the offer's size; the
	// other admin's import and the one refused before it started are not news
	// for them.
	outcomes := func() []importBusiness.ImportOutcome {
		t.Helper()
		code, body := call(importController.HandleImportOutcomes, http.MethodGet, uuid.Nil, nil)
		if code != http.StatusOK {
			t.Fatalf("outcomes: %d %s", code, body)
		}
		var out struct {
			Outcomes []importBusiness.ImportOutcome `json:"outcomes"`
		}
		if err := json.Unmarshal(body, &out); err != nil {
			t.Fatal(err)
		}
		return out.Outcomes
	}
	if o := outcomes(); len(o) != 1 || o[0].JobID != job || o[0].Status != "completed" || o[0].PeopleToInvite != 3 || o[0].Label != "Acme" {
		t.Fatalf("outcomes: %+v", o)
	}

	// 3. Inviting them goes through the workspace's invitation endpoint, the one
	// every invitation goes through, and they leave the offer as invited.
	invite := func(email string) (int, map[string]any) {
		t.Helper()
		code, body := call(userController.AddInvitation, http.MethodPost, uuid.Nil, map[string]string{"email": email})
		var out map[string]any
		_ = json.Unmarshal(body, &out)
		return code, out
	}
	for _, email := range []string{"priya@acme.test", "jo@acme.test"} {
		if code, out := invite(email); code != http.StatusOK || out["invite_link"] == "" {
			t.Fatalf("inviting %s: %d %v", email, code, out)
		}
	}
	got = offer()
	if len(got.People) != 1 || got.People[0].Email != "kim@acme.test" || got.AlreadyInvited != 3 {
		t.Fatalf("after inviting two: %+v", got)
	}

	// 4. A full plan is refused by that same endpoint, with its own reason, and
	// the person stays on offer for when there is room.
	helpers.SeatLimit = "3"
	if code, out := invite("kim@acme.test"); code != http.StatusForbidden || out["code"] != "seat_limit" {
		t.Fatalf("a full plan: %d %v", code, out)
	}
	if got = offer(); len(got.People) != 1 || got.Seats.Left == nil || *got.Seats.Left != 0 {
		t.Fatalf("after a refusal: %+v", got)
	}

	// 5. Dismissed, the news is gone, on every device. Someone else's import is
	// not theirs to dismiss.
	if code, body := call(importController.HandleOutcomeSeen, http.MethodPost, job, nil); code != http.StatusOK {
		t.Fatalf("dismiss: %d %s", code, body)
	}
	if o := outcomes(); len(o) != 0 {
		t.Fatalf("dismissed news came back: %+v", o)
	}
	if code, _ := call(importController.HandleOutcomeSeen, http.MethodPost, othersJob, nil); code != http.StatusNotFound {
		t.Errorf("dismissing another admin's import: %d", code)
	}
	// Planned again and stopped this time: that is news again.
	exec(`UPDATE import_jobs SET status = 'failed', error_message = 'asana auth failed', completed_at = NOW() + interval '1 second' WHERE id = $1`, job)
	if o := outcomes(); len(o) != 1 || o[0].Status != "failed" || o[0].Error != "asana auth failed" || o[0].PeopleToInvite != 0 {
		t.Fatalf("a later failure: %+v", o)
	}

	// 6. A job that is gone says so.
	if code, _ := call(importController.HandleImportPeople, http.MethodGet, uuid.New(), nil); code != http.StatusNotFound {
		t.Errorf("people of a missing import: %d", code)
	}
}
