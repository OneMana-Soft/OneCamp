-- Migration 84: optional vision model for image/GIF analysis.
--
-- The chat model and the vision model are separate selections (mirroring how
-- the embedding model is already separate). The best chat model is often not
-- multimodal, vision tokens are pricier, and many self-hosted setups run a
-- text-only model. So vision is an OPTIONAL extra capability: when no vision
-- model is selected, image analysis is cleanly disabled and nothing else
-- changes. Text documents do not need this - their text is extracted and read
-- by the chat model.
ALTER TABLE ai_settings ADD COLUMN IF NOT EXISTS vision_provider_id uuid;
ALTER TABLE ai_settings ADD COLUMN IF NOT EXISTS vision_model varchar NOT NULL DEFAULT '';
