-- no-transaction
-- Foreign key user_ai_model_preference.authorized_model_id ->
-- ai_authorized_models (ON DELETE SET NULL) had no index, so deleting a
-- ai_authorized_models row scanned all of user_ai_model_preference.
-- CONCURRENTLY: built without blocking writes; one statement per file, outside
-- a transaction (sqlx reads the first line). A build that fails leaves an
-- INVALID index: drop it and run the migration again.
CREATE INDEX CONCURRENTLY IF NOT EXISTS idx_fk_user_ai_model_preference_authorized_model_id ON user_ai_model_preference (authorized_model_id);
