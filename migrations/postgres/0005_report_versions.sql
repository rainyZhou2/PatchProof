-- Reports are immutable versions keyed by (run_id, report_id). Re-aggregation
-- creates a new row instead of overwriting the previous signed report.
UPDATE reports SET report_id = gen_random_uuid() WHERE report_id IS NULL;
ALTER TABLE reports ALTER COLUMN report_id SET NOT NULL;
ALTER TABLE reports DROP CONSTRAINT IF EXISTS reports_pkey;
ALTER TABLE reports ADD CONSTRAINT reports_pkey PRIMARY KEY (run_id, report_id);
CREATE INDEX IF NOT EXISTS reports_run_created_idx ON reports (tenant_id, run_id, created_at DESC);
