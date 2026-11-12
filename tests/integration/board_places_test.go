//go:build integration
// +build integration

package integration_test

// A board is a place (miro-46): a person stars a board they may read
// (POST /places, resource_kind "board"), it is in their starred list by its
// title and its page, and it drops out of that list once they may no longer
// read it. GET /places/recent?resource_kind=board is the boards they opened
// lately, newest first, only those they may still read.
//
// Run: go test -tags=integration ./tests/integration/ -run TestABoardIsAPlace -v

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"reflect"
	"testing"
	"time"

	"github.com/dgraph-io/dgo/v230/protos/api"
	"github.com/go-chi/chi/v5"
	"github.com/google/uuid"

	_ "github.com/akashc777/OneCamp/business/Board" // registers the "board" resolver
	docController "github.com/akashc777/OneCamp/controllers/Doc"
	"github.com/akashc777/OneCamp/helpers"
	"github.com/akashc777/OneCamp/initializers/dgraphInit"
	"github.com/akashc777/OneCamp/initializers/postgresInit"
	customMiddleware "github.com/akashc777/OneCamp/middleware"
	"github.com/akashc777/OneCamp/tests/integration"
)

func TestABoardIsAPlace(t *testing.T) {
	t.Setenv("JWT_SECRET", "board-places-secret")
	t.Setenv("ALLOWED_USERS", "")
	env := integration.SetupEnv(t)
	ctx := context.Background()
	if err := postgresInit.ConnectPostgres(ctx, env.DSN); err != nil {
		t.Fatalf("wire the project pool at the test database: %v", err)
	}
	integration.SetupRedis(t)
	integration.StubMqttClient(t)
	dg := integration.SetupDgraph(t)

	me, them := uuid.New(), uuid.New()
	for name, id := range map[string]uuid.UUID{"me": me, "them": them} {
		if _, err := env.PG.Exec(`INSERT INTO users (id, email_id, username, display_name, created_at, updated_at)
			VALUES ($1, $2, $3, $4, NOW(), NOW())`, id, name+"@example.com", name, name); err != nil {
			t.Fatalf("seed %s: %v", name, err)
		}
	}
	const live = "0001-01-01T00:00:00Z"
	ids := map[string]string{}
	board := func(node, title string, private bool) map[string]any {
		ids[node] = uuid.NewString()
		return map[string]any{"uid": "_:" + node, "dgraph.type": "Board", "board_uuid": ids[node], "board_title": title,
			"board_private": private, "board_deleted_at": live, "board_created_by": map[string]any{"uid": "_:them"}}
	}
	open, shared, private, binned := board("open", "Roadmap", false), board("shared", "Retro", true),
		board("private", "Layoffs", true), board("binned", "Old plan", false)
	shared["board_reading_users"] = []map[string]any{{"uid": "_:me"}}
	binned["board_deleted_at"] = time.Now().Add(-time.Hour).UTC().Format(time.RFC3339)
	uids := dg.Mutate(t, []map[string]any{
		{"uid": "_:me", "dgraph.type": "User", "user_uuid": me.String(), "user_name": "me"},
		{"uid": "_:them", "dgraph.type": "User", "user_uuid": them.String(), "user_name": "them"},
		open, shared, private, binned,
	})

	r := chi.NewRouter()
	r.Use(customMiddleware.VerifyAuth)
	r.Get("/places", docController.GetPlaces)
	r.Post("/places", docController.AddPlace)
	r.Delete("/places", docController.RemovePlace)
	r.Get("/places/recent", docController.GetRecentPlaces)
	call := func(t *testing.T, as uuid.UUID, method, target string, body any) *httptest.ResponseRecorder {
		t.Helper()
		token, err := helpers.SignSessionToken(as.String(), helpers.TokenTypeAccess, time.Now().Add(5*time.Minute).Unix())
		if err != nil {
			t.Fatal(err)
		}
		var buf bytes.Buffer
		if body != nil {
			_ = json.NewEncoder(&buf).Encode(body)
		}
		req := httptest.NewRequest(method, target, &buf)
		req.AddCookie(&http.Cookie{Name: "Authorization", Value: token})
		req.AddCookie(&http.Cookie{Name: "DeviceId", Value: "device-1"})
		rec := httptest.NewRecorder()
		r.ServeHTTP(rec, req)
		return rec
	}
	type place struct{ Title, Href, Kind, ResourceKind string }
	places := func(t *testing.T, rec *httptest.ResponseRecorder) []place {
		t.Helper()
		if rec.Code != http.StatusOK {
			t.Fatalf("%d %s", rec.Code, rec.Body.String())
		}
		var got struct {
			Data struct {
				Places []struct {
					Title        string `json:"title"`
					Href         string `json:"href"`
					Kind         string `json:"kind"`
					ResourceKind string `json:"resource_kind"`
				} `json:"places"`
			} `json:"data"`
		}
		if err := json.Unmarshal(rec.Body.Bytes(), &got); err != nil {
			t.Fatal(err)
		}
		out := []place{}
		for _, p := range got.Data.Places {
			out = append(out, place{p.Title, p.Href, p.Kind, p.ResourceKind})
		}
		return out
	}
	star := func(t *testing.T, id string) int {
		return call(t, me, http.MethodPost, "/places", map[string]any{"kind": "star", "resource_kind": "board", "resource_uuid": id}).Code
	}

	t.Run("they star the boards they may read, and only those", func(t *testing.T) {
		if code := star(t, ids["open"]); code != http.StatusOK {
			t.Fatalf("star a board open to everyone: %d", code)
		}
		if code := star(t, ids["shared"]); code != http.StatusOK {
			t.Fatalf("star a private board shared with them: %d", code)
		}
		for _, node := range []string{"private", "binned"} {
			if code := star(t, ids[node]); code != http.StatusNotFound {
				t.Errorf("star the %s board: %d, want 404", node, code)
			}
		}
		got := places(t, call(t, me, http.MethodGet, "/places?kind=star&resource_kind=board", nil))
		want := []place{
			{"Roadmap", "/app/board/" + ids["open"], "star", "board"},
			{"Retro", "/app/board/" + ids["shared"], "star", "board"},
		}
		if !reflect.DeepEqual(got, want) {
			t.Errorf("starred %+v, want %+v", got, want)
		}
	})

	t.Run("a starred board they can no longer read drops out of the list", func(t *testing.T) {
		raw, _ := json.Marshal(map[string]any{"uid": uids["shared"], "board_reading_users": []map[string]any{{"uid": uids["me"]}}})
		if _, err := dgraphInit.DgraphClient.NewTxn().Mutate(ctx, &api.Mutation{DeleteJson: raw, CommitNow: true}); err != nil {
			t.Fatalf("unshare the board: %v", err)
		}
		got := places(t, call(t, me, http.MethodGet, "/places?kind=star&resource_kind=board", nil))
		if want := []place{{"Roadmap", "/app/board/" + ids["open"], "star", "board"}}; !reflect.DeepEqual(got, want) {
			t.Errorf("starred %+v, want %+v", got, want)
		}
	})

	t.Run("recent boards: newest first, only those they may still read", func(t *testing.T) {
		now := time.Now().UTC()
		for node, ago := range map[string]time.Duration{"open": time.Hour, "shared": time.Minute, "private": 2 * time.Minute, "binned": 3 * time.Minute} {
			if _, err := env.PG.Exec(`INSERT INTO resource_views (resource_type, resource_uuid, user_uuid, first_viewed_at, last_viewed_at)
				VALUES ('board', $1, $2, $3, $3)`, ids[node], me.String(), now.Add(-ago)); err != nil {
				t.Fatal(err)
			}
		}
		// Shared with them again: read, so it is in their recent boards.
		dg.Mutate(t, map[string]any{"uid": uids["shared"], "board_reading_users": []map[string]any{{"uid": uids["me"]}}})
		got := places(t, call(t, me, http.MethodGet, "/places/recent?resource_kind=board", nil))
		want := []place{
			{"Retro", "/app/board/" + ids["shared"], "recent", "board"},
			{"Roadmap", "/app/board/" + ids["open"], "recent", "board"},
		}
		if !reflect.DeepEqual(got, want) {
			t.Errorf("recent %+v, want %+v", got, want)
		}
		if got := places(t, call(t, me, http.MethodGet, "/places/recent?resource_kind=board&limit=1", nil)); len(got) != 1 || got[0].Title != "Retro" {
			t.Errorf("limit=1: %+v, want Retro alone", got)
		}
		if got := places(t, call(t, them, http.MethodGet, "/places/recent?resource_kind=board", nil)); len(got) != 0 {
			t.Errorf("someone who opened none: %+v, want none", got)
		}
		if code := call(t, me, http.MethodGet, "/places/recent?resource_kind=spaceship", nil).Code; code != http.StatusBadRequest {
			t.Errorf("a kind that keeps no places: %d, want 400", code)
		}
	})

	// V-616: the two views after Retro's are boards they can no longer open,
	// and an older one is a board they can: two asked are two answered, read
	// on past what was left out, not one.
	t.Run("recent boards fill the limit from older views past the ones they can't open", func(t *testing.T) {
		got := places(t, call(t, me, http.MethodGet, "/places/recent?resource_kind=board&limit=2", nil))
		want := []place{
			{"Retro", "/app/board/" + ids["shared"], "recent", "board"},
			{"Roadmap", "/app/board/" + ids["open"], "recent", "board"},
		}
		if !reflect.DeepEqual(got, want) {
			t.Errorf("limit=2: %+v, want %+v", got, want)
		}
	})
}
