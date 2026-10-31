-- Calendar head 5b (gcal-9b): an out-of-office event's decline rule. "new"
-- declines invitations to events that start while it runs and arrive after
-- it is set; "all" also declines those already there when it is set. The
-- message goes with each decline as its note. No row: it declines nothing.
CREATE TABLE IF NOT EXISTS event_ooo_rules (
    event_id   uuid PRIMARY KEY REFERENCES events(id) ON DELETE CASCADE,
    decline    text NOT NULL CHECK (decline IN ('new', 'all')),
    message    text NOT NULL DEFAULT '' CHECK (char_length(message) <= 500),
    updated_at timestamptz NOT NULL DEFAULT now()
);
