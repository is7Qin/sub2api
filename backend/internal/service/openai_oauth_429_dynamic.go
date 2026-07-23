package service

import (
	"context"
	"log/slog"
	"time"
)

type openAIOAuth429DynamicWindow struct {
	startedAt time.Time
	planType  string
	policy    OpenAIOAuth429DynamicPolicy
	usage5h   *float64
	usage7d   *float64
	total     int
	count429  int
	limiting  bool
}

func (s *RateLimitService) RecordOpenAIOAuthUpstreamOutcome(ctx context.Context, account *Account, statusCode int, snapshots ...*OpenAICodexUsageSnapshot) {
	if s == nil || s.accountRepo == nil || !isOpenAIOAuthAccount(account) {
		return
	}
	is429 := statusCode == 429
	if !is429 && !s.hasOpenAIOAuth429DynamicStats(account.ID) {
		return
	}

	policy, ok := s.getOpenAIOAuth429DynamicSettings(ctx, account)
	if !ok || !policy.Enabled {
		// 禁用后清理已开启的窗口，避免成功请求继续命中动态统计热路径。
		s.ResetOpenAIOAuth429DynamicStats(account.ID)
		return
	}

	now := time.Now()
	planType := openAIAccountPlanType(account)
	window := time.Duration(policy.WindowSeconds) * time.Second

	s.openAIOAuth429DynamicMu.Lock()
	if s.openAIOAuth429DynamicStat == nil {
		s.openAIOAuth429DynamicStat = make(map[int64]*openAIOAuth429DynamicWindow)
	}

	stat := s.openAIOAuth429DynamicStat[account.ID]
	if stat == nil {
		if !is429 {
			s.openAIOAuth429DynamicMu.Unlock()
			return
		}
		stat = &openAIOAuth429DynamicWindow{startedAt: now, planType: planType, policy: *policy}
		s.openAIOAuth429DynamicStat[account.ID] = stat
	} else if stat.planType != planType || stat.policy != *policy {
		if !is429 {
			delete(s.openAIOAuth429DynamicStat, account.ID)
			s.openAIOAuth429DynamicMu.Unlock()
			return
		}
		stat.startedAt = now
		stat.planType = planType
		stat.policy = *policy
		stat.usage5h = nil
		stat.usage7d = nil
		stat.total = 0
		stat.count429 = 0
		stat.limiting = false
	} else if now.Sub(stat.startedAt) > window {
		if !is429 {
			delete(s.openAIOAuth429DynamicStat, account.ID)
			s.openAIOAuth429DynamicMu.Unlock()
			return
		}
		stat.startedAt = now
		stat.usage5h = nil
		stat.usage7d = nil
		stat.total = 0
		stat.count429 = 0
		stat.limiting = false
	}
	if stat.limiting {
		s.openAIOAuth429DynamicMu.Unlock()
		return
	}

	stat.total++
	if is429 {
		stat.count429++
	}
	total := stat.total
	count429 := stat.count429
	ratio := float64(count429) / float64(total)
	if len(snapshots) > 0 {
		if limits := snapshots[0].Normalize(); limits != nil {
			if limits.Used5hPercent != nil {
				value := *limits.Used5hPercent
				stat.usage5h = &value
			}
			if limits.Used7dPercent != nil {
				value := *limits.Used7dPercent
				stat.usage7d = &value
			}
		}
	}
	shouldLimit := total >= policy.MinSamples && count429 >= policy.Min429 && ratio >= policy.RatioThreshold
	if shouldLimit && policy.UsageWindowCheckEnabled {
		shouldLimit = openAIOAuth429UsageWindowReached(stat.usage5h, stat.usage7d, policy)
	}
	if shouldLimit {
		stat.limiting = true
	}
	s.openAIOAuth429DynamicMu.Unlock()

	if !shouldLimit {
		return
	}

	resetAt := now.Add(time.Duration(policy.BlockSeconds) * time.Second)
	if err := s.accountRepo.SetRateLimited(ctx, account.ID, resetAt); err != nil {
		s.openAIOAuth429DynamicMu.Lock()
		if current := s.openAIOAuth429DynamicStat[account.ID]; current == stat {
			stat.limiting = false
		}
		s.openAIOAuth429DynamicMu.Unlock()
		slog.Warn("openai_oauth_429_dynamic_set_rate_limited_failed", "account_id", account.ID, "error", err)
		return
	}
	s.openAIOAuth429DynamicMu.Lock()
	if current := s.openAIOAuth429DynamicStat[account.ID]; current == stat {
		delete(s.openAIOAuth429DynamicStat, account.ID)
	}
	s.openAIOAuth429DynamicMu.Unlock()
	s.notifyAccountSchedulingBlocked(account, resetAt, "openai_oauth_429_dynamic")
	slog.Info("openai_oauth_429_dynamic_rate_limited",
		"account_id", account.ID,
		"reset_at", resetAt,
		"window_seconds", policy.WindowSeconds,
		"samples", total,
		"count_429", count429,
		"ratio", ratio,
	)
}

func openAIOAuth429UsageWindowReached(used5h, used7d *float64, policy *OpenAIOAuth429DynamicPolicy) bool {
	if used5h == nil && used7d == nil {
		// Missing upstream window data must not disable the existing protection.
		return true
	}
	return (used5h != nil && *used5h >= policy.UsageWindow5hThresholdPercent) ||
		(used7d != nil && *used7d >= policy.UsageWindow7dThresholdPercent)
}

func (s *RateLimitService) ResetOpenAIOAuth429DynamicStats(accountID int64) {
	if s == nil || accountID <= 0 {
		return
	}
	s.openAIOAuth429DynamicMu.Lock()
	delete(s.openAIOAuth429DynamicStat, accountID)
	s.openAIOAuth429DynamicMu.Unlock()
}

func (s *RateLimitService) hasOpenAIOAuth429DynamicStats(accountID int64) bool {
	if s == nil || accountID <= 0 {
		return false
	}
	s.openAIOAuth429DynamicMu.Lock()
	_, ok := s.openAIOAuth429DynamicStat[accountID]
	s.openAIOAuth429DynamicMu.Unlock()
	return ok
}

func (s *RateLimitService) getOpenAIOAuth429DynamicSettings(ctx context.Context, account *Account) (*OpenAIOAuth429DynamicPolicy, bool) {
	accountID := int64(0)
	if account != nil {
		accountID = account.ID
	}
	planType := openAIAccountPlanType(account)
	if s.settingService != nil {
		policy, err := s.settingService.GetOpenAIOAuth429DynamicPolicy(ctx, planType)
		if err == nil {
			return &policy, true
		}
		slog.Warn("openai_oauth_429_dynamic_settings_read_failed", "account_id", accountID, "error", err)
	}
	return DefaultOpenAIOAuth429DynamicSettings().PolicyForPlanType(planType), true
}

func openAIAccountPlanType(account *Account) string {
	if account == nil || account.Credentials == nil {
		return ""
	}
	planType, _ := account.Credentials["plan_type"].(string)
	return normalizeOpenAIOAuth429PlanType(planType)
}
