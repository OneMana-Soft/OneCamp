//go:build integration
// +build integration

package integration_test

// One hold, decided twice (slack-05 weekday schedules, basecamp-19 quiet hours
// from working hours): the push gate decides in SQL whether a person is held
// now (domain/UserFCMToken notPaused), and email's delay, the held status a
// teammate sees and Notify anyway decide in Go (QuietHoursUntil). Against
// Postgres 12, at every instant of a grid (every five minutes, and a second
// before each, across both of New York's 2026 daylight-saving changes), for
// every week of hours in Asia/Kolkata and America/New_York, with and without
// a daily window that a schedule overrides, someone with no preferences row,
// and the daily window alone: the two say the same.
//
// Run: go test -tags=integration ./tests/integration/ -run TestTheGateAndTheGoHoldAgreeOnSchedules -v

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"

	notificationBusiness "github.com/akashc777/OneCamp/business/Notification"
	weekHours "github.com/akashc777/OneCamp/business/WeekHours"
	fcmDomain "github.com/akashc777/OneCamp/domain/UserFCMToken"
	prefModels "github.com/akashc777/OneCamp/models/postgres/UserNotificationPreference"
	"github.com/akashc777/OneCamp/tests/integration"
)

// parityWeek is the same ranges ("09:00-18:00") on each of days (0 Sunday).
func parityWeek(days []int, ranges ...string) map[string][]weekHours.Span {
	out := map[string][]weekHours.Span{}
	for _, d := range days {
		for _, r := range ranges {
			a, b, _ := strings.Cut(r, "-")
			out[strconv.Itoa(d)] = append(out[strconv.Itoa(d)], weekHours.Span{Start: a, End: b})
		}
	}
	return out
}

func TestTheGateAndTheGoHoldAgreeOnSchedules(t *testing.T) {
	env := integration.ConnectServerPostgres(t)
	weekdays, everyDay, sunday := []int{1, 2, 3, 4, 5}, []int{0, 1, 2, 3, 4, 5, 6}, []int{0}
	weeks := map[string]map[string][]weekHours.Span{
		"weekdays only":             parityWeek(weekdays, "09:00-18:00"),
		"weekend off":               parityWeek(weekdays, "00:00-24:00"),
		"two ranges a day":          parityWeek(weekdays, "09:00-12:30", "13:30-17:00"),
		"evenings to midnight":      parityWeek(everyDay, "18:00-24:00"),
		"starts in spring's gap":    parityWeek(sunday, "02:30-05:00"),
		"inside spring's gap":       parityWeek(sunday, "02:00-02:30"),
		"in autumn's repeated hour": parityWeek(sunday, "01:30-01:45"),
	}

	type person struct {
		name string
		id   uuid.UUID
	}
	var people []person
	add := func(name string, row bool, window *[3]string, schedule *prefModels.Schedule) {
		id := uuid.New()
		if _, err := env.PG.Exec(`INSERT INTO users (id, email_id, username, display_name, created_at, updated_at)
			VALUES ($1, $2, $3, $4, NOW(), NOW())`, id, id.String()+"@example.test", "p"+id.String()[:8], name); err != nil {
			t.Fatal(err)
		}
		if row {
			enabled, start, end, tz := false, "", "", ""
			if window != nil {
				enabled, start, end, tz = true, window[0], window[1], window[2]
			}
			if _, err := env.PG.Exec(`INSERT INTO users_notification_preferences
				(user_id, unsubscribe_token, quiet_hours_enabled, quiet_hours_start, quiet_hours_end, quiet_hours_tz)
				VALUES ($1, $2, $3, NULLIF($4, ''), NULLIF($5, ''), NULLIF($6, ''))`, id, uuid.NewString(), enabled, start, end, tz); err != nil {
				t.Fatal(err)
			}
		}
		if schedule != nil {
			v, _ := json.Marshal(schedule)
			if _, err := env.PG.Exec(`INSERT INTO person_preferences (user_id, key, value) VALUES ($1, $2, $3)`, id, prefModels.ScheduleKey, v); err != nil {
				t.Fatal(err)
			}
		}
		people = append(people, person{name, id})
	}
	for name, w := range weeks {
		for _, tz := range []string{"Asia/Kolkata", "America/New_York"} {
			add(name+" in "+tz, true, nil, &prefModels.Schedule{TZ: tz, Week: w})
			// A daily window the schedule overrides: it would hold at noon.
			add(name+" in "+tz+", over a daily window", true, &[3]string{"12:00", "14:00", tz}, &prefModels.Schedule{TZ: tz, Week: w})
		}
	}
	add("weekdays only in America/New_York, with no preferences row", false, nil, &prefModels.Schedule{TZ: "America/New_York", Week: weeks["weekdays only"]})
	add("the daily window alone, overnight in Asia/Kolkata", true, &[3]string{"22:00", "07:00", "Asia/Kolkata"}, nil)
	add("the daily window alone, overnight in America/New_York", true, &[3]string{"22:00", "07:00", "America/New_York"}, nil)
	add("the daily window alone, by day in UTC", true, &[3]string{"09:00", "17:00", ""}, nil)
	add("nothing set", true, nil, nil)
	// A day stored as null (only by hand: a write leaves such days out) is
	// off, and fails no lookup.
	nullSunday := parityWeek(weekdays, "09:00-17:00")
	nullSunday["0"] = nil
	add("weekdays only, Sunday stored as null", true, nil, &prefModels.Schedule{TZ: "Asia/Kolkata", Week: nullSunday})

	gate := `SELECT g.t, ` + fcmDomain.NotPausedAtForTest("$1::uuid", "g.t") + `
		FROM generate_series($2::timestamptz, $3::timestamptz, interval '5 minutes') AS g(t) ORDER BY g.t`
	stretches := [][2]time.Time{
		{time.Date(2026, 3, 6, 0, 0, 0, 0, time.UTC), time.Date(2026, 3, 10, 0, 0, 0, 0, time.UTC)},
		{time.Date(2026, 10, 30, 0, 0, 0, 0, time.UTC), time.Date(2026, 11, 3, 0, 0, 0, 0, time.UTC)},
	}
	checked := 0
	for _, who := range people {
		// The row as the Go hold reads it, as HeldStatusOf does: with no row,
		// the schedule alone.
		p, err := prefModels.GetByUserID(who.id)
		if errors.Is(err, sql.ErrNoRows) {
			p = &prefModels.UserNotificationPreference{UserID: who.id}
			if p.Schedule, err = prefModels.ScheduleOf(context.Background(), who.id); err != nil {
				t.Fatal(err)
			}
			now := time.Now()
			got, err := notificationBusiness.HeldStatusOf(who.id, now)
			want := notificationBusiness.HeldFor(p, now)
			if err != nil || got.Reason != want.Reason || (got.Until == nil) != (want.Until == nil) || (got.Until != nil && !got.Until.Equal(*want.Until)) {
				t.Errorf("%s: HeldStatusOf says %+v (%v), the row's hold %+v", who.name, got, err, want)
			}
		} else if err != nil {
			t.Fatal(err)
		}
		wrong := 0
		for _, s := range stretches {
			for _, early := range []time.Duration{0, time.Second} {
				rows, err := env.PG.Query(gate, who.id, s[0].Add(-early), s[1].Add(-early))
				if err != nil {
					t.Fatalf("%s: the gate: %v", who.name, err)
				}
				for rows.Next() {
					var at time.Time
					var open bool
					if err := rows.Scan(&at, &open); err != nil {
						t.Fatal(err)
					}
					checked++
					held := notificationBusiness.HeldFor(p, at)
					if open != (held.Reason == "") {
						if wrong++; wrong <= 12 {
							t.Errorf("%s at %v (%s): the gate lets through %v, Go holds %+v", who.name, at.UTC(), at.In(zoneOf(p)).Format("Mon 15:04:05 MST"), open, held)
						}
					}
				}
				if err := rows.Err(); err != nil {
					t.Fatal(err)
				}
				rows.Close()
			}
		}
		if wrong > 12 {
			t.Errorf("%s: %d instants disagree in all", who.name, wrong)
		}
	}
	if want := len(people) * 2 * 2 * 4 * 24 * 12; checked < want {
		t.Fatalf("checked %d instants, want at least %d", checked, want)
	}
}

// zoneOf is the zone a person's hold is read in, for the failure's words.
func zoneOf(p *prefModels.UserNotificationPreference) *time.Location {
	name := "UTC"
	if p.Schedule != nil {
		name = p.Schedule.TZ
	} else if p.QuietHoursTZ != nil && *p.QuietHoursTZ != "" {
		name = *p.QuietHoursTZ
	}
	loc, err := time.LoadLocation(name)
	if err != nil {
		return time.UTC
	}
	return loc
}
