-- Migration 133: code-PR coding wall limit as an admin setting.
--
-- How long ONE coding run may work before the sandbox stops it and the agent
-- hands back whatever it finished (partial work is still pushed to a branch).
-- Every other code-PR knob is already an admin-managed column on this singleton
-- row — budgets, model, draft-on-red, egress, unlinked-repo policy — but the
-- wall limit was env-only (AI_CODE_PR_WALL_MINUTES), so changing how long a run
-- may take needed a redeploy. This column makes it a first-class setting.
--
-- Default 30 minutes: a deliberate raise from the previous built-in 15, which
-- was too low for real repositories (GitHub's Copilot coding agent allows up to
-- 59 minutes and OpenAI's Codex cloud tasks land around 30). Peer agents having
-- settled near half an hour is the evidence 15 was cutting healthy runs short.
--
-- 0 means "use the built-in default" (the same convention as
-- context_window_tokens and code_analysis_max_files): the server then falls back
-- to the AI_CODE_PR_WALL_MINUTES env override, if set, and otherwise to its
-- compiled-in default. Any other value is validated/clamped server-side to
-- 2..60 minutes — the bounds business/CodePR already enforces — so a typo can
-- neither starve a run nor pin a worker for a day. The env var remains a
-- boot-time escape hatch for automated deployments that never open the UI.
-- Idempotent.

ALTER TABLE ai_settings
    ADD COLUMN IF NOT EXISTS code_pr_wall_minutes integer NOT NULL DEFAULT 30;
