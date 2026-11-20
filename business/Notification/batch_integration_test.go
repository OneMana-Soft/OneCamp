//go:build integration

package notification

import (
	"encoding/json"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/akashc777/OneCamp/helpers"
	emailQueueModels "github.com/akashc777/OneCamp/models/postgres/NotificationEmailQueue"
)

// TestNoticesInOneWindowAreOneEmail: what someone is told inside one window
// goes out as one email listing each, asked again at the send like any one
// notice; what isn't batched goes out on its own, when due.
func TestNoticesInOneWindowAreOneEmail(t *testing.T) {
	h := NewRecheck(t)
	// dm starts a DM from from to to, to with the default setting and never
	// having opened it, and answers say: from writes text there, which is
	// queued for to.
	dm := func(t *testing.T, from, to Person) (say func(text string) uuid.UUID) {
		t.Helper()
		grp := helpers.GetGroupingId(from.ID.String(), to.ID.String())
		for _, p := range []Person{from, to} {
			h.Exec(t, `INSERT INTO users_chat_notification (user_id, grp_id, notification_type) VALUES ($1, $2, 'all')`, p.ID, grp)
		}
		return func(text string) uuid.UUID {
			msg := uuid.New()
			h.Exec(t, `INSERT INTO chats (id, created_by, grp_id) VALUES ($1, $2, $3)`, msg, from.ID, grp)
			DispatchChatDM(from.ID.String(), "Ada", "", grp, msg.String(), text, []string{to.ID.String()}, false)
			helpers.SettleBackground(10 * time.Second)
			return msg
		}
	}
	// rows counts p's queue rows by status.
	rows := func(t *testing.T, p Person) map[string]int {
		t.Helper()
		r, err := h.Env.PG.Query(`SELECT status, COUNT(*) FROM notification_email_queue WHERE user_id = $1 GROUP BY status`, p.ID)
		if err != nil {
			t.Fatal(err)
		}
		defer r.Close()
		out := map[string]int{}
		for r.Next() {
			var s string
			var n int
			if err := r.Scan(&s, &n); err != nil {
				t.Fatal(err)
			}
			out[s] = n
		}
		return out
	}

	t.Run("a burst of messages is one email listing each", func(t *testing.T) {
		ada, bo := h.Person(t, "Ada"), h.Person(t, "Bo")
		say := dm(t, ada, bo)
		for _, text := range []string{"the launch plan is ready", "budget is in the sheet", "call at four?"} {
			say(text)
		}
		got := h.Send(t)
		if len(got[bo.Email]) != 1 {
			t.Fatalf("Bo was sent %d emails, want one for the burst", len(got[bo.Email]))
		}
		for _, text := range []string{"the launch plan is ready", "budget is in the sheet", "call at four?"} {
			if !strings.Contains(got[bo.Email][0].Text, text) {
				t.Errorf("Bo's email doesn't list %q", text)
			}
		}
		if r := rows(t, bo); r["sent"] != 3 || len(r) != 1 {
			t.Errorf("Bo's rows are %v, want all three sent", r)
		}
	})

	t.Run("a message deleted before the send drops out of the list", func(t *testing.T) {
		ada, bo := h.Person(t, "Ada"), h.Person(t, "Bo")
		say := dm(t, ada, bo)
		say("the launch plan is ready")
		gone := say("the old salary sheet")
		say("call at four?")
		h.Exec(t, `UPDATE chats SET deleted_at = NOW() WHERE id = $1`, gone)
		got := h.Send(t)
		if len(got[bo.Email]) != 1 {
			t.Fatalf("Bo was sent %d emails, want one", len(got[bo.Email]))
		}
		if txt := got[bo.Email][0].Text; strings.Contains(txt, "the old salary sheet") || !strings.Contains(txt, "call at four?") {
			t.Errorf("Bo's email says %q: want the two that hold, not the deleted one", txt)
		}
		if r := rows(t, bo); r["sent"] != 2 || r["suppressed"] != 1 {
			t.Errorf("Bo's rows are %v, want two sent and one suppressed", r)
		}
	})

	t.Run("a window where nothing holds sends nothing", func(t *testing.T) {
		ada, bo := h.Person(t, "Ada"), h.Person(t, "Bo")
		say := dm(t, ada, bo)
		for _, text := range []string{"draft one", "draft two"} {
			h.Exec(t, `UPDATE chats SET deleted_at = NOW() WHERE id = $1`, say(text))
		}
		if got := h.Send(t); len(got[bo.Email]) != 0 {
			t.Errorf("Bo was sent %v, want nothing", got[bo.Email])
		}
		if r := rows(t, bo); r["suppressed"] != 2 || len(r) != 1 {
			t.Errorf("Bo's rows are %v, want both suppressed", r)
		}
	})

	t.Run("a notice waits the window; one not batched goes when due, on its own", func(t *testing.T) {
		ada, bo := h.Person(t, "Ada"), h.Person(t, "Bo")
		dm(t, ada, bo)("the launch plan is ready")
		h.Exec(t, `INSERT INTO notification_email_queue (user_id, to_email, event_type, subject, html_body, text_body, dedup_key, metadata)
			VALUES ($1, $2, 'calendar.reminder', 'Standup in 10 minutes', '<p>Standup</p>', 'Standup in 10 minutes', $3, '{}')`, bo.ID, bo.Email, "reminder|"+bo.ID.String())
		got := h.SendAsDue(t)
		if len(got[bo.Email]) != 1 || got[bo.Email][0].Subject != "Standup in 10 minutes" {
			t.Fatalf("Bo was sent %v, want the reminder alone", got[bo.Email])
		}
		if r := rows(t, bo); r["pending"] != 1 || r["sent"] != 1 {
			t.Errorf("Bo's rows are %v, want the reminder sent and the DM still waiting", r)
		}
	})

	t.Run("the claim takes the person's notices due within the window, not one locked elsewhere", func(t *testing.T) {
		ada, bo, cy, dy, ed := h.Person(t, "Ada"), h.Person(t, "Bo"), h.Person(t, "Cy"), h.Person(t, "Dy"), h.Person(t, "Ed")
		first := dm(t, ada, bo)("first")  // due now
		mate := dm(t, cy, bo)("mate")     // due inside the window
		quiet := dm(t, dy, bo)("quiet")   // past Bo's quiet hours
		locked := dm(t, ed, bo)("locked") // inside the window, held by another worker
		item := `metadata->'about'->>'item'`
		h.Exec(t, `UPDATE notification_email_queue SET next_retry_at = NOW() WHERE `+item+` = $1`, first.String())
		h.Exec(t, `UPDATE notification_email_queue SET next_retry_at = NOW() + interval '3 hours' WHERE `+item+` = $1`, quiet.String())
		tx, err := h.Env.PG.Begin()
		if err != nil {
			t.Fatal(err)
		}
		defer func() { _ = tx.Rollback() }()
		if _, err := tx.Exec(`SELECT id FROM notification_email_queue WHERE `+item+` = $1 FOR UPDATE`, locked.String()); err != nil {
			t.Fatal(err)
		}
		items, err := emailQueueModels.FetchAndLockPending(batchSize, claimMates)
		if err != nil {
			t.Fatal(err)
		}
		var claimed []string
		for _, it := range items {
			var m struct {
				About About `json:"about"`
			}
			_ = json.Unmarshal(it.Metadata, &m)
			claimed = append(claimed, m.About.Item)
		}
		if !slices.Contains(claimed, first.String()) || !slices.Contains(claimed, mate.String()) ||
			slices.Contains(claimed, quiet.String()) || slices.Contains(claimed, locked.String()) {
			t.Errorf("claimed %v: want Bo's first and its window-mate, not the quiet one nor the one locked elsewhere", claimed)
		}
	})
}
