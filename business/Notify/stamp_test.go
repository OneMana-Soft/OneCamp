package business

import (
	"context"
	"testing"
	"time"

	"github.com/akashc777/OneCamp/helpers"
)

// A delivery is stamped when what it tells of happened (V-871): a message
// written at an earlier time (an import, the demo's seed:
// helpers.WithImportTimestamp) has its deliveries sent and written at that
// time, so Why's "Where it went" never reads later than the message; one
// dated after now is stamped now, never ahead of the clock; and a live send
// (no such time) is stamped now and written at the column's own NOW().
func TestADeliveryIsStampedWhenItsMessageWasWritten(t *testing.T) {
	rows, _ := withFakes(t)
	r := routerOn(&fakeInputs{})
	earlier := at.Add(-12 * time.Hour)
	for _, c := range []struct {
		name    string
		ctx     context.Context
		sent    time.Time
		written time.Time // zero: left to the column's NOW()
	}{
		{"a message written 12 h ago", helpers.WithImportTimestamp(context.Background(), earlier), earlier, earlier},
		{"a message dated an hour ahead", helpers.WithImportTimestamp(context.Background(), at.Add(time.Hour)), at, at},
		{"a live send", context.Background(), at, time.Time{}},
	} {
		ev := posted
		ev.ID = posted.ID + ":" + c.name
		r.Deliver(c.ctx, ev, twoTold)
		n := 0
		for _, row := range rows.rows {
			if row.EventID != ev.ID {
				continue
			}
			n++
			if row.SentAt == nil || !row.SentAt.Equal(c.sent) {
				t.Errorf("%s: %s %s sent at %v, want %v", c.name, row.UserID, row.Delivery, row.SentAt, c.sent)
			}
			if !row.CreatedAt.Equal(c.written) {
				t.Errorf("%s: %s %s written at %v, want %v", c.name, row.UserID, row.Delivery, row.CreatedAt, c.written)
			}
		}
		if want := len(named) + 3; n != want {
			t.Errorf("%s: %d rows, want %d", c.name, n, want)
		}
	}
}
