//go:build integration

package business

// Save for later against a real Postgres 12 with every migration applied.
// Run: go test -tags=integration ./business/SavedItem/ -v

import (
	"context"
	"encoding/json"
	"errors"
	"testing"
	"time"

	"github.com/akashc777/OneCamp/initializers/postgresInit"
	model "github.com/akashc777/OneCamp/models/postgres/SavedItem"
	jobModel "github.com/akashc777/OneCamp/models/postgres/ScheduledJob"
	"github.com/akashc777/OneCamp/tests/integration"
	"github.com/google/uuid"
)

func setup(t *testing.T) context.Context {
	t.Helper()
	env := integration.SetupEnv(t)
	if err := postgresInit.ConnectPostgres(context.Background(), env.DSN); err != nil {
		t.Fatalf("wire the pool: %v", err)
	}
	return context.Background()
}

func save(t *testing.T, ctx context.Context, user uuid.UUID, id string, remind *time.Time) *model.SavedItem {
	t.Helper()
	item, err := Save(ctx, user, SaveInput{ItemType: "task", ItemID: id, Link: "/app/task/" + id, Title: "Task " + id, RemindAt: remind})
	if err != nil {
		t.Fatalf("save %s: %v", id, err)
	}
	return item
}

func job(user uuid.UUID, item *model.SavedItem) *jobModel.ScheduledJob {
	p, _ := json.Marshal(reminderPayload{ID: item.ID.String(), RemindAt: *item.RemindAt})
	return &jobModel.ScheduledJob{UserUuid: user, Payload: string(p)}
}

func TestSavedItems(t *testing.T) {
	ctx := setup(t)
	me, other := uuid.New(), uuid.New()
	soon := time.Now().Add(time.Minute)
	past := time.Now().Add(-30 * time.Second)

	a := save(t, ctx, me, "a", nil)
	b := save(t, ctx, me, "b", &soon)
	c := save(t, ctx, me, "c", &past) // due now

	t.Run("due items bubble to the top", func(t *testing.T) {
		res, err := List(ctx, me, false)
		if err != nil {
			t.Fatal(err)
		}
		if res.Open != 3 || res.Due != 1 {
			t.Fatalf("open=%d due=%d, want 3 and 1", res.Open, res.Due)
		}
		if res.Items[0].ID != c.ID {
			t.Fatalf("first item is %s, want the due one", res.Items[0].Title)
		}
		// Then newest first.
		if res.Items[1].ID != b.ID || res.Items[2].ID != a.ID {
			t.Fatalf("order after the due item: %s, %s", res.Items[1].Title, res.Items[2].Title)
		}
	})

	t.Run("saving the same thing again refreshes it and does not duplicate", func(t *testing.T) {
		again := save(t, ctx, me, "a", &soon)
		if again.ID != a.ID || again.RemindAt == nil {
			t.Fatalf("re-save: id %s (was %s), remind %v", again.ID, a.ID, again.RemindAt)
		}
		res, _ := List(ctx, me, false)
		if res.Open != 3 {
			t.Fatalf("open=%d after re-saving, want 3", res.Open)
		}
	})

	t.Run("a member never sees or touches another member's items", func(t *testing.T) {
		res, _ := List(ctx, other, false)
		if len(res.Items) != 0 {
			t.Fatalf("other member sees %d items", len(res.Items))
		}
		if err := Delete(ctx, other, a.ID); !errors.Is(err, ErrNotFound) {
			t.Fatalf("other member deleting: %v, want not found", err)
		}
		if _, err := SetDone(ctx, other, a.ID, true); !errors.Is(err, ErrNotFound) {
			t.Fatalf("other member finishing: %v, want not found", err)
		}
		if _, err := SetReminder(ctx, other, a.ID, nil); !errors.Is(err, ErrNotFound) {
			t.Fatalf("other member changing the reminder: %v, want not found", err)
		}
	})

	t.Run("the reminder fires once", func(t *testing.T) {
		if err := runReminder(ctx, job(me, c)); err != nil {
			t.Fatal(err)
		}
		got, _ := model.Get(ctx, me, c.ID)
		if got.RemindedAt == nil {
			t.Fatal("the due reminder was not marked sent")
		}
		sent, err := model.MarkReminded(ctx, c.ID, *c.RemindAt)
		if err != nil || sent {
			t.Fatalf("a second delivery was allowed: sent=%v err=%v", sent, err)
		}
	})

	t.Run("a changed reminder makes the old job do nothing", func(t *testing.T) {
		old := job(me, b)
		later := time.Now().Add(2 * time.Hour)
		if _, err := SetReminder(ctx, me, b.ID, &later); err != nil {
			t.Fatal(err)
		}
		if err := runReminder(ctx, old); err != nil {
			t.Fatal(err)
		}
		got, _ := model.Get(ctx, me, b.ID)
		if got.RemindedAt != nil {
			t.Fatal("the stale job delivered a reminder that had been moved")
		}
	})

	t.Run("a job for someone else's item does nothing", func(t *testing.T) {
		p := time.Now().Add(-time.Second)
		d := save(t, ctx, me, "d", &p)
		if err := runReminder(ctx, job(other, d)); err != nil {
			t.Fatal(err)
		}
		got, _ := model.Get(ctx, me, d.ID)
		if got.RemindedAt != nil {
			t.Fatal("a job owned by another member delivered this member's reminder")
		}
	})

	t.Run("done clears the reminder and moves it to Done; reopening brings it back", func(t *testing.T) {
		done, err := SetDone(ctx, me, b.ID, true)
		if err != nil || done.DoneAt == nil || done.RemindAt != nil {
			t.Fatalf("done: %+v err=%v", done, err)
		}
		res, _ := List(ctx, me, true)
		if len(res.Items) != 1 || res.Items[0].ID != b.ID {
			t.Fatalf("Done list has %d items", len(res.Items))
		}
		if _, err := SetDone(ctx, me, b.ID, false); err != nil {
			t.Fatal(err)
		}
		res, _ = List(ctx, me, true)
		if len(res.Items) != 0 {
			t.Fatal("a reopened item is still under Done")
		}
	})

	t.Run("a reminder too far away is refused", func(t *testing.T) {
		far := time.Now().Add(400 * 24 * time.Hour)
		if _, err := SetReminder(ctx, me, a.ID, &far); !errors.Is(err, ErrInvalid) {
			t.Fatalf("got %v, want invalid", err)
		}
	})

	t.Run("delete removes it", func(t *testing.T) {
		if err := Delete(ctx, me, a.ID); err != nil {
			t.Fatal(err)
		}
		if _, err := model.Get(ctx, me, a.ID); !errors.Is(err, ErrNotFound) {
			t.Fatalf("still there: %v", err)
		}
	})
}
