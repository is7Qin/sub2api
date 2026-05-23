-- 142_lottery_campaign.sql
-- Lottery campaigns, prizes, expiring chances, and draw records.

CREATE TABLE IF NOT EXISTS lottery_campaigns (
    id BIGSERIAL PRIMARY KEY,
    name VARCHAR(100) NOT NULL,
    description TEXT NOT NULL DEFAULT '',
    status VARCHAR(20) NOT NULL DEFAULT 'draft',
    starts_at TIMESTAMPTZ NOT NULL DEFAULT NOW(),
    ends_at TIMESTAMPTZ,
    chance_expires_in_days INT NOT NULL DEFAULT 1,
    metadata JSONB NOT NULL DEFAULT '{}'::jsonb,
    created_at TIMESTAMPTZ NOT NULL DEFAULT NOW(),
    updated_at TIMESTAMPTZ NOT NULL DEFAULT NOW(),
    CONSTRAINT lottery_campaigns_chance_expiry_positive CHECK (chance_expires_in_days > 0),
    CONSTRAINT lottery_campaigns_time_range CHECK (ends_at IS NULL OR ends_at > starts_at)
);

CREATE TABLE IF NOT EXISTS lottery_prizes (
    id BIGSERIAL PRIMARY KEY,
    campaign_id BIGINT NOT NULL REFERENCES lottery_campaigns(id) ON DELETE CASCADE,
    name VARCHAR(100) NOT NULL,
    description TEXT NOT NULL DEFAULT '',
    status VARCHAR(20) NOT NULL DEFAULT 'active',
    weight INT NOT NULL DEFAULT 0,
    stock_total INT NOT NULL DEFAULT 0,
    stock_used INT NOT NULL DEFAULT 0,
    redeem_type VARCHAR(32) NOT NULL,
    redeem_value DECIMAL(20,8) NOT NULL DEFAULT 0,
    redeem_group_id BIGINT,
    redeem_validity_days INT NOT NULL DEFAULT 30,
    redeem_metadata JSONB NOT NULL DEFAULT '{}'::jsonb,
    sort_order INT NOT NULL DEFAULT 0,
    metadata JSONB NOT NULL DEFAULT '{}'::jsonb,
    created_at TIMESTAMPTZ NOT NULL DEFAULT NOW(),
    updated_at TIMESTAMPTZ NOT NULL DEFAULT NOW(),
    CONSTRAINT lottery_prizes_weight_non_negative CHECK (weight >= 0),
    CONSTRAINT lottery_prizes_stock_total_non_negative CHECK (stock_total >= 0),
    CONSTRAINT lottery_prizes_stock_used_non_negative CHECK (stock_used >= 0),
    CONSTRAINT lottery_prizes_stock_available CHECK (stock_total = 0 OR stock_used <= stock_total),
    CONSTRAINT lottery_prizes_redeem_validity_positive CHECK (redeem_validity_days > 0)
);

CREATE TABLE IF NOT EXISTS lottery_chances (
    id BIGSERIAL PRIMARY KEY,
    campaign_id BIGINT NOT NULL REFERENCES lottery_campaigns(id) ON DELETE CASCADE,
    user_id BIGINT NOT NULL REFERENCES users(id) ON DELETE CASCADE,
    source VARCHAR(64) NOT NULL DEFAULT '',
    source_id VARCHAR(128) NOT NULL DEFAULT '',
    status VARCHAR(20) NOT NULL DEFAULT 'available',
    expires_at TIMESTAMPTZ NOT NULL,
    used_at TIMESTAMPTZ,
    metadata JSONB NOT NULL DEFAULT '{}'::jsonb,
    created_at TIMESTAMPTZ NOT NULL DEFAULT NOW(),
    updated_at TIMESTAMPTZ NOT NULL DEFAULT NOW(),
    CONSTRAINT lottery_chances_expiry_after_created CHECK (expires_at > created_at)
);

CREATE TABLE IF NOT EXISTS lottery_draws (
    id BIGSERIAL PRIMARY KEY,
    campaign_id BIGINT NOT NULL REFERENCES lottery_campaigns(id) ON DELETE CASCADE,
    user_id BIGINT NOT NULL REFERENCES users(id) ON DELETE CASCADE,
    chance_id BIGINT NOT NULL REFERENCES lottery_chances(id) ON DELETE RESTRICT,
    prize_id BIGINT REFERENCES lottery_prizes(id) ON DELETE SET NULL,
    redeem_code_id BIGINT REFERENCES redeem_codes(id) ON DELETE SET NULL,
    redeem_code VARCHAR(64) NOT NULL DEFAULT '',
    status VARCHAR(20) NOT NULL DEFAULT 'awarded',
    error_message TEXT NOT NULL DEFAULT '',
    metadata JSONB NOT NULL DEFAULT '{}'::jsonb,
    drawn_at TIMESTAMPTZ NOT NULL DEFAULT NOW(),
    created_at TIMESTAMPTZ NOT NULL DEFAULT NOW(),
    updated_at TIMESTAMPTZ NOT NULL DEFAULT NOW()
);

CREATE INDEX IF NOT EXISTS idx_lottery_campaigns_status_time
    ON lottery_campaigns (status, starts_at, ends_at);

CREATE INDEX IF NOT EXISTS idx_lottery_prizes_campaign_status_sort
    ON lottery_prizes (campaign_id, status, sort_order, id);

CREATE INDEX IF NOT EXISTS idx_lottery_chances_user_available_expiry
    ON lottery_chances (user_id, campaign_id, status, expires_at, id)
    WHERE status = 'available';

CREATE INDEX IF NOT EXISTS idx_lottery_chances_campaign_status_expiry
    ON lottery_chances (campaign_id, status, expires_at, id);

CREATE UNIQUE INDEX IF NOT EXISTS idx_lottery_chances_source_unique
    ON lottery_chances (campaign_id, user_id, source, source_id)
    WHERE source <> '' AND source_id <> '';

CREATE UNIQUE INDEX IF NOT EXISTS idx_lottery_draws_chance_unique
    ON lottery_draws (chance_id);

CREATE INDEX IF NOT EXISTS idx_lottery_draws_user_campaign_drawn
    ON lottery_draws (user_id, campaign_id, drawn_at DESC);

CREATE INDEX IF NOT EXISTS idx_lottery_draws_campaign_drawn
    ON lottery_draws (campaign_id, drawn_at DESC);
