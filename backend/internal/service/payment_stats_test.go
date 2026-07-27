//go:build unit

package service

import (
	"testing"
	"time"

	dbent "github.com/Wei-Shaw/sub2api/ent"
	"github.com/stretchr/testify/require"
)

func TestComputeBasicStatsSeparatesCurrencies(t *testing.T) {
	todayStart := time.Date(2026, time.July, 27, 0, 0, 0, 0, time.UTC)
	today := todayStart.Add(time.Hour)
	yesterday := todayStart.Add(-time.Hour)
	orders := []*dbent.PaymentOrder{
		paymentStatsOrder(1, "alice@example.com", "cny", 10, &today),
		paymentStatsOrder(2, "bob@example.com", "USD", 10, &today),
		paymentStatsOrder(1, "alice@example.com", "CNY", 5, &yesterday),
	}
	stats := &DashboardStats{}
	computeBasicStats(stats, orders, todayStart)
	require.Equal(t, CurrencyAmounts{"CNY": 15, "USD": 10}, stats.TotalAmount)
	require.Equal(t, CurrencyAmounts{"CNY": 10, "USD": 10}, stats.TodayAmount)
	require.Equal(t, CurrencyAmounts{"CNY": 7.5, "USD": 10}, stats.AvgAmount)
	require.Equal(t, 3, stats.TotalCount)
	require.Equal(t, 2, stats.TodayCount)
}

func TestPaymentDashboardBreakdownsSeparateCurrencies(t *testing.T) {
	first := time.Date(2026, time.July, 26, 12, 0, 0, 0, time.UTC)
	second := first.AddDate(0, 0, 1)
	orders := []*dbent.PaymentOrder{
		paymentStatsOrder(1, "alice@example.com", "CNY", 5.555, &first),
		paymentStatsOrder(2, "bob@example.com", "CNY", 10, &first),
		paymentStatsOrder(1, "alice@example.com", "USD", 20, &second),
		paymentStatsOrder(2, "bob@example.com", "USD", 10, &second),
	}
	orders[0].PaymentType, orders[1].PaymentType, orders[2].PaymentType, orders[3].PaymentType = "stripe", "stripe", "stripe", "alipay"

	require.Equal(t, []DailyStats{
		{Date: "2026-07-26", Amount: CurrencyAmounts{"CNY": 15.56}, Count: 2},
		{Date: "2026-07-27", Amount: CurrencyAmounts{"USD": 30}, Count: 2},
	}, buildDailySeries(orders, first.AddDate(0, 0, -1), 2))
	require.Equal(t, []PaymentMethodStat{
		{Type: "alipay", Amount: CurrencyAmounts{"USD": 10}, Count: 1},
		{Type: "stripe", Amount: CurrencyAmounts{"CNY": 15.56, "USD": 20}, Count: 3},
	}, buildMethodDistribution(orders))
	require.Equal(t, TopUsersByCurrency{
		"CNY": {{UserID: 2, Email: "bob@example.com", Amount: 10}, {UserID: 1, Email: "alice@example.com", Amount: 5.56}},
		"USD": {{UserID: 1, Email: "alice@example.com", Amount: 20}, {UserID: 2, Email: "bob@example.com", Amount: 10}},
	}, buildTopUsers(orders))
}

func TestPaymentDashboardUsesCurrencyPrecision(t *testing.T) {
	paidAt := time.Date(2026, time.July, 27, 12, 0, 0, 0, time.UTC)
	orders := []*dbent.PaymentOrder{
		paymentStatsOrder(1, "alice@example.com", "KWD", 1.234, &paidAt),
		paymentStatsOrder(1, "alice@example.com", "JPY", 100.4, &paidAt),
	}
	orders[0].PaymentType = "stripe"
	orders[1].PaymentType = "stripe"

	stats := &DashboardStats{}
	computeBasicStats(stats, orders, paidAt.Add(-time.Hour))
	require.Equal(t, CurrencyAmounts{"KWD": 1.234, "JPY": 100}, stats.TotalAmount)
	require.Equal(t, CurrencyAmounts{"KWD": 1.234, "JPY": 100}, stats.AvgAmount)
	require.Equal(t, CurrencyAmounts{"KWD": 1.234, "JPY": 100}, buildMethodDistribution(orders)[0].Amount)
	require.Equal(t, float64(1.234), buildTopUsers(orders)["KWD"][0].Amount)
}

func TestPaymentDashboardEmptyUsesNonNilCollections(t *testing.T) {
	stats := &DashboardStats{}
	computeBasicStats(stats, nil, time.Now())
	require.NotNil(t, stats.TotalAmount)
	require.NotNil(t, stats.TodayAmount)
	require.NotNil(t, stats.AvgAmount)
	require.Empty(t, buildTopUsers(nil))
	require.NotNil(t, buildTopUsers(nil))
}

func paymentStatsOrder(userID int64, email, currency string, amount float64, paidAt *time.Time) *dbent.PaymentOrder {
	return &dbent.PaymentOrder{UserID: userID, UserEmail: email, PayAmount: amount, PaidAt: paidAt, ProviderSnapshot: map[string]any{"currency": currency}}
}
