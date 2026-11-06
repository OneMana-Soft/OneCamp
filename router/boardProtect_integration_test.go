//go:build integration

package router

// Protected objects on a board (miro-44), on /board/protect as router mounts
// it, against real Postgres (the people, their admin flag and the admin audit
// log's chain) and Dgraph (the board, its owner and its editor). Sam owns the
// board and protects two objects; Ed may edit it but is refused, and the
// refusal is a row of its own; Ada, a workspace admin, lifts the protection;
// a private board nobody shared with Ada answers 404 and leaves no row. The
// live op is a stand-in that records what it was asked (the guard itself is
// proven over real sockets in the collaboration service's
// live/protect.test.js). The sign-in is a stand-in that puts the member on
// the request, as VerifyAuth does.
//
// Run: PKGS=./router/ (go test -tags=integration ./router/ -run TestOnlyTheOwnerOrAnAdminProtectsAndTheLogHasEveryLockAndLift -v)

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/go-chi/chi/v5"
	"github.com/google/uuid"

	boardController "github.com/akashc777/OneCamp/controllers/Board"
	"github.com/akashc777/OneCamp/helpers"
	"github.com/akashc777/OneCamp/initializers/postgresInit"
	userModels "github.com/akashc777/OneCamp/models/postgres/User"
	"github.com/akashc777/OneCamp/services/collab"
	"github.com/akashc777/OneCamp/tests/integration"
)

func TestOnlyTheOwnerOrAnAdminProtectsAndTheLogHasEveryLockAndLift(t *testing.T) {
	ctx := context.Background()
	env := integration.SetupEnv(t)
	if err := postgresInit.ConnectPostgres(ctx, env.DSN); err != nil {
		t.Fatal(err)
	}
	integration.SetupRedis(t)
	integration.StubMqttClient(t)
	dg := integration.SetupDgraph(t)

	saved := collab.ProtectBoard
	t.Cleanup(func() { collab.ProtectBoard = saved })
	var asked []string
	collab.ProtectBoard = func(_ context.Context, board string, ids []string, on bool, by, name string) (int, error) {
		asked = append(asked, fmt.Sprintf("%s %v %v %s %s", board, ids, on, by, name))
		return len(ids), nil
	}

	board, private := uuid.NewString(), uuid.NewString()
	people := map[string]uuid.UUID{"sam": uuid.New(), "ed": uuid.New(), "ada": uuid.New()}
	nodes := []map[string]any{}
	for name, id := range people {
		if _, err := env.PG.Exec(`INSERT INTO users (id, email_id, username, display_name, created_at, updated_at) VALUES ($1, $2, $3, $4, NOW(), NOW())`,
			id, name+"@example.test", name, name); err != nil {
			t.Fatal(err)
		}
		nodes = append(nodes, map[string]any{"uid": "_:" + name, "dgraph.type": "User", "user_uuid": id.String(), "user_name": name,
			"user_full_name": name, "user_deleted_at": "0001-01-01T00:00:00Z"})
	}
	// Ada is a workspace admin as the app makes one: her email in admin_users.
	if _, err := env.PG.Exec(`INSERT INTO admin_users (email_id) VALUES ('ada@example.test')`); err != nil {
		t.Fatal(err)
	}
	nodes = append(nodes,
		map[string]any{"uid": "_:board", "dgraph.type": "Board", "board_uuid": board, "board_title": "Launch plan", "board_private": false,
			"board_deleted_at": "0001-01-01T00:00:00Z", "board_created_by": map[string]any{"uid": "_:sam"}, "board_editing_users": []map[string]any{{"uid": "_:ed"}}},
		map[string]any{"uid": "_:private", "dgraph.type": "Board", "board_uuid": private, "board_title": "Salaries", "board_private": true,
			"board_deleted_at": "0001-01-01T00:00:00Z", "board_created_by": map[string]any{"uid": "_:sam"}},
	)
	uids := dg.Mutate(t, nodes)
	member := func(name string) userModels.UserInfo {
		var u userModels.UserInfo
		u.UserPostgresInfo.Id = people[name]
		u.UserPostgresInfo.EmailID = name + "@example.test"
		u.UserDgraphInfo.Uid = uids[name]
		u.UserDgraphInfo.Uuid = people[name].String()
		u.UserDgraphInfo.UserName = name
		return u
	}

	r := chi.NewRouter()
	r.Post("/protect", boardController.ProtectBoardElements)
	send := func(name, body string) *httptest.ResponseRecorder {
		t.Helper()
		req := httptest.NewRequest(http.MethodPost, "/protect", strings.NewReader(body))
		req = req.WithContext(context.WithValue(req.Context(), helpers.UserInfoContextKey, member(name)))
		w := httptest.NewRecorder()
		r.ServeHTTP(w, req)
		return w
	}
	rows := func(action string) []string {
		t.Helper()
		list, err := env.PG.Query(`SELECT summary, coalesce(actor_email, '') FROM admin_audit_log WHERE action = $1 ORDER BY seq`, action)
		if err != nil {
			t.Fatal(err)
		}
		defer list.Close()
		var out []string
		for list.Next() {
			var summary, actor string
			if err := list.Scan(&summary, &actor); err != nil {
				t.Fatal(err)
			}
			out = append(out, actor+": "+summary)
		}
		return out
	}
	changed := func(w *httptest.ResponseRecorder) float64 {
		var out map[string]any
		_ = json.Unmarshal(w.Body.Bytes(), &out)
		n, _ := out["changed"].(float64)
		return n
	}
	lock := `{"board_uuid":"` + board + `","element_ids":["e-1","e-2"],"protect":true}`
	lift := `{"board_uuid":"` + board + `","element_ids":["e-1"],"protect":false}`

	if w := send("sam", lock); w.Code != http.StatusOK || changed(w) != 2 {
		t.Fatalf("Sam protecting on his board answered %d %s", w.Code, w.Body)
	}
	if got, want := rows("board.protect.locked"), []string{"sam@example.test: Protected 2 objects on ‘Launch plan’"}; fmt.Sprint(got) != fmt.Sprint(want) {
		t.Fatalf("the lock's row: %q, want %q", got, want)
	}

	if w := send("ed", lift); w.Code != http.StatusForbidden {
		t.Fatalf("Ed, an editor, lifting answered %d %s, want 403", w.Code, w.Body)
	}
	if got, want := rows("board.protect_refused"), []string{"ed@example.test: Refused lifting protection from 1 object on ‘Launch plan’"}; fmt.Sprint(got) != fmt.Sprint(want) {
		t.Fatalf("the refusal's row: %q, want %q", got, want)
	}

	if w := send("ada", lift); w.Code != http.StatusOK {
		t.Fatalf("Ada, an admin, lifting answered %d %s", w.Code, w.Body)
	}
	if got, want := rows("board.protect.lifted"), []string{"ada@example.test: Lifted protection from 1 object on ‘Launch plan’"}; fmt.Sprint(got) != fmt.Sprint(want) {
		t.Fatalf("the lift's row: %q, want %q", got, want)
	}

	if w := send("ada", `{"board_uuid":"`+private+`","element_ids":["e-9"],"protect":false}`); w.Code != http.StatusNotFound {
		t.Fatalf("Ada on a private board nobody shared with her answered %d %s, want 404", w.Code, w.Body)
	}
	if w := send("sam", `{"board_uuid":"`+board+`","element_ids":["e-1"]}`); w.Code != http.StatusBadRequest {
		t.Fatalf("no protect flag answered %d, want 400", w.Code)
	}
	if got := len(rows("board.protect_refused")) + len(rows("board.protect.locked")) + len(rows("board.protect.lifted")); got != 3 {
		t.Fatalf("%d protection rows, want 3 (the 404 and the 400 leave none)", got)
	}

	want := []string{
		board + " [e-1 e-2] true " + people["sam"].String() + " sam",
		board + " [e-1] false " + people["ada"].String() + " ada",
	}
	if fmt.Sprint(asked) != fmt.Sprint(want) {
		t.Fatalf("the live op was asked %q, want %q (never for Ed, the private board or a bad request)", asked, want)
	}
}
