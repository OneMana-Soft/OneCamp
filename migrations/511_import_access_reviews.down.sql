ALTER TABLE import_member_reviews DROP COLUMN IF EXISTS "reason";
DROP INDEX IF EXISTS idx_import_id_map_parent;
DROP TABLE IF EXISTS import_access_changes;
DROP TABLE IF EXISTS import_access_reviews;
