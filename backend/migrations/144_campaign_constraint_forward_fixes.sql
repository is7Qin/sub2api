-- 144_campaign_constraint_forward_fixes.sql
-- Forward-fix constraints tightened after initial campaign migrations.

DO $$
BEGIN
    IF EXISTS (
        SELECT 1
        FROM user_quota_grants
        WHERE amount_usd <= 0
        LIMIT 1
    ) THEN
        RAISE EXCEPTION 'user_quota_grants contains non-positive amount_usd values; clean them before applying user_quota_grants_amount_positive';
    END IF;
END $$;

ALTER TABLE user_quota_grants
    DROP CONSTRAINT IF EXISTS user_quota_grants_amount_non_negative;

ALTER TABLE user_quota_grants
    DROP CONSTRAINT IF EXISTS user_quota_grants_amount_positive;

ALTER TABLE user_quota_grants
    ADD CONSTRAINT user_quota_grants_amount_positive CHECK (amount_usd > 0);

DO $$
BEGIN
    IF EXISTS (
        SELECT 1
        FROM ranking_reward_campaigns
        WHERE chance_count > 1000
        LIMIT 1
    ) THEN
        RAISE EXCEPTION 'ranking_reward_campaigns contains chance_count > 1000 values; clean them before applying ranking_reward_campaigns_chance_count_limit';
    END IF;

    IF EXISTS (
        SELECT 1
        FROM ranking_reward_campaigns
        WHERE top_n * chance_count > 10000
        LIMIT 1
    ) THEN
        RAISE EXCEPTION 'ranking_reward_campaigns contains top_n * chance_count > 10000 values; clean them before applying ranking_reward_campaigns_total_chances_limit';
    END IF;
END $$;

ALTER TABLE ranking_reward_campaigns
    DROP CONSTRAINT IF EXISTS ranking_reward_campaigns_chance_count_limit;

ALTER TABLE ranking_reward_campaigns
    ADD CONSTRAINT ranking_reward_campaigns_chance_count_limit CHECK (chance_count <= 1000);

ALTER TABLE ranking_reward_campaigns
    DROP CONSTRAINT IF EXISTS ranking_reward_campaigns_total_chances_limit;

ALTER TABLE ranking_reward_campaigns
    ADD CONSTRAINT ranking_reward_campaigns_total_chances_limit CHECK (top_n * chance_count <= 10000);
