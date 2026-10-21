package business

// A doc's or board's insights (docs C6-3): its views and viewers by day over
// a window, members apart from a published page's visitors. A page's visitors
// are counted per day only (their key changes daily), so they are never added
// up into "people" across days; members are.

import (
	"context"
	"time"

	viewModels "github.com/akashc777/OneCamp/models/postgres/ResourceView"
)

// InsightDays is the window insights cover, today included.
const InsightDays = 28

// DayPoint is one day of insights; every day of the window has one.
type DayPoint struct {
	Day         string `json:"day"` // YYYY-MM-DD, UTC
	MemberViews int    `json:"member_views"`
	Members     int    `json:"members"`
	PageViews   int    `json:"page_views"`
	Visitors    int    `json:"visitors"`
}

// Insights is a resource's window of views.
type Insights struct {
	From        string     `json:"from"`
	Days        []DayPoint `json:"days"`
	Members     int        `json:"members"` // distinct over the window
	MemberViews int        `json:"member_views"`
	PageViews   int        `json:"page_views"`
	// VisitorDays is the sum of each day's visitors: one person on two days
	// is two, which is why the page says visitors are counted per day.
	VisitorDays int `json:"visitor_days"`
}

// Seams for tests: the day rows and distinct members.
var (
	daySeries       = viewModels.DaySeries
	distinctMembers = viewModels.DistinctMembers
)

// InsightsFor is a resource's last InsightDays days. The caller must have
// already checked the reader may see who viewed it.
func InsightsFor(ctx context.Context, resourceType, resourceUUID string) (*Insights, error) {
	from := windowStart(now())
	rows, err := daySeries(ctx, resourceType, resourceUUID, from)
	if err != nil {
		return nil, err
	}
	members, err := distinctMembers(ctx, resourceType, resourceUUID, from)
	if err != nil {
		return nil, err
	}
	out := summarise(from, rows)
	out.Members = members
	return out, nil
}

// windowStart is the window's first day (UTC midnight). Pure.
func windowStart(t time.Time) time.Time {
	d := t.UTC()
	return time.Date(d.Year(), d.Month(), d.Day(), 0, 0, 0, 0, time.UTC).AddDate(0, 0, -(InsightDays - 1))
}

// summarise lays rows out over every day of the window, oldest first, and
// totals them; rows outside the window are left out. Pure.
func summarise(from time.Time, rows []viewModels.DayCount) *Insights {
	out := &Insights{From: from.Format("2006-01-02"), Days: make([]DayPoint, InsightDays)}
	at := make(map[string]int, InsightDays)
	for i := range out.Days {
		day := from.AddDate(0, 0, i).Format("2006-01-02")
		out.Days[i].Day = day
		at[day] = i
	}
	for _, r := range rows {
		i, ok := at[r.Day]
		if !ok {
			continue
		}
		switch r.Source {
		case viewModels.SourceMember:
			out.Days[i].MemberViews += r.Views
			out.Days[i].Members += r.Viewers
			out.MemberViews += r.Views
		case viewModels.SourcePage:
			out.Days[i].PageViews += r.Views
			out.Days[i].Visitors += r.Viewers
			out.PageViews += r.Views
			out.VisitorDays += r.Viewers
		}
	}
	return out
}
