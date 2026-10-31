-- Person Preferences (design #37; calendar A3a, to chat A7's spec): one
-- store for what a person chooses for themselves, every key typed and checked
-- by the package that owns it (business/Preferences). A row is a value the
-- person set; a key they never set reads as its default.
CREATE TABLE IF NOT EXISTS person_preferences (
    user_id    uuid        NOT NULL REFERENCES users(id) ON DELETE CASCADE,
    key        text        NOT NULL CHECK (key ~ '^[a-z][a-z0-9_]*(\.[a-z][a-z0-9_]*)?$' AND length(key) <= 64),
    value      jsonb       NOT NULL CHECK (pg_column_size(value) <= 4096),
    updated_at timestamptz NOT NULL DEFAULT now(),
    PRIMARY KEY (user_id, key)
);

-- One zone for a person: the quiet hours' zone, where they set one, is
-- where it starts (the quiet-hours card keeps its column meanwhile).
INSERT INTO person_preferences (user_id, key, value)
SELECT p.user_id, 'zone', to_jsonb(p.quiet_hours_tz)
FROM users_notification_preferences p
WHERE p.quiet_hours_tz IS NOT NULL AND p.quiet_hours_tz <> ''
  AND p.quiet_hours_tz IN (SELECT name FROM pg_timezone_names)
ON CONFLICT (user_id, key) DO NOTHING;
