package business

import (
	"testing"
	"time"

	memoryModels "github.com/akashc777/OneCamp/models/postgres/WorkspaceMemory"
)

// V-615: the one rule that holds a signal its member handled.
func TestAHandledNudgeStaysDownUntilItsItemChanges(t *testing.T) {
	now := time.Date(2026, 11, 17, 12, 0, 0, 0, time.UTC)
	ago := func(d time.Duration) time.Time { return now.Add(-d) }
	day := 24 * time.Hour
	for _, c := range []struct {
		name      string
		handledAt time.Time
		changedAt time.Time
		wantHolds bool
	}{
		{"never handled", time.Time{}, ago(10 * day), false},
		{"handled an hour ago, unchanged", ago(time.Hour), ago(10 * day), true},
		{"handled an hour ago, changed since", ago(time.Hour), ago(time.Minute), true},
		{"handled 4 days ago, unchanged", ago(4 * day), ago(10 * day), true},
		{"handled 29 days ago, unchanged", ago(29 * day), ago(40 * day), true},
		{"handled 4 days ago, changed since", ago(4 * day), ago(day), false},
		{"handled 4 days ago, changed at that moment", ago(4 * day), ago(4 * day), true},
		{"handled 4 days ago, its change time unknown", ago(4 * day), time.Time{}, true},
	} {
		if got := nudgeHeld(c.handledAt, c.changedAt, now); got != c.wantHolds {
			t.Errorf("%s: held %v; want %v", c.name, got, c.wantHolds)
		}
	}
}

func TestAQuestionIsNudgedOfOnlyWhileRecentlyStale(t *testing.T) {
	now := time.Date(2026, 11, 17, 12, 0, 0, 0, time.UTC)
	question := func(status string, untouchedDays int) *memoryModels.MemoryItem {
		return &memoryModels.MemoryItem{Kind: memoryModels.KindQuestion, Status: status, UpdatedAt: now.AddDate(0, 0, -untouchedDays)}
	}
	stale := int(nudgeStaleAfter / (24 * time.Hour))
	for _, c := range []struct {
		name string
		it   *memoryModels.MemoryItem
		want bool
	}{
		{"untouched 6 days", question(memoryModels.StatusOpen, stale-1), false},
		{"just gone stale", question(memoryModels.StatusOpen, stale), true},
		{"stale NeedsMeNowDays", question(memoryModels.StatusOpen, stale+NeedsMeNowDays), true},
		{"stale a day longer", question(memoryModels.StatusOpen, stale+NeedsMeNowDays+1), false},
		{"resolved", question(memoryModels.StatusResolved, stale), false},
		{"a commitment", &memoryModels.MemoryItem{Kind: memoryModels.KindCommitment, Status: memoryModels.StatusOpen, UpdatedAt: now.AddDate(0, 0, -stale)}, false},
		{"none", nil, false},
	} {
		if got := questionNeedsMeNow(c.it, now); got != c.want {
			t.Errorf("%s: needs me now %v; want %v", c.name, got, c.want)
		}
	}
}

// A dismissal is kept nudgePurgeAfter, so it must outlive every window in
// which its signal can still be raised; else the purge would let it back.
func TestADismissalOutlivesEverySignalItHolds(t *testing.T) {
	window := time.Duration(NeedsMeNowDays+1) * 24 * time.Hour
	if window >= nudgePurgeAfter {
		t.Fatalf("a signal can be raised for %v after it is first raised, but a dismissal is kept only %v", window, nudgePurgeAfter)
	}
}
