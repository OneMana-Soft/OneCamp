CREATE TABLE IF NOT EXISTS attachments(
    "id" uuid PRIMARY KEY NOT NULL DEFAULT uuid_generate_v4(),
    "obj_key" varchar NOT NULL,
    "src_value" varchar NOT NULL,
    "src_key" varchar NOT NULL,
    "created_by" uuid REFERENCES users(id),
    "created_at" TIMESTAMP WITH TIME ZONE DEFAULT NOW(),
    "deleted_at" TIMESTAMP WITH TIME ZONE ,
    CONSTRAINT unique_id_and_obj_key UNIQUE ("id", "obj_key")
);

CREATE INDEX IF NOT EXISTS idx_chats_grp_id_active 
ON chats(grp_id);

CREATE INDEX IF NOT EXISTS idx_attachments_grpchat_active 
ON attachments(src_key, src_value)