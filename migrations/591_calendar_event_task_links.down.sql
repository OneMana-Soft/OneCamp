-- Back to docs and boards alone (575's check). A live task link is taken off
-- first, as unlinking takes one off (deleted_at stamped; deleted_by left
-- empty: no one removed it), so a build without tasks never shows one, and
-- no row is deleted. The old check comes back NOT VALID: it refuses every
-- new task row and keeps the marked ones as they are: after up again,
-- linking one of those tasks again brings its own row back.

UPDATE calendar_event_files SET deleted_at = NOW() WHERE ref_kind = 'task' AND deleted_at IS NULL;
ALTER TABLE calendar_event_files DROP CONSTRAINT IF EXISTS calendar_event_files_kind_check;
ALTER TABLE calendar_event_files
    ADD CONSTRAINT calendar_event_files_kind_check CHECK (ref_kind IN ('doc', 'board')) NOT VALID;
