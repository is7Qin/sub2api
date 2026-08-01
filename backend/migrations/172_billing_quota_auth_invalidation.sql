-- Add a durable source identity for quota-enforcement invalidations.
-- Existing trigger-generated rows keep NULL so their deduplication behavior is unchanged.
ALTER TABLE auth_cache_invalidation_outbox
    ADD COLUMN IF NOT EXISTS source_key TEXT;

CREATE UNIQUE INDEX IF NOT EXISTS idx_auth_cache_invalidation_outbox_source_key
    ON auth_cache_invalidation_outbox (source_key)
    WHERE source_key IS NOT NULL;

-- Billing also stages quota exhaustion explicitly with the physical attempt source key.
-- Keep generic trigger invalidations because non-billing updates can cross the quota boundary.
CREATE OR REPLACE FUNCTION enqueue_api_key_auth_cache_invalidation()
RETURNS TRIGGER LANGUAGE plpgsql AS $$
BEGIN
    IF TG_OP = 'DELETE' THEN
        PERFORM enqueue_auth_cache_invalidation(OLD.key);
        RETURN OLD;
    END IF;
    IF OLD.key IS DISTINCT FROM NEW.key
       OR OLD.status IS DISTINCT FROM NEW.status
       OR OLD.deleted_at IS DISTINCT FROM NEW.deleted_at
       OR OLD.user_id IS DISTINCT FROM NEW.user_id
       OR OLD.group_id IS DISTINCT FROM NEW.group_id
       OR OLD.ip_whitelist IS DISTINCT FROM NEW.ip_whitelist
       OR OLD.ip_blacklist IS DISTINCT FROM NEW.ip_blacklist
       OR OLD.concurrency IS DISTINCT FROM NEW.concurrency
       OR OLD.quota IS DISTINCT FROM NEW.quota
       OR OLD.expires_at IS DISTINCT FROM NEW.expires_at
       OR OLD.rate_limit_5h IS DISTINCT FROM NEW.rate_limit_5h
       OR OLD.rate_limit_1d IS DISTINCT FROM NEW.rate_limit_1d
       OR OLD.rate_limit_7d IS DISTINCT FROM NEW.rate_limit_7d
       OR OLD.openai_force_priority_tier IS DISTINCT FROM NEW.openai_force_priority_tier THEN
        PERFORM enqueue_auth_cache_invalidation(OLD.key);
        IF NEW.deleted_at IS NULL AND NEW.key IS DISTINCT FROM OLD.key THEN
            PERFORM enqueue_auth_cache_invalidation(NEW.key);
        END IF;
    END IF;
    RETURN NEW;
END;
$$;
