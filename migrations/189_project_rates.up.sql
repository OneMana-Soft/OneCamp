-- Migration 189: what a project's time is billed at. One currency per project,
-- a rate for anyone without their own, and a rate per person who has one.
-- Rates are in hundredths of the currency's unit (cents, paise) per hour, so
-- money adds up exactly. A project's admins set them; only they see money.
-- Changing a rate re-prices time already logged: rates carry no dates.
CREATE TABLE IF NOT EXISTS project_billing (
    "project_uuid"       uuid PRIMARY KEY,
    "currency"           char(3) NOT NULL CHECK (currency ~ '^[A-Z]{3}$'),
    "default_rate_cents" bigint NOT NULL DEFAULT 0 CHECK (default_rate_cents BETWEEN 0 AND 100000000),
    "updated_by"         uuid NOT NULL,
    "updated_at"         TIMESTAMP WITH TIME ZONE NOT NULL DEFAULT NOW()
);

CREATE TABLE IF NOT EXISTS project_person_rates (
    "project_uuid" uuid NOT NULL REFERENCES project_billing (project_uuid) ON DELETE CASCADE,
    "user_uuid"    uuid NOT NULL,
    "rate_cents"   bigint NOT NULL CHECK (rate_cents BETWEEN 0 AND 100000000),
    PRIMARY KEY (project_uuid, user_uuid)
);
