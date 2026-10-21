-- Docs C5-2, undone exactly: doc verdicts and members' replies go with the
-- columns that hold them; the task reviews take their old name back.
DELETE FROM client_reviews WHERE resource_kind = 'doc';
DROP INDEX IF EXISTS client_reviews_doc;
ALTER TABLE client_reviews DROP CONSTRAINT IF EXISTS client_reviews_resource;
ALTER TABLE client_reviews ALTER COLUMN project_uuid SET NOT NULL;
ALTER TABLE client_reviews ALTER COLUMN task_uuid SET NOT NULL;
ALTER TABLE client_reviews DROP COLUMN IF EXISTS doc_uuid;
ALTER TABLE client_reviews DROP COLUMN IF EXISTS resource_kind;
DO $$
BEGIN
    IF to_regclass('public.client_reviews') IS NOT NULL AND to_regclass('public.task_client_reviews') IS NULL THEN
        ALTER TABLE client_reviews RENAME TO task_client_reviews;
    END IF;
END $$;

DELETE FROM guest_comments WHERE author_kind = 'member';
DROP INDEX IF EXISTS idx_guest_comments_parent;
ALTER TABLE guest_comments DROP CONSTRAINT IF EXISTS guest_comments_author_kind;
ALTER TABLE guest_comments DROP COLUMN IF EXISTS author_user_uuid;
ALTER TABLE guest_comments DROP COLUMN IF EXISTS author_kind;
ALTER TABLE guest_comments DROP COLUMN IF EXISTS parent_id;
