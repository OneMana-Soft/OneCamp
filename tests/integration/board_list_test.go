//go:build integration
// +build integration

package integration_test

// GET /board/getBoardList?editable=1 lists only the boards the caller may
// edit (CanEdit: their own, and those shared with them to edit). A call's
// "Present a board" picker asks for it, because the server presents no other
// board (board_not_editable): a board shared with someone to read or to
// comment on is never offered to them. Without the flag the list is every
// board shared with them, as before.
//
// Run: go test -tags=integration ./tests/integration/ -run TestTheBoardListNarrowsToTheBoardsYouMayEdit -v

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"reflect"
	"testing"
	"time"

	"github.com/go-chi/chi/v5"
	"github.com/google/uuid"

	boardController "github.com/akashc777/OneCamp/controllers/Board"
	"github.com/akashc777/OneCamp/helpers"
	"github.com/akashc777/OneCamp/initializers/postgresInit"
	customMiddleware "github.com/akashc777/OneCamp/middleware"
	dgraphStruct "github.com/akashc777/OneCamp/models/dgraph"
	"github.com/akashc777/OneCamp/tests/integration"
)

func TestTheBoardListNarrowsToTheBoardsYouMayEdit(t *testing.T) {
	t.Setenv("JWT_SECRET", "board-list-editable-secret")
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
	edited := time.Date(2026, 10, 27, 9, 0, 0, 0, time.UTC)
	// board is a private board, edited a minute after the one before it.
	board := func(node, title, owner string) map[string]any {
		edited = edited.Add(time.Minute)
		return map[string]any{"uid": "_:" + node, "dgraph.type": "Board", "board_uuid": uuid.NewString(), "board_title": title,
			"board_private": true, "board_deleted_at": live, "board_updated_at": edited.Format(time.RFC3339),
			"board_created_by": map[string]any{"uid": "_:" + owner}}
	}
	mine, edit, read, comment := board("mine", "Roadmap mine", "me"), board("edit", "Roadmap edit", "them"),
		board("read", "Roadmap read", "them"), board("comment", "Roadmap comment", "them")
	edit["board_editing_users"] = []map[string]any{{"uid": "_:me"}}
	read["board_reading_users"] = []map[string]any{{"uid": "_:me"}}
	comment["board_commenting_users"] = []map[string]any{{"uid": "_:me"}}
	unshared := board("unshared", "Roadmap unshared", "them")
	gone := board("gone", "Roadmap gone", "me")
	gone["board_deleted_at"] = edited.Format(time.RFC3339)
	dg.Mutate(t, []map[string]any{
		{"uid": "_:me", "dgraph.type": "User", "user_uuid": me.String(), "user_name": "me"},
		{"uid": "_:them", "dgraph.type": "User", "user_uuid": them.String(), "user_name": "them"},
		mine, edit, read, comment, unshared, gone,
	})

	r := chi.NewRouter()
	r.Use(customMiddleware.VerifyAuth)
	r.Get("/board/getBoardList", boardController.GetBoardList)
	type page struct {
		titles    []string
		count     uint64
		pageCount uint64
	}
	// list asks for the board list as a person, and answers the boards' titles
	// in the order listed, with the count and the number of pages.
	list := func(t *testing.T, as uuid.UUID, query string) page {
		t.Helper()
		token, err := helpers.SignSessionToken(as.String(), helpers.TokenTypeAccess, time.Now().Add(5*time.Minute).Unix())
		if err != nil {
			t.Fatal(err)
		}
		req := httptest.NewRequest(http.MethodGet, "/board/getBoardList?"+query, nil)
		req.AddCookie(&http.Cookie{Name: "Authorization", Value: token})
		req.AddCookie(&http.Cookie{Name: "DeviceId", Value: "device-1"})
		rec := httptest.NewRecorder()
		r.ServeHTTP(rec, req)
		if rec.Code != http.StatusOK {
			t.Fatalf("GET %s: %d %s", query, rec.Code, rec.Body.String())
		}
		var got struct {
			PageCount uint64                       `json:"pageCount"`
			Data      dgraphStruct.DgraphBoardList `json:"data"`
		}
		if err := json.Unmarshal(rec.Body.Bytes(), &got); err != nil {
			t.Fatalf("GET %s: %v", query, err)
		}
		p := page{titles: []string{}, count: got.Data.Count, pageCount: got.PageCount}
		for _, b := range got.Data.Boards {
			p.titles = append(p.titles, b.Title)
		}
		return p
	}
	want := func(t *testing.T, got, want page) {
		t.Helper()
		if !reflect.DeepEqual(got, want) {
			t.Fatalf("listed %+v, want %+v", got, want)
		}
	}

	t.Run("without the flag, every board shared with them is listed, newest edited first", func(t *testing.T) {
		want(t, list(t, me, ""), page{titles: []string{"Roadmap comment", "Roadmap read", "Roadmap edit", "Roadmap mine"}, count: 4, pageCount: 1})
	})
	t.Run("with editable=1, a board they may only read or comment on is left out", func(t *testing.T) {
		want(t, list(t, me, "editable=1"), page{titles: []string{"Roadmap edit", "Roadmap mine"}, count: 2, pageCount: 1})
	})
	t.Run("the count and the pages are the narrowed list's", func(t *testing.T) {
		want(t, list(t, me, "editable=1&pageSize=1"), page{titles: []string{"Roadmap edit"}, count: 2, pageCount: 2})
		want(t, list(t, me, "editable=1&pageSize=1&pageIndex=1"), page{titles: []string{"Roadmap mine"}, count: 2, pageCount: 2})
	})
	t.Run("a search narrows the editable list further", func(t *testing.T) {
		want(t, list(t, me, "editable=1&search=edit"), page{titles: []string{"Roadmap edit"}, count: 1, pageCount: 1})
		want(t, list(t, me, "editable=1&search=read"), page{titles: []string{}, count: 0, pageCount: 1})
	})
	t.Run("the owner of the shared boards may edit all of them", func(t *testing.T) {
		want(t, list(t, them, "editable=1"), page{titles: []string{"Roadmap unshared", "Roadmap comment", "Roadmap read", "Roadmap edit"}, count: 4, pageCount: 1})
	})
}
