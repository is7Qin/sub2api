package service

import (
	"context"
	"database/sql"
	"fmt"
	"strings"
	"testing"

	dbent "github.com/Wei-Shaw/sub2api/ent"
	"github.com/Wei-Shaw/sub2api/ent/enttest"
	"github.com/stretchr/testify/require"

	"entgo.io/ent/dialect"
	entsql "entgo.io/ent/dialect/sql"
	_ "modernc.org/sqlite"
)

func TestRedeemServiceSubscriptionRedeemPlanPrice(t *testing.T) {
	ctx := context.Background()
	client := newRedeemSubscriptionRebateTestClient(t)
	svc := &RedeemService{entClient: client}

	client.SubscriptionPlan.Create().
		SetGroupID(10).
		SetName("Monthly").
		SetPrice(29.9).
		SetValidityDays(1).
		SetValidityUnit(validityUnitMonth).
		SaveX(ctx)
	client.SubscriptionPlan.Create().
		SetGroupID(10).
		SetName("Weekly").
		SetPrice(9.9).
		SetValidityDays(1).
		SetValidityUnit(validityUnitWeek).
		SaveX(ctx)
	client.SubscriptionPlan.Create().
		SetGroupID(11).
		SetName("Other Monthly").
		SetPrice(19.9).
		SetValidityDays(1).
		SetValidityUnit(validityUnitMonth).
		SaveX(ctx)

	amount, ok := svc.subscriptionRedeemPlanPrice(ctx, 10, 30)
	require.True(t, ok)
	require.InDelta(t, 29.9, amount, 1e-9)

	amount, ok = svc.subscriptionRedeemPlanPrice(ctx, 10, 7)
	require.True(t, ok)
	require.InDelta(t, 9.9, amount, 1e-9)

	amount, ok = svc.subscriptionRedeemPlanPrice(ctx, 10, 60)
	require.False(t, ok)
	require.Zero(t, amount)
}

func TestRedeemServiceSubscriptionRedeemPlanPriceAmbiguous(t *testing.T) {
	ctx := context.Background()
	client := newRedeemSubscriptionRebateTestClient(t)
	svc := &RedeemService{entClient: client}

	for _, name := range []string{"Monthly A", "Monthly B"} {
		client.SubscriptionPlan.Create().
			SetGroupID(10).
			SetName(name).
			SetPrice(29.9).
			SetValidityDays(1).
			SetValidityUnit(validityUnitMonth).
			SaveX(ctx)
	}

	amount, ok := svc.subscriptionRedeemPlanPrice(ctx, 10, 30)
	require.False(t, ok)
	require.Zero(t, amount)
}

func TestRedeemServiceRedeemAffiliateRebateBaseAmount(t *testing.T) {
	ctx := context.Background()
	client := newRedeemSubscriptionRebateTestClient(t)
	svc := &RedeemService{entClient: client}
	groupID := int64(10)

	client.SubscriptionPlan.Create().
		SetGroupID(groupID).
		SetName("Monthly").
		SetPrice(29.9).
		SetValidityDays(1).
		SetValidityUnit(validityUnitMonth).
		SaveX(ctx)

	amount, ok := svc.redeemAffiliateRebateBaseAmount(ctx, &RedeemCode{Type: RedeemTypeBalance, Value: 10})
	require.True(t, ok)
	require.InDelta(t, 10, amount, 1e-9)

	amount, ok = svc.redeemAffiliateRebateBaseAmount(ctx, &RedeemCode{Type: RedeemTypeSubscription, GroupID: &groupID, ValidityDays: 30, Value: 999})
	require.True(t, ok)
	require.InDelta(t, 29.9, amount, 1e-9)

	amount, ok = svc.redeemAffiliateRebateBaseAmount(ctx, &RedeemCode{Type: RedeemTypeSubscription, GroupID: &groupID, ValidityDays: -30, Value: 999})
	require.False(t, ok)
	require.Zero(t, amount)
}

func newRedeemSubscriptionRebateTestClient(t *testing.T) *dbent.Client {
	t.Helper()

	dbName := fmt.Sprintf(
		"file:%s?mode=memory&cache=shared&_fk=1",
		strings.NewReplacer("/", "_", " ", "_").Replace(t.Name()),
	)
	db, err := sql.Open("sqlite", dbName)
	require.NoError(t, err)
	t.Cleanup(func() { _ = db.Close() })

	_, err = db.Exec("PRAGMA foreign_keys = ON")
	require.NoError(t, err)

	drv := entsql.OpenDB(dialect.SQLite, db)
	client := enttest.NewClient(t, enttest.WithOptions(dbent.Driver(drv)))
	t.Cleanup(func() { _ = client.Close() })
	return client
}
