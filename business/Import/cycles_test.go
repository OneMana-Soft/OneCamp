package business

import (
	"testing"
	"time"

	importProvider "github.com/akashc777/OneCamp/business/Import/provider"
)

// L-2: a project here runs one cycle at a time, and a Linear project's issues
// can come from two teams whose cycles share days. Each cycle that shares
// days with an earlier one folds into the one running when it starts, with
// the tasks it finished; cycles that only touch stay apart.
func TestCyclesThatShareDaysFoldIntoTheOneRunningWhenTheyStart(t *testing.T) {
	day := func(d int) time.Time { return time.Date(2026, 9, d, 0, 0, 0, 0, time.UTC) }
	cycle := func(id string, from, to, done int) importProvider.SourceCycle {
		return importProvider.SourceCycle{SourceID: id, Name: id, StartsAt: day(from), EndsAt: day(to), Done: done}
	}
	kept, into := placeCycles([]importProvider.SourceCycle{
		cycle("mob-2", 8, 22, 1), // shares days with eng-1
		cycle("eng-2", 15, 29, 4),
		cycle("eng-1", 1, 15, 2),
		cycle("mob-3", 22, 30, 3), // shares days with eng-2, which ends after it starts
	})
	var got []string
	for _, c := range kept {
		got = append(got, c.SourceID)
	}
	if len(got) != 2 || got[0] != "eng-1" || got[1] != "eng-2" {
		t.Fatalf("kept %v, want [eng-1 eng-2] in order", got)
	}
	if into["mob-2"] != "eng-1" || into["mob-3"] != "eng-2" || len(into) != 2 {
		t.Fatalf("folded %v, want mob-2 into eng-1 and mob-3 into eng-2", into)
	}
	if kept[0].Done != 3 || kept[1].Done != 7 {
		t.Fatalf("done %d and %d, want each kept cycle's own plus what folded into it (3, 7)", kept[0].Done, kept[1].Done)
	}
}
