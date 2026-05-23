-- 140_redeem_timed_quota.sql
-- Timed quota grants and redeem-code metadata for campaign/lottery rewards.

ALTER TABLE redeem_codes
    ADD COLUMN IF NOT EXISTS metadata JSONB NOT NULL DEFAULT '{}'::jsonb;

CREATE TABLE IF NOT EXISTS user_quota_grants (
    id BIGSERIAL PRIMARY KEY,
    user_id BIGINT NOT NULL REFERENCES users(id) ON DELETE CASCADE,
    amount_usd DECIMAL(20,8) NOT NULL,
    used_amount_usd DECIMAL(20,8) NOT NULL DEFAULT 0,
    starts_at TIMESTAMPTZ NOT NULL DEFAULT NOW(),
    expires_at TIMESTAMPTZ NOT NULL,
    source VARCHAR(64) NOT NULL DEFAULT '',
    source_id VARCHAR(128) NOT NULL DEFAULT '',
    status VARCHAR(20) NOT NULL DEFAULT 'active',
    metadata JSONB NOT NULL DEFAULT '{}'::jsonb,
    created_at TIMESTAMPTZ NOT NULL DEFAULT NOW(),
    updated_at TIMESTAMPTZ NOT NULL DEFAULT NOW(),
    CONSTRAINT user_quota_grants_amount_positive CHECK (amount_usd > 0),
    CONSTRAINT user_quota_grants_used_non_negative CHECK (used_amount_usd >= 0),
    CONSTRAINT user_quota_grants_used_not_exceed_amount CHECK (used_amount_usd <= amount_usd),
    CONSTRAINT user_quota_grants_time_range CHECK (expires_at > starts_at)
);

CREATE INDEX IF NOT EXISTS idx_redeem_codes_metadata_gin
    ON redeem_codes USING GIN (metadata);

CREATE INDEX IF NOT EXISTS idx_user_quota_grants_user_active_expiry
    ON user_quota_grants (user_id, status, expires_at, id)
    WHERE status = 'active';

CREATE INDEX IF NOT EXISTS idx_user_quota_grants_source
    ON user_quota_grants (source, source_id);

CREATE INDEX IF NOT EXISTS idx_user_quota_grants_expires_at
    ON user_quota_grants (expires_at);
