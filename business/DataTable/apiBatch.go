package business

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"strconv"

	"github.com/google/uuid"

	auditBusiness "github.com/akashc777/OneCamp/business/AdminAudit"
	v1api "github.com/akashc777/OneCamp/business/PublicAPI"
	"github.com/akashc777/OneCamp/helpers"
	"github.com/akashc777/OneCamp/helpers/idempotency"
	"github.com/akashc777/OneCamp/initializers/postgresInit"
	"github.com/akashc777/OneCamp/models/pgexec"
)

// A fields or views batch of the public API (tables H2b; main's ruling (i)
// of 3 Nov) is checked whole, then applied item by item through the app's own
// create, update and delete, in order: its creates, then its updates, then its
// deletes. It is not all-or-nothing, and says so:
//   - one item the check refuses refuses the call and nothing is applied (400,
//     each refused item in the details);
//   - a store failure part way stops it (503, the items done and not done in
//     the details), as does a refusal the table only gives at the item's turn
//     (the table changed under the batch: the refusal's own status).
//
// Its key makes the retry safe. Each item done is recorded under the key at
// once (helpers/idempotency Items), so the same batch sent again skips those
// and answers them as before. A create takes an id worked out from the key, so
// one the store made but the record missed is found by it, never made twice.

// maxSchemaBatch is how many items a fields or views batch takes in all.
const maxSchemaBatch = 100

// batchItem is an item of a batch as the API names it: its list ("create",
// "update" or "delete"), its place there, the id it names or makes, and why
// it was refused when it was.
type batchItem struct {
	List    string `json:"list"`
	Index   int    `json:"index"`
	ID      string `json:"id,omitempty"`
	Message string `json:"message,omitempty"`
}

// batchStep is an item ready to run: the app's own check of it against the
// table as it is, its audit row's action, and its write, which answers what
// it stored (itemDone). A create also says whether a send before made it
// (made, by the id it takes), as does a delete in a batch sent again (made:
// its target is in the trash).
type batchStep struct {
	item batchItem
	// action is its audit row's ("table.field.created").
	action string
	check  func(ctx context.Context) error
	made   func(ctx context.Context) (bool, error)
	// madeAnswer answers the item when made finds it done before: a
	// create's apiMadeBefore when none is set.
	madeAnswer any
	apply      func(ctx context.Context) (itemDone, error)
}

// itemDone is an item's write as the store took it: its answer, and its
// audit row's summary and facts, built from what was stored, never from the
// request. No facts: it changed nothing, so it writes no row.
type itemDone struct {
	answer  any
	summary string
	facts   map[string]any
}

// itemAudit writes an item's audit row as action: the step's, or its
// "_failed" row's.
type itemAudit func(ctx context.Context, s batchStep, done itemDone, action string) error

// batchRecord is what a batch keeps under its key (*idempotency.Items).
type batchRecord interface {
	Done(i int) (json.RawMessage, bool)
	Mark(ctx context.Context, i int, answer json.RawMessage) error
	Release(ctx context.Context)
	// Again: a send of the batch came before this one.
	Again() bool
	// Forget drops a first send's record when it stopped before any item
	// began, so its key isn't used up (a no-op once an item may have run).
	Forget(ctx context.Context)
}

// openItems opens a batch's record under its key; swapped by tests.
var openItems = func(ctx context.Context, caller, route, key, hash string) (batchRecord, error) {
	it, err := idempotency.OpenItems(ctx, caller, route, key, hash)
	if err != nil {
		return nil, err // never a typed nil in the interface
	}
	return it, nil
}

// recordItem writes an item's audit row; swapped by tests.
var recordItem = auditBusiness.RecordForPrincipal

// errBatchAudit and errBatchRecord stop a batch for the server's reason, in
// fixed words (never a store's own text): an item's audit row, or the record
// of an item done, that couldn't be written.
var (
	errBatchAudit  = storeFailed("failed to record the change")
	errBatchRecord = storeFailed("failed to keep the batch's record")
	// errItemSave is an item's transaction that couldn't be opened or
	// committed.
	errItemSave = storeFailed("failed to save the change")
)

// errUnchanged rolls back an item that changed nothing.
var errUnchanged = errors.New("the item changed nothing")

// itemTx runs an item's write and its audit row in one transaction, with the
// budget of a column's cells rewritten with it; swapped by tests.
var itemTx = func(ctx context.Context, fn func(ctx context.Context) error) error {
	return pgexec.InTxFor(ctx, postgresInit.BulkTimeout, fn)
}

// applyItem applies an item and writes its audit row in one transaction
// (main's audit rule, 3 Nov): the write, then its row, built from what the
// store took, then the commit. An item that changed nothing writes no row,
// and its transaction rolls back. A row that can't be written undoes the
// write (no record, no action). A commit that fails once the row is written
// is followed by an action+"_failed" row, so the log says the change didn't
// take. The audit chain has a writer of its own (models/AdminAudit), so the
// row is written inside the item's transaction, before its commit, as
// business/Retention's ChangeHold writes a hold's. What the write tells
// others waits for the commit (pgexec.AfterCommit).
func applyItem(ctx context.Context, s batchStep, audit itemAudit) (any, error) {
	var (
		done     itemDone
		applyErr error
		wrote    bool
	)
	err := itemTx(ctx, func(ctx context.Context) error {
		if done, applyErr = s.apply(ctx); applyErr != nil {
			return applyErr
		}
		if done.facts == nil {
			return errUnchanged
		}
		if err := audit(ctx, s, done, s.action); err != nil {
			return errBatchAudit
		}
		wrote = true
		return nil
	})
	switch {
	case err == nil, errors.Is(err, errUnchanged):
		return done.answer, nil
	case applyErr != nil:
		return nil, applyErr
	case errors.Is(err, errBatchAudit):
		return nil, errBatchAudit
	}
	helpers.LogErrorWithContext(ctx, "business/DataTable %s[%d]: save: %v", s.item.List, s.item.Index, err)
	if wrote {
		if ferr := audit(ctx, s, done, s.action+"_failed"); ferr != nil {
			helpers.LogErrorWithContext(ctx, "business/DataTable %s_failed wasn't recorded: %v", s.action, ferr)
		}
	}
	return nil, errItemSave
}

// batchKeys are what a batch's creates take their ids from: the caller, the
// op, the key and the batch's arguments (the same batch, the same ids); and
// whether a send of the batch came before this one (again), whose deletes
// may be done already.
type batchKeys struct {
	caller, route, key, hash string
	again                    bool
}

// batchSpace is the namespace a batch's creates take their ids in.
var batchSpace = uuid.MustParse("971539f6-9e71-4573-a3a2-5ee1b8694a3f")

// createID is the id the create at index of list takes (a UUIDv5).
func (k batchKeys) createID(list string, index int) uuid.UUID {
	return uuid.NewSHA1(batchSpace, []byte(k.caller+"|"+k.route+"|"+k.key+"|"+k.hash+"|"+list+"|"+strconv.Itoa(index)))
}

// batchCaller is whose keys a batch's key is among: the token's, or the
// member's when the call carries no token id.
func batchCaller(ctx context.Context, actor Actor) string {
	if id := helpers.GetApiTokenID(ctx); id != "" {
		return id
	}
	return "member:" + actor.UserID.String()
}

// createIDs are the ids a batch's n creates take, in order.
func (k batchKeys) createIDs(n int) []uuid.UUID {
	ids := make([]uuid.UUID, n)
	for i := range ids {
		ids[i] = k.createID("create", i)
	}
	return ids
}

// batchBuild builds a batch's steps from its three lists, counted already:
// what the items are checked against is read before any is checked.
type batchBuild func(ctx context.Context, actor Actor, table uuid.UUID, keys batchKeys, creates, updates []map[string]any, deletes []uuid.UUID) ([]batchStep, error)

// serveBatch runs a batch op: counted first, so a batch past maxSchemaBatch
// items is refused whole before anything is read, recorded or applied (the
// cap bounds applyItem's one transaction per item: main's ruling, 3 Nov);
// then its record opened under the call's key, its steps built from the
// call, checked whole, and applied in order.
func serveBatch(ctx context.Context, call v1api.Call, route string, build batchBuild) (any, error) {
	creates, updates, deletes, err := batchLists(call.Args)
	if err != nil {
		return nil, schemaErr(ctx, uuid.Nil, Actor{}, err)
	}
	member, table, err := tableCall(ctx, call)
	if err != nil {
		return nil, err
	}
	actor := actorBy(apiBy(ctx, member))
	key, _ := call.Args["key"].(string)
	keys := batchKeys{caller: batchCaller(ctx, actor), route: route, key: key, hash: idempotency.Fingerprint(call.Args)}
	rec, err := openItems(ctx, keys.caller, route, key, keys.hash)
	if err != nil {
		return nil, batchKeyErr(err)
	}
	defer rec.Release(ctx)
	keys.again = rec.Again()
	steps, err := build(ctx, actor, table, keys, creates, updates, deletes)
	if err != nil {
		rec.Forget(ctx) // nothing began
		return nil, schemaErr(ctx, table, actor, err)
	}
	audit := func(ctx context.Context, s batchStep, done itemDone, action string) error {
		return auditItem(ctx, actor, route, key, table, s, done, action)
	}
	return runSteps(ctx, rec, steps, audit, func(err error) error { return schemaErr(ctx, table, actor, err) })
}

// runSteps checks a batch whole, then applies it item by item: an item the
// record has done is answered as before, unchecked, and a create a send
// before made is answered by its id. Each item applied is written with its
// audit row in one transaction (applyItem) and recorded done at once. A batch
// stopped at its checks applied nothing: its record is forgotten (Forget).
func runSteps(ctx context.Context, rec batchRecord, steps []batchStep, audit itemAudit, says func(error) error) (res any, err error) {
	checked := false
	defer func() {
		if err != nil && !checked {
			rec.Forget(ctx)
		}
	}()
	answers := make([]json.RawMessage, len(steps))
	before := make([]bool, len(steps))
	var refused []batchItem
	for i, s := range steps {
		if a, ok := rec.Done(i); ok {
			answers[i] = a
			continue
		}
		if s.made != nil {
			held, err := s.made(ctx)
			if err != nil {
				return nil, batchStopped(steps, answers, i, storeFailed("failed to read the table"), says)
			}
			if held {
				before[i] = true
				continue
			}
		}
		if err := s.check(ctx); err != nil {
			if serversOwn(err) {
				return nil, batchStopped(steps, answers, i, err, says)
			}
			it := s.item
			it.Message = refusalWords(says(err))
			refused = append(refused, it)
		}
	}
	if len(refused) > 0 {
		e := v1api.Errorf(http.StatusBadRequest, "invalid_argument", "fix the items the details name, then send the batch again",
			"%d of the batch's items can't be applied, so none was", len(refused))
		e.Details = refused
		return nil, e
	}
	checked = true
	for i, s := range steps {
		if answers[i] != nil {
			continue
		}
		var answer any
		if before[i] {
			answer = s.madeAnswer
			if answer == nil {
				answer = apiMadeBefore{ID: s.item.ID, MadeBefore: true}
			}
		} else {
			v, err := applyItem(ctx, s, audit)
			if err != nil {
				return nil, batchStopped(steps, answers, i, err, says)
			}
			answer = v
		}
		a, err := json.Marshal(answer)
		if err != nil {
			return nil, batchStopped(steps, answers, i, err, says)
		}
		// Done, whatever the record says next.
		answers[i] = a
		if err := rec.Mark(ctx, i, a); err != nil {
			return nil, batchStopped(steps, answers, i+1, errBatchRecord, says)
		}
	}
	out := apiBatchDone{Created: []json.RawMessage{}, Updated: []json.RawMessage{}, Deleted: []json.RawMessage{}}
	for i, s := range steps {
		switch s.item.List {
		case "create":
			out.Created = append(out.Created, answers[i])
		case "update":
			out.Updated = append(out.Updated, answers[i])
		default:
			out.Deleted = append(out.Deleted, answers[i])
		}
	}
	return out, nil
}

// apiBatchDone is a batch applied whole: each item's answer by its list, in
// its order.
type apiBatchDone struct {
	Created []json.RawMessage `json:"created"`
	Updated []json.RawMessage `json:"updated"`
	Deleted []json.RawMessage `json:"deleted"`
}

// apiMadeBefore answers a create a send of the same batch before made.
type apiMadeBefore struct {
	ID         string `json:"id"`
	MadeBefore bool   `json:"made_before"`
}

// serversOwn is whether err is the server's failure, not the item's refusal.
func serversOwn(err error) bool {
	return IsStoreFailed(err) || errors.Is(err, errViewsUnavailable) || IsUnavailable(err)
}

// refusalWords are an item's refusal as the API says it.
func refusalWords(err error) string {
	var e *v1api.Error
	if errors.As(err, &e) {
		return e.Message
	}
	return err.Error()
}

// batchStopped is a batch stopped at its item at: the server's failure is a
// 503 that says to send the same batch again with the same key; a refusal is
// its own status. Either way the details name the items done and not done.
func batchStopped(steps []batchStep, answers []json.RawMessage, at int, err error, says func(error) error) error {
	done, notDone := []batchItem{}, []batchItem{}
	for i, s := range steps {
		if answers[i] != nil {
			done = append(done, s.item)
		} else {
			notDone = append(notDone, s.item)
		}
	}
	var e *v1api.Error
	switch {
	case serversOwn(err) || !errors.As(says(err), &e):
		e = v1api.Errorf(http.StatusServiceUnavailable, "unavailable", "send the same batch again with the same key: the items done are skipped",
			"the batch stopped part way: the items done are in the details")
		e.RetryAfter = retryRows
	default:
		stop := steps[min(at, len(steps)-1)].item
		e = v1api.Errorf(e.Status, e.Code, "the items before it were applied: send the rest as a new batch, with a new key",
			"%s[%d] was refused at its turn: %s", stop.List, stop.Index, e.Message)
	}
	e.Details = map[string][]batchItem{"done": done, "not_done": notDone}
	return e
}

// batchKeyErr is a batch's key refused: sent with another batch, still being
// applied, or a record that can't be kept (nothing was applied).
func batchKeyErr(err error) error {
	switch {
	case errors.Is(err, idempotency.ErrItemsReused):
		return v1api.Errorf(http.StatusUnprocessableEntity, "idempotency_key_reused", "send a new key with a new batch",
			"this key was sent with another batch in the last 24 hours")
	case errors.Is(err, idempotency.ErrItemsInProgress):
		return v1api.Errorf(http.StatusConflict, "in_progress", "send it again once the first send has answered",
			"a batch with this key is still being applied")
	}
	e := v1api.Errorf(http.StatusServiceUnavailable, "unavailable", "send it again in a few seconds",
		"the batch's record can't be kept just now, so nothing was applied")
	e.RetryAfter = retryRows
	return e
}

// auditItem writes an item's audit row as action: the asker (the token's
// member), the agent and the token, the op, the batch's key and the call's
// change id, the item, and what its write stored (done: condition e, as each
// row a batch writes is by them). A "_failed" row says the change didn't take.
func auditItem(ctx context.Context, actor Actor, route, key string, table uuid.UUID, s batchStep, done itemDone, action string) error {
	_, email, _ := auditBusiness.ActorFromContext(ctx)
	meta := map[string]any{"op": route, "key": key, "list": s.item.List, "index": s.item.Index, "table_id": table.String(),
		"id": s.item.ID, "token_id": helpers.GetApiTokenID(ctx)}
	for k, v := range done.facts {
		meta[k] = v
	}
	summary := done.summary
	if action != s.action {
		summary += " (it didn't take)"
	}
	if agent := helpers.GetApiAgentID(ctx); agent != "" {
		meta["agent_id"] = agent
	}
	if change, ok := helpers.CallChange(ctx); ok {
		meta["change_id"] = change.String()
	}
	who := actor.UserID
	if err := recordItem(ctx, &who, email, auditKind(actor), action, auditBusiness.CategoryTables, summary, meta); err != nil {
		helpers.LogErrorWithContext(ctx, "business/DataTable %s %s[%d]: audit: %v", route, s.item.List, s.item.Index, err)
		return errBatchAudit
	}
	return nil
}

// batchLists are a batch's three lists, checked: at most maxSchemaBatch
// items in all, and at least one.
func batchLists(args map[string]any) (creates, updates []map[string]any, deletes []uuid.UUID, err error) {
	creates, updates = listArg(args, "create"), listArg(args, "update")
	if deletes, err = idsArg(args, "delete"); err != nil {
		return nil, nil, nil, err
	}
	switch n := len(creates) + len(updates) + len(deletes); {
	case n == 0:
		return nil, nil, nil, badInput("name something to create, update or delete")
	case n > maxSchemaBatch:
		return nil, nil, nil, badInput("a batch takes at most %d items in all; this one has %d", maxSchemaBatch, n)
	}
	return creates, updates, deletes, nil
}

// twice refuses, in a batch's check, an id named more than once in its
// updates and deletes: the second naming's item.
func twice(seen map[uuid.UUID]bool, id uuid.UUID, what string) error {
	if seen[id] {
		return badInput("this %s is named twice in the batch: name each once", what)
	}
	seen[id] = true
	return nil
}

// decodeItem reads a batch item, or a call's arguments, into the app's own
// input type, as its controller decodes a request's body.
func decodeItem(m map[string]any, into any) error {
	b, err := json.Marshal(m)
	if err == nil {
		err = json.Unmarshal(b, into)
	}
	if err != nil {
		return badInput("an item can't be read: %v", err)
	}
	return nil
}
