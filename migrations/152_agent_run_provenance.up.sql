-- Migration 152: record what an agent was actually told, on the run itself.
--
-- A run stored its transcript, its step count and its tokens, and nothing about
-- the instructions that produced them. Skills are shared and editable, so
-- editing one silently changed the meaning of every past run that used it: the
-- ledger still said the agent did this, and no longer said what it was asked.
--
-- Three columns, and the choice of the third is the point.
--
--   model         the model that actually read the prompt, after the agent,
--                 channel and workspace precedence has been resolved. The
--                 agent's stored preference is not this; it is one input to it.
--
--   prompt_sha256 a fingerprint of the fully composed system prompt.
--
--   skills_used   which skills were in that prompt, each with a fingerprint of
--                 the instruction text as it stood at the time.
--
-- FINGERPRINTS RATHER THAN COPIES, for two reasons. Storing the prompt again
-- would double the largest table for a record almost nobody reads. And a
-- fingerprint is not content, so it survives the retention sweep in 151 that
-- clears the transcript: after redaction the run can still say which
-- instructions produced it, having lost the ability to show them.
--
-- This does not promise bit-exact replay, which no honest system can promise
-- across providers and routing. It promises something more useful and fully
-- verifiable: proof of what was sent.
ALTER TABLE ai_agent_runs
    ADD COLUMN IF NOT EXISTS "model"         varchar,
    ADD COLUMN IF NOT EXISTS "prompt_sha256" char(64),
    ADD COLUMN IF NOT EXISTS "skills_used"   jsonb NOT NULL DEFAULT '[]'::jsonb;
