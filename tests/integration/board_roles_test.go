//go:build integration
// +build integration

package integration_test

// A board's roles on the server (miro-32, the lead's rulings R2, R3, R5):
// co-owners share and change settings but never delete, nor make or remove
// co-owners, nor touch the owner; editors share only while the board's
// switch is on, and never change its general access; and when an owner
// leaves, an admin's recovery (the docs' C6-2 path) gives the board to the
// heir, the owner before and its co-owners staying co-owners. A recovery to
// someone who isn't there moves nothing and records nothing.
//
// Run: go test -tags=integration ./tests/integration/ -run 'TestBoardRoles' -v

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/go-chi/chi/v5"
	"github.com/google/uuid"

	accessBusiness "github.com/akashc777/OneCamp/business/Access"
	boardBusiness "github.com/akashc777/OneCamp/business/Board"
	boardController "github.com/akashc777/OneCamp/controllers/Board"
	"github.com/akashc777/OneCamp/helpers"
	"github.com/akashc777/OneCamp/initializers/postgresInit"
	customMiddleware "github.com/akashc777/OneCamp/middleware"
	"github.com/akashc777/OneCamp/tests/integration"
)

func TestBoardRolesAnswerOnTheServer(t *testing.T) {
	t.Setenv("JWT_SECRET", "board-roles-secret")
	t.Setenv("ALLOWED_USERS", "")
	env := integration.SetupEnv(t)
	ctx := context.Background()
	if err := postgresInit.ConnectPostgres(ctx, env.DSN); err != nil {
		t.Fatalf("wire the project pool at the test database: %v", err)
	}
	integration.SetupRedis(t)
	integration.StubMqttClient(t)
	dg := integration.SetupDgraph(t)

	owner, co, ed, vi, heir := uuid.New(), uuid.New(), uuid.New(), uuid.New(), uuid.New()
	names := map[string]uuid.UUID{"owner": owner, "co": co, "ed": ed, "vi": vi, "heir": heir}
	var nodes []map[string]any
	for name, id := range names {
		if _, err := env.PG.Exec(`INSERT INTO users (id, email_id, username, display_name, created_at, updated_at)
			VALUES ($1, $2, $3, $4, NOW(), NOW())`, id, name+"@roles.test", name+"-roles", name); err != nil {
			t.Fatalf("seed %s: %v", name, err)
		}
		nodes = append(nodes, map[string]any{"uid": "_:" + name, "dgraph.type": "User", "user_uuid": id.String(), "user_name": name})
	}
	people := dg.Mutate(t, nodes)
	newBoard := func(t *testing.T) (id, uid string) {
		t.Helper()
		id = uuid.NewString()
		return id, dg.Mutate(t, map[string]any{"uid": "_:b", "dgraph.type": "Board", "board_uuid": id, "board_title": "Roadmap",
			"board_private": true, "board_deleted_at": "0001-01-01T00:00:00Z", "board_created_by": map[string]any{"uid": people["owner"]}})["b"]
	}

	r := chi.NewRouter()
	r.Use(customMiddleware.VerifyAuth)
	r.Post("/board/updateBoardPermissions", boardController.UpdateBoardPermissions)
	r.Post("/board/updateBoard", boardController.UpdateBoard)
	r.Post("/board/deleteBoard", boardController.DeleteBoard)
	r.Get("/board/getBoardPermissions", boardController.GetBoardPermissions)
	call := func(as uuid.UUID, path, board string, body map[string]any) (int, string) {
		t.Helper()
		token, err := helpers.SignSessionToken(as.String(), helpers.TokenTypeAccess, time.Now().Add(5*time.Minute).Unix())
		if err != nil {
			t.Fatal(err)
		}
		body["board_uuid"] = board
		raw, _ := json.Marshal(body)
		req := httptest.NewRequest(http.MethodPost, path, bytes.NewReader(raw))
		req.AddCookie(&http.Cookie{Name: "Authorization", Value: token})
		req.AddCookie(&http.Cookie{Name: "DeviceId", Value: "device-1"})
		rec := httptest.NewRecorder()
		r.ServeHTTP(rec, req)
		return rec.Code, rec.Body.String()
	}
	share := func(as uuid.UUID, board string, body map[string]any) int {
		code, _ := call(as, "/board/updateBoardPermissions", board, body)
		return code
	}
	ids := func(us ...uuid.UUID) []string {
		out := []string{}
		for _, u := range us {
			out = append(out, u.String())
		}
		return out
	}
	deletedOf := func(t *testing.T, uid string) bool {
		t.Helper()
		var got struct {
			N []struct {
				At string `json:"board_deleted_at"`
			} `json:"n"`
		}
		_ = json.Unmarshal(dg.Query(t, `{ n(func: uid(`+uid+`)) { board_deleted_at } }`), &got)
		return len(got.N) != 1 || got.N[0].At > "1970"
	}
	// ownerOf is the user_uuid the board's board_created_by names.
	ownerOf := func(t *testing.T, uid string) string {
		t.Helper()
		var made struct {
			N []struct {
				By struct {
					UUID string `json:"user_uuid"`
				} `json:"board_created_by"`
			} `json:"n"`
		}
		if err := json.Unmarshal(dg.Query(t, `{ n(func: uid(`+uid+`)) { board_created_by { user_uuid } } }`), &made); err != nil || len(made.N) != 1 {
			t.Fatalf("read the board's owner: %+v %v", made, err)
		}
		return made.N[0].By.UUID
	}

	t.Run("a co-owner shares and changes settings, but never deletes nor touches co-owners or the owner", func(t *testing.T) {
		board, uid := newBoard(t)
		if code := share(owner, board, map[string]any{"add_co_owners": ids(co)}); code != http.StatusOK {
			t.Fatalf("the owner making a co-owner answered %d", code)
		}
		if got := holders(t, dg, uid, "board_owning_users"); len(got) != 1 || got[0] != co.String() {
			t.Fatalf("co-owners %v", got)
		}
		if got := holders(t, dg, uid, "board_editing_users"); len(got) != 1 || got[0] != co.String() {
			t.Fatalf("a co-owner is an editor too: editors %v", got)
		}
		if code := share(co, board, map[string]any{"add_viewers": ids(vi)}); code != http.StatusOK {
			t.Errorf("the co-owner sharing answered %d, want 200", code)
		}
		for name, body := range map[string]map[string]any{
			"making a co-owner":         {"add_co_owners": ids(ed)},
			"removing the owner":        {"remove_editors": ids(owner)},
			"making the owner a viewer": {"add_viewers": ids(owner)},
		} {
			if code := share(co, board, body); code != http.StatusForbidden {
				t.Errorf("the co-owner %s answered %d, want 403", name, code)
			}
		}
		if code, body := call(co, "/board/deleteBoard", board, map[string]any{}); code != http.StatusForbidden || deletedOf(t, uid) {
			t.Errorf("the co-owner deleting the board answered %d %s, want 403 and the board live", code, body)
		}
		if code, _ := call(co, "/board/updateBoard", board, map[string]any{"board_editors_share": false}); code != http.StatusOK {
			t.Errorf("the co-owner turning the switch off answered %d, want 200", code)
		}
	})

	t.Run("an editor shares only while the switch is on, and never opens the board", func(t *testing.T) {
		board, uid := newBoard(t)
		if code := share(owner, board, map[string]any{"add_editors": ids(ed)}); code != http.StatusOK {
			t.Fatalf("the owner making an editor answered %d", code)
		}
		if code := share(ed, board, map[string]any{"add_viewers": ids(vi)}); code != http.StatusOK {
			t.Errorf("the editor inviting a viewer with the switch never set answered %d, want 200", code)
		}
		if code := share(ed, board, map[string]any{"remove_viewers": ids(vi)}); code != http.StatusForbidden {
			t.Errorf("the editor removing a viewer answered %d, want 403", code)
		}
		if code, _ := call(owner, "/board/updateBoard", board, map[string]any{"board_editors_share": false}); code != http.StatusOK {
			t.Fatalf("the owner turning the switch off answered %d", code)
		}
		code, body := call(ed, "/board/updateBoardPermissions", board, map[string]any{"add_editors": ids(co)})
		if code != http.StatusForbidden || !bytes.Contains([]byte(body), []byte("turned off sharing by editors")) {
			t.Errorf("the editor inviting with the switch off answered %d %s, want 403 with words", code, body)
		}
		if got := holders(t, dg, uid, "board_editing_users"); len(got) != 1 {
			t.Errorf("editors after the refusal %v", got)
		}
		for name, body := range map[string]map[string]any{"opening it to the workspace": {"board_private": false}, "turning the switch": {"board_editors_share": true}} {
			if code, _ := call(ed, "/board/updateBoard", board, body); code != http.StatusForbidden {
				t.Errorf("the editor %s answered %d, want 403", name, code)
			}
		}
	})

	t.Run("the owner taking a co-owner off with their edit leaves them no role (CR-270 F1)", func(t *testing.T) {
		board, uid := newBoard(t)
		if code := share(owner, board, map[string]any{"add_co_owners": ids(co)}); code != http.StatusOK {
			t.Fatalf("make the co-owner: %d", code)
		}
		if code := share(owner, board, map[string]any{"remove_co_owners": ids(co), "remove_editors": ids(co)}); code != http.StatusOK {
			t.Fatalf("take the co-owner off with their edit: %d", code)
		}
		for _, pred := range []string{"board_owning_users", "board_editing_users"} {
			if got := holders(t, dg, uid, pred); len(got) != 0 {
				t.Errorf("%s after taking the co-owner off with their edit: %v, want none", pred, got)
			}
		}
		token, err := helpers.SignSessionToken(owner.String(), helpers.TokenTypeAccess, time.Now().Add(5*time.Minute).Unix())
		if err != nil {
			t.Fatal(err)
		}
		req := httptest.NewRequest(http.MethodGet, "/board/getBoardPermissions?board_uuid="+board, nil)
		req.AddCookie(&http.Cookie{Name: "Authorization", Value: token})
		req.AddCookie(&http.Cookie{Name: "DeviceId", Value: "device-1"})
		rec := httptest.NewRecorder()
		r.ServeHTTP(rec, req)
		if body := rec.Body.String(); rec.Code != http.StatusOK || !strings.Contains(body, owner.String()) || strings.Contains(body, co.String()) {
			t.Errorf("GET permissions after it: %d %s, want the owner listed and the co-owner in no role", rec.Code, body)
		}
	})

	t.Run("an admin's recovery gives a board to the heir; the owner before and its co-owners stay co-owners", func(t *testing.T) {
		board, uid := newBoard(t)
		if code := share(owner, board, map[string]any{"add_co_owners": ids(co), "add_viewers": ids(heir)}); code != http.StatusOK {
			t.Fatalf("seed the board's roles: %d", code)
		}
		admin := accessBusiness.Subject{UserUUID: uuid.NewString(), Admin: true}
		if _, err := accessBusiness.Recover(ctx, admin, accessBusiness.Resource{Kind: boardBusiness.KindBoard, UUID: board}, heir.String(), "Left the workspace"); err != nil {
			t.Fatalf("recover the board: %v", err)
		}
		if got := ownerOf(t, uid); got != heir.String() {
			t.Errorf("the board's owner after the hand-on %s, want the heir", got)
		}
		want := ids(co, owner)
		if got := holders(t, dg, uid, "board_owning_users"); len(got) != 2 || !((got[0] == want[0] && got[1] == want[1]) || (got[0] == want[1] && got[1] == want[0])) {
			t.Errorf("co-owners after the hand-on %v, want the co-owner and the owner before", got)
		}
		if got := holders(t, dg, uid, "board_reading_users"); len(got) != 0 {
			t.Errorf("the heir keeps a lower role too: viewers %v", got)
		}
		var n int
		if err := env.PG.QueryRow(`SELECT count(*) FROM access_recoveries WHERE resource_kind = 'board' AND resource_uuid = $1 AND to_owner_uuid = $2 AND from_owner_uuid = $3`,
			board, heir, owner).Scan(&n); err != nil || n != 1 {
			t.Errorf("the recovery's record: %d %v", n, err)
		}
	})

	t.Run("an admin's recovery to someone who isn't there moves nothing and records nothing", func(t *testing.T) {
		board, uid := newBoard(t)
		if code := share(owner, board, map[string]any{"add_co_owners": ids(co)}); code != http.StatusOK {
			t.Fatalf("seed the board's co-owner: %d", code)
		}
		admin := accessBusiness.Subject{UserUUID: uuid.NewString(), Admin: true}
		nobody := uuid.NewString()
		if _, err := accessBusiness.Recover(ctx, admin, accessBusiness.Resource{Kind: boardBusiness.KindBoard, UUID: board}, nobody, "Left the workspace"); !errors.Is(err, accessBusiness.ErrNoSuchPrincipal) {
			t.Fatalf("recover the board to no one: %v, want ErrNoSuchPrincipal", err)
		}
		if got := ownerOf(t, uid); got != owner.String() {
			t.Errorf("the board's owner after the refused hand-on %s, want the owner still", got)
		}
		if got := holders(t, dg, uid, "board_owning_users"); len(got) != 1 || got[0] != co.String() {
			t.Errorf("co-owners after the refused hand-on %v, want the co-owner only", got)
		}
		var n int
		if err := env.PG.QueryRow(`SELECT count(*) FROM access_recoveries WHERE resource_kind = 'board' AND resource_uuid = $1`, board).Scan(&n); err != nil || n != 0 {
			t.Errorf("the refused hand-on was recorded: %d rows %v, want none", n, err)
		}
	})
}
