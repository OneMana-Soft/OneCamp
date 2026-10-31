-- Files a visitor sent stay as the project's attachments; those still waiting
-- lose their row here, and their objects stay in the bucket.
DROP TABLE IF EXISTS form_uploads;
ALTER TABLE form_submissions DROP COLUMN IF EXISTS row_id;
ALTER TABLE project_forms DROP COLUMN IF EXISTS target_table;
