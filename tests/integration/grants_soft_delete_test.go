//go:build integration
// +build integration

package integration_test

// A calendar share and a workspace admin role taken away are kept, with who
// took them and when (V-184 head 3b, the owner's rule that every delete of a
// business record is soft).
//
// calendar_shares says who may see or change someone's calendar, and
// admin_users who runs the workspace. Stopping a share, and taking someone's
// admin role away (an admin on the admin page, or the directory at sign-in),
// each DELETEd the row, so nobody could say afterwards who had been given
// what, or who took it away and when. Now the row stays, marked deleted by
// whoever took it (nobody, for the directory), no read sees it, and giving it
// again brings the same row back.
//
// Run: go test -tags=integration ./tests/integration/ -run 'TestATaken(CalendarShare|AdminRole)IsKeptWithWhoAndWhen|TestGrantsSoftDeleteMigrationUpDownUp' -v

import (
	"context"
	"encoding/json"
	"net/http"
	"os"
	"path/filepath"
	"testing"

	"github.com/google/uuid"

	userBusiness "github.com/akashc777/OneCamp/business/User"
	eventController "github.com/akashc777/OneCamp/controllers/Calendar"
	userController "github.com/akashc777/OneCamp/controllers/User"
	userDomain "github.com/akashc777/OneCamp/domain/User"
	calendarModel "github.com/akashc777/OneCamp/models/postgres/CalendarV2"
	userModels "github.com/akashc777/OneCamp/models/postgres/User"
	"github.com/akashc777/OneCamp/tests/integration"
)

func TestATakenCalendarShareIsKeptWithWhoAndWhen(t *testing.T) {
	t.Setenv("CALENDAR_STORE", "postgres")
	env, dg := calendarStores(t)
	ctx := context.Background()
	sam, ria := seedCalendarPerson(t, env.PG, dg, "Sam"), seedCalendarPerson(t, env.PG, dg, "Ria")
	samID, riaID := sam.UserPostgresInfo.Id, ria.UserPostgresInfo.Id
	own, err := calendarModel.PersonalCalendar(ctx, env.PG, samID, uuid.New(), "UTC")
	if err != nil {
		t.Fatal(err)
	}
	path := "/calendar/calendars/" + own.String() + "/shares"
	share := func(level string) {
		t.Helper()
		rec := call(t, eventController.ShareCalendar, http.MethodPut, path+"/"+riaID.String(), sam, `{"level":"`+level+`"}`,
			map[string]string{"calendarId": own.String(), "userId": riaID.String()})
		if rec.Code != http.StatusNoContent {
			t.Fatalf("Sam shares his calendar with Ria at %q: HTTP %d %s", level, rec.Code, rec.Body.String())
		}
	}
	// Every read of the share: the list on Sam's calendar, Ria's level on it,
	// and whether it is among the calendars shared with her.
	reads := func() (listed int, level string, sharedWithRia bool) {
		t.Helper()
		rec := call(t, eventController.CalendarShares, http.MethodGet, path, sam, "", map[string]string{"calendarId": own.String()})
		var body struct {
			Data []json.RawMessage `json:"data"`
		}
		if rec.Code != http.StatusOK || json.Unmarshal(rec.Body.Bytes(), &body) != nil {
			t.Fatalf("Sam lists his calendar's shares: HTTP %d %s", rec.Code, rec.Body.String())
		}
		levels, err := calendarModel.SharesFor(ctx, riaID, []uuid.UUID{own})
		if err != nil {
			t.Fatal(err)
		}
		with, err := calendarModel.SharedWith(ctx, riaID)
		if err != nil {
			t.Fatal(err)
		}
		for _, c := range with {
			sharedWithRia = sharedWithRia || c.ID == own
		}
		return len(body.Data), levels[own], sharedWithRia
	}

	// Sam lets Ria edit his calendar, then stops sharing it.
	share(calendarModel.ShareEdit)
	share("")
	if listed, level, with := reads(); listed != 0 || level != "" || with {
		t.Errorf("after Sam stopped sharing: listed %d, Ria's level %q, shared with her %v; want nothing", listed, level, with)
	}
	// The share is kept: the level Ria had, who stopped it, and when.
	var level string
	if err := env.PG.QueryRow(`SELECT level FROM calendar_shares WHERE calendar_id = $1 AND person_id = $2`, own, riaID).Scan(&level); err != nil {
		t.Fatalf("the share Sam stopped is gone, and with it who had it: %v", err)
	}
	var stopped bool
	var by uuid.NullUUID
	if err := env.PG.QueryRow(`SELECT deleted_at IS NOT NULL, deleted_by FROM calendar_shares WHERE calendar_id = $1 AND person_id = $2`,
		own, riaID).Scan(&stopped, &by); err != nil {
		t.Fatal(err)
	}
	if level != calendarModel.ShareEdit || !stopped || by.UUID != samID {
		t.Errorf("the kept share: level %q, stopped %v, by %s; want edit, stopped, by Sam %s", level, stopped, by.UUID, samID)
	}

	// Shared again, to see details: the same row comes back, live, given by Sam.
	share(calendarModel.ShareDetails)
	var rows int
	var live bool
	if err := env.PG.QueryRow(`SELECT count(*), bool_and(deleted_at IS NULL AND deleted_by IS NULL AND level = 'details' AND granted_by = $3)
		FROM calendar_shares WHERE calendar_id = $1 AND person_id = $2`, own, riaID, samID).Scan(&rows, &live); err != nil {
		t.Fatal(err)
	}
	if rows != 1 || !live {
		t.Errorf("shared again: %d rows, live at details from Sam %v; want its one row back", rows, live)
	}
	if listed, level, with := reads(); listed != 1 || level != calendarModel.ShareDetails || !with {
		t.Errorf("shared again: listed %d, Ria's level %q, shared with her %v; want 1, details, true", listed, level, with)
	}
}

func TestATakenAdminRoleIsKeptWithWhoAndWhen(t *testing.T) {
	t.Setenv("RESEND_API_KEY", "")
	t.Setenv("APP_SECRET_KEK", "grants-soft-delete-integration-kek")
	lr := setupLandReady(t)
	ctx := lr.ctx
	danaID, _, err := userBusiness.CreateUserWithMethod(ctx, "dana@example.test", "Dana", nil, userModels.AuthMethodEmail, false)
	if err != nil {
		t.Fatal(err)
	}
	dana := map[string]string{"user_uuid": danaID.String()}
	// Every read that says whether Dana runs the workspace: her signed-in
	// profile, the sign-in's flags, the admins' ids and the admin page's list.
	reads := func() (profile, signIn, ids, page bool) {
		t.Helper()
		var err error
		profile = lr.info(danaID).UserPostgresInfo.IsAdmin
		if signIn, _, err = userDomain.GetUserSignInFlags(ctx, danaID); err != nil {
			t.Fatal(err)
		}
		admins, err := userDomain.AdminUserIDs(ctx)
		if err != nil {
			t.Fatal(err)
		}
		for _, id := range admins {
			ids = ids || id == danaID.String()
		}
		list, _, err := userBusiness.GetAllAdminUsers(ctx, 0, 50)
		if err != nil {
			t.Fatal(err)
		}
		for _, u := range list {
			page = page || u.Id == danaID
		}
		return profile, signIn, ids, page
	}

	act(t, lr, userController.CreateAdmin, http.MethodPost, dana)
	if p, s, i, g := reads(); !p || !s || !i || !g {
		t.Fatalf("made an admin: profile %v, sign-in %v, admin ids %v, admin page %v; want all", p, s, i, g)
	}
	act(t, lr, userController.RemoveAdmin, http.MethodPost, dana)
	if p, s, i, g := reads(); p || s || i || g {
		t.Errorf("the role taken away: profile %v, sign-in %v, admin ids %v, admin page %v; want none", p, s, i, g)
	}
	// The role is kept: whose it was, who took it away, and when.
	var email string
	if err := lr.env.PG.QueryRow(`SELECT email_id FROM admin_users WHERE email_id = 'dana@example.test'`).Scan(&email); err != nil {
		t.Fatalf("the admin role the owner took from Dana is gone, and with it who had it: %v", err)
	}
	var taken bool
	var by uuid.NullUUID
	if err := lr.env.PG.QueryRow(`SELECT deleted_at IS NOT NULL, deleted_by FROM admin_users WHERE email_id = 'dana@example.test'`).Scan(&taken, &by); err != nil {
		t.Fatal(err)
	}
	if owner := lr.owner.UserPostgresInfo.Id; !taken || by.UUID != owner {
		t.Errorf("the kept role: taken %v, by %s; want taken, by the owner %s", taken, by.UUID, owner)
	}

	// Made an admin again: the same row comes back, live.
	act(t, lr, userController.CreateAdmin, http.MethodPost, dana)
	var rows int
	var live bool
	if err := lr.env.PG.QueryRow(`SELECT count(*), bool_and(deleted_at IS NULL AND deleted_by IS NULL) FROM admin_users WHERE LOWER(email_id) = 'dana@example.test'`).Scan(&rows, &live); err != nil {
		t.Fatal(err)
	}
	if rows != 1 || !live {
		t.Errorf("made an admin again: %d rows, live %v; want its one row back", rows, live)
	}
	if p, s, i, g := reads(); !p || !s || !i || !g {
		t.Errorf("made an admin again: profile %v, sign-in %v, admin ids %v, admin page %v; want all", p, s, i, g)
	}
}

// TestGrantsSoftDeleteMigrationUpDownUp: the migration goes down and up again,
// twice, found by its name so a renumber at landing keeps it. Its down takes
// the taken rows away first, as the schema before it never kept them, so a
// share stopped or an admin role taken away never comes back live when the
// columns go; what is live stays.
func TestGrantsSoftDeleteMigrationUpDownUp(t *testing.T) {
	read := func(dir string) string {
		t.Helper()
		files, err := filepath.Glob("../../migrations/*_grants_soft_delete." + dir + ".sql")
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
	sam, ria, kim := uuid.New(), uuid.New(), uuid.New()
	for _, id := range []uuid.UUID{sam, ria, kim} {
		if _, err := env.PG.Exec(`INSERT INTO users (id, email_id, username, display_name, created_at, updated_at) VALUES ($1, $2, $3, $4, NOW(), NOW())`,
			id, id.String()+"@example.test", id.String()[:8], id.String()[:8]); err != nil {
			t.Fatal(err)
		}
	}
	cal := uuid.New()
	if _, err := env.PG.Exec(`INSERT INTO calendars (id, kind, owner_id, tz) VALUES ($1, 'personal', $2, 'UTC')`, cal, sam); err != nil {
		t.Fatal(err)
	}
	// Ria's share is live and Kim's was stopped; Lee is an admin and Max was one.
	if _, err := env.PG.Exec(`INSERT INTO calendar_shares (calendar_id, person_id, level, granted_by, deleted_at, deleted_by)
		VALUES ($1, $2, 'edit', $4, NULL, NULL), ($1, $3, 'details', $4, NOW(), $4)`, cal, ria, kim, sam); err != nil {
		t.Fatal(err)
	}
	if _, err := env.PG.Exec(`INSERT INTO admin_users (email_id, deleted_at, deleted_by) VALUES ('lee@example.test', NULL, NULL), ('max@example.test', NOW(), $1)`, sam); err != nil {
		t.Fatal(err)
	}
	count := func(q string, args ...any) int {
		t.Helper()
		var n int
		if err := env.PG.QueryRow(q, args...).Scan(&n); err != nil {
			t.Fatal(err)
		}
		return n
	}
	for i, step := range []string{down, up, down, up} {
		if _, err := env.PG.Exec(step); err != nil {
			t.Fatalf("step %d: %v", i, err)
		}
		wantColumns := i%2 == 1
		for _, tc := range [][2]string{{"calendar_shares", "deleted_at"}, {"calendar_shares", "deleted_by"}, {"admin_users", "deleted_at"}, {"admin_users", "deleted_by"}} {
			if got := columnExists(t, env, tc[0], tc[1]); got != wantColumns {
				t.Fatalf("step %d: %s.%s exists = %v", i, tc[0], tc[1], got)
			}
		}
		if n := count(`SELECT count(*) FROM calendar_shares WHERE calendar_id = $1 AND person_id = $2`, cal, ria); n != 1 {
			t.Errorf("step %d: Ria's live share: %d rows, want 1", i, n)
		}
		if n := count(`SELECT count(*) FROM calendar_shares WHERE calendar_id = $1 AND person_id = $2`, cal, kim); n != 0 {
			t.Errorf("step %d: Kim's stopped share is back: %d rows", i, n)
		}
		if n := count(`SELECT count(*) FROM admin_users WHERE email_id = 'lee@example.test'`); n != 1 {
			t.Errorf("step %d: Lee's live admin role: %d rows, want 1", i, n)
		}
		if n := count(`SELECT count(*) FROM admin_users WHERE email_id = 'max@example.test'`); n != 0 {
			t.Errorf("step %d: the admin role taken from Max is back: %d rows", i, n)
		}
	}
}
