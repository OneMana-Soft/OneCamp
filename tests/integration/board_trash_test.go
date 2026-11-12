//go:build integration
// +build integration

package integration_test

// Deleted boards, and bringing one back (UX-TB-H2-BE, TABLES-BOARDS-13), over
// the real routes against Postgres, Dgraph, OpenSearch and MinIO. A delete
// was already soft (board_deleted_at) but nothing listed or restored one, so
// the web's "This can't be undone" was the only way out. Now:
//   - GET /board/trash lists the boards a person may bring back, newest
//     deletion first, a page at a time: the boards they made, and for a
//     workspace admin every board that isn't private too (a private board is
//     its owner's alone, as the audit log never names one to an admin). An
//     editor of someone else's board doesn't see it there.
//   - GET /board/trash/{board_uuid} answers one of those, or 404: the dead
//     link's "Restore this board" asks it.
//   - POST /board/restore {board_uuid} brings it back for its owner or an
//     admin: live in the graph, in the lists, and in search with its comments;
//     one board.restored row in the admin audit log. Anyone else, and any
//     board not in the trash for them, is answered 404 as a missing board is;
//     a board restored already is 409.
//   - A board a retention rule removed is gone: never written back (no bare
//     node with its uuid), 404 to its owner and to an admin alike.
//
// Run: go test -tags=integration ./tests/integration/ -run '^TestDeletedBoardsComeBackAndNeverOneARuleRemoved$' -v

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/opensearch-project/opensearch-go/v4/opensearchapi"

	outboxBusiness "github.com/akashc777/OneCamp/business/Outbox"
	retentionBusiness "github.com/akashc777/OneCamp/business/Retention"
	userBusiness "github.com/akashc777/OneCamp/business/User"
	"github.com/akashc777/OneCamp/helpers"
	"github.com/akashc777/OneCamp/initializers/opensearchInit"
	"github.com/akashc777/OneCamp/initializers/postgresInit"
	openSearchStruct "github.com/akashc777/OneCamp/models/openSearch"
	userModels "github.com/akashc777/OneCamp/models/postgres/User"
	"github.com/akashc777/OneCamp/router"
	"github.com/akashc777/OneCamp/tests/integration"
)

func TestDeletedBoardsComeBackAndNeverOneARuleRemoved(t *testing.T) {
	t.Setenv("JWT_SECRET", "board-trash-integration-secret")
	t.Setenv("ALLOWED_USERS", "")
	t.Setenv("CSRF_DISABLED", "1")
	ctx := context.Background()
	env := integration.SetupEnv(t)
	if err := postgresInit.ConnectPostgres(ctx, env.DSN); err != nil {
		t.Fatal(err)
	}
	integration.SetupRedis(t)
	integration.StubMqttClient(t)
	dg := integration.SetupDgraph(t)
	startMinio(t)
	search := integration.SetupOpenSearch(t)
	if err := opensearchInit.ConnectOpenSearch(&opensearchInit.OpenSearchConfig{Host: search.URL}); err != nil {
		t.Fatalf("create the indices: %v", err)
	}
	db := postgresInit.DBConn.SqlDB

	person := func(email, name string) uuid.UUID {
		t.Helper()
		id, _, err := userBusiness.CreateUserWithMethod(ctx, email, name, nil, userModels.AuthMethodEmail, false)
		if err != nil {
			t.Fatal(err)
		}
		return id
	}
	ada := person("ada@board-trash.test", "Ada Admin")
	if err := userBusiness.CreateAdminUser(ctx, "ada@board-trash.test"); err != nil {
		t.Fatal(err)
	}
	sam, lee, kim := person("sam@board-trash.test", "Sam"), person("lee@board-trash.test", "Lee"), person("kim@board-trash.test", "Kim")
	uidOf := map[string]string{}
	var users struct {
		U []struct {
			UID  string `json:"uid"`
			UUID string `json:"user_uuid"`
		} `json:"u"`
	}
	raw := dg.Query(t, `{ u(func: eq(user_uuid, ["`+sam.String()+`", "`+lee.String()+`"])) { uid user_uuid } }`)
	if err := json.Unmarshal(raw, &users); err != nil || len(users.U) != 2 {
		t.Fatalf("the people's nodes: %s (%v)", raw, err)
	}
	for _, u := range users.U {
		uidOf[u.UUID] = u.UID
	}

	now := time.Now().UTC()
	ago := func(days int) string { return now.AddDate(0, 0, -days).Format(time.RFC3339) }
	const live = "0001-01-01T00:00:00Z"
	ids := map[string]string{}
	seed := func(title string, maker uuid.UUID, private bool, deleted string, editors ...uuid.UUID) {
		t.Helper()
		id := uuid.NewString()
		ids[title] = id
		node := map[string]any{"uid": "_:b", "dgraph.type": "Board", "board_uuid": id, "board_title": title, "board_private": private,
			"board_created_at": ago(90), "board_updated_at": ago(60), "board_deleted_at": deleted,
			"board_created_by": map[string]any{"uid": uidOf[maker.String()]}}
		var edit []map[string]any
		for _, e := range append([]uuid.UUID{maker}, editors...) {
			edit = append(edit, map[string]any{"uid": uidOf[e.String()]})
		}
		node["board_editing_users"] = edit
		dg.Mutate(t, []map[string]any{node,
			{"uid": "_:c", "dgraph.type": "Comment", "comment_uuid": uuid.NewString(), "comment_text": "a note on " + title, "comment_board": map[string]any{"uid": "_:b"}}})
		for index, doc := range map[string]map[string]any{
			openSearchStruct.BOARD_INDEX:   {"board_uuid": id, "board_title": title, "board_private": private},
			openSearchStruct.COMMENT_INDEX: {"comment_board_id": id, "comment_text": "a note on " + title},
		} {
			docID := id
			if index == openSearchStruct.COMMENT_INDEX {
				docID = uuid.NewString()
			}
			b, _ := json.Marshal(map[string]any{"doc": doc})
			if err := opensearchInit.UpsertDocument(ctx, index, docID, bytes.NewReader(b)); err != nil {
				t.Fatal(err)
			}
		}
	}
	seed("Sam's plan", sam, false, live, lee)
	seed("Sam's secret", sam, true, live)
	seed("Lee's sketch", lee, false, ago(5))
	seed("Sam's old map", sam, false, ago(40))

	routes := router.Routes()
	call := func(as uuid.UUID, method, path string, body any) (int, json.RawMessage, string) {
		t.Helper()
		token, err := helpers.SignSessionToken(as.String(), helpers.TokenTypeAccess, time.Now().Add(10*time.Minute).Unix())
		if err != nil {
			t.Fatal(err)
		}
		var rawBody []byte
		if body != nil {
			rawBody, _ = json.Marshal(body)
		}
		req := httptest.NewRequest(method, path, bytes.NewReader(rawBody))
		req.AddCookie(&http.Cookie{Name: "Authorization", Value: token})
		req.AddCookie(&http.Cookie{Name: "DeviceId", Value: "device-1"})
		rec := httptest.NewRecorder()
		routes.ServeHTTP(rec, req)
		var out struct {
			Data json.RawMessage `json:"data"`
		}
		_ = json.Unmarshal(rec.Body.Bytes(), &out)
		return rec.Code, out.Data, rec.Body.String()
	}
	type item struct {
		UUID      string     `json:"board_uuid"`
		Title     string     `json:"board_title"`
		Private   bool       `json:"board_private"`
		DeletedAt *time.Time `json:"deleted_at"`
		Owner     *struct {
			UUID string `json:"user_uuid"`
		} `json:"owner"`
		Mine bool `json:"mine"`
	}
	trash := func(as uuid.UUID, query string) ([]string, string) {
		t.Helper()
		code, data, body := call(as, http.MethodGet, "/board/trash"+query, nil)
		var page struct {
			Boards []item `json:"boards"`
			Next   string `json:"next"`
		}
		if code != http.StatusOK || json.Unmarshal(data, &page) != nil {
			t.Fatalf("GET /board/trash%s: %d %s", query, code, body)
		}
		titles := []string{}
		for _, b := range page.Boards {
			if b.DeletedAt == nil || b.DeletedAt.IsZero() {
				t.Errorf("%q is listed without its deletion time", b.Title)
			}
			titles = append(titles, b.Title)
		}
		return titles, page.Next
	}
	wantTrash := func(as uuid.UUID, who string, want ...string) {
		t.Helper()
		if want == nil {
			want = []string{}
		}
		if got, _ := trash(as, ""); !reflect.DeepEqual(got, want) {
			t.Errorf("%s's deleted boards: %q, want %q", who, got, want)
		}
	}
	restore := func(as uuid.UUID, title string) (int, string) {
		t.Helper()
		code, _, body := call(as, http.MethodPost, "/board/restore", map[string]string{"board_uuid": ids[title]})
		return code, body
	}

	// Sam deletes two boards of their own, the private one last.
	for _, title := range []string{"Sam's plan", "Sam's secret"} {
		if code, _, body := call(sam, http.MethodPost, "/board/deleteBoard", map[string]string{"board_uuid": ids[title]}); code != http.StatusOK {
			t.Fatalf("deleting %s: %d %s", title, code, body)
		}
		time.Sleep(20 * time.Millisecond)
	}

	t.Run("the trash lists what each person may bring back, newest deletion first", func(t *testing.T) {
		wantTrash(sam, "Sam", "Sam's secret", "Sam's plan", "Sam's old map")
		wantTrash(lee, "Lee (who edits Sam's plan)", "Lee's sketch")
		wantTrash(kim, "Kim")
		wantTrash(ada, "Ada (an admin: every board that isn't private)", "Sam's plan", "Lee's sketch", "Sam's old map")
		first, next := trash(sam, "?limit=2")
		if !reflect.DeepEqual(first, []string{"Sam's secret", "Sam's plan"}) || next == "" {
			t.Fatalf("the first page of two: %q, next %q", first, next)
		}
		rest, end := trash(sam, "?limit=2&cursor="+next)
		if !reflect.DeepEqual(rest, []string{"Sam's old map"}) || end != "" {
			t.Errorf("the page after %q: %q, next %q", next, rest, end)
		}
	})

	t.Run("one deleted board answers only to those who may bring it back", func(t *testing.T) {
		for _, c := range []struct {
			as    uuid.UUID
			who   string
			title string
			want  int
		}{
			{sam, "its owner", "Sam's plan", http.StatusOK},
			{ada, "an admin", "Sam's plan", http.StatusOK},
			{lee, "its editor", "Sam's plan", http.StatusNotFound},
			{kim, "a member", "Sam's plan", http.StatusNotFound},
			{ada, "an admin, for a private board", "Sam's secret", http.StatusNotFound},
		} {
			code, data, body := call(c.as, http.MethodGet, "/board/trash/"+ids[c.title], nil)
			if code != c.want {
				t.Errorf("GET /board/trash/{%s} as %s: %d %s, want %d", c.title, c.who, code, body, c.want)
				continue
			}
			var got item
			if code == http.StatusOK && (json.Unmarshal(data, &got) != nil || got.Title != c.title || got.Owner == nil || got.Owner.UUID != sam.String() || got.Mine != (c.as == sam)) {
				t.Errorf("GET /board/trash/{%s} as %s: %s", c.title, c.who, body)
			}
		}
	})

	t.Run("nobody but the owner or an admin restores, and a private board is its owner's alone", func(t *testing.T) {
		for _, c := range []struct {
			as    uuid.UUID
			who   string
			title string
		}{{lee, "its editor", "Sam's plan"}, {kim, "a member", "Sam's plan"}, {ada, "an admin", "Sam's secret"}} {
			if code, body := restore(c.as, c.title); code != http.StatusNotFound {
				t.Errorf("%s restoring %s: %d %s, want 404 as for a missing board", c.who, c.title, code, body)
			}
		}
		if code, _, _ := call(sam, http.MethodGet, "/board/getBoardInfo/"+ids["Sam's plan"], nil); code == http.StatusOK {
			t.Error("a refused restore brought Sam's plan back")
		}
	})

	t.Run("its owner brings it back: the board, its lists, search and its comments", func(t *testing.T) {
		code, body := restore(sam, "Sam's plan")
		if code != http.StatusOK || !strings.Contains(body, ids["Sam's plan"]) || !strings.Contains(body, "Sam's plan") {
			t.Fatalf("Sam restoring Sam's plan: %d %s", code, body)
		}
		for _, who := range []uuid.UUID{sam, lee} {
			if code, _, body := call(who, http.MethodGet, "/board/getBoardInfo/"+ids["Sam's plan"], nil); code != http.StatusOK {
				t.Errorf("opening the restored board: %d %s", code, body)
			}
		}
		if code, _, body := call(sam, http.MethodGet, "/board/getBoardList", nil); code != http.StatusOK || !strings.Contains(body, ids["Sam's plan"]) {
			t.Errorf("the board list after the restore: %d %s", code, body)
		}
		wantTrash(sam, "Sam after the restore", "Sam's secret", "Sam's old map")
		for _, c := range []struct{ index, field string }{{openSearchStruct.BOARD_INDEX, "_id"}, {openSearchStruct.COMMENT_INDEX, "comment_board_id"}} {
			if n := liveEntries(t, ctx, c.index, c.field, ids["Sam's plan"]); n != 1 {
				t.Errorf("%s: %d live entries for the restored board, want 1", c.index, n)
			}
		}
		rows := trailRows(t, db, ids["Sam's plan"], 1)
		if rows[0].action != "board.restored" || rows[0].category != "boards" || rows[0].actor != sam.String() {
			t.Errorf("the restore's audit row: %+v", rows[0])
		}
		if code, body := restore(sam, "Sam's plan"); code != http.StatusConflict {
			t.Errorf("restoring a board that's back already: %d %s, want 409", code, body)
		}
	})

	t.Run("a board a retention rule removed stays gone", func(t *testing.T) {
		if code, _, body := call(ada, http.MethodPut, "/admin/retention/rules/boards", map[string]any{"days": 30, "shown": 1}); code != http.StatusOK {
			t.Fatalf("setting the boards rule: %d %s", code, body)
		}
		if err := retentionBusiness.Sweep(ctx); err != nil {
			t.Fatal(err)
		}
		helpers.SettleBackground(30 * time.Second)
		for deadline := time.Now().Add(30 * time.Second); ; time.Sleep(200 * time.Millisecond) {
			_ = outboxBusiness.Drain(ctx)
			var waiting int
			if err := db.QueryRow(`SELECT count(*) FROM outbox WHERE topic = 'board.purged' AND (state IN ('held', 'ready') OR claimed_until > NOW())`).Scan(&waiting); err != nil {
				t.Fatal(err)
			}
			if waiting == 0 {
				break
			}
			if time.Now().After(deadline) {
				t.Fatalf("%d purge events still waiting", waiting)
			}
		}
		gone := ids["Sam's old map"]
		inGraph := func() int {
			var got struct {
				B []struct{} `json:"b"`
			}
			if err := json.Unmarshal(dg.Query(t, `{ b(func: eq(board_uuid, "`+gone+`")) { uid } }`), &got); err != nil {
				t.Fatal(err)
			}
			return len(got.B)
		}
		if n := inGraph(); n != 0 {
			t.Fatalf("the rule left %d nodes of Sam's old map", n)
		}
		for _, c := range []struct {
			as  uuid.UUID
			who string
		}{{sam, "its owner"}, {ada, "an admin"}} {
			if code, body := restore(c.as, "Sam's old map"); code != http.StatusNotFound {
				t.Errorf("%s restoring a board the rule removed: %d %s, want 404", c.who, code, body)
			}
			if code, _, body := call(c.as, http.MethodGet, "/board/trash/"+gone, nil); code != http.StatusNotFound {
				t.Errorf("%s asking for a board the rule removed: %d %s, want 404", c.who, code, body)
			}
		}
		if n := inGraph(); n != 0 {
			t.Errorf("restoring a board the rule removed wrote %d nodes back", n)
		}
		wantTrash(sam, "Sam after the rule", "Sam's secret")
		if code, _, body := call(sam, http.MethodGet, "/board/getBoardList", nil); code != http.StatusOK || strings.Contains(body, gone) {
			t.Errorf("the board list after the rule: %d %s", code, body)
		}
	})
}

// liveEntries is how many entries in index name id in field and aren't marked
// deleted, as global search keeps them (business/GlobalSearch notDeleted).
func liveEntries(t *testing.T, ctx context.Context, index, field, id string) int {
	t.Helper()
	_ = opensearchInit.RefreshIndices(ctx, index)
	q := `{"query":{"bool":{"should":[{"terms":{"` + field + `":["` + id + `"]}},{"terms":{"` + field + `.keyword":["` + id + `"]}}],"minimum_should_match":1,` +
		`"must_not":[{"exists":{"field":"deleted_date"}}]}}}`
	for deadline := time.Now().Add(10 * time.Second); ; time.Sleep(200 * time.Millisecond) {
		resp, err := opensearchInit.OpenSearchClient.Indices.Count(ctx, &opensearchapi.IndicesCountReq{Indices: []string{index}, Body: strings.NewReader(q)})
		if err != nil {
			t.Fatal(err)
		}
		if resp.Count > 0 || time.Now().After(deadline) {
			return resp.Count
		}
		_ = opensearchInit.RefreshIndices(ctx, index)
	}
}
