-- Per-key OpenAI override: force service_tier=priority on OpenAI gateway requests.
ALTER TABLE api_keys
  ADD COLUMN IF NOT EXISTS openai_force_priority_tier BOOLEAN NOT NULL DEFAULT FALSE;
