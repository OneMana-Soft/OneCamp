package business

// A date a source keeps with no time of day arrives as a provider.Day, and is
// written the way the app writes a day someone picks: that day's midnight
// where the team is (provider/day.go says why).

import (
	"context"
	"strings"
	"time"

	importProvider "github.com/akashc777/OneCamp/business/Import/provider"
	"github.com/akashc777/OneCamp/helpers"
	importModels "github.com/akashc777/OneCamp/models/postgres/Import"
	prefModels "github.com/akashc777/OneCamp/models/postgres/UserNotificationPreference"
)

// taskDates are st's start and due as the task keeps them: a time the source
// gave stays as it is, and a day becomes its midnight in loc (noon UTC when
// no zone is known). Pure.
func taskDates(st *importProvider.SourceTask, loc *time.Location) (start, due *time.Time) {
	return dayOr(st.StartDate, st.StartDay, loc), dayOr(st.DueDate, st.DueDay, loc)
}

func dayOr(at *time.Time, day importProvider.Day, loc *time.Location) *time.Time {
	if at != nil || day.IsZero() {
		return at
	}
	v := day.At(loc)
	return &v
}

// dayZone is where an import's days fall: the zone the admin's browser sent
// with the job (options.time_zone), else the one they set with their quiet
// hours (the one place a person's zone is kept), else none.
// A name Go can't read, or the server's own zone, is no zone: noon UTC, never
// a failed import.
func dayZone(ctx context.Context, job *importModels.Job, opts importProvider.JobOptions) *time.Location {
	sent, _ := opts["time_zone"].(string)
	if loc, ok := helpers.FirstLocation(sent); ok {
		return loc
	}
	if job == nil || job.TriggeredBy == nil {
		return nil
	}
	id := job.TriggeredBy.String()
	zones, err := prefModels.ZonesOf(ctx, []string{id})
	if err != nil {
		return nil
	}
	if loc, ok := helpers.FirstLocation(zones[strings.ToLower(id)]); ok {
		return loc
	}
	return nil
}
