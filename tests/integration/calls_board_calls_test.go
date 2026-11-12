//go:build integration

package integration_test

// A board's calls (miro-27, calls with the board), on Postgres 12 and a real
// graph: every call that showed the board, the latest first, each with what
// it kept (its recordings, its transcript, its notes and its recap), for
// someone who may read the board AND open the call's conversation, never
// more. A board they can't read isn't there (404); a call in a conversation
// they aren't in, or are no longer in, isn't listed; a recording deleted
// (softly, as every delete is) drops out; and nothing answers an API token, a
// bot or someone who isn't signed in.

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"slices"
	"testing"
	"time"

	"github.com/dgraph-io/dgo/v230/protos/api"
	"github.com/go-chi/chi/v5"
	"github.com/google/uuid"

	boardBusiness "github.com/akashc777/OneCamp/business/Board"
	callStateBusiness "github.com/akashc777/OneCamp/business/CallState"
	callsController "github.com/akashc777/OneCamp/controllers/Calls"
	recordingDomain "github.com/akashc777/OneCamp/domain/Recording"
	"github.com/akashc777/OneCamp/helpers"
	"github.com/akashc777/OneCamp/initializers/dgraphInit"
	"github.com/akashc777/OneCamp/initializers/postgresInit"
	dgraphStruct "github.com/akashc777/OneCamp/models/dgraph"
	userModels "github.com/akashc777/OneCamp/models/postgres/User"
	"github.com/akashc777/OneCamp/tests/integration"
)

type boardCallArtefact struct {
	Key       string `json:"key"`
	Lines     int    `json:"lines"`
	NotesDoc  string `json:"notes_doc"`
	RecapChat string `json:"recap_chat"`
	Recorded  bool   `json:"recorded"`
}

type boardCall struct {
	SessionID    string              `json:"session_id"`
	Kind         string              `json:"kind"`
	Conversation string              `json:"conversation"`
	Live         bool                `json:"live"`
	OnStage      bool                `json:"on_stage"`
	ShownBy      string              `json:"shown_by"`
	People       int                 `json:"people"`
	EndedAt      *time.Time          `json:"ended_at"`
	Artefacts    []boardCallArtefact `json:"artefacts"`
}

// boardCallsOf reads a board's calls as who: as their API token's call when
// token, and as nobody signed in when who is nil.
func boardCallsOf(t *testing.T, who *dgraphStruct.DgraphUser, board string, token bool) (int, []boardCall) {
	t.Helper()
	r := httptest.NewRequest(http.MethodGet, "/board/getBoardCalls/"+board, nil)
	ctx := r.Context()
	if who != nil {
		info := userModels.UserInfo{UserDgraphInfo: *who}
		if id, err := uuid.Parse(who.Uuid); err == nil {
			info.UserPostgresInfo.Id = id
		}
		ctx = context.WithValue(ctx, helpers.UserInfoContextKey, info)
		if token {
			ctx = helpers.WithActsThroughToken(ctx)
		}
	}
	rc := chi.NewRouteContext()
	rc.URLParams.Add("board_uuid", board)
	w := httptest.NewRecorder()
	callsController.BoardCalls(w, r.WithContext(context.WithValue(ctx, chi.RouteCtxKey, rc)))
	var out struct {
		Data struct {
			Calls []boardCall `json:"calls"`
		} `json:"data"`
	}
	_ = json.Unmarshal(w.Body.Bytes(), &out)
	return w.Code, out.Data.Calls
}

// boardCallsWorld is a channel (#launch: Sam and Ana; an outsider and a bot
// outside it), Sam's public board and his private one, and two calls in the
// channel that showed them: one over an hour ago, which kept a transcript,
// notes, a recap and a recording, and one going on now with the public board
// on its stage.
type boardCallsWorld struct {
	people          map[string]*dgraphStruct.DgraphUser
	channel         string
	public, private string
	ended, live     uuid.UUID
	sessionKey      string
	channelUID      string
}

const (
	boardCallsRecording = "EG_board_calls_kept"
	boardCallsDeleted   = "EG_board_calls_deleted"
	boardCallsEarlier   = "EG_board_calls_earlier"
)

func seedBoardCalls(t *testing.T) boardCallsWorld {
	t.Helper()
	ctx := context.Background()
	pg := integration.SetupEnv(t)
	if err := postgresInit.ConnectPostgres(ctx, pg.DSN); err != nil {
		t.Fatal(err)
	}
	integration.SetupRedis(t)
	dg := integration.SetupDgraph(t)
	integration.StubMqttClient(t)

	w := boardCallsWorld{channel: uuid.NewString(), public: uuid.NewString(), private: uuid.NewString(), ended: uuid.New(), live: uuid.New()}
	w.people = seedChannel(t, pg, dg, w.channel, "sam", "ana", "outsider")
	sid := "RM_" + w.ended.String()[:8]
	w.sessionKey = helpers.CallSessionKeyFor(sid)

	now := time.Now().UTC()
	began, ended := now.Add(-2*time.Hour), now.Add(-time.Hour)
	stamp := func(at time.Time) string { return at.Format(time.RFC3339) }
	recording := func(blank, key string, at time.Time, extra map[string]any) map[string]any {
		n := map[string]any{"uid": "_:" + blank, "dgraph.type": "Recording", "recording_egress_id": key,
			"recording_stared_at": stamp(at), "recording_ended_at": stamp(at.Add(10 * time.Minute)),
			"recording_obj_key": "recordings/launch/" + key + ".mp4"}
		for k, v := range extra {
			n[k] = v
		}
		return n
	}
	uids := dg.Mutate(t, []map[string]any{
		{"uid": "_:pub", "dgraph.type": "Board", "board_uuid": w.public, "board_title": "Launch plan", "board_private": false,
			"board_deleted_at": "0001-01-01T00:00:00Z", "board_created_by": map[string]any{"uid": w.people["sam"].Uid}},
		{"uid": "_:priv", "dgraph.type": "Board", "board_uuid": w.private, "board_title": "Sam's sketch", "board_private": true,
			"board_deleted_at": "0001-01-01T00:00:00Z", "board_created_by": map[string]any{"uid": w.people["sam"].Uid}},
		{"uid": "_:bot", "dgraph.type": "User", "user_uuid": uuid.NewString(), "user_name": "helper-bot", "is_bot": true},
		// The call nobody recorded at first keeps its words on its session's
		// node, which also notes its notes and where its recap went.
		{"uid": "_:session", "dgraph.type": "Recording", "recording_egress_id": w.sessionKey,
			"recording_transcript_only": true, "recording_stared_at": stamp(began), "recording_ended_at": stamp(ended),
			"recording_notes_doc": "notes-" + w.ended.String()[:8], "recording_recap_chat": "recap-" + w.ended.String()[:8],
			"recording_transcript": []map[string]any{
				{"dgraph.type": "Transcript", "transcript_text": "Shall we start with the launch plan?", "transcript_timestamp": began.Add(time.Minute).UnixMilli()},
				{"dgraph.type": "Transcript", "transcript_text": "Yes, the board's up.", "transcript_timestamp": began.Add(2 * time.Minute).UnixMilli()},
			}},
		// Then someone recorded it; a second recording was deleted; a third is
		// the channel's from an earlier call.
		recording("kept", boardCallsRecording, began.Add(10*time.Minute), nil),
		recording("deleted", boardCallsDeleted, began.Add(20*time.Minute), map[string]any{"recording_deleted_at": stamp(now.Add(-30 * time.Minute))}),
		recording("earlier", boardCallsEarlier, now.Add(-6*time.Hour), nil),
	})
	q := dg.Query(t, `{ c(func: eq(ch_uuid, "`+w.channel+`")) { uid } }`)
	var found struct {
		C []struct {
			UID string `json:"uid"`
		} `json:"c"`
	}
	if err := json.Unmarshal(q, &found); err != nil || len(found.C) != 1 {
		t.Fatalf("the channel's node: %s, %v", q, err)
	}
	w.channelUID = found.C[0].UID
	dg.Mutate(t, map[string]any{"uid": w.channelUID, "ch_recording": []map[string]any{
		{"uid": uids["session"]}, {"uid": uids["kept"]}, {"uid": uids["deleted"]}, {"uid": uids["earlier"]}}})
	w.people["bot"] = &dgraphStruct.DgraphUser{Uid: uids["bot"], UserName: "helper-bot", IsBot: true}

	exec := func(q string, args ...any) {
		t.Helper()
		if _, err := pg.PG.Exec(q, args...); err != nil {
			t.Fatalf("%s: %v", q, err)
		}
	}
	exec(`INSERT INTO call_sessions (id, room, room_sid, kind, resource_kind, resource_id, started_at, ended_at, ended_reason)
		VALUES ($1, $2, $3, 'huddle', 'channel', $2, $4, $5, 'last_left')`, w.ended, w.channel, sid, began, ended)
	exec(`INSERT INTO call_sessions (id, room, room_sid, kind, resource_kind, resource_id, started_at, board_uuid, board_by)
		VALUES ($1, $2, $3, 'huddle', 'channel', $2, $4, $5, $6)`, w.live, w.channel, "RM_"+w.live.String()[:8], now.Add(-5*time.Minute), w.public, w.people["ana"].Uid)
	for _, a := range []struct {
		session uuid.UUID
		who     string
	}{{w.ended, "sam"}, {w.ended, "ana"}, {w.live, "ana"}} {
		exec(`INSERT INTO call_attendance (session_id, identity, user_uuid, name, role, joined_at, last_joined_at)
			VALUES ($1, $2, $3, $4, 'participant', $5, $5)`, a.session, w.people[a.who].Uid, w.people[a.who].Uuid, title(a.who), began)
	}
	exec(`INSERT INTO call_session_boards (session_id, board_uuid, shown_by, first_shown_at) VALUES ($1, $2, $3, $4), ($1, $5, $3, $6), ($7, $2, $8, $9)`,
		w.ended, w.public, w.people["sam"].Uid, began.Add(5*time.Minute), w.private, began.Add(6*time.Minute),
		w.live, w.people["ana"].Uid, now.Add(-4*time.Minute))
	return w
}

// Someone in the channel sees every call that showed the board, the latest
// first: the one going on now, with the board on its stage, to join; and the
// earlier one with what it kept, its transcript (on its session's node, with
// its notes and its recap) and the recording made during it, and nothing
// deleted or from another call. A recording deleted afterwards drops out on
// the next read.
func TestABoardListsTheCallsThatShowedIt(t *testing.T) {
	ctx := context.Background()
	w := seedBoardCalls(t)

	var indexed bool
	if err := postgresInit.DBConn.SqlDB.QueryRowContext(ctx, `SELECT EXISTS (SELECT 1 FROM pg_indexes
		WHERE tablename = 'call_session_boards' AND indexname = 'call_session_boards_board_idx')`).Scan(&indexed); err != nil || !indexed {
		t.Fatalf("a board's calls aren't indexed by the board: %v, %v", indexed, err)
	}

	code, calls := boardCallsOf(t, w.people["ana"], w.public, false)
	if code != http.StatusOK || len(calls) != 2 {
		t.Fatalf("Ana reads the public board's calls: %d, %+v; want both calls", code, calls)
	}
	live, past := calls[0], calls[1]
	if live.SessionID != w.live.String() || !live.Live || !live.OnStage || live.Kind != "channel" || live.Conversation != w.channel || live.ShownBy != "Ana" {
		t.Fatalf("the call going on now: %+v", live)
	}
	if past.SessionID != w.ended.String() || past.Live || past.OnStage || past.EndedAt == nil || past.ShownBy != "Sam" || past.People != 2 {
		t.Fatalf("the earlier call: %+v", past)
	}
	keys := map[string]boardCallArtefact{}
	for _, a := range past.Artefacts {
		keys[a.Key] = a
	}
	if len(keys) != 2 {
		t.Fatalf("the earlier call kept %+v; want its transcript and the recording made during it, nothing deleted or earlier", past.Artefacts)
	}
	if s := keys[w.sessionKey]; s.Recorded || s.Lines != 2 || s.NotesDoc == "" || s.RecapChat == "" {
		t.Fatalf("its transcript, notes and recap: %+v", s)
	}
	if r := keys[boardCallsRecording]; !r.Recorded {
		t.Fatalf("its recording: %+v", r)
	}

	if err := recordingDomain.BulkArchiveRecordings(ctx, []string{boardCallsRecording}); err != nil {
		t.Fatal(err)
	}
	_, calls = boardCallsOf(t, w.people["ana"], w.public, false)
	if len(calls) != 2 || len(calls[1].Artefacts) != 1 || calls[1].Artefacts[0].Key != w.sessionKey {
		t.Fatalf("after its recording was deleted, the earlier call keeps %+v; want its transcript only", calls)
	}
}

// A board's calls are for who may read the board and open each call's
// conversation, read afresh each time: a private board not shared with them
// isn't there; someone outside the channel sees the board but none of its
// calls; someone taken out of the channel stops seeing them; a deleted board
// is gone for everyone; and an API token, a bot and someone not signed in get
// nothing.
func TestABoardsCallsFollowTheBoardsAndTheCallsRules(t *testing.T) {
	ctx := context.Background()
	w := seedBoardCalls(t)

	if code, calls := boardCallsOf(t, w.people["sam"], w.private, false); code != http.StatusOK || len(calls) != 1 || calls[0].SessionID != w.ended.String() {
		t.Fatalf("Sam reads his private board's calls: %d, %+v", code, calls)
	}
	for _, who := range []string{"ana", "outsider"} {
		if code, calls := boardCallsOf(t, w.people[who], w.private, false); code != http.StatusNotFound || len(calls) != 0 {
			t.Fatalf("%s reads the calls of a private board not shared with them: %d, %+v", who, code, calls)
		}
	}
	if code, calls := boardCallsOf(t, w.people["outsider"], w.public, false); code != http.StatusOK || len(calls) != 0 {
		t.Fatalf("someone outside the channel reads the public board's calls: %d, %+v; want none", code, calls)
	}
	if code, calls := boardCallsOf(t, w.people["ana"], w.public, true); code != http.StatusForbidden || len(calls) != 0 {
		t.Fatalf("Ana's API token reads the board's calls: %d, %+v", code, calls)
	}
	if code, calls := boardCallsOf(t, w.people["bot"], w.public, false); code != http.StatusForbidden || len(calls) != 0 {
		t.Fatalf("a bot reads the board's calls: %d, %+v", code, calls)
	}
	if code, calls := boardCallsOf(t, nil, w.public, false); code != http.StatusUnauthorized || len(calls) != 0 {
		t.Fatalf("someone not signed in (a call's guest) reads the board's calls: %d, %+v", code, calls)
	}
	if code, _ := boardCallsOf(t, w.people["ana"], "not-a-board", false); code != http.StatusNotFound {
		t.Fatalf("a board that isn't one: %d", code)
	}

	// Ana is taken out of the channel: her next read shows none of its calls.
	del, _ := json.Marshal(map[string]any{"uid": w.channelUID, "ch_members": []map[string]any{{"uid": w.people["ana"].Uid}}})
	if _, err := dgraphInit.DoCommitNow(ctx, &api.Request{Mutations: []*api.Mutation{{DeleteJson: del}}}); err != nil {
		t.Fatal(err)
	}
	if code, calls := boardCallsOf(t, w.people["ana"], w.public, false); code != http.StatusOK || len(calls) != 0 {
		t.Fatalf("Ana, out of the channel, reads the board's calls: %d, %+v; want none", code, calls)
	}

	if err := boardBusiness.DeleteBoard(ctx, w.public); err != nil {
		t.Fatal(err)
	}
	if code, calls := boardCallsOf(t, w.people["sam"], w.public, false); code != http.StatusNotFound || len(calls) != 0 {
		t.Fatalf("Sam reads a deleted board's calls: %d, %+v", code, calls)
	}
}

// A DM's call and a group's that showed the board are listed for their people
// and no one else: Sam and the outsider's DM for them, not for Ana; Ana and
// the outsider's group for them, not for Sam. A page's worth of calls were
// shown the board between, all at one instant, so their session ids order
// them: the first page of the board's calls ends among them, and the next
// starts by its key inside that instant. The first page's last call and the
// next page's first are in #launch, and Sam and Ana see each of them once, in
// that order; the rest are in channels none of them is in. The DM's and the
// group's calls, further down, come from the next page.
func TestABoardListsADMsAndAGroupsCallsToTheirPeople(t *testing.T) {
	ctx := context.Background()
	w := seedBoardCalls(t)
	sam, ana, outsider := w.people["sam"], w.people["ana"], w.people["outsider"]
	dm := helpers.GetGroupingId(sam.Uuid, outsider.Uuid)
	group, err := helpers.GenerateGroupID([]string{ana.Uuid, outsider.Uuid, uuid.NewString()})
	if err != nil {
		t.Fatal(err)
	}
	set, _ := json.Marshal(map[string]any{"dgraph.type": "Dm", "dm_grouping_id": group, "dm_name": "Launch crew",
		"dm_participants": []map[string]any{{"uid": ana.Uid}, {"uid": outsider.Uid}}})
	if _, err := dgraphInit.DoCommitNow(ctx, &api.Request{Mutations: []*api.Mutation{{SetJson: set}}}); err != nil {
		t.Fatal(err)
	}
	exec := func(q string, args ...any) {
		t.Helper()
		if _, err := postgresInit.DBConn.SqlDB.ExecContext(ctx, q, args...); err != nil {
			t.Fatalf("%s: %v", q, err)
		}
	}
	call := func(id uuid.UUID, kind, room string, shown time.Time) uuid.UUID {
		exec(`INSERT INTO call_sessions (id, room, room_sid, kind, resource_kind, resource_id, started_at, ended_at, ended_reason)
			VALUES ($1, $2, $3, 'huddle', $4, $2, $5, $6, 'last_left')`, id, room, "RM_"+id.String()[:8], kind, shown.Add(-time.Minute), shown.Add(time.Minute))
		exec(`INSERT INTO call_session_boards (session_id, board_uuid, shown_by, first_shown_at) VALUES ($1, $2, $3, $4)`, id, w.public, sam.Uid, shown)
		return id
	}
	// Sorted, the tied calls' session ids are their order at their instant.
	// The live call and the ended one come first, so the first page ends at
	// tied[edge] and the next starts at tied[edge+1].
	now := time.Now().UTC()
	tied, edge := make([]uuid.UUID, callStateBusiness.MaxBoardCalls), callStateBusiness.MaxBoardCalls-3
	for i := range tied {
		tied[i] = uuid.New()
	}
	slices.SortFunc(tied, func(a, b uuid.UUID) int { return bytes.Compare(a[:], b[:]) })
	for i, id := range tied {
		room := uuid.NewString()
		if i == edge || i == edge+1 {
			room = w.channel
		}
		call(id, "channel", room, now.Add(-150*time.Minute))
	}
	dmCall, groupCall := call(uuid.New(), "dm", dm, now.Add(-3*time.Hour)), call(uuid.New(), "group", group, now.Add(-4*time.Hour))

	for _, c := range []struct {
		who  string
		want []uuid.UUID
		kind map[uuid.UUID]string
	}{
		{"outsider", []uuid.UUID{dmCall, groupCall}, map[uuid.UUID]string{dmCall: "dm", groupCall: "group"}},
		{"sam", []uuid.UUID{w.live, w.ended, tied[edge], tied[edge+1], dmCall},
			map[uuid.UUID]string{tied[edge]: "channel", tied[edge+1]: "channel", dmCall: "dm"}},
		{"ana", []uuid.UUID{w.live, w.ended, tied[edge], tied[edge+1], groupCall},
			map[uuid.UUID]string{tied[edge]: "channel", tied[edge+1]: "channel", groupCall: "group"}},
	} {
		code, calls := boardCallsOf(t, w.people[c.who], w.public, false)
		if code != http.StatusOK || len(calls) != len(c.want) {
			t.Fatalf("%s reads the board's calls: %d, %+v; want %v, each once", c.who, code, calls, c.want)
		}
		for i, id := range c.want {
			if calls[i].SessionID != id.String() || (c.kind[id] != "" && (calls[i].Kind != c.kind[id] ||
				calls[i].Conversation != map[string]string{"channel": w.channel, "dm": dm, "group": group}[c.kind[id]])) {
				t.Fatalf("%s's call %d: %+v; want %s, the latest they may read first", c.who, i, calls[i], id)
			}
		}
	}
}
