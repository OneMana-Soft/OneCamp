-- Migration 82: admin-configurable budget for the code-aware bug agent.
--
-- The code analysis agent retrieves repo files via the GitHub API and feeds
-- them to the model. How many files it pulls per analysis is the main
-- cost/latency lever (more files = more tokens = slower + pricier on paid
-- endpoints, but better grounding). This makes it admin-tunable instead of a
-- hardcoded constant, mirroring context_window_tokens / rate_limit_per_min.
--
-- 0 means "use the built-in default". The business layer clamps to a safe
-- range so a misconfigured value can't blow the context window or the bill.
-- Idempotent: safe to re-run.

ALTER TABLE ai_settings
    ADD COLUMN IF NOT EXISTS "code_analysis_max_files" integer NOT NULL DEFAULT 0;
