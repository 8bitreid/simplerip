ALTER TABLE jobs ADD COLUMN finished_at TIMESTAMPTZ;

UPDATE jobs
SET finished_at = updated_at
WHERE status IN ('done', 'error', 'cancelled');
