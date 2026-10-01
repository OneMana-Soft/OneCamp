-- Reverse migration 95: restore the install counter and reviews table.

ALTER TABLE marketplace_templates
    ADD COLUMN IF NOT EXISTS "install_count" integer NOT NULL DEFAULT 0;

CREATE TABLE IF NOT EXISTS marketplace_reviews (
    "id"          uuid PRIMARY KEY DEFAULT uuid_generate_v4(),
    "template_id" uuid NOT NULL REFERENCES marketplace_templates(id) ON DELETE CASCADE,
    "created_by"  uuid NOT NULL REFERENCES users(id) ON DELETE CASCADE,
    "rating"      smallint NOT NULL CHECK (rating BETWEEN 1 AND 5),
    "comment"     text,
    "created_at"  TIMESTAMP WITH TIME ZONE DEFAULT NOW(),
    "updated_at"  TIMESTAMP WITH TIME ZONE DEFAULT NOW(),
    UNIQUE (template_id, created_by)
);

CREATE INDEX IF NOT EXISTS idx_marketplace_reviews_template
    ON marketplace_reviews (template_id);
