//go:build integration
// +build integration

package integration_test

// First-admin setup (POST /auth/admin-setup) is one-time. Once the workspace
// has been given an admin it stays shut, whatever becomes of the admin rows.
// Released v2.76.0 offered it again whenever admin_users was empty, and on an
// install with no address pinned, anyone who reached the page could make
// themselves the admin. Through the handlers (CheckAdminSetup, AdminSetup),
// with the database real.
//
// Run: go test -tags=integration ./tests/integration/ -run 'TestFirstAdminSetup' -v

import (
	"bytes"
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	userBusiness "github.com/akashc777/OneCamp/business/User"
	authController "github.com/akashc777/OneCamp/controllers/Auth"
	"github.com/akashc777/OneCamp/helpers"
	"github.com/akashc777/OneCamp/initializers/postgresInit"
	ldapService "github.com/akashc777/OneCamp/services/LDAP"
	"github.com/akashc777/OneCamp/tests/integration"
	"github.com/google/uuid"
)

// setupOffered is GET /auth/admin-setup-required: whether the setup page is
// offered.
func setupOffered(t *testing.T) bool {
	t.Helper()
	rec := httptest.NewRecorder()
	authController.CheckAdminSetup(rec, httptest.NewRequest(http.MethodGet, "/auth/admin-setup-required", nil))
	if rec.Code != http.StatusOK {
		t.Fatalf("admin-setup-required: %d %s", rec.Code, rec.Body.String())
	}
	var body struct {
		Required bool `json:"required"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
		t.Fatal(err)
	}
	return body.Required
}

// claimSetup posts the first-admin setup for email, as anyone who reaches the
// page can.
func claimSetup(email string) *httptest.ResponseRecorder {
	raw, _ := json.Marshal(map[string]string{"email": email, "password": "Correct-Horse-Battery-9", "username": strings.Split(email, "@")[0]})
	rec := httptest.NewRecorder()
	authController.AdminSetup(rec, httptest.NewRequest(http.MethodPost, "/auth/admin-setup", bytes.NewReader(raw)))
	return rec
}

// countRows is the one number q selects.
func countRows(t *testing.T, pg *sql.DB, q string) int {
	t.Helper()
	var n int
	if err := pg.QueryRow(q).Scan(&n); err != nil {
		t.Fatal(err)
	}
	return n
}

// assertSetupShut: the page is not offered, and a stranger's claim is refused
// and leaves no account and no admin role behind.
func assertSetupShut(t *testing.T, pg *sql.DB, when string) {
	t.Helper()
	if setupOffered(t) {
		t.Errorf("%s: the first-admin setup is offered again", when)
	}
	if rec := claimSetup("mallory@example.test"); rec.Code == http.StatusOK {
		t.Errorf("%s: a stranger's first-admin claim was taken: %d %s", when, rec.Code, rec.Body.String())
	}
	if n := countRows(t, pg, `SELECT (SELECT count(*) FROM users WHERE email_id = 'mallory@example.test')
		+ (SELECT count(*) FROM admin_users WHERE email_id = 'mallory@example.test')`); n != 0 {
		t.Errorf("%s: the stranger has %d account or admin rows", when, n)
	}
}

// upgrade applies the grants migration's up file, as upgrading does to an
// install from before it, and says how many files it applied. Found by name,
// so a renumber at landing keeps it. A tree without the migration applies
// none, and the workspace stays as an older version leaves it.
func upgrade(t *testing.T, pg *sql.DB) int {
	t.Helper()
	files, err := filepath.Glob("../../migrations/*_grants_soft_delete.up.sql")
	if err != nil {
		t.Fatal(err)
	}
	for _, f := range files {
		b, err := os.ReadFile(f)
		if err != nil {
			t.Fatal(err)
		}
		if _, err := pg.Exec(string(b)); err != nil {
			t.Fatalf("%s: %v", filepath.Base(f), err)
		}
	}
	return len(files)
}

// beforeTheMark puts the workspace as an install from before the setup mark
// leaves it: no mark, and no admin row at all (its last admin role was taken
// away, which deleted the row then).
func beforeTheMark(t *testing.T, pg *sql.DB) {
	t.Helper()
	if _, err := pg.Exec(`DELETE FROM admin_users`); err != nil {
		t.Fatal(err)
	}
	if _, err := pg.Exec(`DELETE FROM system_configs WHERE key = 'first_admin_setup'`); err != nil {
		t.Fatal(err)
	}
}

// startedInstall connects what setup reads and writes: Postgres, the cache,
// the graph (a new account is made in both stores), with no seat cap and no
// address pinned at install. It gives the workspace the users rows every
// start makes before anyone has set it up: the automation bot's.
func startedInstall(t *testing.T) *integration.Env {
	t.Helper()
	t.Setenv("JWT_SECRET", "admin-setup-once-integration-secret")
	t.Setenv("ADMIN_EMAIL_ID", "")
	env := integration.SetupEnv(t)
	ctx := context.Background()
	if err := postgresInit.ConnectPostgres(ctx, env.DSN); err != nil {
		t.Fatal(err)
	}
	integration.SetupRedis(t)
	integration.SetupDgraph(t)
	oldLimit := helpers.SeatLimit
	t.Cleanup(func() { helpers.SeatLimit = oldLimit })
	helpers.SeatLimit = ""
	if _, err := userBusiness.EnsureAutomationBot(ctx); err != nil {
		t.Fatal(err)
	}
	if n := countRows(t, env.PG, `SELECT count(*) FROM users WHERE is_bot`); n == 0 {
		t.Fatal("the automation bot made no users row: this test no longer stands for a started install")
	}
	return env
}

// The directory takes the last admin role away: setup stays shut, and stays
// shut even when the role's row is gone for good (as the grants migration's
// down removes taken rows, or an operator might by hand).
func TestFirstAdminSetupStaysShutWhenTheDirectoryTakesTheLastAdmin(t *testing.T) {
	const adminGroup = "CN=OneCamp Admins,OU=Groups,DC=example,DC=test"
	t.Setenv("JWT_SECRET", "admin-setup-once-integration-secret")
	t.Setenv("ADMIN_EMAIL_ID", "")
	t.Setenv("LDAP_ENABLED", "true")
	t.Setenv("LDAP_HOST", "directory.example.test")
	t.Setenv("LDAP_BASE_DN", "dc=example,dc=test")
	t.Setenv("LDAP_USER_FILTER", "(uid=%s)")
	t.Setenv("LDAP_ADMIN_GROUPS", "OneCamp Admins")
	env := integration.SetupEnv(t)
	ctx := context.Background()
	if err := postgresInit.ConnectPostgres(ctx, env.DSN); err != nil {
		t.Fatal(err)
	}
	integration.SetupRedis(t)
	oldLimit := helpers.SeatLimit
	t.Cleanup(func() { helpers.SeatLimit = oldLimit })
	helpers.SeatLimit = ""

	var groups []string // what the directory says frank is in, each sign-in
	t.Cleanup(authController.UseDirectoryForTest(func(_ *ldapService.LDAPClient, who, password string) (*ldapService.LDAPUser, error) {
		return &ldapService.LDAPUser{DN: "uid=" + who + ",dc=example,dc=test", Email: who + "@example.test", Username: who, Groups: groups}, nil
	}))
	if _, err := env.PG.Exec(`INSERT INTO users (id, email_id, username, signup_method, is_sso_managed, created_at, updated_at)
		VALUES ($1, 'frank@example.test', 'frank', 'ldap', true, NOW(), NOW())`, uuid.New()); err != nil {
		t.Fatal(err)
	}
	signIn := func() {
		t.Helper()
		raw, _ := json.Marshal(map[string]string{"username_or_email": "frank", "password": "pw"})
		rec := httptest.NewRecorder()
		authController.LDAPLogin(rec, httptest.NewRequest(http.MethodPost, "/", bytes.NewReader(raw)))
		if rec.Code != http.StatusOK {
			t.Fatalf("sign in: %d %s", rec.Code, rec.Body.String())
		}
	}
	// frank's live admin roles: rows not marked taken, read through to_jsonb so
	// the same query holds on a schema from before deleted_at, where every row
	// is live.
	liveAdmins := `SELECT count(*) FROM admin_users WHERE email_id = 'frank@example.test'
		AND (to_jsonb(admin_users) ->> 'deleted_at') IS NULL`

	groups = []string{adminGroup}
	signIn()
	if n := countRows(t, env.PG, liveAdmins); n != 1 {
		t.Fatalf("in the directory's admin group: frank holds %d live admin roles, want 1", n)
	}
	if setupOffered(t) {
		t.Fatal("with frank an admin, the first-admin setup is offered")
	}

	groups = nil
	signIn()
	if n := countRows(t, env.PG, liveAdmins); n != 0 {
		t.Fatalf("taken out of the directory's admin group: frank still holds %d live admin roles", n)
	}
	assertSetupShut(t, env.PG, "after the directory took the last admin role")

	if _, err := env.PG.Exec(`DELETE FROM admin_users`); err != nil {
		t.Fatal(err)
	}
	assertSetupShut(t, env.PG, "after the taken role's row went for good")
}

// An install from before the mark that already lost its last admin row: people
// are there, admin_users is empty. After the upgrade, setup is shut.
func TestFirstAdminSetupStaysShutForPeopleWithNoAdminRowAfterTheUpgrade(t *testing.T) {
	env := startedInstall(t)
	if _, err := env.PG.Exec(`INSERT INTO users (id, email_id, username, display_name, created_at, updated_at)
		VALUES ($1, 'rita@example.test', 'rita', 'Rita', NOW(), NOW())`, uuid.New()); err != nil {
		t.Fatal(err)
	}
	beforeTheMark(t, env.PG)
	upgrade(t, env.PG)
	assertSetupShut(t, env.PG, "people and no admin row, after the upgrade")
}

// An install that has started but whose owner has not finished setup has
// users rows already: its bots. After the upgrade, setup is still offered,
// or the owner could never claim it.
func TestFirstAdminSetupStaysOpenForAnInstallWithOnlyItsBots(t *testing.T) {
	env := startedInstall(t)
	beforeTheMark(t, env.PG)
	if n := upgrade(t, env.PG); n != 1 {
		t.Fatalf("the grants migration: %d up files, want 1", n)
	}
	if !setupOffered(t) {
		t.Fatal("an install with only its bots, after the upgrade: setup is shut, and its owner can never claim it")
	}
}

// Claims racing each other on an install with only its bots: exactly one
// makes an admin, and setup is shut after it.
func TestFirstAdminSetupRacedMakesOneAdmin(t *testing.T) {
	env := startedInstall(t)
	const racers = 6
	codes := make([]int, racers)
	start := make(chan struct{})
	var wg sync.WaitGroup
	for i := 0; i < racers; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			<-start
			codes[i] = claimSetup(fmt.Sprintf("owner%d@example.test", i)).Code
		}(i)
	}
	close(start)
	wg.Wait()
	won := 0
	for _, code := range codes {
		if code == http.StatusOK {
			won++
		}
	}
	if won != 1 {
		t.Errorf("%d of %d racing setups made an admin (codes %v), want exactly 1", won, racers, codes)
	}
	if n := countRows(t, env.PG, `SELECT count(*) FROM admin_users`); n != 1 {
		t.Errorf("%d admin rows after the race, want 1", n)
	}
	if n := countRows(t, env.PG, `SELECT count(*) FROM users WHERE NOT is_bot`); n != 1 {
		t.Errorf("%d accounts after the race, want 1", n)
	}
	assertSetupShut(t, env.PG, "after a setup won the race")
}

// firstRead closes started when the request's body is first read. By then
// AdminSetup has passed its first check, which runs before it reads the body.
type firstRead struct {
	io.Reader
	once    sync.Once
	started chan struct{}
}

func (f *firstRead) Read(p []byte) (int, error) {
	f.once.Do(func() { close(f.started) })
	return f.Reader.Read(p)
}

// A claim that passed the first check while another setup was still to run
// is refused once it holds the lock: AdminSetup checks again under it. The
// first check runs before the body is read, so a claim whose body comes
// slowly reaches a free lock after the other setup made its admin and let the
// lock go; only the second check stops a second admin (CODE-REVIEWER-154
// row 2248, mutant MR).
func TestFirstAdminSetupChecksAgainOnceItHoldsTheLock(t *testing.T) {
	env := startedInstall(t)
	pr, pw := io.Pipe()
	body := &firstRead{Reader: pr, started: make(chan struct{})}
	slow := make(chan *httptest.ResponseRecorder, 1)
	go func() {
		rec := httptest.NewRecorder()
		authController.AdminSetup(rec, httptest.NewRequest(http.MethodPost, "/auth/admin-setup", body))
		slow <- rec
	}()
	select {
	case <-body.started:
	case <-time.After(30 * time.Second):
		t.Fatal("the slow claim never began to read its body")
	}
	if rec := claimSetup("owner@example.test"); rec.Code != http.StatusOK {
		t.Fatalf("the setup that finishes first: %d %s", rec.Code, rec.Body.String())
	}
	raw, _ := json.Marshal(map[string]string{"email": "late@example.test", "password": "Correct-Horse-Battery-9", "username": "late"})
	go func() {
		_, _ = pw.Write(raw)
		_ = pw.Close()
	}()
	var rec *httptest.ResponseRecorder
	select {
	case rec = <-slow:
	case <-time.After(60 * time.Second):
		t.Fatal("the slow claim never answered")
	}
	if rec.Code != http.StatusBadRequest {
		t.Errorf("the slow claim, once it holds the lock: %d %s, want 400 (setup is done)", rec.Code, rec.Body.String())
	}
	if n := countRows(t, env.PG, `SELECT count(*) FROM admin_users`); n != 1 {
		t.Errorf("%d admin rows after the slow claim, want 1", n)
	}
	if n := countRows(t, env.PG, `SELECT count(*) FROM users WHERE NOT is_bot`); n != 1 {
		t.Errorf("%d accounts after the slow claim, want 1", n)
	}
}
