-- Migration 70: admin-configurable AI reasoning ("thinking") mode.
--
-- "Thinking" models (Ollama gemma4, deepseek-r1, qwen3, …) generate an
-- internal chain-of-thought before their final answer. That reasoning often
-- improves answer quality on hard questions, but it is expensive to generate
-- on CPU-only hosts (5-10x slower) and OneCamp discards the trace anyway.
--
-- Whether the extra latency is worth the quality is a model- AND hardware-
-- dependent tradeoff with no universally-correct value — exactly like the
-- context window (migration 69). So it belongs with the admin-managed
-- settings rather than a boot-time env var: it hot-reloads with the model
-- selection and an operator on a GPU box can opt into higher-quality
-- reasoning while a CPU box keeps the fast default.
--
-- false (default) == thinking OFF (fast). Non-thinking models ignore it.
-- Existing installs are unchanged. Idempotent.

ALTER TABLE ai_settings
    ADD COLUMN IF NOT EXISTS "reasoning_enabled" boolean NOT NULL DEFAULT false;
