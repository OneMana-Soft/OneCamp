//go:build integration

package router

// An export's tell to the audit log is limited by the member who asks, not by
// the address (V-363), on /board/recordExport and /doc/recordExport as router
// mounts them, against real Postgres (the docs.export.members policy is read
// there), Redis and Dgraph. Ana and Ben sit behind one office's address: Ana's
// 61st tell of a board is refused with the export's words while Ben's is
// recorded, and her first tell of a doc still passes, since each surface
// keeps its own count. The sign-in is a stand-in that puts the member on the
// request, as VerifyAuth does. The doc's pipeline is the recording one
// (docsContentIO_test.go), so the doc's own access check stays out of it.
//
// A tell the budget holds back leaves the admin log one row (miro-44 (a)): the
// first refusal in a member's window on a surface writes "<surface>.export_held"
// under that surface's category, naming the member, and later refusals in the
// same window write none, so a member who keeps pressing can't flood the chain.
//
// Run: PKGS=./router/ (go test -tags=integration ./router/ -run TestAnExportsTellsAreCountedPerMemberBehindOneAddress -v)

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/go-chi/chi/v5"
	"github.com/google/uuid"

	contentIOBusiness "github.com/akashc777/OneCamp/business/ContentIO"
	"github.com/akashc777/OneCamp/helpers"
	"github.com/akashc777/OneCamp/initializers/postgresInit"
	userModels "github.com/akashc777/OneCamp/models/postgres/User"
	"github.com/akashc777/OneCamp/tests/integration"
)

func TestAnExportsTellsAreCountedPerMemberBehindOneAddress(t *testing.T) {
	ctx := context.Background()
	env := integration.SetupEnv(t)
	if err := postgresInit.ConnectPostgres(ctx, env.DSN); err != nil {
		t.Fatal(err)
	}
	integration.SetupRedis(t)
	integration.StubMqttClient(t)
	dg := integration.SetupDgraph(t)

	saved := contentIOBusiness.Pipeline
	t.Cleanup(func() { contentIOBusiness.Pipeline = saved })
	pipe := &recording{}
	contentIOBusiness.Pipeline = pipe

	board := uuid.NewString()
	people := map[string]uuid.UUID{"ana": uuid.New(), "ben": uuid.New()}
	nodes := []map[string]any{{"uid": "_:board", "dgraph.type": "Board", "board_uuid": board, "board_title": "Launch plan",
		"board_private": false, "board_deleted_at": "0001-01-01T00:00:00Z"}}
	for name, id := range people {
		if _, err := env.PG.Exec(`INSERT INTO users (id, email_id, username, display_name, created_at, updated_at) VALUES ($1, $2, $3, $4, NOW(), NOW())`, id, id.String()+"@example.test", name, name); err != nil {
			t.Fatal(err)
		}
		nodes = append(nodes, map[string]any{"uid": "_:" + name, "dgraph.type": "User", "user_uuid": id.String(), "user_name": name,
			"user_full_name": name, "user_deleted_at": "0001-01-01T00:00:00Z"})
	}
	uids := dg.Mutate(t, nodes)
	member := func(name string) userModels.UserInfo {
		var u userModels.UserInfo
		u.UserPostgresInfo.Id = people[name]
		u.UserDgraphInfo.Uid = uids[name]
		u.UserDgraphInfo.Uuid = people[name].String()
		return u
	}

	boards, doc := chi.NewRouter(), chi.NewRouter()
	mountBoardExportTell(boards)
	mountContentIO(docsRouters{Doc: doc, Member: chi.NewRouter(), Admin: chi.NewRouter(), Internal: chi.NewRouter(), V1: chi.NewRouter(), Files: chi.NewRouter()})

	// tell sends one export's tell as the web does, signed in as name, from
	// the office's one address.
	tell := func(r http.Handler, name, body string) *httptest.ResponseRecorder {
		t.Helper()
		req := httptest.NewRequest(http.MethodPost, "/recordExport", strings.NewReader(body))
		req.RemoteAddr = "203.0.113.7:41000"
		req = req.WithContext(context.WithValue(req.Context(), helpers.UserInfoContextKey, member(name)))
		w := httptest.NewRecorder()
		r.ServeHTTP(w, req)
		return w
	}
	refusedInWords := func(t *testing.T, w *httptest.ResponseRecorder) {
		t.Helper()
		var out map[string]any
		_ = json.Unmarshal(w.Body.Bytes(), &out)
		if w.Code != http.StatusTooManyRequests || out["msg"] != exportTellsMsg || w.Header().Get("Retry-After") == "" {
			t.Fatalf("over the budget answered %d %v (Retry-After %q), want 429 with the export's words", w.Code, out, w.Header().Get("Retry-After"))
		}
	}
	// held is the admin log's export_held rows for action, as their actor ids.
	held := func(t *testing.T, action string) []string {
		t.Helper()
		rows, err := env.PG.Query(`SELECT coalesce(actor_id::text, ''), category FROM admin_audit_log WHERE action = $1 ORDER BY seq`, action)
		if err != nil {
			t.Fatal(err)
		}
		defer rows.Close()
		var out []string
		for rows.Next() {
			var id, category string
			if err := rows.Scan(&id, &category); err != nil {
				t.Fatal(err)
			}
			out = append(out, id+" "+category)
		}
		return out
	}
	boardTell := `{"board_uuid":"` + board + `","format":"pdf"}`
	docTell := `{"doc_uuid":"33333333-3333-3333-3333-333333333333","format":"md"}`

	t.Run("a board's tells", func(t *testing.T) {
		for i := 1; i <= 60; i++ {
			if w := tell(boards, "ana", boardTell); w.Code != http.StatusOK {
				t.Fatalf("Ana's tell %d of the board answered %d %s", i, w.Code, w.Body)
			}
		}
		refusedInWords(t, tell(boards, "ana", boardTell))
		if got, want := held(t, "board.export_held"), []string{people["ana"].String() + " boards"}; strings.Join(got, ",") != strings.Join(want, ",") {
			t.Fatalf("Ana's held board export left the admin log %v, want one row naming her under boards %v", got, want)
		}
		refusedInWords(t, tell(boards, "ana", boardTell))
		if got := held(t, "board.export_held"); len(got) != 1 {
			t.Fatalf("Ana's second held board export in the window left %d export_held rows, want still 1: %v", len(got), got)
		}
		if w := tell(boards, "ben", boardTell); w.Code != http.StatusOK {
			t.Fatalf("Ben's tell from Ana's address, after her 61st, answered %d %s", w.Code, w.Body)
		}
	})

	t.Run("a doc's tells keep their own count", func(t *testing.T) {
		for i := 1; i <= 60; i++ {
			if w := tell(doc, "ana", docTell); w.Code != http.StatusNoContent {
				t.Fatalf("Ana's tell %d of the doc (her board's budget spent) answered %d %s", i, w.Code, w.Body)
			}
		}
		refusedInWords(t, tell(doc, "ana", docTell))
		refusedInWords(t, tell(doc, "ana", docTell))
		if got, want := held(t, "doc.export_held"), []string{people["ana"].String() + " docs"}; strings.Join(got, ",") != strings.Join(want, ",") {
			t.Fatalf("Ana's two held doc exports left the admin log %v, want one row naming her under docs %v", got, want)
		}
		if w := tell(doc, "ben", docTell); w.Code != http.StatusNoContent {
			t.Fatalf("Ben's tell of the doc from Ana's address answered %d %s", w.Code, w.Body)
		}
		if got := len(pipe.asked); got != 61 {
			t.Errorf("the pipeline recorded %d doc tells, want 61 (Ana's 60 and Ben's 1; the refused one never reaches it)", got)
		}
	})
}
