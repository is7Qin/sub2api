//go:build unit

package service

import (
	"context"
	"testing"

	"github.com/stretchr/testify/require"
)

func TestFinalizeOutboxNotificationsSendsAndDeduplicatesBalanceAlert(t *testing.T) {
	ctx := context.Background()
	repo := newNotificationEmailMemorySettingRepo()
	smtpServer := startNotificationEmailTestSMTPServer(t)
	require.NoError(t, repo.SetMultiple(ctx, smtpServer.settings()))
	require.NoError(t, repo.SetMultiple(ctx, map[string]string{
		SettingKeyBalanceLowNotifyEnabled:   "true",
		SettingKeyBalanceLowNotifyThreshold: "10",
		SettingKeySiteName:                  "Billing Test",
	}))

	emailService := NewEmailService(repo, nil)
	notificationService := NewNotificationEmailService(repo, emailService)
	service := NewBalanceNotifyService(emailService, repo, nil)
	service.SetNotificationEmailService(notificationService)
	newBalance := 5.0
	user := &User{
		ID:                   41,
		Username:             "billing-user",
		BalanceNotifyEnabled: true,
		BalanceNotifyExtraEmails: []NotifyEmailEntry{
			{Email: "billing@example.com", Verified: true},
		},
	}
	account := &Account{ID: 52, Type: AccountTypeAPIKey}
	cost := &CostBreakdown{ActualCost: 10}
	result := &UsageBillingApplyResult{Applied: true, NewBalance: &newBalance}

	require.NoError(t, service.FinalizeOutboxNotifications(ctx, "attempt-19", user, account, cost, result))
	require.Equal(t, int64(1), smtpServer.messageCount())

	// Replayed finalization uses the same durable attempt identity, so it does
	// not deliver the same notification a second time.
	require.NoError(t, service.FinalizeOutboxNotifications(ctx, "attempt-19", user, account, cost, result))
	require.Equal(t, int64(1), smtpServer.messageCount())

	deliveryKey := notificationEmailDeliveryKey(
		NotificationEmailEventBalanceLow,
		"billing_outbox_balance_low",
		"attempt-19",
		"billing@example.com",
		"balance_low",
	)
	_, err := repo.GetValue(ctx, deliveryKey)
	require.NoError(t, err)
}

func TestFinalizeOutboxNotificationsReturnsTemplateErrors(t *testing.T) {
	ctx := context.Background()
	repo := newNotificationEmailMemorySettingRepo()
	require.NoError(t, repo.SetMultiple(ctx, map[string]string{
		SettingKeyBalanceLowNotifyEnabled:   "true",
		SettingKeyBalanceLowNotifyThreshold: "10",
	}))
	service := NewBalanceNotifyService(NewEmailService(repo, nil), repo, nil)
	service.SetNotificationEmailService(NewNotificationEmailService(repo, nil))
	newBalance := 5.0

	err := service.FinalizeOutboxNotifications(
		ctx,
		"attempt-19",
		&User{ID: 41, BalanceNotifyEnabled: true, BalanceNotifyExtraEmails: []NotifyEmailEntry{{Email: "billing@example.com", Verified: true}}},
		&Account{Type: AccountTypeAPIKey},
		&CostBreakdown{ActualCost: 10},
		&UsageBillingApplyResult{Applied: true, NewBalance: &newBalance},
	)

	require.Error(t, err)
}

func TestFinalizeOutboxNotificationsSkipsUnappliedBilling(t *testing.T) {
	service, repo := newBalanceNotifyServiceForTest()
	repo.data[SettingKeyBalanceLowNotifyEnabled] = "true"
	repo.data[SettingKeyBalanceLowNotifyThreshold] = "10"
	newBalance := 5.0

	require.NoError(t, service.FinalizeOutboxNotifications(
		context.Background(),
		"attempt-19",
		&User{BalanceNotifyEnabled: true},
		&Account{Type: AccountTypeAPIKey},
		&CostBreakdown{ActualCost: 10},
		&UsageBillingApplyResult{Applied: false, NewBalance: &newBalance},
	))
}
