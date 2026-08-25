-- Per-key concurrent request limit; zero keeps existing unlimited behavior.
ALTER TABLE api_keys
  ADD COLUMN IF NOT EXISTS concurrency INTEGER NOT NULL DEFAULT 0 CHECK (concurrency >= 0);
