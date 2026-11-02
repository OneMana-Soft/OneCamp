//go:build integration
// +build integration

package integration_test

// A withdrawn invitation is kept, with who withdrew it and when (V-184 head
// 3c, the owner's rule that every delete of a business record is soft).
//
// An admin withdrawing an invitation from the list (DELETE
// /deleteInvitation/{email}) DELETEd its rows, so who invited whom, and who
// withdrew it and when, went with them. Now each row stays, marked deleted by
// the admin, and its link, the invited person's credential, is cleared: a
// withdrawn invitation never lets anyone in. No read sees it: the admin's
// list, the sign-up link, sending it again, Google and GitHub admitting the
// address, and an import's "already invited". Inviting the address again
// makes a new invitation, and the withdrawn one stays on record.
//
// Run: go test -tags=integration ./tests/integration/ -run 'TestAWithdrawnInvitationIsKeptWithWhoAndWhen|TestInvitationsSoftDeleteMigrationUpDownUp' -v

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/google/uuid"

	userBusiness "github.com/akashc777/OneCamp/business/User"
	authController "github.com/akashc777/OneCamp/controllers/Auth"
	userController "github.com/akashc777/OneCamp/controllers/User"
	userDomain "github.com/akashc777/OneCamp/domain/User"
	"github.com/akashc777/OneCamp/helpers"
	importModels "github.com/akashc777/OneCamp/models/postgres/Import"
	"github.com/akashc777/OneCamp/tests/integration"
)

func TestAWithdrawnInvitationIsKeptWithWhoAndWhen(t *testing.T) {
	t.Setenv("RESEND_API_KEY", "")
	t.Setenv("APP_SECRET_KEK", "invitations-soft-delete-integration-kek")
	lr := setupLandReady(t)
	ctx := lr.ctx
	owner := lr.owner.UserPostgresInfo.Id
	const kim, lee = "kim@example.test", "lee@example.test"

	// Lee came in with an import, as a guest. The import offers to invite the
	// people it brought, except those already invited.
	leeID, job := uuid.New(), uuid.New()
	for _, q := range []struct {
		sql  string
		args []any
	}{
		{`INSERT INTO import_jobs (id, provider, source_workspace_name, source, status, started_at, completed_at, triggered_by)
			VALUES ($1, 'asana', 'Acme', 'api', 'completed', NOW() - interval '1 hour', NOW(), $2)`, []any{job, owner}},
		{`INSERT INTO users (id, email_id, display_name, is_external) VALUES ($1, $2, 'Lee', true)`, []any{leeID, lee}},
		{`INSERT INTO import_id_map (import_id, entity_type, source_id, onecamp_uuid) VALUES ($1, 'user', 'src-lee', $2)`, []any{job, leeID}},
	} {
		if _, err := lr.env.PG.Exec(q.sql, q.args...); err != nil {
			t.Fatalf("%s: %v", q.sql, err)
		}
	}
	leeInvited := func() bool {
		t.Helper()
		people, err := importModels.ListImportedPeople(ctx, job)
		if err != nil || len(people) != 1 {
			t.Fatalf("the import's people: %+v %v", people, err)
		}
		return people[0].Invited
	}

	// tokenFor is the link of the invitation to email that still has one, as
	// the admin's list copies it: its sealed copy, opened.
	tokenFor := func(email string) string {
		t.Helper()
		var hash []byte
		var sealed string
		if err := lr.env.PG.QueryRow(`SELECT token_hash, token_sealed FROM invitations WHERE email = $1 AND token_hash IS NOT NULL`, email).Scan(&hash, &sealed); err != nil {
			t.Fatal(err)
		}
		token := helpers.OpenLinkSecret(sealed, hash)
		if token == "" {
			t.Fatal("the invitation's link can't be opened from its sealed copy")
		}
		return token
	}
	// opens says whether the sign-up page takes a link's token.
	opens := func(token string) bool {
		t.Helper()
		rec := httptest.NewRecorder()
		authController.ValidateInvitationToken(rec, httptest.NewRequest(http.MethodGet, "/auth/validate-token?token="+url.QueryEscape(token), nil))
		return rec.Code == http.StatusOK
	}
	// listed is how many times the admin's list shows Kim; usable is whether
	// Google and GitHub would let her in on her invitation.
	listed := func() (n int) {
		t.Helper()
		invs, err := userBusiness.GetAllInvitations(ctx)
		if err != nil {
			t.Fatal(err)
		}
		for _, inv := range invs {
			if strings.EqualFold(inv.Email, kim) {
				n++
			}
		}
		return n
	}
	usable := func() bool {
		t.Helper()
		ok, err := userDomain.HasUsableInvitation(ctx, kim)
		if err != nil {
			t.Fatal(err)
		}
		return ok
	}

	act(t, lr, userController.AddInvitation, http.MethodPost, map[string]string{"email": kim})
	act(t, lr, userController.AddInvitation, http.MethodPost, map[string]string{"email": lee})
	token := tokenFor(kim)
	if l, o, u := listed(), opens(token), usable(); l != 1 || !o || !u {
		t.Fatalf("invited: listed %d times, link opens %v, usable for Google and GitHub %v; want once, and both", l, o, u)
	}
	if !leeInvited() {
		t.Fatal("invited: the import doesn't count Lee as invited")
	}

	act(t, lr, userController.DeleteInvitation, http.MethodDelete, nil, "email", kim)
	act(t, lr, userController.DeleteInvitation, http.MethodDelete, nil, "email", lee)

	// The invitation is kept: whom it was for, who sent it, who withdrew it
	// and when. Its link is gone with the withdrawal.
	var rows int
	if err := lr.env.PG.QueryRow(`SELECT count(*) FROM invitations WHERE LOWER(email) = $1`, kim).Scan(&rows); err != nil {
		t.Fatal(err)
	}
	if rows != 1 {
		t.Fatalf("the invitation the owner withdrew from Kim: %d rows; want it kept, and with it who invited whom", rows)
	}
	var invitedBy, by uuid.NullUUID
	var withdrawn, cleared bool
	if err := lr.env.PG.QueryRow(`SELECT invited_by, deleted_at IS NOT NULL, deleted_by,
			token_hash IS NULL AND token_sealed = '' AND token IS NULL
		FROM invitations WHERE LOWER(email) = $1`, kim).Scan(&invitedBy, &withdrawn, &by, &cleared); err != nil {
		t.Fatal(err)
	}
	if invitedBy.UUID != owner || !withdrawn || by.UUID != owner || !cleared {
		t.Errorf("the kept invitation: invited by %s, withdrawn %v by %s, link cleared %v; want invited and withdrawn by the owner %s, link cleared",
			invitedBy.UUID, withdrawn, by.UUID, cleared, owner)
	}

	// No read sees it.
	if l, o, u := listed(), opens(token), usable(); l != 0 || o || u {
		t.Errorf("withdrawn: listed %d times, link opens %v, usable for Google and GitHub %v; want none", l, o, u)
	}
	if _, err := userBusiness.AdmitOAuthUser(ctx, kim, "Kim", "google", ""); !errors.Is(err, userBusiness.ErrNotInvited) {
		t.Errorf("Google admitting Kim after the withdrawal: %v; want ErrNotInvited", err)
	}
	if leeInvited() {
		t.Error("withdrawn: the import still counts Lee as invited, so it won't offer to invite her")
	}
	raw, _ := json.Marshal(map[string]string{"email": kim})
	rec := httptest.NewRecorder()
	userController.ResendInvitation(rec, httptest.NewRequest(http.MethodPost, "/", strings.NewReader(string(raw))).
		WithContext(context.WithValue(ctx, helpers.UserInfoContextKey, lr.owner)))
	if rec.Code != http.StatusConflict {
		t.Errorf("sending the withdrawn invitation again: %d %s; want 409, nothing to send again", rec.Code, rec.Body.String())
	}

	// Invited again: a new invitation, with a new link; the withdrawn one
	// stays on record, and its link stays dead.
	act(t, lr, userController.AddInvitation, http.MethodPost, map[string]string{"email": kim})
	var live int
	if err := lr.env.PG.QueryRow(`SELECT count(*), count(*) FILTER (WHERE deleted_at IS NULL) FROM invitations WHERE LOWER(email) = $1`, kim).Scan(&rows, &live); err != nil {
		t.Fatal(err)
	}
	if rows != 2 || live != 1 {
		t.Errorf("invited again: %d rows, %d live; want the withdrawn one kept and one new", rows, live)
	}
	again := tokenFor(kim)
	if l, o, u := listed(), opens(again), usable(); again == token || l != 1 || !o || !u {
		t.Errorf("invited again: a new link %v, listed %d times, link opens %v, usable %v; want a new link, once, and both", again != token, l, o, u)
	}
	if opens(token) {
		t.Error("invited again: the withdrawn invitation's link opens again")
	}

	// Kim joins with Google on the new invitation: it is marked joined, and
	// the withdrawn one stays as it was withdrawn.
	if _, err := userBusiness.AdmitOAuthUser(ctx, kim, "Kim", "google", ""); err != nil {
		t.Fatalf("Google admitting Kim on her new invitation: %v", err)
	}
	var newStatus, oldStatus string
	if err := lr.env.PG.QueryRow(`SELECT
			max(status) FILTER (WHERE deleted_at IS NULL), max(status) FILTER (WHERE deleted_at IS NOT NULL)
		FROM invitations WHERE LOWER(email) = $1`, kim).Scan(&newStatus, &oldStatus); err != nil {
		t.Fatal(err)
	}
	if newStatus != "joined" || oldStatus != "sent" {
		t.Errorf("Kim joined: the new invitation %q, the withdrawn one %q; want joined, and sent as it was withdrawn", newStatus, oldStatus)
	}
}

// TestInvitationsSoftDeleteMigrationUpDownUp: the migration goes down and up
// again, twice, found by its name so a renumber at landing keeps it. Its down
// takes the withdrawn invitations away first, as the schema before it never
// kept them, so none comes back live when the columns go; what is live stays.
func TestInvitationsSoftDeleteMigrationUpDownUp(t *testing.T) {
	read := func(dir string) string {
		t.Helper()
		files, err := filepath.Glob("../../migrations/*_invitations_soft_delete." + dir + ".sql")
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
	// Ria's invitation is live; Kim's was withdrawn.
	if _, err := env.PG.Exec(`INSERT INTO invitations (email, invited_by, status, deleted_at, deleted_by)
		VALUES ('ria@example.test', $1, 'sent', NULL, NULL), ('kim@example.test', $1, 'sent', NOW(), $1)`, sam); err != nil {
		t.Fatal(err)
	}
	count := func(email string) int {
		t.Helper()
		var n int
		if err := env.PG.QueryRow(`SELECT count(*) FROM invitations WHERE email = $1`, email).Scan(&n); err != nil {
			t.Fatal(err)
		}
		return n
	}
	for i, step := range []string{down, up, down, up} {
		if _, err := env.PG.Exec(step); err != nil {
			t.Fatalf("step %d: %v", i, err)
		}
		wantColumns := i%2 == 1
		for _, column := range []string{"deleted_at", "deleted_by"} {
			if got := columnExists(t, env, "invitations", column); got != wantColumns {
				t.Fatalf("step %d: invitations.%s exists = %v", i, column, got)
			}
		}
		if n := count("ria@example.test"); n != 1 {
			t.Errorf("step %d: Ria's live invitation: %d rows, want 1", i, n)
		}
		if n := count("kim@example.test"); n != 0 {
			t.Errorf("step %d: Kim's withdrawn invitation is back: %d rows", i, n)
		}
	}
}
