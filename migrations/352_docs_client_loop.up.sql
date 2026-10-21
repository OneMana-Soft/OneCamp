-- Docs C5-2: the client loop on docs.
--
-- Guest comments take replies. A member's reply in a guest's thread ("Visible
-- to guest") is a row of the same table: author_kind 'member', its author's
-- user id kept for the workspace (never shown to the guest), its parent the
-- guest's comment, and the parent's grant (so revoking the link takes the
-- conversation with it, as before).
ALTER TABLE guest_comments ADD COLUMN IF NOT EXISTS parent_id uuid REFERENCES guest_comments(id) ON DELETE CASCADE;
ALTER TABLE guest_comments ADD COLUMN IF NOT EXISTS author_kind varchar(8) NOT NULL DEFAULT 'guest';
ALTER TABLE guest_comments ADD COLUMN IF NOT EXISTS author_user_uuid uuid;
DO $$
BEGIN
    IF NOT EXISTS (SELECT 1 FROM pg_constraint WHERE conname = 'guest_comments_author_kind') THEN
        ALTER TABLE guest_comments ADD CONSTRAINT guest_comments_author_kind
            CHECK (author_kind IN ('guest', 'member') AND (author_kind = 'guest' OR author_user_uuid IS NOT NULL));
    END IF;
END $$;
CREATE INDEX IF NOT EXISTS idx_guest_comments_parent ON guest_comments (parent_id) WHERE parent_id IS NOT NULL;

-- Client verdicts: the task reviews (migration 183) generalise to docs, in
-- place. A rename and new columns with defaults: no copy, no rewrite, so a
-- live install migrates at once. Existing rows stay task reviews.
DO $$
BEGIN
    IF to_regclass('public.task_client_reviews') IS NOT NULL AND to_regclass('public.client_reviews') IS NULL THEN
        ALTER TABLE task_client_reviews RENAME TO client_reviews;
    END IF;
END $$;
ALTER TABLE client_reviews ADD COLUMN IF NOT EXISTS resource_kind varchar(8) NOT NULL DEFAULT 'task';
ALTER TABLE client_reviews ADD COLUMN IF NOT EXISTS doc_uuid varchar;
ALTER TABLE client_reviews ALTER COLUMN task_uuid DROP NOT NULL;
ALTER TABLE client_reviews ALTER COLUMN project_uuid DROP NOT NULL;
DO $$
BEGIN
    IF NOT EXISTS (SELECT 1 FROM pg_constraint WHERE conname = 'client_reviews_resource') THEN
        ALTER TABLE client_reviews ADD CONSTRAINT client_reviews_resource CHECK (
            (resource_kind = 'task' AND task_uuid IS NOT NULL AND project_uuid IS NOT NULL AND doc_uuid IS NULL)
            OR (resource_kind = 'doc' AND doc_uuid IS NOT NULL AND task_uuid IS NULL AND project_uuid IS NULL));
    END IF;
END $$;
CREATE INDEX IF NOT EXISTS client_reviews_doc ON client_reviews (doc_uuid, created_at DESC) WHERE doc_uuid IS NOT NULL;
