-- What the merge of a person's stray graph nodes changed (business/User
-- strayMerge.go), so it can be put back: each stray's own predicates as the
-- graph held them ('node'), and each edge into a stray ('in_edge') or out of
-- it ('out_edge'), with whether the merge added the person's copy. A batch's
-- rows are written before its graph change ('pending') and marked once it has
-- landed ('applied'); a restore puts an applied run back and marks it
-- 'restored'. Rows are small and the merge runs once per install, so they're
-- kept.
CREATE TABLE IF NOT EXISTS person_merge_backups (
    "id"         bigserial PRIMARY KEY,
    "run_id"     uuid NOT NULL,
    "batch_id"   uuid NOT NULL,
    "state"      varchar NOT NULL DEFAULT 'pending' CHECK (state IN ('pending', 'applied', 'restored')),
    "kind"       varchar NOT NULL CHECK (kind IN ('node', 'in_edge', 'out_edge')),
    "user_uuid"  uuid NOT NULL,
    "person_uid" varchar NOT NULL,
    "stray_uid"  varchar NOT NULL,
    "predicate"  varchar,
    "other_uid"  varchar,
    "list"       boolean NOT NULL DEFAULT false,
    "added"      boolean NOT NULL DEFAULT false,
    "facets"     jsonb,
    "node"       jsonb,
    "created_at" TIMESTAMP WITH TIME ZONE NOT NULL DEFAULT NOW()
);

CREATE INDEX IF NOT EXISTS idx_person_merge_backups_run_batch ON person_merge_backups (run_id, batch_id);
