-- Migration 200: a repeating task's dates are worked out where the person who
-- set the repeat lives, and a monthly or yearly repeat remembers its day.
--   time_zone   IANA name ('Asia/Kolkata'); '' means UTC, which is how every
--               repeat was worked out before: a weekly repeat on Monday, due at
--               midnight in India, is Sunday evening in UTC and came out on
--               Tuesday.
--   anchor_day  the day of the month a monthly or yearly repeat was set for
--               (0: none). Each copy took the last one's due date as its base,
--               so a repeat on the 31st landed on Feb 28 and stayed on the 28th.
ALTER TABLE task_recurrences ADD COLUMN IF NOT EXISTS "time_zone" varchar(64) NOT NULL DEFAULT '';
ALTER TABLE task_recurrences ADD COLUMN IF NOT EXISTS "anchor_day" smallint NOT NULL DEFAULT 0
    CHECK (anchor_day BETWEEN 0 AND 31);

-- Repeats set before now: the zone their creator gave for quiet hours, where
-- they gave one Postgres knows. The rest keep UTC until the repeat is next set.
UPDATE task_recurrences r
   SET time_zone = p.quiet_hours_tz
  FROM users_notification_preferences p
 WHERE p.user_id = r.created_by
   AND r.time_zone = ''
   AND p.quiet_hours_tz IN (SELECT name FROM pg_timezone_names);
