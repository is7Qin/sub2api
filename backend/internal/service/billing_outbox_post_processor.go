package service

import "context"

type billingOutboxNotificationFinalizer interface {
	FinalizeNotifications(context.Context, *BillingOutboxCommand, *UsageBillingApplyResult) error
}

type billingOutboxNotificationFinalizerFunc func(context.Context, *BillingOutboxCommand, *UsageBillingApplyResult) error

func (f billingOutboxNotificationFinalizerFunc) FinalizeNotifications(ctx context.Context, command *BillingOutboxCommand, result *UsageBillingApplyResult) error {
	return f(ctx, command, result)
}

// billingOutboxPostProcessor restores request-independent cache, enforcement,
// and notification effects from the immutable outbox snapshot.
type billingOutboxPostProcessor struct {
	deps                  *billingDeps
	apiKeyService         *APIKeyService
	notificationFinalizer billingOutboxNotificationFinalizer
}

func NewBillingOutboxPostProcessor(deps *billingDeps, apiKeyService *APIKeyService) BillingOutboxPostProcessor {
	return &billingOutboxPostProcessor{deps: deps, apiKeyService: apiKeyService}
}

func (p *billingOutboxPostProcessor) Finalize(ctx context.Context, command *BillingOutboxCommand, result *UsageBillingApplyResult) error {
	if p == nil || p.deps == nil || command == nil || command.PostEffects == nil || result == nil || !result.Applied {
		return nil
	}
	effects := command.PostEffects
	if result.APIKeyQuotaExhausted && p.apiKeyService != nil {
		apiKey, err := p.apiKeyService.GetByID(ctx, command.APIKeyID)
		if err != nil {
			return err
		}
		if apiKey != nil {
			p.apiKeyService.InvalidateAuthCacheByKey(ctx, apiKey.Key)
		}
	}
	if p.deps.billingCacheService != nil {
		// The billing transaction is authoritative. Finalization may be replayed
		// after a crash, so every cache action here is an idempotent invalidation,
		// never an additive mutation that could double-count usage.
		if effects.IsSubscriptionBill && effects.APIKeyGroupID != nil {
			if err := p.deps.billingCacheService.InvalidateSubscription(ctx, effects.UserID, *effects.APIKeyGroupID); err != nil {
				return err
			}
		} else if effects.ActualCost > 0 {
			if err := p.deps.billingCacheService.InvalidateUserBalance(ctx, effects.UserID); err != nil {
				return err
			}
		}
		if command.Billing.APIKeyRateLimitCost > 0 {
			if err := p.deps.billingCacheService.InvalidateAPIKeyRateLimit(ctx, command.APIKeyID); err != nil {
				return err
			}
		}
		// User-platform quota uses a separate additive ledger/cache and is
		// intentionally excluded here: replaying it safely belongs to Issue #24.
	}
	if !effects.PreserveAccountHealth && p.deps.deferredService != nil {
		p.deps.deferredService.ScheduleLastUsedUpdate(effects.AccountID)
	}
	if p.notificationFinalizer != nil {
		if err := p.notificationFinalizer.FinalizeNotifications(ctx, command, result); err != nil {
			return err
		}
	} else if p.deps.balanceNotifyService != nil {
		if err := p.finalizeNotifications(ctx, command, result); err != nil {
			return err
		}
	}
	return nil
}

func (p *billingOutboxPostProcessor) finalizeNotifications(ctx context.Context, command *BillingOutboxCommand, result *UsageBillingApplyResult) error {
	effects := command.PostEffects
	user := &User{
		ID:                         effects.UserID,
		Username:                   effects.UserUsername,
		Email:                      effects.UserEmail,
		Balance:                    effects.UserBalance,
		TotalRecharged:             effects.UserTotalRecharged,
		BalanceNotifyEnabled:       effects.BalanceNotifyEnabled,
		BalanceNotifyThreshold:     effects.BalanceNotifyThreshold,
		BalanceNotifyThresholdType: effects.BalanceNotifyThresholdType,
		BalanceNotifyExtraEmails:   append([]NotifyEmailEntry(nil), effects.BalanceNotifyExtraEmails...),
	}
	account := &Account{
		ID:       effects.AccountID,
		Name:     effects.AccountName,
		Platform: effects.AccountPlatform,
		Type:     effects.AccountType,
		Extra: map[string]any{
			"quota_notify_daily_enabled":         effects.QuotaNotifyDailyEnabled,
			"quota_notify_daily_threshold":       effects.QuotaNotifyDailyThreshold,
			"quota_notify_daily_threshold_type":  effects.QuotaNotifyDailyThresholdType,
			"quota_notify_weekly_enabled":        effects.QuotaNotifyWeeklyEnabled,
			"quota_notify_weekly_threshold":      effects.QuotaNotifyWeeklyThreshold,
			"quota_notify_weekly_threshold_type": effects.QuotaNotifyWeeklyThresholdType,
			"quota_notify_total_enabled":         effects.QuotaNotifyTotalEnabled,
			"quota_notify_total_threshold":       effects.QuotaNotifyTotalThreshold,
			"quota_notify_total_threshold_type":  effects.QuotaNotifyTotalThresholdType,
		},
	}
	notificationCost := &CostBreakdown{TotalCost: effects.TotalCost * effects.AccountRateMultiplier}
	if !effects.IsSubscriptionBill {
		notificationCost.ActualCost = effects.ActualCost
	}
	return p.deps.balanceNotifyService.FinalizeOutboxNotifications(ctx, command.AttemptID, user, account, notificationCost, result)
}
