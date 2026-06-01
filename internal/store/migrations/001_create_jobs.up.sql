CREATE EXTENSION IF NOT EXISTS "pgcrypto";

CREATE TABLE jobs (
  id          UUID PRIMARY KEY DEFAULT gen_random_uuid(),
  device      TEXT NOT NULL,
  disc_label  TEXT,
  title       TEXT,
  year        INT,
  status      TEXT NOT NULL DEFAULT 'pending',
  pattern     TEXT,
  created_at  TIMESTAMPTZ NOT NULL DEFAULT now(),
  updated_at  TIMESTAMPTZ NOT NULL DEFAULT now()
);

CREATE TABLE job_events (
  id         BIGSERIAL PRIMARY KEY,
  job_id     UUID NOT NULL REFERENCES jobs(id) ON DELETE CASCADE,
  stage      TEXT NOT NULL,
  message    TEXT NOT NULL,
  data       JSONB,
  created_at TIMESTAMPTZ NOT NULL DEFAULT now()
);
