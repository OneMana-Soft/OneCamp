-- An agent whose brain is a remote AG-UI endpoint.
--
-- Every current agent framework can be reached the same way: one POST carrying
-- the conversation and the tools on offer, answered with a stream of text and
-- tool calls. The remote publishes no tools of its own; it decides which of
-- ours to call and ends its run to ask us to run it. So a remote agent needs
-- no second loop in this workspace and gets no second set of rules: it is a
-- provider to the same runner, and every call it asks for passes the same
-- allow-list, scope check, autonomy gate, destructive backstop and intent row
-- as a call from any model. What it does on its own machine is outside this
-- record, and the run transcript says so rather than claiming to govern it.
--
-- Three columns, empty on every existing row, so nothing changes for an agent
-- that runs on the workspace's own model. The secret is stored the way every
-- other credential here is: encrypted under AI_CONFIG_KEK, never returned.
ALTER TABLE ai_agents
    ADD COLUMN IF NOT EXISTS "agui_endpoint"              text  NOT NULL DEFAULT '',
    ADD COLUMN IF NOT EXISTS "agui_auth_header"           text  NOT NULL DEFAULT '',
    ADD COLUMN IF NOT EXISTS "agui_auth_secret_encrypted" bytea;
