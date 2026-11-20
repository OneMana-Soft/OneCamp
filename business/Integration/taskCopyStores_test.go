package Integration

import (
	"context"
	"database/sql"
	"testing"

	"github.com/akashc777/OneCamp/models/pgexec"
)

type traceKey struct{}

// requestCtx is a request's ctx as a door holds it: a value of its own (the
// trace the pass's statements and log lines carry), a transaction the request
// opened, and a cancel that comes when the request ends.
func requestCtx(t *testing.T) (context.Context, *sql.Tx, context.CancelFunc) {
	t.Helper()
	tx := &sql.Tx{}
	ctx, end := context.WithCancel(pgexec.WithTx(context.WithValue(context.Background(), traceKey{}, "trace-1119"), tx))
	t.Cleanup(end)
	return ctx, tx, end
}

// passKeptTheRequest fails t unless every store call the pass made kept the
// request's values, ran outside its transaction (by the pass's turn the
// request has committed or rolled it back) and outlived the request.
func (w *reassignWorld) passKeptTheRequest(t *testing.T, tx *sql.Tx) {
	t.Helper()
	w.mu.Lock()
	ctxs := append([]context.Context(nil), w.ctxs...)
	w.mu.Unlock()
	if len(ctxs) == 0 {
		t.Fatal("the pass made no store call")
	}
	for i, ctx := range ctxs {
		if ctx.Value(traceKey{}) != "trace-1119" {
			t.Errorf("store call %d lost the request's values", i+1)
		}
		if q, _ := pgexec.Current(ctx); q == pgexec.Querier(tx) {
			t.Errorf("store call %d runs in the request's transaction", i+1)
		}
		if ctx.Err() != nil {
			t.Errorf("store call %d ended with the request: %v", i+1, ctx.Err())
		}
	}
}

// A pass runs after the request has ended, so it keeps the request's values
// but never its cancel or its transaction (V-1119 F1): each door hands it
// pgexec.Detached(ctx), as googleCopyDoors_test.go (business/Task) holds them.
func TestACopyPassKeepsTheRequestsValuesButNotItsTransaction(t *testing.T) {
	t.Run("hand-over", func(t *testing.T) {
		w := newReassignWorld(t)
		a := w.person(t, "a", "a-refresh", true, "evA")
		b := w.person(t, "b", "b-refresh", true)
		w.handedOver(a, "evA")
		w.assign(b)

		ctx, tx, end := requestCtx(t)
		done := MoveTaskGoogleCopy(pgexec.Detached(ctx), w.task.String(), a.String(), b.String())
		end()
		if err := <-done; err != nil {
			t.Fatal(err)
		}
		w.passKeptTheRequest(t, tx)
	})
	t.Run("archive", func(t *testing.T) {
		w := newReassignWorld(t)
		a := w.person(t, "a", "a-refresh", false, "evA")
		w.handedOver(a, "evA")
		w.archived(t)

		ctx, tx, end := requestCtx(t)
		done := ArchiveTaskGoogleCopy(pgexec.Detached(ctx), w.task.String())
		end()
		archiveEnds(t, done)
		w.passKeptTheRequest(t, tx)
	})
}

// An archive's pass makes the store calls its case needs and no more: one row
// read always; the graph and the link only for an archived row that names a
// copy; one DELETE and one clear only for a copy it can reach (V-1119 F2).
func TestAnArchivePassMakesOnlyTheStoreCallsItsCaseNeeds(t *testing.T) {
	cases := []struct {
		name                    string
		id, live                bool
		token                   string
		row, graph, link, clear int
		deletes                 int
	}{
		{name: "no id", token: "a-refresh", row: 1},
		{name: "a live row", id: true, live: true, token: "a-refresh", row: 1},
		{name: "an archived row with an id and a link", id: true, token: "a-refresh", row: 1, graph: 1, link: 1, clear: 1, deletes: 1},
		{name: "no usable link", id: true, row: 1, graph: 1, link: 1},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			w := newReassignWorld(t)
			a := w.person(t, "a", c.token, false, "evA")
			w.assign(a)
			if c.id {
				w.handedOver(a, "evA")
			}
			w.archived(t).Store(!c.live)

			archiveEnds(t, w.archive())
			got := w.storeCalls()
			if got["row"] != c.row || got["graph"] != c.graph || got["link"] != c.link || got["clear"] != c.clear || got["set"] != 0 {
				t.Errorf("the pass made %d row / %d graph / %d link reads, %d clears and %d sets, want %d / %d / %d, %d and 0",
					got["row"], got["graph"], got["link"], got["clear"], got["set"], c.row, c.graph, c.link, c.clear)
			}
			if calls, _ := w.cals[a].snapshot(); len(calls) != c.deletes || (c.deletes == 1 && calls[0] != "DELETE evA") {
				t.Errorf("A's calendar was asked %v, want %d DELETE evA and nothing else", calls, c.deletes)
			}
		})
	}
}
