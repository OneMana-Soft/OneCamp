//go:build integration
// +build integration

package integration_test

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"sort"
	"strings"
	"testing"
	"time"

	"github.com/go-chi/chi/v5"
	"github.com/google/uuid"

	boardAdapter "github.com/akashc777/OneCamp/adapter/Board"
	docAdapter "github.com/akashc777/OneCamp/adapter/Doc"
	boardBusiness "github.com/akashc777/OneCamp/business/Board"
	boardController "github.com/akashc777/OneCamp/controllers/Board"
	docDomain "github.com/akashc777/OneCamp/domain/Doc"
	"github.com/akashc777/OneCamp/helpers"
	"github.com/akashc777/OneCamp/initializers/postgresInit"
	customMiddleware "github.com/akashc777/OneCamp/middleware"
	"github.com/akashc777/OneCamp/tests/integration"
)

// holders answers the user uuids at the end of pred on the node with uid.
func holders(t *testing.T, dg *integration.DgraphEnv, uid, pred string) []string {
	t.Helper()
	var got struct {
		N []map[string][]struct {
			UUID string `json:"user_uuid"`
		} `json:"n"`
	}
	if err := json.Unmarshal(dg.Query(t, fmt.Sprintf(`{ n(func: uid(%s)) { %s { user_uuid } } }`, uid, pred)), &got); err != nil {
		t.Fatal(err)
	}
	var out []string
	for _, n := range got.N {
		for _, h := range n[pred] {
			out = append(out, h.UUID)
		}
	}
	sort.Strings(out)
	return out
}

// A board's sharing names people by their uuids. An id that isn't one is
// refused at the door, and the board's sharing stays as it was; a uuid that
// names no one writes nothing, where it used to give the role to a new, empty
// node; the people a change does name gain and lose the role as before; and
// someone below the owner still can't share the board.
func TestBoardSharingNamesPeopleByTheirUUIDs(t *testing.T) {
	t.Setenv("JWT_SECRET", "board-sharing-by-uuid-secret")
	t.Setenv("ALLOWED_USERS", "")
	env := integration.SetupEnv(t)
	ctx := context.Background()
	if err := postgresInit.ConnectPostgres(ctx, env.DSN); err != nil {
		t.Fatalf("wire the project pool at the test database: %v", err)
	}
	integration.SetupRedis(t)
	integration.StubMqttClient(t)
	dg := integration.SetupDgraph(t)

	owner, ed, vi := uuid.New(), uuid.New(), uuid.NewString()
	for name, id := range map[string]uuid.UUID{"owner": owner, "ed": ed} {
		if _, err := env.PG.Exec(`INSERT INTO users (id, email_id, username, display_name, created_at, updated_at)
			VALUES ($1, $2, $3, $4, NOW(), NOW())`, id, name+"@example.com", name, name); err != nil {
			t.Fatalf("seed %s: %v", name, err)
		}
	}
	people := dg.Mutate(t, []map[string]any{
		{"uid": "_:owner", "dgraph.type": "User", "user_uuid": owner.String(), "user_name": "owner"},
		{"uid": "_:ed", "dgraph.type": "User", "user_uuid": ed.String(), "user_name": "ed"},
		{"uid": "_:vi", "dgraph.type": "User", "user_uuid": vi, "user_name": "vi"},
	})
	// newBoard is a private board of the owner's, shared with nobody: each
	// case has its own, so none reads what another left.
	newBoard := func(t *testing.T) (id, uid string) {
		t.Helper()
		id = uuid.NewString()
		return id, dg.Mutate(t, map[string]any{"uid": "_:b", "dgraph.type": "Board", "board_uuid": id, "board_title": "Launch plan",
			"board_private": true, "board_deleted_at": "0001-01-01T00:00:00Z", "board_created_by": map[string]any{"uid": people["owner"]}})["b"]
	}

	r := chi.NewRouter()
	r.Use(customMiddleware.VerifyAuth)
	r.Post("/board/updateBoardPermissions", boardController.UpdateBoardPermissions)
	share := func(as uuid.UUID, board string, body map[string]any) int {
		t.Helper()
		token, err := helpers.SignSessionToken(as.String(), helpers.TokenTypeAccess, time.Now().Add(5*time.Minute).Unix())
		if err != nil {
			t.Fatal(err)
		}
		body["board_uuid"] = board
		raw, _ := json.Marshal(body)
		req := httptest.NewRequest(http.MethodPost, "/board/updateBoardPermissions", bytes.NewReader(raw))
		req.AddCookie(&http.Cookie{Name: "Authorization", Value: token})
		req.AddCookie(&http.Cookie{Name: "DeviceId", Value: "device-1"})
		rec := httptest.NewRecorder()
		r.ServeHTTP(rec, req)
		return rec.Code
	}

	t.Run("an id that isn't a uuid is refused, and nothing changes", func(t *testing.T) {
		board, uid := newBoard(t)
		if code := share(owner, board, map[string]any{"add_editors": []string{ed.String(), "not-a-uuid"}}); code != http.StatusBadRequest {
			t.Errorf("answered %d, want 400", code)
		}
		if got := countOf(t, dg, uid, "board_editing_users"); got != 0 {
			t.Errorf("the board has %d editors after a refused change, want none", got)
		}
	})

	t.Run("a uuid that names no one writes nothing", func(t *testing.T) {
		board, uid := newBoard(t)
		if code := share(owner, board, map[string]any{"add_editors": []string{uuid.NewString()}}); code != http.StatusOK {
			t.Errorf("answered %d, want 200", code)
		}
		if got := countOf(t, dg, uid, "board_editing_users"); got != 0 {
			t.Errorf("the board has %d editors, want none: an id naming no one became one", got)
		}
	})

	t.Run("a person is given the role, and taken off it", func(t *testing.T) {
		board, uid := newBoard(t)
		// Named in capitals, as a uuid may be written: still Ed.
		if code := share(owner, board, map[string]any{"add_editors": []string{strings.ToUpper(ed.String())}}); code != http.StatusOK {
			t.Fatalf("answered %d, want 200", code)
		}
		if got := holders(t, dg, uid, "board_editing_users"); len(got) != 1 || got[0] != ed.String() {
			t.Fatalf("editors %v, want Ed alone", got)
		}
		if code := share(owner, board, map[string]any{"remove_editors": []string{ed.String()}}); code != http.StatusOK {
			t.Fatalf("answered %d, want 200", code)
		}
		if got := holders(t, dg, uid, "board_editing_users"); len(got) != 0 {
			t.Errorf("editors %v after Ed was taken off, want none", got)
		}
	})

	t.Run("someone below the owner can't share it", func(t *testing.T) {
		board, uid := newBoard(t)
		if code := share(owner, board, map[string]any{"add_editors": []string{ed.String()}}); code != http.StatusOK {
			t.Fatalf("the owner making Ed an editor answered %d, want 200", code)
		}
		// The route's refusal today: the business's owner check, answered 400.
		if code := share(ed, board, map[string]any{"add_viewers": []string{vi}}); code != http.StatusBadRequest {
			t.Errorf("Ed, an editor, sharing the board answered %d, want 400", code)
		}
		if got := countOf(t, dg, uid, "board_reading_users"); got != 0 {
			t.Errorf("the board has %d viewers after an editor tried to share it, want none", got)
		}
	})

	t.Run("the change itself refuses an id that isn't a uuid", func(t *testing.T) {
		board, uid := newBoard(t)
		err := boardBusiness.UpdateBoardPermissions(ctx, boardAdapter.InputUpdateBoardPermissions{
			BoardId: board, AddViewers: []string{ed.String(), "not-a-uuid"}}, people["owner"])
		if err == nil {
			t.Error("a viewer named by something that isn't a uuid was taken")
		}
		if got := countOf(t, dg, uid, "board_reading_users"); got != 0 {
			t.Errorf("the board has %d viewers after a refused change, want none", got)
		}
	})
}

// A doc's sharing through the graph model (as the demo seeds it) names people
// the same way: the person named gains and loses the role, and a uuid that
// names no one writes nothing.
func TestDocSharingByTheModelNamesPeopleByTheirUUIDs(t *testing.T) {
	dg := integration.SetupDgraph(t)
	ctx := context.Background()
	ed := uuid.NewString()
	doc := uuid.NewString()
	uids := dg.Mutate(t, []map[string]any{
		{"uid": "_:ed", "dgraph.type": "User", "user_uuid": ed, "user_name": "ed"},
		{"uid": "_:d", "dgraph.type": "Doc", "doc_uuid": doc, "doc_title": "Launch notes", "doc_deleted_at": "0001-01-01T00:00:00Z"},
	})

	if err := docDomain.UpdateDocPermissions(ctx, &docAdapter.InputUpdateDocPermissions{
		DocId: doc, AddEditors: []string{ed}, AddViewers: []string{uuid.NewString()}}); err != nil {
		t.Fatal(err)
	}
	if got := holders(t, dg, uids["d"], "doc_editing_users"); len(got) != 1 || got[0] != ed {
		t.Errorf("editors %v, want Ed alone", got)
	}
	if got := countOf(t, dg, uids["d"], "doc_reading_users"); got != 0 {
		t.Errorf("the doc has %d viewers, want none: an id naming no one became one", got)
	}
	if err := docDomain.UpdateDocPermissions(ctx, &docAdapter.InputUpdateDocPermissions{DocId: doc, RemoveEditors: []string{ed}}); err != nil {
		t.Fatal(err)
	}
	if got := countOf(t, dg, uids["d"], "doc_editing_users"); got != 0 {
		t.Errorf("the doc has %d editors after Ed was taken off, want none", got)
	}
}
