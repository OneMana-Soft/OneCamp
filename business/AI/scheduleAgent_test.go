package business

import (
	"testing"
	"time"
)

// Use a fixed Monday 00:00 UTC anchor so business-hour/weekday logic is
// deterministic regardless of when the test runs.
func mondayUTC() time.Time {
	// 2026-06-29 is a Monday.
	return time.Date(2026, 6, 29, 0, 0, 0, 0, time.UTC)
}

func TestComputeCandidateSlots_BusinessHoursAndWeekday(t *testing.T) {
	now := mondayUTC() // 00:00 UTC Monday; offset 0 → local == UTC
	users := []string{"u1"}
	busy := map[string][]busyInterval{"u1": nil}

	got := computeCandidateSlots(now, 1, 60, 0, 9, 18, 50, busy, users)

	if len(got) == 0 {
		t.Fatal("expected some candidate slots")
	}
	for _, c := range got {
		st, err := time.Parse(time.RFC3339, c.Start)
		if err != nil {
			t.Fatalf("bad start %q: %v", c.Start, err)
		}
		// All slots must start at/after 09:00 and end by 18:00 local (== UTC here).
		if st.Hour() < 9 {
			t.Fatalf("slot before business hours: %s", c.Start)
		}
		end, _ := time.Parse(time.RFC3339, c.End)
		endMin := end.Hour()*60 + end.Minute()
		if endMin > 18*60 {
			t.Fatalf("slot ends after business hours: %s", c.End)
		}
		if wd := st.Weekday(); wd == time.Saturday || wd == time.Sunday {
			t.Fatalf("slot on a weekend: %s", c.Start)
		}
		if !c.AllFree || c.FreeCount != 1 || c.Total != 1 {
			t.Fatalf("expected all-free single-user slot, got %+v", c)
		}
	}
}

func TestComputeCandidateSlots_BusyBlocksOverlap(t *testing.T) {
	now := mondayUTC()
	users := []string{"u1", "u2"}
	// u1 busy 09:00-12:00 Monday; u2 free all day.
	busy := map[string][]busyInterval{
		"u1": {{start: now.Add(9 * time.Hour), end: now.Add(12 * time.Hour)}},
		"u2": nil,
	}
	got := computeCandidateSlots(now, 1, 60, 0, 9, 18, 100, busy, users)

	// A 10:00 slot overlaps u1's busy block → not all-free (only u2 free).
	// A 13:00 slot is after the block → all-free.
	var ten, thirteen *struct {
		allFree   bool
		freeCount int
	}
	for _, c := range got {
		st, _ := time.Parse(time.RFC3339, c.Start)
		if st.Hour() == 10 && st.Minute() == 0 {
			ten = &struct {
				allFree   bool
				freeCount int
			}{c.AllFree, c.FreeCount}
		}
		if st.Hour() == 13 && st.Minute() == 0 {
			thirteen = &struct {
				allFree   bool
				freeCount int
			}{c.AllFree, c.FreeCount}
		}
	}
	if ten == nil || thirteen == nil {
		t.Fatalf("expected 10:00 and 13:00 candidates; got %d slots", len(got))
	}
	if ten.allFree || ten.freeCount != 1 {
		t.Fatalf("10:00 should have only 1 free (u2), got allFree=%v freeCount=%d", ten.allFree, ten.freeCount)
	}
	if !thirteen.allFree || thirteen.freeCount != 2 {
		t.Fatalf("13:00 should be all-free (2), got allFree=%v freeCount=%d", thirteen.allFree, thirteen.freeCount)
	}
}

func TestComputeCandidateSlots_RankingAllFreeFirst(t *testing.T) {
	now := mondayUTC()
	users := []string{"u1", "u2"}
	// u1 busy the entire first business day so early slots are only-1-free;
	// the next day is fully free → all-free slots must rank ahead.
	busy := map[string][]busyInterval{
		"u1": {{start: now.Add(9 * time.Hour), end: now.Add(18 * time.Hour)}},
		"u2": nil,
	}
	got := computeCandidateSlots(now, 3, 60, 0, 9, 18, 5, busy, users)
	if len(got) == 0 {
		t.Fatal("expected candidates")
	}
	// Top candidate must be all-free (Tuesday), not a Monday partial.
	if !got[0].AllFree {
		t.Fatalf("top candidate should be all-free, got %+v", got[0])
	}
}

func TestComputeCandidateSlots_RespectsMaxCandidates(t *testing.T) {
	now := mondayUTC()
	users := []string{"u1"}
	busy := map[string][]busyInterval{"u1": nil}
	got := computeCandidateSlots(now, 5, 30, 0, 9, 18, 4, busy, users)
	if len(got) != 4 {
		t.Fatalf("expected exactly 4 candidates (cap), got %d", len(got))
	}
}

func TestDisplayName(t *testing.T) {
	if got := displayName("alice", "Alice A"); got != "alice" {
		t.Fatalf("expected username, got %q", got)
	}
	if got := displayName("  ", "Bob B"); got != "Bob B" {
		t.Fatalf("expected full name fallback, got %q", got)
	}
	if got := displayName("", ""); got != "" {
		t.Fatalf("expected empty, got %q", got)
	}
}

func TestMinTime(t *testing.T) {
	a := mondayUTC()
	b := a.Add(time.Hour)
	if !minTime(a, b).Equal(a) || !minTime(b, a).Equal(a) {
		t.Fatal("minTime should return the earlier time regardless of order")
	}
}
