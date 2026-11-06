-- Sensitivity labels (miro-44 head L, business/Label): a workspace admin names
-- labels, each with rules ("no_guest_links", "no_export"; the rules are
-- registered in code, so a new one needs no migration), and anyone who can
-- edit a board or a doc puts one on it. While it is on, its rules hold on the
-- server, and every refusal is a row in the audit log.
--
-- Nothing here is ever deleted. Removing a label stamps deleted_at and
-- deleted_by, and its rules stop holding on every item it was on. Taking a
-- label off an item, or putting another on, stamps unset_at and unset_by on
-- the item's row and adds a new one, so the rows say what each item was
-- labelled, from when to when and by whom, beside the audit log's record of
-- each change.
--
-- item_uuid has no foreign key: an item is a Dgraph node (a board, a doc), and
-- its label must never block it from being deleted or restored.

CREATE TABLE IF NOT EXISTS sensitivity_labels (
    "id"         uuid PRIMARY KEY NOT NULL,
    "name"       varchar(60) NOT NULL,
    "colour"     varchar(20) NOT NULL DEFAULT 'gray',
    "rank"       integer NOT NULL DEFAULT 0,
    "rules"      text[] NOT NULL DEFAULT '{}',
    "created_by" uuid NOT NULL,
    "created_at" timestamptz NOT NULL DEFAULT NOW(),
    "updated_at" timestamptz NOT NULL DEFAULT NOW(),
    "deleted_at" timestamptz,
    "deleted_by" uuid,
    CONSTRAINT sensitivity_labels_name_check CHECK (btrim(name) <> ''),
    CONSTRAINT sensitivity_labels_rank_check CHECK (rank BETWEEN 0 AND 100),
    CONSTRAINT sensitivity_labels_deleted_check CHECK ((deleted_at IS NULL) = (deleted_by IS NULL))
);

-- Names are unique among live labels, whatever their case: two labels both
-- called "Confidential" would be one too many to choose between. A removed
-- label's name is free again.
CREATE UNIQUE INDEX IF NOT EXISTS sensitivity_labels_live_name_idx
    ON sensitivity_labels (lower(name)) WHERE deleted_at IS NULL;

CREATE TABLE IF NOT EXISTS item_labels (
    "id"        uuid PRIMARY KEY NOT NULL,
    "kind"      varchar(20) NOT NULL,
    "item_uuid" uuid NOT NULL,
    "label_id"  uuid NOT NULL REFERENCES sensitivity_labels(id),
    "set_by"    uuid NOT NULL,
    "set_at"    timestamptz NOT NULL DEFAULT NOW(),
    "unset_at"  timestamptz,
    "unset_by"  uuid,
    CONSTRAINT item_labels_unset_check CHECK ((unset_at IS NULL) = (unset_by IS NULL))
);

-- Every row of a label, unset ones included, so the foreign key above is
-- indexed whole (tests/integration/fk_indexes_test.go); it also lists the
-- items a label is on when an admin gives it another rule.
CREATE INDEX IF NOT EXISTS item_labels_label_idx
    ON item_labels (label_id);

-- One live label per item. Every enforcement point reads it (kind, item_uuid),
-- and a change to an item's label unsets its live row before it adds one.
CREATE UNIQUE INDEX IF NOT EXISTS item_labels_live_idx
    ON item_labels (kind, item_uuid) WHERE unset_at IS NULL;
