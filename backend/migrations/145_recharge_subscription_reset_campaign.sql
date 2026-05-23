CREATE TABLE IF NOT EXISTS recharge_reset_campaigns (
    id BIGSERIAL PRIMARY KEY,
    name VARCHAR(100) NOT NULL,
    description TEXT NOT NULL DEFAULT '',
    status VARCHAR(20) NOT NULL DEFAULT 'draft',
    starts_at TIMESTAMPTZ NOT NULL,
    ends_at TIMESTAMPTZ,
    reset_daily BOOLEAN NOT NULL DEFAULT true,
    reset_weekly BOOLEAN NOT NULL DEFAULT true,
    reset_monthly BOOLEAN NOT NULL DEFAULT true,
    metadata JSONB NOT NULL DEFAULT '{}'::jsonb,
    created_at TIMESTAMPTZ NOT NULL DEFAULT NOW(),
    updated_at TIMESTAMPTZ NOT NULL DEFAULT NOW(),
    CONSTRAINT recharge_reset_campaigns_status_check CHECK (status IN ('draft', 'active', 'disabled', 'ended')),
    CONSTRAINT recharge_reset_campaigns_time_range CHECK (ends_at IS NULL OR ends_at > starts_at),
    CONSTRAINT recharge_reset_campaigns_has_window CHECK (reset_daily OR reset_weekly OR reset_monthly)
);

CREATE TABLE IF NOT EXISTS recharge_reset_campaign_rules (
    id BIGSERIAL PRIMARY KEY,
    campaign_id BIGINT NOT NULL REFERENCES recharge_reset_campaigns(id) ON DELETE CASCADE,
    group_id BIGINT NOT NULL REFERENCES groups(id) ON DELETE CASCADE,
    threshold_amount DECIMAL(20,2) NOT NULL,
    status VARCHAR(20) NOT NULL DEFAULT 'active',
    metadata JSONB NOT NULL DEFAULT '{}'::jsonb,
    created_at TIMESTAMPTZ NOT NULL DEFAULT NOW(),
    updated_at TIMESTAMPTZ NOT NULL DEFAULT NOW(),
    CONSTRAINT recharge_reset_campaign_rules_status_check CHECK (status IN ('active', 'disabled')),
    CONSTRAINT recharge_reset_campaign_rules_threshold_positive CHECK (threshold_amount > 0),
    CONSTRAINT recharge_reset_campaign_rules_campaign_group_unique UNIQUE (campaign_id, group_id)
);

CREATE TABLE IF NOT EXISTS recharge_reset_records (
    id BIGSERIAL PRIMARY KEY,
    campaign_id BIGINT NOT NULL REFERENCES recharge_reset_campaigns(id) ON DELETE CASCADE,
    rule_id BIGINT NOT NULL REFERENCES recharge_reset_campaign_rules(id) ON DELETE CASCADE,
    redeem_code_id BIGINT NOT NULL REFERENCES redeem_codes(id) ON DELETE CASCADE,
    user_id BIGINT NOT NULL REFERENCES users(id) ON DELETE CASCADE,
    subscription_id BIGINT NOT NULL REFERENCES user_subscriptions(id) ON DELETE CASCADE,
    group_id BIGINT NOT NULL REFERENCES groups(id) ON DELETE CASCADE,
    recharge_amount DECIMAL(20,2) NOT NULL,
    threshold_amount DECIMAL(20,2) NOT NULL,
    reset_daily BOOLEAN NOT NULL DEFAULT false,
    reset_weekly BOOLEAN NOT NULL DEFAULT false,
    reset_monthly BOOLEAN NOT NULL DEFAULT false,
    metadata JSONB NOT NULL DEFAULT '{}'::jsonb,
    created_at TIMESTAMPTZ NOT NULL DEFAULT NOW(),
    CONSTRAINT recharge_reset_records_amount_non_negative CHECK (recharge_amount >= 0),
    CONSTRAINT recharge_reset_records_threshold_positive CHECK (threshold_amount > 0),
    CONSTRAINT recharge_reset_records_redeem_code_rule_unique UNIQUE (redeem_code_id, rule_id),
    CONSTRAINT recharge_reset_records_redeem_code_subscription_unique UNIQUE (redeem_code_id, subscription_id)
);

CREATE INDEX IF NOT EXISTS idx_recharge_reset_campaigns_status_time ON recharge_reset_campaigns(status, starts_at, ends_at);
CREATE INDEX IF NOT EXISTS idx_recharge_reset_campaign_rules_group ON recharge_reset_campaign_rules(group_id, status);
CREATE INDEX IF NOT EXISTS idx_recharge_reset_records_redeem_code ON recharge_reset_records(redeem_code_id);
CREATE INDEX IF NOT EXISTS idx_recharge_reset_records_user ON recharge_reset_records(user_id, created_at DESC);
