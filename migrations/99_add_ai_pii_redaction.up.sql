-- AI data residency, part 2: PII redaction before cloud egress.
--
-- When pii_redaction_enabled is on, outbound prompts to a NON-LOCAL (cloud)
-- model are scrubbed of detected PII before they leave the customer's
-- infrastructure. Local models receive unmodified content (data never leaves
-- the box, so redaction there only degrades answers). Default OFF preserves
-- existing behavior for current installs.
--
-- pii_custom_patterns holds admin-defined regexes, one per line (newline
-- delimited). Invalid patterns are skipped at compile time and never fail a
-- request.
ALTER TABLE ai_settings
    ADD COLUMN IF NOT EXISTS pii_redaction_enabled boolean NOT NULL DEFAULT false,
    ADD COLUMN IF NOT EXISTS pii_custom_patterns text NOT NULL DEFAULT '';
