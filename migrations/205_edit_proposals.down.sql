-- Migration 205 down: edit proposals go. Accepted ones are already in their
-- docs; open ones are lost with the table.
DROP TABLE IF EXISTS edit_proposals;
