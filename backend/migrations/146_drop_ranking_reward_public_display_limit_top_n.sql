-- 146_drop_ranking_reward_public_display_limit_top_n.sql
-- Allow public leaderboard display size to exceed reward winner count.

ALTER TABLE ranking_reward_campaigns
    DROP CONSTRAINT IF EXISTS ranking_reward_campaigns_public_display_limit_top_n;
