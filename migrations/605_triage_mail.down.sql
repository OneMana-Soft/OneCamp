-- 605 down: the six sources again, and no mailboxes. An item that came by
-- mail is a stranger's submission, as a form's is: it is re-labelled 'form'
-- first, so the check holds for every row and an UPDATE of such an item
-- still passes it.
UPDATE triage_items SET source = 'form' WHERE source = 'mail';
ALTER TABLE triage_items
    DROP CONSTRAINT IF EXISTS triage_items_source_check,
    ADD CONSTRAINT triage_items_source_check CHECK (source IN ('form', 'api', 'agent', 'github', 'chat', 'import'));
DROP TABLE IF EXISTS triage_mail_seen;
DROP TABLE IF EXISTS triage_mailboxes;
