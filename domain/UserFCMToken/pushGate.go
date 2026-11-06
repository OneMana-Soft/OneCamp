package domain

import prefModels "github.com/akashc777/OneCamp/models/postgres/UserNotificationPreference"

// pushGate is the gate every lookup of devices to push to applies, by person:
// an account still active (stillActive) that isn't held from pushes now
// (notPaused). It sits in the SQL so the places that send pushes need no
// change of their own. Deactivation leaves a person's devices' tokens in
// place, and the lookups other than Deliver's asked only notPaused, so after
// BE-11-G-3 a deactivated person's phone was still pushed by every place that
// looked devices up for itself.
// userCol is the lookup's user id column (its tables use different aliases).
func pushGate(userCol string) string {
	return stillActive(userCol) + ` AND` + notPaused(userCol)
}

// notPaused is the push gate's hold: a person who has paused notifications,
// is inside their quiet hours, or is in one of their own focus-time events
// (migration 184; prefModels.FocusStore's Running, the rule email and the
// held status read too), gets no push now. Notify anyway goes past it, but
// never past stillActive (reachableThroughPauseQuery). Focus time is read
// from whichever store holds the calendar's events, decided once for the
// query (prefModels.FocusStoreNow).
//
// Quiet hours are local wall-clock times (HH:MM, validated on write; migration
// 174 cleared any invalid rows) in the person's zone, UTC when none is set, as
// the email deferral reads them. A window may cross midnight (22:00-07:00); one
// that starts where it ends is empty. Zone names are checked against Postgres
// on write, since one it can't read would fail the lookup for everyone.
//
// A schedule (notify.schedule, a person preference in person_preferences)
// decides instead of the daily window, by itself, row or no row: outside its
// ranges a person is held. Its ranges are read by the wall clock in its zone,
// as the Go hold reads them (prefModels QuietHoursUntil, weekHours Inside):
// the weekday the clock shows there (EXTRACT(DOW), "0" Sunday), at or after
// a range's start and before its end, and "24:00" ends the day (Postgres's
// time reads it). An integration test holds the two to the same answer at
// every instant of a grid, across daylight-saving changes. The daily window
// counts only for someone with no schedule; quiet_hours_enabled stays its
// switch alone.
func notPaused(userCol string) string {
	return notPausedAt(userCol, "NOW()")
}

// notPausedAt is notPaused at the instant the SQL expression now names
// (NOW() for every lookup; a test names other instants, to hold the gate to
// the Go hold's answer at each, NotPausedAtForTest). Focus time is read at
// NOW() whatever now says (prefModels.FocusStore's Running).
func notPausedAt(userCol, now string) string {
	local := `(` + now + ` AT TIME ZONE COALESCE(NULLIF(p.quiet_hours_tz, ''), 'UTC'))::time`
	// The schedule's wall clock: now in its zone, a timestamp without one.
	clock := `(` + now + ` AT TIME ZONE (s.value->>'tz'))`
	// Its ranges for the weekday that clock shows; anything but a list (a day
	// it leaves out reads as none) is no ranges, never a lookup that fails.
	today := `(s.value->'week'->(EXTRACT(DOW FROM ` + clock + `)::int::text))`
	focus := prefModels.FocusStoreNow()
	return ` NOT EXISTS (
		SELECT 1 FROM users_notification_preferences p
		WHERE p.user_id = ` + userCol + `
		  AND (
		    (p.notifications_paused_until IS NOT NULL AND p.notifications_paused_until > ` + now + `)
		    OR (p.quiet_hours_enabled
		        AND p.quiet_hours_start IS NOT NULL AND p.quiet_hours_end IS NOT NULL
		        AND NOT EXISTS (SELECT 1 FROM person_preferences s WHERE s.user_id = p.user_id AND s.key = '` + prefModels.ScheduleKey + `')
		        AND CASE
		          WHEN p.quiet_hours_start = p.quiet_hours_end THEN false
		          WHEN p.quiet_hours_start::time < p.quiet_hours_end::time
		            THEN ` + local + ` >= p.quiet_hours_start::time AND ` + local + ` < p.quiet_hours_end::time
		          ELSE ` + local + ` >= p.quiet_hours_start::time OR ` + local + ` < p.quiet_hours_end::time
		        END)
		  ))
		AND NOT EXISTS (
		  SELECT 1 FROM person_preferences s
		  WHERE s.user_id = ` + userCol + ` AND s.key = '` + prefModels.ScheduleKey + `'
		    AND NOT EXISTS (
		      SELECT 1 FROM jsonb_array_elements(CASE WHEN jsonb_typeof(` + today + `) = 'array' THEN ` + today + ` END) r
		      WHERE (` + clock + `)::time >= (r->>'start')::time AND (` + clock + `)::time < (r->>'end')::time))
		AND NOT EXISTS (
		  SELECT 1 FROM ` + focus.Events("fe") + `
		  WHERE ` + focus.Running("fe", userCol) + `) `
}
