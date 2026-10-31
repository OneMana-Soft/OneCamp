package basecamp

import (
	"testing"
	"time"

	importProvider "github.com/akashc777/OneCamp/business/Import/provider"
)

// A to-do's due_on and starts_on are days with no time, so they arrive
// as calendar days (written where the team is), never as midnight UTC.
func TestATodosDatesArriveAsDays(t *testing.T) {
	st := toTask(bcRecording{ID: 1, Title: "Book the venue", DueOn: "2026-10-22", StartsOn: "2026-10-20"}, "p", "todo", false, nil)
	if st.DueDay != (importProvider.Day{Year: 2026, Month: time.October, Day: 22}) || st.DueDate != nil {
		t.Errorf("due_on: day %v, instant %v; want the day 2026-10-22 and no instant", st.DueDay, st.DueDate)
	}
	if st.StartDay != (importProvider.Day{Year: 2026, Month: time.October, Day: 20}) || st.StartDate != nil {
		t.Errorf("starts_on: day %v, instant %v; want the day 2026-10-20 and no instant", st.StartDay, st.StartDate)
	}
}
