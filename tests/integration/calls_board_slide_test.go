//go:build integration

package integration_test

// Slides control (meet-14, Meet GM-49), on Postgres 12, a real call server and
// graph: the board on a call's stage shows everyone one of its frames as a
// slide. Its migration goes down and up and holds a slide's shape; a move
// lands only on the board it was meant for and, for a presenter who doesn't
// run the call, only while the board is still theirs; a new board, or none,
// starts on the whole board; and through the API, lower parts first, only the
// presenter and the people running the call move it, each move reaching every
// screen through the room's metadata and the audit log.

import (
	"context"
	"encoding/json"
	"net/http"
	"strings"
	"testing"

	"github.com/google/uuid"
	lksdk "github.com/livekit/server-sdk-go/v2"

	callControlBusiness "github.com/akashc777/OneCamp/business/CallControl"
	callStateBusiness "github.com/akashc777/OneCamp/business/CallState"
	liveKitBusiness "github.com/akashc777/OneCamp/business/LiveKit"
	callsController "github.com/akashc777/OneCamp/controllers/Calls"
	"github.com/akashc777/OneCamp/initializers/livekitInit"
	"github.com/akashc777/OneCamp/initializers/postgresInit"
	callStateModel "github.com/akashc777/OneCamp/models/postgres/CallState"
	"github.com/akashc777/OneCamp/tests/integration"
)

func TestCallBoardSlideMigrationUpDownUp(t *testing.T) {
	env := integration.SetupEnv(t)
	column := func() bool {
		var n int
		if err := env.PG.QueryRow(`SELECT count(*) FROM information_schema.columns WHERE table_name = 'call_sessions' AND column_name = 'board_slide'`).Scan(&n); err != nil {
			t.Fatal(err)
		}
		return n == 1
	}
	if _, err := env.PG.Exec(migrationSQL(t, "call_sessions_board_slide", "down")); err != nil {
		t.Fatalf("down: %v", err)
	}
	if column() {
		t.Fatal("board_slide is still there after down")
	}
	if _, err := env.PG.Exec(migrationSQL(t, "call_sessions_board_slide", "up")); err != nil {
		t.Fatalf("up again: %v", err)
	}
	if _, err := env.PG.Exec(migrationSQL(t, "call_sessions_board_slide", "up")); err != nil {
		t.Fatalf("up twice: %v", err)
	}
	session := uuid.New()
	if _, err := env.PG.Exec(`INSERT INTO call_sessions (id, room, kind, resource_kind, resource_id, started_at, board_uuid, board_slide)
		VALUES ($1, 'room-sl', 'huddle', 'channel', 'room-sl', NOW(), $2, 'Xk3_9-aB')`, session, uuid.New()); err != nil {
		t.Fatalf("a board on its slide: %v", err)
	}
	for _, bad := range []string{"frame 2", `f"}`, strings.Repeat("a", 65), ""} {
		if _, err := env.PG.Exec(`UPDATE call_sessions SET board_slide = $2 WHERE id = $1`, session, bad); err == nil {
			t.Errorf("a slide shaped %q was stored", bad)
		}
	}
	if _, err := env.PG.Exec(`UPDATE call_sessions SET board_uuid = NULL WHERE id = $1`, session); err == nil {
		t.Error("a slide stayed on a stage with no board")
	}
	if _, err := env.PG.Exec(`UPDATE call_sessions SET board_uuid = NULL, board_slide = NULL WHERE id = $1`, session); err != nil {
		t.Errorf("the board and its slide leave together: %v", err)
	}
}

// A move lands only on the board it was meant for, and a presenter's only
// while the board is theirs; a draw switch keeps the slide and answers it; the
// whole board is NULL; a new board on the stage, a stop and an ended call each
// leave no slide to land on.
func TestCallBoardSlideOnPostgres12(t *testing.T) {
	ctx := context.Background()
	pg := integration.SetupEnv(t)
	if err := postgresInit.ConnectPostgres(ctx, pg.DSN); err != nil {
		t.Fatal(err)
	}
	session := uuid.New()
	if _, err := pg.PG.Exec(`INSERT INTO call_sessions (id, room, kind, resource_kind, resource_id, started_at) VALUES ($1, 'room-sl', 'huddle', 'channel', 'room-sl', NOW())`, session); err != nil {
		t.Fatal(err)
	}
	slideNow := func(t *testing.T) (string, bool) {
		t.Helper()
		row, err := callStateModel.SessionByID(ctx, session)
		if err != nil || row == nil {
			t.Fatalf("the call's row: %v %v", row, err)
		}
		var null bool
		if err := pg.PG.QueryRow(`SELECT board_slide IS NULL FROM call_sessions WHERE id = $1`, session).Scan(&null); err != nil {
			t.Fatal(err)
		}
		return row.BoardSlide, null
	}
	first, second := uuid.New(), uuid.New()

	if _, on, err := callStateModel.SetBoardSlide(ctx, session, first, "", "f1"); err != nil || on {
		t.Fatalf("a move with no board on the stage: %v %v", on, err)
	}
	if on, _, err := callStateModel.SetBoard(ctx, session, first, "0xa", false); err != nil || !on {
		t.Fatalf("Ann presents: %v %v", on, err)
	}
	if b, on, err := callStateModel.SetBoardSlide(ctx, session, first, "0xa", "f1"); err != nil || !on || b.Slide != "f1" || b.UUID != first || b.By != "0xa" {
		t.Fatalf("Ann moves to f1: %+v %v %v", b, on, err)
	}
	if _, on, err := callStateModel.SetBoardSlide(ctx, session, first, "0xb", "f2"); err != nil || on {
		t.Fatalf("a presenter's move landed on Ann's board: %v %v", on, err)
	}
	if _, on, err := callStateModel.SetBoardSlide(ctx, session, second, "", "f2"); err != nil || on {
		t.Fatalf("a move meant for another board landed: %v %v", on, err)
	}
	if got, _ := slideNow(t); got != "f1" {
		t.Fatalf("the refused moves changed the slide to %q", got)
	}
	if b, on, err := callStateModel.SetBoardSlide(ctx, session, first, "", "f2"); err != nil || !on || b.Slide != "f2" {
		t.Fatalf("the people running the call move whatever is on the stage: %+v %v %v", b, on, err)
	}
	presenter := callStateModel.DrawPresenter
	if b, on, err := callStateModel.SetBoardDraw(ctx, session, &presenter, nil); err != nil || !on || b.Slide != "f2" || b.Draw != presenter {
		t.Fatalf("only the presenter draws, on the same slide: %+v %v %v", b, on, err)
	}
	if b, on, err := callStateModel.SetBoardSlide(ctx, session, first, "0xa", ""); err != nil || !on || b.Slide != "" {
		t.Fatalf("back to the whole board: %+v %v %v", b, on, err)
	}
	if got, null := slideNow(t); got != "" || !null {
		t.Fatalf("the whole board is %q (NULL %v)", got, null)
	}

	if _, on, err := callStateModel.SetBoardSlide(ctx, session, first, "0xa", "f3"); err != nil || !on {
		t.Fatalf("Ann moves to f3: %v %v", on, err)
	}
	if on, was, err := callStateModel.SetBoard(ctx, session, second, "0xh", true); err != nil || !on || was != first {
		t.Fatalf("a host takes over: %v %v %v", on, was, err)
	}
	if got, null := slideNow(t); got != "" || !null {
		t.Fatalf("the host's board opens on %q (NULL %v), not the whole board", got, null)
	}
	if _, on, err := callStateModel.SetBoardSlide(ctx, session, second, "", "f4"); err != nil || !on {
		t.Fatalf("the host moves to f4: %v %v", on, err)
	}
	if was, err := callStateModel.ClearBoard(ctx, session, ""); err != nil || was != second {
		t.Fatalf("the stop took off %v (%v)", was, err)
	}
	if got, null := slideNow(t); got != "" || !null {
		t.Fatalf("a stopped board left the slide %q (NULL %v)", got, null)
	}
	if on, _, err := callStateModel.SetBoard(ctx, session, second, "0xh", true); err != nil || !on {
		t.Fatalf("the host presents again: %v %v", on, err)
	}
	if _, err := pg.PG.Exec(`UPDATE call_sessions SET ended_at = NOW() WHERE id = $1`, session); err != nil {
		t.Fatal(err)
	}
	if _, on, err := callStateModel.SetBoardSlide(ctx, session, second, "", "f5"); err != nil || on {
		t.Fatalf("a move in an ended call: %v %v", on, err)
	}
}

// Through the API, lower parts first: someone outside the call, an API token
// and a participant who isn't presenting are refused, and a slide that isn't
// a frame's id is refused before anything is written. The presenter's and a
// host's moves reach every screen through the room's metadata and are audited
// with the frame; a draw switch keeps the slide; the whole board names none; a
// host's board over the presenter's starts on the whole board, after which
// the old presenter moves nothing; and with no board there is nothing to move.
func TestCallSlidesMoveForEveryoneInTheCall(t *testing.T) {
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
	people := seedChannel(t, pg, dg, room, "maya", "sam", "ana", "outsider")
	boardOf := func(maker string) string {
		b := uuid.NewString()
		dg.Mutate(t, []map[string]any{
			{"uid": "_:b", "dgraph.type": "Board", "board_uuid": b, "board_title": "Q4 review", "board_private": true,
				"board_deleted_at": "0001-01-01T00:00:00Z", "board_created_by": map[string]any{"uid": people[maker].Uid}},
		})
		return b
	}
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
	for _, name := range []string{"maya", "sam", "ana"} { // Maya starts it, so hosts it
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
	samsBoard := boardOf("sam")
	if _, err := callControlBusiness.Act(ctx, people["sam"], s.ID, callControlBusiness.Action{Action: callControlBusiness.ActPresentBoard, Board: samsBoard}); err != nil {
		t.Fatalf("Sam presents his board: %v", err)
	}

	move := func(name, slide string) (int, string) {
		t.Helper()
		body, _ := json.Marshal(map[string]string{"action": callControlBusiness.ActBoardSlide, "slide": slide})
		code, out := callRead(t, callsController.ActInCall, http.MethodPost, "/calls/"+s.ID+"/act", people[name], string(body), s.ID)
		var refusal string
		_ = json.Unmarshal(out["code"], &refusal)
		return code, refusal
	}
	// stage is the board every screen reads from the room's metadata.
	stage := func() *callStateBusiness.Board {
		r, err := livekitInit.LiveKitService.GetRoom(ctx, room)
		if err != nil || r == nil {
			return nil
		}
		var meta map[string]json.RawMessage
		if json.Unmarshal([]byte(r.Metadata), &meta) != nil || len(meta["board"]) == 0 || string(meta["board"]) == "null" {
			return nil
		}
		var b callStateBusiness.Board
		if json.Unmarshal(meta["board"], &b) != nil {
			return nil
		}
		return &b
	}
	showing := func(board, slide string) func() bool {
		return func() bool { b := stage(); return b != nil && b.UUID == board && b.Slide == slide }
	}

	if code, refusal := move("outsider", "frame-2"); code != http.StatusForbidden || refusal != "not_in_call" {
		t.Errorf("someone outside the call moved the slides: %d %s", code, refusal)
	}
	body := `{"action":"board_slide","slide":"frame-2"}`
	if code, out := callReadThroughToken(t, callsController.ActInCall, people["sam"], body); code != http.StatusForbidden || !strings.Contains(string(out["code"]), "not_through_token") {
		t.Errorf("an API token moved the slides: %d %s", code, out["code"])
	}
	if code, refusal := move("ana", "frame-2"); code != http.StatusForbidden || refusal != "not_allowed" {
		t.Errorf("a participant moved Sam's slides: %d %s", code, refusal)
	}
	if code, refusal := move("sam", "frame 2"); code != http.StatusBadRequest || refusal != "no_such_slide" {
		t.Errorf("a slide that isn't a frame's id: %d %s", code, refusal)
	}
	if b := stage(); b == nil || b.UUID != samsBoard || b.Slide != "" {
		t.Fatalf("a refused move reached the stage: %+v", b)
	}

	if code, refusal := move("sam", "frame-2"); code != http.StatusOK {
		t.Fatalf("Sam moves to frame-2: %d %s", code, refusal)
	}
	eventually(t, "every screen to show frame-2", showing(samsBoard, "frame-2"))
	eventually(t, "the audit log to keep Sam's move with its frame", func() bool {
		var n int
		err := pg.PG.QueryRow(`SELECT count(*) FROM admin_audit_log WHERE action = 'call.board_slide' AND metadata->>'slide' = 'frame-2'`).Scan(&n)
		return err == nil && n == 1
	})
	if code, refusal := move("maya", "frame-3"); code != http.StatusOK {
		t.Fatalf("Maya, who hosts, moves to frame-3: %d %s", code, refusal)
	}
	eventually(t, "every screen to show frame-3", showing(samsBoard, "frame-3"))
	if _, err := callControlBusiness.Act(ctx, people["sam"], s.ID, callControlBusiness.Action{Action: callControlBusiness.ActBoardDraw, Draw: callStateBusiness.DrawPresenter}); err != nil {
		t.Fatalf("Sam keeps the drawing to himself: %v", err)
	}
	eventually(t, "only Sam drawing, still on frame-3", func() bool {
		b := stage()
		return b != nil && b.Draw == callStateBusiness.DrawPresenter && b.Slide == "frame-3"
	})
	if code, refusal := move("sam", ""); code != http.StatusOK {
		t.Fatalf("Sam goes back to the whole board: %d %s", code, refusal)
	}
	eventually(t, "every screen to show the whole board", showing(samsBoard, ""))
	if r, err := livekitInit.LiveKitService.GetRoom(ctx, room); err != nil || strings.Contains(r.Metadata, `"slide"`) {
		t.Errorf("the whole board's metadata names a slide: %v %v", r, err)
	}

	if code, refusal := move("sam", "frame-4"); code != http.StatusOK {
		t.Fatalf("Sam moves to frame-4: %d %s", code, refusal)
	}
	eventually(t, "every screen to show frame-4", showing(samsBoard, "frame-4"))
	mayasBoard := boardOf("maya")
	if _, err := callControlBusiness.Act(ctx, people["maya"], s.ID, callControlBusiness.Action{Action: callControlBusiness.ActPresentBoard, Board: mayasBoard}); err != nil {
		t.Fatalf("Maya puts her board up over Sam's: %v", err)
	}
	eventually(t, "Maya's board on the whole board", showing(mayasBoard, ""))
	if code, refusal := move("sam", "frame-5"); code != http.StatusForbidden || refusal != "not_allowed" {
		t.Errorf("Sam moved the slides of Maya's board: %d %s", code, refusal)
	}
	if _, err := callControlBusiness.Act(ctx, people["maya"], s.ID, callControlBusiness.Action{Action: callControlBusiness.ActStopBoard}); err != nil {
		t.Fatalf("Maya stops presenting: %v", err)
	}
	if code, refusal := move("maya", "frame-6"); code != http.StatusConflict || refusal != "no_board" {
		t.Errorf("a move with no board on the stage: %d %s", code, refusal)
	}
}
