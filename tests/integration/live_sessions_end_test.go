//go:build integration
// +build integration

package integration_test

// Does losing access end a live doc or board session at once?
//
// A collaboration socket is authorised once, when it connects: the
// collaboration service asks the API then, and never again. So someone
// deactivated, taken off a team or a project, or taken off a board, and a
// guest whose link was turned off, kept the doc or board they had open: its
// live edits kept arriving, and an editor kept editing, until the tab closed.
// Each of those paths now asks the service to close the sockets concerned,
// and the reconnect that follows is refused.
//
// The service's internal port is a recorder here (COLLAB_LIVE_URL): these
// check what the API asks of it and that the reconnect is refused;
// collaboration-service live/ops.test.js checks that the sockets close.
//
// Run: go test -tags=integration ./tests/integration/ -run 'TestLiveSessions' -v

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/go-chi/chi/v5"
	"github.com/google/uuid"

	boardAdapter "github.com/akashc777/OneCamp/adapter/Board"
	accessBusiness "github.com/akashc777/OneCamp/business/Access"
	boardBusiness "github.com/akashc777/OneCamp/business/Board"
	departedBusiness "github.com/akashc777/OneCamp/business/Departed"
	docBusiness "github.com/akashc777/OneCamp/business/Doc"
	guestBusiness "github.com/akashc777/OneCamp/business/Guest"
	projectBusiness "github.com/akashc777/OneCamp/business/Project"
	settingsBusiness "github.com/akashc777/OneCamp/business/Settings"
	teamBusiness "github.com/akashc777/OneCamp/business/Team"
	userBusiness "github.com/akashc777/OneCamp/business/User"
	docController "github.com/akashc777/OneCamp/controllers/Doc"
	guestController "github.com/akashc777/OneCamp/controllers/Guest"
	"github.com/akashc777/OneCamp/helpers"
	"github.com/akashc777/OneCamp/initializers/postgresInit"
	customMiddleware "github.com/akashc777/OneCamp/middleware"
	dgraphStruct "github.com/akashc777/OneCamp/models/dgraph"
	guestModel "github.com/akashc777/OneCamp/models/postgres/Guest"
	userModels "github.com/akashc777/OneCamp/models/postgres/User"
	"github.com/akashc777/OneCamp/services/collab"
	"github.com/akashc777/OneCamp/tests/integration"
)

// liveCall is one op the API asked of the collaboration service.
type liveCall struct {
	Path string
	Body struct {
		UserIDs   []string `json:"user_ids"`
		UserUUIDs []string `json:"user_uuids"`
	}
}

// liveOps is the collaboration service's internal port, recording every op
// the API asks of it and answering each as done.
type liveOps struct {
	mu    sync.Mutex
	calls []liveCall
}

func recordLiveOps(t *testing.T) *liveOps {
	t.Helper()
	ops := &liveOps{}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		c := liveCall{Path: r.URL.Path}
		_ = json.NewDecoder(r.Body).Decode(&c.Body)
		ops.mu.Lock()
		ops.calls = append(ops.calls, c)
		ops.mu.Unlock()
		w.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(w, `{"msg":"ok","data":{"closed":1}}`)
	}))
	t.Cleanup(srv.Close)
	t.Setenv("INTERNAL_SECRET", "live-sessions-integration-secret")
	t.Setenv(collab.LiveURLEnv, srv.URL)
	return ops
}

// asked waits for an op matching match.
func (o *liveOps) asked(match func(liveCall) bool) bool {
	for end := time.Now().Add(5 * time.Second); time.Now().Before(end); time.Sleep(50 * time.Millisecond) {
		o.mu.Lock()
		found := slices.ContainsFunc(o.calls, match)
		o.mu.Unlock()
		if found {
			return true
		}
	}
	return false
}

// closedEverywhere is whether the API asked that every socket who has open,
// on any doc or board, be closed.
func (o *liveOps) closedEverywhere(who string) bool {
	return o.asked(func(c liveCall) bool {
		return c.Path == "/live/people/close" && slices.Contains(c.Body.UserIDs, who)
	})
}

// boardAskedAgain is whether the API asked that a board's sockets be
// authorised again: who's, or everyone's when who is "".
func (o *liveOps) boardAskedAgain(board, who string) bool {
	return o.asked(func(c liveCall) bool {
		if c.Path != "/live/boards/"+board+"/reauth" {
			return false
		}
		if who == "" {
			return len(c.Body.UserUUIDs) == 0
		}
		return slices.Contains(c.Body.UserUUIDs, who)
	})
}

// liveSessionStores brings up what the paths here read and write.
func liveSessionStores(t *testing.T) (*integration.Env, *integration.DgraphEnv) {
	t.Helper()
	t.Setenv("JWT_SECRET", "live-sessions-integration-jwt-secret")
	env := integration.SetupEnv(t)
	if err := postgresInit.ConnectPostgres(context.Background(), env.DSN); err != nil {
		t.Fatalf("wire the project pool at the test database: %v", err)
	}
	integration.SetupRedis(t)
	integration.StubMqttClient(t)
	return env, integration.SetupDgraph(t)
}

// collabAnswer is what the API answers the collaboration service when a
// member's tab connects with token (VerifyDocColabAuth, which both the doc
// session and a board's authorize go through).
func collabAnswer(token string) int {
	ok := http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) { w.WriteHeader(http.StatusOK) })
	r := httptest.NewRequest(http.MethodPost, "/docColab/session", strings.NewReader(`{}`))
	r.Header.Set("Authorization", "Bearer "+token)
	w := httptest.NewRecorder()
	customMiddleware.VerifyDocColabAuth(ok).ServeHTTP(w, r)
	return w.Code
}

// Deactivated, or taken off the team or project a doc reached them through:
// every live socket the person has open closes, and the reconnect is
// refused.
func TestLiveSessionsEndWhenSomeoneLosesTheirPlace(t *testing.T) {
	env, dg := liveSessionStores(t)
	live := recordLiveOps(t)
	ctx := context.Background()
	people := livePeople(t, env, "owner", "sam", "maya", "ines")
	team, project := uuid.NewString(), uuid.NewString()
	uids := dg.Mutate(t, []map[string]any{
		{"uid": "_:owner", "dgraph.type": "User", "user_uuid": people["owner"].String(), "user_name": "owner"},
		{"uid": "_:sam", "dgraph.type": "User", "user_uuid": people["sam"].String(), "user_name": "sam"},
		{"uid": "_:maya", "dgraph.type": "User", "user_uuid": people["maya"].String(), "user_name": "maya", "user_teams": []map[string]any{{"uid": "_:team"}}},
		{"uid": "_:ines", "dgraph.type": "User", "user_uuid": people["ines"].String(), "user_name": "ines", "user_projects": []map[string]any{{"uid": "_:p"}}},
		{"uid": "_:team", "dgraph.type": "Team", "team_uuid": team, "team_name": "Finance", "team_deleted_at": "0001-01-01T00:00:00Z",
			"team_members": []map[string]any{{"uid": "_:maya"}}},
		{"uid": "_:p", "dgraph.type": "Project", "project_uuid": project, "project_name": "Launch", "project_deleted_at": "0001-01-01T00:00:00Z",
			"project_members": []map[string]any{{"uid": "_:ines"}}},
	})

	t.Run("deactivated", func(t *testing.T) {
		sam := people["sam"]
		token, err := helpers.SignSessionToken(sam.String(), helpers.TokenTypeAccess, time.Now().Add(10*time.Minute).Unix())
		if err != nil {
			t.Fatal(err)
		}
		if code := collabAnswer(token); code != http.StatusOK {
			t.Fatalf("before: Sam's tab is answered %d", code)
		}
		if err := userBusiness.DeactivateUser(ctx, sam); err != nil {
			t.Fatalf("deactivate Sam: %v", err)
		}
		if !live.closedEverywhere(sam.String()) {
			t.Fatal("deactivating Sam left his live doc and board sockets open")
		}
		if code := collabAnswer(token); code != http.StatusUnauthorized {
			t.Fatalf("a deactivated person's reconnect is answered %d", code)
		}
	})

	owner := accessBusiness.Subject{UserUID: uids["owner"], UserUUID: people["owner"].String()}
	by := &dgraphStruct.DgraphUser{Uid: uids["owner"], Uuid: people["owner"].String(), UserName: "owner"}
	priv := true
	level := func(name, doc string) accessBusiness.Level {
		t.Helper()
		d, err := accessBusiness.Rules.Resolve(ctx, accessBusiness.Subject{UserUID: uids[name], UserUUID: people[name].String()},
			accessBusiness.Resource{Kind: accessBusiness.KindDoc, UUID: doc})
		if err != nil && !accessBusiness.Refused(err) {
			t.Fatalf("resolve %s on %s: %v", name, doc, err)
		}
		return d.Level
	}

	t.Run("taken off a team", func(t *testing.T) {
		budget, err := docBusiness.CreateDocInTree(ctx, by, docBusiness.NewDoc{Title: "Quarterly budget", Private: &priv}, nil)
		if err != nil {
			t.Fatal(err)
		}
		if _, err := accessBusiness.Apply(ctx, owner, accessBusiness.Change{
			Resource: accessBusiness.Resource{Kind: accessBusiness.KindDoc, UUID: budget.Uuid},
			Add:      []accessBusiness.Grant{{Kind: accessBusiness.PrincipalTeam, UUID: team, Level: accessBusiness.LevelView}},
		}); err != nil {
			t.Fatal(err)
		}
		if got := level("maya", budget.Uuid); got < accessBusiness.LevelView {
			t.Fatalf("before: Maya reads the team's doc at %v", got)
		}
		finance := &dgraphStruct.DgraphTeam{Uid: uids["team"], Uuid: team}
		if err := teamBusiness.RemoveMemberFromTeam(ctx, finance, uids["maya"], people["maya"].String()); err != nil {
			t.Fatalf("take Maya off the team: %v", err)
		}
		if !live.closedEverywhere(people["maya"].String()) {
			t.Fatal("taking Maya off the team left her live doc sockets open")
		}
		if got := level("maya", budget.Uuid); got >= accessBusiness.LevelView {
			t.Fatalf("Maya's reconnect to the team's doc is let in at %v", got)
		}
	})

	t.Run("taken off a project", func(t *testing.T) {
		plan, err := docBusiness.CreateDocInTree(ctx, by, docBusiness.NewDoc{Title: "Launch checklist", Private: &priv, Space: "project:" + project}, nil)
		if err != nil {
			t.Fatal(err)
		}
		if got := level("ines", plan.Uuid); got < accessBusiness.LevelView {
			t.Fatalf("before: Ines reads the project's doc at %v", got)
		}
		if err := projectBusiness.RemoveMemberFromProject(ctx, uids["ines"], people["ines"].String(), uids["p"], project); err != nil {
			t.Fatalf("take Ines off the project: %v", err)
		}
		if !live.closedEverywhere(people["ines"].String()) {
			t.Fatal("taking Ines off the project left her live doc sockets open")
		}
		if got := level("ines", plan.Uuid); got >= accessBusiness.LevelView {
			t.Fatalf("Ines's reconnect to the project's doc is let in at %v", got)
		}
	})
}

// A guest link turned off, by any path: the guest's live view of the doc or
// board ends at once (their collab identity is guest-<grant id>), and the
// reconnect is refused. A link's end is told to the service with each
// authorisation, so an open socket closes when the link runs out.
func TestLiveSessionsOfAGuestEndWhenTheirLinkStops(t *testing.T) {
	env, dg := liveSessionStores(t)
	live := recordLiveOps(t)
	ctx := context.Background()
	people := livePeople(t, env, "owner", "maya")
	uids := dg.Mutate(t, []map[string]any{
		{"uid": "_:owner", "dgraph.type": "User", "user_uuid": people["owner"].String(), "user_name": "owner"},
		{"uid": "_:maya", "dgraph.type": "User", "user_uuid": people["maya"].String(), "user_name": "maya"},
	})
	if err := settingsBusiness.SetGuestAccessEnabled(true); err != nil {
		t.Fatal(err)
	}
	priv := true
	doc, err := docBusiness.CreateDocInTree(ctx, &dgraphStruct.DgraphUser{Uid: uids["owner"], Uuid: people["owner"].String(), UserName: "owner"},
		docBusiness.NewDoc{Title: "Launch plan", Private: &priv}, nil)
	if err != nil {
		t.Fatal(err)
	}
	board := uuid.NewString()
	dg.Mutate(t, []map[string]any{{"uid": "_:b", "dgraph.type": "Board", "board_uuid": board, "board_title": "Launch retro",
		"board_private": false, "board_deleted_at": "0001-01-01T00:00:00Z"}})

	// link is a live link someone made to a doc ("comment": a doc's view link
	// is a page, never live) or a board, and a guest's collab token for it.
	link := func(by uuid.UUID, kind, id string) (uuid.UUID, string) {
		t.Helper()
		capability := "comment"
		if kind == "board" {
			capability = "view"
		}
		g, err := guestBusiness.CreateResourceGrant(ctx, by, kind, id, capability, time.Hour, false)
		if err != nil {
			t.Fatal(err)
		}
		grant, err := guestModel.GetByID(ctx, g.GrantID)
		if err != nil {
			t.Fatal(err)
		}
		token, err := guestBusiness.IssueGuestCollabToken(ctx, grant, "Ana")
		if err != nil {
			t.Fatal(err)
		}
		if _, err := guestBusiness.AuthorizeGuestCollab(ctx, token, kind, id); err != nil {
			t.Fatalf("before: the guest's tab is refused: %v", err)
		}
		return g.GrantID, token
	}
	refused := func(token, kind, id string) bool {
		_, err := guestBusiness.AuthorizeGuestCollab(ctx, token, kind, id)
		return err != nil
	}
	guest := func(grant uuid.UUID) string { return "guest-" + grant.String() }
	// owner is the owner's session, and asOwner a request made in it.
	var owner userModels.UserInfo
	owner.UserPostgresInfo.Id = people["owner"]
	owner.UserDgraphInfo = dgraphStruct.DgraphUser{Uid: uids["owner"], Uuid: people["owner"].String(), UserName: "owner"}
	asOwner := func(method, target, body string) *http.Request {
		r := httptest.NewRequest(method, target, strings.NewReader(body))
		return r.WithContext(context.WithValue(r.Context(), helpers.UserInfoContextKey, owner))
	}

	t.Run("an admin revokes it", func(t *testing.T) {
		g, token := link(people["owner"], "doc", doc.Uuid)
		if err := guestBusiness.RevokeGrant(ctx, g); err != nil {
			t.Fatal(err)
		}
		if !live.closedEverywhere(guest(g)) {
			t.Fatal("revoking a doc link left its guest's live socket open")
		}
		if !refused(token, "doc", doc.Uuid) {
			t.Fatal("the guest's reconnect is let in")
		}
	})

	t.Run("its doc is made private", func(t *testing.T) {
		open := false
		notes, err := docBusiness.CreateDocInTree(ctx, &owner.UserDgraphInfo, docBusiness.NewDoc{Title: "Launch notes", Private: &open}, nil)
		if err != nil {
			t.Fatal(err)
		}
		g, token := link(people["owner"], "doc", notes.Uuid)
		w := httptest.NewRecorder()
		docController.UpdateDocAccess(w, asOwner(http.MethodPost, "/doc/access/update",
			`{"doc_uuid":"`+notes.Uuid+`","general":{"private":true},"guest_links":"revoke"}`))
		if w.Code != http.StatusOK {
			t.Fatalf("making the doc private answered %d: %s", w.Code, w.Body.String())
		}
		if !live.closedEverywhere(guest(g)) {
			t.Fatal("making a doc private, its links off, left its guest's live socket open")
		}
		if !refused(token, "doc", notes.Uuid) {
			t.Fatal("the guest's reconnect is let in")
		}
	})

	t.Run("its maker turns it off in the share dialog", func(t *testing.T) {
		g, token := link(people["owner"], "board", board)
		m := chi.NewRouter()
		m.Post("/guest/links/{id}/revoke", guestController.RevokeResourceGuestLink)
		w := httptest.NewRecorder()
		m.ServeHTTP(w, asOwner(http.MethodPost, "/guest/links/"+g.String()+"/revoke", ""))
		if w.Code != http.StatusOK {
			t.Fatalf("turning the link off answered %d: %s", w.Code, w.Body.String())
		}
		if !live.closedEverywhere(guest(g)) {
			t.Fatal("turning a board link off left its guest's live socket open")
		}
		if !refused(token, "board", board) {
			t.Fatal("the guest's reconnect is let in")
		}
	})

	t.Run("an admin turns off the links of someone who left", func(t *testing.T) {
		g, token := link(people["maya"], "doc", doc.Uuid)
		if _, err := env.PG.Exec(`UPDATE users SET deleted_at = NOW() WHERE id = $1`, people["maya"]); err != nil {
			t.Fatal(err)
		}
		admin := accessBusiness.Subject{UserUID: uids["owner"], UserUUID: people["owner"].String(), Admin: true}
		if n, err := departedBusiness.TurnOffLinks(ctx, admin, people["maya"]); err != nil || n != 1 {
			t.Fatalf("turned off %d, %v", n, err)
		}
		if !live.closedEverywhere(guest(g)) {
			t.Fatal("turning off a departed person's links left their guest's live socket open")
		}
		if !refused(token, "doc", doc.Uuid) {
			t.Fatal("the guest's reconnect is let in")
		}
	})

	t.Run("the workspace turns guest access off", func(t *testing.T) {
		onDoc, docToken := link(people["owner"], "doc", doc.Uuid)
		onBoard, boardToken := link(people["owner"], "board", board)
		if err := settingsBusiness.SetGuestAccessEnabled(false); err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() { _ = settingsBusiness.SetGuestAccessEnabled(true) })
		guestBusiness.RemoveAllLinkGuests(ctx)
		if !live.closedEverywhere(guest(onDoc)) || !live.closedEverywhere(guest(onBoard)) {
			t.Fatal("turning guest access off left guests' live sockets open")
		}
		if !refused(docToken, "doc", doc.Uuid) || !refused(boardToken, "board", board) {
			t.Fatal("a guest's reconnect is let in with guest access off")
		}
	})

	t.Run("its end is told with each authorisation", func(t *testing.T) {
		_, token := link(people["owner"], "board", board)
		r := httptest.NewRequest(http.MethodPost, "/boardColab/guestAuthorize", strings.NewReader(`{"board_uuid":"`+board+`"}`))
		r.Header.Set("Authorization", "Bearer "+token)
		w := httptest.NewRecorder()
		guestController.GuestBoardColabAuthorize(w, r)
		var out struct {
			Until *time.Time `json:"until"`
		}
		if err := json.Unmarshal(w.Body.Bytes(), &out); err != nil || w.Code != http.StatusOK {
			t.Fatalf("the authorisation answered %d: %s", w.Code, w.Body.String())
		}
		if out.Until == nil || out.Until.Before(time.Now().Add(50*time.Minute)) || out.Until.After(time.Now().Add(time.Hour)) {
			t.Fatalf("the answer gives the link's end as %v, want about an hour from now", out.Until)
		}
	})
}

// A board's sharing changed: its live sockets are authorised again, the
// people's whose place moved, or everyone's when it was made private or
// deleted; the reconnect follows the board as it is now.
func TestLiveSessionsOnABoardFollowItsSharing(t *testing.T) {
	env, dg := liveSessionStores(t)
	live := recordLiveOps(t)
	ctx := context.Background()
	people := livePeople(t, env, "owner", "ed", "vi")
	board := uuid.NewString()
	uids := dg.Mutate(t, []map[string]any{
		{"uid": "_:owner", "dgraph.type": "User", "user_uuid": people["owner"].String(), "user_name": "owner"},
		{"uid": "_:ed", "dgraph.type": "User", "user_uuid": people["ed"].String(), "user_name": "ed"},
		{"uid": "_:vi", "dgraph.type": "User", "user_uuid": people["vi"].String(), "user_name": "vi"},
		{"uid": "_:b", "dgraph.type": "Board", "board_uuid": board, "board_title": "Launch retro", "board_private": true,
			"board_deleted_at": "0001-01-01T00:00:00Z", "board_created_by": map[string]any{"uid": "_:owner"},
			"board_editing_users": []map[string]any{{"uid": "_:ed"}, {"uid": "_:vi"}}},
	})
	// canRead is what a reconnect is told: the board as its live canvas's
	// authorise reads it (GetBasicBoardByUUID), and as its page does.
	canRead := func(name string) (bool, bool) {
		t.Helper()
		basic, err := boardBusiness.GetBasicBoardByUUID(ctx, board, uids[name])
		if err != nil {
			t.Fatal(err)
		}
		page, err := boardBusiness.GetBoardByBoardUUID(ctx, board, uids[name])
		if err != nil {
			t.Fatal(err)
		}
		who := people[name].String()
		if boardBusiness.CanRead(basic, who) != boardBusiness.CanRead(page, who) {
			t.Fatalf("the board's two reads disagree on whether %s may read it", name)
		}
		return boardBusiness.CanRead(basic, who), boardBusiness.CanEdit(basic, who)
	}

	t.Run("an editor taken off it", func(t *testing.T) {
		if err := boardBusiness.UpdateBoardPermissions(ctx, boardAdapter.InputUpdateBoardPermissions{
			BoardId: board, RemoveEditors: []string{people["ed"].String()}}, uids["owner"]); err != nil {
			t.Fatal(err)
		}
		if !live.boardAskedAgain(board, people["ed"].String()) {
			t.Fatal("taking Ed off the board left his live board socket as it was")
		}
		if read, _ := canRead("ed"); read {
			t.Fatal("Ed's reconnect to the private board is let in")
		}
	})

	t.Run("an editor made a viewer", func(t *testing.T) {
		if err := boardBusiness.UpdateBoardPermissions(ctx, boardAdapter.InputUpdateBoardPermissions{
			BoardId: board, AddViewers: []string{people["vi"].String()}}, uids["owner"]); err != nil {
			t.Fatal(err)
		}
		if !live.boardAskedAgain(board, people["vi"].String()) {
			t.Fatal("making Vi a viewer left her live board socket editing")
		}
		if read, edit := canRead("vi"); !read || edit {
			t.Fatalf("Vi comes back reading %v, editing %v; want reading only", read, edit)
		}
	})

	t.Run("made private", func(t *testing.T) {
		if err := boardBusiness.UpdateBoard(ctx, &boardAdapter.InputUpdateBoard{BoardId: board, IsPrivate: &[]bool{true}[0]}); err != nil {
			t.Fatal(err)
		}
		if !live.boardAskedAgain(board, "") {
			t.Fatal("making the board private left everyone's live board sockets as they were")
		}
	})

	t.Run("deleted", func(t *testing.T) {
		if err := boardBusiness.DeleteBoard(ctx, board); err != nil {
			t.Fatal(err)
		}
		if !live.boardAskedAgain(board, "") {
			t.Fatal("deleting the board left everyone's live board sockets open")
		}
		if read, _ := canRead("owner"); read {
			t.Fatal("a deleted board still opens, live canvas and page")
		}
	})
}
