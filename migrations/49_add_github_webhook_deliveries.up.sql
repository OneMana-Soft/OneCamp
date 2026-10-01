CREATE TABLE IF NOT EXISTS github_webhook_deliveries (
    id uuid PRIMARY KEY DEFAULT uuid_generate_v4(),
    delivery_id varchar(255) NOT NULL UNIQUE,
    event_type varchar(100) NOT NULL,
    received_at TIMESTAMPTZ NOT NULL DEFAULT NOW()
);

CREATE INDEX IF NOT EXISTS idx_github_webhook_deliveries_delivery_id ON github_webhook_deliveries(delivery_id);
CREATE INDEX IF NOT EXISTS idx_github_webhook_deliveries_received_at ON github_webhook_deliveries(received_at);
