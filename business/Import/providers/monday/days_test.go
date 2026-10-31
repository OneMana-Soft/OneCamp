package monday

import (
	"testing"
	"time"

	importProvider "github.com/akashc777/OneCamp/business/Import/provider"
)

// A date column with no time, and a timeline's two ends, are days, so
// they arrive as calendar days (written where the team is), never as
// midnight UTC. A date column with a time is the instant monday gives (UTC).
func TestMondaysDatesWithNoTimeArriveAsDays(t *testing.T) {
	p := New()
	st := p.itemToSourceTask(mondayItem{ID: "1", Name: "Ship", ColumnValues: []mondayColumnValue{
		cv("timeline", "timeline", "Timeline", "", `{"from":"2026-10-01","to":"2026-10-10"}`),
		cv("date4", "date", "Due date", "2026-10-12", `{"date":"2026-10-12"}`),
	}}, "b", "", 0, nil, fieldColumns{})
	if st.DueDay != (importProvider.Day{Year: 2026, Month: time.October, Day: 12}) || st.DueDate != nil {
		t.Errorf("date column: day %v, instant %v; want the day 2026-10-12 and no instant", st.DueDay, st.DueDate)
	}
	if st.StartDay != (importProvider.Day{Year: 2026, Month: time.October, Day: 1}) || st.StartDate != nil {
		t.Errorf("timeline's start: day %v, instant %v; want the day 2026-10-01 and no instant", st.StartDay, st.StartDate)
	}
	st = p.itemToSourceTask(mondayItem{ID: "2", Name: "Call", ColumnValues: []mondayColumnValue{
		cv("date4", "date", "Due date", "2026-10-12 09:30", `{"date":"2026-10-12","time":"09:30:00"}`),
	}}, "b", "", 0, nil, fieldColumns{})
	if st.DueDate == nil || !st.DueDay.IsZero() || st.DueDate.UTC().Format(time.RFC3339) != "2026-10-12T09:30:00Z" {
		t.Errorf("date column with a time: instant %v, day %v; want 2026-10-12T09:30:00Z", st.DueDate, st.DueDay)
	}
}
