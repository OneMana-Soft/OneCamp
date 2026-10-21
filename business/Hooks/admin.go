package business

// What Admin › Webhooks does with deliveries: reads them, replays one, pauses
// and resumes a webhook, and reads how each webhook's deliveries stand; and
// the housekeeping (the purge, the system check). Each change is an admin's,
// through a session, and the controller writes it into the audit log.

import (
	"context"
	"errors"
	"fmt"
	"strconv"
	"time"

	"github.com/akashc777/OneCamp/helpers"
	models "github.com/akashc777/OneCamp/models/postgres/WebhookDelivery"
	"github.com/google/uuid"
)

// The admin's seams, for tests.
var (
	listRows   = models.List
	replayRow  = models.Replay
	replayDead = models.ReplayDead
	dueNow     = models.DueNow
	takeOver   = models.TakeOver
	healthOf   = models.HealthOf
	readStats  = models.ReadStats
	expireRows = models.Expire
	purgeRows  = models.Purge
)

// Page is one page of a webhook's deliveries, newest first; Next, when there
// are more, is the cursor for the page after.
type Page struct {
	Items []models.Row `json:"items"`
	Next  string       `json:"next_cursor,omitempty"`
}

// Deliveries is a page of a webhook's deliveries, in state when it isn't
// empty, after cursor ("" is from the newest), limit at a time (1-100).
func Deliveries(ctx context.Context, webhookID uuid.UUID, state, cursor string, limit int) (Page, error) {
	switch state {
	case "", models.StatePending, models.StateDone, models.StateDead, models.StateCancelled:
	default:
		return Page{}, fmt.Errorf("a delivery is pending, done, dead or cancelled, never %q", state)
	}
	if limit < 1 || limit > 100 {
		limit = 25
	}
	var before int64
	if cursor != "" {
		n, err := strconv.ParseInt(cursor, 10, 64)
		if err != nil || n < 1 {
			return Page{}, errors.New("that page cursor isn't one this list gave")
		}
		before = n
	}
	items, err := listRows(ctx, webhookID, state, before, limit+1)
	if err != nil {
		return Page{}, err
	}
	p := Page{Items: items}
	if len(items) > limit {
		p.Items = items[:limit]
		p.Next = strconv.FormatInt(items[limit-1].ID, 10)
	}
	return p, nil
}

// Replay sends a webhook's delivery again from its first try, with the same
// body and event id, and starts it now.
func Replay(ctx context.Context, webhookID uuid.UUID, deliveryID int64) error {
	ok, err := replayRow(ctx, webhookID, deliveryID)
	if err != nil {
		return err
	}
	if !ok {
		return ErrNotFound
	}
	startSending([]int64{deliveryID})
	return nil
}

// replayAtOnce bounds one "send all failed again": the rest wait for the next.
const replayAtOnce = 1000

// ReplayFailed sends every dead delivery of a webhook again from its first
// try, each with its own body and event id, oldest first and at most
// replayAtOnce in one go, and answers how many go. A paused webhook's wait
// for the resume, as a single replay's do.
func ReplayFailed(ctx context.Context, webhookID uuid.UUID) (int, error) {
	ids, err := replayDead(ctx, webhookID, replayAtOnce)
	if err != nil {
		return 0, err
	}
	if len(ids) > 0 {
		startSending(ids)
	}
	return len(ids), nil
}

// Pause holds a webhook's deliveries until it's resumed.
func Pause(ctx context.Context, webhookID uuid.UUID) error {
	at := now()
	changed, err := rows.setPaused(ctx, webhookID, &at, PausedByAdmin)
	if err != nil {
		return err
	}
	if !changed {
		if _, ok, err := rows.target(ctx, webhookID); err == nil && !ok {
			return ErrNotFound
		}
	}
	return nil
}

// Resume lets a paused webhook's deliveries go, at once. When its creator is
// no longer a member, by (the admin resuming it) takes it over: what it sends
// goes out on a member's say-so, never on nobody's.
func Resume(ctx context.Context, webhookID, by uuid.UUID) (tookOver bool, err error) {
	t, ok, err := rows.target(ctx, webhookID)
	if err != nil {
		return false, err
	}
	if !ok || t.Deleted {
		return false, ErrNotFound
	}
	if member, err := isMember(ctx, t.CreatedBy); err == nil && !member && by != uuid.Nil {
		if err := takeOver(ctx, webhookID, by); err != nil {
			return false, err
		}
		tookOver = true
	}
	if _, err := rows.setPaused(ctx, webhookID, nil, ""); err != nil {
		return tookOver, err
	}
	return tookOver, dueNow(ctx, webhookID)
}

// Health is how every outgoing webhook's deliveries stand.
func Health(ctx context.Context) ([]models.Health, error) { return healthOf(ctx) }

// purge gives up on deliveries a paused webhook held a month, and deletes the
// finished rows past their keep: done ones after a week, dead ones after a
// month (so an admin can see and replay them meanwhile).
func purge(ctx context.Context) error {
	if n, err := expireRows(ctx, waitLimit, "Waited 30 days while the webhook was paused."); err != nil {
		return fmt.Errorf("hooks: couldn't give up on the deliveries a paused webhook held: %w", err)
	} else if n > 0 {
		helpers.LogInfoWithContext(ctx, "hooks: gave up on %d deliveries a paused webhook held for 30 days", n)
	}
	for pass := 0; pass < 20; pass++ {
		n, err := purgeRows(ctx, now().Add(-doneKeep), now().Add(-deadKeep), 1000)
		if err != nil {
			return fmt.Errorf("hooks: couldn't delete old deliveries: %w", err)
		}
		if n < 1000 {
			return nil
		}
	}
	return nil
}

// stuckAfter is how long a delivery that may go can wait before the system
// check says the drain is stuck.
const stuckAfter = 5 * time.Minute

func init() {
	helpers.RegisterSystemCheck(helpers.SystemCheck{
		Name: "webhook deliveries",
		Kind: helpers.CheckKindBehaviour,
		Describe: "Outgoing webhooks' deliveries are being sent: none that may go has waited more than five minutes. " +
			"It doesn't prove the endpoints accept them; a webhook's own page says how its deliveries went.",
		Probe: func(ctx context.Context) error {
			s, err := readStats(ctx)
			if err != nil {
				return fmt.Errorf("couldn't read the webhook deliveries: %w", err)
			}
			if s.OldestDue > stuckAfter {
				return fmt.Errorf("a webhook delivery has waited %s to be sent: the workers that send them may be down (%d waiting)",
					s.OldestDue.Round(time.Second), s.Pending)
			}
			if s.DeadToday > 0 {
				return helpers.SystemCheckNote(fmt.Sprintf("%d webhook deliveries failed every try in the last day; Admin, Webhooks shows them and can send them again", s.DeadToday))
			}
			return nil
		},
	})
}
