-- Migration 214: an approval is bound to the call it approved.
--
-- A proposal held its tool, its arguments and who must approve it. Two more
-- things decide what an approved call does, and either could change while it
-- waited for a person:
--
--   asked_by      who the agent's run was for, when that was someone other than
--                 its sponsor. That run reached only what both of them could, and
--                 the approved call has to as well: it runs for the same person,
--                 and is refused if they could no longer ask for it. NULL for a
--                 run that acted for its sponsor alone, and for every proposal
--                 that isn't an agent's.
--
--   tool_binding  a fingerprint of where the call's tools send it: for a tool
--                 from an MCP server, that server and its address. Re-pointing
--                 the server, or registering the tool's name under another one,
--                 changes it, and the approved call is not sent. NULL when every
--                 tool is OneCamp's own.
--
-- Deleting the person deletes what was proposed for them, rather than leaving a
-- call bound to nobody, which would run for the sponsor alone.
ALTER TABLE ai_pending_actions
    ADD COLUMN IF NOT EXISTS "asked_by"     uuid REFERENCES users(id) ON DELETE CASCADE,
    ADD COLUMN IF NOT EXISTS "tool_binding" varchar;
