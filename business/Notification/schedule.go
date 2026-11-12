package notification

// A person's notification schedule (slack-05 weekday schedules, basecamp-19
// quiet hours from working hours): notify.schedule, a person preference
// (business/Preferences), is the hours notifications may reach them, each
// weekday's ranges in their zone, as Slack's "Allow notifications" per day.
// Outside them, pushes wait (the push gate, domain/UserFCMToken) and so does
// email (deliveryTime), and a teammate sees them held until the next range
// (HeldFor). Only the person reads and sets theirs, through their
// preferences; nobody else's hours are ever shown.
//
// {"working_hours": true} follows their calendar working hours instead: the
// server copies calendar.working_hours and their zone into the value when it
// is set, and again in the same change whenever either changes
// (followWorkingHours).

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"

	preferencesBusiness "github.com/akashc777/OneCamp/business/Preferences"
	weekHours "github.com/akashc777/OneCamp/business/WeekHours"
	prefModels "github.com/akashc777/OneCamp/models/postgres/UserNotificationPreference"
	"github.com/google/uuid"
)

// prefWorkingHours is business/Calendar's PrefWorkingHours, by name: the
// calendar imports this package, so this one can't import it.
const prefWorkingHours = "calendar.working_hours"

var (
	// knownZone is whether Postgres reads a zone (the push gate converts
	// times there); swapped in tests.
	knownZone = prefModels.KnownTimeZone
	// keptValue is one of the person's preferences as they set it, nil when
	// they set none; swapped in tests.
	keptValue = func(ctx context.Context, user uuid.UUID, key string) (json.RawMessage, error) {
		set, err := preferencesBusiness.OfPeople(ctx, []uuid.UUID{user}, key)
		if err != nil {
			return nil, err
		}
		return set[user], nil
	}
)

func init() {
	preferencesBusiness.Register(prefModels.ScheduleKey, preferencesBusiness.Key{
		Default: json.RawMessage("null"),
		Check:   scheduleProblem,
		Prepare: keptSchedule,
	})
	// A schedule that follows working hours follows them as they change,
	// and the zone they are read in.
	preferencesBusiness.OnChange(prefWorkingHours, followWorkingHours)
	preferencesBusiness.OnChange("zone", followWorkingHours)
}

// followWorkingHours copies the person's working hours and zone into their
// schedule again when it follows them, after either changed. With nothing
// left to follow (their hours cleared, or a zone Postgres can't read) the
// schedule is cleared: notifications reach them as before they set one,
// rather than by hours they took away. Their devices are told either way
// (Set). It runs in the change it follows (OnChange): a copy that can't be
// kept fails that change too, so the person is told their save didn't
// happen, and pushes are never held by hours they no longer keep (V-532).
func followWorkingHours(ctx context.Context, user uuid.UUID, _ json.RawMessage) error {
	raw, err := keptValue(ctx, user, prefModels.ScheduleKey)
	if err != nil {
		return fmt.Errorf("notification/followWorkingHours %s: read the schedule: %w", user, err)
	}
	var s prefModels.Schedule
	if raw == nil || json.Unmarshal(raw, &s) != nil || !s.WorkingHours {
		return nil
	}
	err = preferencesBusiness.Set(ctx, user, prefModels.ScheduleKey, json.RawMessage(`{"working_hours":true}`))
	var refused *preferencesBusiness.Refused
	if errors.As(err, &refused) {
		err = preferencesBusiness.Set(ctx, user, prefModels.ScheduleKey, json.RawMessage("null"))
	}
	if err != nil {
		return fmt.Errorf("notification/followWorkingHours %s: %w", user, err)
	}
	return nil
}

// maxRangesADay is as many ranges as a day of the schedule takes, as many
// as a day of working hours does.
const maxRangesADay = 6

// scheduleProblem is why a schedule can't be kept as given, or nil: a week of
// hours in a zone this server knows, with at least one range in it. One that
// follows working hours is checked when they are copied in (keptSchedule).
// Pure.
func scheduleProblem(v json.RawMessage) error {
	var s prefModels.Schedule
	if json.Unmarshal(v, &s) != nil {
		return &preferencesBusiness.Refused{Msg: "A schedule is the hours notifications may reach you, each day's in your time zone."}
	}
	if s.WorkingHours {
		return nil
	}
	return weekProblem(s)
}

// weekProblem is why a schedule's week can't be kept, or nil. Pure.
func weekProblem(s prefModels.Schedule) error {
	ranges := 0
	for _, day := range s.Week {
		if len(day) > maxRangesADay {
			return &preferencesBusiness.Refused{Msg: "Up to six ranges a day."}
		}
		ranges += len(day)
	}
	if ranges == 0 {
		// A week with no hours in it is a pause, which says when it ends.
		return &preferencesBusiness.Refused{Msg: "Use Pause for a whole stretch off."}
	}
	if err := preferencesBusiness.ZoneProblem(s.TZ); err != nil {
		return err
	}
	if _, err := s.Hours().Check(); err != nil {
		return &preferencesBusiness.Refused{Msg: err.Error()}
	}
	return nil
}

// keptSchedule is the schedule kept for the person from one scheduleProblem
// passed, in one shape: their working hours and zone copied in when it
// follows them, and its zone one Postgres reads, since the push gate reads
// the schedule there and a zone it can't read would fail every lookup that
// touches the person.
func keptSchedule(ctx context.Context, user uuid.UUID, v json.RawMessage) (json.RawMessage, error) {
	var s prefModels.Schedule
	if json.Unmarshal(v, &s) != nil {
		return nil, scheduleProblem(v)
	}
	if s.WorkingHours {
		hours, err := workingHoursOf(ctx, user)
		if err != nil {
			return nil, err
		}
		s = hours
		if err := weekProblem(s); err != nil {
			return nil, err
		}
	}
	known, err := knownZone(s.TZ)
	if err != nil {
		return nil, err
	}
	if !known {
		return nil, &preferencesBusiness.Refused{Msg: "That isn't a time zone this server knows."}
	}
	// A day with no ranges is left out: it reads as off all the same, and the
	// push gate reads each day it keeps as a list ("1": null would not be).
	week := make(map[string][]weekHours.Span, len(s.Week))
	for day, ranges := range s.Week {
		if len(ranges) > 0 {
			week[day] = ranges
		}
	}
	return json.Marshal(prefModels.Schedule{TZ: s.TZ, Week: week, WorkingHours: s.WorkingHours})
}

// workingHoursOf is a schedule of the person's working hours in their zone,
// marked as following them, or a refusal saying which of the two to set.
// calendar.working_hours is a week of ranges like the schedule's, read in
// the person-wide zone (business/Calendar HoursOf).
func workingHoursOf(ctx context.Context, user uuid.UUID) (prefModels.Schedule, error) {
	out := prefModels.Schedule{WorkingHours: true}
	hours, err := keptValue(ctx, user, prefWorkingHours)
	if err != nil {
		return out, err
	}
	if hours == nil || json.Unmarshal(hours, &out.Week) != nil || len(out.Week) == 0 {
		return out, &preferencesBusiness.Refused{Msg: "Set your working hours first."}
	}
	zone, err := keptValue(ctx, user, "zone")
	if err != nil {
		return out, err
	}
	if zone == nil || json.Unmarshal(zone, &out.TZ) != nil || out.TZ == "" {
		return out, &preferencesBusiness.Refused{Msg: "Set your time zone first."}
	}
	return out, nil
}
