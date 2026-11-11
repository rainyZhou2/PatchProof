CREATE TABLE IF NOT EXISTS action_cache (
  tenant_id uuid NOT NULL,
  action_key text NOT NULL,
  step_kind text NOT NULL,
  engine text NOT NULL,
  engine_version text NOT NULL,
  result jsonb NOT NULL,
  created_at timestamptz NOT NULL DEFAULT now(),
  expires_at timestamptz NOT NULL,
  PRIMARY KEY (tenant_id, action_key)
);

CREATE INDEX IF NOT EXISTS idx_action_cache_expiry ON action_cache (expires_at);
