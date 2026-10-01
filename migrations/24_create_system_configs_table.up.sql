CREATE TABLE IF NOT EXISTS system_configs (
    "id" uuid PRIMARY KEY NOT NULL DEFAULT uuid_generate_v4(),
    "key" varchar NOT NULL UNIQUE,
    "value" text NOT NULL,
    "updated_at" TIMESTAMP WITH TIME ZONE DEFAULT NOW()
);

-- Seed defaults
INSERT INTO system_configs ("key", "value") VALUES
    ('sender_email', 'noreply@onemana.dev'),
    ('invitation_email_subject', 'You''re invited to OneCamp!'),
    ('invitation_email_template', '<h2>Welcome to OneCamp!</h2><p>You''ve been invited to join. Click the link below to set up your account:</p><p><a href="{{signup_link}}">Accept Invitation</a></p><p>This link expires in 7 days.</p>')
ON CONFLICT ("key") DO NOTHING;
