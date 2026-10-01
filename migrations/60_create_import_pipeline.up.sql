-- Migration 60: Create the provider-agnostic import pipeline
--
-- This is a squashed migration that replaces the original sequence:
--   60_create_slack_imports
--   61_slack_import_dedup
--   62_create_slack_emoji_map
--   63_generalize_imports
--   65_add_linear_provider
--   66_add_clickup_provider
--
-- Instead of creating Slack-only tables and then renaming/generalising
-- them in later migrations, we build the final schema directly. This
-- eliminates the intermediate renames and ALTERs that existed only
-- because the feature was developed incrementally.
--
-- Tables created here (final names):
--   import_jobs, import_id_map, import_chunks, import_errors
--   import_workspace_id_map, slack_emoji_map
--   import_status_mappings, import_priority_mappings
--   import_oauth_tokens

CREATE EXTENSION IF NOT EXISTS "uuid-ossp";
CREATE EXTENSION IF NOT EXISTS pgcrypto;

-- =====================================================================
-- 1. import_jobs — one job per import run
-- =====================================================================
CREATE TABLE IF NOT EXISTS import_jobs (
    "id"                   uuid PRIMARY KEY DEFAULT uuid_generate_v4(),
    "provider"             varchar NOT NULL DEFAULT 'slack',
    "source_workspace_name" varchar NOT NULL,
    -- Slack: export_zip | corporate_zip | api
    -- Task providers: board_json | backup_xml | backup_csv
    "source"               varchar NOT NULL
        CHECK (source IN ('export_zip','corporate_zip','api','board_json','backup_xml','backup_csv')),
    "raw_object_key"       varchar,
    "status"               varchar NOT NULL DEFAULT 'pending'
        CHECK (status IN ('pending','validating','planned','running','paused','completed','failed','cancelled','rolled_back')),
    "stage"                varchar,
    "started_at"           TIMESTAMP WITH TIME ZONE,
    "completed_at"         TIMESTAMP WITH TIME ZONE,
    "options"              jsonb NOT NULL DEFAULT '{}',
    "plan"                 jsonb,
    "progress"             jsonb NOT NULL DEFAULT '{}',
    "error_message"        text,
    "content_hash"         varchar,
    "triggered_by"         uuid REFERENCES users(id),
    "created_at"           TIMESTAMP WITH TIME ZONE DEFAULT NOW(),
    "updated_at"           TIMESTAMP WITH TIME ZONE DEFAULT NOW()
);

CREATE INDEX IF NOT EXISTS idx_import_jobs_status
    ON import_jobs(status);
CREATE INDEX IF NOT EXISTS idx_import_jobs_created_at
    ON import_jobs(created_at DESC);
CREATE INDEX IF NOT EXISTS idx_import_jobs_content_hash
    ON import_jobs(provider, source_workspace_name, content_hash)
    WHERE content_hash IS NOT NULL;
CREATE INDEX IF NOT EXISTS idx_import_jobs_provider_status
    ON import_jobs(provider, status);

-- Prevent two simultaneous imports for the same (provider, workspace).
CREATE UNIQUE INDEX IF NOT EXISTS uq_import_jobs_active_per_provider_workspace
    ON import_jobs(provider, source_workspace_name)
    WHERE status IN ('validating','planned','running','paused');

-- =====================================================================
-- 2. import_id_map — per-import entity identity map (retry backbone)
-- =====================================================================
CREATE TABLE IF NOT EXISTS import_id_map (
    "import_id"            uuid NOT NULL REFERENCES import_jobs(id) ON DELETE CASCADE,
    "entity_type"          varchar NOT NULL,
    "source_id"            varchar NOT NULL,
    "onecamp_uuid"         uuid NOT NULL,
    "parent_source_id"     varchar,
    "created_by_this_import" boolean NOT NULL DEFAULT true,
    "metadata"             jsonb,
    "created_at"           TIMESTAMP WITH TIME ZONE DEFAULT NOW(),
    PRIMARY KEY (import_id, entity_type, source_id)
);

CREATE INDEX IF NOT EXISTS idx_import_id_map_lookup
    ON import_id_map(import_id, entity_type);
CREATE INDEX IF NOT EXISTS idx_import_id_map_onecamp
    ON import_id_map(onecamp_uuid);

-- =====================================================================
-- 3. import_chunks — durable, resumable work queue
-- =====================================================================
CREATE TABLE IF NOT EXISTS import_chunks (
    "id"               uuid PRIMARY KEY DEFAULT uuid_generate_v4(),
    "import_id"        uuid NOT NULL REFERENCES import_jobs(id) ON DELETE CASCADE,
    "chunk_type"       varchar NOT NULL,
    "channel_slack_id" varchar,
    "object_key"       varchar,
    "status"           varchar NOT NULL DEFAULT 'pending'
        CHECK (status IN ('pending','in_progress','done','failed','skipped','cancelled')),
    "attempts"         int NOT NULL DEFAULT 0,
    "max_attempts"     int NOT NULL DEFAULT 5,
    "items_total"      int,
    "items_done"       int NOT NULL DEFAULT 0,
    "last_cursor"      varchar,
    "claimed_by"       varchar,
    "claimed_at"       TIMESTAMP WITH TIME ZONE,
    "error"            text,
    "created_at"       TIMESTAMP WITH TIME ZONE DEFAULT NOW(),
    "updated_at"       TIMESTAMP WITH TIME ZONE DEFAULT NOW()
);

CREATE INDEX IF NOT EXISTS idx_import_chunks_runnable
    ON import_chunks(import_id, chunk_type, created_at)
    WHERE status IN ('pending','failed');
CREATE INDEX IF NOT EXISTS idx_import_chunks_stuck
    ON import_chunks(claimed_at)
    WHERE status = 'in_progress';
CREATE INDEX IF NOT EXISTS idx_import_chunks_import_status
    ON import_chunks(import_id, status);

-- Idempotency for re-planning: the same logical chunk can't be duplicated.
CREATE UNIQUE INDEX IF NOT EXISTS uq_import_chunks_logical
    ON import_chunks(
        import_id,
        chunk_type,
        COALESCE(channel_slack_id, ''),
        COALESCE(object_key, '')
    );

-- =====================================================================
-- 4. import_errors — per-item error log surfaced to the admin UI
-- =====================================================================
CREATE TABLE IF NOT EXISTS import_errors (
    "id"          uuid PRIMARY KEY DEFAULT uuid_generate_v4(),
    "import_id"   uuid NOT NULL REFERENCES import_jobs(id) ON DELETE CASCADE,
    "chunk_id"    uuid REFERENCES import_chunks(id) ON DELETE SET NULL,
    "entity_type" varchar,
    "source_id"   varchar,
    "severity"    varchar NOT NULL CHECK (severity IN ('warning','error','fatal')),
    "code"        varchar,
    "message"     text,
    "context"     jsonb,
    "created_at"  TIMESTAMP WITH TIME ZONE DEFAULT NOW()
);

CREATE INDEX IF NOT EXISTS idx_import_errors_lookup
    ON import_errors(import_id, severity, created_at DESC);

-- =====================================================================
-- 5. import_workspace_id_map — cross-import workspace-level identity map
-- =====================================================================
CREATE TABLE IF NOT EXISTS import_workspace_id_map (
    "provider"             varchar NOT NULL DEFAULT 'slack',
    "source_workspace_name" varchar NOT NULL,
    "entity_type"          varchar NOT NULL,
    "source_id"            varchar NOT NULL,
    "onecamp_uuid"         uuid NOT NULL,
    "origin_import_id"     uuid REFERENCES import_jobs(id) ON DELETE SET NULL,
    "created_at"           TIMESTAMP WITH TIME ZONE DEFAULT NOW(),
    PRIMARY KEY (provider, source_workspace_name, entity_type, source_id)
);

CREATE INDEX IF NOT EXISTS idx_import_workspace_id_map_uuid
    ON import_workspace_id_map(onecamp_uuid);
CREATE INDEX IF NOT EXISTS idx_import_workspace_id_map_origin
    ON import_workspace_id_map(origin_import_id)
    WHERE origin_import_id IS NOT NULL;

-- =====================================================================
-- 6. slack_emoji_map — shortcode translation for Slack reaction pass
-- =====================================================================
CREATE TABLE IF NOT EXISTS slack_emoji_map (
    "slack_name" varchar PRIMARY KEY,
    "emoji_id"   varchar NOT NULL,
    "created_at" TIMESTAMP WITH TIME ZONE DEFAULT NOW()
);

INSERT INTO slack_emoji_map (slack_name, emoji_id) VALUES
    ('+1', '+1'), ('-1', '-1'), ('100', '100'),
    ('smile', 'smile'), ('smiley', 'smiley'), ('grinning', 'grinning'),
    ('grin', 'grin'), ('joy', 'joy'), ('rofl', 'rofl'),
    ('laughing', 'laughing'), ('sweat_smile', 'sweat_smile'),
    ('blush', 'blush'), ('innocent', 'innocent'),
    ('slightly_smiling_face', 'slightly_smiling_face'),
    ('heart_eyes', 'heart_eyes'), ('star_struck', 'star_struck'),
    ('kissing_heart', 'kissing_heart'), ('relaxed', 'relaxed'),
    ('wink', 'wink'), ('sunglasses', 'sunglasses'),
    ('thinking_face', 'thinking_face'), ('neutral_face', 'neutral_face'),
    ('expressionless', 'expressionless'), ('hushed', 'hushed'),
    ('zipper_mouth_face', 'zipper_mouth_face'),
    ('face_with_raised_eyebrow', 'face_with_raised_eyebrow'),
    ('face_with_monocle', 'face_with_monocle'),
    ('frowning_face', 'frowning_face'), ('disappointed', 'disappointed'),
    ('cry', 'cry'), ('sob', 'sob'), ('weary', 'weary'),
    ('confused', 'confused'), ('worried', 'worried'),
    ('angry', 'angry'), ('rage', 'rage'), ('triumph', 'triumph'),
    ('face_with_symbols_on_mouth', 'face_with_symbols_on_mouth'),
    ('heart', 'heart'), ('orange_heart', 'orange_heart'),
    ('yellow_heart', 'yellow_heart'), ('green_heart', 'green_heart'),
    ('blue_heart', 'blue_heart'), ('purple_heart', 'purple_heart'),
    ('black_heart', 'black_heart'), ('white_heart', 'white_heart'),
    ('broken_heart', 'broken_heart'), ('heart_on_fire', 'heart_on_fire'),
    ('two_hearts', 'two_hearts'), ('sparkling_heart', 'sparkling_heart'),
    ('thumbsup', 'thumbsup'), ('thumbsdown', 'thumbsdown'),
    ('clap', 'clap'), ('raised_hands', 'raised_hands'),
    ('pray', 'pray'), ('ok_hand', 'ok_hand'),
    ('point_up', 'point_up'), ('point_down', 'point_down'),
    ('point_left', 'point_left'), ('point_right', 'point_right'),
    ('wave', 'wave'), ('handshake', 'handshake'),
    ('crossed_fingers', 'crossed_fingers'),
    ('vulcan_salute', 'vulcan_salute'), ('metal', 'metal'),
    ('muscle', 'muscle'),
    ('tada', 'tada'), ('confetti_ball', 'confetti_ball'),
    ('party_popper', 'party_popper'), ('rocket', 'rocket'),
    ('fire', 'fire'), ('sparkles', 'sparkles'), ('star', 'star'),
    ('star2', 'star2'), ('boom', 'boom'), ('zap', 'zap'),
    ('eyes', 'eyes'), ('white_check_mark', 'white_check_mark'),
    ('heavy_check_mark', 'heavy_check_mark'), ('x', 'x'),
    ('warning', 'warning'), ('exclamation', 'exclamation'),
    ('question', 'question'), ('bell', 'bell'), ('lock', 'lock'),
    ('unlock', 'unlock'), ('key', 'key'),
    ('shipit', 'shipit'), ('lgtm', 'lgtm'),
    ('bug', 'bug'), ('hammer_and_wrench', 'hammer_and_wrench'),
    ('robot_face', 'robot_face'), ('computer', 'computer'),
    ('memo', 'memo'), ('book', 'book'), ('clipboard', 'clipboard'),
    ('chart_with_upwards_trend', 'chart_with_upwards_trend'),
    ('chart_with_downwards_trend', 'chart_with_downwards_trend'),
    ('hourglass', 'hourglass'), ('hourglass_flowing_sand', 'hourglass_flowing_sand'),
    ('coffee', 'coffee'), ('beer', 'beer'), ('beers', 'beers'),
    ('pizza', 'pizza'), ('hamburger', 'hamburger'),
    ('cat', 'cat'), ('dog', 'dog'), ('parrot', 'parrot'),
    ('penguin', 'penguin'), ('koala', 'koala'),
    ('sun', 'sun'), ('cloud', 'cloud'), ('rainbow', 'rainbow'),
    ('snowflake', 'snowflake'), ('umbrella', 'umbrella'),
    ('thumbs_up', 'thumbsup'),
    ('thumbs_down', 'thumbsdown'),
    ('+1::skin-tone-2', '+1'), ('+1::skin-tone-3', '+1'),
    ('+1::skin-tone-4', '+1'), ('+1::skin-tone-5', '+1'),
    ('+1::skin-tone-6', '+1')
ON CONFLICT (slack_name) DO NOTHING;

-- =====================================================================
-- 7. import_status_mappings — operator-confirmed status translation
-- =====================================================================
CREATE TABLE IF NOT EXISTS import_status_mappings (
    "import_id"     uuid NOT NULL REFERENCES import_jobs(id) ON DELETE CASCADE,
    "source_status" varchar NOT NULL,
    "target_status" varchar NOT NULL CHECK (target_status IN
        ('todo','inProgress','backlog','inReview','canceled','done')),
    "created_at"    TIMESTAMP WITH TIME ZONE DEFAULT NOW(),
    PRIMARY KEY (import_id, source_status)
);

-- =====================================================================
-- 8. import_priority_mappings — operator-confirmed priority translation
-- =====================================================================
CREATE TABLE IF NOT EXISTS import_priority_mappings (
    "import_id"       uuid NOT NULL REFERENCES import_jobs(id) ON DELETE CASCADE,
    "source_priority" varchar NOT NULL,
    "target_priority" varchar NOT NULL CHECK (target_priority IN ('low','medium','high')),
    "created_at"      TIMESTAMP WITH TIME ZONE DEFAULT NOW(),
    PRIMARY KEY (import_id, source_priority)
);

-- =====================================================================
-- 9. import_oauth_tokens — encrypted token storage for live-API providers
-- =====================================================================
CREATE TABLE IF NOT EXISTS import_oauth_tokens (
    "id"                  uuid PRIMARY KEY DEFAULT uuid_generate_v4(),
    "provider"            varchar NOT NULL
        CHECK (provider IN ('asana','jira','trello','notion','todoist','linear','clickup')),
    "owner_user_id"       uuid NOT NULL REFERENCES users(id) ON DELETE CASCADE,
    "source_account_id"   varchar,
    "source_account_name" varchar,
    "access_token_enc"    bytea NOT NULL,
    "refresh_token_enc"   bytea,
    "scopes"              text,
    "expires_at"          TIMESTAMP WITH TIME ZONE,
    "metadata"            jsonb NOT NULL DEFAULT '{}',
    "created_at"          TIMESTAMP WITH TIME ZONE DEFAULT NOW(),
    "updated_at"          TIMESTAMP WITH TIME ZONE DEFAULT NOW(),
    UNIQUE (provider, owner_user_id)
);

CREATE INDEX IF NOT EXISTS idx_import_oauth_tokens_owner
    ON import_oauth_tokens(owner_user_id);
