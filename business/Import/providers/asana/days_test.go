package asana

import (
	"testing"
	"time"

	importProvider "github.com/akashc777/OneCamp/business/Import/provider"
)

// due_on and start_on are days with no time, so they arrive as calendar
// days (written where the team is), never as midnight UTC; due_at and
// start_at stay the instants Asana gave.
func TestAsanasDatesWithNoTimeArriveAsDays(t *testing.T) {
	st := buildSourceTask(&asanaTask{GID: "1", Name: "Launch", DueOn: "2026-10-22", StartOn: "2026-10-20"}, "p", nil, "")
	if st.DueDay != (importProvider.Day{Year: 2026, Month: time.October, Day: 22}) || st.DueDate != nil {
		t.Errorf("due_on: day %v, instant %v; want the day 2026-10-22 and no instant", st.DueDay, st.DueDate)
	}
	if st.StartDay != (importProvider.Day{Year: 2026, Month: time.October, Day: 20}) || st.StartDate != nil {
		t.Errorf("start_on: day %v, instant %v; want the day 2026-10-20 and no instant", st.StartDay, st.StartDate)
	}
	st = buildSourceTask(&asanaTask{GID: "2", Name: "Call", DueAt: "2026-10-22T17:30:00.000Z"}, "p", nil, "")
	if st.DueDate == nil || !st.DueDay.IsZero() || st.DueDate.UTC().Format(time.RFC3339) != "2026-10-22T17:30:00Z" {
		t.Errorf("due_at: day %v, instant %v; want the instant 17:30Z", st.DueDay, st.DueDate)
	}
}
