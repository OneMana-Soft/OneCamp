//go:build integration
// +build integration

package integration_test

// Polls on a slide (miro-28 to_win 4), through the real router: chat's polls
// (business/Poll, one engine) with a board and one of its frames as their
// owner, as a call's poll has its call. Who may edit the board starts and
// closes a poll on one of its frames; whoever may open the board reads it and
// votes once (or once per option when it takes several); a share-link guest
// reads its question and tally and never votes; nobody reads or votes on a
// poll of a board they can't open, even by the poll's id; a deleted board's
// polls read as gone. Every vote and close tells the board's own topic, so
// every open tab of the board reads the poll again, and never a channel's.
//
// Run: go test -tags=integration ./tests/integration/ -run 'TestBoardPolls' -v

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
	pollBusiness "github.com/akashc777/OneCamp/business/Poll"
	settingsBusiness "github.com/akashc777/OneCamp/business/Settings"
	"github.com/akashc777/OneCamp/helpers"
	mqttStruct "github.com/akashc777/OneCamp/models/mqtt"
	"github.com/akashc777/OneCamp/models/pgexec"
	guestModel "github.com/akashc777/OneCamp/models/postgres/Guest"
	"github.com/akashc777/OneCamp/router"
	"github.com/akashc777/OneCamp/tests/integration"
)

func TestBoardPollsMigrationUpDownUp(t *testing.T) {
	env := integration.SetupEnv(t)
	count := func(q string, args ...any) int {
		t.Helper()
		var n int
		if err := env.PG.QueryRow(q, args...).Scan(&n); err != nil {
			t.Fatal(err)
		}
		return n
	}
	column := func(name string) bool {
		return count(`SELECT count(*) FROM information_schema.columns WHERE table_name = 'polls' AND column_name = $1`, name) == 1
	}
	board := uuid.New()
	insert := func(boardID any, frame any, session any) error {
		_, err := env.PG.Exec(`INSERT INTO polls (id, channel_id, question, options, created_by, board_uuid, frame_id, session_id)
			VALUES ($1, $2, 'Which day?', '[]', $3, $4, $5, $6)`, uuid.New(), uuid.Nil, uuid.New(), boardID, frame, session)
		return err
	}
	if err := insert(board, "intro", nil); err != nil {
		t.Fatalf("a board's poll on a frame: %v", err)
	}
	if _, err := env.PG.Exec(migrationSQL(t, "board_polls", "down")); err != nil {
		t.Fatalf("down: %v", err)
	}
	if column("board_uuid") || column("frame_id") || count(`SELECT count(*) FROM pg_indexes WHERE indexname = 'polls_board_idx'`) != 0 {
		t.Fatal("the board's owner of a poll is still there after down")
	}
	if count(`SELECT count(*) FROM polls`) != 0 {
		t.Fatal("a board's poll outlived the column that said whose it is")
	}
	if _, err := env.PG.Exec(migrationSQL(t, "board_polls", "up")); err != nil {
		t.Fatalf("up again: %v", err)
	}
	if count(`SELECT count(*) FROM pg_indexes WHERE indexname = 'polls_board_idx'`) != 1 {
		t.Fatal("no index on a poll's board")
	}
	if err := insert(board, "intro", nil); err != nil {
		t.Fatalf("a board's poll on a frame after up: %v", err)
	}
	if insert(board, nil, nil) == nil {
		t.Error("a board's poll with no frame")
	}
	if insert(nil, "intro", nil) == nil {
		t.Error("a frame with no board")
	}
	if insert(board, strings.Repeat("f", 129), nil) == nil {
		t.Error("a frame id of 129 characters")
	}
	session := uuid.New()
	if _, err := env.PG.Exec(`INSERT INTO call_sessions (id, room, kind, resource_kind, resource_id, started_at) VALUES ($1, 'room-b', 'huddle', 'channel', 'room-b', NOW())`, session); err != nil {
		t.Fatal(err)
	}
	if insert(board, "intro", session) == nil {
		t.Error("a poll both a call's and a board's")
	}
	if _, err := env.PG.Exec(`INSERT INTO polls (id, channel_id, question, options, created_by, board_uuid, frame_id) VALUES ($1, $2, 'Q?', '[]', $3, $4, 'intro')`,
		uuid.New(), uuid.New(), uuid.New(), board); err == nil {
		t.Error("a board's poll in a channel")
	}
}

func TestBoardPollsReachOnlyWhoMayOpenTheBoard(t *testing.T) {
	env, dg := liveSessionStores(t)
	rec := integration.RecordMqtt(t)
	t.Setenv("CSRF_DISABLED", "true")
	ctx := context.Background()
	people := livePeople(t, env, "owner", "editor", "viewer", "commenter", "stranger")

	intro := scene.Frame("intro", "Intro", 0, 0, 400, 300, 1)
	vote := scene.Frame("vote", "Vote", 500, 0, 400, 300, 1)
	state, _, err := scene.State(5, []scene.Element{intro, vote}, nil, nil)
	if err != nil {
		t.Fatal(err)
	}
	board, open := uuid.NewString(), uuid.NewString()
	uids := dg.Mutate(t, []map[string]any{
		{"uid": "_:owner", "dgraph.type": "User", "user_uuid": people["owner"].String(), "user_name": "owner"},
		{"uid": "_:editor", "dgraph.type": "User", "user_uuid": people["editor"].String(), "user_name": "editor"},
		{"uid": "_:viewer", "dgraph.type": "User", "user_uuid": people["viewer"].String(), "user_name": "viewer"},
		{"uid": "_:commenter", "dgraph.type": "User", "user_uuid": people["commenter"].String(), "user_name": "commenter"},
		{"uid": "_:stranger", "dgraph.type": "User", "user_uuid": people["stranger"].String(), "user_name": "stranger"},
		{"uid": "_:b", "dgraph.type": "Board", "board_uuid": board, "board_title": "Quarterly review", "board_private": true,
			"board_deleted_at": "0001-01-01T00:00:00Z", "board_created_by": map[string]any{"uid": "_:owner"}, "board_state": state,
			"board_editing_users":    []map[string]any{{"uid": "_:editor"}},
			"board_reading_users":    []map[string]any{{"uid": "_:viewer"}},
			"board_commenting_users": []map[string]any{{"uid": "_:commenter"}}},
		// A board everyone may open, with the same frame ids: a poll's frame
		// is the one on its own board.
		{"uid": "_:o", "dgraph.type": "Board", "board_uuid": open, "board_title": "Open", "board_private": false,
			"board_deleted_at": "0001-01-01T00:00:00Z", "board_created_by": map[string]any{"uid": "_:owner"}, "board_state": state},
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
	type option struct {
		ID    string `json:"id"`
		Text  string `json:"text"`
		Votes int    `json:"votes"`
	}
	type poll struct {
		ID        string   `json:"id"`
		Channel   string   `json:"channel_uuid"`
		Board     string   `json:"board_uuid"`
		Frame     string   `json:"frame_id"`
		Question  string   `json:"question"`
		Multiple  bool     `json:"multiple"`
		Options   []option `json:"options"`
		Voters    int      `json:"voters"`
		Mine      []string `json:"mine"`
		Closed    bool     `json:"closed"`
		CanClose  bool     `json:"can_close"`
		CreatedBy string   `json:"created_by"`
	}
	type answer struct {
		Code string `json:"code"`
		Data poll   `json:"data"`
	}
	as := func(who, method, path, body string) (int, answer) {
		t.Helper()
		code, out := call(session(who), true, method, path, body)
		var a answer
		_ = json.Unmarshal([]byte(out), &a)
		return code, a
	}
	start := func(who, onBoard, frame string, multiple bool) (int, answer) {
		t.Helper()
		in, _ := json.Marshal(map[string]any{"board_uuid": onBoard, "frame_id": frame, "question": "Ship on Friday?",
			"options": []string{"Yes", "No", "Next week"}, "multiple": multiple})
		return as(who, http.MethodPost, "/board/polls", string(in))
	}
	list := func(who string) (int, []poll) {
		t.Helper()
		code, out := call(session(who), true, http.MethodGet, "/board/polls/"+board, "")
		var got struct {
			Data []poll `json:"data"`
		}
		if code == http.StatusOK {
			if err := json.Unmarshal([]byte(out), &got); err != nil {
				t.Fatalf("%s's list: %v in %s", who, err, out)
			}
		}
		return code, got.Data
	}
	votes := func(pollID string) int {
		t.Helper()
		n, err := pgexec.Scalar[int](ctx, `SELECT count(*) FROM poll_votes WHERE poll_id = $1`, pollID)
		if err != nil {
			t.Fatal(err)
		}
		return n
	}
	told := func(pollID string, kind int8) int {
		t.Helper()
		n := 0
		for _, raw := range rec.Messages(helpers.GetMqttTopicForBoard(board), mqttStruct.MESSAGE_POLL_UPDATE) {
			var m struct {
				Type  int8   `json:"type"`
				Poll  string `json:"poll_uuid"`
				Board string `json:"board_uuid"`
				Frame string `json:"frame_id"`
			}
			if json.Unmarshal(raw, &m) == nil && m.Poll == pollID && m.Board == board && m.Frame == "intro" && m.Type == kind {
				n++
			}
		}
		return n
	}
	// told waits for a publish made from a goroutine the handler didn't wait for.
	eventually := func(what string, ok func() bool) {
		t.Helper()
		for end := time.Now().Add(5 * time.Second); !ok(); time.Sleep(20 * time.Millisecond) {
			if time.Now().After(end) {
				t.Fatalf("never: %s", what)
			}
		}
	}

	// Editors start a poll on a frame of the board: the owner and who it's
	// shared with to edit. A viewer and a commenter are refused, a member the
	// private board is hidden from is told it isn't there.
	code, a := start("editor", board, "intro", false)
	if code != http.StatusCreated || a.Data.ID == "" || a.Data.Board != board || a.Data.Frame != "intro" || a.Data.Channel != "" ||
		!a.Data.CanClose || len(a.Data.Options) != 3 || a.Data.Closed {
		t.Fatalf("the editor's poll on the intro answers %d %+v", code, a)
	}
	id := a.Data.ID
	eventually("the board's topic told of the new poll", func() bool { return told(id, mqttStruct.TYPE_CREATE) == 1 })
	if code, _ := start("owner", board, "vote", true); code != http.StatusCreated {
		t.Fatalf("the owner's poll on the vote frame answers %d", code)
	}
	for _, who := range []string{"viewer", "commenter"} {
		if code, _ := start(who, board, "intro", false); code != http.StatusForbidden {
			t.Errorf("a %s starting a poll answers %d, want 403", who, code)
		}
	}
	if code, _ := start("stranger", board, "intro", false); code != http.StatusNotFound {
		t.Errorf("a member the board is hidden from starting a poll answers %d, want 404", code)
	}
	// The frame is the board's own: one its stored document doesn't hold is
	// refused, and so is a board that isn't one.
	if code, a := start("editor", board, "elsewhere", false); code != http.StatusConflict || a.Code != "frame_not_on_board" {
		t.Errorf("a poll on a frame not on the board answers %d %+v, want 409 frame_not_on_board", code, a)
	}
	if code, _ := start("editor", uuid.NewString(), "intro", false); code != http.StatusNotFound {
		t.Errorf("a poll on a board that doesn't exist answers %d, want 404", code)
	}
	if code, _ := start("editor", uids["editor"], "intro", false); code != http.StatusNotFound {
		t.Errorf("a poll on a Dgraph uid that isn't a board's uuid answers %d, want 404", code)
	}

	// Whoever may open the board reads its polls, by the board and by id; a
	// member it is hidden from reads neither.
	for _, who := range []string{"owner", "editor", "viewer", "commenter"} {
		code, polls := list(who)
		if code != http.StatusOK || len(polls) != 2 || polls[0].ID != id || polls[0].Frame != "intro" || polls[1].Frame != "vote" {
			t.Fatalf("%s lists the board's polls: %d %+v", who, code, polls)
		}
		for _, p := range polls {
			if p.CanClose != (who == "owner" || who == "editor") || len(p.Mine) != 0 {
				t.Errorf("%s's list has poll %s with can_close %v and mine %v; want can_close only for who may edit, no choices yet", who, p.ID, p.CanClose, p.Mine)
			}
		}
		if code, a := as(who, http.MethodGet, "/poll/"+id, ""); code != http.StatusOK || a.Data.Question != "Ship on Friday?" ||
			a.Data.CanClose != (who == "owner" || who == "editor") {
			t.Errorf("%s reads the poll by id: %d %+v", who, code, a)
		}
	}
	if code, polls := list("stranger"); code != http.StatusNotFound || len(polls) != 0 {
		t.Errorf("a member the board is hidden from lists its polls: %d %+v, want 404", code, polls)
	}
	if code, out := call(session("stranger"), true, http.MethodGet, "/poll/"+id, ""); code != http.StatusNotFound || strings.Contains(out, "Ship on") {
		t.Errorf("a member the board is hidden from reads its poll by id: %d %s, want 404", code, out)
	}
	if code, _ := as("stranger", http.MethodPost, "/poll/"+id+"/vote", `{"option_ids":["1"]}`); code != http.StatusNotFound || votes(id) != 0 {
		t.Errorf("a member the board is hidden from votes: %d, %d votes kept; want 404 and none", code, votes(id))
	}

	// Votes: once each (a second replaces the first), one option unless the
	// poll takes several; the tally moves for everyone, and the board's own
	// topic says so, never a channel's.
	if code, a := as("viewer", http.MethodPost, "/poll/"+id+"/vote", `{"option_ids":["1"]}`); code != http.StatusOK || a.Data.Voters != 1 ||
		len(a.Data.Mine) != 1 || a.Data.Mine[0] != "1" || a.Data.Options[0].Votes != 1 {
		t.Fatalf("the viewer's vote answers %d %+v", code, a)
	}
	// The board's list says each person's own choices: the viewer's, and none
	// for the editor, who hasn't voted.
	if _, polls := list("viewer"); len(polls) != 2 || len(polls[0].Mine) != 1 || polls[0].Mine[0] != "1" || polls[0].Voters != 1 {
		t.Errorf("the viewer's list after their vote: %+v, want mine [1] and 1 voter on the first poll", polls)
	}
	if _, polls := list("editor"); len(polls) != 2 || len(polls[0].Mine) != 0 || polls[0].Voters != 1 {
		t.Errorf("the editor's list after the viewer's vote: %+v, want none of theirs and 1 voter", polls)
	}
	if code, _ := as("viewer", http.MethodPost, "/poll/"+id+"/vote", `{"option_ids":["1","2"]}`); code != http.StatusBadRequest {
		t.Errorf("two options in a one-choice poll answer %d, want 400", code)
	}
	if code, a := as("commenter", http.MethodPost, "/poll/"+id+"/vote", `{"option_ids":["2"]}`); code != http.StatusOK || a.Data.Voters != 2 {
		t.Fatalf("the commenter's vote answers %d %+v", code, a)
	}
	if code, a := as("viewer", http.MethodPost, "/poll/"+id+"/vote", `{"option_ids":["2"]}`); code != http.StatusOK || a.Data.Voters != 2 ||
		a.Data.Options[0].Votes != 0 || a.Data.Options[1].Votes != 2 {
		t.Fatalf("the viewer's second vote answers %d %+v, want it to replace the first", code, a)
	}
	eventually("the board's topic told of each vote", func() bool { return told(id, mqttStruct.TYPE_UPDATE) >= 3 })
	for _, p := range rec.All() {
		if strings.Contains(string(p.Payload), id) && p.Topic != helpers.GetMqttTopicForBoard(board) {
			t.Errorf("the board's poll was told on %s, not the board's topic", p.Topic)
		}
	}
	_, polls := list("viewer")
	multi := polls[1].ID
	if code, a := as("viewer", http.MethodPost, "/poll/"+multi+"/vote", `{"option_ids":["1","3"]}`); code != http.StatusOK || a.Data.Voters != 1 ||
		len(a.Data.Mine) != 2 {
		t.Errorf("two options in a poll that takes several answer %d %+v", code, a)
	}

	// A share-link guest reads the question and the tally, never whose
	// votes, and never votes: not through the member route with their
	// collab token, not through the guest route.
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
	guestRead := func() (int, []poll) {
		t.Helper()
		code, out := call("", false, http.MethodGet, "/guest/board-polls/"+grant.Token, "")
		var got struct {
			Data struct {
				Polls []poll `json:"polls"`
			} `json:"data"`
		}
		if code == http.StatusOK {
			if err := json.Unmarshal([]byte(out), &got); err != nil {
				t.Fatalf("the guest's read: %v in %s", err, out)
			}
		}
		return code, got.Data.Polls
	}
	code, guestPolls := guestRead()
	if code != http.StatusOK || len(guestPolls) != 2 || guestPolls[0].Question != "Ship on Friday?" || guestPolls[0].Voters != 2 ||
		guestPolls[0].Options[1].Votes != 2 || len(guestPolls[0].Mine) != 0 || guestPolls[0].CanClose || guestPolls[0].CreatedBy != "" {
		t.Fatalf("the share-link guest reads %d %+v, want the question and tally only", code, guestPolls)
	}
	for _, cookie := range []bool{true, false} {
		if code, _ := call(linkGuest, cookie, http.MethodPost, "/poll/"+id+"/vote", `{"option_ids":["3"]}`); code != http.StatusUnauthorized {
			t.Errorf("a share-link guest voting (cookie %v) answers %d, want 401", cookie, code)
		}
		if code, _ := call(linkGuest, cookie, http.MethodGet, "/poll/"+id, ""); code != http.StatusUnauthorized {
			t.Errorf("a share-link guest reading by id (cookie %v) answers %d, want 401", cookie, code)
		}
		if code, _ := call(linkGuest, cookie, http.MethodPost, "/board/polls", `{"board_uuid":"`+board+`","frame_id":"intro","question":"Q?","options":["a","b"]}`); code != http.StatusUnauthorized {
			t.Errorf("a share-link guest starting a poll (cookie %v) answers %d, want 401", cookie, code)
		}
	}
	if code, _ := call("", false, http.MethodPost, "/guest/board-polls/"+grant.Token, `{"poll_id":"`+id+`","option_ids":["3"]}`); code != http.StatusMethodNotAllowed {
		t.Errorf("a vote through the guest's read answers %d, want 405", code)
	}
	if votes(id) != 2 {
		t.Fatalf("after the guest's tries the poll has %d votes, want 2", votes(id))
	}
	if code, _ := call("", false, http.MethodGet, "/guest/board-polls/not-a-link", ""); code != http.StatusNotFound {
		t.Errorf("a link that isn't one reads %d, want 404", code)
	}

	// Closing: who may edit the board, nobody else; a closed poll takes no
	// vote, and its close is told on the board's topic.
	if code, _ := as("viewer", http.MethodPost, "/poll/"+id+"/close", ""); code != http.StatusForbidden {
		t.Errorf("a viewer closing the poll answers %d, want 403", code)
	}
	if code, _ := as("stranger", http.MethodPost, "/poll/"+id+"/close", ""); code != http.StatusNotFound {
		t.Errorf("a member the board is hidden from closing the poll answers %d, want 404", code)
	}
	before := told(id, mqttStruct.TYPE_UPDATE)
	if code, a := as("owner", http.MethodPost, "/poll/"+id+"/close", ""); code != http.StatusOK || !a.Data.Closed {
		t.Fatalf("the owner closing the editor's poll answers %d %+v", code, a)
	}
	eventually("the board's topic told of the close", func() bool { return told(id, mqttStruct.TYPE_UPDATE) > before })
	if code, _ := as("commenter", http.MethodPost, "/poll/"+id+"/vote", `{"option_ids":["3"]}`); code != http.StatusBadRequest {
		t.Errorf("a vote on a closed poll answers %d, want 400", code)
	}
	// There is no delete: a poll closes instead.
	if code, _ := as("owner", http.MethodDelete, "/poll/"+id, ""); code != http.StatusMethodNotAllowed {
		t.Errorf("deleting a poll answers %d, want 405", code)
	}

	// The poll belongs to its board: the open board, with the same frame ids,
	// lists none of it.
	if code, out := call(session("stranger"), true, http.MethodGet, "/board/polls/"+open, ""); code != http.StatusOK || strings.Contains(out, id) {
		t.Errorf("the open board lists %d %s, want none of the private board's polls", code, out)
	}

	// A board holds at most MaxBoardPolls polls, counted on that board alone:
	// at the cap the next is refused and nothing is stored, while another
	// board still takes one.
	onBoard := func(id string) int {
		t.Helper()
		var n int
		if err := env.PG.QueryRow(`SELECT count(*) FROM polls WHERE board_uuid = $1`, id).Scan(&n); err != nil {
			t.Fatal(err)
		}
		return n
	}
	for n := onBoard(board); n < pollBusiness.MaxBoardPolls; n++ {
		if _, err := env.PG.Exec(`INSERT INTO polls (id, channel_id, question, options, created_by, board_uuid, frame_id)
			VALUES ($1, $2, 'Lunch?', '[]', $3, $4, 'intro')`, uuid.New(), uuid.Nil, people["owner"], board); err != nil {
			t.Fatal(err)
		}
	}
	if code, _ := start("editor", board, "intro", false); code != http.StatusBadRequest || onBoard(board) != pollBusiness.MaxBoardPolls {
		t.Errorf("a poll past the board's %d answers %d with %d stored, want 400 and none added", pollBusiness.MaxBoardPolls, code, onBoard(board))
	}
	if code, _ := start("owner", open, "intro", false); code != http.StatusCreated || onBoard(open) != 1 {
		t.Errorf("a poll on another board answers %d with %d stored, want 201: the cap is per board", code, onBoard(open))
	}

	// A board whose stored document can't be read can't say which frames it
	// holds: a poll on it answers 503 (try again), never 403, 409 or a stored
	// poll.
	unreadable := uuid.NewString()
	dg.Mutate(t, []map[string]any{{"uid": "_:u", "dgraph.type": "Board", "board_uuid": unreadable, "board_title": "Unreadable",
		"board_private": true, "board_deleted_at": "0001-01-01T00:00:00Z", "board_created_by": map[string]any{"uid": uids["owner"]},
		"board_state": "not a board's document"}})
	if code, _ := start("owner", unreadable, "intro", false); code != http.StatusServiceUnavailable || onBoard(unreadable) != 0 {
		t.Errorf("a poll on a board whose document can't be read answers %d with %d stored, want 503 and none", code, onBoard(unreadable))
	}

	// A deleted board's polls read as gone, to members and its link's guest.
	dg.Mutate(t, []map[string]any{{"uid": uids["b"], "board_deleted_at": time.Now().UTC().Format(time.RFC3339)}})
	if code, _ := list("owner"); code != http.StatusNotFound {
		t.Errorf("the owner lists a deleted board's polls: %d, want 404", code)
	}
	for _, who := range []string{"owner", "viewer"} {
		if code, _ := as(who, http.MethodGet, "/poll/"+multi, ""); code != http.StatusNotFound {
			t.Errorf("%s reads a deleted board's poll: %d, want 404", who, code)
		}
		if code, _ := as(who, http.MethodPost, "/poll/"+multi+"/vote", `{"option_ids":["2"]}`); code != http.StatusNotFound {
			t.Errorf("%s votes on a deleted board's poll: %d, want 404", who, code)
		}
	}
	if code, _ := guestRead(); code != http.StatusNotFound {
		t.Errorf("the link's guest reads a deleted board's polls: %d, want 404", code)
	}
}
