//go:build integration

package integration_test

// Polls and Q&A in a call (calls package A, "Ask the room": meet-10,
// zoom-02), against a real channel's call on a local LiveKit with its host
// (Maya), a participant (Ben), someone let in by the invite (Jonas) and a
// guest (Gia): who may do what, anonymity in the rows, the views, the packets,
// the audit and the report, the caps and limits, the moderation, and a report
// written once.

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/go-chi/chi/v5"
	"github.com/google/uuid"

	callControlBusiness "github.com/akashc777/OneCamp/business/CallControl"
	callInviteBusiness "github.com/akashc777/OneCamp/business/CallInvite"
	callQABusiness "github.com/akashc777/OneCamp/business/CallQA"
	callStateBusiness "github.com/akashc777/OneCamp/business/CallState"
	callThreadBusiness "github.com/akashc777/OneCamp/business/CallThread"
	guestBusiness "github.com/akashc777/OneCamp/business/Guest"
	liveKitBusiness "github.com/akashc777/OneCamp/business/LiveKit"
	pollBusiness "github.com/akashc777/OneCamp/business/Poll"
	resourceEventBusiness "github.com/akashc777/OneCamp/business/ResourceEvent"
	settingsBusiness "github.com/akashc777/OneCamp/business/Settings"
	callsController "github.com/akashc777/OneCamp/controllers/Calls"
	userDomain "github.com/akashc777/OneCamp/domain/User"
	"github.com/akashc777/OneCamp/helpers"
	"github.com/akashc777/OneCamp/initializers/postgresInit"
	dgraphStruct "github.com/akashc777/OneCamp/models/dgraph"
	userModels "github.com/akashc777/OneCamp/models/postgres/User"
	"github.com/akashc777/OneCamp/tests/integration"
)

// The migration comes off and goes back on, and its rules hold: an anonymous
// question keeps no name, the Q&A shows all or answered, a question is at most
// 300 characters, and a call's polls and questions go with the call.
func TestCallActivitiesMigrationUpDownUp(t *testing.T) {
	env := integration.SetupEnv(t)
	count := func(q string, args ...any) int {
		t.Helper()
		var n int
		if err := env.PG.QueryRow(q, args...).Scan(&n); err != nil {
			t.Fatal(err)
		}
		return n
	}
	column := func(table, name string) bool {
		return count(`SELECT count(*) FROM information_schema.columns WHERE table_name = $1 AND column_name = $2`, table, name) == 1
	}
	table := func(name string) bool {
		return count(`SELECT count(*) FROM information_schema.tables WHERE table_name = $1`, name) == 1
	}
	if _, err := env.PG.Exec(migrationSQL(t, "call_activities", "down")); err != nil {
		t.Fatalf("down: %v", err)
	}
	if column("polls", "session_id") || column("call_sessions", "qa_open") || table("call_questions") || table("call_question_votes") {
		t.Fatal("the activities are still there after down")
	}
	if _, err := env.PG.Exec(migrationSQL(t, "call_activities", "up")); err != nil {
		t.Fatalf("up again: %v", err)
	}
	session := uuid.New()
	if _, err := env.PG.Exec(`INSERT INTO call_sessions (id, room, kind, resource_kind, resource_id, started_at) VALUES ($1, 'room-a', 'huddle', 'channel', 'room-a', NOW())`, session); err != nil {
		t.Fatal(err)
	}
	if _, err := env.PG.Exec(`UPDATE call_sessions SET qa_shown = 'everyone' WHERE id = $1`, session); err == nil {
		t.Error("a Q&A shown to a way the call doesn't know")
	}
	ask := func(name string, anonymous bool, body string) error {
		_, err := env.PG.Exec(`INSERT INTO call_questions (id, session_id, asker, asker_name, anonymous, body) VALUES ($1, $2, 'a', $3, $4, $5)`, uuid.New(), session, name, anonymous, body)
		return err
	}
	if ask("Ben", true, "Will it scale?") == nil {
		t.Error("an anonymous question kept a name")
	}
	if ask("", false, strings.Repeat("a", 301)) == nil {
		t.Error("a question of 301 characters")
	}
	if err := ask("", true, "Will it scale?"); err != nil {
		t.Fatal(err)
	}
	if _, err := env.PG.Exec(`INSERT INTO polls (id, channel_id, question, options, created_by, session_id, anonymous) VALUES ($1, $2, 'Which day?', '[]', $3, $4, true)`, uuid.New(), uuid.Nil, uuid.New(), session); err != nil {
		t.Fatal(err)
	}
	if _, err := env.PG.Exec(`DELETE FROM call_sessions WHERE id = $1`, session); err != nil {
		t.Fatal(err)
	}
	if count(`SELECT count(*) FROM call_questions WHERE session_id = $1`, session)+count(`SELECT count(*) FROM polls WHERE session_id = $1`, session) != 0 {
		t.Error("a call's polls and questions outlived it")
	}
}

func TestCallPollsAndQuestionsInACall(t *testing.T) {
	ctx := context.Background()
	t.Setenv("JWT_SECRET", "the activities test's secret")
	pg := integration.SetupEnv(t)
	if err := postgresInit.ConnectPostgres(ctx, pg.DSN); err != nil {
		t.Fatal(err)
	}
	lk := integration.SetupLiveKit(t)
	integration.SetupRedis(t)
	dg := integration.SetupDgraph(t)
	integration.StubMqttClient(t)
	if err := settingsBusiness.SetGuestAccessEnabled(true); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = settingsBusiness.SetGuestAccessEnabled(false) })
	room := uuid.NewString()
	people := seedChannel(t, pg, dg, room, "maya", "ben", "jonas")
	maya, ben, jonas := people["maya"], people["ben"], people["jonas"]
	enter := func(u *dgraphStruct.DgraphUser, token string) *joined {
		t.Helper()
		j := join(t, lk, token)
		joinSettled(t, room, u.Uid, j)
		connected(t, ctx, room, u.Uid)
		return j
	}
	mayaToken, _, err := liveKitBusiness.CreateRoomAndGetToken(ctx, room, maya, false, "")
	if err != nil {
		t.Fatal(err)
	}
	enter(maya, mayaToken)
	var s *callStateBusiness.Session
	eventually(t, "the call to take its post", func() bool {
		s, err = callStateBusiness.Current(ctx, callStateBusiness.Ref{Kind: "channel", ID: room})
		return err == nil && s != nil && s.PostUUID != ""
	})
	benToken, _, err := liveKitBusiness.CreateRoomAndGetToken(ctx, room, ben, false, "")
	if err != nil {
		t.Fatal(err)
	}
	enter(ben, benToken)
	link, err := callInviteBusiness.Invite(ctx, room, maya)
	if err != nil {
		t.Fatal(err)
	}
	entry, err := callInviteBusiness.Enter(ctx, callInviteBusiness.Door{Token: link.GuestToken}, jonas, "")
	if err != nil {
		t.Fatal(err)
	}
	jonasIn := enter(jonas, entry.Token)
	door, err := guestBusiness.OpenDoor(ctx, link.GuestToken)
	if err != nil {
		t.Fatal(err)
	}
	guestToken, _, err := guestBusiness.IssueGuestMeetingToken(ctx, door, "Gia", "")
	if err != nil {
		t.Fatal(err)
	}
	giaIn := join(t, lk, guestToken)
	gia := ""
	eventually(t, "Gia to be in the call", func() bool {
		for _, id := range stillIn(ctx, room) {
			if strings.HasPrefix(id, "guest-") {
				gia = id
				return true
			}
		}
		return false
	})
	connected(t, ctx, room, gia)
	giaAuth := "Bearer " + guestToken

	do := func(u *dgraphStruct.DgraphUser, a callControlBusiness.Activity) (string, error) {
		return callControlBusiness.DoActivity(ctx, u, s.ID, a)
	}
	must := func(id string, err error) string {
		t.Helper()
		if err != nil {
			t.Fatal(err)
		}
		return id
	}
	view := func(u *dgraphStruct.DgraphUser) *callControlBusiness.Activities {
		t.Helper()
		a, err := callControlBusiness.ActivitiesFor(ctx, u, s.ID)
		if err != nil {
			t.Fatalf("%s's view: %v", u.UserName, err)
		}
		return a
	}
	giaView := func() *callControlBusiness.Activities {
		t.Helper()
		a, err := callControlBusiness.GuestActivities(ctx, giaAuth)
		if err != nil {
			t.Fatalf("Gia's view: %v", err)
		}
		return a
	}
	pollIn := func(a *callControlBusiness.Activities, id string) *pollBusiness.View {
		for _, p := range a.Polls {
			if p.ID == id {
				return p
			}
		}
		t.Fatalf("poll %s isn't in the view", id)
		return nil
	}
	votes := func(p *pollBusiness.View) []int {
		var out []int
		for _, o := range p.Options {
			out = append(out, o.Votes)
		}
		return out
	}
	refused := func(err error, code string) bool {
		var r *callControlBusiness.Refusal
		return errors.As(err, &r) && r.Code == code
	}
	vote := func(id, option string) callControlBusiness.Activity {
		return callControlBusiness.Activity{Op: callControlBusiness.OpVote, ID: id, Options: []string{option}}
	}
	ask := func(text string, anonymous bool) callControlBusiness.Activity {
		return callControlBusiness.Activity{Op: callControlBusiness.OpAsk, Text: text, Anonymous: anonymous}
	}

	// Only whoever runs the call starts a poll; a token never does.
	day := callControlBusiness.Activity{Op: callControlBusiness.OpPoll, Poll: &pollBusiness.CallPoll{Question: "Which day do we ship?", Options: []string{"Tuesday", "Wednesday", "Thursday"}, HideResults: true}}
	if _, err := do(ben, day); !errors.Is(err, callControlBusiness.ErrNotAllowed) {
		t.Errorf("a participant starts a poll: %v", err)
	}
	if _, err := do(jonas, day); !errors.Is(err, callControlBusiness.ErrNotAllowed) {
		t.Errorf("an invitee starts a poll: %v", err)
	}
	if _, err := callControlBusiness.GuestDoesActivity(ctx, giaAuth, day); !errors.Is(err, callControlBusiness.ErrNotAllowed) {
		t.Errorf("a guest starts a poll: %v", err)
	}
	if _, err := callControlBusiness.DoActivity(helpers.WithActsThroughToken(ctx), maya, s.ID, day); !errors.Is(err, callControlBusiness.ErrThroughToken) {
		t.Errorf("a poll through a token: %v", err)
	}
	if _, err := callControlBusiness.ActivitiesFor(helpers.WithActsThroughToken(ctx), maya, s.ID); !errors.Is(err, callControlBusiness.ErrThroughToken) {
		t.Errorf("a read through a token: %v", err)
	}
	eleven := callControlBusiness.Activity{Op: callControlBusiness.OpPoll, Poll: &pollBusiness.CallPoll{Question: "Too many?", Options: strings.Split("a b c d e f g h i j k", " ")}}
	if _, err := do(maya, eleven); !refused(err, "bad_activity") {
		t.Errorf("a poll of 11 options: %v", err)
	}
	dayID := must(do(maya, day))

	// Everyone votes, the guest too; held-back counts are the host's alone.
	must(do(ben, vote(dayID, "1")))
	must(do(jonas, vote(dayID, "1")))
	must(callControlBusiness.GuestDoesActivity(ctx, giaAuth, vote(dayID, "2")))
	if p := pollIn(view(ben), dayID); p.Voters != 0 || votes(p)[0]+votes(p)[1]+votes(p)[2] != 0 || len(p.Mine) != 1 || p.Mine[0] != "1" {
		t.Errorf("Ben, results held back: %+v", p)
	}
	if p := pollIn(giaView(), dayID); p.Voters != 0 || votes(p)[1] != 0 || len(p.Mine) != 1 || p.Mine[0] != "2" {
		t.Errorf("Gia, results held back: %+v", p)
	}
	if p := pollIn(view(maya), dayID); p.Voters != 3 || votes(p)[0] != 2 || votes(p)[1] != 1 {
		t.Errorf("Maya, who runs the call: %+v", p)
	}
	for _, op := range []string{callControlBusiness.OpShare, callControlBusiness.OpClosePoll} {
		if _, err := callControlBusiness.GuestDoesActivity(ctx, giaAuth, callControlBusiness.Activity{Op: op, ID: dayID}); !errors.Is(err, callControlBusiness.ErrNotAllowed) {
			t.Errorf("Gia's %s: %v", op, err)
		}
		if _, err := do(ben, callControlBusiness.Activity{Op: op, ID: dayID}); !errors.Is(err, callControlBusiness.ErrNotAllowed) {
			t.Errorf("Ben's %s: %v", op, err)
		}
	}
	must(do(maya, callControlBusiness.Activity{Op: callControlBusiness.OpShare, ID: dayID}))
	if p := pollIn(view(ben), dayID); p.Voters != 3 || votes(p)[0] != 2 {
		t.Errorf("Ben, results shared: %+v", p)
	}
	// Chat's poll routes never reach a call's poll.
	mayaInfo := &userModels.UserInfo{UserDgraphInfo: *maya}
	mayaInfo.UserPostgresInfo.Id = uuid.MustParse(maya.Uuid)
	if _, err := pollBusiness.Get(ctx, mayaInfo, dayID); !errors.Is(err, pollBusiness.ErrNoAccess) {
		t.Errorf("chat's read of a call's poll: %v", err)
	}
	if _, err := pollBusiness.Close(ctx, mayaInfo, dayID); !errors.Is(err, pollBusiness.ErrNoAccess) {
		t.Errorf("chat's close of a call's poll: %v", err)
	}

	// An anonymous poll's vote is kept under nobody's id.
	anonID := must(do(maya, callControlBusiness.Activity{Op: callControlBusiness.OpPoll, Poll: &pollBusiness.CallPoll{Question: "How was the demo?", Options: []string{"Good", "Not yet"}, Anonymous: true}}))
	must(do(ben, vote(anonID, "2")))
	var kept uuid.UUID
	if err := pg.PG.QueryRow(`SELECT user_id FROM poll_votes WHERE poll_id = $1`, anonID).Scan(&kept); err != nil || kept.String() == ben.Uuid {
		t.Errorf("Ben's anonymous vote is kept under %v (%v)", kept, err)
	}
	if p := pollIn(view(ben), anonID); len(p.Mine) != 1 || p.Mine[0] != "2" || !p.Anonymous {
		t.Errorf("Ben's own anonymous vote: %+v", p)
	}

	// Q&A: closed until opened, anonymous only where allowed, capped.
	if _, err := do(ben, ask("Is the price final?", false)); !errors.Is(err, callControlBusiness.ErrQAClosed) {
		t.Errorf("a question while Q&A is closed: %v", err)
	}
	must(do(maya, callControlBusiness.Activity{Op: callControlBusiness.OpQA, QA: &callStateBusiness.QA{Open: true}}))
	if _, err := do(ben, ask("Will it scale?", true)); !errors.Is(err, callControlBusiness.ErrNoAnonymous) {
		t.Errorf("an anonymous question while they're off: %v", err)
	}
	must(do(maya, callControlBusiness.Activity{Op: callControlBusiness.OpQA, QA: &callStateBusiness.QA{Open: true, Anonymous: true}}))
	benQ := must(do(ben, ask("Will it scale?", true)))
	giaQ := must(callControlBusiness.GuestDoesActivity(ctx, giaAuth, ask("Is the price final?", false)))
	if _, err := do(ben, ask(strings.Repeat("a", 301), false)); !refused(err, "bad_activity") {
		t.Errorf("a question of 301 characters: %v", err)
	}
	var asker, askerName string
	if err := pg.PG.QueryRow(`SELECT asker, asker_name FROM call_questions WHERE id = $1`, benQ).Scan(&asker, &askerName); err != nil || asker == ben.Uid || askerName != "" {
		t.Errorf("Ben's anonymous question is kept as %q %q (%v)", asker, askerName, err)
	}
	must(do(jonas, callControlBusiness.Activity{Op: callControlBusiness.OpUpvote, ID: giaQ, On: true}))
	must(callControlBusiness.GuestDoesActivity(ctx, giaAuth, callControlBusiness.Activity{Op: callControlBusiness.OpUpvote, ID: benQ, On: true}))
	// Every upvote is kept under a key, anonymous Q&A or not: no row says who
	// upvoted what, and each voter still reads their own upvote.
	var named int
	if err := pg.PG.QueryRow(`SELECT count(*) FROM call_question_votes WHERE voter IN ($1, $2)`, jonas.Uid, gia).Scan(&named); err != nil || named != 0 {
		t.Errorf("upvotes kept under who gave them: %d (%v)", named, err)
	}
	for _, q := range view(jonas).Questions {
		if q.ID == giaQ && (!q.Voted || q.Votes != 1) {
			t.Errorf("Jonas reads his upvote of Gia's question as %+v", q)
		}
	}
	for name, a := range map[string]*callControlBusiness.Activities{"Maya": view(maya), "Jonas": view(jonas), "Gia": giaView()} {
		raw, _ := json.Marshal(a)
		for _, who := range []string{ben.Uid, ben.Uuid} {
			if strings.Contains(string(raw), who) {
				t.Errorf("%s's view names Ben (%s): %s", name, who, raw)
			}
		}
		for _, q := range a.Questions {
			if q.ID == benQ && (q.Asker != "" || !q.Anonymous || q.Yours) {
				t.Errorf("%s sees Ben's anonymous question as %+v", name, q)
			}
		}
	}
	for _, q := range view(ben).Questions {
		if q.ID == benQ && !q.Yours {
			t.Error("Ben doesn't see his anonymous question as his")
		}
	}
	// A guest asks no faster than the limit: five a minute.
	for i := 0; i < 4; i++ {
		must(callControlBusiness.GuestDoesActivity(ctx, giaAuth, ask("One more thing", false)))
	}
	if _, err := callControlBusiness.GuestDoesActivity(ctx, giaAuth, ask("And another", false)); !errors.Is(err, callControlBusiness.ErrTooFast) {
		t.Errorf("a sixth question in a minute: %v", err)
	}

	// Answered only: Jonas sees answered questions and his own; a dismissed
	// question leaves everyone's view at once; Gia moderates nothing.
	must(do(maya, callControlBusiness.Activity{Op: callControlBusiness.OpQA, QA: &callStateBusiness.QA{Open: true, Shown: "answered", Anonymous: true}}))
	if qs := view(jonas).Questions; len(qs) != 0 {
		t.Errorf("Jonas sees unanswered questions: %+v", qs)
	}
	if _, err := callControlBusiness.GuestDoesActivity(ctx, giaAuth, callControlBusiness.Activity{Op: callControlBusiness.OpQuestion, ID: giaQ, State: "answered"}); !errors.Is(err, callControlBusiness.ErrNotAllowed) {
		t.Errorf("Gia answers her own question: %v", err)
	}
	must(do(maya, callControlBusiness.Activity{Op: callControlBusiness.OpQuestion, ID: giaQ, State: "answered"}))
	if qs := view(jonas).Questions; len(qs) != 1 || qs[0].ID != giaQ {
		t.Errorf("Jonas, answered only: %+v", qs)
	}
	must(do(maya, callControlBusiness.Activity{Op: callControlBusiness.OpQuestion, ID: benQ, State: "dismissed"}))
	for name, a := range map[string]*callControlBusiness.Activities{"Maya": view(maya), "Ben": view(ben), "Gia": giaView()} {
		for _, q := range a.Questions {
			if q.ID == benQ {
				t.Errorf("%s still sees the dismissed question", name)
			}
		}
	}
	if _, err := do(jonas, callControlBusiness.Activity{Op: callControlBusiness.OpUpvote, ID: benQ, On: true}); !errors.Is(err, callControlBusiness.ErrNoActivity) {
		t.Errorf("an upvote of a dismissed question: %v", err)
	}

	// Through the route: what Maya does is audited, what Ben does never is,
	// and no audit row carries anyone's question or name.
	act := func(u *dgraphStruct.DgraphUser, a callControlBusiness.Activity) int {
		t.Helper()
		row, err := userDomain.GetActiveUserWithAdminFlagByUserUUID(ctx, uuid.MustParse(u.Uuid))
		if err != nil || row == nil {
			t.Fatalf("load %s: %v", u.UserName, err)
		}
		rc := chi.NewRouteContext()
		rc.URLParams.Add("session_id", s.ID)
		as := context.WithValue(context.WithValue(ctx, helpers.UserInfoContextKey, userModels.UserInfo{UserPostgresInfo: *row, UserDgraphInfo: *u}), chi.RouteCtxKey, rc)
		raw, _ := json.Marshal(a)
		rec := httptest.NewRecorder()
		callsController.DoCallActivity(rec, httptest.NewRequest(http.MethodPost, "/", bytes.NewReader(raw)).WithContext(as))
		return rec.Code
	}
	pinned := true
	if code := act(maya, callControlBusiness.Activity{Op: callControlBusiness.OpQuestion, ID: giaQ, Pinned: &pinned}); code != http.StatusOK {
		t.Errorf("Maya pins through the route: %d", code)
	}
	if code := act(ben, vote(dayID, "2")); code != http.StatusOK {
		t.Errorf("Ben votes through the route: %d", code)
	}
	eventually(t, "the pin in the audit log", func() bool {
		var n int
		return pg.PG.QueryRow(`SELECT count(*) FROM admin_audit_log WHERE action = 'call.activities.question' AND metadata->>'id' = $1`, giaQ).Scan(&n) == nil && n == 1
	})
	var leaks int
	if err := pg.PG.QueryRow(`SELECT count(*) FROM admin_audit_log WHERE action LIKE 'call.activities.%'
		AND (action IN ('call.activities.vote', 'call.activities.ask', 'call.activities.upvote')
		  OR metadata::text LIKE '%Is the price final%' OR metadata::text LIKE '%' || $1 || '%' OR metadata::text LIKE '%' || $2 || '%')`, gia, ben.Uid).Scan(&leaks); err != nil || leaks != 0 {
		t.Errorf("the audit log keeps a vote, a question or a person: %d (%v)", leaks, err)
	}

	// Every packet says that something changed, at most which kind, and never who.
	giaIn.mu.Lock()
	told := 0
	kinds := []string{callControlBusiness.ToldPoll, callControlBusiness.ToldResults, callControlBusiness.ToldQA, callControlBusiness.ToldQuestion}
	for _, p := range giaIn.packets {
		if p.Topic != "oc.call" || !bytes.Contains(p.Payload, []byte(`"activities"`)) {
			continue
		}
		told++
		var m map[string]any
		err := json.Unmarshal(p.Payload, &m)
		what, hasWhat := m["what"].(string)
		keys := 2 // v and t
		if hasWhat {
			keys++
		}
		if err != nil || len(m) != keys || m["t"] != "activities" || (hasWhat && !slices.Contains(kinds, what)) {
			t.Errorf("an activities packet carries more: %s", p.Payload)
		}
	}
	giaIn.mu.Unlock()
	if told == 0 {
		t.Error("nobody's screen was told the activities changed")
	}

	// Someone who left votes and reads nothing.
	jonasIn.room.Disconnect()
	eventually(t, "Jonas to have left", func() bool {
		for _, id := range stillIn(ctx, room) {
			if id == jonas.Uid {
				return false
			}
		}
		return true
	})
	if _, err := do(jonas, vote(dayID, "3")); !errors.Is(err, callControlBusiness.ErrNotInCall) {
		t.Errorf("a vote after leaving: %v", err)
	}
	if _, err := callControlBusiness.ActivitiesFor(ctx, jonas, s.ID); !errors.Is(err, callControlBusiness.ErrNotInCall) {
		t.Errorf("a read after leaving a live call: %v", err)
	}
	// A guest who left votes and reads nothing either, until they're back:
	// nothing here tells Call State they left, as when LiveKit's webhook is late.
	giaIn.room.Disconnect()
	eventually(t, "Gia to have left", func() bool { return !slices.Contains(stillIn(ctx, room), gia) })
	if _, err := callControlBusiness.GuestDoesActivity(ctx, giaAuth, vote(dayID, "1")); !errors.Is(err, callControlBusiness.ErrNotInCall) {
		t.Errorf("a guest's vote after leaving: %v", err)
	}
	if _, err := callControlBusiness.GuestActivities(ctx, giaAuth); !errors.Is(err, callControlBusiness.ErrNotInCall) {
		t.Errorf("a guest's read after leaving a live call: %v", err)
	}
	join(t, lk, guestToken)
	connected(t, ctx, room, gia)
	giaView()

	// The call ends: one report under its post, the anonymous question with no
	// name and the held-back counts kept back, written once across a second
	// end and a lost mark; the guest reads nothing after. It ends showing only
	// the answered questions, with Gia's four "One more thing" still open, so
	// the report, which everyone in the conversation reads, lists only those.
	if _, err := callControlBusiness.Act(ctx, maya, s.ID, callControlBusiness.Action{Action: callControlBusiness.ActEnd}); err != nil {
		t.Fatal(err)
	}
	reports := func() []string {
		th, err := guestBusiness.GetCallThread(ctx, "channel", s.PostUUID, s.StartedAt, time.Now().Add(time.Hour))
		if err != nil {
			return nil
		}
		var out []string
		for _, r := range th.Replies {
			if strings.Contains(r.Text, "From the call") {
				out = append(out, r.Text)
			}
		}
		return out
	}
	eventually(t, "the call's report", func() bool { return len(reports()) == 1 })
	report := reports()[0]
	for _, want := range []string{"Which day do we ship?", "How was the demo?", "anonymously", "Is the price final?", "asked by Guest: Gia"} {
		if !strings.Contains(report, want) {
			t.Errorf("the report lacks %q:\n%s", want, report)
		}
	}
	if strings.Contains(report, "One more thing") {
		t.Errorf("an answered-only call's report lists an open question:\n%s", report)
	}
	if strings.Contains(report, "Will it scale?") {
		t.Errorf("the report lists a dismissed question:\n%s", report)
	}
	again := func(what string) {
		t.Helper()
		resourceEventBusiness.Bus.Emit(ctx, resourceEventBusiness.Event{Kind: resourceEventBusiness.CallEnded, Data: map[string]string{"session": s.ID, "room": room, "kind": "huddle"}})
		resourceEventBusiness.Sinks.Settle(30 * time.Second)
		if n := len(reports()); n != 1 {
			t.Errorf("after %s: %d reports", what, n)
		}
	}
	again("a second end")
	if _, err := pg.PG.Exec(`UPDATE call_sessions SET report_done_at = NULL, report_claimed_at = NULL WHERE id = $1`, s.ID); err != nil {
		t.Fatal(err)
	}
	again("a write whose mark was lost")
	if _, err := callControlBusiness.GuestActivities(ctx, giaAuth); !errors.Is(err, callControlBusiness.ErrNotInCall) {
		t.Errorf("Gia's read after the call: %v", err)
	}
	if _, err := callThreadBusiness.ForGuest(ctx, giaAuth, time.Now(), time.Time{}); !errors.Is(err, callThreadBusiness.ErrNotFound) {
		t.Errorf("Gia's read of the thread after the call: %v", err)
	}
	// A member who was in the call reads its activities after it, as its page does.
	if a, err := callControlBusiness.ActivitiesFor(ctx, ben, s.ID); err != nil || len(a.Polls) != 2 {
		t.Errorf("Ben after the call: %v %v", a, err)
	}
}

// A call's caps hold when many reach for the last place at once: of twelve
// starts at the 50th poll one goes through, and of twelve asks at the 500th
// question one does; the rest are told the call is full.
func TestACallsCapsHoldWhenManyAskAtOnce(t *testing.T) {
	ctx := context.Background()
	t.Setenv("JWT_SECRET", "the caps test's secret")
	pg := integration.SetupEnv(t)
	if err := postgresInit.ConnectPostgres(ctx, pg.DSN); err != nil {
		t.Fatal(err)
	}
	session := uuid.New()
	if _, err := pg.PG.Exec(`INSERT INTO call_sessions (id, room, kind, resource_kind, resource_id, started_at) VALUES ($1, 'room-caps', 'huddle', 'channel', 'room-caps', NOW())`, session); err != nil {
		t.Fatal(err)
	}
	if _, err := pg.PG.Exec(`INSERT INTO polls (id, channel_id, question, options, created_by, session_id)
		SELECT md5('poll' || g)::uuid, $1, 'Poll ' || g, '[]', $2, $3 FROM generate_series(1, $4) g`,
		uuid.Nil, uuid.New(), session, pollBusiness.MaxCallPolls-1); err != nil {
		t.Fatal(err)
	}
	if _, err := pg.PG.Exec(`INSERT INTO call_questions (id, session_id, asker, body)
		SELECT md5('question' || g)::uuid, $1, 'seed', 'Question ' || g FROM generate_series(1, $2) g`,
		session, callQABusiness.MaxQuestions-1); err != nil {
		t.Fatal(err)
	}
	// race runs try twelve times at once, and says how many went through and
	// whether each of the rest was refused as full.
	race := func(try func(i int) error, full func(error) bool) (made int) {
		t.Helper()
		start := make(chan struct{})
		errs := make(chan error, 12)
		for i := 0; i < 12; i++ {
			go func() {
				<-start
				errs <- try(i)
			}()
		}
		close(start)
		for i := 0; i < 12; i++ {
			if err := <-errs; err == nil {
				made++
			} else if !full(err) {
				t.Errorf("refused for another reason: %v", err)
			}
		}
		return made
	}
	count := func(table string) int {
		t.Helper()
		var n int
		if err := pg.PG.QueryRow(`SELECT count(*) FROM `+table+` WHERE session_id = $1`, session).Scan(&n); err != nil {
			t.Fatal(err)
		}
		return n
	}

	made := race(func(int) error {
		_, err := pollBusiness.CreateInCall(ctx, session, uuid.New(), pollBusiness.CallPoll{Question: "Which day?", Options: []string{"Tue", "Thu"}})
		return err
	}, func(err error) bool {
		var full pollBusiness.InputError
		return errors.As(err, &full)
	})
	if n := count("polls"); made != 1 || n != pollBusiness.MaxCallPolls {
		t.Errorf("twelve starts at the last poll's place: %d went through, the call has %d polls", made, n)
	}
	made = race(func(i int) error {
		_, err := callQABusiness.Ask(ctx, session, fmt.Sprintf("guest-%d", i), "", "Is it ready?", true)
		return err
	}, func(err error) bool {
		var full callQABusiness.InputError
		return errors.As(err, &full)
	})
	if n := count("call_questions"); made != 1 || n != callQABusiness.MaxQuestions {
		t.Errorf("twelve asks at the last question's place: %d went through, the call has %d questions", made, n)
	}
}
