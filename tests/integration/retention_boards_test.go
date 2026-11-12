//go:build integration
// +build integration

package integration_test

// Boards under retention rules and holds (miro-44 head R), over the real
// routes against Postgres 12, Dgraph, OpenSearch and MinIO: a boards rule
// removes for good only boards in the trash longer than its days, never a
// live one and never one a held person made or edits; what it removes leaves
// with it (its comments, the links tasks held to it, its search entries and
// its comments', its drawing, its snapshots, its presenter notes and its
// polls with their votes); the
// sweep's one audit row
// counts the boards; and a hold's boards are searched and exported, deleted
// ones included.
//
// Run: go test -tags=integration ./tests/integration/ -run TestBoardsUnderARetentionRuleAndAHoldOverHTTP -v

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/minio/minio-go/v7"
	"github.com/opensearch-project/opensearch-go/v4/opensearchapi"

	outboxBusiness "github.com/akashc777/OneCamp/business/Outbox"
	retentionBusiness "github.com/akashc777/OneCamp/business/Retention"
	userBusiness "github.com/akashc777/OneCamp/business/User"
	"github.com/akashc777/OneCamp/helpers"
	"github.com/akashc777/OneCamp/initializers/minioInit"
	"github.com/akashc777/OneCamp/initializers/opensearchInit"
	"github.com/akashc777/OneCamp/initializers/postgresInit"
	openSearchStruct "github.com/akashc777/OneCamp/models/openSearch"
	userModels "github.com/akashc777/OneCamp/models/postgres/User"
	"github.com/akashc777/OneCamp/router"
	"github.com/akashc777/OneCamp/tests/integration"
)

func TestBoardsUnderARetentionRuleAndAHoldOverHTTP(t *testing.T) {
	t.Setenv("JWT_SECRET", "retention-boards-integration-secret")
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
	ada := person("ada@boards-retention.test", "Ada Admin")
	if err := userBusiness.CreateAdminUser(ctx, "ada@boards-retention.test"); err != nil {
		t.Fatal(err)
	}
	maya, lee, sam := person("maya@boards-retention.test", "Maya"), person("lee@boards-retention.test", "Lee"), person("sam@boards-retention.test", "Sam")
	uidOf := map[string]string{}
	var users struct {
		U []struct {
			UID  string `json:"uid"`
			UUID string `json:"user_uuid"`
		} `json:"u"`
	}
	raw := dg.Query(t, `{ u(func: eq(user_uuid, ["`+maya.String()+`", "`+lee.String()+`", "`+sam.String()+`"])) { uid user_uuid } }`)
	if err := json.Unmarshal(raw, &users); err != nil || len(users.U) != 3 {
		t.Fatalf("the people's nodes: %s (%v)", raw, err)
	}
	for _, u := range users.U {
		uidOf[u.UUID] = u.UID
	}

	now := time.Now().UTC()
	ago := func(days int) string { return now.AddDate(0, 0, -days).Format(time.RFC3339) }
	live := "0001-01-01T00:00:00Z"
	type boardSpec struct{ name, maker, editor, deleted string }
	specs := []boardSpec{
		{"Sam's old map", sam.String(), "", ago(40)},                 // the rule removes it
		{"Sam's recent sketch", sam.String(), "", ago(10)},           // in the trash for fewer days than the rule's
		{"Sam's live plan", sam.String(), "", live},                  // live, however old
		{"Maya's old flow", maya.String(), "", ago(400)},             // a held maker's
		{"Sam's shared draft", sam.String(), lee.String(), ago(400)}, // a held editor's
	}
	ids, tasks := map[string]string{}, map[string]string{}
	put := func(bucketKey string) {
		t.Helper()
		if _, err := minioInit.MinioClient.PutObject(ctx, helpers.UserUploadBucket(), bucketKey, bytes.NewReader([]byte("gz")), 2, minio.PutObjectOptions{}); err != nil {
			t.Fatal(err)
		}
	}
	upsert := func(index, id string, doc map[string]any) {
		t.Helper()
		// The whole entry as an update's doc, created when it isn't there (opensearchInit.UpsertDocument).
		b, _ := json.Marshal(map[string]any{"doc": doc})
		if err := opensearchInit.UpsertDocument(ctx, index, id, bytes.NewReader(b)); err != nil {
			t.Fatal(err)
		}
	}
	for _, s := range specs {
		id := uuid.NewString()
		ids[s.name] = id
		tasks[id] = uuid.NewString()
		node := map[string]any{"uid": "_:b", "dgraph.type": "Board", "board_uuid": id, "board_title": s.name, "board_private": false,
			"board_created_at": ago(900), "board_updated_at": ago(500), "board_deleted_at": s.deleted,
			"board_state_key": "boardState/" + id + ".gz", "board_created_by": map[string]any{"uid": uidOf[s.maker]}}
		if s.editor != "" {
			node["board_editing_users"] = []map[string]any{{"uid": uidOf[s.editor]}}
		}
		got := dg.Mutate(t, []map[string]any{
			node,
			{"uid": "_:c", "dgraph.type": "Comment", "comment_uuid": uuid.NewString(), "comment_text": "a note on " + s.name, "comment_board": map[string]any{"uid": "_:b"}},
			{"uid": "_:t", "dgraph.type": "Task", "task_uuid": tasks[id], "task_name": "links " + s.name, "linked_boards": []map[string]any{{"uid": "_:b"}}},
		})
		if got["b"] == "" {
			t.Fatalf("the board %q wasn't written", s.name)
		}
		put("boardState/" + id + ".gz")
		snapKey := "boardSnapshot/" + id + "/s1.gz"
		put(snapKey)
		if _, err := db.ExecContext(ctx, `INSERT INTO board_snapshots (board_uuid, object_key, element_count, state_bytes, reason, contributor_uuids) VALUES ($1, $2, 1, 2, 'interval', '{}')`, id, snapKey); err != nil {
			t.Fatal(err)
		}
		// A presenter note live and one deleted (soft): the purge takes both with the board.
		for _, deleted := range []string{"NULL", "now()"} {
			if _, err := db.ExecContext(ctx, `INSERT INTO board_frame_notes (id, board_uuid, frame_id, body, created_by, updated_by, deleted_at)
				VALUES ($1, $2, 'intro', 'Open with the numbers', $3, $3, `+deleted+`)`, uuid.NewString(), id, s.maker); err != nil {
				t.Fatal(err)
			}
		}
		// A poll on one of its frames (miro-28 to_win 4) with a vote: the purge takes both with the board.
		pollID := uuid.NewString()
		if _, err := db.ExecContext(ctx, `INSERT INTO polls (id, channel_id, question, options, created_by, board_uuid, frame_id)
			VALUES ($1, '00000000-0000-0000-0000-000000000000', 'Ship it?', '[{"id":"a","text":"Yes"}]', $2, $3, 'intro')`, pollID, s.maker, id); err != nil {
			t.Fatal(err)
		}
		if _, err := db.ExecContext(ctx, `INSERT INTO poll_votes (poll_id, option_id, user_id) VALUES ($1, 'a', $2)`, pollID, s.maker); err != nil {
			t.Fatal(err)
		}
		upsert(openSearchStruct.BOARD_INDEX, id, map[string]any{"board_uuid": id, "board_title": s.name})
		upsert(openSearchStruct.COMMENT_INDEX, uuid.NewString(), map[string]any{"comment_board_id": id, "comment_text": "a note on " + s.name})
	}
	// No refresh here, on purpose: the purge must take entries no refresh has shown yet. An index nothing
	// searched for 30 s goes search-idle and skips its 1 s refresh, and a delete-by-query sees only what the
	// last refresh showed (b204: the comment index, which nothing here searches, kept its entry while the
	// board index, which the hold's search reads, lost its own).

	routes := router.Routes()
	call := func(method, path string, body any) (int, map[string]any, *httptest.ResponseRecorder) {
		t.Helper()
		token, err := helpers.SignSessionToken(ada.String(), helpers.TokenTypeAccess, time.Now().Add(10*time.Minute).Unix())
		if err != nil {
			t.Fatal(err)
		}
		var raw []byte
		if body != nil {
			raw, _ = json.Marshal(body)
		}
		req := httptest.NewRequest(method, path, bytes.NewReader(raw))
		req.AddCookie(&http.Cookie{Name: "Authorization", Value: token})
		req.AddCookie(&http.Cookie{Name: "DeviceId", Value: "device-1"})
		rec := httptest.NewRecorder()
		routes.ServeHTTP(rec, req)
		out := map[string]any{}
		_ = json.Unmarshal(rec.Body.Bytes(), &out)
		return rec.Code, out, rec
	}
	preview := func() float64 {
		t.Helper()
		code, out, _ := call(http.MethodGet, "/admin/retention/rules/boards/preview?days=30", nil)
		data, _ := out["data"].(map[string]any)
		n, ok := data["would_remove"].(float64)
		if code != http.StatusOK || !ok {
			t.Fatalf("preview: %d %v", code, out)
		}
		return n
	}
	if n := preview(); n != 3 {
		t.Fatalf("before any hold a 30-day boards rule would remove %v, want 3 (every board in the trash 30+ days)", n)
	}
	code, out, _ := call(http.MethodPost, "/admin/retention/holds", map[string]any{"name": "Design dispute", "add": []string{maya.String(), lee.String()}})
	data, _ := out["data"].(map[string]any)
	holdID, _ := data["id"].(string)
	if code != http.StatusCreated || holdID == "" {
		t.Fatalf("placing the hold: %d %v", code, out)
	}
	if n := preview(); n != 1 {
		t.Fatalf("with Maya and Lee held a 30-day rule would remove %v, want 1 (Sam's old map)", n)
	}

	// A hold's boards: those its people made or edit, deleted ones included.
	code, out, _ = call(http.MethodGet, "/admin/retention/holds/"+holdID+"/items/boards", nil)
	items, _ := out["data"].([]any)
	if code != http.StatusOK || len(items) != 2 {
		t.Fatalf("searching the hold's boards: %d %v, want Maya's and the one Lee edits", code, out)
	}
	for _, it := range items {
		m := it.(map[string]any)
		if m["id"] != ids["Maya's old flow"] && m["id"] != ids["Sam's shared draft"] || m["deleted_at"] == nil {
			t.Errorf("a hold's board item %v: want Maya's or Lee's, with its trash time", m)
		}
	}
	code, _, rec := call(http.MethodGet, "/admin/retention/holds/"+holdID+"/export/boards", nil)
	var file struct {
		Boards []struct {
			ID string `json:"id"`
		} `json:"boards"`
	}
	if code != http.StatusOK || json.Unmarshal(rec.Body.Bytes(), &file) != nil || len(file.Boards) != 2 {
		t.Fatalf("the hold's boards export: %d %s", code, rec.Body.String())
	}

	if code, out, _ := call(http.MethodPut, "/admin/retention/rules/boards", map[string]any{"days": 30, "shown": 1}); code != http.StatusOK {
		t.Fatalf("setting the boards rule: %d %v", code, out)
	}
	if err := retentionBusiness.Sweep(ctx); err != nil {
		t.Fatal(err)
	}
	helpers.SettleBackground(30 * time.Second)
	for deadline := time.Now().Add(30 * time.Second); ; {
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
		time.Sleep(200 * time.Millisecond)
	}

	inGraph := func(id string) (board, comments, links int) {
		t.Helper()
		var got struct {
			B []struct {
				Notes []struct{} `json:"notes"`
			} `json:"b"`
			L []struct {
				N int `json:"n"`
			} `json:"l"`
		}
		raw := dg.Query(t, `{ b(func: eq(board_uuid, "`+id+`")) { notes: ~comment_board { uid } }
			l(func: eq(task_uuid, "`+tasks[id]+`")) { n: count(linked_boards) } }`)
		if err := json.Unmarshal(raw, &got); err != nil {
			t.Fatal(err)
		}
		for _, b := range got.B {
			board++
			comments += len(b.Notes)
		}
		for _, l := range got.L {
			links += l.N
		}
		return
	}
	entries := func(index, field, id string) int {
		t.Helper()
		_ = opensearchInit.RefreshIndices(ctx, index)
		q := `{"query":{"bool":{"should":[{"terms":{"` + field + `":["` + id + `"]}},{"terms":{"` + field + `.keyword":["` + id + `"]}}],"minimum_should_match":1}}}`
		resp, err := opensearchInit.OpenSearchClient.Indices.Count(ctx, &opensearchapi.IndicesCountReq{Indices: []string{index}, Body: strings.NewReader(q)})
		if err != nil {
			t.Fatal(err)
		}
		return resp.Count
	}
	stored := func(key string) bool {
		_, err := minioInit.MinioClient.StatObject(ctx, helpers.UserUploadBucket(), key, minio.StatObjectOptions{})
		return err == nil
	}
	snapshots := func(id string) int {
		var n int
		if err := db.QueryRow(`SELECT count(*) FROM board_snapshots WHERE board_uuid = $1`, id).Scan(&n); err != nil {
			t.Fatal(err)
		}
		return n
	}
	notes := func(id string) int {
		var n int
		if err := db.QueryRow(`SELECT count(*) FROM board_frame_notes WHERE board_uuid = $1`, id).Scan(&n); err != nil {
			t.Fatal(err)
		}
		return n
	}
	polls := func(id string) (n, votes int) {
		if err := db.QueryRow(`SELECT count(*), (SELECT count(*) FROM poll_votes v JOIN polls p ON p.id = v.poll_id WHERE p.board_uuid = $1)
			FROM polls WHERE board_uuid = $1`, id).Scan(&n, &votes); err != nil {
			t.Fatal(err)
		}
		return n, votes
	}
	for _, s := range specs {
		id := ids[s.name]
		board, comments, links := inGraph(id)
		gone := s.name == "Sam's old map"
		want := 1
		if gone {
			want = 0
		}
		if board != want || comments != want || links != want {
			t.Errorf("%s after the sweep: board %d, comments %d, task links %d; want %d each", s.name, board, comments, links, want)
		}
		if n := entries(openSearchStruct.BOARD_INDEX, "_id", id); n != want {
			t.Errorf("%s after the sweep: %d board entries, want %d", s.name, n, want)
		}
		if n := entries(openSearchStruct.COMMENT_INDEX, "comment_board_id", id); n != want {
			t.Errorf("%s after the sweep: %d comment entries, want %d", s.name, n, want)
		}
		if stored("boardState/"+id+".gz") != !gone || stored("boardSnapshot/"+id+"/s1.gz") != !gone || snapshots(id) != want {
			t.Errorf("%s after the sweep: drawing stored %v, snapshot stored %v, %d snapshot rows; want each kept only for a kept board",
				s.name, stored("boardState/"+id+".gz"), stored("boardSnapshot/"+id+"/s1.gz"), snapshots(id))
		}
		if n := notes(id); n != 2*want {
			t.Errorf("%s after the sweep: %d presenter note rows, want %d (a purged board takes its notes, live and deleted)", s.name, n, 2*want)
		}
		if n, votes := polls(id); n != want || votes != want {
			t.Errorf("%s after the sweep: %d poll rows and %d votes, want %d each (a purged board takes its polls and their votes)", s.name, n, votes, want)
		}
	}
	var meta, summary string
	if err := db.QueryRow(`SELECT metadata::text, summary FROM admin_audit_log WHERE action = 'retention.swept'`).Scan(&meta, &summary); err != nil {
		t.Fatalf("the sweep's one row: %v", err)
	}
	if !strings.Contains(meta, `"boards": 1`) || !strings.Contains(summary, "1 boards") {
		t.Errorf("the sweep's row says %s / %q, want 1 board removed", meta, summary)
	}
}
