//go:build unit

package service

import (
	"context"
	"errors"
	"testing"

	"github.com/stretchr/testify/require"
)

func TestBuildBillingOutboxPostEffectsCapturesNotificationInputs(t *testing.T) {
	balanceThreshold := 12.5
	p := &postUsageBillingParams{
		Cost: &CostBreakdown{ActualCost: 3.25, TotalCost: 6.5},
		User: &User{
			ID:                         41,
			Username:                   "billing-user",
			Email:                      "billing@example.com",
			Balance:                    18.75,
			TotalRecharged:             250,
			BalanceNotifyEnabled:       true,
			BalanceNotifyThreshold:     &balanceThreshold,
			BalanceNotifyThresholdType: thresholdTypePercentage,
			BalanceNotifyExtraEmails: []NotifyEmailEntry{
				{Email: "verified@example.com", Verified: true},
			},
		},
		APIKey: &APIKey{},
		Account: &Account{
			ID:       52,
			Name:     "billing-account",
			Platform: PlatformAnthropic,
			Type:     AccountTypeAPIKey,
			Extra: map[string]any{
				"quota_notify_daily_enabled":         true,
				"quota_notify_daily_threshold":       10.0,
				"quota_notify_daily_threshold_type":  thresholdTypeFixed,
				"quota_notify_weekly_enabled":        true,
				"quota_notify_weekly_threshold":      20.0,
				"quota_notify_weekly_threshold_type": thresholdTypePercentage,
				"quota_notify_total_enabled":         true,
				"quota_notify_total_threshold":       30.0,
				"quota_notify_total_threshold_type":  thresholdTypeFixed,
			},
		},
	}

	effects := buildBillingOutboxPostEffects(p)

	require.Equal(t, "billing-user", effects.UserUsername)
	require.Equal(t, "billing@example.com", effects.UserEmail)
	require.Equal(t, 250.0, effects.UserTotalRecharged)
	require.Equal(t, p.User.BalanceNotifyExtraEmails, effects.BalanceNotifyExtraEmails)
	require.Equal(t, "billing-account", effects.AccountName)
	require.Equal(t, PlatformAnthropic, effects.AccountPlatform)
	require.True(t, effects.QuotaNotifyDailyEnabled)
	require.Equal(t, 10.0, effects.QuotaNotifyDailyThreshold)
	require.Equal(t, thresholdTypeFixed, effects.QuotaNotifyDailyThresholdType)
	require.True(t, effects.QuotaNotifyWeeklyEnabled)
	require.Equal(t, 20.0, effects.QuotaNotifyWeeklyThreshold)
	require.Equal(t, thresholdTypePercentage, effects.QuotaNotifyWeeklyThresholdType)
	require.True(t, effects.QuotaNotifyTotalEnabled)
	require.Equal(t, 30.0, effects.QuotaNotifyTotalThreshold)
	require.Equal(t, thresholdTypeFixed, effects.QuotaNotifyTotalThresholdType)
}

func TestBillingOutboxPostProcessorSkipsBalanceAlertForSubscriptionBill(t *testing.T) {
	ctx := context.Background()
	repo := newNotificationEmailMemorySettingRepo()
	smtpServer := startNotificationEmailTestSMTPServer(t)
	require.NoError(t, repo.SetMultiple(ctx, smtpServer.settings()))
	require.NoError(t, repo.SetMultiple(ctx, map[string]string{
		SettingKeyBalanceLowNotifyEnabled:   "true",
		SettingKeyBalanceLowNotifyThreshold: "10",
	}))
	emailService := NewEmailService(repo, nil)
	notifyService := NewBalanceNotifyService(emailService, repo, nil)
	notifyService.SetNotificationEmailService(NewNotificationEmailService(repo, emailService))
	processor := &billingOutboxPostProcessor{deps: &billingDeps{balanceNotifyService: notifyService}}
	newBalance := 5.0
	command := &BillingOutboxCommand{
		AttemptID: "subscription-attempt-19",
		PostEffects: &BillingOutboxPostEffects{
			UserID:                   41,
			UserUsername:             "subscription-user",
			BalanceNotifyEnabled:     true,
			BalanceNotifyExtraEmails: []NotifyEmailEntry{{Email: "billing@example.com", Verified: true}},
			AccountID:                52,
			AccountType:              AccountTypeAPIKey,
			ActualCost:               10,
			IsSubscriptionBill:       true,
		},
	}

	require.NoError(t, processor.Finalize(ctx, command, &UsageBillingApplyResult{Applied: true, NewBalance: &newBalance}))
	require.Zero(t, smtpServer.messageCount())
}

func TestBillingOutboxPostProcessorReturnsSynchronousNotificationFinalizationError(t *testing.T) {
	notificationErr := errors.New("notification provider unavailable")
	finalizer := billingOutboxNotificationFinalizerFunc(func(context.Context, *BillingOutboxCommand, *UsageBillingApplyResult) error {
		return notificationErr
	})
	processor := &billingOutboxPostProcessor{
		deps:                  &billingDeps{},
		notificationFinalizer: finalizer,
	}
	command := &BillingOutboxCommand{
		AttemptID: "billing-attempt-19",
		PostEffects: &BillingOutboxPostEffects{
			UserID:    41,
			AccountID: 52,
		},
	}

	err := processor.Finalize(context.Background(), command, &UsageBillingApplyResult{Applied: true})

	require.ErrorIs(t, err, notificationErr)
}
