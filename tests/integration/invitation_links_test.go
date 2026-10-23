//go:build integration
// +build integration

package integration_test

// Invitation links against real Postgres 12 (audit A1): only a link's hash
// and its sealed copy are kept, so a copy of the table lets nobody sign up as
// someone invited; a link sent before that still lets its person in, through
// the minute the old server runs on the new schema and the new one's start;
// and the migration goes down and up again.
//
// Run: go test -tags=integration ./tests/integration/ -run '^(TestInvitationLinksAreKeptOnlyAsTheirHash|TestInvitationLinksMigrationUpDownUp)$' -v

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"

	"github.com/google/uuid"

	authController "github.com/akashc777/OneCamp/controllers/Auth"
	userController "github.com/akashc777/OneCamp/controllers/User"
	"github.com/akashc777/OneCamp/helpers"
	linkSecretModel "github.com/akashc777/OneCamp/models/postgres/LinkSecret"
)

func invitationLinksMigration(t *testing.T, dir string) string {
	t.Helper()
	_, here, _, _ := runtime.Caller(0)
	matches, _ := filepath.Glob(filepath.Join(filepath.Dir(here), "..", "..", "migrations", "*_invitation_link_hash."+dir+".sql"))
	if len(matches) != 1 {
		t.Fatalf("want one invitation_link_hash %s migration, found %v", dir, matches)
	}
	b, err := os.ReadFile(matches[0])
	if err != nil {
		t.Fatal(err)
	}
	return string(b)
}

// invitationLinkTaken is whether the sign-up page takes an invitation's link.
func invitationLinkTaken(token string) bool {
	rec := httptest.NewRecorder()
	authController.ValidateInvitationToken(rec, httptest.NewRequest(http.MethodGet, "/auth/validate-token?token="+url.QueryEscape(token), nil))
	return rec.Code == http.StatusOK
}

// invitationsHolding is how many invitations hold text anywhere in their row,
// where a copy of the table would find it.
func invitationsHolding(t *testing.T, lr *landReady, text string) int {
	t.Helper()
	var n int
	if err := lr.env.PG.QueryRow(`SELECT COUNT(*) FROM invitations i WHERE strpos(i::text, $1) > 0`, text).Scan(&n); err != nil {
		t.Fatal(err)
	}
	return n
}

func TestInvitationLinksAreKeptOnlyAsTheirHash(t *testing.T) {
	t.Setenv("RESEND_API_KEY", "")
	t.Setenv("APP_SECRET_KEK", "invitation-links-integration-kek")
	lr := setupLandReady(t)
	asOwner := context.WithValue(lr.ctx, helpers.UserInfoContextKey, lr.owner)
	rec := httptest.NewRecorder()
	userController.AddInvitation(rec, httptest.NewRequest(http.MethodPost, "/", strings.NewReader(`{"email":"kim@example.test"}`)).WithContext(asOwner))
	var answer struct {
		InviteLink string `json:"invite_link"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &answer); err != nil || rec.Code != http.StatusOK || answer.InviteLink == "" {
		t.Fatalf("inviting: %d %s", rec.Code, rec.Body.String())
	}
	link, err := url.Parse(answer.InviteLink)
	if err != nil {
		t.Fatal(err)
	}
	token := link.Query().Get("token")
	if n := invitationsHolding(t, lr, token); n != 0 {
		t.Errorf("%d invitations keep the link in the clear", n)
	}
	if !invitationLinkTaken(token) {
		t.Error("the sign-up page doesn't take the link")
	}
	if invitationLinkTaken(token + "x") {
		t.Error("the sign-up page takes a link nobody was sent")
	}
}

// The migration only adds, so the server from before keeps working on it
// until the new one starts: it finds an invitation by its link, sends one
// again with a new link, and invites someone. The new server's start hashes
// what it wrote, and a link from before, which can't be sealed, lets its
// person in by its hash alone. Going down puts no link back in the clear:
// the invitation is sent again.
func TestInvitationLinksMigrationUpDownUp(t *testing.T) {
	lr := setupLandReady(t)
	up, down := invitationLinksMigration(t, "up"), invitationLinksMigration(t, "down")
	exec := func(q string, args ...any) {
		t.Helper()
		if _, err := lr.env.PG.Exec(q, args...); err != nil {
			t.Fatalf("%.60s: %v", q, err)
		}
	}
	link := func() string { return strings.ReplaceAll(uuid.NewString()+uuid.NewString(), "-", "") }
	exec(down)
	// Lee's link as invitations sent it before: 64 hex characters, kept as they are.
	mailed := link()
	exec(`INSERT INTO invitations (email, invited_by, status, token, token_expires_at)
		VALUES ('lee@example.test', $1, 'sent', $2, NOW() + interval '3 days')`, lr.owner.UserPostgresInfo.Id, mailed)
	exec(up)

	// The old server, still running: its own statements.
	var lee string
	if err := lr.env.PG.QueryRow(`SELECT id FROM invitations WHERE token = $1`, mailed).Scan(&lee); err != nil {
		t.Fatalf("the old server looking Lee's link up: %v", err)
	}
	leeAgain, maxLink := link(), link()
	exec(`UPDATE invitations SET token = $1, token_expires_at = NOW() + interval '7 days', status = 'sent', invited_by = $3 WHERE id = $2`,
		leeAgain, lee, lr.owner.UserPostgresInfo.Id)
	exec(`INSERT INTO invitations (email, invited_by, status, token, token_expires_at) VALUES ('max@example.test', $1, 'sent', $2, NOW() + interval '7 days')`,
		lr.owner.UserPostgresInfo.Id, maxLink)

	// The new server starts.
	if err := linkSecretModel.HashLeftInClear(lr.ctx); err != nil {
		t.Fatal(err)
	}
	for _, l := range []string{mailed, leeAgain, maxLink} {
		if n := invitationsHolding(t, lr, l); n != 0 {
			t.Errorf("%d invitations keep a link in the clear", n)
		}
	}
	if !invitationLinkTaken(leeAgain) || !invitationLinkTaken(maxLink) {
		t.Errorf("the links the old server sent: Lee's again %v, Max's %v", invitationLinkTaken(leeAgain), invitationLinkTaken(maxLink))
	}
	if invitationLinkTaken(mailed) {
		t.Error("Lee's first link is still taken after it was sent again")
	}
	exec(down)
	if n := invitationsHolding(t, lr, leeAgain); n != 0 {
		t.Errorf("down: %d invitations hold a link again", n)
	}
	exec(up)
	if invitationLinkTaken(leeAgain) {
		t.Error("up again: a link whose hash went down is taken")
	}
}
