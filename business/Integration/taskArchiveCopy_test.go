package Integration

import (
	"context"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	models "github.com/akashc777/OneCamp/models/postgres/Task"
	"github.com/google/uuid"
)

// archived makes the task's row archived while the flag it returns is set (it
// starts set), as the archive door leaves it; clearing it is an unarchive.
func (w *reassignWorld) archived(t *testing.T) *atomic.Bool {
	t.Helper()
	on := &atomic.Bool{}
	on.Store(true)
	row := taskRowOf
	taskRowOf = func(ctx context.Context, id uuid.UUID) (*models.Task, error) {
		r, err := row(ctx, id)
		if r != nil && on.Load() {
			r.DeletedAt = time.Date(2026, 11, 16, 23, 0, 0, 0, time.UTC)
		}
		return r, err
	}
	return on
}

// archive is the archive door's one call on the task's Google copy.
func (w *reassignWorld) archive() <-chan error {
	return ArchiveTaskGoogleCopy(context.Background(), w.task.String())
}

// archiveEnds waits for the archive's pass, whatever it answered.
func archiveEnds(t *testing.T, done <-chan error) {
	t.Helper()
	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("the archive's pass never ended")
	}
}

// V-1119 (a), (b): archiving a task takes its copy, title and all, out of the
// assignee's calendar through the assignee's own link, whether or not their
// task sync is on: our copy, not their calendar. Nobody else's link is used.
func TestArchiveRemovesTheAssigneesCopy(t *testing.T) {
	for _, taskSync := range []bool{false, true} {
		t.Run(map[bool]string{false: "task sync off", true: "task sync on"}[taskSync], func(t *testing.T) {
			w := newReassignWorld(t)
			a := w.person(t, "a", "a-refresh", taskSync, "evA")
			by := w.person(t, "x", "x-refresh", true, "evA")
			w.handedOver(a, "evA")
			w.archived(t)

			archiveEnds(t, w.archive())
			if calls, events := w.cals[a].snapshot(); len(calls) != 1 || calls[0] != "DELETE evA" || len(events) != 0 {
				t.Fatalf("A's calendar was asked %v and holds %v, want one DELETE evA and no copy left", calls, events)
			}
			if calls, _ := w.cals[by].snapshot(); len(calls) != 0 {
				t.Fatalf("a bystander's calendar was asked %v, want nothing", calls)
			}
			if got := w.storedID(); got != "" {
				t.Fatalf("the archived task keeps %q, want the copy's id forgotten", got)
			}
		})
	}
}

// V-1119 (c): an assignee whose link has no token can't be reached: no Google
// call, the id stays (a later pass may reach them), and one log line says so.
func TestArchiveWithoutAUsableLinkKeepsTheId(t *testing.T) {
	w := newReassignWorld(t)
	logs := captureLogs(t)
	a := w.person(t, "a", "", true, "evA")
	w.handedOver(a, "evA")
	w.archived(t)

	archiveEnds(t, w.archive())
	if calls, _ := w.cals[a].snapshot(); len(calls) != 0 {
		t.Fatalf("A's calendar was asked %v, want nothing", calls)
	}
	if got := w.storedID(); got != "evA" {
		t.Fatalf("the task keeps %q, want evA", got)
	}
	if lines := logs.with(w.task.String()); len(lines) != 1 || !strings.Contains(lines[0], "evA") {
		t.Fatalf("the log says %q, want one line naming the copy evA left behind", lines)
	}
}

// V-1119 (d), (e): one DELETE, no retry. A copy Google says is gone (404, 410)
// is forgotten; any other failure keeps the id, so the next pass on the task
// (an unarchive, another archive) tries again: the assignee stays the same.
func TestArchiveEndsAtGooglesAnswer(t *testing.T) {
	for _, tc := range []struct {
		name   string
		status int // 0 the event isn't there (404); -1 the connection drops
		keep   bool
	}{
		{"404", 0, false}, {"410", 410, false},
		{"500", 500, true}, {"503", 503, true}, {"429", 429, true},
		{"401", 401, true}, {"403", 403, true}, {"network", -1, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			w := newReassignWorld(t)
			events := []string{"evA"}
			if tc.name == "404" {
				events = nil
			}
			a := w.person(t, "a", "a-refresh", false, events...)
			w.cals[a].deleteStatus = tc.status
			w.handedOver(a, "evA")
			w.archived(t)

			archiveEnds(t, w.archive())
			if calls, _ := w.cals[a].snapshot(); len(calls) != 1 || calls[0] != "DELETE evA" {
				t.Fatalf("A's calendar was asked %v, want one DELETE evA and no retry", calls)
			}
			want := map[bool]string{true: "evA", false: ""}[tc.keep]
			if got := w.storedID(); got != want {
				t.Fatalf("the task keeps %q, want %q", got, want)
			}
		})
	}
}

// V-1119: unarchiving re-syncs the assignee and never makes a copy for someone
// whose task sync is off, whether the archive left an id behind or not.
func TestUnarchiveMakesNoCopyForTaskSyncOff(t *testing.T) {
	for _, left := range []string{"", "evA"} {
		t.Run("id "+left, func(t *testing.T) {
			w := newReassignWorld(t)
			a := w.person(t, "a", "a-refresh", false, "evA")
			w.handedOver(a, left)
			if left == "" {
				w.googleID = nil
			}
			if err := SyncTaskToGoogleCalendar(context.Background(), w.task.String(), a.String()); err != nil {
				t.Fatal(err)
			}
			if calls, _ := w.cals[a].snapshot(); len(calls) != 0 {
				t.Fatalf("A's calendar was asked %v, want nothing", calls)
			}
			if got := w.storedID(); got != left {
				t.Fatalf("the task keeps %q, want %q", got, left)
			}
		})
	}
}

// V-1119: an archive and then an unarchive run in the order asked. The
// unarchive's re-sync waits for the archive's DELETE, then gives the assignee
// one copy whose id the row keeps.
func TestArchiveThenUnarchiveRunInTheOrderAsked(t *testing.T) {
	w := newReassignWorld(t)
	a := w.person(t, "a", "a-refresh", true, "evA")
	w.handedOver(a, "evA")
	archived := w.archived(t)
	entered, release := w.holdDeletes(t, a)

	archive := w.archive()
	passEntered(t, entered)
	archived.Store(false)
	unarchive := make(chan error, 1)
	go func() { unarchive <- SyncTaskToGoogleCalendar(context.Background(), w.task.String(), a.String()) }()
	err, beside := ranBeside(unarchive)
	if beside {
		t.Errorf("the unarchive synced (err %v) while the archive's DELETE was in flight, want it to wait its turn", err)
	}
	close(release)
	archiveEnds(t, archive)
	if !beside {
		await(t, "the unarchive", unarchive)
	}
	w.holdsOneTrackedCopy(t, "A", a, "after the archive and the unarchive")
}

// V-1119: a pass that finds the task live again (unarchived before its turn)
// leaves the copy where it is.
func TestArchivePassLeavesALiveTasksCopy(t *testing.T) {
	w := newReassignWorld(t)
	a := w.person(t, "a", "a-refresh", false, "evA")
	w.handedOver(a, "evA")
	w.archived(t).Store(false)

	archiveEnds(t, w.archive())
	if calls, _ := w.cals[a].snapshot(); len(calls) != 0 {
		t.Fatalf("A's calendar was asked %v, want nothing for a live task", calls)
	}
	if got := w.storedID(); got != "evA" {
		t.Fatalf("the task keeps %q, want evA", got)
	}
}
