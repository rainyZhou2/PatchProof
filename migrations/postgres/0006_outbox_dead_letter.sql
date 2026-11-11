ALTER TABLE outbox_events ADD COLUMN IF NOT EXISTS dead_lettered_at timestamptz;
CREATE INDEX IF NOT EXISTS outbox_pending_idx ON outbox_events (created_at)
    WHERE delivered_at IS NULL AND dead_lettered_at IS NULL;
