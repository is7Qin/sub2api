package service

import (
	"context"
	"log/slog"
	"time"
)

type openAIOAuth429DynamicWindow struct {
	startedAt time.Time
	planType  string
	total     int
	count429  int
	limiting  bool
}

func (s *RateLimitService) RecordOpenAIOAuthUpstreamOutcome(ctx context.Context, account *Account, statusCode int) {
	if s == nil || s.accountRepo == nil || !isOpenAIOAuthAccount(account) {
		return
	}
	is429 := statusCode == 429
	if !is429 && !s.hasOpenAIOAuth429DynamicStats(account.ID) {
		return
	}

	settings, ok := s.getOpenAIOAuth429DynamicSettings(ctx, account)
	if !ok || !settings.Enabled {
		// 禁用后清理已开启的窗口，避免成功请求继续命中动态统计热路径。
		s.ResetOpenAIOAuth429DynamicStats(account.ID)
		return
	}

	now := time.Now()
	planType := openAIAccountPlanType(account)
	window := time.Duration(settings.WindowSeconds) * time.Second

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
		stat = &openAIOAuth429DynamicWindow{startedAt: now, planType: planType}
		s.openAIOAuth429DynamicStat[account.ID] = stat
	} else if stat.planType != planType {
		if !is429 {
			delete(s.openAIOAuth429DynamicStat, account.ID)
			s.openAIOAuth429DynamicMu.Unlock()
			return
		}
		stat.startedAt = now
		stat.planType = planType
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
	shouldLimit := total >= settings.MinSamples && count429 >= settings.Min429 && ratio >= settings.RatioThreshold
	if shouldLimit {
		stat.limiting = true
	}
	s.openAIOAuth429DynamicMu.Unlock()

	if !shouldLimit {
		return
	}

	resetAt := now.Add(time.Duration(settings.BlockSeconds) * time.Second)
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
		"window_seconds", settings.WindowSeconds,
		"samples", total,
		"count_429", count429,
		"ratio", ratio,
	)
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
	if s.settingService != nil {
		settings, err := s.settingService.GetOpenAIOAuth429DynamicSettings(ctx)
		if err == nil && settings != nil {
			return settings.PolicyForPlanType(openAIAccountPlanType(account)), true
		}
		slog.Warn("openai_oauth_429_dynamic_settings_read_failed", "account_id", accountID, "error", err)
	}
	return DefaultOpenAIOAuth429DynamicSettings().PolicyForPlanType(openAIAccountPlanType(account)), true
}

func openAIAccountPlanType(account *Account) string {
	if account == nil || account.Credentials == nil {
		return ""
	}
	planType, _ := account.Credentials["plan_type"].(string)
	return normalizeOpenAIOAuth429PlanType(planType)
}
