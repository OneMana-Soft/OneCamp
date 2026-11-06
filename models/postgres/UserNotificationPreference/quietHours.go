package models

// Quiet hours, decided once in Go (slack-05 weekday schedules, basecamp-19
// quiet hours from working hours): when notifications may reach a person, for
// the email dispatcher's delay, the held status teammates see and Notify
// anyway (business/Notification). The push gate decides the same in SQL
// (domain/UserFCMToken notPaused), and an integration test holds the two to
// the same answer at every instant of a grid.

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"time"

	weekHours "github.com/akashc777/OneCamp/business/WeekHours"
	"github.com/akashc777/OneCamp/helpers"
	"github.com/akashc777/OneCamp/models/pgexec"
	"github.com/google/uuid"
)

// ScheduleKey is the person preference (business/Preferences, the
// person_preferences table) holding the hours notifications may reach a
// person: business/Notification registers and checks it, the row read loads
// it (Schedule) and the push gate reads it.
const ScheduleKey = "notify.schedule"

// Schedule is notify.schedule's value: the hours notifications may reach the
// person, each weekday's ranges ("0" Sunday to "6") in their zone, as Slack's
// "Allow notifications" per weekday. Outside them notifications are held
// until the next range starts. WorkingHours is "follow my working hours": the
// server copies calendar.working_hours and the person's zone into TZ and Week
// when it is set, and again whenever either changes.
type Schedule struct {
	TZ           string                      `json:"tz"`
	Week         map[string][]weekHours.Span `json:"week"`
	WorkingHours bool                        `json:"working_hours,omitempty"`
}

// Hours is the schedule as a week of hours.
func (s Schedule) Hours() weekHours.Hours {
	return weekHours.Hours{TZ: s.TZ, Week: s.Week}
}

// QuietHoursUntil is whether notifications are held from the person at now
// by their hours, and until when; (now, false) when they aren't. A schedule
// decides by itself: outside its ranges they are held until the next range
// starts, by the wall clock in its zone. Without one, the daily quiet-hours
// window decides, when it is on (quiet_hours_enabled). A pause and focus time
// are QuietUntil's. Pure.
func (p *UserNotificationPreference) QuietHoursUntil(now time.Time) (time.Time, bool) {
	if p.Schedule != nil {
		at, ok := p.Schedule.Hours().NextOpen(now, scheduleSearch)
		if !ok || !at.After(now) {
			return now, false
		}
		return at, true
	}
	return p.dailyQuietHours(now)
}

// scheduleSearch is how far ahead a schedule's next range is looked for: two
// weeks and a day. A range shows on its weekday each week, but the clock can
// skip it on the day it changes (spring's gap), and then its next showing is
// the week after: up to 14 days off when asked just as the week before's ends.
// No zone changes its clock two weeks running, so every schedule that checks
// (at least one range) is open somewhere in that span, and the Go hold holds
// whenever the push gate does (tests/integration hold_schedule_parity_test.go).
const scheduleSearch = 15 * 24 * time.Hour

// parseSchedule is a stored notify.schedule, or nil for none (NULL) or one
// that doesn't read: every write is checked (business/Notification), so the
// second never happens but by hand.
func parseSchedule(raw []byte) *Schedule {
	if len(raw) == 0 {
		return nil
	}
	var s Schedule
	if json.Unmarshal(raw, &s) != nil {
		return nil
	}
	return &s
}

// ScheduleOf is the person's notify.schedule, nil when they set none, for
// someone who may have no preferences row: the push gate holds by it all the
// same (HeldStatusOf reads it so).
func ScheduleOf(ctx context.Context, userID uuid.UUID) (*Schedule, error) {
	raw, _, err := pgexec.QueryOne(ctx, `SELECT value FROM person_preferences WHERE user_id = $1 AND key = $2`,
		func(r pgexec.Row) (raw []byte, err error) { return raw, r.Scan(&raw) }, userID, ScheduleKey)
	if err != nil {
		helpers.LogErrorWithContext(ctx, "models/UserNotificationPreference/ScheduleOf err: %+v", err)
		return nil, err
	}
	return parseSchedule(raw), nil
}

// dailyQuietHours is the one daily window (quiet_hours_start to
// quiet_hours_end in quiet_hours_tz, UTC when none), read as it always was:
// it may cross midnight (22:00-07:00), and one that starts where it ends is
// empty, as the push gate reads it.
func (p *UserNotificationPreference) dailyQuietHours(now time.Time) (time.Time, bool) {
	if !p.QuietHoursEnabled || p.QuietHoursStart == nil || p.QuietHoursEnd == nil {
		return now, false
	}
	loc := time.UTC
	if p.QuietHoursTZ != nil && *p.QuietHoursTZ != "" {
		if l, err := time.LoadLocation(*p.QuietHoursTZ); err == nil {
			loc = l
		}
	}
	local := now.In(loc)

	startH, startM, ok1 := parseHHMM(*p.QuietHoursStart)
	endH, endM, ok2 := parseHHMM(*p.QuietHoursEnd)
	if !ok1 || !ok2 {
		return now, false
	}

	startToday := time.Date(local.Year(), local.Month(), local.Day(), startH, startM, 0, 0, loc)
	endToday := time.Date(local.Year(), local.Month(), local.Day(), endH, endM, 0, 0, loc)

	// Two cases: same-day window (08:00 → 17:00) and overnight window (22:00 → 07:00).
	// A window that starts where it ends is empty, as the push gate reads it.
	if endToday.Equal(startToday) {
		return now, false
	}
	if endToday.After(startToday) {
		// Same-day. If now is inside, deliver at end.
		if (local.Equal(startToday) || local.After(startToday)) && local.Before(endToday) {
			return endToday, true
		}
		return now, false
	}
	// Overnight: in window if now ≥ start OR now < end (next morning).
	if local.Equal(startToday) || local.After(startToday) {
		// In window, deliver at end (next day).
		return endToday.AddDate(0, 0, 1), true
	}
	if local.Before(endToday) {
		return endToday, true
	}
	return now, false
}

func parseHHMM(s string) (int, int, bool) {
	parts := strings.Split(s, ":")
	if len(parts) != 2 {
		return 0, 0, false
	}
	var h, m int
	if _, err := fmt.Sscanf(parts[0], "%d", &h); err != nil {
		return 0, 0, false
	}
	if _, err := fmt.Sscanf(parts[1], "%d", &m); err != nil {
		return 0, 0, false
	}
	if h < 0 || h > 23 || m < 0 || m > 59 {
		return 0, 0, false
	}
	return h, m, true
}
