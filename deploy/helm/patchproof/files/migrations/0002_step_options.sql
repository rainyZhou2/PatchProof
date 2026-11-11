-- Add planner/worker step options to the steps table.

ALTER TABLE steps ADD COLUMN IF NOT EXISTS options jsonb NOT NULL DEFAULT '{}'::jsonb;
