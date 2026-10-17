//go:build integration
// +build integration

package integration_test

// Do the demo's own people have @handles?
//
// The nightly reset restores the snapshot, starts the API, whose startup
// backfill gives every member without a handle one (BackfillHandles), and
// only then runs `demoseed --refresh`. The seed makes the shared visitor and
// the fictional cast (Maya Chen, Jonas Weber) through EnsureDemoUserExists,
// which made them with a creator that gave no handle. The visitor got one on
// signing in, from reading their own profile; the cast never sign in, so the
// @mention list showed them without a handle until the API next started: on
// 12 Oct 2026 from the 09:51 IST seed until a deploy at 17:49.
//
// This makes the visitor and the cast the way the seed does, after the
// backfill has run, and reads /user/allUsers as the visitor on a DEMO_MODE
// server through the real middleware against Postgres 12 and Dgraph: each has
// the handle sign up gives, and making them again, as every refresh and demo
// login does, changes nothing.
//
// Run: go test -tags=integration ./tests/integration/ -run TestDemoPeopleHaveHandles -v

import (
	"bytes"
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/go-chi/chi/v5"
	"github.com/google/uuid"
	"github.com/opensearch-project/opensearch-go/v4"
	"github.com/opensearch-project/opensearch-go/v4/opensearchapi"

	userBusiness "github.com/akashc777/OneCamp/business/User"
	userController "github.com/akashc777/OneCamp/controllers/User"
	"github.com/akashc777/OneCamp/helpers"
	"github.com/akashc777/OneCamp/initializers/opensearchInit"
	"github.com/akashc777/OneCamp/initializers/postgresInit"
	customMiddleware "github.com/akashc777/OneCamp/middleware"
	"github.com/akashc777/OneCamp/tests/integration"
)

func TestDemoPeopleHaveHandles(t *testing.T) {
	t.Setenv("JWT_SECRET", "demo-people-handles-secret")
	t.Setenv("ALLOWED_USERS", "")
	env := integration.SetupEnv(t)
	ctx := context.Background()
	if err := postgresInit.ConnectPostgres(ctx, env.DSN); err != nil {
		t.Fatalf("wire the project pool at the test database: %v", err)
	}
	integration.SetupRedis(t)
	integration.StubMqttClient(t)
	integration.SetupDgraph(t)

	// Making someone indexes them for search in the background.
	search := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		fmt.Fprint(w, `{"_index":"users","_id":"x","result":"created","_shards":{"total":1,"successful":1,"failed":0}}`)
	}))
	defer search.Close()
	client, err := opensearchapi.NewClient(opensearchapi.Config{Client: opensearch.Config{Addresses: []string{search.URL}}})
	if err != nil {
		t.Fatal(err)
	}
	before := opensearchInit.OpenSearchClient
	opensearchInit.OpenSearchClient = client
	t.Cleanup(func() { opensearchInit.OpenSearchClient = before })

	const visitorEmail = "visitor@demo.example"
	t.Setenv("DEMO_MODE", "true")
	t.Setenv("DEMO_USER_EMAIL", visitorEmail)

	// The API has started and its backfill has run before the seed: there is
	// nobody for it to give a handle to yet.
	if given, failed, err := userBusiness.BackfillHandles(ctx); err != nil || given != 0 || failed != 0 {
		t.Fatalf("the backfill before the seed gave %d, failed %d (%v); want nothing to do", given, failed, err)
	}

	// The seed's cast (Cast in business/DemoSeed), and the handle sign up
	// gives each name.
	cast := []struct{ name, email string }{
		{"Maya Chen", "maya.chen@cast.demo.onemana.dev"},
		{"Jonas Weber", "jonas.weber@cast.demo.onemana.dev"},
	}
	want := map[string]string{"Sam Rivera": "sam-rivera", "Maya Chen": "maya-chen", "Jonas Weber": "jonas-weber"}
	// The seed: the visitor first (services/DemoSeed), then the cast
	// (ensureCast); twice, as each refresh and each demo login runs it again.
	seed := func() {
		t.Helper()
		if err := userBusiness.EnsureDemoUserExists(ctx, visitorEmail, "Sam Rivera"); err != nil {
			t.Fatalf("making the visitor: %v", err)
		}
		for _, p := range cast {
			if err := userBusiness.EnsureDemoUserExists(ctx, p.email, p.name); err != nil {
				t.Fatalf("making %s: %v", p.name, err)
			}
		}
	}
	seed()
	seed()

	var visitor uuid.UUID
	if err := env.PG.QueryRow(`SELECT id FROM users WHERE email_id = $1`, visitorEmail).Scan(&visitor); err != nil {
		t.Fatalf("the visitor's account: %v", err)
	}
	for _, p := range cast {
		var handle sql.NullString
		if err := env.PG.QueryRow(`SELECT username FROM users WHERE email_id = $1`, p.email).Scan(&handle); err != nil {
			t.Fatal(err)
		}
		if handle.String != want[p.name] {
			t.Errorf("%s is stored with handle %q, want %q", p.name, handle.String, want[p.name])
		}
	}

	// What the @mention list reads, as the visitor reads it.
	r := chi.NewRouter()
	r.Use(customMiddleware.VerifyAuth)
	r.Get("/user/allUsers", userController.AllUsersList)
	token, err := helpers.SignSessionToken(visitor.String(), helpers.TokenTypeAccess, time.Now().Add(5*time.Minute).Unix())
	if err != nil {
		t.Fatal(err)
	}
	req := httptest.NewRequest(http.MethodGet, "/user/allUsers", bytes.NewReader(nil))
	req.AddCookie(&http.Cookie{Name: "Authorization", Value: token})
	req.AddCookie(&http.Cookie{Name: "DeviceId", Value: "device-1"})
	rec := httptest.NewRecorder()
	r.ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("allUsers: %d %s", rec.Code, rec.Body.String())
	}
	var got struct {
		Users []struct {
			Name   string `json:"user_name"`
			Handle string `json:"user_handle"`
		} `json:"users"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &got); err != nil {
		t.Fatalf("allUsers is not the list it was: %v: %s", err, rec.Body.String())
	}
	listed := map[string]string{}
	for _, u := range got.Users {
		listed[u.Name] = u.Handle
	}
	for name, handle := range want {
		if h, ok := listed[name]; !ok {
			t.Errorf("allUsers leaves %s out: %s", name, rec.Body.String())
		} else if h != handle {
			t.Errorf("allUsers gives %s the handle %q, want %q", name, h, handle)
		}
	}
}
