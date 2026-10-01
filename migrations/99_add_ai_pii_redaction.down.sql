ALTER TABLE ai_settings
    DROP COLUMN IF EXISTS pii_redaction_enabled,
    DROP COLUMN IF EXISTS pii_custom_patterns;
