//go:build integration
// +build integration

package integration_test

// Presenter notes (miro-28 2a), through the real router. Who may present a
// board (its edit right: the web's Present and a call's present_board) reads
// its frames' notes; a member who may only view it is refused, and one it is
// hidden from is told it isn't there. Writing needs the edit right, and each
// write names the version it read: a stale one is refused with the note as it
// now is, so two editors never overwrite each other. A share-link guest, a
// call's guest and a caller with no token never reach the notes. Notes show
// only for frames on the board: a frame deleted on the canvas hides its note,
// and so does one gone from the document, while the note stays live (a read
// never writes), so a restored document brings it back. An empty note is the
// note's soft delete, on the version read: a stale one is refused.
// The purge of a board takes its notes: retention_boards_test.go.
//
// Run: go test -tags=integration ./tests/integration/ -run TestPresenterNotesReachOnlyWhoMayPresent -v

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/akashc777/OneCamp/business/Board/scene"
	guestBusiness "github.com/akashc777/OneCamp/business/Guest"
	settingsBusiness "github.com/akashc777/OneCamp/business/Settings"
	"github.com/akashc777/OneCamp/helpers"
	"github.com/akashc777/OneCamp/models/pgexec"
	guestModel "github.com/akashc777/OneCamp/models/postgres/Guest"
	"github.com/akashc777/OneCamp/router"
)

func TestPresenterNotesReachOnlyWhoMayPresent(t *testing.T) {
	env, dg := liveSessionStores(t)
	t.Setenv("CSRF_DISABLED", "true")
	ctx := context.Background()
	people := livePeople(t, env, "owner", "editor", "viewer", "stranger")

	// The board's stored document: a frame on the canvas, and one deleted there.
	intro := scene.Frame("intro", "Intro", 0, 0, 400, 300, 1)
	erased := scene.Frame("erased", "Old idea", 500, 0, 400, 300, 1)
	erased["isDeleted"] = true
	state, _, err := scene.State(5, []scene.Element{intro, erased}, nil, nil)
	if err != nil {
		t.Fatal(err)
	}
	board := uuid.NewString()
	uids := dg.Mutate(t, []map[string]any{
		{"uid": "_:owner", "dgraph.type": "User", "user_uuid": people["owner"].String(), "user_name": "owner"},
		{"uid": "_:editor", "dgraph.type": "User", "user_uuid": people["editor"].String(), "user_name": "editor"},
		{"uid": "_:viewer", "dgraph.type": "User", "user_uuid": people["viewer"].String(), "user_name": "viewer"},
		{"uid": "_:stranger", "dgraph.type": "User", "user_uuid": people["stranger"].String(), "user_name": "stranger"},
		{"uid": "_:b", "dgraph.type": "Board", "board_uuid": board, "board_title": "Quarterly review", "board_private": true,
			"board_deleted_at": "0001-01-01T00:00:00Z", "board_created_by": map[string]any{"uid": "_:owner"}, "board_state": state,
			"board_editing_users": []map[string]any{{"uid": "_:editor"}}, "board_reading_users": []map[string]any{{"uid": "_:viewer"}}},
	})
	if err := settingsBusiness.SetGuestAccessEnabled(true); err != nil {
		t.Fatal(err)
	}

	session := func(who string) string {
		t.Helper()
		token, err := helpers.SignSessionToken(people[who].String(), helpers.TokenTypeAccess, time.Now().Add(10*time.Minute).Unix())
		if err != nil {
			t.Fatal(err)
		}
		return token
	}
	routes := router.Routes()
	type note struct {
		FrameID string `json:"frame_id"`
		Body    string `json:"body"`
		Version int    `json:"version"`
	}
	// A token as the web sends a member's session (authcookie: the
	// Authorization and DeviceId cookies), or as a Bearer header.
	call := func(token string, cookie bool, method, path, body string) (int, string) {
		t.Helper()
		req := httptest.NewRequest(method, path, strings.NewReader(body))
		req.Header.Set("Content-Type", "application/json")
		switch {
		case token == "":
		case cookie:
			req.AddCookie(&http.Cookie{Name: "Authorization", Value: token})
			req.AddCookie(&http.Cookie{Name: "DeviceId", Value: "device-1"})
		default:
			req.Header.Set("Authorization", "Bearer "+token)
		}
		rec := httptest.NewRecorder()
		routes.ServeHTTP(rec, req)
		return rec.Code, rec.Body.String()
	}
	read := func(who string) (int, []note) {
		t.Helper()
		code, body := call(session(who), true, http.MethodGet, "/board/getFrameNotes/"+board, "")
		var got struct {
			Data []note `json:"data"`
		}
		if code == http.StatusOK {
			if err := json.Unmarshal([]byte(body), &got); err != nil {
				t.Fatalf("%s's notes: %v in %s", who, err, body)
			}
		}
		return code, got.Data
	}
	type answer struct {
		Code string `json:"code"`
		Data *note  `json:"data"`
	}
	save := func(who, frame, body string, version int) (int, answer) {
		t.Helper()
		in, _ := json.Marshal(map[string]any{"board_uuid": board, "frame_id": frame, "body": body, "version": version})
		code, out := call(session(who), true, http.MethodPost, "/board/saveFrameNote", string(in))
		var a answer
		_ = json.Unmarshal([]byte(out), &a)
		return code, a
	}

	// The owner writes the intro's note; the editor, who read none, can't
	// overwrite it: the 409 carries the owner's note.
	if code, a := save("owner", "intro", "Open with the numbers", 0); code != http.StatusOK || a.Data == nil || a.Data.Version != 1 {
		t.Fatalf("the owner's first note answers %d %+v", code, a)
	}
	if code, a := save("editor", "intro", "Start with the demo", 0); code != http.StatusConflict || a.Code != "stale_note" || a.Data == nil ||
		a.Data.Body != "Open with the numbers" || a.Data.Version != 1 {
		t.Fatalf("a write from no version over a note answers %d %+v, want 409 stale_note with the owner's note", code, a)
	}
	if code, a := save("editor", "intro", "Open with the numbers, then the demo", 1); code != http.StatusOK || a.Data.Version != 2 {
		t.Fatalf("the editor's write on version 1 answers %d %+v", code, a)
	}
	if code, a := save("owner", "intro", "Open with the numbers!", 1); code != http.StatusConflict || a.Data == nil || a.Data.Version != 2 ||
		a.Data.Body != "Open with the numbers, then the demo" {
		t.Fatalf("the owner's stale write answers %d %+v, want 409 with version 2", code, a)
	}
	if code, _ := save("owner", "intro", strings.Repeat("x", 10_001), 2); code != http.StatusBadRequest {
		t.Fatalf("a note past 10,000 characters answers %d", code)
	}

	// Who may not present: a viewer is refused both ways, a member the
	// private board is hidden from is told it isn't there.
	if code, _ := read("viewer"); code != http.StatusForbidden {
		t.Errorf("a viewer reading the notes answers %d, want 403", code)
	}
	if code, _ := save("viewer", "intro", "Mine now", 2); code != http.StatusForbidden {
		t.Errorf("a viewer writing a note answers %d, want 403", code)
	}
	if code, _ := read("stranger"); code != http.StatusNotFound {
		t.Errorf("a member the board is hidden from answers %d, want 404", code)
	}

	// Guests and the public: a share-link guest's collab token, a call
	// guest's board token, and no token never pass the member gate, sent in
	// a member session's cookies or as a Bearer header.
	grant, err := guestBusiness.CreateResourceGrant(ctx, people["owner"], "board", board, "view", time.Hour, false)
	if err != nil {
		t.Fatal(err)
	}
	g, err := guestModel.GetByID(ctx, grant.GrantID)
	if err != nil {
		t.Fatal(err)
	}
	linkGuest, err := guestBusiness.IssueGuestCollabToken(ctx, g, "Ana")
	if err != nil {
		t.Fatal(err)
	}
	callGuest, err := guestBusiness.IssueCallBoardToken(ctx, "guest-"+uuid.NewString()+"-k3x9", "Guest: Jane", uuid.NewString(), board)
	if err != nil {
		t.Fatal(err)
	}
	for name, token := range map[string]string{"a share-link guest": linkGuest, "a call's guest": callGuest, "no token": ""} {
		for _, cookie := range []bool{true, false} {
			if code, body := call(token, cookie, http.MethodGet, "/board/getFrameNotes/"+board, ""); code != http.StatusUnauthorized || strings.Contains(body, "Open with") {
				t.Errorf("%s reading the notes (cookie %v) answers %d %s, want 401", name, cookie, code, body)
			}
			if code, _ := call(token, cookie, http.MethodPost, "/board/saveFrameNote", `{"board_uuid":"`+board+`","frame_id":"intro","body":"x","version":2}`); code != http.StatusUnauthorized {
				t.Errorf("%s writing a note (cookie %v) answers %d, want 401", name, cookie, code)
			}
		}
	}

	// Frames: a note on a frame deleted on the canvas is hidden and kept; one
	// on a frame newer than the stored document shows; one whose frame has
	// been gone from the document past the store's delay is hidden and kept.
	for _, frame := range []string{"erased", "just-drawn", "pruned"} {
		if code, a := save("owner", frame, "Note for "+frame, 0); code != http.StatusOK {
			t.Fatalf("a note on %s answers %d %+v", frame, code, a)
		}
	}
	if _, err := pgexec.Exec(ctx, `UPDATE board_frame_notes SET updated_at = now() - interval '20 minutes' WHERE board_uuid = $1 AND frame_id = 'pruned'`, board); err != nil {
		t.Fatal(err)
	}
	shown := func(who string) string {
		t.Helper()
		code, notes := read(who)
		var out []string
		for _, n := range notes {
			out = append(out, n.FrameID+"@"+string(rune('0'+n.Version)))
		}
		if code != http.StatusOK {
			t.Fatalf("%s reads the notes: %d", who, code)
		}
		return strings.Join(out, ",")
	}
	if got := shown("editor"); got != "intro@2,just-drawn@1" {
		t.Fatalf("the editor reads [%s], want intro@2,just-drawn@1", got)
	}
	live := func(frame string) bool {
		t.Helper()
		n, err := pgexec.Scalar[int](ctx, `SELECT count(*) FROM board_frame_notes WHERE board_uuid = $1 AND frame_id = $2 AND deleted_at IS NULL`, board, frame)
		if err != nil {
			t.Fatal(err)
		}
		return n == 1
	}
	if !live("erased") || !live("pruned") {
		t.Errorf("after a read: the deleted frame's note live = %v, the gone frame's = %v; want both live (a read never writes)", live("erased"), live("pruned"))
	}

	// The frame comes back (a snapshot restored, an undo after the prune):
	// the stored document holds "pruned" again, and its note shows.
	pruned := scene.Frame("pruned", "Appendix", 1000, 0, 400, 300, 1)
	restored, _, err := scene.State(6, []scene.Element{intro, erased, pruned}, nil, nil)
	if err != nil {
		t.Fatal(err)
	}
	dg.Mutate(t, []map[string]any{{"uid": uids["b"], "board_state": restored}})
	if got := shown("owner"); got != "intro@2,just-drawn@1,pruned@1" {
		t.Fatalf("after the frame came back the owner reads [%s], want intro@2,just-drawn@1,pruned@1", got)
	}

	// A delete is a write on the version read too: one on a version that is
	// no longer the note's, and one from no version over a live note, are
	// each refused with the note as it now is, and the note stays live.
	for _, c := range []struct {
		name    string
		version int
	}{{"a delete on an old version", 1}, {"a delete from no version", 0}} {
		if code, a := save("editor", "intro", "", c.version); code != http.StatusConflict || a.Code != "stale_note" || a.Data == nil ||
			a.Data.Version != 2 || a.Data.Body != "Open with the numbers, then the demo" {
			t.Errorf("%s answers %d %+v, want 409 stale_note with version 2", c.name, code, a)
		}
		if !live("intro") {
			t.Fatalf("%s removed the note", c.name)
		}
	}

	// An empty note is its soft delete, on the version read; then the frame
	// starts again from no version.
	if code, a := save("owner", "intro", "", 2); code != http.StatusOK || a.Data != nil {
		t.Fatalf("emptying the intro's note answers %d %+v", code, a)
	}
	if live("intro") {
		t.Fatal("an emptied note is still live")
	}
	if n, err := pgexec.Scalar[int](ctx, `SELECT count(*) FROM board_frame_notes WHERE board_uuid = $1 AND frame_id = 'intro' AND deleted_at IS NOT NULL`, board); err != nil || n != 1 {
		t.Fatalf("the emptied note's row: %d soft-deleted (%v), want 1", n, err)
	}
	if code, a := save("editor", "intro", "A fresh start", 0); code != http.StatusOK || a.Data.Version != 1 {
		t.Fatalf("a new note after the delete answers %d %+v", code, a)
	}
}
