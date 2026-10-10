//go:build integration
// +build integration

package integration_test

// Does the demo's shared visitor see anyone else's email address?
//
// Everyone who opens the public demo signs in as one visitor, so whatever the
// visitor reads, any stranger reads. The people who run the demo have real
// accounts on it with the addresses they signed up with, and every list of
// people carried user_email_id: the @mention popup showed them to anyone.
// helpers.ServeHidingEmails blanks every other person's address in the
// visitor's answers, on the way out of the auth middleware.
//
// This signs in through the real middleware (a session cookie through
// VerifyAuth) against Postgres 12 and Dgraph and reads the people lists the web
// app reads: as the visitor on a DEMO_MODE server no other address appears
// (their own still does); as one of the demo's own members, and as a member of
// an ordinary server, every address does, as before.
//
// Run: go test -tags=integration ./tests/integration/ -run TestDemoVisitor -v

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/go-chi/chi/v5"
	"github.com/google/uuid"

	boardController "github.com/akashc777/OneCamp/controllers/Board"
	docController "github.com/akashc777/OneCamp/controllers/Doc"
	userController "github.com/akashc777/OneCamp/controllers/User"
	"github.com/akashc777/OneCamp/helpers"
	"github.com/akashc777/OneCamp/initializers/postgresInit"
	customMiddleware "github.com/akashc777/OneCamp/middleware"
	"github.com/akashc777/OneCamp/tests/integration"
)

func TestDemoVisitorSeesNoOtherEmails(t *testing.T) {
	t.Setenv("JWT_SECRET", "demo-visitor-emails-secret")
	t.Setenv("ALLOWED_USERS", "")
	env := integration.SetupEnv(t)
	ctx := context.Background()
	if err := postgresInit.ConnectPostgres(ctx, env.DSN); err != nil {
		t.Fatalf("wire the project pool at the test database: %v", err)
	}
	integration.SetupRedis(t)
	integration.StubMqttClient(t)
	dg := integration.SetupDgraph(t)

	const visitorEmail = "visitor@demo.example"
	seed := func(email, name, full string) uuid.UUID {
		t.Helper()
		id := uuid.New()
		if _, err := env.PG.Exec(`INSERT INTO users (id, email_id, username, display_name, created_at, updated_at)
			VALUES ($1, $2, $3, $4, NOW(), NOW())`, id, email, name, name); err != nil {
			t.Fatalf("seed %s: %v", email, err)
		}
		dg.Mutate(t, map[string]any{"uid": "_:u", "dgraph.type": "User", "user_uuid": id.String(),
			"user_email_id": email, "user_name": name, "user_full_name": full})
		return id
	}
	visitor := seed(visitorEmail, "sam", "Sam Rivera")
	seed("owner.realname@example.com", "ownerperson", "Owner Person")
	member := seed("member.personal@example.org", "memberperson", "Member Person")
	others := []string{"owner.realname@example.com", "member.personal@example.org"}

	r := chi.NewRouter()
	r.Use(customMiddleware.VerifyAuth)
	r.Get("/user/allUsers", userController.AllUsersList)
	r.Post("/user/searchUserAndChannelList", userController.FwdUserAndChannelList)
	r.Post("/doc/searchUsers", docController.SearchUsersForDoc)
	r.Post("/board/searchUsers", boardController.SearchUsersForBoard)

	call := func(as uuid.UUID, method, path string, body any) string {
		t.Helper()
		token, err := helpers.SignSessionToken(as.String(), helpers.TokenTypeAccess, time.Now().Add(5*time.Minute).Unix())
		if err != nil {
			t.Fatal(err)
		}
		var rd *bytes.Reader
		if body != nil {
			raw, _ := json.Marshal(body)
			rd = bytes.NewReader(raw)
		} else {
			rd = bytes.NewReader(nil)
		}
		req := httptest.NewRequest(method, path, rd)
		req.AddCookie(&http.Cookie{Name: "Authorization", Value: token})
		req.AddCookie(&http.Cookie{Name: "DeviceId", Value: "device-1"})
		rec := httptest.NewRecorder()
		r.ServeHTTP(rec, req)
		if rec.Code != http.StatusOK {
			t.Fatalf("%s %s: %d %s", method, path, rec.Code, rec.Body.String())
		}
		if !json.Valid(rec.Body.Bytes()) {
			t.Fatalf("%s %s: the answer is no longer JSON: %s", method, path, rec.Body.String())
		}
		return rec.Body.String()
	}
	// The lists the web app reads people from, with something in each that
	// should list the other two.
	reads := []struct {
		name, method, path string
		body               any
	}{
		{"allUsers (the @mention popup)", http.MethodGet, "/user/allUsers", nil},
		{"people search (forwarding, pickers)", http.MethodPost, "/user/searchUserAndChannelList", map[string]string{"search_text": "person"}},
		{"doc people search", http.MethodPost, "/doc/searchUsers", map[string]string{"searchText": "person"}},
		{"board people search", http.MethodPost, "/board/searchUsers", map[string]string{"searchText": "person"}},
	}

	// 1. An ordinary server: a member sees their teammates' addresses.
	t.Setenv("DEMO_MODE", "")
	t.Setenv("DEMO_USER_EMAIL", visitorEmail)
	all := call(member, http.MethodGet, "/user/allUsers", nil)
	if !strings.Contains(all, "owner.realname@example.com") {
		t.Errorf("a member of an ordinary server no longer sees a teammate's address in allUsers: %s", all)
	}
	// The same account as the demo's visitor, on a server that isn't the demo.
	if v := call(visitor, http.MethodGet, "/user/allUsers", nil); !strings.Contains(v, "owner.realname@example.com") {
		t.Errorf("off the demo, the visitor's address alone hid others': %s", v)
	}

	// 2. The demo.
	t.Setenv("DEMO_MODE", "true")
	listed := false
	for _, rd := range reads {
		got := call(visitor, rd.method, rd.path, rd.body)
		for _, e := range others {
			if strings.Contains(got, e) {
				t.Errorf("%s: the demo visitor reads %s: %s", rd.name, e, got)
			}
		}
		if strings.Contains(got, "Owner Person") || strings.Contains(got, "ownerperson") {
			listed = true
		}
	}
	if !listed {
		t.Error("no read listed the other people at all, so the check above proves nothing")
	}
	if v := call(visitor, http.MethodGet, "/user/allUsers", nil); !strings.Contains(v, visitorEmail) {
		t.Errorf("the visitor's own address went too: %s", v)
	}
	// The demo's own members, signed in as themselves, still see addresses.
	if m := call(member, http.MethodGet, "/user/allUsers", nil); !strings.Contains(m, "owner.realname@example.com") {
		t.Errorf("one of the demo's own members lost a teammate's address: %s", m)
	}
}
