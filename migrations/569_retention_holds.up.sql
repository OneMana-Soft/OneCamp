-- Holds (the governance trail's H2, business/Retention): an admin puts people
-- on hold, and no retention rule removes their items, deleted ones included,
-- until the hold is released. A hold covers every kind a rule can remove.
--
-- Nothing here is ever deleted. Releasing a hold stamps released_at and
-- released_by; taking a person off one stamps removed_at and removed_by, and
-- putting them back adds a new row. The rows say who was held, from when to
-- when and by whom, beside the audit log's record of each change.
--
-- user_id has no foreign key on purpose: a hold outlives anything that happens
-- to the person's account, and must never block it either.

CREATE TABLE IF NOT EXISTS retention_holds (
    "id"          uuid PRIMARY KEY NOT NULL,
    "name"        varchar(120) NOT NULL,
    "note"        text NOT NULL DEFAULT '',
    "created_by"  uuid NOT NULL,
    "created_at"  timestamptz NOT NULL DEFAULT NOW(),
    "updated_at"  timestamptz NOT NULL DEFAULT NOW(),
    "released_at" timestamptz,
    "released_by" uuid,
    CONSTRAINT retention_holds_name_check CHECK (btrim(name) <> ''),
    CONSTRAINT retention_holds_released_check CHECK ((released_at IS NULL) = (released_by IS NULL))
);

CREATE INDEX IF NOT EXISTS retention_holds_live_idx
    ON retention_holds (created_at) WHERE released_at IS NULL;

CREATE TABLE IF NOT EXISTS retention_hold_people (
    "id"         uuid PRIMARY KEY NOT NULL,
    "hold_id"    uuid NOT NULL REFERENCES retention_holds(id),
    "user_id"    uuid NOT NULL,
    "added_by"   uuid NOT NULL,
    "added_at"   timestamptz NOT NULL DEFAULT NOW(),
    "removed_at" timestamptz,
    "removed_by" uuid,
    CONSTRAINT retention_hold_people_removed_check CHECK ((removed_at IS NULL) = (removed_by IS NULL))
);

-- Every row of a hold, removed ones included, so the foreign key above is
-- indexed whole (tests/integration/fk_indexes_test.go). A partial index on
-- removed_at IS NULL can't serve the lookup Postgres makes for hold_id = $1
-- across all rows. Plain CREATE INDEX, as the table is new in this migration.
CREATE INDEX IF NOT EXISTS retention_hold_people_hold_idx
    ON retention_hold_people (hold_id);

-- One live row per person per hold. It is also the arbiter of AddPeople's
-- "ON CONFLICT (hold_id, user_id) WHERE removed_at IS NULL DO NOTHING"
-- (models/postgres/Retention/holdModel.go), so a person already on the hold
-- is skipped, not added twice.
CREATE UNIQUE INDEX IF NOT EXISTS retention_hold_people_live_idx
    ON retention_hold_people (hold_id, user_id) WHERE removed_at IS NULL;

-- Who is held now, which every sweep asks before it removes anything.
CREATE INDEX IF NOT EXISTS retention_hold_people_user_idx
    ON retention_hold_people (user_id) WHERE removed_at IS NULL;
