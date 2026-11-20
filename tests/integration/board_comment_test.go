//go:build integration
// +build integration

package integration_test

// Can comment on a board (miro-32 head B), on the routes as router mounts
// them, against real Postgres and Dgraph: the owner, a co-owner and an
// editor (while the switch is on) make commenters; a commenter adds a pin,
// replies and deletes their own message through POST /board/comment, always
// as themselves, and the server mirrors it; they can't share, invite,
// change, delete, nor delete anyone else's message, nor write the web's
// mirror; a viewer can't comment; someone the board isn't shared with finds
// no board. The live op is a stand-in that keeps pins by author as the
// collaboration service does (live/comment.test.js proves the op itself, and
// that a commenter's socket stays read-only).
//
// Run: PKGS=./tests/integration/ (go test -tags=integration ./tests/integration/ -run TestABoardsCommentersCommentOnTheServer -v)

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
	"github.com/akashc777/OneCamp/helpers"
	"github.com/akashc777/OneCamp/initializers/postgresInit"
	customMiddleware "github.com/akashc777/OneCamp/middleware"
	"github.com/akashc777/OneCamp/services/collab"
	"github.com/akashc777/OneCamp/tests/integration"
)

func TestABoardsCommentersCommentOnTheServer(t *testing.T) {
	t.Setenv("JWT_SECRET", "board-comment-secret")
	t.Setenv("ALLOWED_USERS", "")
	env := integration.SetupEnv(t)
	ctx := context.Background()
	if err := postgresInit.ConnectPostgres(ctx, env.DSN); err != nil {
		t.Fatalf("wire the project pool at the test database: %v", err)
	}
	integration.SetupRedis(t)
	integration.StubMqttClient(t)
	dg := integration.SetupDgraph(t)

	owner, co, ed, cm, cm2, vi, out := uuid.New(), uuid.New(), uuid.New(), uuid.New(), uuid.New(), uuid.New(), uuid.New()
	names := map[string]uuid.UUID{"owner": owner, "co": co, "ed": ed, "cm": cm, "cm2": cm2, "vi": vi, "out": out}
	var nodes []map[string]any
	for name, id := range names {
		if _, err := env.PG.Exec(`INSERT INTO users (id, email_id, username, display_name, created_at, updated_at)
			VALUES ($1, $2, $3, $4, NOW(), NOW())`, id, name+"@comment.test", name+"-comment", name); err != nil {
			t.Fatalf("seed %s: %v", name, err)
		}
		nodes = append(nodes, map[string]any{"uid": "_:" + name, "dgraph.type": "User", "user_uuid": id.String(), "user_name": name})
	}
	people := dg.Mutate(t, nodes)
	// The commenter's name on the board (users.display_name) is not their graph name.
	if _, err := env.PG.Exec(`UPDATE users SET display_name = 'Cam Commenter' WHERE id = $1`, cm); err != nil {
		t.Fatalf("name the commenter: %v", err)
	}
	board := uuid.NewString()
	boardUID := dg.Mutate(t, map[string]any{"uid": "_:b", "dgraph.type": "Board", "board_uuid": board, "board_title": "Launch plan",
		"board_private": true, "board_deleted_at": "0001-01-01T00:00:00Z", "board_created_by": map[string]any{"uid": people["owner"]}})["b"]

	// The live op's stand-in: pins by id, each message with its author; a
	// delete of someone else's message is the op's 403.
	type msg struct{ id, by string }
	pins := map[string][]msg{}
	var asked []string
	saved := collab.CommentBoard
	t.Cleanup(func() { collab.CommentBoard = saved })
	collab.CommentBoard = func(_ context.Context, b string, c collab.CommentChange, by, name string) (collab.CommentAnswer, error) {
		asked = append(asked, c.Action+" by "+by)
		id := c.CommentID
		switch c.Action {
		case "add":
			id = uuid.NewString()
			pins[id] = []msg{{uuid.NewString(), by}}
		case "reply":
			if pins[id] == nil {
				return collab.CommentAnswer{}, &collab.OpError{Op: "comment", Status: 404, Msg: "no such comment"}
			}
			pins[id] = append(pins[id], msg{uuid.NewString(), by})
		case "delete":
			kept := []msg{}
			for _, m := range pins[id] {
				if m.id == c.MessageID && m.by != by {
					return collab.CommentAnswer{}, &collab.OpError{Op: "comment", Status: 403, Msg: "not yours"}
				}
				if m.id != c.MessageID {
					kept = append(kept, m)
				}
			}
			if pins[id] = kept; len(kept) == 0 {
				delete(pins, id)
				return collab.CommentAnswer{Removed: true}, nil
			}
		}
		out := &collab.BoardComment{ID: id, AuthorID: pins[id][0].by}
		for _, m := range pins[id] {
			out.Messages = append(out.Messages, collab.BoardCommentMessage{ID: m.id, AuthorID: m.by, AuthorName: name, Body: c.Text})
		}
		return collab.CommentAnswer{Comment: out}, nil
	}

	r := chi.NewRouter()
	r.Use(customMiddleware.VerifyAuth)
	r.Post("/board/comment", boardController.CommentOnBoard)
	r.Post("/boardColab/authorize", boardController.BoardCollabAuthorize)
	r.Post("/board/commentMention", boardController.BoardCommentMention)
	r.Post("/board/commentDelete", boardController.BoardCommentDelete)
	r.Post("/board/updateBoardPermissions", boardController.UpdateBoardPermissions)
	r.Post("/board/updateBoard", boardController.UpdateBoard)
	r.Post("/board/deleteBoard", boardController.DeleteBoard)
	r.Get("/board/getBoardInfo/{board_uuid}", boardController.GetBoardInfo)
	serve := func(as uuid.UUID, req *http.Request) (int, string) {
		token, err := helpers.SignSessionToken(as.String(), helpers.TokenTypeAccess, time.Now().Add(5*time.Minute).Unix())
		if err != nil {
			t.Fatal(err)
		}
		req.AddCookie(&http.Cookie{Name: "Authorization", Value: token})
		req.AddCookie(&http.Cookie{Name: "DeviceId", Value: "device-1"})
		rec := httptest.NewRecorder()
		r.ServeHTTP(rec, req)
		return rec.Code, rec.Body.String()
	}
	call := func(as uuid.UUID, path string, body map[string]any) (int, string) {
		t.Helper()
		body["board_uuid"] = board
		raw, _ := json.Marshal(body)
		return serve(as, httptest.NewRequest(http.MethodPost, path, bytes.NewReader(raw)))
	}
	comment := func(as uuid.UUID, body map[string]any) (int, string, map[string]any) {
		t.Helper()
		code, raw := call(as, "/board/comment", body)
		var got struct {
			Data struct {
				Comment map[string]any `json:"comment"`
			} `json:"data"`
		}
		_ = json.Unmarshal([]byte(raw), &got)
		return code, raw, got.Data.Comment
	}
	share := func(as uuid.UUID, body map[string]any) int {
		code, _ := call(as, "/board/updateBoardPermissions", body)
		return code
	}

	t.Run("the owner, a co-owner and an editor make commenters; a viewer can't", func(t *testing.T) {
		for who, body := range map[string]map[string]any{"co-owner": {"add_co_owners": []string{co.String()}}, "editor": {"add_editors": []string{ed.String()}}, "viewer": {"add_viewers": []string{vi.String()}}} {
			if code := share(owner, body); code != http.StatusOK {
				t.Fatalf("the owner adding the %s answered %d", who, code)
			}
		}
		if code := share(co, map[string]any{"add_commenters": []string{cm.String()}}); code != http.StatusOK {
			t.Errorf("the co-owner making a commenter answered %d, want 200", code)
		}
		if code := share(ed, map[string]any{"add_commenters": []string{cm2.String()}}); code != http.StatusOK {
			t.Errorf("the editor (switch on) making a commenter answered %d, want 200", code)
		}
		if code := share(vi, map[string]any{"add_commenters": []string{out.String()}}); code != http.StatusForbidden {
			t.Errorf("the viewer making a commenter answered %d, want 403", code)
		}
		if got := holders(t, dg, boardUID, "board_commenting_users"); len(got) != 2 {
			t.Fatalf("commenters %v, want cm and cm2", got)
		}
		if code := share(owner, map[string]any{"remove_commenters": []string{cm2.String()}}); code != http.StatusOK {
			t.Errorf("the owner removing a commenter answered %d", code)
		}
	})

	var pin, edPin string
	t.Run("a commenter adds a pin and replies, always as themselves, and the server mirrors it", func(t *testing.T) {
		code, raw, c := comment(cm, map[string]any{"action": "add", "x": 120.5, "y": -40, "comment_text": "Ship the beta first",
			"authorId": owner.String(), "author_id": owner.String(), "by": map[string]any{"user_uuid": owner.String()}})
		if code != http.StatusOK || c == nil {
			t.Fatalf("the commenter's pin answered %d %s, want 200 with the pin", code, raw)
		}
		pin = c["id"].(string)
		if c["authorId"] != cm.String() || len(asked) != 1 || asked[0] != "add by "+cm.String() {
			t.Errorf("the pin's author %v, the op asked %v: want the commenter whatever the body said", c["authorId"], asked)
		}
		// Named as the live canvas names them (authorize's name, which the
		// collaboration service stamps on an editor's pins), so it reads as an
		// editor's pin does; the canvas stays read-only, the comment box offered.
		var auth struct {
			CanEdit    bool   `json:"canEdit"`
			CanComment bool   `json:"canComment"`
			Name       string `json:"name"`
		}
		authCode, authRaw := call(cm, "/boardColab/authorize", map[string]any{})
		_ = json.Unmarshal([]byte(authRaw), &auth)
		msgs, _ := c["messages"].([]any)
		first, _ := func() (map[string]any, bool) {
			if len(msgs) == 0 {
				return nil, false
			}
			m, ok := msgs[0].(map[string]any)
			return m, ok
		}()
		if authCode != http.StatusOK || auth.CanEdit || !auth.CanComment || auth.Name != "Cam Commenter" || first["authorName"] != auth.Name {
			t.Errorf("authorize answered %d %s; the pin's message names %v: want canEdit false, canComment true, the pin named as authorize names them", authCode, authRaw, first["authorName"])
		}
		var viewer struct {
			CanEdit    bool `json:"canEdit"`
			CanComment bool `json:"canComment"`
		}
		if code, raw := call(vi, "/boardColab/authorize", map[string]any{}); code != http.StatusOK || json.Unmarshal([]byte(raw), &viewer) != nil || viewer.CanEdit || viewer.CanComment {
			t.Errorf("the viewer's authorize answered %d %s, want 200 without canEdit or canComment", code, raw)
		}
		if code, raw, _ := comment(cm, map[string]any{"action": "reply", "board_comment_id": pin, "comment_text": "and then the docs"}); code != http.StatusOK {
			t.Errorf("the commenter's reply answered %d %s", code, raw)
		}
		var got struct {
			B []struct {
				C []struct {
					At string `json:"comment_deleted_at"`
					By struct {
						UUID string `json:"user_uuid"`
					} `json:"comment_by"`
				} `json:"~comment_board"`
			} `json:"b"`
		}
		_ = json.Unmarshal(dg.Query(t, `{ b(func: uid(`+boardUID+`)) { ~comment_board { comment_deleted_at comment_by { user_uuid } } } }`), &got)
		if len(got.B) != 1 || len(got.B[0].C) != 1 || got.B[0].C[0].By.UUID != cm.String() || got.B[0].C[0].At > "1970" {
			t.Errorf("the mirror %+v, want one live comment by the commenter", got)
		}
		if code, _, c := comment(ed, map[string]any{"action": "add", "x": 1, "y": 1, "comment_text": "Mine"}); code != http.StatusOK || c == nil {
			t.Fatalf("the editor's pin answered %d", code)
		} else {
			edPin = c["id"].(string)
		}
	})

	t.Run("a commenter deletes their own message, never another's", func(t *testing.T) {
		msgs := pins[edPin]
		if len(msgs) == 0 || pin == "" {
			t.Fatalf("no pins to delete from (the editor's %q, the commenter's %q): the adds above failed", edPin, pin)
		}
		if code, raw, _ := comment(cm, map[string]any{"action": "delete", "board_comment_id": edPin, "message_id": msgs[0].id}); code != http.StatusForbidden || !strings.Contains(raw, "only your own") {
			t.Errorf("the commenter deleting the editor's message answered %d %s, want 403 with words", code, raw)
		}
		if len(pins[edPin]) != 1 {
			t.Errorf("the editor's pin after the refusal: %v", pins[edPin])
		}
		for _, m := range pins[pin] {
			if code, raw, _ := comment(cm, map[string]any{"action": "delete", "board_comment_id": pin, "message_id": m.id}); code != http.StatusOK {
				t.Errorf("the commenter deleting their own message answered %d %s", code, raw)
			}
		}
		if _, there := pins[pin]; there {
			t.Errorf("the pin outlived its last message")
		}
		var got struct {
			B []struct {
				C []struct {
					At string `json:"comment_deleted_at"`
				} `json:"~comment_board"`
			} `json:"b"`
		}
		_ = json.Unmarshal(dg.Query(t, `{ b(func: uid(`+boardUID+`)) { ~comment_board { comment_deleted_at } } }`), &got)
		soft := 0
		for _, b := range got.B {
			for _, c := range b.C {
				if c.At > "1970" {
					soft++
				}
			}
		}
		if soft != 1 {
			t.Errorf("mirrors soft-deleted %d, want the commenter's one (kept, stamped)", soft)
		}
	})

	t.Run("a commenter does nothing else; a viewer can't comment; someone off the board finds none", func(t *testing.T) {
		for name, try := range map[string]func() int{
			"invite a viewer": func() int { return share(cm, map[string]any{"add_viewers": []string{out.String()}}) },
			"rename the board": func() int {
				c, _ := call(cm, "/board/updateBoard", map[string]any{"board_title": "Mine now"})
				return c
			},
			"delete the board": func() int { c, _ := call(cm, "/board/deleteBoard", map[string]any{}); return c },
			"rewrite the mirror": func() int {
				c, _ := call(cm, "/board/commentMention", map[string]any{"board_comment_id": edPin, "comment_text": "rewritten"})
				return c
			},
			"drop another's mirror": func() int {
				c, _ := call(cm, "/board/commentDelete", map[string]any{"board_comment_id": edPin})
				return c
			},
		} {
			if code := try(); code != http.StatusForbidden {
				t.Errorf("the commenter trying to %s answered %d, want 403", name, code)
			}
		}
		before := len(asked)
		if code, raw, _ := comment(vi, map[string]any{"action": "add", "x": 1, "y": 1, "comment_text": "hi"}); code != http.StatusForbidden || !strings.Contains(raw, "commenters can comment") {
			t.Errorf("the viewer commenting answered %d %s, want 403 with words", code, raw)
		}
		for _, who := range []uuid.UUID{out, cm2} {
			if code, _, _ := comment(who, map[string]any{"action": "add", "x": 1, "y": 1, "comment_text": "hi"}); code != http.StatusNotFound {
				t.Errorf("%s (not on the board) commenting answered %d, want 404", who, code)
			}
		}
		if len(asked) != before {
			t.Errorf("refused comments reached the board: %v", asked[before:])
		}
		for who, want := range map[uuid.UUID]bool{cm: true, ed: true, vi: false} {
			_, raw := serve(who, httptest.NewRequest(http.MethodGet, "/board/getBoardInfo/"+board, nil))
			var info struct {
				Data struct {
					CanComment bool `json:"board_can_comment"`
				} `json:"data"`
			}
			if err := json.Unmarshal([]byte(raw), &info); err != nil || info.Data.CanComment != want {
				t.Errorf("getBoardInfo for %s offers the comment box %v (%v), want %v", who, info.Data.CanComment, err, want)
			}
		}
	})

	t.Run("a comment mentions at most 50 people, each by their id", func(t *testing.T) {
		many := make([]string, 51)
		for i := range many {
			many[i] = uuid.NewString()
		}
		before := len(asked)
		for name, ids := range map[string][]string{"51 people": many, "an id not in its canonical form": {"{" + ed.String() + "}"}} {
			if code, raw, _ := comment(cm, map[string]any{"action": "add", "x": 1, "y": 1, "comment_text": "hi", "mentioned_user_uuids": ids}); code != http.StatusBadRequest || !strings.Contains(raw, "Mention at most 50 people, each by their id.") {
				t.Errorf("mentioning %s answered %d %s, want 400 with words", name, code, raw)
			}
		}
		if len(asked) != before {
			t.Errorf("refused mentions reached the board: %v", asked[before:])
		}
		if code, raw, c := comment(cm, map[string]any{"action": "add", "x": 2, "y": 2, "comment_text": "Over to you", "mentioned_user_uuids": []string{ed.String()}}); code != http.StatusOK || c == nil || len(asked) != before+1 {
			t.Errorf("one mention on an add answered %d %s (the op asked %v), want 200 with the pin", code, raw, asked[before:])
		}
	})
}
