-- Migration 348 down: the stored comments go, with their reactions (in that
-- order). Threads on docs are untouched: a doc's comments live in the graph.
-- Threads whose comments were stored here are left with none, and a thread
-- with no comments is never shown (business/Thread assemble).
DROP TABLE IF EXISTS thread_comment_reactions;
DROP TABLE IF EXISTS thread_comments;
