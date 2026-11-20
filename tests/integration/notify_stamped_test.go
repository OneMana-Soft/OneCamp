//go:build integration
// +build integration

package integration_test

// V-871: a delivery is stamped when what it tells of happened. A message
// written at an earlier time (the demo's seed, an import:
// helpers.WithImportTimestamp) has its notification_deliveries rows written
// and sent at that time, so Why's "Where it went" never reads ~12 h after the
// message it tells of; one dated ahead of the clock is stamped now; and a live
// send is stamped now, as before. Each through the product's own doors: a
// channel mention, a DM and a thread reply.

import (
	"context"
	"testing"
	"time"

	postAdapter "github.com/akashc777/OneCamp/adapter/Post"
	"github.com/akashc777/OneCamp/helpers"
)

// stamps is each delivery row of the message as (delivery, state, written,
// sent), once want of them are written.
func (w *reachWorld) stamps(t *testing.T, messageID string, want int) []stampedRow {
	t.Helper()
	w.rowsFor(t, messageID, want)
	rows, err := w.env.PG.Query(`SELECT lower(user_id::text), delivery, state, created_at, sent_at FROM notification_deliveries WHERE message_id = $1`, messageID)
	if err != nil {
		t.Fatalf("read the rows: %v", err)
	}
	defer rows.Close()
	var out []stampedRow
	for rows.Next() {
		var r stampedRow
		if err := rows.Scan(&r.user, &r.delivery, &r.state, &r.written, &r.sent); err != nil {
			t.Fatal(err)
		}
		out = append(out, r)
	}
	if len(out) < want {
		t.Fatalf("%d rows of %s, want at least %d", len(out), messageID, want)
	}
	return out
}

type stampedRow struct {
	user, delivery, state string
	written               time.Time
	sent                  *time.Time
}

// stampedAt checks every row was written at, and every sent one sent at, a
// time in [from, to].
func stampedAt(t *testing.T, what string, rows []stampedRow, from, to time.Time) {
	t.Helper()
	in := func(at time.Time) bool { return !at.Before(from) && !at.After(to) }
	for _, r := range rows {
		if !in(r.written) {
			t.Errorf("%s: %s's %s written at %v, want in [%v, %v]", what, r.user, r.delivery, r.written, from, to)
		}
		if r.state == "sent" && (r.sent == nil || !in(*r.sent)) {
			t.Errorf("%s: %s's %s sent at %v, want in [%v, %v]", what, r.user, r.delivery, r.sent, from, to)
		}
	}
}

func TestADeliveryIsStampedWhenItsMessageWasWritten(t *testing.T) {
	w := newReachWorld(t)
	dated := func(ctx context.Context, at time.Time) context.Context { return helpers.WithImportTimestamp(ctx, at) }
	written := time.Now().Add(-12 * time.Hour).Truncate(time.Second)

	// The demo's seeded mention: Maya names Jonas in a message written 12 h ago.
	post := w.postAs(t, dated(w.as(t, "maya", false), written), postAdapter.InputCreateOrUpdatePostInfo{HTMLText: "<p>" + w.mention("jonas") + " the deck?</p>"})
	stampedAt(t, "a mention written 12 h ago", w.stamps(t, post, len(namedRows)), written, written)

	// A DM, and a reply in Jonas's thread, written 11 and 10 h ago.
	dm := written.Add(time.Hour)
	stampedAt(t, "a DM written 11 h ago", w.stamps(t, w.dmAs(t, dated(w.as(t, "maya", false), dm), "jonas", "<p>lunch?</p>"), len(namedRows)), dm, dm)
	asked := w.postAs(t, w.as(t, "jonas", false), postAdapter.InputCreateOrUpdatePostInfo{HTMLText: "<p>the plan</p>"})
	replied := written.Add(2 * time.Hour)
	reply := w.replyAs(t, dated(w.as(t, "maya", false), replied), asked, "<p>agreed</p>")
	stampedAt(t, "a reply written 10 h ago", w.stamps(t, reply, len(namedRows)), replied, replied)

	// The other side: a live send is stamped now, and a message dated an hour
	// ahead of the clock is stamped now, never ahead of it.
	before := time.Now().Add(-time.Second)
	live := w.postAs(t, w.as(t, "maya", false), postAdapter.InputCreateOrUpdatePostInfo{HTMLText: "<p>" + w.mention("jonas") + " now?</p>"})
	ahead := w.postAs(t, dated(w.as(t, "maya", false), time.Now().Add(time.Hour)), postAdapter.InputCreateOrUpdatePostInfo{HTMLText: "<p>" + w.mention("jonas") + " later?</p>"})
	liveRows, aheadRows := w.stamps(t, live, len(namedRows)), w.stamps(t, ahead, len(namedRows))
	after := time.Now().Add(time.Second)
	stampedAt(t, "a live mention", liveRows, before, after)
	stampedAt(t, "a mention dated an hour ahead", aheadRows, before, after)
}
