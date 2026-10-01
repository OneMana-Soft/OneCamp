-- Migration 148: opt-in meeting notes document.
--
-- The recap is posted as a message, which is where a conversation belongs and
-- not where notes do. A message scrolls away, cannot be edited by the people who
-- were in the room, and cannot hold the full transcript without burying the
-- channel. Notes want a document.
--
-- OPT-IN, and default false deliberately. Turning this on creates a document
-- per call in a workspace that did not ask for one, and documents are things
-- people own, organise and delete. An existing install should not wake up to a
-- new document for every meeting it has.
ALTER TABLE ai_settings
    ADD COLUMN IF NOT EXISTS "meeting_notes_doc_enabled" boolean NOT NULL DEFAULT false;
