//go:build integration

package business

// NOTIF M11: nudges vanished, then came back as new. When the pass's read of
// the actionable items failed it went on with no signals, so it superseded
// every open nudge of every person (each badge dropped, a "cleared" went
// out), and the next good pass raised them all again as new rows with new
// ids and a live "new". A failed owner-scope lookup closed that owner's
// nudges the same way, and a read cut off at the pass's cap closed the
// signals past it.
//
// Now a pass closes only what it positively read as gone: a failed read
// closes nothing, a failed scope lookup holds the owner's nudges for those
// items, and a capped read raises but closes nothing. And the pass is one
// statement per step for everyone, not one per person. A profile read as
// gone (a deleted member) or a scope read as left is a read: their nudge
// closes; only the one who left loses the item.
//
// Run: go test -tags=integration ./business/AI/ -run 'TestAFailedNudgeReadClosesNothing|TestAFailedScopeLookupHoldsTheNudge|TestAGoneMembersNudgeCloses|TestAMemberWhoLeftLosesTheNudge|TestACappedNudgeReadClosesNothing|TestTheNudgePassIsFlatInPeople' -v

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"maps"
	"testing"
	"time"

	userDomain "github.com/akashc777/OneCamp/domain/User"
	"github.com/akashc777/OneCamp/helpers"
	"github.com/akashc777/OneCamp/initializers/postgresInit"
	dgraphStruct "github.com/akashc777/OneCamp/models/dgraph"
	mqttStruct "github.com/akashc777/OneCamp/models/mqtt"
	memoryModels "github.com/akashc777/OneCamp/models/postgres/WorkspaceMemory"
	"github.com/akashc777/OneCamp/tests/integration"
	"github.com/google/uuid"
	"github.com/lib/pq"
)

// nudgeStates is every nudge of these people: its status by its id.
func nudgeStates(t *testing.T, env *integration.Env, who ...uuid.UUID) map[uuid.UUID]string {
	t.Helper()
	rows, err := env.PG.Query(`SELECT id, status FROM workspace_nudges WHERE user_id = ANY($1::uuid[])`, pq.Array(who))
	if err != nil {
		t.Fatal(err)
	}
	defer rows.Close()
	out := map[uuid.UUID]string{}
	for rows.Next() {
		var id uuid.UUID
		var status string
		if err := rows.Scan(&id, &status); err != nil {
			t.Fatal(err)
		}
		out[id] = status
	}
	return out
}

// nudgeEvent is one live nudge event: what it says, and the nudges it names.
type nudgeEvent struct {
	Action   string   `json:"action"`
	NudgeID  string   `json:"nudge_id"`
	NudgeIDs []string `json:"nudge_ids"`
}

// nudgeEvents is every nudge event published to whom so far, once the work
// the server handed off has settled.
func nudgeEvents(t *testing.T, broker *integration.MqttRecorder, whom uuid.UUID) []nudgeEvent {
	t.Helper()
	if busy := helpers.SettleBackground(10 * time.Second); len(busy) > 0 {
		t.Fatalf("still at work: %v", busy)
	}
	var out []nudgeEvent
	for _, raw := range broker.Messages(helpers.GetMqttTopicForUserActivity(whom.String()), mqttStruct.MESSAGE_AI_NUDGE) {
		var e nudgeEvent
		if err := json.Unmarshal(raw, &e); err != nil {
			t.Fatal(err)
		}
		out = append(out, e)
	}
	return out
}

func TestAFailedNudgeReadClosesNothing(t *testing.T) {
	ctx := context.Background()
	env := integration.SetupEnv(t)
	if err := postgresInit.ConnectPostgres(ctx, env.DSN); err != nil {
		t.Fatal(err)
	}
	dg := integration.SetupDgraph(t)
	broker := integration.RecordMqtt(t)

	// Two members, each nudged already of an overdue commitment and a stale
	// question.
	ana, anaChannel := nudgeMember(t, env, dg, "ana")
	ben, benChannel := nudgeMember(t, env, dg, "ben")
	for _, m := range [][2]uuid.UUID{{ana, anaChannel}, {ben, benChannel}} {
		c := memoryItemFor(t, env, m[0], m[1], "commitment", "Send the venue deposit", "1 day", "NOW() - interval '2 days'")
		q := memoryItemFor(t, env, m[0], m[1], "question", "Who books the photographer?", "10 days", "NULL")
		nudgeRow(t, env, m[0], c, "overdue_commitment", "open", "1 hour")
		nudgeRow(t, env, m[0], q, "stale_question", "open", "1 hour")
	}
	before := nudgeStates(t, env, ana, ben)

	listActionable = func(context.Context, time.Time, time.Time, int) ([]memoryModels.DigestItem, error) {
		return nil, errors.New("the actionable items are unread")
	}
	t.Cleanup(func() { listActionable = memoryModels.ListActionableForDigest })
	raiseNudges(ctx, time.Now())
	if got := nudgeStates(t, env, ana, ben); !maps.Equal(got, before) {
		t.Errorf("after a pass whose read failed the nudges are %v; want them as they were, %v", got, before)
	}

	// The next good pass finds the same signals: the same nudges stay open,
	// none is new.
	listActionable = memoryModels.ListActionableForDigest
	raiseNudges(ctx, time.Now())
	if got := nudgeStates(t, env, ana, ben); !maps.Equal(got, before) {
		t.Errorf("after the next good pass the nudges are %v; want the same ones open, none new, %v", got, before)
	}
	for _, who := range []uuid.UUID{ana, ben} {
		if events := nudgeEvents(t, broker, who); len(events) > 0 {
			t.Errorf("%s was told %+v; want nothing: no nudge of theirs closed or came new", who, events)
		}
	}
}

func TestAFailedScopeLookupHoldsTheNudge(t *testing.T) {
	ctx := context.Background()
	env := integration.SetupEnv(t)
	if err := postgresInit.ConnectPostgres(ctx, env.DSN); err != nil {
		t.Fatal(err)
	}
	dg := integration.SetupDgraph(t)
	broker := integration.RecordMqtt(t)

	// Ana is nudged of a commitment that still needs her, and of an item
	// that is gone.
	ana, anaChannel := nudgeMember(t, env, dg, "ana")
	live := memoryItemFor(t, env, ana, anaChannel, "commitment", "Send the venue deposit", "1 day", "NOW() - interval '2 days'")
	liveNudge := nudgeRow(t, env, ana, live, "overdue_commitment", "open", "1 hour")
	goneNudge := nudgeRow(t, env, ana, uuid.New(), "stale_question", "open", "1 hour")

	// Where she is can't be read this pass.
	ownerProfileNow = func(ctx context.Context, id string) (*dgraphStruct.DgraphUser, error) {
		if id == ana.String() {
			return nil, errors.New("the graph is unreachable")
		}
		return userDomain.GetActiveDgraphUserInfoByUUID(ctx, id)
	}
	t.Cleanup(func() { ownerProfileNow = userDomain.GetActiveDgraphUserInfoByUUID })
	raiseNudges(ctx, time.Now())

	got := nudgeStates(t, env, ana)
	if got[liveNudge] != "open" {
		t.Errorf("the nudge of the commitment whose scope was unread is %q; want it held open as it was", got[liveNudge])
	}
	if got[goneNudge] != "superseded" {
		t.Errorf("the nudge of the gone item is %q; want it superseded: the hold is for the unread item only", got[goneNudge])
	}
	if len(got) != 2 {
		t.Errorf("ana has nudges %v; want only the two she had", got)
	}
	if owner := memoryItemOwner(t, env, live); owner == nil || *owner != ana {
		t.Errorf("the commitment's owner is %v; want ana still: an unread scope is not leaving it", owner)
	}
	for _, e := range nudgeEvents(t, broker, ana) {
		if e.Action != "cleared" || len(e.NudgeIDs) != 1 || e.NudgeIDs[0] != goneNudge.String() {
			t.Errorf("ana was told %+v; want one cleared naming only the gone item's nudge %s", e, goneNudge)
		}
	}
}

// memoryItemOwner is the memory item's owner now: nil once detached.
func memoryItemOwner(t *testing.T, env *integration.Env, item uuid.UUID) *uuid.UUID {
	t.Helper()
	var owner *uuid.UUID
	if err := env.PG.QueryRow(`SELECT owner_user_id FROM workspace_memory_items WHERE id = $1`, item).Scan(&owner); err != nil {
		t.Fatal(err)
	}
	return owner
}

// assertOneCleared fails unless whom was told exactly one cleared, naming
// only nudge.
func assertOneCleared(t *testing.T, broker *integration.MqttRecorder, whom, nudge uuid.UUID) {
	t.Helper()
	events := nudgeEvents(t, broker, whom)
	if len(events) != 1 || events[0].Action != "cleared" || len(events[0].NudgeIDs) != 1 || events[0].NudgeIDs[0] != nudge.String() {
		t.Errorf("%s was told %+v; want one cleared naming only %s", whom, events, nudge)
	}
}

func TestAGoneMembersNudgeCloses(t *testing.T) {
	ctx := context.Background()
	env := integration.SetupEnv(t)
	if err := postgresInit.ConnectPostgres(ctx, env.DSN); err != nil {
		t.Fatal(err)
	}
	integration.SetupDgraph(t)
	broker := integration.RecordMqtt(t)

	// Gil was deleted: his Postgres row is left, his live profile is not.
	// He still owns an overdue commitment, and its nudge is open.
	gil := uuid.New()
	if _, err := env.PG.Exec(`INSERT INTO users (id, email_id, username, display_name, created_at, updated_at)
		VALUES ($1, 'gil@example.test', 'gil', 'gil', NOW(), NOW())`, gil); err != nil {
		t.Fatal(err)
	}
	item := memoryItemFor(t, env, gil, uuid.New(), "commitment", "Send the venue deposit", "1 day", "NOW() - interval '2 days'")
	nudge := nudgeRow(t, env, gil, item, "overdue_commitment", "open", "1 hour")
	raiseNudges(ctx, time.Now())

	// A profile read as gone is a read, not an unread one: the nudge closes.
	if got := nudgeStates(t, env, gil); got[nudge] != "superseded" || len(got) != 1 {
		t.Errorf("gil's nudges are %v; want only %s, superseded: a gone profile closes it, it is not held", got, nudge)
	}
	if owner := memoryItemOwner(t, env, item); owner == nil || *owner != gil {
		t.Errorf("the commitment's owner is %v; want gil still: a gone profile is not leaving the item", owner)
	}
	assertOneCleared(t, broker, gil, nudge)
}

func TestAMemberWhoLeftLosesTheNudge(t *testing.T) {
	ctx := context.Background()
	env := integration.SetupEnv(t)
	if err := postgresInit.ConnectPostgres(ctx, env.DSN); err != nil {
		t.Fatal(err)
	}
	dg := integration.SetupDgraph(t)
	broker := integration.RecordMqtt(t)

	// Ana's profile reads, but her commitment lives in a channel that is no
	// longer one of hers: she left it.
	ana, _ := nudgeMember(t, env, dg, "ana")
	item := memoryItemFor(t, env, ana, uuid.New(), "commitment", "Send the venue deposit", "1 day", "NOW() - interval '2 days'")
	nudge := nudgeRow(t, env, ana, item, "overdue_commitment", "open", "1 hour")
	raiseNudges(ctx, time.Now())

	// Read as left: the nudge closes and the item is no longer hers.
	if got := nudgeStates(t, env, ana); got[nudge] != "superseded" || len(got) != 1 {
		t.Errorf("ana's nudges are %v; want only %s, superseded: she left where it lives", got, nudge)
	}
	if owner := memoryItemOwner(t, env, item); owner != nil {
		t.Errorf("the commitment's owner is %v; want none: an owner read as left is detached", *owner)
	}
	assertOneCleared(t, broker, ana, nudge)
}

func TestACappedNudgeReadClosesNothing(t *testing.T) {
	ctx := context.Background()
	env := integration.SetupEnv(t)
	if err := postgresInit.ConnectPostgres(ctx, env.DSN); err != nil {
		t.Fatal(err)
	}
	dg := integration.SetupDgraph(t)
	broker := integration.RecordMqtt(t)

	// Ana is nudged of an item that may lie past the cap, and has a new
	// stale question.
	ana, anaChannel := nudgeMember(t, env, dg, "ana")
	pastCap := nudgeRow(t, env, ana, uuid.New(), "stale_question", "open", "1 hour")
	fresh := memoryItemFor(t, env, ana, anaChannel, "question", "Who books the photographer?", "10 days", "NULL")

	// The read comes back full: what lies past the cap is unread.
	ghost := uuid.New()
	listActionable = func(ctx context.Context, overdueBefore, staleBefore time.Time, limit int) ([]memoryModels.DigestItem, error) {
		items, err := memoryModels.ListActionableForDigest(ctx, overdueBefore, staleBefore, limit)
		for len(items) < limit {
			items = append(items, memoryModels.DigestItem{OwnerID: ghost, Item: &memoryModels.MemoryItem{ID: uuid.New(), Kind: memoryModels.KindQuestion}})
		}
		return items, err
	}
	t.Cleanup(func() { listActionable = memoryModels.ListActionableForDigest })
	raiseNudges(ctx, time.Now())

	got := nudgeStates(t, env, ana)
	if got[pastCap] != "open" {
		t.Errorf("the nudge of the item past the cap is %q; want it open as it was", got[pastCap])
	}
	var raised uuid.UUID
	if err := env.PG.QueryRow(`SELECT id FROM workspace_nudges WHERE user_id = $1 AND source_id = $2 AND status = 'open'`, ana, fresh.String()).Scan(&raised); err != nil {
		t.Fatalf("the new question's nudge: %v; want it raised all the same", err)
	}
	events := nudgeEvents(t, broker, ana)
	if len(events) != 1 || events[0].Action != "new" || events[0].NudgeID != raised.String() {
		t.Errorf("ana was told %+v; want one new naming %s and no cleared", events, raised)
	}
}

func TestTheNudgePassIsFlatInPeople(t *testing.T) {
	ctx := context.Background()
	env := integration.SetupEnv(t)
	if err := postgresInit.ConnectPostgres(ctx, env.DSN); err != nil {
		t.Fatal(err)
	}
	dg := integration.SetupDgraph(t)
	calls := integration.CountStoreCalls(t, env, dg.Endpoint)
	broker := integration.RecordMqtt(t)

	// people writes n members with a new stale question each, and n whose
	// one open nudge's item is gone.
	people := func(n int, tag string) (raised, resolved []uuid.UUID, gone map[uuid.UUID]uuid.UUID) {
		gone = map[uuid.UUID]uuid.UUID{}
		for i := range n {
			who, channel := nudgeMember(t, env, dg, fmt.Sprintf("raised%s%d", tag, i))
			memoryItemFor(t, env, who, channel, "question", "Who books the photographer?", "10 days", "NULL")
			raised = append(raised, who)
			done, _ := nudgeMember(t, env, dg, fmt.Sprintf("resolved%s%d", tag, i))
			gone[done] = nudgeRow(t, env, done, uuid.New(), "stale_question", "open", "1 hour")
			resolved = append(resolved, done)
		}
		return raised, resolved, gone
	}
	// pass runs one pass and answers the store calls it made: the
	// statements on the nudges, every statement, and the graph calls.
	pass := func() (nudges, all, graph int) {
		if busy := helpers.SettleBackground(10 * time.Second); len(busy) > 0 {
			t.Fatalf("still at work: %v", busy)
		}
		calls.Reset()
		raiseNudges(calls.Context(ctx), time.Now())
		if busy := helpers.SettleBackground(10 * time.Second); len(busy) > 0 {
			t.Fatalf("still at work: %v", busy)
		}
		return calls.Sent("workspace_nudges"), calls.Statements(), calls.Graph()
	}
	check := func(raised, resolved []uuid.UUID, gone map[uuid.UUID]uuid.UUID) {
		t.Helper()
		for _, who := range raised {
			events := nudgeEvents(t, broker, who)
			if len(events) != 1 || events[0].Action != "new" {
				t.Errorf("%s was told %+v; want one new", who, events)
			}
		}
		for _, who := range resolved {
			if got := nudgeStates(t, env, who)[gone[who]]; got != "superseded" {
				t.Errorf("%s's nudge of a gone item is %q; want superseded", who, got)
			}
			assertOneCleared(t, broker, who, gone[who])
		}
	}

	raised1, resolved1, gone1 := people(1, "a")
	nudges1, all1, graph1 := pass()
	check(raised1, resolved1, gone1)
	raised4, resolved4, gone4 := people(4, "b")
	nudges4, all4, graph4 := pass()
	check(raised4, resolved4, gone4)

	t.Logf("a pass for 1+1 people sent %d statements (%d on the nudges) and %d graph calls; for 5+4, %d (%d) and %d",
		all1, nudges1, graph1, all4, nudges4, graph4)
	if nudges4 != nudges1 {
		t.Errorf("a pass for 5 raised + 4 resolved people sent %d statements on the nudges; for 1 + 1, %d: want the same, one per step for everyone", nudges4, nudges1)
	}
}
