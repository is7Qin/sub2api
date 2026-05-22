-- 143_daily_ranking_lottery_reward.sql
-- Daily user spending ranking campaigns that grant lottery chances.

CREATE TABLE IF NOT EXISTS ranking_reward_campaigns (
    id BIGSERIAL PRIMARY KEY,
    name VARCHAR(100) NOT NULL,
    description TEXT NOT NULL DEFAULT '',
    status VARCHAR(20) NOT NULL DEFAULT 'draft',
    lottery_campaign_id BIGINT NOT NULL REFERENCES lottery_campaigns(id) ON DELETE RESTRICT,
    top_n INT NOT NULL DEFAULT 10,
    chance_count INT NOT NULL DEFAULT 1,
    min_actual_cost DECIMAL(20,10) NOT NULL DEFAULT 0,
    starts_at TIMESTAMPTZ NOT NULL DEFAULT NOW(),
    ends_at TIMESTAMPTZ,
    timezone VARCHAR(64) NOT NULL DEFAULT 'Asia/Shanghai',
    last_run_date DATE,
    metadata JSONB NOT NULL DEFAULT '{}'::jsonb,
    created_at TIMESTAMPTZ NOT NULL DEFAULT NOW(),
    updated_at TIMESTAMPTZ NOT NULL DEFAULT NOW(),
    CONSTRAINT ranking_reward_campaigns_status_check CHECK (status IN ('draft', 'active', 'disabled', 'ended')),
    CONSTRAINT ranking_reward_campaigns_top_n_positive CHECK (top_n > 0),
    CONSTRAINT ranking_reward_campaigns_chance_count_positive CHECK (chance_count > 0),
    CONSTRAINT ranking_reward_campaigns_chance_count_limit CHECK (chance_count <= 1000),
    CONSTRAINT ranking_reward_campaigns_total_chances_limit CHECK (top_n * chance_count <= 10000),
    CONSTRAINT ranking_reward_campaigns_min_actual_cost_non_negative CHECK (min_actual_cost >= 0),
    CONSTRAINT ranking_reward_campaigns_time_range CHECK (ends_at IS NULL OR ends_at > starts_at)
);

CREATE TABLE IF NOT EXISTS ranking_reward_excluded_users (
    id BIGSERIAL PRIMARY KEY,
    campaign_id BIGINT NOT NULL REFERENCES ranking_reward_campaigns(id) ON DELETE CASCADE,
    user_id BIGINT NOT NULL REFERENCES users(id) ON DELETE CASCADE,
    reason TEXT NOT NULL DEFAULT '',
    created_at TIMESTAMPTZ NOT NULL DEFAULT NOW(),
    updated_at TIMESTAMPTZ NOT NULL DEFAULT NOW(),
    CONSTRAINT ranking_reward_excluded_users_campaign_user_unique UNIQUE (campaign_id, user_id)
);

CREATE TABLE IF NOT EXISTS ranking_reward_runs (
    id BIGSERIAL PRIMARY KEY,
    campaign_id BIGINT NOT NULL REFERENCES ranking_reward_campaigns(id) ON DELETE CASCADE,
    reward_date DATE NOT NULL,
    window_start TIMESTAMPTZ NOT NULL,
    window_end TIMESTAMPTZ NOT NULL,
    status VARCHAR(20) NOT NULL DEFAULT 'running',
    awarded_count INT NOT NULL DEFAULT 0,
    total_actual_cost DECIMAL(20,10) NOT NULL DEFAULT 0,
    error_message TEXT NOT NULL DEFAULT '',
    metadata JSONB NOT NULL DEFAULT '{}'::jsonb,
    started_at TIMESTAMPTZ NOT NULL DEFAULT NOW(),
    finished_at TIMESTAMPTZ,
    created_at TIMESTAMPTZ NOT NULL DEFAULT NOW(),
    updated_at TIMESTAMPTZ NOT NULL DEFAULT NOW(),
    CONSTRAINT ranking_reward_runs_status_check CHECK (status IN ('running', 'completed', 'failed')),
    CONSTRAINT ranking_reward_runs_window_range CHECK (window_end > window_start),
    CONSTRAINT ranking_reward_runs_awarded_non_negative CHECK (awarded_count >= 0),
    CONSTRAINT ranking_reward_runs_total_non_negative CHECK (total_actual_cost >= 0),
    CONSTRAINT ranking_reward_runs_campaign_date_unique UNIQUE (campaign_id, reward_date)
);

CREATE TABLE IF NOT EXISTS ranking_reward_awards (
    id BIGSERIAL PRIMARY KEY,
    run_id BIGINT NOT NULL REFERENCES ranking_reward_runs(id) ON DELETE CASCADE,
    campaign_id BIGINT NOT NULL REFERENCES ranking_reward_campaigns(id) ON DELETE CASCADE,
    lottery_campaign_id BIGINT NOT NULL REFERENCES lottery_campaigns(id) ON DELETE RESTRICT,
    user_id BIGINT NOT NULL REFERENCES users(id) ON DELETE CASCADE,
    rank INT NOT NULL,
    actual_cost DECIMAL(20,10) NOT NULL DEFAULT 0,
    requests BIGINT NOT NULL DEFAULT 0,
    tokens BIGINT NOT NULL DEFAULT 0,
    chance_count INT NOT NULL DEFAULT 1,
    lottery_chance_ids JSONB NOT NULL DEFAULT '[]'::jsonb,
    metadata JSONB NOT NULL DEFAULT '{}'::jsonb,
    created_at TIMESTAMPTZ NOT NULL DEFAULT NOW(),
    CONSTRAINT ranking_reward_awards_rank_positive CHECK (rank > 0),
    CONSTRAINT ranking_reward_awards_actual_cost_non_negative CHECK (actual_cost >= 0),
    CONSTRAINT ranking_reward_awards_requests_non_negative CHECK (requests >= 0),
    CONSTRAINT ranking_reward_awards_tokens_non_negative CHECK (tokens >= 0),
    CONSTRAINT ranking_reward_awards_chance_count_positive CHECK (chance_count > 0),
    CONSTRAINT ranking_reward_awards_run_user_unique UNIQUE (run_id, user_id)
);

CREATE INDEX IF NOT EXISTS idx_ranking_reward_campaigns_status_time
    ON ranking_reward_campaigns (status, starts_at, ends_at);

CREATE INDEX IF NOT EXISTS idx_ranking_reward_excluded_users_campaign
    ON ranking_reward_excluded_users (campaign_id, user_id);

CREATE INDEX IF NOT EXISTS idx_ranking_reward_runs_campaign_date
    ON ranking_reward_runs (campaign_id, reward_date DESC);

CREATE INDEX IF NOT EXISTS idx_ranking_reward_awards_campaign_user
    ON ranking_reward_awards (campaign_id, user_id, created_at DESC);
