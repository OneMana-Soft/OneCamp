-- Per-model token limits on the admin allowlist.
--
-- OneCamp is model-agnostic. An admin authorizes a SET of models (migration 81), and a
-- member, a channel (migration 121) or an agent then picks one from it. Until now there
-- was exactly ONE context window in the system — ai_settings.context_window_tokens
-- (migration 69) — and every token budget read it regardless of which model answered.
--
-- That is wrong in both directions, and silently:
--
--   * Pin a channel to a 128k-window model while the workspace says 8192 and the prompt
--     budget discards context that would have fitted. On Ollama it is worse than wasteful:
--     num_ctx is applied at client construction, so the model is genuinely RUN small.
--   * Pin an 8k model while the workspace says 128k and the budget builds a prompt the
--     model cannot accept. Cloud providers answer that with HTTP 400.
--
-- A context window is a property OF A MODEL, so it belongs on the row where the admin
-- names the model, beside its label and its enabled flag — not in a workspace singleton,
-- and not in a table of model names compiled into the binary. A compiled-in catalogue
-- goes stale the week a provider ships a new model, and this project cannot enumerate
-- them anyway: the openai_compatible provider kind accepts ANY endpoint, including a
-- router that serves different models under one name, and Ollama runs whatever the
-- operator pulled.
--
-- 0 MEANS INHERIT, and is the default, so this migration changes no behaviour for any
-- existing workspace: every model keeps resolving to ai_settings.context_window_tokens
-- exactly as it does today. The columns start mattering when an admin fills one in, which
-- is the point — they are how an operator writes down what they know about a model they
-- chose to allow. Nothing here guesses on their behalf.
ALTER TABLE ai_authorized_models
    -- Total tokens this model accepts (prompt + completion), as the provider defines it.
    -- 0 = inherit the workspace value. Range-checked rather than free: a nonsense window
    -- is worse than an absent one, because an absent one falls back to something that
    -- works. The ceiling is deliberately generous — published windows already reach the
    -- millions — and the floor matches the application's own minimum usable window.
    ADD COLUMN IF NOT EXISTS context_window_tokens integer NOT NULL DEFAULT 0
        CONSTRAINT ai_authorized_models_context_window_sane
        CHECK (context_window_tokens = 0 OR context_window_tokens BETWEEN 2048 AND 20000000),
    -- Most tokens this model will GENERATE in one response, which is a separate limit
    -- from the window and frequently much smaller: a model can read a million tokens and
    -- still refuse to write more than a few tens of thousands. 0 = inherit, meaning the
    -- caller's own per-call reservation stands.
    ADD COLUMN IF NOT EXISTS max_output_tokens integer NOT NULL DEFAULT 0
        CONSTRAINT ai_authorized_models_max_output_sane
        CHECK (max_output_tokens = 0 OR max_output_tokens BETWEEN 256 AND 1000000);

COMMENT ON COLUMN ai_authorized_models.context_window_tokens IS
    'Total tokens this model accepts (prompt+completion). 0 = inherit ai_settings.context_window_tokens.';
COMMENT ON COLUMN ai_authorized_models.max_output_tokens IS
    'Max tokens this model generates per response. 0 = inherit the caller''s own reservation.';
