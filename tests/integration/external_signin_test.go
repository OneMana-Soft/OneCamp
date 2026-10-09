//go:build integration
// +build integration

package integration_test

// External people don't sign in; joining adopts them.
//
// An import or GitHub sync makes a users row (is_external) for someone it
// brings history in from, so that history has an author. That row isn't an
// account. It counted as one: its owner signed in through Google or GitHub
// without an invitation, accepting an invitation set a password on it, and a
// session naming it was honoured, all as an external who never took a seat.
// This runs those paths against real Postgres, Redis and Dgraph: an external
// row (and a bot's) can't sign in by password, by Google/GitHub or with a
// session minted for it; an invited person with an external row adopts it,
// keeping its id and taking a seat; a full workspace refuses.
//
// Run: go test -tags=integration ./tests/integration/ -run TestExternalPeople -v

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/google/uuid"
	"golang.org/x/crypto/bcrypt"

	userBusiness "github.com/akashc777/OneCamp/business/User"
	authController "github.com/akashc777/OneCamp/controllers/Auth"
	userDomain "github.com/akashc777/OneCamp/domain/User"
	"github.com/akashc777/OneCamp/helpers"
	"github.com/akashc777/OneCamp/initializers/postgresInit"
	customMiddleware "github.com/akashc777/OneCamp/middleware"
	"github.com/akashc777/OneCamp/tests/integration"
)

func TestExternalPeopleSignInOnlyByJoining(t *testing.T) {
	t.Setenv("JWT_SECRET", "external-signin-integration-secret")
	t.Setenv("ALLOWED_USERS", "")
	env := integration.SetupEnv(t)
	ctx := context.Background()
	if err := postgresInit.ConnectPostgres(ctx, env.DSN); err != nil {
		t.Fatalf("wire the project pool at the test database: %v", err)
	}
	integration.SetupRedis(t)
	integration.StubMqttClient(t)
	dg := integration.SetupDgraph(t)
	oldLimit := helpers.SeatLimit
	t.Cleanup(func() { helpers.SeatLimit = oldLimit })
	helpers.SeatLimit = ""

	const password = "correct horse battery"
	hash, err := bcrypt.GenerateFromPassword([]byte(password), bcrypt.MinCost)
	if err != nil {
		t.Fatal(err)
	}

	// Rows and graph nodes as the product writes them (the business creators
	// also index search, which these tests don't run). Everyone holds a
	// password: external rows, the way accepting an invitation used to leave
	// one on them, and the bot, which is external by class.
	seed := func(email, name string, external, bot bool) uuid.UUID {
		t.Helper()
		id := uuid.New()
		if _, err := env.PG.Exec(`INSERT INTO users (id, email_id, password_hash, is_external, is_bot, created_at, updated_at)
			VALUES ($1, $2, $3, $4, $5, NOW(), NOW())`, id, email, string(hash), external, bot); err != nil {
			t.Fatalf("seed %s: %v", email, err)
		}
		node := map[string]any{"uid": "_:u", "dgraph.type": "User", "user_uuid": id.String(),
			"user_email_id": email, "user_name": name, "user_full_name": name}
		if external {
			node["is_external"] = true
		}
		if bot {
			node["is_bot"] = true
		}
		dg.Mutate(t, node)
		return id
	}
	bob := seed("bob@example.test", "bob", false, false) // a member, who invites people
	alice := seed("alice@example.test", "jira-alice", true, false)
	carol := seed("carol@example.test", "jira-carol", true, false)
	dave := seed("dave@example.test", "jira-dave", true, false)
	botID := seed(userDomain.SystemBotEmail, "OneCamp AI", true, true)

	post := func(h http.HandlerFunc, body any) *httptest.ResponseRecorder {
		t.Helper()
		raw, _ := json.Marshal(body)
		rec := httptest.NewRecorder()
		h(rec, httptest.NewRequest(http.MethodPost, "/", bytes.NewReader(raw)))
		return rec
	}
	signIn := func(email string) int {
		t.Helper()
		return post(authController.EmailLogin, map[string]string{"email": email, "password": password}).Code
	}
	session := func(id uuid.UUID, typ string, cookie string, mw func(http.Handler) http.Handler) int {
		t.Helper()
		token, err := helpers.SignSessionToken(id.String(), typ, time.Now().Add(5*time.Minute).Unix())
		if err != nil {
			t.Fatal(err)
		}
		req := httptest.NewRequest(http.MethodGet, "/", nil)
		req.AddCookie(&http.Cookie{Name: cookie, Value: token})
		req.AddCookie(&http.Cookie{Name: "DeviceId", Value: "device-1"})
		rec := httptest.NewRecorder()
		mw(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) { w.WriteHeader(http.StatusOK) })).ServeHTTP(rec, req)
		return rec.Code
	}
	oauthTokens := func(email string) bool {
		t.Helper()
		_, access, _, _, _ := userBusiness.LoginUserByEmailID(ctx, email, "", time.Now().Add(time.Minute).Unix(), time.Now().Add(time.Hour).Unix())
		return access != ""
	}
	isExternal := func(id uuid.UUID) (pg bool, graph bool) {
		t.Helper()
		if err := env.PG.QueryRow(`SELECT is_external FROM users WHERE id = $1`, id).Scan(&pg); err != nil {
			t.Fatal(err)
		}
		node, err := userDomain.GetDgraphUserInfoByUUID(ctx, id.String())
		if err != nil {
			t.Fatal(err)
		}
		return pg, node.IsExternal
	}
	invite := func(email, token string) {
		t.Helper()
		if err := userDomain.AddInvitationWithToken(ctx, email, bob, token, time.Now().Add(time.Hour)); err != nil {
			t.Fatal(err)
		}
	}

	// 1. Nobody external signs in, whatever they hold.
	for name, who := range map[string]struct {
		id    uuid.UUID
		email string
	}{
		"an external person": {alice, "alice@example.test"},
		"a bot":              {botID, userDomain.SystemBotEmail},
	} {
		if code := signIn(who.email); code != http.StatusUnauthorized {
			t.Errorf("%s signed in with a password: %d", name, code)
		}
		for mwName, c := range map[string]struct {
			typ, cookie string
			mw          func(http.Handler) http.Handler
		}{
			"VerifyAuth":             {helpers.TokenTypeAccess, "Authorization", customMiddleware.VerifyAuth},
			"VerifyAuthOnlyPostgres": {helpers.TokenTypeAccess, "Authorization", customMiddleware.VerifyAuthOnlyPostgres},
			"VerifyRefreshToken":     {helpers.TokenTypeRefresh, "RefreshToken", customMiddleware.VerifyRefreshToken},
		} {
			if code := session(who.id, c.typ, c.cookie, c.mw); code != http.StatusUnauthorized {
				t.Errorf("%s: a session minted for %s was honoured: %d", mwName, name, code)
			}
		}
	}
	if code := session(bob, helpers.TokenTypeAccess, "Authorization", customMiddleware.VerifyAuth); code != http.StatusOK {
		t.Fatalf("the fixture is wrong: a member's session is refused (%d), so the refusals above prove nothing", code)
	}
	if _, err := userBusiness.AdmitOAuthUser(ctx, "alice@example.test", "Alice", "google", ""); !errors.Is(err, userBusiness.ErrNotInvited) {
		t.Errorf("Google and an uninvited external person: %v, want ErrNotInvited, which the sign-in page names", err)
	}
	if oauthTokens("alice@example.test") || oauthTokens(userDomain.SystemBotEmail) {
		t.Error("an OAuth sign-in found an external person or a bot to sign in as")
	}
	if !oauthTokens("bob@example.test") {
		t.Fatal("the fixture is wrong: a member gets no OAuth session")
	}

	// 2. An invitation adopts the external row: same id, a member, a seat.
	helpers.SeatLimit = "2"
	invite("alice@example.test", "tok-alice")
	rec := post(authController.EmailSignup, map[string]string{"token": "tok-alice", "username": "alice", "password": "a new password"})
	if rec.Code != http.StatusOK {
		t.Fatalf("accepting the invitation: %d %s", rec.Code, rec.Body.String())
	}
	if pg, graph := isExternal(alice); pg || graph {
		t.Fatalf("after joining, alice is still external (postgres %v, graph %v)", pg, graph)
	}
	var adopted uuid.UUID
	if err := env.PG.QueryRow(`SELECT id FROM users WHERE email_id = 'alice@example.test'`).Scan(&adopted); err != nil || adopted != alice {
		t.Fatalf("joining made another account (%v, %v) instead of adopting %v", adopted, err, alice)
	}
	if used, _, err := userDomain.SeatUsage(ctx); err != nil || used != 2 {
		t.Fatalf("seats used = %d (%v), want 2: adopting must take a seat", used, err)
	}
	if code := post(authController.EmailLogin, map[string]string{"email": "alice@example.test", "password": "a new password"}).Code; code != http.StatusOK {
		t.Errorf("alice can't sign in with the password she joined with: %d", code)
	}
	if code := session(alice, helpers.TokenTypeAccess, "Authorization", customMiddleware.VerifyAuth); code != http.StatusOK {
		t.Errorf("alice's session is refused after joining: %d", code)
	}

	// 3. A full workspace refuses, and the row stays external.
	invite("carol@example.test", "tok-carol")
	rec = post(authController.EmailSignup, map[string]string{"token": "tok-carol", "username": "carol", "password": "a new password"})
	if rec.Code != http.StatusForbidden || !bytes.Contains(rec.Body.Bytes(), []byte("seat_limit")) {
		t.Fatalf("joining a full workspace: %d %s, want 403 seat_limit", rec.Code, rec.Body.String())
	}
	if _, err := userBusiness.AdmitOAuthUser(ctx, "carol@example.test", "Carol", "google", ""); !helpers.IsSeatLimit(err) {
		t.Fatalf("an invited external joining a full workspace through Google: %v, want the seat limit", err)
	}
	if pg, graph := isExternal(carol); !pg || !graph {
		t.Fatalf("a refused join changed carol (postgres external %v, graph external %v)", pg, graph)
	}

	// 4. With room, an invited external joins through Google, keeping the row.
	helpers.SeatLimit = "3"
	invite("dave@example.test", "tok-dave")
	if _, err := userBusiness.AdmitOAuthUser(ctx, "dave@example.test", "Dave", "google", ""); err != nil {
		t.Fatalf("an invited external joining through Google: %v", err)
	}
	if pg, graph := isExternal(dave); pg || graph {
		t.Fatalf("after joining through Google, dave is still external (postgres %v, graph %v)", pg, graph)
	}
	var method string
	if err := env.PG.QueryRow(`SELECT signup_method FROM users WHERE id = $1`, dave).Scan(&method); err != nil || method != "google" {
		t.Errorf("dave's sign-up method = %q (%v), want google", method, err)
	}
	if !oauthTokens("dave@example.test") {
		t.Error("dave gets no session after joining through Google")
	}
}
