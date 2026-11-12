-- Migration 577 down: nothing is undone. The cursors it cleared were
-- Google's to give, and the sweep after it has read each calendar whole and
-- kept a new one; a calendar without one is only read whole again. A
-- statement, so there is one to run.
SELECT 1;
