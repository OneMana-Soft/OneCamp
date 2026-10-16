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
// (migration 184; prefModels.FocusRunning, the rule email and the held status
// read too), gets no push now. Notify anyway goes past it, but never past
// stillActive (reachableThroughPauseQuery).
//
// Quiet hours are local wall-clock times (HH:MM, validated on write; migration
// 174 cleared any invalid rows) in the person's zone, UTC when none is set, as
// the email deferral reads them. A window may cross midnight (22:00-07:00); one
// that starts where it ends is empty. Zone names are checked against Postgres
// on write, since one it can't read would fail the lookup for everyone.
func notPaused(userCol string) string {
	local := `(NOW() AT TIME ZONE COALESCE(NULLIF(p.quiet_hours_tz, ''), 'UTC'))::time`
	return ` NOT EXISTS (
		SELECT 1 FROM users_notification_preferences p
		WHERE p.user_id = ` + userCol + `
		  AND (
		    (p.notifications_paused_until IS NOT NULL AND p.notifications_paused_until > NOW())
		    OR (p.quiet_hours_enabled
		        AND p.quiet_hours_start IS NOT NULL AND p.quiet_hours_end IS NOT NULL
		        AND CASE
		          WHEN p.quiet_hours_start = p.quiet_hours_end THEN false
		          WHEN p.quiet_hours_start::time < p.quiet_hours_end::time
		            THEN ` + local + ` >= p.quiet_hours_start::time AND ` + local + ` < p.quiet_hours_end::time
		          ELSE ` + local + ` >= p.quiet_hours_start::time OR ` + local + ` < p.quiet_hours_end::time
		        END)
		  ))
		AND NOT EXISTS (
		  SELECT 1 FROM calendar_events fe
		  WHERE ` + prefModels.FocusRunning("fe", userCol) + `) `
}
