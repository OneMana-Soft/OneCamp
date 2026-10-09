-- Migration 204 down: nothing is undone. Which invitations 204 marked joined
-- isn't recorded, and reopening them would hand back the links it closed, to
-- accounts that already exist. A statement, so there is one to run.
SELECT 1;
