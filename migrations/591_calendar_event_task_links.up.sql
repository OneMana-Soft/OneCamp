-- A task linked to an event (EVLINK head A, gcal-15's recorded gap: "a doc or
-- task links to an event only as a file or a notes link"). An event carries a
-- task by reference as it carries a doc or a board (575: no title kept, soft
-- removal, one live row per event and thing), so the kinds' check takes
-- 'task'. Nothing else changes: no new table, no new index.
--
-- Who sees one (business/Calendar eventFiles.go): a linked doc or task shows
-- on an event only to someone who may see both; whoever may change the event
-- links one they may open; unlinking marks the row, and linking again brings
-- it back.

ALTER TABLE calendar_event_files DROP CONSTRAINT IF EXISTS calendar_event_files_kind_check;
ALTER TABLE calendar_event_files
    ADD CONSTRAINT calendar_event_files_kind_check CHECK (ref_kind IN ('doc', 'board', 'task'));
