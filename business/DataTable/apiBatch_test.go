package business

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"slices"
	"strings"
	"testing"

	"github.com/google/uuid"

	auditBusiness "github.com/akashc777/OneCamp/business/AdminAudit"
	v1api "github.com/akashc777/OneCamp/business/PublicAPI"
	rowLedger "github.com/akashc777/OneCamp/business/RowLedger"
	"github.com/akashc777/OneCamp/helpers"
	"github.com/akashc777/OneCamp/helpers/idempotency"
	userModels "github.com/akashc777/OneCamp/models/postgres/User"
)

// fakeRecord is a batch's record in memory; failMark names items whose
// record doesn't take.
type fakeRecord struct {
	done     map[int]json.RawMessage
	marked   []int
	failMark map[int]bool
}

func newRecord() *fakeRecord {
	return &fakeRecord{done: map[int]json.RawMessage{}, failMark: map[int]bool{}}
}

func (r *fakeRecord) Done(i int) (json.RawMessage, bool) { a, ok := r.done[i]; return a, ok }

func (r *fakeRecord) Mark(_ context.Context, i int, a json.RawMessage) error {
	if r.failMark[i] {
		return errors.New("redis: OOM command not allowed")
	}
	r.done[i], r.marked = a, append(r.marked, i)
	return nil
}

func (r *fakeRecord) Release(context.Context) {}

// batchRun is a batch of three fake items (create[0], update[0], delete[0])
// and the trail of what the engine did with them, in order: each item's
// transaction (begin, then commit or rollback), its write, and its audit
// rows (by action, with the name its write stored).
type batchRun struct {
	trail     []string
	steps     []batchStep
	auditErr  map[string]error
	commitErr map[string]error
	// unchanged items' writes store nothing new.
	unchanged map[string]bool
}

func newBatchRun(checks, applies map[string]error) *batchRun {
	b := &batchRun{auditErr: map[string]error{}, commitErr: map[string]error{}, unchanged: map[string]bool{}}
	for _, list := range []string{"create", "update", "delete"} {
		at, id := list+"[0]", uuid.NewSHA1(uuid.Nil, []byte(list)).String()
		b.steps = append(b.steps, batchStep{
			item: batchItem{List: list, Index: 0, ID: id}, action: "table.field." + list + "d",
			check: func(context.Context) error { b.trail = append(b.trail, "check "+at); return checks[at] },
			apply: func(context.Context) (itemDone, error) {
				b.trail = append(b.trail, "apply "+at)
				if err := applies[at]; err != nil {
					return itemDone{}, err
				}
				done := itemDone{answer: map[string]string{"id": id}}
				if !b.unchanged[at] {
					// What the store took, not what was sent.
					done.summary, done.facts = "stored "+at, map[string]any{"id": id, "name": "stored " + at}
				}
				return done, nil
			},
		})
	}
	return b
}

func (b *batchRun) run(rec batchRecord) (any, error) {
	audit := func(_ context.Context, s batchStep, done itemDone, action string) error {
		at := fmt.Sprintf("%s[%d]", s.item.List, s.item.Index)
		b.trail = append(b.trail, fmt.Sprintf("audit %s %s %v", action, at, done.facts["name"]))
		if action != s.action {
			return nil
		}
		return b.auditErr[at]
	}
	was := itemTx
	defer func() { itemTx = was }()
	itemTx = func(ctx context.Context, fn func(ctx context.Context) error) error {
		b.trail = append(b.trail, "begin")
		err := fn(ctx)
		item := b.lastApplied()
		if err == nil {
			err = b.commitErr[item]
		}
		if err != nil {
			b.trail = append(b.trail, "rollback "+item)
			return err
		}
		b.trail = append(b.trail, "commit "+item)
		return nil
	}
	says := func(err error) error { return schemaErr(context.Background(), uuid.Nil, Actor{}, err) }
	return runSteps(context.Background(), rec, b.steps, audit, says)
}

// lastApplied is the item whose write the trail shows last.
func (b *batchRun) lastApplied() string {
	applied := b.did("apply ")
	if len(applied) == 0 {
		return ""
	}
	return applied[len(applied)-1]
}

func (b *batchRun) did(prefix string) []string {
	var out []string
	for _, s := range b.trail {
		if strings.HasPrefix(s, prefix) {
			out = append(out, strings.TrimPrefix(s, prefix))
		}
	}
	return out
}

func asAPIError(t *testing.T, err error) *v1api.Error {
	t.Helper()
	var e *v1api.Error
	if !errors.As(err, &e) {
		t.Fatalf("%v isn't the API's error body (a 500)", err)
	}
	return e
}

// stoppedAt is a stopped batch's details: the items done and not done.
func stoppedAt(t *testing.T, e *v1api.Error) (done, notDone []string) {
	t.Helper()
	d, ok := e.Details.(map[string][]batchItem)
	if !ok {
		t.Fatalf("a stopped batch's details are %#v, want done and not_done", e.Details)
	}
	for _, it := range d["done"] {
		done = append(done, fmt.Sprintf("%s[%d]", it.List, it.Index))
	}
	for _, it := range d["not_done"] {
		notDone = append(notDone, fmt.Sprintf("%s[%d]", it.List, it.Index))
	}
	return done, notDone
}

// Main's ruling (i), 1: every item is checked before any is applied, and one
// bad item refuses the call: a 400 whose details name each item refused and
// why, with nothing audited, applied or recorded.
func TestABatchIsCheckedWholeBeforeAnyItemIsApplied(t *testing.T) {
	b := newBatchRun(map[string]error{"update[0]": badInput("this field is named twice in the batch: name each once"),
		"delete[0]": errFieldNotFound}, nil)
	rec := newRecord()
	_, err := b.run(rec)
	e := asAPIError(t, err)
	refused, _ := e.Details.([]batchItem)
	if e.Status != http.StatusBadRequest || e.Code != "invalid_argument" || len(refused) != 2 ||
		refused[0].List != "update" || refused[0].Message != "this field is named twice in the batch: name each once" ||
		refused[1].List != "delete" || refused[1].Message != "there is no such field" || refused[1].ID == "" {
		t.Errorf("a batch with two bad items answered %d %s %+v; want 400 naming both, in the API's words", e.Status, e.Code, e.Details)
	}
	if got := b.did(""); !slices.Equal(got, []string{"check create[0]", "check update[0]", "check delete[0]"}) || len(rec.marked) != 0 {
		t.Errorf("a refused batch did %v and recorded %v; want the three checks alone", got, rec.marked)
	}
}

// Checked whole, a batch is applied in order, creates then updates then
// deletes. Each item's write and its audit row, built from what the write
// stored, share one transaction (main's audit rule, 3 Nov), and the item is
// recorded done once it commits; the answer gives each item's answer by list.
func TestABatchAppliesEachItemWithItsAuditRowInOneTransaction(t *testing.T) {
	b := newBatchRun(nil, nil)
	rec := newRecord()
	v, err := b.run(rec)
	if err != nil {
		t.Fatal(err)
	}
	want := []string{"check create[0]", "check update[0]", "check delete[0]",
		"begin", "apply create[0]", "audit table.field.created create[0] stored create[0]", "commit create[0]",
		"begin", "apply update[0]", "audit table.field.updated update[0] stored update[0]", "commit update[0]",
		"begin", "apply delete[0]", "audit table.field.deleted delete[0] stored delete[0]", "commit delete[0]"}
	if got := b.did(""); !slices.Equal(got, want) || !slices.Equal(rec.marked, []int{0, 1, 2}) {
		t.Errorf("the batch did %v and recorded %v; want %v and all three", got, rec.marked, want)
	}
	out, _ := v.(apiBatchDone)
	if len(out.Created) != 1 || len(out.Updated) != 1 || len(out.Deleted) != 1 || !strings.Contains(string(out.Updated[0]), b.steps[1].item.ID) {
		t.Errorf("the batch answered %+v; want each item's answer under its list", v)
	}
}

// Main's ruling (i), 2: the same batch sent again with its key skips the
// items it did, unchecked (a field it deleted is no longer there to check),
// and answers them as it did.
func TestABatchSentAgainSkipsTheItemsItDid(t *testing.T) {
	b := newBatchRun(map[string]error{"create[0]": errFieldNotFound}, nil)
	rec := newRecord()
	rec.done[0] = json.RawMessage(`{"id":"made the first time"}`)
	v, err := b.run(rec)
	if err != nil {
		t.Fatal(err)
	}
	if got := b.did(""); slices.Contains(got, "check create[0]") || slices.Contains(got, "apply create[0]") || slices.Contains(got, "audit create[0]") {
		t.Errorf("the batch sent again did %v; want create[0] skipped", got)
	}
	if out, _ := v.(apiBatchDone); len(out.Created) != 1 || string(out.Created[0]) != `{"id":"made the first time"}` {
		t.Errorf("the batch sent again answered %+v; want create[0] answered as before", v)
	}
}

// A create the store made but the record missed is found by the id the key
// gives it, and answered as made before: never checked, audited or made
// twice. One whose finding fails stops the batch before anything is applied.
func TestACreateMadeBeforeIsNotMadeTwice(t *testing.T) {
	b := newBatchRun(nil, nil)
	b.steps[0].made = func(context.Context) (bool, error) { return true, nil }
	rec := newRecord()
	v, err := b.run(rec)
	if err != nil {
		t.Fatal(err)
	}
	if got := b.did(""); slices.Contains(got, "check create[0]") || slices.Contains(got, "apply create[0]") || slices.Contains(got, "audit create[0]") ||
		!slices.Equal(rec.marked, []int{0, 1, 2}) {
		t.Errorf("a create made before: the batch did %v and recorded %v; want it answered alone, and recorded", got, rec.marked)
	}
	out, _ := v.(apiBatchDone)
	var made apiMadeBefore
	if len(out.Created) != 1 || json.Unmarshal(out.Created[0], &made) != nil || !made.MadeBefore || made.ID != b.steps[0].item.ID {
		t.Errorf("a create made before answered %+v; want its id and made_before", v)
	}

	b = newBatchRun(nil, nil)
	b.steps[0].made = func(context.Context) (bool, error) { return false, errors.New("pq: connection reset") }
	_, err = b.run(newRecord())
	if e := asAPIError(t, err); e.Status != http.StatusServiceUnavailable || len(b.did("apply ")) != 0 {
		t.Errorf("a create whose finding failed: %d, did %v; want 503 with nothing applied", e.Status, b.trail)
	}
}

// Main's ruling (i), 2: a store failure part way stops the batch, and the
// 503 says when to send it again and names the items done and not done.
func TestABatchStoppedByTheStoreNamesWhatItDid(t *testing.T) {
	b := newBatchRun(nil, map[string]error{"update[0]": storeFailed("failed to update field")})
	rec := newRecord()
	_, err := b.run(rec)
	e := asAPIError(t, err)
	done, notDone := stoppedAt(t, e)
	if e.Status != http.StatusServiceUnavailable || e.Code != "unavailable" || e.RetryAfter != retryRows ||
		!slices.Equal(done, []string{"create[0]"}) || !slices.Equal(notDone, []string{"update[0]", "delete[0]"}) {
		t.Errorf("a batch the store stopped answered %d %s retry %d, done %v, not done %v; want 503, create done, the rest not",
			e.Status, e.Code, e.RetryAfter, done, notDone)
	}
	if strings.Contains(e.Message, "failed to update field") || slices.Contains(b.trail, "apply delete[0]") || !slices.Equal(rec.marked, []int{0}) {
		t.Errorf("a stopped batch said %q, did %v and recorded %v; want the API's words, nothing after the failure", e.Message, b.trail, rec.marked)
	}
}

// No record, no action: an item whose audit row can't be written is rolled
// back with it, and the batch stops there for the server's reason.
func TestAnItemThatCantBeAuditedIsRolledBack(t *testing.T) {
	b := newBatchRun(nil, nil)
	b.auditErr["update[0]"] = errors.New("pq: the audit chain's lock timed out")
	rec := newRecord()
	_, err := b.run(rec)
	e := asAPIError(t, err)
	done, notDone := stoppedAt(t, e)
	if e.Status != http.StatusServiceUnavailable || !slices.Contains(b.trail, "rollback update[0]") || slices.Contains(b.trail, "commit update[0]") ||
		!slices.Equal(done, []string{"create[0]"}) || !slices.Equal(notDone, []string{"update[0]", "delete[0]"}) || !slices.Equal(rec.marked, []int{0}) {
		t.Errorf("an item that couldn't be audited: %d, did %v, done %v, not done %v, recorded %v; want 503, it rolled back and not done",
			e.Status, b.trail, done, notDone, rec.marked)
	}
	if strings.Contains(e.Message, "pq:") || slices.Contains(b.trail, "audit table.field.updated_failed update[0] stored update[0]") {
		t.Errorf("an item that couldn't be audited said %q and did %v; want the API's words and no _failed row (nothing was recorded)", e.Message, b.trail)
	}
}

// An item whose write fails leaves no audit row at all: nothing claims it
// happened.
func TestAnItemWhoseWriteFailsWritesNoRow(t *testing.T) {
	b := newBatchRun(nil, map[string]error{"update[0]": storeFailed("failed to update field")})
	_, err := b.run(newRecord())
	if e := asAPIError(t, err); e.Status != http.StatusServiceUnavailable {
		t.Errorf("an item whose write failed answered %d; want 503", e.Status)
	}
	for _, step := range b.trail {
		if strings.HasPrefix(step, "audit ") && strings.Contains(step, "update[0]") {
			t.Errorf("an item whose write failed wrote %q; want no row for it (trail %v)", step, b.trail)
		}
	}
	if !slices.Contains(b.trail, "rollback update[0]") {
		t.Errorf("an item whose write failed did %v; want its transaction rolled back", b.trail)
	}
}

// THE PIN (main's audit rule, 3 Nov): an item whose write fails AFTER its
// audit row is written (its transaction's commit) is undone, and a "_failed"
// row follows its row, so the log never says a change took that didn't. The
// batch stops there in the API's words.
func TestAnItemWhoseWriteFailsAfterItsRowIsRecordedAsFailed(t *testing.T) {
	b := newBatchRun(nil, nil)
	b.commitErr["update[0]"] = errors.New("pq: could not serialize access")
	rec := newRecord()
	_, err := b.run(rec)
	e := asAPIError(t, err)
	done, notDone := stoppedAt(t, e)
	want := []string{"begin", "apply update[0]", "audit table.field.updated update[0] stored update[0]", "rollback update[0]",
		"audit table.field.updated_failed update[0] stored update[0]"}
	got := b.trail[slices.Index(b.trail, "apply update[0]")-1:]
	if !slices.Equal(got, want) {
		t.Errorf("an item whose commit failed did %v; want %v", got, want)
	}
	if e.Status != http.StatusServiceUnavailable || strings.Contains(e.Message, "pq:") || !slices.Equal(done, []string{"create[0]"}) ||
		!slices.Equal(notDone, []string{"update[0]", "delete[0]"}) || !slices.Equal(rec.marked, []int{0}) {
		t.Errorf("an item whose commit failed: %d %q, done %v, not done %v, recorded %v; want 503 in the API's words and it not done",
			e.Status, e.Message, done, notDone, rec.marked)
	}
}

// An item that changed nothing writes no row, and its transaction rolls back
// (nothing it touched moves); it still counts done with its answer.
func TestAnItemThatChangedNothingWritesNoRow(t *testing.T) {
	b := newBatchRun(nil, nil)
	b.unchanged["update[0]"] = true
	rec := newRecord()
	v, err := b.run(rec)
	if err != nil {
		t.Fatal(err)
	}
	for _, step := range b.trail {
		if strings.HasPrefix(step, "audit ") && strings.Contains(step, "update[0]") {
			t.Errorf("an item that changed nothing wrote %q; want no row (trail %v)", step, b.trail)
		}
	}
	out, _ := v.(apiBatchDone)
	if !slices.Contains(b.trail, "rollback update[0]") || !slices.Equal(rec.marked, []int{0, 1, 2}) || len(out.Updated) != 1 ||
		!strings.Contains(string(out.Updated[0]), b.steps[1].item.ID) {
		t.Errorf("an item that changed nothing did %v, recorded %v, answered %+v; want it rolled back, done and answered", b.trail, rec.marked, v)
	}
}

// An item whose write took counts as done though its record didn't: the
// 503 says so, so the caller knows what the table holds.
func TestAnItemTheRecordMissedStillCountsDone(t *testing.T) {
	b := newBatchRun(nil, nil)
	rec := newRecord()
	rec.failMark[0] = true
	_, err := b.run(rec)
	e := asAPIError(t, err)
	done, notDone := stoppedAt(t, e)
	if e.Status != http.StatusServiceUnavailable || slices.Contains(b.trail, "apply update[0]") ||
		!slices.Equal(done, []string{"create[0]"}) || !slices.Equal(notDone, []string{"update[0]", "delete[0]"}) {
		t.Errorf("an item the record missed: %d, did %v, done %v, not done %v; want 503 with it done", e.Status, b.trail, done, notDone)
	}
}

// A refusal the table gives only at the item's turn (it changed under the
// batch) keeps its own status and words, naming the item and what was done.
func TestARefusalAtItsTurnKeepsItsStatus(t *testing.T) {
	b := newBatchRun(nil, map[string]error{"update[0]": errViewLocked})
	_, err := b.run(newRecord())
	e := asAPIError(t, err)
	done, notDone := stoppedAt(t, e)
	if e.Status != http.StatusConflict || e.Code != "locked" || !strings.HasPrefix(e.Message, "update[0] was refused at its turn: ") ||
		!slices.Equal(done, []string{"create[0]"}) || !slices.Equal(notDone, []string{"update[0]", "delete[0]"}) {
		t.Errorf("a lock at the item's turn answered %d %s %q, done %v, not done %v", e.Status, e.Code, e.Message, done, notDone)
	}
}

// A batch's key refused is said in the API's words: another batch's key is
// Idempotency-Key's 422, one still being applied 409, and a record that
// can't be kept a 503 before anything ran.
func TestABatchsKeyIsRefusedInTheAPIsWords(t *testing.T) {
	for _, c := range []struct {
		err    error
		status int
		code   string
	}{
		{idempotency.ErrItemsReused, http.StatusUnprocessableEntity, "idempotency_key_reused"},
		{idempotency.ErrItemsInProgress, http.StatusConflict, "in_progress"},
		{idempotency.ErrItemsUnavailable, http.StatusServiceUnavailable, "unavailable"},
	} {
		e := asAPIError(t, batchKeyErr(c.err))
		if e.Status != c.status || e.Code != c.code || (c.status == http.StatusServiceUnavailable) != (e.RetryAfter > 0) {
			t.Errorf("%v answered %d %s retry %d, want %d %s", c.err, e.Status, e.Code, e.RetryAfter, c.status, c.code)
		}
	}
}

// A batch's creates take ids from its key: the same batch, the same ids;
// another key, caller, batch or place, another id.
func TestABatchsCreatesTakeIDsFromItsKey(t *testing.T) {
	k := batchKeys{caller: "tok-1", route: "tables.fields.batch", key: "k1", hash: "h1"}
	id := k.createID("create", 0)
	if id != k.createID("create", 0) || id.Version() != 5 {
		t.Fatalf("the same create took %s then %s (version %d)", id, k.createID("create", 0), id.Version())
	}
	for _, other := range []batchKeys{{"tok-2", k.route, k.key, k.hash}, {k.caller, "tables.views.batch", k.key, k.hash},
		{k.caller, k.route, "k2", k.hash}, {k.caller, k.route, k.key, "h2"}} {
		if other.createID("create", 0) == id {
			t.Errorf("%+v gives create[0] the id %+v's does", other, k)
		}
	}
	if k.createID("create", 1) == id {
		t.Error("create[1] takes create[0]'s id")
	}
}

// Condition (e) and ruling (i), 3: an item's audit row says who asked (the
// token's member), the agent and the token, the batch's op, key and change
// id, and the item; one that can't be written is the server's failure.
func TestAnItemIsAuditedAsTheTokensMemberWithTheBatchsChange(t *testing.T) {
	member, agent, token, change, table := uuid.New(), uuid.New(), uuid.NewString(), uuid.New(), uuid.New()
	info := userModels.UserInfo{}
	info.UserPostgresInfo.Id, info.UserPostgresInfo.EmailID = member, "bo@example.test"
	ctx := context.WithValue(context.Background(), helpers.UserInfoContextKey, info)
	ctx = context.WithValue(ctx, helpers.ApiTokenIDContextKey, token)
	ctx = context.WithValue(ctx, helpers.ApiAgentIDContextKey, agent.String())
	ctx = helpers.WithCallChange(ctx, change)
	var got struct {
		actor                                  *uuid.UUID
		email, kind, action, category, summary string
		meta                                   map[string]any
	}
	was := recordItem
	t.Cleanup(func() { recordItem = was })
	recordItem = func(_ context.Context, actor *uuid.UUID, email, kind, action, category, summary string, meta map[string]any) error {
		got.actor, got.email, got.kind, got.action, got.category, got.summary, got.meta = actor, email, kind, action, category, summary, meta
		return nil
	}
	s := batchStep{item: batchItem{List: "update", Index: 2, ID: uuid.NewString()}, action: "table.field.updated"}
	done := itemDone{summary: "Changed the field “Budget” of a table through the API",
		facts: map[string]any{"id": s.item.ID, "name": "Budget", "type": "number", "changed": []string{"name"}, "cells_changed": 0}}
	if err := auditItem(ctx, Actor{UserID: member, Kind: rowLedger.ActorAgent}, "tables.fields.batch", "k1", table, s, done, s.action); err != nil {
		t.Fatal(err)
	}
	want := map[string]any{"op": "tables.fields.batch", "key": "k1", "list": "update", "index": 2, "table_id": table.String(), "id": s.item.ID,
		"token_id": token, "agent_id": agent.String(), "change_id": change.String()}
	if got.actor == nil || *got.actor != member || got.email != "bo@example.test" || got.kind != auditBusiness.ActorAgent ||
		got.action != "table.field.updated" || got.category != auditBusiness.CategoryTables || got.summary != done.summary ||
		got.meta["name"] != "Budget" || got.meta["type"] != "number" || !slices.Equal(got.meta["changed"].([]string), []string{"name"}) {
		t.Errorf("the item's row is by %v %q %s, %s in %s; want Bo, an agent, table.field.updated in tables", got.actor, got.email, got.kind, got.action, got.category)
	}
	for k, v := range want {
		if fmt.Sprint(got.meta[k]) != fmt.Sprint(v) {
			t.Errorf("the item's row has %s %v, want %v", k, got.meta[k], v)
		}
	}
	recordItem = func(context.Context, *uuid.UUID, string, string, string, string, string, map[string]any) error {
		return errors.New("pq: could not serialize access")
	}
	if err := auditItem(ctx, Actor{UserID: member}, "tables.fields.batch", "k1", table, s, done, s.action); !errors.Is(err, errBatchAudit) {
		t.Errorf("an audit row that couldn't be written: %v, want errBatchAudit", err)
	}
}

// A batch holds something, and at most maxSchemaBatch items in all; an id
// that isn't one is the caller's to fix.
func TestABatchsListsAreCounted(t *testing.T) {
	var bad *inputError
	if _, _, _, err := batchLists(map[string]any{}); !errors.As(err, &bad) {
		t.Errorf("an empty batch: %v, want the caller's to fix", err)
	}
	many := make([]any, maxSchemaBatch+1)
	for i := range many {
		many[i] = uuid.NewString()
	}
	if _, _, _, err := batchLists(map[string]any{"delete": many}); !errors.As(err, &bad) || !strings.Contains(err.Error(), fmt.Sprint(maxSchemaBatch)) {
		t.Errorf("a batch of %d: %v, want refused at %d", len(many), err, maxSchemaBatch)
	}
	if _, _, _, err := batchLists(map[string]any{"delete": []any{"not an id"}}); asAPIError(t, err).Status != http.StatusBadRequest {
		t.Errorf("a delete that isn't an id: %v, want 400", err)
	}
	c, u, d, err := batchLists(map[string]any{"create": []any{map[string]any{"name": "A"}}, "update": []any{map[string]any{"field": uuid.NewString()}}, "delete": []any{uuid.NewString()}})
	if err != nil || len(c) != 1 || len(u) != 1 || len(d) != 1 {
		t.Errorf("a batch of one of each: %d %d %d, %v", len(c), len(u), len(d), err)
	}
}

// A fields or views batch past maxSchemaBatch items in all is refused whole,
// before its record opens or any item is read or applied: the cap is what
// bounds applyItem's one transaction per item (main's ruling, 3 Nov), so
// serveBatch counts first. Exactly maxSchemaBatch items are taken.
func TestABatchPastTheCapIsRefusedBeforeAnythingIsApplied(t *testing.T) {
	wasOpen, wasTx := openItems, itemTx
	defer func() { openItems, itemTx = wasOpen, wasTx }()
	var reached []string
	openItems = func(_ context.Context, _, route, _, _ string) (batchRecord, error) {
		reached = append(reached, "openItems "+route)
		return nil, errors.New("opened")
	}
	itemTx = func(context.Context, func(context.Context) error) error {
		reached = append(reached, "itemTx")
		return errors.New("applied")
	}
	// spread is n items over the three lists, as a caller sends them.
	spread := func(n int, named string) map[string]any {
		var creates, updates, deletes []any
		for i := range n {
			switch i % 3 {
			case 0:
				creates = append(creates, map[string]any{"name": fmt.Sprintf("Item %d", i)})
			case 1:
				updates = append(updates, map[string]any{named: uuid.NewString(), "name": fmt.Sprintf("Item %d", i)})
			default:
				deletes = append(deletes, uuid.NewString())
			}
		}
		return map[string]any{"id": uuid.NewString(), "key": "past-the-cap", "create": creates, "update": updates, "delete": deletes}
	}
	// A member who may call: a count after the record opens would reach it.
	info := userModels.UserInfo{}
	info.UserPostgresInfo.Id = uuid.New()
	ctx := context.WithValue(context.Background(), helpers.UserInfoContextKey, info)
	for _, op := range []struct {
		route, named string
		serve        func(context.Context, v1api.Call) (any, error)
	}{{"tables.fields.batch", "field", serveFieldsBatch}, {"tables.views.batch", "view", serveViewsBatch}} {
		_, err := op.serve(ctx, v1api.Call{Args: spread(maxSchemaBatch+1, op.named)})
		want := fmt.Sprintf("at most %d items in all; this one has %d", maxSchemaBatch, maxSchemaBatch+1)
		if e := asAPIError(t, err); e.Status != http.StatusBadRequest || !strings.Contains(e.Message, want) {
			t.Errorf("%s with %d items: %d %q, want 400 naming the cap (%s)", op.route, maxSchemaBatch+1, e.Status, e.Message, want)
		}
	}
	if len(reached) != 0 {
		t.Errorf("a batch past the cap reached %v; want it refused before its record opens or any item applies", reached)
	}
	for _, named := range []string{"field", "view"} {
		c, u, d, err := batchLists(spread(maxSchemaBatch, named))
		if err != nil || len(c) != 34 || len(u) != 33 || len(d) != 33 {
			t.Errorf("a %s batch of exactly %d: %d %d %d, %v; want all taken", named, maxSchemaBatch, len(c), len(u), len(d), err)
		}
	}
}
