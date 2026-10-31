package Domain

import (
	"reflect"
	"testing"
	"time"
)

// The graph answers each count in a block of its own and each sum as an
// object of its own in one block (ReadPlanTotalsOf's query), each project's
// blocks named with its place in the list; what it leaves out is none.
func TestPlanTotalsAreReadFromTheGraphsCountsAndSums(t *testing.T) {
	raw := map[string][]map[string]any{
		"tasksp0":        {{"n": float64(9)}},
		"openp0":         {{"n": float64(3)}},
		"openPointedp0":  {{"n": float64(1)}},
		"done0p0":        {{"n": float64(2)}},
		"donePointed0p0": {{"n": float64(1)}},
		"done1p0":        {{"n": float64(1)}},
		"sumsp0":         {{"open": float64(3)}, {"done0": float64(2)}},
		"firstDonep0":    {{"task_status_since": "2026-09-01T09:00:00Z"}},
		"projectp0":      {{"project_created_at": "2026-08-01T10:30:00.5Z"}},
		// Another project in the same answer counts only its own blocks.
		"tasksp1":   {{"n": float64(4)}},
		"openp1":    {{"n": float64(4)}},
		"done0p1":   {{"n": float64(7)}},
		"sumsp1":    {{"open": float64(8)}},
		"projectp1": {{}},
	}
	got := planTotalsOf(raw, 3, "p0")
	first, made := time.Date(2026, 9, 1, 9, 0, 0, 0, time.UTC), time.Date(2026, 8, 1, 10, 30, 0, 5e8, time.UTC)
	want := PlanTotals{Tasks: 9, Open: Tally{N: 3, Pointed: 1, Points: 3}, Done: []Tally{{N: 2, Pointed: 1, Points: 2}, {N: 1}, {}}, FirstDone: &first, CreatedAt: &made}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("totals %+v, want %+v", got, want)
	}
	other := PlanTotals{Tasks: 4, Open: Tally{N: 4, Points: 8}, Done: []Tally{{N: 7}, {}, {}}}
	if got := planTotalsOf(raw, 3, "p1"); !reflect.DeepEqual(got, other) {
		t.Errorf("the second project's totals %+v, want %+v", got, other)
	}
	// A project with no tasks, or none in the answer: every count none, no days known.
	for _, p := range []string{"p1x", "p2"} {
		if got := planTotalsOf(map[string][]map[string]any{"projectp1x": {{}}}, 4, p); got.Tasks != 0 || len(got.Done) != 4 || got.FirstDone != nil || got.CreatedAt != nil {
			t.Errorf("no tasks (%s): %+v", p, got)
		}
	}
}
