-- 145_ranking_reward_public_display_limit.sql
-- Configurable public leaderboard display size for ranking reward campaigns.

ALTER TABLE ranking_reward_campaigns
    ADD COLUMN IF NOT EXISTS public_display_limit INT;

UPDATE ranking_reward_campaigns
SET public_display_limit = LEAST(GREATEST(top_n, 1), 1000)
WHERE public_display_limit IS NULL;

ALTER TABLE ranking_reward_campaigns
    ALTER COLUMN public_display_limit SET DEFAULT 10;

ALTER TABLE ranking_reward_campaigns
    ALTER COLUMN public_display_limit SET NOT NULL;

DO $$
BEGIN
    IF EXISTS (
        SELECT 1
        FROM ranking_reward_campaigns
        WHERE public_display_limit <= 0 OR public_display_limit > 1000 OR public_display_limit > top_n
        LIMIT 1
    ) THEN
        RAISE EXCEPTION 'ranking_reward_campaigns contains invalid public_display_limit values; clean them before applying public display limit constraints';
    END IF;
END $$;

ALTER TABLE ranking_reward_campaigns
    DROP CONSTRAINT IF EXISTS ranking_reward_campaigns_public_display_limit_positive;

ALTER TABLE ranking_reward_campaigns
    ADD CONSTRAINT ranking_reward_campaigns_public_display_limit_positive CHECK (public_display_limit > 0);

ALTER TABLE ranking_reward_campaigns
    DROP CONSTRAINT IF EXISTS ranking_reward_campaigns_public_display_limit_limit;

ALTER TABLE ranking_reward_campaigns
    ADD CONSTRAINT ranking_reward_campaigns_public_display_limit_limit CHECK (public_display_limit <= 1000);

ALTER TABLE ranking_reward_campaigns
    DROP CONSTRAINT IF EXISTS ranking_reward_campaigns_public_display_limit_top_n;

ALTER TABLE ranking_reward_campaigns
    ADD CONSTRAINT ranking_reward_campaigns_public_display_limit_top_n CHECK (public_display_limit <= top_n);
