//go:build integration

package integration_test

// The board on a call's stage (calls package B), on Postgres 12, a real call
// server and graph: its migration goes down and up; one board at a time, put
// there and taken off atomically, each write answering the board it replaced;
// the two drawing switches each set their own column; a board's live calls
// are the ones its person hasn't left; and the grant a call gives on a board
// (BoardCollabAuthorize) holds while its person is in the call and ends on
// every way out of it: a leave, an end for everyone, and the sweep's.

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"
	lksdk "github.com/livekit/server-sdk-go/v2"

	boardBusiness "github.com/akashc777/OneCamp/business/Board"
	callControlBusiness "github.com/akashc777/OneCamp/business/CallControl"
	callStateBusiness "github.com/akashc777/OneCamp/business/CallState"
	liveKitBusiness "github.com/akashc777/OneCamp/business/LiveKit"
	boardController "github.com/akashc777/OneCamp/controllers/Board"
	"github.com/akashc777/OneCamp/helpers"
	"github.com/akashc777/OneCamp/initializers/livekitInit"
	"github.com/akashc777/OneCamp/initializers/postgresInit"
	dgraphStruct "github.com/akashc777/OneCamp/models/dgraph"
	snapshotModels "github.com/akashc777/OneCamp/models/postgres/BoardSnapshot"
	callStateModel "github.com/akashc777/OneCamp/models/postgres/CallState"
	userModels "github.com/akashc777/OneCamp/models/postgres/User"
	collab "github.com/akashc777/OneCamp/services/collab"
	"github.com/akashc777/OneCamp/tests/integration"
)

func TestCallBoardMigrationUpDownUp(t *testing.T) {
	env := integration.SetupEnv(t)
	column := func(name string) bool {
		var n int
		if err := env.PG.QueryRow(`SELECT count(*) FROM information_schema.columns WHERE table_name = 'call_sessions' AND column_name = $1`, name).Scan(&n); err != nil {
			t.Fatal(err)
		}
		return n == 1
	}
	if _, err := env.PG.Exec(migrationSQL(t, "call_board", "down")); err != nil {
		t.Fatalf("down: %v", err)
	}
	if column("board_uuid") || column("board_guests_draw") {
		t.Fatal("the board's columns are still there after down")
	}
	if _, err := env.PG.Exec(migrationSQL(t, "call_board", "up")); err != nil {
		t.Fatalf("up again: %v", err)
	}
	if _, err := env.PG.Exec(migrationSQL(t, "call_board", "up")); err != nil {
		t.Fatalf("up twice: %v", err)
	}
	session := uuid.New()
	if _, err := env.PG.Exec(`INSERT INTO call_sessions (id, room, kind, resource_kind, resource_id, started_at, board_uuid, board_draw) VALUES ($1, 'room-b', 'huddle', 'channel', 'room-b', NOW(), $2, 'presenter')`, session, uuid.New()); err != nil {
		t.Fatalf("a board only its presenter draws on: %v", err)
	}
	if _, err := env.PG.Exec(`UPDATE call_sessions SET board_draw = 'nobody' WHERE id = $1`, session); err == nil {
		t.Error("a way of drawing the call doesn't know was stored")
	}
	if _, err := env.PG.Exec(`INSERT INTO board_snapshots (board_uuid, object_key, element_count, reason) VALUES ($1, 'k', 0, 'guests')`, uuid.New()); err != nil {
		t.Errorf("the guests' snapshot reason: %v", err)
	}
}

// One board on a stage at a time; each write answers the board it replaced or
// took off, read under the row's lock; the two switches each keep their own
// column; a board's stages are its people's live calls only; and a call keeps
// the boards it showed in the order it first did.
func TestCallBoardStateOnPostgres12(t *testing.T) {
	ctx := context.Background()
	pg := integration.SetupEnv(t)
	if err := postgresInit.ConnectPostgres(ctx, pg.DSN); err != nil {
		t.Fatal(err)
	}
	session, member := uuid.New(), uuid.New()
	if _, err := pg.PG.Exec(`INSERT INTO call_sessions (id, room, kind, resource_kind, resource_id, started_at) VALUES ($1, 'room-s', 'huddle', 'channel', 'room-s', NOW())`, session); err != nil {
		t.Fatal(err)
	}
	if _, err := pg.PG.Exec(`INSERT INTO call_attendance (session_id, identity, user_uuid, name, role, joined_at, last_joined_at) VALUES
		($1, '0xa', $2, 'Ann', 'participant', NOW(), NOW()), ($1, 'guest-g', NULL, 'Guest: Gil', 'guest', NOW(), NOW()),
		($1, '0xc', NULL, 'Cy', 'participant', NOW(), NOW())`, session, member); err != nil {
		t.Fatal(err)
	}
	if _, err := pg.PG.Exec(`UPDATE call_attendance SET left_at = NOW() WHERE session_id = $1 AND identity = '0xc'`, session); err != nil {
		t.Fatal(err)
	}
	first, second := uuid.New(), uuid.New()

	if on, was, err := callStateModel.SetBoard(ctx, session, first, "0xa", false); err != nil || !on || was != uuid.Nil {
		t.Fatalf("Ann presents to an empty stage: %v %v %v", on, was, err)
	}
	if on, _, err := callStateModel.SetBoard(ctx, session, second, "0xb", false); err != nil || on {
		t.Fatalf("someone else presented over Ann without taking over: %v %v", on, err)
	}
	if on, was, err := callStateModel.SetBoard(ctx, session, second, "0xh", true); err != nil || !on || was != first {
		t.Fatalf("a host takes over: on %v, replaced %v (want %v), %v", on, was, first, err)
	}

	// One version from before guests drew, per board's time on the stage: the
	// first "Guests can draw" keeps it, a later one doesn't (SF5).
	if first, err := callStateModel.MarkGuestsKept(ctx, session); err != nil || !first {
		t.Fatalf("the first keep: %v %v", first, err)
	}
	if again, err := callStateModel.MarkGuestsKept(ctx, session); err != nil || again {
		t.Fatalf("guests switched off and on again kept another version: %v %v", again, err)
	}
	if kept, err := callStateModel.GuestsKept(ctx, session); err != nil || !kept {
		t.Fatalf("the stage has its version: %v %v", kept, err)
	}

	presenter := callStateModel.DrawPresenter
	yes := true
	if b, on, err := callStateModel.SetBoardDraw(ctx, session, &presenter, nil); err != nil || !on || b.Draw != presenter || b.GuestsDraw {
		t.Fatalf("only the presenter draws: %+v %v %v", b, on, err)
	}
	if b, _, err := callStateModel.SetBoardDraw(ctx, session, nil, &yes); err != nil || b.Draw != presenter || !b.GuestsDraw || b.UUID != second {
		t.Fatalf("guests can draw kept who draws: %+v %v", b, err)
	}

	stages, err := callStateModel.BoardStagesOf(ctx, second, "guest-g")
	if err != nil || len(stages) != 1 || !stages[0].Guest || stages[0].Identity != "guest-g" {
		t.Fatalf("the guest's stage: %+v %v", stages, err)
	}
	if stages, err = callStateModel.BoardStagesOfMember(ctx, second, member); err != nil || len(stages) != 1 || stages[0].Identity != "0xa" {
		t.Fatalf("Ann's stage by her user uuid: %+v %v", stages, err)
	}
	if stages, err = callStateModel.BoardStagesOf(ctx, second, "0xc"); err != nil || len(stages) != 0 {
		t.Fatalf("Cy left, and still has the stage: %+v %v", stages, err)
	}

	if was, err := callStateModel.ClearBoard(ctx, session, ""); err != nil || was != second {
		t.Fatalf("the stop took off %v, want %v (%v)", was, second, err)
	}
	if was, err := callStateModel.ClearBoard(ctx, session, ""); err != nil || was != uuid.Nil {
		t.Fatalf("a second stop took off %v (%v)", was, err)
	}
	if kept, err := callStateModel.GuestsKept(ctx, session); err != nil || kept {
		t.Fatalf("a board off the stage still counts its guests' version: %v %v", kept, err)
	}
	if _, on, err := callStateModel.SetBoardDraw(ctx, session, &presenter, nil); err != nil || on {
		t.Fatalf("a switch with no board on the stage: %v %v", on, err)
	}
	if stages, err = callStateModel.BoardStagesOf(ctx, second, "guest-g"); err != nil || len(stages) != 0 {
		t.Fatalf("a board off the stage still has stages: %+v %v", stages, err)
	}
	shown, err := callStateModel.BoardsShown(ctx, session)
	if err != nil || len(shown) != 2 || shown[0] != first || shown[1] != second {
		t.Fatalf("the boards shown: %v %v", shown, err)
	}
}

// A member with no rights on a board reads and draws on it while it is on the
// stage of a call they are in, never through an API token, and not once they
// leave or the call ends, by any path: each closes the board's sockets.
func TestCallBoardGrantEndsWithTheCall(t *testing.T) {
	ctx := context.Background()
	pg := integration.SetupEnv(t)
	if err := postgresInit.ConnectPostgres(ctx, pg.DSN); err != nil {
		t.Fatal(err)
	}
	integration.SetupRedis(t)
	dg := integration.SetupDgraph(t)
	integration.StubMqttClient(t)
	lk := integration.SetupLiveKit(t)
	live := recordLiveOps(t)

	room := uuid.NewString()
	people := seedChannel(t, pg, dg, room, "sam", "ana", "outsider")
	aBoard := func(t *testing.T) string {
		t.Helper()
		b := uuid.NewString()
		dg.Mutate(t, []map[string]any{
			{"uid": "_:b", "dgraph.type": "Board", "board_uuid": b, "board_title": "Sam's sketch", "board_private": true,
				"board_deleted_at": "0001-01-01T00:00:00Z", "board_created_by": map[string]any{"uid": people["sam"].Uid}},
		})
		return b
	}
	board := aBoard(t)
	// Each helper takes the running subtest's t, so a failure stops only that
	// leg and the legs after it still run.
	authorize := func(t *testing.T, name string, token bool) (int, bool) {
		t.Helper()
		code, out := callRead(t, boardController.BoardCollabAuthorize, http.MethodPost, "/boardColab/authorize", people[name], `{"board_uuid":"`+board+`"}`, "")
		if token {
			code, out = callReadThroughToken(t, boardController.BoardCollabAuthorize, people[name], `{"board_uuid":"`+board+`"}`)
		}
		return code, string(out["canEdit"]) == "true"
	}
	// call starts the call with Sam's board on its stage, and ends it when the
	// leg ends, passed or not, so the next leg starts a fresh one.
	call := func(t *testing.T) (*callStateBusiness.Session, map[string]*joined) {
		t.Helper()
		// Each leg presents a board of its own, so its count of closes holds
		// only its own: presenting's and its leg's. A close another leg set
		// going in the background can't land in it.
		board = aBoard(t)
		if _, err := livekitInit.LiveKitService.CreateRoom(ctx, room); err != nil {
			t.Fatal(err)
		}
		in := map[string]*joined{}
		t.Cleanup(func() {
			for _, j := range in {
				if j.room.ConnectionState() != lksdk.ConnectionStateDisconnected {
					j.room.Disconnect()
				}
			}
			liveKitBusiness.DeleteRoom(ctx, room)
			if err := callControlBusiness.Reconcile(ctx); err != nil {
				t.Error(err)
			}
		})
		for _, name := range []string{"sam", "ana"} {
			token, err := livekitInit.LiveKitService.GenerateToken(room, people[name].Uid, title(name), false, "", livekitInit.CallPublishSources)
			if err != nil {
				t.Fatal(err)
			}
			in[name] = join(t, lk, token)
			joinSettled(t, room, people[name].Uid, in[name])
			connected(t, ctx, room, people[name].Uid)
		}
		s, err := callStateBusiness.Current(ctx, callStateBusiness.Ref{Kind: "channel", ID: room})
		if err != nil || s == nil {
			t.Fatalf("no call recorded: %v", err)
		}
		if _, err := callControlBusiness.Act(ctx, people["sam"], s.ID, callControlBusiness.Action{Action: callControlBusiness.ActPresentBoard, Board: board}); err != nil {
			t.Fatalf("Sam presents his board: %v", err)
		}
		if !live.boardAskedAgain(board, "") {
			t.Fatal("presenting left the board's sockets as they were")
		}
		return s, in
	}
	closed := func() int {
		live.mu.Lock()
		defer live.mu.Unlock()
		n := 0
		for _, c := range live.calls {
			if c.Path == "/live/boards/"+board+"/reauth" {
				n++
			}
		}
		return n
	}
	closedAgain := func(t *testing.T, before int, what string) {
		t.Helper()
		eventually(t, what+" to close the board's sockets", func() bool { return closed() > before })
	}

	if code, _ := authorize(t, "ana", false); code != http.StatusForbidden {
		t.Fatalf("Ana opens Sam's private board outside any call: %d", code)
	}

	t.Run("a leave", func(t *testing.T) {
		_, in := call(t)
		if code, edit := authorize(t, "ana", false); code != http.StatusOK || !edit {
			t.Fatalf("Ana in the call: %d, drawing %v; want to read and draw", code, edit)
		}
		if code, _ := authorize(t, "ana", true); code != http.StatusForbidden {
			t.Fatalf("Ana's API token opens the board through her call: %d", code)
		}
		if code, _ := authorize(t, "outsider", false); code != http.StatusForbidden {
			t.Fatalf("someone outside the call opens it: %d", code)
		}
		before := closed()
		r, _ := livekitInit.LiveKitService.GetRoom(ctx, room)
		anaInfo, _ := livekitInit.LiveKitService.Participant(ctx, room, people["ana"].Uid)
		in["ana"].room.Disconnect()
		eventually(t, "Ana to be gone from LiveKit", func() bool {
			p, _ := livekitInit.LiveKitService.Participant(ctx, room, people["ana"].Uid)
			return p == nil
		})
		callControlBusiness.OnLeft(ctx, room, r.Sid, anaInfo, time.Now())
		closedAgain(t, before, "Ana's leave")
		if code, _ := authorize(t, "ana", false); code != http.StatusForbidden {
			t.Fatalf("Ana opens the board after leaving: %d", code)
		}
	})

	t.Run("an end for everyone", func(t *testing.T) {
		s, _ := call(t)
		before := closed()
		if _, err := callControlBusiness.Act(ctx, people["sam"], s.ID, callControlBusiness.Action{Action: callControlBusiness.ActEnd}); err != nil {
			t.Fatalf("Sam ends the call: %v", err)
		}
		closedAgain(t, before, "the end for everyone")
		if code, _ := authorize(t, "ana", false); code != http.StatusForbidden {
			t.Fatalf("Ana opens the board after the call ended: %d", code)
		}
	})

	t.Run("the sweep's end", func(t *testing.T) {
		_, in := call(t)
		for _, j := range in {
			j.room.Disconnect()
		}
		before := closed()
		liveKitBusiness.DeleteRoom(ctx, room) // room_finished is lost
		if err := callControlBusiness.Reconcile(ctx); err != nil {
			t.Fatal(err)
		}
		closedAgain(t, before, "the sweep's end")
		if code, _ := authorize(t, "ana", false); code != http.StatusForbidden {
			t.Fatalf("Ana opens the board after the sweep ended the call: %d", code)
		}
	})

	t.Run("a deleted board", func(t *testing.T) {
		call(t)
		if err := boardBusiness.DeleteBoard(ctx, board); err != nil {
			t.Fatal(err)
		}
		if code, _ := authorize(t, "ana", false); code == http.StatusOK {
			t.Fatal("a deleted board opens through a call")
		}
	})
}

// New board in a channel's call makes a private board, shared to edit with the
// channel's members as the graph has them, and puts it on the stage: a member
// of the channel draws on it during the call and after it ends, while a
// workspace member outside the channel never opens it. Nobody outside the
// call makes one for it, and nobody makes one through an API token.
func TestCallNewBoardIsSharedWithTheCallsConversation(t *testing.T) {
	ctx := context.Background()
	pg := integration.SetupEnv(t)
	if err := postgresInit.ConnectPostgres(ctx, pg.DSN); err != nil {
		t.Fatal(err)
	}
	integration.SetupRedis(t)
	dg := integration.SetupDgraph(t)
	integration.StubMqttClient(t)
	lk := integration.SetupLiveKit(t)
	recordLiveOps(t)

	room := uuid.NewString()
	people := seedChannel(t, pg, dg, room, "sam", "ana", "outsider")
	if _, err := livekitInit.LiveKitService.CreateRoom(ctx, room); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { liveKitBusiness.DeleteRoom(ctx, room) })
	for _, name := range []string{"sam", "ana"} {
		token, err := livekitInit.LiveKitService.GenerateToken(room, people[name].Uid, title(name), false, "", livekitInit.CallPublishSources)
		if err != nil {
			t.Fatal(err)
		}
		j := join(t, lk, token)
		t.Cleanup(j.room.Disconnect)
		joinSettled(t, room, people[name].Uid, j)
		connected(t, ctx, room, people[name].Uid)
	}
	s, err := callStateBusiness.Current(ctx, callStateBusiness.Ref{Kind: "channel", ID: room})
	if err != nil || s == nil {
		t.Fatalf("no call recorded: %v", err)
	}
	newBoard := callControlBusiness.Action{Action: callControlBusiness.ActNewBoard}

	if _, err := callControlBusiness.Act(ctx, people["outsider"], s.ID, newBoard); !errors.Is(err, callControlBusiness.ErrNotInCall) {
		t.Fatalf("someone outside the call makes a board for it: %v", err)
	}
	if _, err := callControlBusiness.Act(helpers.WithActsThroughToken(ctx), people["ana"], s.ID, newBoard); !errors.Is(err, callControlBusiness.ErrThroughToken) {
		t.Fatalf("Ana's API token makes a board for her call: %v", err)
	}
	made, err := callControlBusiness.Act(ctx, people["sam"], s.ID, newBoard)
	if err != nil || made.Board == "" {
		t.Fatalf("Sam makes a board in the call: %+v, %v", made, err)
	}
	if now, err := callStateBusiness.Get(ctx, s.ID); err != nil || now.Board == nil || now.Board.UUID != made.Board || now.Board.By != people["sam"].Uid {
		t.Fatalf("the stage after New board: %+v, %v", now, err)
	}

	authorize := func(t *testing.T, name string) (int, bool) {
		t.Helper()
		code, out := callRead(t, boardController.BoardCollabAuthorize, http.MethodPost, "/boardColab/authorize", people[name], `{"board_uuid":"`+made.Board+`"}`, "")
		return code, string(out["canEdit"]) == "true"
	}
	opens := func(t *testing.T, when string) {
		t.Helper()
		for _, name := range []string{"sam", "ana"} {
			if code, edit := authorize(t, name); code != http.StatusOK || !edit {
				t.Fatalf("%s, %s: %d, drawing %v; want to read and draw", when, name, code, edit)
			}
		}
		if code, _ := authorize(t, "outsider"); code != http.StatusForbidden {
			t.Fatalf("%s, someone outside the channel opens its board: %d", when, code)
		}
	}
	opens(t, "during the call")
	if _, err := callControlBusiness.Act(ctx, people["sam"], s.ID, callControlBusiness.Action{Action: callControlBusiness.ActEnd}); err != nil {
		t.Fatalf("Sam ends the call: %v", err)
	}
	opens(t, "after the call")
	board, err := boardBusiness.GetBasicBoardByUUID(ctx, made.Board, people["sam"].Uid)
	if err != nil || board == nil || board.IsPrivate == nil || !*board.IsPrivate {
		t.Fatalf("the call's board isn't private: %+v, %v", board, err)
	}
}

// callReadThroughToken is callRead as a member's API token acts.
func callReadThroughToken(t *testing.T, h http.HandlerFunc, who *dgraphStruct.DgraphUser, body string) (int, map[string]json.RawMessage) {
	t.Helper()
	r := httptest.NewRequest(http.MethodPost, "/boardColab/authorize", strings.NewReader(body))
	info := userModels.UserInfo{UserDgraphInfo: *who}
	ctx := helpers.WithActsThroughToken(context.WithValue(r.Context(), helpers.UserInfoContextKey, info))
	w := httptest.NewRecorder()
	h(w, r.WithContext(ctx))
	var out map[string]json.RawMessage
	_ = json.Unmarshal(w.Body.Bytes(), &out)
	return w.Code, out
}

// The version from before a call's guests drew is kept and read back, and the
// per-board cap on versions never prunes the newest one for a day (SF7), while
// the latest-version read with no reasons still sees every reason.
func TestCallBoardGuestsVersionIsKept(t *testing.T) {
	ctx := context.Background()
	env := integration.SetupEnv(t)
	if err := postgresInit.ConnectPostgres(ctx, env.DSN); err != nil {
		t.Fatal(err)
	}
	integration.SetupRedis(t)
	integration.SetupDgraph(t)
	startMinio(t)
	board := uuid.NewString()
	state := boardState(t, "undo-before.b64")

	if _, err := boardBusiness.GuestsVersion(ctx, board); !errors.Is(err, boardBusiness.ErrNoGuestsVersion) {
		t.Fatalf("a board with no guests' version: %v", err)
	}
	if err := boardBusiness.KeepForGuests(ctx, board, collab.BoardVersion{State: state, Elements: 3}); err != nil {
		t.Fatal(err)
	}
	if got, err := boardBusiness.GuestsVersion(ctx, board); err != nil || got != state {
		t.Fatalf("the guests' version read back: %d bytes, %v", len(got), err)
	}
	for i := 0; i < 5; i++ {
		if _, err := snapshotModels.CreateSnapshot(ctx, &snapshotModels.BoardSnapshot{BoardUUID: board, ObjectKey: "k" + strconv.Itoa(i), Reason: snapshotModels.ReasonInterval, ContributorUUIDs: []string{}}); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := snapshotModels.PruneBoardSnapshots(ctx, board, 2); err != nil {
		t.Fatal(err)
	}
	if latest, err := snapshotModels.GetLatestSnapshot(ctx, board, snapshotModels.ReasonGuests); err != nil || latest == nil {
		t.Fatalf("the cap pruned the guests' version: %v", err)
	}
	if latest, err := snapshotModels.GetLatestSnapshot(ctx, board); err != nil || latest == nil || latest.Reason != snapshotModels.ReasonInterval {
		t.Fatalf("the latest version of any reason: %+v %v", latest, err)
	}
	var n int
	if err := env.PG.QueryRow(`SELECT count(*) FROM board_snapshots WHERE board_uuid = $1`, board).Scan(&n); err != nil || n != 3 {
		t.Fatalf("after the cap: %d versions (%v), want the 2 newest and the guests'", n, err)
	}
}
