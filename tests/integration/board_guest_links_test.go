//go:build integration
// +build integration

package integration_test

// A board's link with a role, a password and an end (miro-33 HEAD A). A
// link's guest is a named guest grant, never a user: it views, comments
// (comment) or draws (edit) as its link lets it, under "Guest: <name>" from
// the token the server signed, and nothing it sends names anyone else. Only
// the board's owner and co-owners make, change or turn off its links, each
// change audited; a link turned off, run out or re-passworded stops on the
// next request and the next live connection; password tries are limited per
// address and link (the docs' C6 code); no plan gates any of it.
//
// Run: go test -tags=integration ./tests/integration/ -run 'TestM[1-6]_|TestToWin_' -v

import (
	"bytes"
	"context"
	"database/sql"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/dgraph-io/dgo/v230/protos/api"
	"github.com/go-chi/chi/v5"
	"github.com/google/uuid"

	boardBusiness "github.com/akashc777/OneCamp/business/Board"
	settingsBusiness "github.com/akashc777/OneCamp/business/Settings"
	guestController "github.com/akashc777/OneCamp/controllers/Guest"
	userDomain "github.com/akashc777/OneCamp/domain/User"
	"github.com/akashc777/OneCamp/helpers"
	"github.com/akashc777/OneCamp/initializers/dgraphInit"
	dgraphStruct "github.com/akashc777/OneCamp/models/dgraph"
	guestModel "github.com/akashc777/OneCamp/models/postgres/Guest"
	userModels "github.com/akashc777/OneCamp/models/postgres/User"
	"github.com/akashc777/OneCamp/services/collab"
	"github.com/akashc777/OneCamp/tests/integration"
)

// boardOp is one op the API asked of the collaboration service, its body as sent.
type boardOp struct {
	Path string
	Body map[string]any
}

// boardOps is the collaboration service's internal port, recording every op
// whole (a comment's author, a close's people) and answering each as done.
type boardOps struct {
	mu    sync.Mutex
	calls []boardOp
}

func recordBoardOps(t *testing.T) *boardOps {
	t.Helper()
	ops := &boardOps{}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		c := boardOp{Path: r.URL.Path}
		_ = json.NewDecoder(r.Body).Decode(&c.Body)
		ops.mu.Lock()
		ops.calls = append(ops.calls, c)
		ops.mu.Unlock()
		w.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(w, `{"msg":"ok","data":{"closed":1}}`)
	}))
	t.Cleanup(srv.Close)
	t.Setenv("INTERNAL_SECRET", "board-guest-links-integration-secret")
	t.Setenv(collab.LiveURLEnv, srv.URL)
	return ops
}

// find waits up to five seconds for an op matching match.
func (o *boardOps) find(match func(boardOp) bool) (boardOp, bool) {
	for end := time.Now().Add(5 * time.Second); ; time.Sleep(50 * time.Millisecond) {
		o.mu.Lock()
		for _, c := range o.calls {
			if match(c) {
				o.mu.Unlock()
				return c, true
			}
		}
		o.mu.Unlock()
		if time.Now().After(end) {
			return boardOp{}, false
		}
	}
}

// comments is every comment op asked so far.
func (o *boardOps) comments() []boardOp {
	o.mu.Lock()
	defer o.mu.Unlock()
	var out []boardOp
	for _, c := range o.calls {
		if strings.HasSuffix(c.Path, "/comment") {
			out = append(out, c)
		}
	}
	return out
}

// closes is whether the service was asked to close the sockets of who.
func (o *boardOps) closes(who string) bool {
	_, ok := o.find(func(c boardOp) bool {
		if c.Path != "/live/people/close" {
			return false
		}
		ids, _ := c.Body["user_ids"].([]any)
		for _, id := range ids {
			if id == who {
				return true
			}
		}
		return false
	})
	return ok
}

// opAuthor is the author a comment op names: by.user_uuid and name.
func opAuthor(c boardOp) (string, string) {
	by, _ := c.Body["by"].(map[string]any)
	id, _ := by["user_uuid"].(string)
	name, _ := c.Body["name"].(string)
	return id, name
}

// linkWorld is a board (owner, an editor who may share by the board's
// switch, an outsider), the link routes as its people call them, and the
// public guest routes as a link's guest calls them.
type linkWorld struct {
	t      *testing.T
	pg     *sql.DB
	dg     *integration.DgraphEnv
	ops    *boardOps
	board  string
	people map[string]userModels.UserInfo
	member chi.Router
	public chi.Router
}

func newLinkWorld(t *testing.T) *linkWorld {
	t.Helper()
	env, dg := liveSessionStores(t)
	ops := recordBoardOps(t)
	ids := livePeople(t, env, "owner", "maya", "omar")
	board := uuid.NewString()
	uids := dg.Mutate(t, []map[string]any{
		{"uid": "_:owner", "dgraph.type": "User", "user_uuid": ids["owner"].String(), "user_name": "owner"},
		{"uid": "_:maya", "dgraph.type": "User", "user_uuid": ids["maya"].String(), "user_name": "maya"},
		{"uid": "_:omar", "dgraph.type": "User", "user_uuid": ids["omar"].String(), "user_name": "omar"},
		{"uid": "_:b", "dgraph.type": "Board", "board_uuid": board, "board_title": "Launch retro", "board_private": false,
			"board_deleted_at": "0001-01-01T00:00:00Z", "board_created_by": map[string]any{"uid": "_:owner"},
			"board_editing_users": []map[string]any{{"uid": "_:maya"}}, "board_editors_share": true},
	})
	if err := settingsBusiness.SetGuestAccessEnabled(true); err != nil {
		t.Fatal(err)
	}
	w := &linkWorld{t: t, pg: env.PG, dg: dg, ops: ops, board: board, people: map[string]userModels.UserInfo{}}
	for _, n := range []string{"owner", "maya", "omar"} {
		var u userModels.UserInfo
		u.UserPostgresInfo.Id = ids[n]
		u.UserDgraphInfo = dgraphStruct.DgraphUser{Uid: uids[n], Uuid: ids[n].String(), UserName: n}
		w.people[n] = u
	}
	m := chi.NewRouter()
	m.Post("/guest/links", guestController.CreateResourceGuestLink)
	m.Patch("/guest/links/{id}", guestController.ChangeResourceGuestLink)
	m.Post("/guest/links/{id}/revoke", guestController.RevokeResourceGuestLink)
	m.Delete("/guest/links/{id}/publish", guestController.UnpublishGuestLink)
	w.member = m
	p := chi.NewRouter()
	p.Post("/guest/collab/{token}", guestController.GuestCollabToken)
	p.Post("/boardColab/guestAuthorize", guestController.GuestBoardColabAuthorize)
	p.Post("/guest/board-comment", guestController.GuestBoardComment)
	p.Get("/guest/board-polls/{token}", guestController.GuestBoardPolls)
	p.Get("/guest/board-attachment/{token}/{obj_uuid}", guestController.GuestBoardAttachment)
	w.public = p
	return w
}

// as is a request by one of the board's people.
func (w *linkWorld) as(who, method, target, body string) *httptest.ResponseRecorder {
	r := httptest.NewRequest(method, target, strings.NewReader(body))
	rec := httptest.NewRecorder()
	w.member.ServeHTTP(rec, r.WithContext(context.WithValue(r.Context(), helpers.UserInfoContextKey, w.people[who])))
	return rec
}

// guest is a request by a link's guest from ip, with bearer (a live token) when set.
func (w *linkWorld) guest(target, body, bearer, ip string) *httptest.ResponseRecorder {
	r := httptest.NewRequest(http.MethodPost, target, strings.NewReader(body))
	r.RemoteAddr = ip + ":40000"
	if bearer != "" {
		r.Header.Set("Authorization", "Bearer "+bearer)
	}
	rec := httptest.NewRecorder()
	w.public.ServeHTTP(rec, r)
	return rec
}

// linkAnswer is a JSON answer's fields: data's when it has data.
func linkAnswer(t *testing.T, rec *httptest.ResponseRecorder) map[string]any {
	t.Helper()
	var out map[string]any
	if err := json.Unmarshal(rec.Body.Bytes(), &out); err != nil {
		t.Fatalf("not JSON (%d): %s", rec.Code, rec.Body)
	}
	if d, ok := out["data"].(map[string]any); ok {
		return d
	}
	return out
}

// mint makes a board link as who: the answer, its grant id and raw token.
func (w *linkWorld) mint(who, role, extra string) (*httptest.ResponseRecorder, string, string) {
	w.t.Helper()
	body := `{"resource_type":"board","resource_id":"` + w.board + `","capability":"` + role + `","ttl_hours":24` + extra + `}`
	rec := w.as(who, http.MethodPost, "/guest/links", body)
	if rec.Code != http.StatusOK {
		return rec, "", ""
	}
	d := linkAnswer(w.t, rec)
	gid, _ := d["grant_id"].(string)
	token, _ := d["token"].(string)
	return rec, gid, token
}

// mustMint is mint by the owner, which must work.
func (w *linkWorld) mustMint(role, extra string) (string, string) {
	w.t.Helper()
	rec, gid, token := w.mint("owner", role, extra)
	if rec.Code != http.StatusOK || gid == "" || token == "" {
		w.t.Fatalf("the owner makes a %s link: %d %s", role, rec.Code, rec.Body)
	}
	return gid, token
}

// exchange is the guest opening the link: its answer.
func (w *linkWorld) exchange(token, body, ip string) *httptest.ResponseRecorder {
	return w.guest("/guest/collab/"+token, body, "", ip)
}

// liveToken is the guest named name opening the link (with password when
// set): their live token, which must be given.
func (w *linkWorld) liveToken(token, name, password string) string {
	w.t.Helper()
	body := `{"display_name":"` + name + `"`
	if password != "" {
		body += `,"password":"` + password + `"`
	}
	rec := w.exchange(token, body+`}`, "198.51.100.20")
	d := linkAnswer(w.t, rec)
	live, _ := d["collab_token"].(string)
	if rec.Code != http.StatusOK || live == "" {
		w.t.Fatalf("the guest opens the link: %d %s", rec.Code, rec.Body)
	}
	return live
}

// authorize is the collaboration service asking about a guest's live token:
// the status and canEdit / canComment.
func (w *linkWorld) authorize(live string) (int, bool, bool) {
	rec := w.guest("/boardColab/guestAuthorize", `{"board_uuid":"`+w.board+`"}`, live, "10.1.2.3")
	if rec.Code != http.StatusOK {
		return rec.Code, false, false
	}
	var out struct {
		CanEdit    bool `json:"canEdit"`
		CanComment bool `json:"canComment"`
	}
	_ = json.Unmarshal(rec.Body.Bytes(), &out)
	return rec.Code, out.CanEdit, out.CanComment
}

// comment is the guest's comment change, body's fields beside the board's id.
func (w *linkWorld) comment(live, fields string) *httptest.ResponseRecorder {
	return w.guest("/guest/board-comment", `{"board_id":"`+w.board+`",`+fields+`}`, live, "198.51.100.20")
}

// auditActions waits for the log's rows about grant until it holds each of
// want, and answers every action there with its metadata.
func (w *linkWorld) auditActions(grant string, want ...string) map[string]string {
	w.t.Helper()
	for end := time.Now().Add(10 * time.Second); ; time.Sleep(100 * time.Millisecond) {
		rows, err := w.pg.Query(`SELECT action, metadata::text FROM admin_audit_log WHERE metadata::text LIKE '%' || $1 || '%' ORDER BY seq`, grant)
		if err != nil {
			w.t.Fatal(err)
		}
		got := map[string]string{}
		for rows.Next() {
			var action, meta string
			if err := rows.Scan(&action, &meta); err != nil {
				w.t.Fatal(err)
			}
			got[action] = meta
		}
		rows.Close()
		missing := false
		for _, a := range want {
			if _, ok := got[a]; !ok {
				missing = true
			}
		}
		if !missing || time.Now().After(end) {
			return got
		}
	}
}

// linkRefusal is a JSON refusal's status and words.
func linkRefusal(t *testing.T, rec *httptest.ResponseRecorder) (string, string) {
	t.Helper()
	var out struct {
		Status string `json:"status"`
		Msg    string `json:"msg"`
	}
	_ = json.Unmarshal(rec.Body.Bytes(), &out)
	return out.Status, out.Msg
}

const unavailableWords = "This link is no longer available."

// M-1: a link's guest is its grant, never a user: after a guest opens the
// link and comments, the workspace has the same users and seats as before,
// and no person in the graph wears the guest's name.
func TestM1_ALinkGuestIsAGrantNeverAUser(t *testing.T) {
	w := newLinkWorld(t)
	ctx := context.Background()
	count := func() int {
		var n int
		if err := w.pg.QueryRow(`SELECT count(*) FROM users`).Scan(&n); err != nil {
			t.Fatal(err)
		}
		return n
	}
	usersBefore := count()
	seatsBefore, _, err := userDomain.SeatUsage(ctx)
	if err != nil {
		t.Fatal(err)
	}
	_, token := w.mustMint("comment", "")
	live := w.liveToken(token, "Priya Guestly", "")
	if rec := w.comment(live, `"action":"add","x":10,"y":20,"text":"Love the colours"`); rec.Code != http.StatusOK {
		t.Fatalf("the guest comments: %d %s", rec.Code, rec.Body)
	}
	if n := count(); n != usersBefore {
		t.Errorf("users went from %d to %d: a guest became a user row", usersBefore, n)
	}
	if seats, _, err := userDomain.SeatUsage(ctx); err != nil || seats != seatsBefore {
		t.Errorf("seats used went from %d to %d (%v): a guest took a seat", seatsBefore, seats, err)
	}
	for _, name := range []string{"Priya Guestly", "Guest: Priya Guestly"} {
		got := string(w.dg.Query(t, `{ q(func: type(User)) @filter(eq(user_name, "`+name+`")) { uid } }`))
		if strings.Contains(got, "uid") {
			t.Errorf("the graph holds a person named %q: %s", name, got)
		}
	}
}

// M-2: a link's token is kept only as its hash, and a link turned off or run
// out is refused on the very next request and live connection; turning it
// off closes the live sessions its guests have open.
func TestM2_LinkTokensAreStoredHashedAndCheckedOnEveryRequest(t *testing.T) {
	w := newLinkWorld(t)
	gid, token := w.mustMint("comment", "")
	var row string
	if err := w.pg.QueryRow(`SELECT row_to_json(g)::text FROM guest_grants g WHERE id = $1`, gid).Scan(&row); err != nil {
		t.Fatal(err)
	}
	if strings.Contains(row, token) {
		t.Fatalf("the link's row holds its raw token: %s", row)
	}
	// row_to_json shows a bytea as hex, so the token kept as bytes would pass
	// the check above: the bytes kept must be the token's SHA-256.
	var kept []byte
	if err := w.pg.QueryRow(`SELECT token_hash FROM guest_grants WHERE id = $1`, gid).Scan(&kept); err != nil {
		t.Fatal(err)
	}
	if want := helpers.LinkSecretHash(token); !bytes.Equal(kept, want) || bytes.Contains(kept, []byte(token)) {
		t.Fatalf("the link keeps %x for its token, want only its SHA-256 %x", kept, want)
	}
	refusedNow := func(what, token, live string) {
		t.Helper()
		if rec := w.exchange(token, `{"display_name":"Priya"}`, "198.51.100.30"); rec.Code != http.StatusNotFound {
			t.Errorf("%s: opening the link answers %d %s, want 404", what, rec.Code, rec.Body)
		} else if _, msg := linkRefusal(t, rec); msg != unavailableWords {
			t.Errorf("%s: opening the link says %q, want %q", what, msg, unavailableWords)
		}
		if code, _, _ := w.authorize(live); code != http.StatusUnauthorized {
			t.Errorf("%s: the live connection is authorised: %d, want 401", what, code)
		}
		if rec := w.comment(live, `"action":"add","x":1,"y":1,"text":"still here?"`); rec.Code != http.StatusUnauthorized {
			t.Errorf("%s: the guest's comment answers %d %s, want 401", what, rec.Code, rec.Body)
		}
	}

	live := w.liveToken(token, "Priya", "")
	if code, _, _ := w.authorize(live); code != http.StatusOK {
		t.Fatalf("a live link's guest is authorised: %d", code)
	}
	if rec := w.as("owner", http.MethodPost, "/guest/links/"+gid+"/revoke", ""); rec.Code != http.StatusOK {
		t.Fatalf("the owner turns the link off: %d %s", rec.Code, rec.Body)
	}
	refusedNow("turned off", token, live)
	if !w.ops.closes("guest-" + gid) {
		t.Errorf("turning the link off didn't close its guests' live sessions (guest-%s)", gid)
	}

	gid2, token2 := w.mustMint("edit", "")
	live2 := w.liveToken(token2, "Priya", "")
	if _, err := w.pg.Exec(`UPDATE guest_grants SET expires_at = now() - interval '1 minute' WHERE id = $1`, gid2); err != nil {
		t.Fatal(err)
	}
	refusedNow("run out", token2, live2)
}

// M-3: a password link opens only with its password (the docs' C6 code), and
// the eleventh try from one address in the window is refused even when
// right, while another address still gets in.
func TestM3_WrongPasswordRefusedAndRateLimited(t *testing.T) {
	w := newLinkWorld(t)
	_, token := w.mustMint("comment", `,"password":"orchid-42"`)
	if rec := w.exchange(token, `{"display_name":"Priya"}`, "203.0.113.7"); rec.Code != http.StatusUnauthorized {
		t.Fatalf("no password: %d %s, want 401", rec.Code, rec.Body)
	} else if status, _ := linkRefusal(t, rec); status != "password_needed" {
		t.Errorf("no password says %q, want password_needed", status)
	}
	for i := 1; i <= 10; i++ {
		rec := w.exchange(token, `{"display_name":"Priya","password":"tulip-`+string(rune('a'+i))+`"}`, "203.0.113.7")
		if status, _ := linkRefusal(t, rec); rec.Code != http.StatusUnauthorized || status != "wrong_password" {
			t.Fatalf("wrong try %d: %d %s, want 401 wrong_password", i, rec.Code, rec.Body)
		}
	}
	rec := w.exchange(token, `{"display_name":"Priya","password":"orchid-42"}`, "203.0.113.7")
	if status, _ := linkRefusal(t, rec); rec.Code != http.StatusTooManyRequests || status != "rate_limited" {
		t.Fatalf("the 11th try from one address: %d %s, want 429 rate_limited", rec.Code, rec.Body)
	}
	ok := w.exchange(token, `{"display_name":"Priya","password":"orchid-42"}`, "198.51.100.44")
	d := linkAnswer(t, ok)
	if ok.Code != http.StatusOK || d["collab_token"] == nil || d["unlock"] == nil {
		t.Fatalf("the right password from another address: %d %s, want a live token and an unlock", ok.Code, ok.Body)
	}
	unlock, _ := d["unlock"].(string)
	if again := w.exchange(token, `{"display_name":"Priya","unlock":"`+unlock+`"}`, "198.51.100.44"); again.Code != http.StatusOK {
		t.Errorf("the unlock opens the link again without the password: %d %s", again.Code, again.Body)
	}
}

// M-4: only the board's owner and co-owners make, change or turn off its
// links: an editor is refused all three even with the editors' switch on,
// and makes nothing; each change the owner makes is in the audit log.
func TestM4_OnlyManagersMintChangeOrRevokeBoardLinks(t *testing.T) {
	w := newLinkWorld(t)
	notManager := func(what string, rec *httptest.ResponseRecorder) {
		t.Helper()
		if _, msg := linkRefusal(t, rec); rec.Code != http.StatusForbidden || msg != boardBusiness.ErrNotManager.Error() {
			t.Errorf("the editor %s: %d %s, want 403 %q", what, rec.Code, rec.Body, boardBusiness.ErrNotManager)
		}
	}
	for _, role := range []string{"view", "comment", "edit"} {
		rec, _, _ := w.mint("maya", role, "")
		notManager("making a "+role+" link", rec)
	}
	var n int
	if err := w.pg.QueryRow(`SELECT count(*) FROM guest_grants WHERE resource_id = $1`, w.board).Scan(&n); err != nil || n != 0 {
		t.Fatalf("the refused editor made %d links (%v), want none", n, err)
	}
	gid, _ := w.mustMint("view", "")
	notManager("changing the owner's link", w.as("maya", http.MethodPatch, "/guest/links/"+gid, `{"capability":"edit"}`))
	notManager("turning the owner's link off", w.as("maya", http.MethodPost, "/guest/links/"+gid+"/revoke", ""))
	if rec := w.as("omar", http.MethodPatch, "/guest/links/"+gid, `{"capability":"edit"}`); rec.Code == http.StatusOK {
		t.Errorf("someone who only reads the board changed its link: %d", rec.Code)
	}
	var role string
	if err := w.pg.QueryRow(`SELECT capability FROM guest_grants WHERE id = $1 AND revoked_at IS NULL`, gid).Scan(&role); err != nil || role != "view" {
		t.Fatalf("after the refusals the link is %q (%v), want a live view link", role, err)
	}

	if rec := w.as("owner", http.MethodPatch, "/guest/links/"+gid, `{"capability":"comment","password":"orchid-42","ttl_hours":48}`); rec.Code != http.StatusOK {
		t.Fatalf("the owner changes the link: %d %s", rec.Code, rec.Body)
	}
	if rec := w.as("owner", http.MethodPost, "/guest/links/"+gid+"/revoke", ""); rec.Code != http.StatusOK {
		t.Fatalf("the owner turns the link off: %d %s", rec.Code, rec.Body)
	}
	want := []string{"guest.link.create", "board.guest_link_made", "guest.link.change", "board.guest_link_changed",
		"guest.link.revoke", "board.guest_link_turned_off"}
	got := w.auditActions(gid, want...)
	for _, a := range want {
		if _, ok := got[a]; !ok {
			t.Errorf("the audit log has no %s row for the link (has %v)", a, got)
		}
	}
	for a, meta := range got {
		if strings.Contains(meta, "orchid-42") {
			t.Errorf("the %s row holds the password: %s", a, meta)
		}
	}
}

// M-5: what a guest writes is authored by its grant: the comment op names
// guest-<grant> and "Guest: <name>" from the server's token, whatever author
// the request's body names.
func TestM5_AGuestCommentIsAuthoredByItsGrant(t *testing.T) {
	w := newLinkWorld(t)
	gid, token := w.mustMint("comment", "")
	live := w.liveToken(token, "Priya", "")
	owner := w.people["owner"].UserDgraphInfo.Uuid
	forged := `"action":"add","x":5,"y":6,"text":"Ship it","author_id":"` + owner + `","author_name":"owner","authorId":"` + owner +
		`","authorName":"owner","by":{"user_uuid":"` + owner + `"},"name":"owner"`
	if rec := w.comment(live, forged); rec.Code != http.StatusOK {
		t.Fatalf("the guest comments: %d %s", rec.Code, rec.Body)
	}
	op, ok := w.ops.find(func(c boardOp) bool { return strings.HasSuffix(c.Path, "/live/boards/"+w.board+"/comment") })
	if !ok {
		t.Fatal("the guest's comment never reached the board")
	}
	if id, name := opAuthor(op); id != "guest-"+gid || name != "Guest: Priya" {
		t.Errorf("the comment is written as %q %q, want %q %q", id, name, "guest-"+gid, "Guest: Priya")
	}
}

// M-6: no plan gates a board link's role, password or end: on the free plan
// the owner makes a view, a comment and an edit link, each with a password
// and an end, and the guest opens one with its password.
func TestM6_EveryRoleOnThePlanFree(t *testing.T) {
	prev := helpers.SeatLimit
	t.Cleanup(func() { helpers.SeatLimit = prev })
	helpers.SeatLimit = "25"
	if !helpers.OnFreePlan() {
		t.Fatal("the workspace should be on the free plan")
	}
	w := newLinkWorld(t)
	for _, role := range []string{"view", "comment", "edit"} {
		rec, gid, token := w.mint("owner", role, `,"password":"orchid-42"`)
		if rec.Code != http.StatusOK || gid == "" {
			t.Fatalf("a %s link with a password and an end on the free plan: %d %s", role, rec.Code, rec.Body)
		}
		if d := linkAnswer(t, rec); d["capability"] != role || d["expires_at"] == nil {
			t.Errorf("the %s link answers %v, want its role and its end", role, d)
		}
		if role == "edit" {
			if live := w.liveToken(token, "Priya", "orchid-42"); live == "" {
				t.Error("the guest couldn't open the free plan's password link")
			}
		}
	}
}

// to_win 1: a comment link's guest comments and replies under their guest
// name, and their live connection reads only.
func TestToWin_CommentGuestCommentsAndReplies(t *testing.T) {
	w := newLinkWorld(t)
	gid, token := w.mustMint("comment", "")
	live := w.liveToken(token, "Priya", "")
	if code, canEdit, canComment := w.authorize(live); code != http.StatusOK || canEdit || !canComment {
		t.Errorf("a comment link's guest is authorised %d edit=%v comment=%v, want 200 false true", code, canEdit, canComment)
	}
	if rec := w.comment(live, `"action":"add","x":3,"y":4,"text":"Why this colour?"`); rec.Code != http.StatusOK {
		t.Fatalf("the guest comments: %d %s", rec.Code, rec.Body)
	}
	if rec := w.comment(live, `"action":"reply","comment_id":"pin-1","text":"Answering myself"`); rec.Code != http.StatusOK {
		t.Fatalf("the guest replies: %d %s", rec.Code, rec.Body)
	}
	var actions []string
	for _, c := range w.ops.comments() {
		if id, name := opAuthor(c); id != "guest-"+gid || name != "Guest: Priya" {
			t.Errorf("a comment op written as %q %q", id, name)
		}
		a, _ := c.Body["action"].(string)
		actions = append(actions, a)
	}
	if strings.Join(actions, ",") != "add,reply" {
		t.Errorf("the comment ops asked: %v, want add then reply", actions)
	}
}

// to_win 1: an edit link's guest draws: their live connection writes, and
// they comment too.
func TestToWin_EditGuestDraws(t *testing.T) {
	w := newLinkWorld(t)
	_, token := w.mustMint("edit", "")
	live := w.liveToken(token, "Priya", "")
	if code, canEdit, canComment := w.authorize(live); code != http.StatusOK || !canEdit || !canComment {
		t.Errorf("an edit link's guest is authorised %d edit=%v comment=%v, want 200 true true", code, canEdit, canComment)
	}
	if rec := w.comment(live, `"action":"add","x":3,"y":4,"text":"Moved the frame"`); rec.Code != http.StatusOK {
		t.Errorf("the edit guest comments: %d %s", rec.Code, rec.Body)
	}
}

// to_win 1: a view link's guest writes nothing: their connection reads only
// and their comment is refused before it reaches the board.
func TestToWin_ViewGuestCannotWrite(t *testing.T) {
	w := newLinkWorld(t)
	_, token := w.mustMint("view", "")
	live := w.liveToken(token, "Priya", "")
	if code, canEdit, canComment := w.authorize(live); code != http.StatusOK || canEdit || canComment {
		t.Errorf("a view link's guest is authorised %d edit=%v comment=%v, want 200 false false", code, canEdit, canComment)
	}
	if rec := w.comment(live, `"action":"add","x":3,"y":4,"text":"Let me in"`); rec.Code != http.StatusForbidden {
		t.Errorf("the view guest's comment answers %d %s, want 403", rec.Code, rec.Body)
	}
	if n := len(w.ops.comments()); n != 0 {
		t.Errorf("the view guest's comment reached the board (%d ops)", n)
	}
}

// to_win 1 + 2: a change to a live link reaches the sessions open on it: its
// guests' sockets close, the reconnect is authorised with the new role, and
// a new password refuses the live token given before it until it is typed.
func TestToWin_RoleChangeReachesOpenSessions(t *testing.T) {
	w := newLinkWorld(t)
	gid, token := w.mustMint("view", "")
	live := w.liveToken(token, "Priya", "")
	if _, canEdit, _ := w.authorize(live); canEdit {
		t.Fatal("a view link's guest may edit")
	}
	if rec := w.as("owner", http.MethodPatch, "/guest/links/"+gid, `{"capability":"edit"}`); rec.Code != http.StatusOK {
		t.Fatalf("the owner makes the link an edit link: %d %s", rec.Code, rec.Body)
	}
	if !w.ops.closes("guest-" + gid) {
		t.Errorf("the role change didn't close the link's live sessions (guest-%s)", gid)
	}
	if code, canEdit, _ := w.authorize(live); code != http.StatusOK || !canEdit {
		t.Errorf("the reconnect after the change is authorised %d edit=%v, want 200 true", code, canEdit)
	}
	if rec := w.as("owner", http.MethodPatch, "/guest/links/"+gid, `{"password":"orchid-42"}`); rec.Code != http.StatusOK {
		t.Fatalf("the owner puts a password on the link: %d %s", rec.Code, rec.Body)
	}
	if code, _, _ := w.authorize(live); code != http.StatusUnauthorized {
		t.Errorf("a live token given before the password is authorised: %d, want 401", code)
	}
	if rec := w.exchange(token, `{"display_name":"Priya"}`, "198.51.100.20"); rec.Code != http.StatusUnauthorized {
		t.Errorf("opening the link without its new password: %d %s, want 401", rec.Code, rec.Body)
	}
	fresh := w.liveToken(token, "Priya", "orchid-42")
	if code, canEdit, _ := w.authorize(fresh); code != http.StatusOK || !canEdit {
		t.Errorf("the guest who typed the password is authorised %d edit=%v, want 200 true", code, canEdit)
	}
}

// boardUID is the board's node in the graph.
func (w *linkWorld) boardUID() string {
	w.t.Helper()
	var out struct {
		Q []struct {
			UID string `json:"uid"`
		} `json:"q"`
	}
	if err := json.Unmarshal(w.dg.Query(w.t, `{ q(func: eq(board_uuid, "`+w.board+`")) { uid } }`), &out); err != nil || len(out.Q) != 1 {
		w.t.Fatalf("the board's node: %+v %v", out, err)
	}
	return out.Q[0].UID
}

// read is a guest's GET of one of a link's own reads, from one address.
func (w *linkWorld) read(target string) *httptest.ResponseRecorder {
	r := httptest.NewRequest(http.MethodGet, target, nil)
	r.RemoteAddr = "198.51.100.30:1234"
	rec := httptest.NewRecorder()
	w.public.ServeHTTP(rec, r)
	return rec
}

// M-3 / to_win 2 (CR-292 F1a): a password link's own reads, its polls and its
// pictures, need the unlock its exchange gave (?k=), as a page's do; a new
// password stops every unlock given before it.
func TestM3_APasswordLinksReadsNeedItsUnlock(t *testing.T) {
	w := newLinkWorld(t)
	gid, token := w.mustMint("view", `,"password":"orchid-42"`)
	rec := w.exchange(token, `{"display_name":"Priya","password":"orchid-42"}`, "198.51.100.30")
	unlock, _ := linkAnswer(t, rec)["unlock"].(string)
	if rec.Code != http.StatusOK || unlock == "" {
		t.Fatalf("the guest opens the password link: %d %s", rec.Code, rec.Body)
	}
	polls, picture := "/guest/board-polls/"+token, "/guest/board-attachment/"+token+"/"+uuid.NewString()
	for _, target := range []string{polls, picture} {
		if rec := w.read(target); rec.Code != http.StatusNotFound {
			t.Errorf("%s with no unlock: %d %s, want 404", target, rec.Code, rec.Body)
		}
	}
	if rec := w.read(polls + "?k=" + unlock); rec.Code != http.StatusOK {
		t.Errorf("the polls with the exchange's unlock: %d %s, want 200", rec.Code, rec.Body)
	}
	if rec := w.as("owner", http.MethodPatch, "/guest/links/"+gid, `{"password":"tulip-77"}`); rec.Code != http.StatusOK {
		t.Fatalf("the owner changes the password: %d %s", rec.Code, rec.Body)
	}
	for _, target := range []string{polls, picture} {
		if rec := w.read(target + "?k=" + unlock); rec.Code != http.StatusNotFound {
			t.Errorf("%s with the unlock given before the password changed: %d %s, want 404", target, rec.Code, rec.Body)
		}
	}
}

// M-4 (CR-292 F1b): making a link gives its maker nothing over it. A co-owner
// makes one, is made an editor, and can no longer turn it off.
func TestM4_ALinksMakerWhoNoLongerManagesTheBoardCannotTurnItOff(t *testing.T) {
	w := newLinkWorld(t)
	board, omar := w.boardUID(), w.people["omar"].UserDgraphInfo.Uid
	w.dg.Mutate(t, map[string]any{"uid": board, "board_owning_users": []map[string]any{{"uid": omar}},
		"board_editing_users": []map[string]any{{"uid": omar}}})
	rec, gid, _ := w.mint("omar", "view", "")
	if rec.Code != http.StatusOK || gid == "" {
		t.Fatalf("the co-owner makes a link: %d %s", rec.Code, rec.Body)
	}
	drop, _ := json.Marshal(map[string]any{"uid": board, "board_owning_users": []map[string]any{{"uid": omar}}})
	if _, err := dgraphInit.DoCommitNow(context.Background(), &api.Request{Mutations: []*api.Mutation{{DeleteJson: drop}}}); err != nil {
		t.Fatalf("making the co-owner an editor: %v", err)
	}
	rec = w.as("omar", http.MethodPost, "/guest/links/"+gid+"/revoke", "")
	if _, msg := linkRefusal(t, rec); rec.Code != http.StatusForbidden || msg != boardBusiness.ErrNotManager.Error() {
		t.Errorf("the link's maker, now an editor, turning it off: %d %s, want 403 %q", rec.Code, rec.Body, boardBusiness.ErrNotManager)
	}
	var off sql.NullTime
	if err := w.pg.QueryRow(`SELECT revoked_at FROM guest_grants WHERE id = $1`, gid).Scan(&off); err != nil || off.Valid {
		t.Errorf("the link is off (%v, %v), want it on", off, err)
	}
}

// L1 (CR-292): two changes to one link at once, one of them a password. The
// write is a compare-and-set on the link as it was read: both apply in turn,
// or one is told 409 and writes nothing. A password answered 200 is never lost.
func TestL1_TwoChangesAtOnceNeverLoseAPassword(t *testing.T) {
	w := newLinkWorld(t)
	ctx := context.Background()
	gid, _ := w.mustMint("view", "")
	id := uuid.MustParse(gid)
	stale, err := guestModel.GetByID(ctx, id)
	if err != nil || stale == nil {
		t.Fatalf("reading the link: %v", err)
	}
	if rec := w.as("owner", http.MethodPatch, "/guest/links/"+gid, `{"password":"orchid-42"}`); rec.Code != http.StatusOK {
		t.Fatalf("one manager sets a password: %d %s", rec.Code, rec.Body)
	}
	lost, err := guestModel.ChangeLink(ctx, id, stale, guestModel.LinkChange{Capability: "edit", ExpiresAt: stale.ExpiresAt, PasswordHash: stale.PasswordHash})
	if err != nil || lost != nil {
		t.Errorf("a role change made from a read before the password wrote %+v (%v), want nothing written", lost, err)
	}
	state := func(gid string) (bool, string) {
		var locked bool
		var role string
		if err := w.pg.QueryRow(`SELECT password_hash IS NOT NULL, capability FROM guest_grants WHERE id = $1`, gid).Scan(&locked, &role); err != nil {
			t.Fatal(err)
		}
		return locked, role
	}
	if locked, role := state(gid); !locked || role != "view" {
		t.Errorf("after the stale change the link is locked=%v %q, want its password kept and still view", locked, role)
	}
	for i := 0; i < 8; i++ {
		gid, _ := w.mustMint("view", "")
		var wg sync.WaitGroup
		var pw, role *httptest.ResponseRecorder
		wg.Add(2)
		go func() {
			defer wg.Done()
			pw = w.as("owner", http.MethodPatch, "/guest/links/"+gid, `{"password":"orchid-42"}`)
		}()
		go func() {
			defer wg.Done()
			role = w.as("owner", http.MethodPatch, "/guest/links/"+gid, `{"capability":"edit"}`)
		}()
		wg.Wait()
		for what, rec := range map[string]*httptest.ResponseRecorder{"password": pw, "role": role} {
			if rec.Code == http.StatusConflict {
				if _, msg := linkRefusal(t, rec); !strings.Contains(msg, "changed this link just now") {
					t.Errorf("round %d: the %s change's 409 says %q", i, what, msg)
				}
			} else if rec.Code != http.StatusOK {
				t.Fatalf("round %d: the %s change: %d %s, want 200 or 409", i, what, rec.Code, rec.Body)
			}
		}
		locked, now := state(gid)
		if pw.Code == http.StatusOK && !locked {
			t.Errorf("round %d: the password was answered 200 and the link has none (role change %d)", i, role.Code)
		}
		if role.Code == http.StatusOK && now != "edit" {
			t.Errorf("round %d: the role change was answered 200 and the link is %q", i, now)
		}
	}
}

// F5 (CR-297's probe, its assertion kept): the doc pages' unpublish route
// (DELETE /guest/links/{id}/publish) takes a page off the web. A board link is
// never on the web, so the route refuses it, 404 with words, and the link's
// password stays: no route but the link's own change drops it.
func TestF5_UnpublishNeverDropsABoardLinksPassword(t *testing.T) {
	w := newLinkWorld(t)
	gid, _ := w.mustMint("view", "")
	if rec := w.as("owner", http.MethodPatch, "/guest/links/"+gid, `{"password":"orchid-42"}`); rec.Code != http.StatusOK {
		t.Fatalf("the owner sets a password: %d %s", rec.Code, rec.Body)
	}
	rec := w.as("owner", http.MethodDelete, "/guest/links/"+gid+"/publish", "")
	var locked bool
	if err := w.pg.QueryRow(`SELECT password_hash IS NOT NULL FROM guest_grants WHERE id = $1`, gid).Scan(&locked); err != nil {
		t.Fatal(err)
	}
	if !locked {
		t.Errorf("PROBE: DELETE /guest/links/{id}/publish on a password board link answered %d %s and the link has no password now", rec.Code, rec.Body)
	}
	if rec.Code != http.StatusNotFound || !strings.Contains(rec.Body.String(), "That link isn't on the web.") {
		t.Errorf("unpublishing a board link: %d %s, want 404 \"That link isn't on the web.\"", rec.Code, rec.Body)
	}
}

// F2 (CR-B92's mutant C4): a change of role alone, or of end alone, keeps
// the link's password. After each PATCH /guest/links/{id}: the password and
// its unlock_version stand, the exchange with no password is still 401
// password_needed, and an unlock given before the change still opens the
// link's reads (only a new password or address ends those).
func TestF2_ARoleOrEndChangeAloneKeepsTheLinksPassword(t *testing.T) {
	w := newLinkWorld(t)
	gid, token := w.mustMint("view", "")
	if rec := w.as("owner", http.MethodPatch, "/guest/links/"+gid, `{"password":"orchid-42"}`); rec.Code != http.StatusOK {
		t.Fatalf("the owner sets a password: %d %s", rec.Code, rec.Body)
	}
	rec := w.exchange(token, `{"display_name":"Priya","password":"orchid-42"}`, "198.51.100.30")
	unlock, _ := linkAnswer(t, rec)["unlock"].(string)
	if rec.Code != http.StatusOK || unlock == "" {
		t.Fatalf("the guest opens the password link: %d %s", rec.Code, rec.Body)
	}
	state := func() (bool, int) {
		var locked bool
		var version int
		if err := w.pg.QueryRow(`SELECT password_hash IS NOT NULL, unlock_version FROM guest_grants WHERE id = $1`, gid).Scan(&locked, &version); err != nil {
			t.Fatal(err)
		}
		return locked, version
	}
	_, before := state()
	polls := "/guest/board-polls/" + token + "?k=" + unlock
	for _, change := range []string{`{"capability":"edit"}`, `{"ttl_hours":48}`} {
		if rec := w.as("owner", http.MethodPatch, "/guest/links/"+gid, change); rec.Code != http.StatusOK {
			t.Fatalf("the owner changes %s alone: %d %s", change, rec.Code, rec.Body)
		}
		if locked, version := state(); !locked || version != before {
			t.Errorf("after %s alone the link is locked=%v at unlock_version %d, want its password kept at %d", change, locked, version, before)
		}
		rec := w.exchange(token, `{"display_name":"Priya"}`, "198.51.100.30")
		if status, _ := linkRefusal(t, rec); rec.Code != http.StatusUnauthorized || status != "password_needed" {
			t.Errorf("after %s alone the exchange with no password: %d %s, want 401 password_needed", change, rec.Code, rec.Body)
		}
		if rec := w.read(polls); rec.Code != http.StatusOK {
			t.Errorf("after %s alone the unlock given before: %d %s, want 200", change, rec.Code, rec.Body)
		}
	}
}
