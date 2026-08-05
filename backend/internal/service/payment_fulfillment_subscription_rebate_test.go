//go:build unit

package service

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"math"
	"strings"
	"testing"
	"time"

	dbent "github.com/Wei-Shaw/sub2api/ent"
	"github.com/Wei-Shaw/sub2api/ent/enttest"
	"github.com/Wei-Shaw/sub2api/ent/paymentauditlog"
	"github.com/Wei-Shaw/sub2api/internal/payment"
	"github.com/stretchr/testify/require"

	"entgo.io/ent/dialect"
	entsql "entgo.io/ent/dialect/sql"
	_ "modernc.org/sqlite"
)

type paymentFulfillmentSettingRepo struct {
	values map[string]string
}

func (r *paymentFulfillmentSettingRepo) Get(context.Context, string) (*Setting, error) {
	panic("unexpected Get call")
}
func (r *paymentFulfillmentSettingRepo) GetValue(_ context.Context, key string) (string, error) {
	value, ok := r.values[key]
	if !ok {
		return "", ErrSettingNotFound
	}
	return value, nil
}
func (r *paymentFulfillmentSettingRepo) Set(context.Context, string, string) error {
	panic("unexpected Set call")
}
func (r *paymentFulfillmentSettingRepo) GetMultiple(context.Context, []string) (map[string]string, error) {
	panic("unexpected GetMultiple call")
}
func (r *paymentFulfillmentSettingRepo) SetMultiple(context.Context, map[string]string) error {
	panic("unexpected SetMultiple call")
}
func (r *paymentFulfillmentSettingRepo) GetAll(context.Context) (map[string]string, error) {
	panic("unexpected GetAll call")
}
func (r *paymentFulfillmentSettingRepo) Delete(context.Context, string) error {
	panic("unexpected Delete call")
}

type paymentFulfillmentAffiliateRepo struct {
	invitee     *AffiliateSummary
	inviter     *AffiliateSummary
	accrued     map[int64]float64
	calls       int
	lastBase    float64
	lastOrder   *int64
	failOnce    bool
	afterAccrue func(context.Context) error
}

func (r *paymentFulfillmentAffiliateRepo) EnsureUserAffiliate(_ context.Context, userID int64) (*AffiliateSummary, error) {
	switch {
	case r.invitee != nil && r.invitee.UserID == userID:
		cp := *r.invitee
		return &cp, nil
	case r.inviter != nil && r.inviter.UserID == userID:
		cp := *r.inviter
		return &cp, nil
	default:
		return &AffiliateSummary{UserID: userID, CreatedAt: time.Now().Add(-time.Hour)}, nil
	}
}
func (r *paymentFulfillmentAffiliateRepo) GetAffiliateByCode(context.Context, string) (*AffiliateSummary, error) {
	panic("unexpected GetAffiliateByCode call")
}
func (r *paymentFulfillmentAffiliateRepo) BindInviter(context.Context, int64, int64) (bool, error) {
	panic("unexpected BindInviter call")
}
func (r *paymentFulfillmentAffiliateRepo) AccrueQuota(ctx context.Context, inviterID, inviteeUserID int64, amount float64, _ int, sourceOrderID *int64) (bool, error) {
	r.calls++
	r.lastBase = amount
	if sourceOrderID != nil {
		oid := *sourceOrderID
		r.lastOrder = &oid
	}
	if r.failOnce {
		r.failOnce = false
		return false, errors.New("affiliate unavailable")
	}
	if r.afterAccrue != nil {
		if err := r.afterAccrue(ctx); err != nil {
			return false, err
		}
	}
	if sourceOrderID != nil {
		if _, ok := r.accrued[*sourceOrderID]; ok {
			return false, nil
		}
		r.accrued[*sourceOrderID] = amount
	}
	return true, nil
}
func (r *paymentFulfillmentAffiliateRepo) GetAccruedRebateFromInvitee(context.Context, int64, int64) (float64, error) {
	return 0, nil
}
func (r *paymentFulfillmentAffiliateRepo) ThawFrozenQuota(context.Context, int64) (float64, error) {
	return 0, nil
}
func (r *paymentFulfillmentAffiliateRepo) TransferQuotaToBalance(context.Context, int64) (float64, float64, error) {
	panic("unexpected TransferQuotaToBalance call")
}
func (r *paymentFulfillmentAffiliateRepo) ListInvitees(context.Context, int64, int) ([]AffiliateInvitee, error) {
	panic("unexpected ListInvitees call")
}
func (r *paymentFulfillmentAffiliateRepo) UpdateUserAffCode(context.Context, int64, string) error {
	panic("unexpected UpdateUserAffCode call")
}
func (r *paymentFulfillmentAffiliateRepo) ResetUserAffCode(context.Context, int64) (string, error) {
	panic("unexpected ResetUserAffCode call")
}
func (r *paymentFulfillmentAffiliateRepo) SetUserRebateRate(context.Context, int64, *float64) error {
	panic("unexpected SetUserRebateRate call")
}
func (r *paymentFulfillmentAffiliateRepo) BatchSetUserRebateRate(context.Context, []int64, *float64) error {
	panic("unexpected BatchSetUserRebateRate call")
}
func (r *paymentFulfillmentAffiliateRepo) ListUsersWithCustomSettings(context.Context, AffiliateAdminFilter) ([]AffiliateAdminEntry, int64, error) {
	panic("unexpected ListUsersWithCustomSettings call")
}
func (r *paymentFulfillmentAffiliateRepo) ListAffiliateInviteRecords(context.Context, AffiliateRecordFilter) ([]AffiliateInviteRecord, int64, error) {
	panic("unexpected ListAffiliateInviteRecords call")
}
func (r *paymentFulfillmentAffiliateRepo) ListAffiliateRebateRecords(context.Context, AffiliateRecordFilter) ([]AffiliateRebateRecord, int64, error) {
	panic("unexpected ListAffiliateRebateRecords call")
}
func (r *paymentFulfillmentAffiliateRepo) ListAffiliateTransferRecords(context.Context, AffiliateRecordFilter) ([]AffiliateTransferRecord, int64, error) {
	panic("unexpected ListAffiliateTransferRecords call")
}
func (r *paymentFulfillmentAffiliateRepo) GetAffiliateUserOverview(context.Context, int64) (*AffiliateUserOverview, error) {
	panic("unexpected GetAffiliateUserOverview call")
}

type paymentFulfillmentUserSubRepo struct {
	userSubRepoNoop

	nextID          int64
	byID            map[int64]*UserSubscription
	byUserGroup     map[string]*UserSubscription
	extendCalls     int
	createCalls     int
	updateCalls     int
	getWitnessError error
	createError     error
}

func newPaymentFulfillmentUserSubRepo() *paymentFulfillmentUserSubRepo {
	return &paymentFulfillmentUserSubRepo{
		nextID:      1,
		byID:        make(map[int64]*UserSubscription),
		byUserGroup: make(map[string]*UserSubscription),
	}
}

func (r *paymentFulfillmentUserSubRepo) key(userID, groupID int64) string {
	return fmt.Sprintf("%d:%d", userID, groupID)
}

func (r *paymentFulfillmentUserSubRepo) seed(sub *UserSubscription) {
	cp := *sub
	if cp.ID == 0 {
		cp.ID = r.nextID
		r.nextID++
	}
	r.byID[cp.ID] = &cp
	r.byUserGroup[r.key(cp.UserID, cp.GroupID)] = &cp
}

func (r *paymentFulfillmentUserSubRepo) Create(_ context.Context, sub *UserSubscription) error {
	r.createCalls++
	if r.createError != nil {
		return r.createError
	}
	cp := *sub
	if cp.ID == 0 {
		cp.ID = r.nextID
		r.nextID++
	}
	sub.ID = cp.ID
	r.byID[cp.ID] = &cp
	r.byUserGroup[r.key(cp.UserID, cp.GroupID)] = &cp
	return nil
}

func (r *paymentFulfillmentUserSubRepo) GetByID(_ context.Context, id int64) (*UserSubscription, error) {
	sub := r.byID[id]
	if sub == nil {
		return nil, ErrSubscriptionNotFound
	}
	cp := *sub
	return &cp, nil
}

func (r *paymentFulfillmentUserSubRepo) GetByIDForUpdate(ctx context.Context, id int64) (*UserSubscription, error) {
	return r.GetByID(ctx, id)
}

func (r *paymentFulfillmentUserSubRepo) GetByUserIDAndGroupID(_ context.Context, userID, groupID int64) (*UserSubscription, error) {
	if r.getWitnessError != nil {
		return nil, r.getWitnessError
	}
	sub := r.byUserGroup[r.key(userID, groupID)]
	if sub == nil {
		return nil, ErrSubscriptionNotFound
	}
	cp := *sub
	return &cp, nil
}

func (r *paymentFulfillmentUserSubRepo) GetActiveByUserIDAndGroupID(ctx context.Context, userID, groupID int64) (*UserSubscription, error) {
	return r.GetByUserIDAndGroupID(ctx, userID, groupID)
}

func (r *paymentFulfillmentUserSubRepo) Update(_ context.Context, sub *UserSubscription) error {
	r.updateCalls++
	existing := r.byID[sub.ID]
	if existing == nil {
		return ErrSubscriptionNotFound
	}
	oldKey := r.key(existing.UserID, existing.GroupID)
	cp := *sub
	r.byID[cp.ID] = &cp
	delete(r.byUserGroup, oldKey)
	r.byUserGroup[r.key(cp.UserID, cp.GroupID)] = &cp
	return nil
}

func (r *paymentFulfillmentUserSubRepo) ExtendExpiry(_ context.Context, subscriptionID int64, newExpiresAt time.Time) error {
	r.extendCalls++
	sub := r.byID[subscriptionID]
	if sub == nil {
		return ErrSubscriptionNotFound
	}
	sub.ExpiresAt = newExpiresAt
	return nil
}

func (r *paymentFulfillmentUserSubRepo) UpdateStatus(_ context.Context, subscriptionID int64, status string) error {
	sub := r.byID[subscriptionID]
	if sub == nil {
		return ErrSubscriptionNotFound
	}
	sub.Status = status
	return nil
}

func (r *paymentFulfillmentUserSubRepo) UpdateNotes(_ context.Context, subscriptionID int64, notes string) error {
	sub := r.byID[subscriptionID]
	if sub == nil {
		return ErrSubscriptionNotFound
	}
	sub.Notes = notes
	return nil
}

type paymentFulfillmentGroupRepo struct {
	groupRepoNoop
	group *Group
}

func (r paymentFulfillmentGroupRepo) GetByID(context.Context, int64) (*Group, error) {
	return r.group, nil
}

func TestSubscriptionPaymentFulfillment_RebateAfterAssignment(t *testing.T) {
	ctx := context.Background()
	fixture := newPaymentFulfillmentFixture(t)
	order := fixture.createSubscriptionOrder(t, 100, 80)

	err := fixture.paymentSvc.ExecuteSubscriptionFulfillment(ctx, order.ID)
	require.NoError(t, err)

	require.Equal(t, 1, fixture.subRepo.createCalls)
	require.Equal(t, 1, fixture.affiliateRepo.calls)
	require.InDelta(t, 100, fixture.affiliateRepo.lastBase, 1e-9)
	fixture.requireOrderStatus(t, order.ID, OrderStatusCompleted)
	fixture.requireAuditAction(t, order.ID, "SUBSCRIPTION_SUCCESS")
	fixture.requireAuditAction(t, order.ID, "AFFILIATE_REBATE_APPLIED")
}

func TestSubscriptionPaymentFulfillment_RetryAfterAffiliateFailureUsesAssignmentWitness(t *testing.T) {
	ctx := context.Background()
	fixture := newPaymentFulfillmentFixture(t)
	fixture.affiliateRepo.failOnce = true
	order := fixture.createSubscriptionOrder(t, 100, 80)

	err := fixture.paymentSvc.ExecuteSubscriptionFulfillment(ctx, order.ID)
	require.ErrorContains(t, err, "accrue affiliate rebate")
	require.Equal(t, 1, fixture.subRepo.createCalls)
	fixture.requireOrderStatus(t, order.ID, OrderStatusFailed)

	err = fixture.paymentSvc.RetryFulfillment(ctx, order.ID)
	require.NoError(t, err)
	require.Equal(t, 1, fixture.subRepo.createCalls)
	require.Equal(t, 0, fixture.subRepo.extendCalls)
	require.Equal(t, 2, fixture.affiliateRepo.calls)
	fixture.requireOrderStatus(t, order.ID, OrderStatusCompleted)
}

func TestSubscriptionPaymentFulfillment_MissingAssignmentAuditUsesDurableNoteWitness(t *testing.T) {
	ctx := context.Background()
	fixture := newPaymentFulfillmentFixture(t)
	order := fixture.createSubscriptionOrder(t, 100, 80)
	seedExistingSubscription(t, fixture.subRepo, order.UserID, *order.SubscriptionGroupID, 30, subscriptionPaymentOrderNote(order.ID))

	err := fixture.paymentSvc.ExecuteSubscriptionFulfillment(ctx, order.ID)
	require.NoError(t, err)
	require.Equal(t, 0, fixture.subRepo.createCalls)
	require.Equal(t, 0, fixture.subRepo.extendCalls)
	require.Equal(t, 1, fixture.affiliateRepo.calls)
	fixture.requireOrderStatus(t, order.ID, OrderStatusCompleted)
}

func TestSubscriptionPaymentFulfillment_StaleSuccessAuditWithoutNoteDoesNotBypassAssignment(t *testing.T) {
	ctx := context.Background()
	fixture := newPaymentFulfillmentFixture(t)
	order := fixture.createSubscriptionOrder(t, 100, 80)
	fixture.writeAuditAction(t, order.ID, "SUBSCRIPTION_SUCCESS")
	fixture.subRepo.createError = errors.New("subscription write failed")

	err := fixture.paymentSvc.ExecuteSubscriptionFulfillment(ctx, order.ID)
	require.ErrorContains(t, err, "assign subscription")

	require.Equal(t, 1, fixture.subRepo.createCalls)
	require.Equal(t, 0, fixture.affiliateRepo.calls)
	fixture.requireOrderStatus(t, order.ID, OrderStatusFailed)
	fixture.requireNoAuditAction(t, order.ID, "AFFILIATE_REBATE_APPLIED")
}

func TestSubscriptionPaymentFulfillment_NoDuplicateRebateOnRetry(t *testing.T) {
	ctx := context.Background()
	fixture := newPaymentFulfillmentFixture(t)
	order := fixture.createSubscriptionOrder(t, 100, 80)

	err := fixture.paymentSvc.ExecuteSubscriptionFulfillment(ctx, order.ID)
	require.NoError(t, err)
	_, err = fixture.client.PaymentOrder.UpdateOneID(order.ID).SetStatus(OrderStatusFailed).Save(ctx)
	require.NoError(t, err)

	err = fixture.paymentSvc.RetryFulfillment(ctx, order.ID)
	require.NoError(t, err)
	require.Equal(t, 1, fixture.affiliateRepo.calls)
	require.Len(t, fixture.affiliateRepo.accrued, 1)
}

func TestSubscriptionPaymentFulfillment_RebateBaseUsesAmountNotPayAmount(t *testing.T) {
	ctx := context.Background()
	fixture := newPaymentFulfillmentFixture(t)
	order := fixture.createSubscriptionOrder(t, 123.45, 88.88)

	err := fixture.paymentSvc.ExecuteSubscriptionFulfillment(ctx, order.ID)
	require.NoError(t, err)
	require.InDelta(t, 123.45, fixture.affiliateRepo.lastBase, 1e-9)
}

func TestAffiliateRebateBaseValidationSkipsUnsafeValues(t *testing.T) {
	ctx := context.Background()
	fixture := newPaymentFulfillmentFixture(t)

	for _, base := range []float64{0, -1, math.NaN(), math.Inf(1), math.Inf(-1)} {
		t.Run(fmt.Sprintf("%v", base), func(t *testing.T) {
			order := &dbent.PaymentOrder{
				ID:        9000,
				UserID:    2000,
				Amount:    base,
				PayAmount: 100,
				OrderType: payment.OrderTypeSubscription,
			}
			err := fixture.paymentSvc.applyAffiliateRebateForOrder(ctx, order)
			require.NoError(t, err)
		})
	}

	require.Equal(t, 0, fixture.affiliateRepo.calls)
	count, err := fixture.client.PaymentAuditLog.Query().Where(paymentauditlog.ActionIn("AFFILIATE_REBATE_APPLIED", "AFFILIATE_REBATE_SKIPPED", "AFFILIATE_REBATE_FAILED")).Count(ctx)
	require.NoError(t, err)
	require.Zero(t, count)
}

func TestAffiliateRebateAuditClaimDBErrorFailsClosed(t *testing.T) {
	ctx := context.Background()
	fixture := newPaymentFulfillmentFixture(t)
	order := fixture.createSubscriptionOrder(t, 100, 80)
	_, err := fixture.client.ExecContext(ctx, "DROP TABLE payment_audit_logs")
	require.NoError(t, err)

	err = fixture.paymentSvc.applyAffiliateRebateForOrder(ctx, order)
	require.Error(t, err)
	require.ErrorContains(t, err, "claim affiliate rebate audit")
	require.Equal(t, 0, fixture.affiliateRepo.calls)
}

func TestAffiliateRebateAuditUpdateDBErrorFailsClosed(t *testing.T) {
	ctx := context.Background()
	fixture := newPaymentFulfillmentFixture(t)
	order := fixture.createSubscriptionOrder(t, 100, 80)
	fixture.affiliateRepo.afterAccrue = func(ctx context.Context) error {
		tx := dbent.TxFromContext(ctx)
		if tx == nil {
			return errors.New("missing transaction")
		}
		_, err := tx.Client().ExecContext(ctx, "DROP TABLE payment_audit_logs")
		return err
	}

	err := fixture.paymentSvc.applyAffiliateRebateForOrder(ctx, order)
	require.Error(t, err)
	require.ErrorContains(t, err, "update affiliate rebate applied audit")
	fixture.requireAuditAction(t, order.ID, "AFFILIATE_REBATE_FAILED")
	fixture.requireNoAuditAction(t, order.ID, "AFFILIATE_REBATE_APPLIED")
}

func TestSubscriptionPaymentFulfillment_WitnessRepoErrorFailsSafely(t *testing.T) {
	ctx := context.Background()
	fixture := newPaymentFulfillmentFixture(t)
	order := fixture.createSubscriptionOrder(t, 100, 80)
	fixture.subRepo.getWitnessError = errors.New("subscription witness lookup failed")

	err := fixture.paymentSvc.doSub(ctx, order, &paymentFulfillmentLease{token: "test-lease-token", version: order.UpdatedAt})
	require.ErrorContains(t, err, "check subscription assignment witness")
	require.Equal(t, 0, fixture.subRepo.createCalls)
	require.Equal(t, 0, fixture.affiliateRepo.calls)
}

type paymentFulfillmentFixture struct {
	client        *dbent.Client
	paymentSvc    *PaymentService
	subRepo       *paymentFulfillmentUserSubRepo
	affiliateRepo *paymentFulfillmentAffiliateRepo
	userID        int64
}

func newPaymentFulfillmentFixture(t *testing.T) *paymentFulfillmentFixture {
	t.Helper()
	ctx := context.Background()
	client := newPaymentFulfillmentTestClient(t)
	require.NoError(t, ensurePaymentAuditOrderActionUnique(ctx, client))

	user, err := client.User.Create().
		SetEmail("buyer@example.com").
		SetPasswordHash("hash").
		SetUsername("buyer").
		Save(ctx)
	require.NoError(t, err)

	groupRepo := paymentFulfillmentGroupRepo{
		group: &Group{ID: 2001, Name: "Pro", Status: payment.EntityStatusActive, SubscriptionType: SubscriptionTypeSubscription},
	}
	subRepo := newPaymentFulfillmentUserSubRepo()
	subSvc := NewSubscriptionService(groupRepo, subRepo, nil, nil, nil)

	inviterID := int64(3001)
	settingSvc := NewSettingService(&paymentFulfillmentSettingRepo{values: map[string]string{
		SettingKeyAffiliateEnabled:             "true",
		SettingKeyAffiliateRebateRate:          "100",
		SettingKeyAffiliateRebateFreezeHours:   "0",
		SettingKeyAffiliateRebateDurationDays:  "0",
		SettingKeyAffiliateRebatePerInviteeCap: "0",
	}}, nil)
	affiliateRepo := &paymentFulfillmentAffiliateRepo{
		invitee: &AffiliateSummary{
			UserID:    user.ID,
			InviterID: &inviterID,
			CreatedAt: time.Now().Add(-time.Hour),
		},
		inviter: &AffiliateSummary{
			UserID:    inviterID,
			CreatedAt: time.Now().Add(-time.Hour),
		},
		accrued: make(map[int64]float64),
	}
	affiliateSvc := NewAffiliateService(affiliateRepo, settingSvc, nil, nil)
	paymentSvc := NewPaymentService(client, payment.NewRegistry(), nil, nil, subSvc, nil, nil, groupRepo, affiliateSvc)

	return &paymentFulfillmentFixture{
		client:        client,
		paymentSvc:    paymentSvc,
		subRepo:       subRepo,
		affiliateRepo: affiliateRepo,
		userID:        user.ID,
	}
}

func newPaymentFulfillmentTestClient(t *testing.T) *dbent.Client {
	t.Helper()
	db, err := sql.Open("sqlite", fmt.Sprintf("file:%s?mode=memory&cache=shared&_fk=1", strings.ReplaceAll(t.Name(), "/", "_")))
	require.NoError(t, err)
	t.Cleanup(func() { _ = db.Close() })

	_, err = db.Exec("PRAGMA foreign_keys = ON")
	require.NoError(t, err)

	drv := entsql.OpenDB(dialect.SQLite, db)
	client := enttest.NewClient(t, enttest.WithOptions(dbent.Driver(drv)))
	t.Cleanup(func() { _ = client.Close() })
	return client
}

func ensurePaymentAuditOrderActionUnique(ctx context.Context, client *dbent.Client) error {
	_, err := client.ExecContext(ctx, "CREATE UNIQUE INDEX IF NOT EXISTS idx_payment_audit_logs_order_action_uniq ON payment_audit_logs(order_id, action)")
	return err
}

func (f *paymentFulfillmentFixture) createSubscriptionOrder(t *testing.T, amount, payAmount float64) *dbent.PaymentOrder {
	t.Helper()
	ctx := context.Background()
	order, err := f.client.PaymentOrder.Create().
		SetUserID(f.userID).
		SetUserEmail("buyer@example.com").
		SetUserName("buyer").
		SetAmount(amount).
		SetPayAmount(payAmount).
		SetFeeRate(0).
		SetRechargeCode(fmt.Sprintf("SUB-%d", time.Now().UnixNano())).
		SetOutTradeNo(fmt.Sprintf("sub2_test_%d", time.Now().UnixNano())).
		SetPaymentType(payment.TypeAlipay).
		SetPaymentTradeNo("trade-test").
		SetOrderType(payment.OrderTypeSubscription).
		SetSubscriptionGroupID(2001).
		SetSubscriptionDays(30).
		SetStatus(OrderStatusPaid).
		SetPaidAt(time.Now()).
		SetExpiresAt(time.Now().Add(time.Hour)).
		SetClientIP("127.0.0.1").
		SetSrcHost("api.example.com").
		Save(ctx)
	require.NoError(t, err)
	return order
}

func (f *paymentFulfillmentFixture) requireOrderStatus(t *testing.T, orderID int64, status string) {
	t.Helper()
	order, err := f.client.PaymentOrder.Get(context.Background(), orderID)
	require.NoError(t, err)
	require.Equal(t, status, order.Status)
}

func (f *paymentFulfillmentFixture) requireAuditAction(t *testing.T, orderID int64, action string) {
	t.Helper()
	exists, err := f.client.PaymentAuditLog.Query().
		Where(paymentauditlog.OrderIDEQ(fmt.Sprintf("%d", orderID)), paymentauditlog.ActionEQ(action)).
		Exist(context.Background())
	require.NoError(t, err)
	require.True(t, exists, action)
}

func (f *paymentFulfillmentFixture) requireNoAuditAction(t *testing.T, orderID int64, action string) {
	t.Helper()
	exists, err := f.client.PaymentAuditLog.Query().
		Where(paymentauditlog.OrderIDEQ(fmt.Sprintf("%d", orderID)), paymentauditlog.ActionEQ(action)).
		Exist(context.Background())
	require.NoError(t, err)
	require.False(t, exists, action)
}

func (f *paymentFulfillmentFixture) writeAuditAction(t *testing.T, orderID int64, action string) {
	t.Helper()
	f.paymentSvc.writeAuditLog(context.Background(), orderID, action, "system", map[string]any{
		"test": "stale broad assignment audit without subscription note",
	})
}

func seedExistingSubscription(t *testing.T, repo *paymentFulfillmentUserSubRepo, userID, groupID int64, days int, notes string) {
	t.Helper()
	start := time.Now().Add(-time.Hour)
	repo.seed(&UserSubscription{
		UserID:     userID,
		GroupID:    groupID,
		StartsAt:   start,
		ExpiresAt:  start.AddDate(0, 0, days),
		Status:     SubscriptionStatusActive,
		AssignedAt: start,
		Notes:      notes,
	})
}
