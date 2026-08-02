//go:build integration

package repository

import (
	"context"
	"encoding/json"
	"strconv"
	"testing"
	"time"

	baseent "entgo.io/ent"
	entsql "entgo.io/ent/dialect/sql"
	dbent "github.com/Wei-Shaw/sub2api/ent"
	dbaccount "github.com/Wei-Shaw/sub2api/ent/account"
	"github.com/Wei-Shaw/sub2api/ent/accountgroup"
	"github.com/Wei-Shaw/sub2api/ent/intercept"
	"github.com/Wei-Shaw/sub2api/internal/pkg/pagination"
	"github.com/Wei-Shaw/sub2api/internal/service"
	"github.com/stretchr/testify/suite"
)

type AccountRepoSuite struct {
	suite.Suite
	ctx    context.Context
	client *dbent.Client
	repo   *accountRepository
}

type schedulerCacheRecorder struct {
	setAccounts []*service.Account
	deleteIDs   []int64
	accounts    map[int64]*service.Account
}

func (s *schedulerCacheRecorder) GetSnapshot(ctx context.Context, bucket service.SchedulerBucket) ([]*service.Account, bool, error) {
	return nil, false, nil
}

func (s *schedulerCacheRecorder) SetSnapshot(ctx context.Context, bucket service.SchedulerBucket, accounts []service.Account) error {
	return nil
}

func (s *schedulerCacheRecorder) GetAccount(ctx context.Context, accountID int64) (*service.Account, error) {
	if s.accounts == nil {
		return nil, nil
	}
	return s.accounts[accountID], nil
}

func (s *schedulerCacheRecorder) SetAccount(ctx context.Context, account *service.Account) error {
	s.setAccounts = append(s.setAccounts, account)
	if s.accounts == nil {
		s.accounts = make(map[int64]*service.Account)
	}
	if account != nil {
		s.accounts[account.ID] = account
	}
	return nil
}

func (s *schedulerCacheRecorder) DeleteAccount(ctx context.Context, accountID int64) error {
	s.deleteIDs = append(s.deleteIDs, accountID)
	if s.accounts != nil {
		delete(s.accounts, accountID)
	}
	return nil
}

func (s *schedulerCacheRecorder) UpdateLastUsed(ctx context.Context, updates map[int64]time.Time) error {
	return nil
}

func (s *schedulerCacheRecorder) TryLockBucket(ctx context.Context, bucket service.SchedulerBucket, ttl time.Duration) (string, bool, error) {
	return "test-lock", true, nil
}

func (s *schedulerCacheRecorder) UnlockBucket(ctx context.Context, bucket service.SchedulerBucket, token string) error {
	return nil
}

func (s *schedulerCacheRecorder) ListBuckets(ctx context.Context) ([]service.SchedulerBucket, error) {
	return nil, nil
}

func (s *schedulerCacheRecorder) GetOutboxWatermark(ctx context.Context) (int64, error) {
	return 0, nil
}

func (s *schedulerCacheRecorder) SetOutboxWatermark(ctx context.Context, id int64) error {
	return nil
}

func (s *AccountRepoSuite) SetupTest() {
	s.ctx = context.Background()
	tx := testEntTx(s.T())
	s.client = tx.Client()
	s.repo = newAccountRepositoryWithSQL(s.client, tx, nil)
}

func TestAccountRepoSuite(t *testing.T) {
	suite.Run(t, new(AccountRepoSuite))
}

// --- Create / GetByID / Update / Delete ---

func (s *AccountRepoSuite) TestCreate_RollbackKeepsAccountAndOutboxAtomic() {
	client := testEntClient(s.T())
	repo := newAccountRepositoryWithSQL(client, integrationDB, nil)
	_, err := integrationDB.ExecContext(s.ctx, "TRUNCATE scheduler_outbox")
	s.Require().NoError(err)
	tx, err := client.Tx(s.ctx)
	s.Require().NoError(err)
	txCtx := dbent.NewTxContext(s.ctx, tx)
	account := &service.Account{
		Name:     "create-rollback-" + strconv.FormatInt(time.Now().UnixNano(), 10),
		Platform: service.PlatformOpenAI,
		Type:     service.AccountTypeOAuth,
	}

	s.Require().NoError(repo.Create(txCtx, account))
	s.Require().Positive(account.ID)
	s.Require().NoError(tx.Rollback())

	var accountCount, outboxCount int
	s.Require().NoError(integrationDB.QueryRowContext(s.ctx, "SELECT count(*) FROM accounts WHERE id = $1", account.ID).Scan(&accountCount))
	s.Require().Zero(accountCount)
	s.Require().NoError(integrationDB.QueryRowContext(s.ctx, "SELECT count(*) FROM scheduler_outbox").Scan(&outboxCount))
	s.Require().Zero(outboxCount)
}

func (s *AccountRepoSuite) TestCreate() {
	account := &service.Account{
		Name:        "test-create",
		Platform:    service.PlatformAnthropic,
		Type:        service.AccountTypeOAuth,
		Status:      service.StatusActive,
		Credentials: map[string]any{},
		Extra:       map[string]any{},
		Concurrency: 3,
		Priority:    50,
		Schedulable: true,
	}

	err := s.repo.Create(s.ctx, account)
	s.Require().NoError(err, "Create")
	s.Require().NotZero(account.ID, "expected ID to be set")

	got, err := s.repo.GetByID(s.ctx, account.ID)
	s.Require().NoError(err, "GetByID")
	s.Require().Equal("test-create", got.Name)
}

func (s *AccountRepoSuite) TestGetByID_NotFound() {
	_, err := s.repo.GetByID(s.ctx, 999999)
	s.Require().Error(err, "expected error for non-existent ID")
}

func (s *AccountRepoSuite) TestUpdate() {
	account := mustCreateAccount(s.T(), s.client, &service.Account{Name: "original"})

	account.Name = "updated"
	err := s.repo.Update(s.ctx, account)
	s.Require().NoError(err, "Update")

	got, err := s.repo.GetByID(s.ctx, account.ID)
	s.Require().NoError(err, "GetByID after update")
	s.Require().Equal("updated", got.Name)
}

func (s *AccountRepoSuite) TestUpdate_OpenAIOAuthLikePreservesConcurrentFingerprint() {
	profile := service.ParseOpenAICodexUAProfile(service.DefaultOpenAICodexUserAgent)
	fingerprint, _ := service.NormalizeOpenAICodexFingerprint(nil, profile, time.Now())
	account := mustCreateAccount(s.T(), s.client, &service.Account{
		Name:     "acc-update-fingerprint-race",
		Platform: service.PlatformOpenAI,
		Type:     service.AccountTypeOAuth,
		Extra:    map[string]any{"editable": "stale"},
	})

	stale := *account
	stale.Extra = map[string]any{"editable": "fresh"}
	_, err := s.repo.EnsureOpenAICodexFingerprint(s.ctx, account.ID, fingerprint, nil)
	s.Require().NoError(err)

	s.Require().NoError(s.repo.Update(s.ctx, &stale))

	got, err := s.repo.GetByID(s.ctx, account.ID)
	s.Require().NoError(err)
	s.Require().Equal("fresh", got.Extra["editable"])
	persisted, changed := service.NormalizeOpenAICodexFingerprint(got.Extra[service.OpenAICodexFingerprintExtraKey], profile, time.Now())
	s.Require().False(changed)
	s.Require().Equal(fingerprint.InstallationID, persisted.InstallationID)
}

func (s *AccountRepoSuite) TestUpdate_PreservesConcurrentRuntimeState() {
	account := mustCreateAccount(s.T(), s.client, &service.Account{
		Name:  "acc-update-runtime-race",
		Extra: map[string]any{"editable": "stale"},
	})
	stale := *account
	stale.Name = "updated"
	stale.Extra = map[string]any{
		"editable":                    "fresh",
		"codex_usage_updated_at":      "2025-01-01T00:00:00Z",
		"codex_5h_used_percent":       10.0,
		"unknown_lifecycle_attribute": "preserved",
	}

	observedAt := time.Now().UTC()
	resetAt := observedAt.Add(5 * time.Hour)
	tempUntil := observedAt.Add(10 * time.Minute)
	s.Require().NoError(s.repo.SetRateLimited(s.ctx, account.ID, resetAt))
	s.Require().NoError(s.repo.SetOverloaded(s.ctx, account.ID, observedAt.Add(time.Minute)))
	s.Require().NoError(s.repo.SetTempUnschedulable(s.ctx, account.ID, tempUntil, "runtime penalty"))
	s.Require().NoError(s.repo.UpdateSessionWindow(
		s.ctx,
		account.ID,
		&observedAt,
		&resetAt,
		"allowed_warning",
	))
	updated, err := s.repo.UpdateRuntimeExtra(s.ctx, account.ID, map[string]any{
		"codex_5h_used_percent": 75.0,
	}, "codex_usage_updated_at", observedAt)
	s.Require().NoError(err)
	s.Require().True(updated)

	s.Require().NoError(s.repo.Update(s.ctx, &stale))

	got, err := s.repo.GetByID(s.ctx, account.ID)
	s.Require().NoError(err)
	s.Require().Equal("updated", got.Name)
	s.Require().Equal("fresh", got.Extra["editable"])
	s.Require().Equal("preserved", got.Extra["unknown_lifecycle_attribute"])
	s.Require().Equal(75.0, got.Extra["codex_5h_used_percent"])
	s.Require().Equal(observedAt.Format(time.RFC3339Nano), got.Extra["codex_usage_updated_at"])
	s.Require().NotNil(got.RateLimitedAt)
	s.Require().WithinDuration(resetAt, *got.RateLimitResetAt, time.Microsecond)
	s.Require().WithinDuration(observedAt.Add(time.Minute), *got.OverloadUntil, time.Microsecond)
	s.Require().NotNil(got.TempUnschedulableUntil)
	s.Require().WithinDuration(tempUntil, *got.TempUnschedulableUntil, time.Microsecond)
	s.Require().Equal("runtime penalty", got.TempUnschedulableReason)
	s.Require().WithinDuration(observedAt, *got.SessionWindowStart, time.Microsecond)
	s.Require().WithinDuration(resetAt, *got.SessionWindowEnd, time.Microsecond)
	s.Require().Equal("allowed_warning", got.SessionWindowStatus)
}

func (s *AccountRepoSuite) TestUpdate_PreservesConcurrentLastUsed() {
	account := mustCreateAccount(s.T(), s.client, &service.Account{
		Name: "acc-update-last-used-race",
	})
	stale := *account
	stale.Name = "updated"

	s.Require().NoError(s.repo.UpdateLastUsed(s.ctx, account.ID))
	stored, err := s.repo.GetByID(s.ctx, account.ID)
	s.Require().NoError(err)
	s.Require().NotNil(stored.LastUsedAt)

	s.Require().NoError(s.repo.Update(s.ctx, &stale))

	got, err := s.repo.GetByID(s.ctx, account.ID)
	s.Require().NoError(err)
	s.Require().Equal("updated", got.Name)
	s.Require().NotNil(got.LastUsedAt)
	s.Require().WithinDuration(*stored.LastUsedAt, *got.LastUsedAt, time.Microsecond)
}

func (s *AccountRepoSuite) TestUpdate_SyncSchedulerSnapshotOnDisabled() {
	account := mustCreateAccount(s.T(), s.client, &service.Account{Name: "sync-update", Status: service.StatusActive, Schedulable: true})
	cacheRecorder := &schedulerCacheRecorder{}
	s.repo.schedulerCache = cacheRecorder

	account.Status = service.StatusDisabled
	err := s.repo.Update(s.ctx, account)
	s.Require().NoError(err, "Update")

	s.Require().Len(cacheRecorder.setAccounts, 1)
	s.Require().Equal(account.ID, cacheRecorder.setAccounts[0].ID)
	s.Require().Equal(service.StatusDisabled, cacheRecorder.setAccounts[0].Status)
}

func (s *AccountRepoSuite) TestUpdate_PublishesOnlyAfterCommit() {
	client := testEntClient(s.T())
	account := mustCreateAccount(s.T(), client, &service.Account{
		Name:   "update-transaction-" + strconv.FormatInt(time.Now().UnixNano(), 10),
		Status: service.StatusActive,
	})
	s.T().Cleanup(func() { _, _ = client.Account.Delete().Where(dbaccount.IDEQ(account.ID)).Exec(context.Background()) })
	cacheRecorder := &schedulerCacheRecorder{}
	repo := newAccountRepositoryWithSQL(client, integrationDB, cacheRecorder)
	tx, err := client.Tx(s.ctx)
	s.Require().NoError(err)
	txCtx := dbent.NewTxContext(s.ctx, tx)
	account.Status = service.StatusDisabled

	s.Require().NoError(repo.Update(txCtx, account))
	s.Require().Empty(cacheRecorder.setAccounts)
	s.Require().NoError(tx.Commit())

	s.Require().Len(cacheRecorder.setAccounts, 1)
	s.Require().Equal(service.StatusDisabled, cacheRecorder.setAccounts[0].Status)
}

func (s *AccountRepoSuite) TestUpdate_RollbackKeepsOutboxAtomic() {
	client := testEntClient(s.T())
	account := mustCreateAccount(s.T(), client, &service.Account{
		Name:   "update-outbox-rollback-" + strconv.FormatInt(time.Now().UnixNano(), 10),
		Status: service.StatusActive,
	})
	s.T().Cleanup(func() { _, _ = client.Account.Delete().Where(dbaccount.IDEQ(account.ID)).Exec(context.Background()) })
	repo := newAccountRepositoryWithSQL(client, integrationDB, &schedulerCacheRecorder{})
	_, err := integrationDB.ExecContext(s.ctx, "TRUNCATE scheduler_outbox")
	s.Require().NoError(err)
	tx, err := client.Tx(s.ctx)
	s.Require().NoError(err)
	txCtx := dbent.NewTxContext(s.ctx, tx)
	account.Status = service.StatusDisabled

	s.Require().NoError(repo.Update(txCtx, account))
	s.Require().NoError(tx.Rollback())

	var outboxCount int
	s.Require().NoError(integrationDB.QueryRowContext(s.ctx, "SELECT count(*) FROM scheduler_outbox").Scan(&outboxCount))
	s.Require().Zero(outboxCount)
}

func (s *AccountRepoSuite) TestUpdate_SyncSchedulerSnapshotOnCredentialsChange() {
	account := mustCreateAccount(s.T(), s.client, &service.Account{
		Name:        "sync-credentials-update",
		Status:      service.StatusActive,
		Schedulable: true,
		Credentials: map[string]any{
			"model_mapping": map[string]any{
				"gpt-5": "gpt-5.1",
			},
		},
	})
	cacheRecorder := &schedulerCacheRecorder{}
	s.repo.schedulerCache = cacheRecorder

	account.Credentials = map[string]any{
		"model_mapping": map[string]any{
			"gpt-5": "gpt-5.2",
		},
	}
	err := s.repo.Update(s.ctx, account)
	s.Require().NoError(err, "Update")

	s.Require().Len(cacheRecorder.setAccounts, 1)
	s.Require().Equal(account.ID, cacheRecorder.setAccounts[0].ID)
	mapping, ok := cacheRecorder.setAccounts[0].Credentials["model_mapping"].(map[string]any)
	s.Require().True(ok)
	s.Require().Equal("gpt-5.2", mapping["gpt-5"])
}

func (s *AccountRepoSuite) TestDelete() {
	account := mustCreateAccount(s.T(), s.client, &service.Account{Name: "to-delete"})

	err := s.repo.Delete(s.ctx, account.ID)
	s.Require().NoError(err, "Delete")

	_, err = s.repo.GetByID(s.ctx, account.ID)
	s.Require().Error(err, "expected error after delete")
}

func (s *AccountRepoSuite) TestDelete_RemovesSchedulerAccountSnapshot() {
	account := mustCreateAccount(s.T(), s.client, &service.Account{Name: "to-delete-cache"})
	cacheRecorder := &schedulerCacheRecorder{
		accounts: map[int64]*service.Account{
			account.ID: {
				ID:          account.ID,
				Name:        account.Name,
				Status:      service.StatusActive,
				Schedulable: true,
			},
		},
	}
	s.repo.schedulerCache = cacheRecorder

	err := s.repo.Delete(s.ctx, account.ID)
	s.Require().NoError(err, "Delete")

	s.Require().Equal([]int64{account.ID}, cacheRecorder.deleteIDs)
	s.Require().NotContains(cacheRecorder.accounts, account.ID)
}

func (s *AccountRepoSuite) TestDelete_RollbackKeepsAccountOutboxAndCacheAtomic() {
	client := testEntClient(s.T())
	account := mustCreateAccount(s.T(), client, &service.Account{Name: "delete-rollback-" + strconv.FormatInt(time.Now().UnixNano(), 10)})
	s.T().Cleanup(func() { _, _ = client.Account.Delete().Where(dbaccount.IDEQ(account.ID)).Exec(context.Background()) })
	cacheRecorder := &schedulerCacheRecorder{accounts: map[int64]*service.Account{account.ID: account}}
	repo := newAccountRepositoryWithSQL(client, integrationDB, cacheRecorder)
	_, err := integrationDB.ExecContext(s.ctx, "TRUNCATE scheduler_outbox")
	s.Require().NoError(err)
	tx, err := client.Tx(s.ctx)
	s.Require().NoError(err)
	txCtx := dbent.NewTxContext(s.ctx, tx)

	s.Require().NoError(repo.Delete(txCtx, account.ID))
	s.Require().Empty(cacheRecorder.deleteIDs)
	s.Require().NoError(tx.Rollback())

	got, err := repo.GetByID(s.ctx, account.ID)
	s.Require().NoError(err)
	s.Require().Equal(account.ID, got.ID)
	s.Require().Contains(cacheRecorder.accounts, account.ID)
	var outboxCount int
	s.Require().NoError(integrationDB.QueryRowContext(s.ctx, "SELECT count(*) FROM scheduler_outbox").Scan(&outboxCount))
	s.Require().Zero(outboxCount)
}

func (s *AccountRepoSuite) TestDelete_WithGroupBindings() {
	group := mustCreateGroup(s.T(), s.client, &service.Group{Name: "g-del"})
	account := mustCreateAccount(s.T(), s.client, &service.Account{Name: "acc-del"})
	mustBindAccountToGroup(s.T(), s.client, account.ID, group.ID, 1)

	err := s.repo.Delete(s.ctx, account.ID)
	s.Require().NoError(err, "Delete should cascade remove bindings")

	count, err := s.client.AccountGroup.Query().Where(accountgroup.AccountIDEQ(account.ID)).Count(s.ctx)
	s.Require().NoError(err)
	s.Require().Zero(count, "expected bindings to be removed")
}

// --- List / ListWithFilters ---

func (s *AccountRepoSuite) TestList() {
	mustCreateAccount(s.T(), s.client, &service.Account{Name: "acc1"})
	mustCreateAccount(s.T(), s.client, &service.Account{Name: "acc2"})

	accounts, page, err := s.repo.List(s.ctx, pagination.PaginationParams{Page: 1, PageSize: 10})
	s.Require().NoError(err, "List")
	s.Require().Len(accounts, 2)
	s.Require().Equal(int64(2), page.Total)
}

func (s *AccountRepoSuite) TestListWithFilters_PaginationCountCloneDoesNotMutateListQuery() {
	for _, account := range []service.Account{
		{Name: "clone-page-1", Platform: service.PlatformOpenAI, Priority: 10},
		{Name: "clone-page-2", Platform: service.PlatformOpenAI, Priority: 20},
		{Name: "clone-page-3", Platform: service.PlatformOpenAI, Priority: 30},
		{Name: "clone-page-4", Platform: service.PlatformOpenAI, Priority: 40},
		{Name: "clone-page-other", Platform: service.PlatformAnthropic, Priority: 100},
	} {
		account := account
		mustCreateAccount(s.T(), s.client, &account)
	}

	s.client.Account.Intercept(intercept.TraverseFunc(func(ctx context.Context, q intercept.Query) error {
		qc := baseent.QueryFromContext(ctx)
		if qc != nil && qc.Op == baseent.OpQueryCount && q.Type() == dbent.TypeAccount {
			q.WhereP(func(s *entsql.Selector) {
				s.Select(s.C("id"))
			})
		}
		return nil
	}))

	accounts, page, err := s.repo.ListWithFilters(s.ctx, pagination.PaginationParams{
		Page:      2,
		PageSize:  2,
		SortBy:    "priority",
		SortOrder: "asc",
	}, service.AccountListFilters{
		Platform: service.PlatformOpenAI,
	})

	s.Require().NoError(err, "ListWithFilters")
	s.Require().Equal(int64(4), page.Total)
	s.Require().Equal(2, page.Page)
	s.Require().Equal(2, page.PageSize)
	s.Require().Equal(2, page.Pages)
	s.Require().Len(accounts, 2)
	s.Require().Equal("clone-page-3", accounts[0].Name)
	s.Require().Equal("clone-page-4", accounts[1].Name)
}

func (s *AccountRepoSuite) TestListWithFilters() {
	tests := []struct {
		name        string
		setup       func(client *dbent.Client)
		platform    string
		accType     string
		status      string
		search      string
		groupID     int64
		privacyMode string
		wantCount   int
		validate    func(accounts []service.Account)
	}{
		{
			name: "filter_by_platform",
			setup: func(client *dbent.Client) {
				mustCreateAccount(s.T(), client, &service.Account{Name: "a1", Platform: service.PlatformAnthropic})
				mustCreateAccount(s.T(), client, &service.Account{Name: "a2", Platform: service.PlatformOpenAI})
			},
			platform:  service.PlatformOpenAI,
			wantCount: 1,
			validate: func(accounts []service.Account) {
				s.Require().Equal(service.PlatformOpenAI, accounts[0].Platform)
			},
		},
		{
			name: "filter_by_type",
			setup: func(client *dbent.Client) {
				mustCreateAccount(s.T(), client, &service.Account{Name: "t1", Type: service.AccountTypeOAuth})
				mustCreateAccount(s.T(), client, &service.Account{Name: "t2", Type: service.AccountTypeAPIKey})
			},
			accType:   service.AccountTypeAPIKey,
			wantCount: 1,
			validate: func(accounts []service.Account) {
				s.Require().Equal(service.AccountTypeAPIKey, accounts[0].Type)
			},
		},
		{
			name: "filter_by_status",
			setup: func(client *dbent.Client) {
				mustCreateAccount(s.T(), client, &service.Account{Name: "s1", Status: service.StatusActive})
				mustCreateAccount(s.T(), client, &service.Account{Name: "s2", Status: service.StatusDisabled})
			},
			status:    service.StatusDisabled,
			wantCount: 1,
			validate: func(accounts []service.Account) {
				s.Require().Equal(service.StatusDisabled, accounts[0].Status)
			},
		},
		{
			name: "filter_by_status_active_excludes_runtime_blocked_accounts",
			setup: func(client *dbent.Client) {
				mustCreateAccount(s.T(), client, &service.Account{Name: "active-normal", Status: service.StatusActive})
				rateLimited := mustCreateAccount(s.T(), client, &service.Account{Name: "active-rate-limited", Status: service.StatusActive})
				err := client.Account.UpdateOneID(rateLimited.ID).
					SetRateLimitResetAt(time.Now().Add(10 * time.Minute)).
					Exec(context.Background())
				s.Require().NoError(err)
				tempUnsched := mustCreateAccount(s.T(), client, &service.Account{Name: "active-temp-unsched", Status: service.StatusActive})
				err = client.Account.UpdateOneID(tempUnsched.ID).
					SetTempUnschedulableUntil(time.Now().Add(15 * time.Minute)).
					Exec(context.Background())
				s.Require().NoError(err)
				unsched := mustCreateAccount(s.T(), client, &service.Account{Name: "active-unsched", Status: service.StatusActive})
				err = client.Account.UpdateOneID(unsched.ID).
					SetSchedulable(false).
					Exec(context.Background())
				s.Require().NoError(err)
			},
			status:    service.StatusActive,
			wantCount: 1,
			validate: func(accounts []service.Account) {
				s.Require().Equal("active-normal", accounts[0].Name)
			},
		},
		{
			name: "filter_by_status_unschedulable_excludes_rate_limited_and_temp_unschedulable",
			setup: func(client *dbent.Client) {
				mustCreateAccount(s.T(), client, &service.Account{Name: "active-normal", Status: service.StatusActive, Schedulable: true})
				unsched := mustCreateAccount(s.T(), client, &service.Account{Name: "active-unsched", Status: service.StatusActive})
				err := client.Account.UpdateOneID(unsched.ID).
					SetSchedulable(false).
					Exec(context.Background())
				s.Require().NoError(err)
				rateLimited := mustCreateAccount(s.T(), client, &service.Account{Name: "active-rate-limited", Status: service.StatusActive})
				err = client.Account.UpdateOneID(rateLimited.ID).
					SetSchedulable(false).
					SetRateLimitResetAt(time.Now().Add(10 * time.Minute)).
					Exec(context.Background())
				s.Require().NoError(err)
				tempUnsched := mustCreateAccount(s.T(), client, &service.Account{Name: "active-temp-unsched", Status: service.StatusActive})
				err = client.Account.UpdateOneID(tempUnsched.ID).
					SetSchedulable(false).
					SetTempUnschedulableUntil(time.Now().Add(15 * time.Minute)).
					Exec(context.Background())
				s.Require().NoError(err)
			},
			status:    "unschedulable",
			wantCount: 1,
			validate: func(accounts []service.Account) {
				s.Require().Equal("active-unsched", accounts[0].Name)
			},
		},
		{
			name: "filter_by_status_rate_limited_excludes_temp_unschedulable",
			setup: func(client *dbent.Client) {
				rateLimited := mustCreateAccount(s.T(), client, &service.Account{Name: "active-rate-limited", Status: service.StatusActive})
				err := client.Account.UpdateOneID(rateLimited.ID).
					SetRateLimitResetAt(time.Now().Add(10 * time.Minute)).
					Exec(context.Background())
				s.Require().NoError(err)
				tempUnsched := mustCreateAccount(s.T(), client, &service.Account{Name: "active-temp-unsched", Status: service.StatusActive})
				err = client.Account.UpdateOneID(tempUnsched.ID).
					SetRateLimitResetAt(time.Now().Add(20 * time.Minute)).
					SetTempUnschedulableUntil(time.Now().Add(15 * time.Minute)).
					Exec(context.Background())
				s.Require().NoError(err)
			},
			status:    "rate_limited",
			wantCount: 1,
			validate: func(accounts []service.Account) {
				s.Require().Equal("active-rate-limited", accounts[0].Name)
			},
		},
		{
			name: "filter_by_status_temp_unschedulable_excludes_manually_unschedulable",
			setup: func(client *dbent.Client) {
				tempUnsched := mustCreateAccount(s.T(), client, &service.Account{Name: "active-temp-unsched", Status: service.StatusActive, Schedulable: true})
				err := client.Account.UpdateOneID(tempUnsched.ID).
					SetTempUnschedulableUntil(time.Now().Add(15 * time.Minute)).
					Exec(context.Background())
				s.Require().NoError(err)
				unsched := mustCreateAccount(s.T(), client, &service.Account{Name: "active-unsched", Status: service.StatusActive})
				err = client.Account.UpdateOneID(unsched.ID).
					SetSchedulable(false).
					Exec(context.Background())
				s.Require().NoError(err)
			},
			status:    "temp_unschedulable",
			wantCount: 1,
			validate: func(accounts []service.Account) {
				s.Require().Equal("active-temp-unsched", accounts[0].Name)
			},
		},
		{
			name: "filter_by_search",
			setup: func(client *dbent.Client) {
				mustCreateAccount(s.T(), client, &service.Account{Name: "alpha-account"})
				mustCreateAccount(s.T(), client, &service.Account{Name: "beta-account"})
			},
			search:    "alpha",
			wantCount: 1,
			validate: func(accounts []service.Account) {
				s.Require().Contains(accounts[0].Name, "alpha")
			},
		},
		{
			name: "filter_by_ungrouped",
			setup: func(client *dbent.Client) {
				group := mustCreateGroup(s.T(), client, &service.Group{Name: "g-ungrouped"})
				grouped := mustCreateAccount(s.T(), client, &service.Account{Name: "grouped-account"})
				mustCreateAccount(s.T(), client, &service.Account{Name: "ungrouped-account"})
				mustBindAccountToGroup(s.T(), client, grouped.ID, group.ID, 1)
			},
			groupID:   service.AccountListGroupUngrouped,
			wantCount: 1,
			validate: func(accounts []service.Account) {
				s.Require().Equal("ungrouped-account", accounts[0].Name)
				s.Require().Empty(accounts[0].GroupIDs)
			},
		},
		{
			name: "filter_by_privacy_mode",
			setup: func(client *dbent.Client) {
				mustCreateAccount(s.T(), client, &service.Account{Name: "privacy-ok", Extra: map[string]any{"privacy_mode": service.PrivacyModeTrainingOff}})
				mustCreateAccount(s.T(), client, &service.Account{Name: "privacy-fail", Extra: map[string]any{"privacy_mode": service.PrivacyModeFailed}})
			},
			privacyMode: service.PrivacyModeTrainingOff,
			wantCount:   1,
			validate: func(accounts []service.Account) {
				s.Require().Equal("privacy-ok", accounts[0].Name)
			},
		},
		{
			name: "filter_by_privacy_mode_unset",
			setup: func(client *dbent.Client) {
				mustCreateAccount(s.T(), client, &service.Account{Name: "privacy-unset", Extra: nil})
				mustCreateAccount(s.T(), client, &service.Account{Name: "privacy-empty", Extra: map[string]any{"privacy_mode": ""}})
				mustCreateAccount(s.T(), client, &service.Account{Name: "privacy-set", Extra: map[string]any{"privacy_mode": service.PrivacyModeTrainingOff}})
			},
			privacyMode: service.AccountPrivacyModeUnsetFilter,
			wantCount:   2,
			validate: func(accounts []service.Account) {
				names := []string{accounts[0].Name, accounts[1].Name}
				s.ElementsMatch([]string{"privacy-unset", "privacy-empty"}, names)
			},
		},
	}

	for _, tt := range tests {
		s.Run(tt.name, func() {
			// 每个 case 重新获取隔离资源
			tx := testEntTx(s.T())
			client := tx.Client()
			repo := newAccountRepositoryWithSQL(client, tx, nil)
			ctx := context.Background()

			tt.setup(client)

			accounts, _, err := repo.ListWithFilters(ctx, pagination.PaginationParams{Page: 1, PageSize: 10}, service.AccountListFilters{
				Platform:    tt.platform,
				AccountType: tt.accType,
				Status:      tt.status,
				Search:      tt.search,
				GroupID:     tt.groupID,
				PrivacyMode: tt.privacyMode,
			})
			s.Require().NoError(err)
			s.Require().Len(accounts, tt.wantCount)
			if tt.validate != nil {
				tt.validate(accounts)
			}
		})
	}
}

// --- ListByGroup / ListActive / ListByPlatform ---

func (s *AccountRepoSuite) TestListByGroup() {
	group := mustCreateGroup(s.T(), s.client, &service.Group{Name: "g-list"})
	acc1 := mustCreateAccount(s.T(), s.client, &service.Account{Name: "a1", Status: service.StatusActive})
	acc2 := mustCreateAccount(s.T(), s.client, &service.Account{Name: "a2", Status: service.StatusActive})
	mustBindAccountToGroup(s.T(), s.client, acc1.ID, group.ID, 2)
	mustBindAccountToGroup(s.T(), s.client, acc2.ID, group.ID, 1)

	accounts, err := s.repo.ListByGroup(s.ctx, group.ID)
	s.Require().NoError(err, "ListByGroup")
	s.Require().Len(accounts, 2)
	// Should be ordered by priority
	s.Require().Equal(acc2.ID, accounts[0].ID, "expected acc2 first (priority=1)")
}

func (s *AccountRepoSuite) TestListActive() {
	mustCreateAccount(s.T(), s.client, &service.Account{Name: "active1", Status: service.StatusActive})
	mustCreateAccount(s.T(), s.client, &service.Account{Name: "inactive1", Status: service.StatusDisabled})

	accounts, err := s.repo.ListActive(s.ctx)
	s.Require().NoError(err, "ListActive")
	s.Require().Len(accounts, 1)
	s.Require().Equal("active1", accounts[0].Name)
}

func (s *AccountRepoSuite) TestListByPlatform() {
	mustCreateAccount(s.T(), s.client, &service.Account{Name: "p1", Platform: service.PlatformAnthropic, Status: service.StatusActive})
	mustCreateAccount(s.T(), s.client, &service.Account{Name: "p2", Platform: service.PlatformOpenAI, Status: service.StatusActive})

	accounts, err := s.repo.ListByPlatform(s.ctx, service.PlatformAnthropic)
	s.Require().NoError(err, "ListByPlatform")
	s.Require().Len(accounts, 1)
	s.Require().Equal(service.PlatformAnthropic, accounts[0].Platform)
}

func (s *AccountRepoSuite) TestListByPlatformForValidationIncludesDisabledAndExcludesDeleted() {
	disabled := mustCreateAccount(s.T(), s.client, &service.Account{Name: "openai-disabled", Platform: service.PlatformOpenAI, Type: service.AccountTypeOAuth, Status: service.StatusDisabled})
	mustCreateAccount(s.T(), s.client, &service.Account{Name: "openai-active", Platform: service.PlatformOpenAI, Type: service.AccountTypeOAuth, Status: service.StatusActive})
	deleted := mustCreateAccount(s.T(), s.client, &service.Account{Name: "openai-deleted", Platform: service.PlatformOpenAI, Type: service.AccountTypeOAuth, Status: service.StatusError})
	mustCreateAccount(s.T(), s.client, &service.Account{Name: "anthropic-disabled", Platform: service.PlatformAnthropic, Status: service.StatusDisabled})
	s.Require().NoError(s.repo.Delete(s.ctx, deleted.ID), "delete account")

	accounts, err := s.repo.ListByPlatformForValidation(s.ctx, service.PlatformOpenAI)

	s.Require().NoError(err, "ListByPlatformForValidation")
	s.Require().Len(accounts, 2)
	ids := make([]int64, 0, len(accounts))
	for _, account := range accounts {
		s.Require().Equal(service.PlatformOpenAI, account.Platform)
		ids = append(ids, account.ID)
	}
	s.Require().Contains(ids, disabled.ID)
	s.Require().NotContains(ids, deleted.ID)
}

// --- Preload and VirtualFields ---

func (s *AccountRepoSuite) TestPreload_And_VirtualFields() {
	proxy := mustCreateProxy(s.T(), s.client, &service.Proxy{Name: "p1"})
	group := mustCreateGroup(s.T(), s.client, &service.Group{Name: "g1"})

	account := mustCreateAccount(s.T(), s.client, &service.Account{
		Name:    "acc1",
		ProxyID: &proxy.ID,
	})
	mustBindAccountToGroup(s.T(), s.client, account.ID, group.ID, 1)

	got, err := s.repo.GetByID(s.ctx, account.ID)
	s.Require().NoError(err, "GetByID")
	s.Require().NotNil(got.Proxy, "expected Proxy preload")
	s.Require().Equal(proxy.ID, got.Proxy.ID)
	s.Require().Len(got.GroupIDs, 1, "expected GroupIDs to be populated")
	s.Require().Equal(group.ID, got.GroupIDs[0])
	s.Require().Len(got.Groups, 1, "expected Groups to be populated")
	s.Require().Equal(group.ID, got.Groups[0].ID)

	accounts, page, err := s.repo.ListWithFilters(s.ctx, pagination.PaginationParams{Page: 1, PageSize: 10}, service.AccountListFilters{Search: "acc"})
	s.Require().NoError(err, "ListWithFilters")
	s.Require().Equal(int64(1), page.Total)
	s.Require().Len(accounts, 1)
	s.Require().NotNil(accounts[0].Proxy, "expected Proxy preload in list")
	s.Require().Equal(proxy.ID, accounts[0].Proxy.ID)
	s.Require().Len(accounts[0].GroupIDs, 1, "expected GroupIDs in list")
	s.Require().Equal(group.ID, accounts[0].GroupIDs[0])
}

// --- GroupBinding / AddToGroup / RemoveFromGroup / BindGroups / GetGroups ---

func (s *AccountRepoSuite) TestGroupBinding_And_BindGroups() {
	g1 := mustCreateGroup(s.T(), s.client, &service.Group{Name: "g1"})
	g2 := mustCreateGroup(s.T(), s.client, &service.Group{Name: "g2"})
	account := mustCreateAccount(s.T(), s.client, &service.Account{Name: "acc"})

	s.Require().NoError(s.repo.AddToGroup(s.ctx, account.ID, g1.ID, 10), "AddToGroup")
	groups, err := s.repo.GetGroups(s.ctx, account.ID)
	s.Require().NoError(err, "GetGroups")
	s.Require().Len(groups, 1, "expected 1 group")
	s.Require().Equal(g1.ID, groups[0].ID)

	s.Require().NoError(s.repo.RemoveFromGroup(s.ctx, account.ID, g1.ID), "RemoveFromGroup")
	groups, err = s.repo.GetGroups(s.ctx, account.ID)
	s.Require().NoError(err, "GetGroups after remove")
	s.Require().Empty(groups, "expected 0 groups after remove")

	s.Require().NoError(s.repo.BindGroups(s.ctx, account.ID, []int64{g1.ID, g2.ID}), "BindGroups")
	groups, err = s.repo.GetGroups(s.ctx, account.ID)
	s.Require().NoError(err, "GetGroups after bind")
	s.Require().Len(groups, 2, "expected 2 groups after bind")
}

func (s *AccountRepoSuite) TestAddToGroup_RollbackKeepsMembershipAndOutboxAtomic() {
	client := testEntClient(s.T())
	group := mustCreateGroup(s.T(), client, &service.Group{Name: "group-rollback-" + strconv.FormatInt(time.Now().UnixNano(), 10)})
	account := mustCreateAccount(s.T(), client, &service.Account{Name: "membership-rollback-" + strconv.FormatInt(time.Now().UnixNano(), 10)})
	s.T().Cleanup(func() {
		_, _ = client.Account.Delete().Where(dbaccount.IDEQ(account.ID)).Exec(context.Background())
		_, _ = integrationDB.ExecContext(context.Background(), "DELETE FROM groups WHERE id = $1", group.ID)
	})
	repo := newAccountRepositoryWithSQL(client, integrationDB, nil)
	_, err := integrationDB.ExecContext(s.ctx, "TRUNCATE scheduler_outbox")
	s.Require().NoError(err)
	tx, err := client.Tx(s.ctx)
	s.Require().NoError(err)
	txCtx := dbent.NewTxContext(s.ctx, tx)

	s.Require().NoError(repo.AddToGroup(txCtx, account.ID, group.ID, 1))
	s.Require().NoError(tx.Rollback())

	groups, err := repo.GetGroups(s.ctx, account.ID)
	s.Require().NoError(err)
	s.Require().Empty(groups)
	var outboxCount int
	s.Require().NoError(integrationDB.QueryRowContext(s.ctx, "SELECT count(*) FROM scheduler_outbox").Scan(&outboxCount))
	s.Require().Zero(outboxCount)
}

func (s *AccountRepoSuite) TestBindGroups_RollbackKeepsReplacementAndOutboxAtomic() {
	client := testEntClient(s.T())
	oldGroup := mustCreateGroup(s.T(), client, &service.Group{Name: "bind-old-" + strconv.FormatInt(time.Now().UnixNano(), 10)})
	newGroup := mustCreateGroup(s.T(), client, &service.Group{Name: "bind-new-" + strconv.FormatInt(time.Now().UnixNano(), 10)})
	account := mustCreateAccount(s.T(), client, &service.Account{Name: "bind-rollback-" + strconv.FormatInt(time.Now().UnixNano(), 10)})
	mustBindAccountToGroup(s.T(), client, account.ID, oldGroup.ID, 1)
	s.T().Cleanup(func() {
		_, _ = client.Account.Delete().Where(dbaccount.IDEQ(account.ID)).Exec(context.Background())
		_, _ = integrationDB.ExecContext(context.Background(), "DELETE FROM groups WHERE id = ANY($1)", []int64{oldGroup.ID, newGroup.ID})
	})
	repo := newAccountRepositoryWithSQL(client, integrationDB, nil)
	_, err := integrationDB.ExecContext(s.ctx, "TRUNCATE scheduler_outbox")
	s.Require().NoError(err)
	tx, err := client.Tx(s.ctx)
	s.Require().NoError(err)
	txCtx := dbent.NewTxContext(s.ctx, tx)

	s.Require().NoError(repo.BindGroups(txCtx, account.ID, []int64{newGroup.ID}))
	s.Require().NoError(tx.Rollback())

	groups, err := repo.GetGroups(s.ctx, account.ID)
	s.Require().NoError(err)
	s.Require().Len(groups, 1)
	s.Require().Equal(oldGroup.ID, groups[0].ID)
	var outboxCount int
	s.Require().NoError(integrationDB.QueryRowContext(s.ctx, "SELECT count(*) FROM scheduler_outbox").Scan(&outboxCount))
	s.Require().Zero(outboxCount)
}

func (s *AccountRepoSuite) TestBindGroups_EmptyList() {
	account := mustCreateAccount(s.T(), s.client, &service.Account{Name: "acc-empty"})
	group := mustCreateGroup(s.T(), s.client, &service.Group{Name: "g-empty"})
	mustBindAccountToGroup(s.T(), s.client, account.ID, group.ID, 1)

	s.Require().NoError(s.repo.BindGroups(s.ctx, account.ID, []int64{}), "BindGroups empty")

	groups, err := s.repo.GetGroups(s.ctx, account.ID)
	s.Require().NoError(err)
	s.Require().Empty(groups, "expected 0 groups after binding empty list")
}

// --- Schedulable ---

func (s *AccountRepoSuite) TestListSchedulable() {
	now := time.Now()
	group := mustCreateGroup(s.T(), s.client, &service.Group{Name: "g-sched"})

	okAcc := mustCreateAccount(s.T(), s.client, &service.Account{Name: "ok", Schedulable: true})
	mustBindAccountToGroup(s.T(), s.client, okAcc.ID, group.ID, 1)

	future := now.Add(10 * time.Minute)
	overloaded := mustCreateAccount(s.T(), s.client, &service.Account{Name: "over", Schedulable: true, OverloadUntil: &future})
	mustBindAccountToGroup(s.T(), s.client, overloaded.ID, group.ID, 1)

	sched, err := s.repo.ListSchedulable(s.ctx)
	s.Require().NoError(err, "ListSchedulable")
	ids := idsOfAccounts(sched)
	s.Require().Contains(ids, okAcc.ID)
	s.Require().NotContains(ids, overloaded.ID)
}

func (s *AccountRepoSuite) TestListSchedulableByGroupID_TimeBoundaries_And_StatusUpdates() {
	now := time.Now()
	group := mustCreateGroup(s.T(), s.client, &service.Group{Name: "g-sched"})

	okAcc := mustCreateAccount(s.T(), s.client, &service.Account{Name: "ok", Schedulable: true})
	mustBindAccountToGroup(s.T(), s.client, okAcc.ID, group.ID, 1)

	future := now.Add(10 * time.Minute)
	overloaded := mustCreateAccount(s.T(), s.client, &service.Account{Name: "over", Schedulable: true, OverloadUntil: &future})
	mustBindAccountToGroup(s.T(), s.client, overloaded.ID, group.ID, 1)

	rateLimited := mustCreateAccount(s.T(), s.client, &service.Account{Name: "rl", Schedulable: true})
	mustBindAccountToGroup(s.T(), s.client, rateLimited.ID, group.ID, 1)
	s.Require().NoError(s.repo.SetRateLimited(s.ctx, rateLimited.ID, now.Add(10*time.Minute)), "SetRateLimited")

	s.Require().NoError(s.repo.SetError(s.ctx, overloaded.ID, "boom"), "SetError")

	sched, err := s.repo.ListSchedulableByGroupID(s.ctx, group.ID)
	s.Require().NoError(err, "ListSchedulableByGroupID")
	s.Require().Len(sched, 1, "expected only ok account schedulable")
	s.Require().Equal(okAcc.ID, sched[0].ID)

	s.Require().NoError(s.repo.ClearRateLimit(s.ctx, rateLimited.ID), "ClearRateLimit")
	sched2, err := s.repo.ListSchedulableByGroupID(s.ctx, group.ID)
	s.Require().NoError(err, "ListSchedulableByGroupID after ClearRateLimit")
	s.Require().Len(sched2, 2, "expected 2 schedulable accounts after ClearRateLimit")
}

func (s *AccountRepoSuite) TestListSchedulableByPlatform() {
	mustCreateAccount(s.T(), s.client, &service.Account{Name: "a1", Platform: service.PlatformAnthropic, Schedulable: true})
	mustCreateAccount(s.T(), s.client, &service.Account{Name: "a2", Platform: service.PlatformOpenAI, Schedulable: true})

	accounts, err := s.repo.ListSchedulableByPlatform(s.ctx, service.PlatformAnthropic)
	s.Require().NoError(err)
	s.Require().Len(accounts, 1)
	s.Require().Equal(service.PlatformAnthropic, accounts[0].Platform)
}

func (s *AccountRepoSuite) TestListSchedulableByGroupIDAndPlatform() {
	group := mustCreateGroup(s.T(), s.client, &service.Group{Name: "g-sp"})
	a1 := mustCreateAccount(s.T(), s.client, &service.Account{Name: "a1", Platform: service.PlatformAnthropic, Schedulable: true})
	a2 := mustCreateAccount(s.T(), s.client, &service.Account{Name: "a2", Platform: service.PlatformOpenAI, Schedulable: true})
	mustBindAccountToGroup(s.T(), s.client, a1.ID, group.ID, 1)
	mustBindAccountToGroup(s.T(), s.client, a2.ID, group.ID, 2)

	accounts, err := s.repo.ListSchedulableByGroupIDAndPlatform(s.ctx, group.ID, service.PlatformAnthropic)
	s.Require().NoError(err)
	s.Require().Len(accounts, 1)
	s.Require().Equal(a1.ID, accounts[0].ID)
}

func (s *AccountRepoSuite) TestSetSchedulable() {
	account := mustCreateAccount(s.T(), s.client, &service.Account{Name: "acc-sched", Schedulable: true})
	cacheRecorder := &schedulerCacheRecorder{}
	s.repo.schedulerCache = cacheRecorder

	s.Require().NoError(s.repo.SetSchedulable(s.ctx, account.ID, false))

	got, err := s.repo.GetByID(s.ctx, account.ID)
	s.Require().NoError(err)
	s.Require().False(got.Schedulable)
	s.Require().Len(cacheRecorder.setAccounts, 1)
	s.Require().Equal(account.ID, cacheRecorder.setAccounts[0].ID)
}

func (s *AccountRepoSuite) TestSetSchedulable_RollbackKeepsStateAndOutboxAtomic() {
	client := testEntClient(s.T())
	account := mustCreateAccount(s.T(), client, &service.Account{
		Name:        "schedulable-rollback-" + strconv.FormatInt(time.Now().UnixNano(), 10),
		Schedulable: true,
	})
	s.T().Cleanup(func() { _, _ = client.Account.Delete().Where(dbaccount.IDEQ(account.ID)).Exec(context.Background()) })
	cacheRecorder := &schedulerCacheRecorder{}
	repo := newAccountRepositoryWithSQL(client, integrationDB, cacheRecorder)
	_, err := integrationDB.ExecContext(s.ctx, "TRUNCATE scheduler_outbox")
	s.Require().NoError(err)
	tx, err := client.Tx(s.ctx)
	s.Require().NoError(err)
	txCtx := dbent.NewTxContext(s.ctx, tx)

	s.Require().NoError(repo.SetSchedulable(txCtx, account.ID, false))
	s.Require().NoError(tx.Rollback())

	s.Require().Empty(cacheRecorder.setAccounts)
	got, err := repo.GetByID(s.ctx, account.ID)
	s.Require().NoError(err)
	s.Require().True(got.Schedulable)
	var outboxCount int
	s.Require().NoError(integrationDB.QueryRowContext(s.ctx, "SELECT count(*) FROM scheduler_outbox").Scan(&outboxCount))
	s.Require().Zero(outboxCount)
}

func (s *AccountRepoSuite) TestBulkUpdate_RollbackKeepsStateOutboxAndCacheAtomic() {
	client := testEntClient(s.T())
	account := mustCreateAccount(s.T(), client, &service.Account{
		Name:        "bulk-rollback-" + strconv.FormatInt(time.Now().UnixNano(), 10),
		Status:      service.StatusActive,
		Schedulable: true,
	})
	s.T().Cleanup(func() {
		_, _ = client.Account.Delete().Where(dbaccount.IDEQ(account.ID)).Exec(context.Background())
	})
	cacheRecorder := &schedulerCacheRecorder{}
	repo := newAccountRepositoryWithSQL(client, integrationDB, cacheRecorder)
	_, err := integrationDB.ExecContext(s.ctx, "TRUNCATE scheduler_outbox")
	s.Require().NoError(err)
	tx, err := client.Tx(s.ctx)
	s.Require().NoError(err)
	txCtx := dbent.NewTxContext(s.ctx, tx)
	disabled := service.StatusDisabled

	rows, err := repo.BulkUpdate(txCtx, []int64{account.ID}, service.AccountBulkUpdate{Status: &disabled})
	s.Require().NoError(err)
	s.Require().Equal(int64(1), rows)
	s.Require().Empty(cacheRecorder.setAccounts)
	s.Require().NoError(tx.Rollback())

	got, err := repo.GetByID(s.ctx, account.ID)
	s.Require().NoError(err)
	s.Require().Equal(service.StatusActive, got.Status)
	s.Require().Empty(cacheRecorder.setAccounts)
	var outboxCount int
	s.Require().NoError(integrationDB.QueryRowContext(s.ctx, "SELECT count(*) FROM scheduler_outbox").Scan(&outboxCount))
	s.Require().Zero(outboxCount)
}

func (s *AccountRepoSuite) TestBulkUpdate_PublishesSnapshotOnlyAfterCommit() {
	client := testEntClient(s.T())
	account := mustCreateAccount(s.T(), client, &service.Account{
		Name:        "bulk-commit-" + strconv.FormatInt(time.Now().UnixNano(), 10),
		Status:      service.StatusActive,
		Schedulable: true,
	})
	s.T().Cleanup(func() {
		_, _ = client.Account.Delete().Where(dbaccount.IDEQ(account.ID)).Exec(context.Background())
	})
	cacheRecorder := &schedulerCacheRecorder{}
	repo := newAccountRepositoryWithSQL(client, integrationDB, cacheRecorder)
	tx, err := client.Tx(s.ctx)
	s.Require().NoError(err)
	txCtx := dbent.NewTxContext(s.ctx, tx)
	disabled := service.StatusDisabled

	rows, err := repo.BulkUpdate(txCtx, []int64{account.ID}, service.AccountBulkUpdate{Status: &disabled})
	s.Require().NoError(err)
	s.Require().Equal(int64(1), rows)
	s.Require().Empty(cacheRecorder.setAccounts)
	s.Require().NoError(tx.Commit())

	s.Require().Len(cacheRecorder.setAccounts, 1)
	s.Require().Equal(account.ID, cacheRecorder.setAccounts[0].ID)
	s.Require().Equal(service.StatusDisabled, cacheRecorder.setAccounts[0].Status)
}

func (s *AccountRepoSuite) TestBulkUpdate_SyncSchedulerSnapshotOnDisabled() {
	account1 := mustCreateAccount(s.T(), s.client, &service.Account{Name: "bulk-1", Status: service.StatusActive, Schedulable: true})
	account2 := mustCreateAccount(s.T(), s.client, &service.Account{Name: "bulk-2", Status: service.StatusActive, Schedulable: true})
	cacheRecorder := &schedulerCacheRecorder{}
	s.repo.schedulerCache = cacheRecorder

	disabled := service.StatusDisabled
	rows, err := s.repo.BulkUpdate(s.ctx, []int64{account1.ID, account2.ID}, service.AccountBulkUpdate{
		Status: &disabled,
	})
	s.Require().NoError(err)
	s.Require().Equal(int64(2), rows)

	s.Require().Len(cacheRecorder.setAccounts, 2)
	ids := map[int64]struct{}{}
	for _, acc := range cacheRecorder.setAccounts {
		ids[acc.ID] = struct{}{}
	}
	s.Require().Contains(ids, account1.ID)
	s.Require().Contains(ids, account2.ID)
}

// --- SetOverloaded / SetRateLimited / ClearRateLimit ---

func (s *AccountRepoSuite) requireNoSchedulerOutbox() {
	s.T().Helper()
	var count int
	s.Require().NoError(scanSingleRow(s.ctx, s.repo.sql, "SELECT COUNT(*) FROM scheduler_outbox", nil, &count))
	s.Require().Zero(count)
}

func (s *AccountRepoSuite) TestUpdateSessionWindow_SyncsSchedulerSnapshot() {
	account := mustCreateAccount(s.T(), s.client, &service.Account{
		Name:                "session-window-sync",
		SessionWindowStatus: "idle",
	})
	cacheRecorder := &schedulerCacheRecorder{}
	s.repo.schedulerCache = cacheRecorder
	_, err := s.repo.sql.ExecContext(s.ctx, "TRUNCATE scheduler_outbox")
	s.Require().NoError(err)

	s.Require().NoError(s.repo.UpdateSessionWindow(s.ctx, account.ID, nil, nil, "active"))

	s.Require().Len(cacheRecorder.setAccounts, 1)
	s.Require().Equal(account.ID, cacheRecorder.setAccounts[0].ID)
	s.Require().Equal("active", cacheRecorder.setAccounts[0].SessionWindowStatus)
	var outboxCount int
	s.Require().NoError(scanSingleRow(s.ctx, s.repo.sql, "SELECT COUNT(*) FROM scheduler_outbox", nil, &outboxCount))
	s.Require().Zero(outboxCount)
}

func (s *AccountRepoSuite) TestUpdateSessionWindow_AvoidsRedundantSnapshotWrite() {
	start := time.Now().UTC().Truncate(time.Second)
	end := start.Add(5 * time.Hour)
	account := mustCreateAccount(s.T(), s.client, &service.Account{
		Name:                "session-window-unchanged",
		SessionWindowStart:  &start,
		SessionWindowEnd:    &end,
		SessionWindowStatus: "active",
	})
	cacheRecorder := &schedulerCacheRecorder{}
	s.repo.schedulerCache = cacheRecorder

	s.Require().NoError(s.repo.UpdateSessionWindow(s.ctx, account.ID, &start, &end, "active"))

	s.Require().Empty(cacheRecorder.setAccounts)
}

func (s *AccountRepoSuite) TestUpdateSessionWindow_DoesNotReplaceNewerWindow() {
	laterStart := time.Now().UTC().Truncate(time.Second)
	laterEnd := laterStart.Add(5 * time.Hour)
	earlierStart := laterStart.Add(-time.Hour)
	earlierEnd := laterEnd.Add(-time.Hour)
	account := mustCreateAccount(s.T(), s.client, &service.Account{
		Name:                "session-window-monotonic-" + strconv.FormatInt(time.Now().UnixNano(), 10),
		SessionWindowStart:  &laterStart,
		SessionWindowEnd:    &laterEnd,
		SessionWindowStatus: "active",
	})
	cacheRecorder := &schedulerCacheRecorder{}
	s.repo.schedulerCache = cacheRecorder

	s.Require().NoError(s.repo.UpdateSessionWindow(s.ctx, account.ID, &earlierStart, &earlierEnd, "rejected"))

	got, err := s.repo.GetByID(s.ctx, account.ID)
	s.Require().NoError(err)
	s.Require().NotNil(got.SessionWindowStart)
	s.Require().NotNil(got.SessionWindowEnd)
	s.Require().WithinDuration(laterStart, *got.SessionWindowStart, time.Microsecond)
	s.Require().WithinDuration(laterEnd, *got.SessionWindowEnd, time.Microsecond)
	s.Require().Equal("active", got.SessionWindowStatus)
	s.Require().Empty(cacheRecorder.setAccounts)
}

func (s *AccountRepoSuite) TestUpdateSessionWindowEnd_SyncsWithoutLifecycleOutbox() {
	account := mustCreateAccount(s.T(), s.client, &service.Account{Name: "session-window-end-sync"})
	cacheRecorder := &schedulerCacheRecorder{}
	s.repo.schedulerCache = cacheRecorder
	_, err := s.repo.sql.ExecContext(s.ctx, "TRUNCATE scheduler_outbox")
	s.Require().NoError(err)
	end := time.Now().UTC().Add(5 * time.Hour)

	s.Require().NoError(s.repo.UpdateSessionWindowEnd(s.ctx, account.ID, end))

	s.Require().Len(cacheRecorder.setAccounts, 1)
	s.Require().Equal(account.ID, cacheRecorder.setAccounts[0].ID)
	s.Require().NotNil(cacheRecorder.setAccounts[0].SessionWindowEnd)
	s.Require().WithinDuration(end, *cacheRecorder.setAccounts[0].SessionWindowEnd, time.Second)
	var outboxCount int
	s.Require().NoError(scanSingleRow(s.ctx, s.repo.sql, "SELECT COUNT(*) FROM scheduler_outbox", nil, &outboxCount))
	s.Require().Zero(outboxCount)
}

func (s *AccountRepoSuite) TestUpdateSessionWindow_PublishesOnlyAfterCommit() {
	client := testEntClient(s.T())
	account := mustCreateAccount(s.T(), client, &service.Account{
		Name:                "session-window-transaction-" + strconv.FormatInt(time.Now().UnixNano(), 10),
		SessionWindowStatus: "idle",
	})
	s.T().Cleanup(func() { _, _ = client.Account.Delete().Where(dbaccount.IDEQ(account.ID)).Exec(context.Background()) })
	cacheRecorder := &schedulerCacheRecorder{}
	repo := newAccountRepositoryWithSQL(client, integrationDB, cacheRecorder)
	tx, err := client.Tx(s.ctx)
	s.Require().NoError(err)
	txCtx := dbent.NewTxContext(s.ctx, tx)

	s.Require().NoError(repo.UpdateSessionWindow(txCtx, account.ID, nil, nil, "active"))
	s.Require().Empty(cacheRecorder.setAccounts)
	s.Require().NoError(tx.Commit())

	s.Require().Len(cacheRecorder.setAccounts, 1)
	s.Require().Equal("active", cacheRecorder.setAccounts[0].SessionWindowStatus)
}

func (s *AccountRepoSuite) TestUpdateSessionWindowEnd_DoesNotShortenExistingWindow() {
	laterEnd := time.Now().UTC().Add(5 * time.Hour).Truncate(time.Second)
	earlierEnd := laterEnd.Add(-time.Hour)
	account := mustCreateAccount(s.T(), s.client, &service.Account{
		Name:             "session-window-end-monotonic-" + strconv.FormatInt(time.Now().UnixNano(), 10),
		SessionWindowEnd: &laterEnd,
	})
	cacheRecorder := &schedulerCacheRecorder{}
	s.repo.schedulerCache = cacheRecorder

	s.Require().NoError(s.repo.UpdateSessionWindowEnd(s.ctx, account.ID, earlierEnd))

	got, err := s.repo.GetByID(s.ctx, account.ID)
	s.Require().NoError(err)
	s.Require().NotNil(got.SessionWindowEnd)
	s.Require().WithinDuration(laterEnd, *got.SessionWindowEnd, time.Microsecond)
	s.Require().Empty(cacheRecorder.setAccounts)
}

func (s *AccountRepoSuite) TestUpdateSessionWindowEnd_PublishesOnlyAfterCommit() {
	client := testEntClient(s.T())
	originalEnd := time.Now().UTC().Add(time.Hour).Truncate(time.Second)
	account := mustCreateAccount(s.T(), client, &service.Account{
		Name:             "session-window-end-transaction-" + strconv.FormatInt(time.Now().UnixNano(), 10),
		SessionWindowEnd: &originalEnd,
	})
	s.T().Cleanup(func() { _, _ = client.Account.Delete().Where(dbaccount.IDEQ(account.ID)).Exec(context.Background()) })
	cacheRecorder := &schedulerCacheRecorder{}
	repo := newAccountRepositoryWithSQL(client, integrationDB, cacheRecorder)
	tx, err := client.Tx(s.ctx)
	s.Require().NoError(err)
	txCtx := dbent.NewTxContext(s.ctx, tx)
	updatedEnd := originalEnd.Add(4 * time.Hour)

	s.Require().NoError(repo.UpdateSessionWindowEnd(txCtx, account.ID, updatedEnd))
	s.Require().Empty(cacheRecorder.setAccounts)
	s.Require().NoError(tx.Commit())

	s.Require().Len(cacheRecorder.setAccounts, 1)
	s.Require().NotNil(cacheRecorder.setAccounts[0].SessionWindowEnd)
	s.Require().WithinDuration(updatedEnd, *cacheRecorder.setAccounts[0].SessionWindowEnd, time.Microsecond)
}

func (s *AccountRepoSuite) TestUpdateSessionWindowEnd_RollbackDoesNotPublishSnapshot() {
	client := testEntClient(s.T())
	originalEnd := time.Now().UTC().Add(time.Hour).Truncate(time.Second)
	account := mustCreateAccount(s.T(), client, &service.Account{
		Name:             "session-window-end-rollback-" + strconv.FormatInt(time.Now().UnixNano(), 10),
		SessionWindowEnd: &originalEnd,
	})
	s.T().Cleanup(func() { _, _ = client.Account.Delete().Where(dbaccount.IDEQ(account.ID)).Exec(context.Background()) })
	cacheRecorder := &schedulerCacheRecorder{}
	repo := newAccountRepositoryWithSQL(client, integrationDB, cacheRecorder)
	tx, err := client.Tx(s.ctx)
	s.Require().NoError(err)
	txCtx := dbent.NewTxContext(s.ctx, tx)

	s.Require().NoError(repo.UpdateSessionWindowEnd(txCtx, account.ID, originalEnd.Add(4*time.Hour)))
	s.Require().NoError(tx.Rollback())

	s.Require().Empty(cacheRecorder.setAccounts)
	got, err := repo.GetByID(s.ctx, account.ID)
	s.Require().NoError(err)
	s.Require().NotNil(got.SessionWindowEnd)
	s.Require().WithinDuration(originalEnd, *got.SessionWindowEnd, time.Microsecond)
}

func (s *AccountRepoSuite) TestUpdateSessionWindow_RollbackDoesNotPublishSnapshot() {
	client := testEntClient(s.T())
	account := mustCreateAccount(s.T(), client, &service.Account{
		Name:                "session-window-rollback-" + strconv.FormatInt(time.Now().UnixNano(), 10),
		SessionWindowStatus: "idle",
	})
	s.T().Cleanup(func() { _, _ = client.Account.Delete().Where(dbaccount.IDEQ(account.ID)).Exec(context.Background()) })
	cacheRecorder := &schedulerCacheRecorder{}
	repo := newAccountRepositoryWithSQL(client, integrationDB, cacheRecorder)
	tx, err := client.Tx(s.ctx)
	s.Require().NoError(err)
	txCtx := dbent.NewTxContext(s.ctx, tx)

	s.Require().NoError(repo.UpdateSessionWindow(txCtx, account.ID, nil, nil, "active"))
	s.Require().NoError(tx.Rollback())

	s.Require().Empty(cacheRecorder.setAccounts)
	got, err := repo.GetByID(s.ctx, account.ID)
	s.Require().NoError(err)
	s.Require().Equal("idle", got.SessionWindowStatus)
}

func (s *AccountRepoSuite) TestUpdateSessionWindowEnd_AvoidsRedundantSnapshotWrite() {
	end := time.Now().UTC().Add(5 * time.Hour).Truncate(time.Second)
	account := mustCreateAccount(s.T(), s.client, &service.Account{
		Name:             "session-window-end-unchanged",
		SessionWindowEnd: &end,
	})
	cacheRecorder := &schedulerCacheRecorder{}
	s.repo.schedulerCache = cacheRecorder

	s.Require().NoError(s.repo.UpdateSessionWindowEnd(s.ctx, account.ID, end))

	s.Require().Empty(cacheRecorder.setAccounts)
}

func (s *AccountRepoSuite) TestUpdateExtraRuntimeOverlay_PublishesOnlyAfterCommit() {
	client := testEntClient(s.T())
	account := mustCreateAccount(s.T(), client, &service.Account{
		Name:  "extra-runtime-transaction-" + strconv.FormatInt(time.Now().UnixNano(), 10),
		Extra: map[string]any{"session_window_utilization": 0.1},
	})
	s.T().Cleanup(func() { _, _ = client.Account.Delete().Where(dbaccount.IDEQ(account.ID)).Exec(context.Background()) })
	cacheRecorder := &schedulerCacheRecorder{}
	repo := newAccountRepositoryWithSQL(client, integrationDB, cacheRecorder)
	tx, err := client.Tx(s.ctx)
	s.Require().NoError(err)
	txCtx := dbent.NewTxContext(s.ctx, tx)

	s.Require().NoError(repo.UpdateExtra(txCtx, account.ID, map[string]any{"session_window_utilization": 0.5}))
	s.Require().Empty(cacheRecorder.setAccounts)
	s.Require().NoError(tx.Commit())

	s.Require().Len(cacheRecorder.setAccounts, 1)
	s.Require().Equal(0.5, cacheRecorder.setAccounts[0].Extra["session_window_utilization"])
}

func (s *AccountRepoSuite) TestUpdateExtraRuntimeOverlay_RollbackDoesNotPublishSnapshot() {
	client := testEntClient(s.T())
	account := mustCreateAccount(s.T(), client, &service.Account{
		Name:  "extra-runtime-rollback-" + strconv.FormatInt(time.Now().UnixNano(), 10),
		Extra: map[string]any{"session_window_utilization": 0.1},
	})
	s.T().Cleanup(func() { _, _ = client.Account.Delete().Where(dbaccount.IDEQ(account.ID)).Exec(context.Background()) })
	cacheRecorder := &schedulerCacheRecorder{}
	repo := newAccountRepositoryWithSQL(client, integrationDB, cacheRecorder)
	tx, err := client.Tx(s.ctx)
	s.Require().NoError(err)
	txCtx := dbent.NewTxContext(s.ctx, tx)

	s.Require().NoError(repo.UpdateExtra(txCtx, account.ID, map[string]any{"session_window_utilization": 0.5}))
	s.Require().NoError(tx.Rollback())

	s.Require().Empty(cacheRecorder.setAccounts)
	got, err := repo.GetByID(s.ctx, account.ID)
	s.Require().NoError(err)
	s.Require().Equal(0.1, got.Extra["session_window_utilization"])
}

func (s *AccountRepoSuite) TestSetOverloaded() {
	account := mustCreateAccount(s.T(), s.client, &service.Account{Name: "acc-over"})
	until := time.Date(2025, 6, 15, 12, 0, 0, 0, time.UTC)
	cacheRecorder := &schedulerCacheRecorder{}
	s.repo.schedulerCache = cacheRecorder
	_, err := s.repo.sql.ExecContext(s.ctx, "TRUNCATE scheduler_outbox")
	s.Require().NoError(err)

	s.Require().NoError(s.repo.SetOverloaded(s.ctx, account.ID, until))

	got, err := s.repo.GetByID(s.ctx, account.ID)
	s.Require().NoError(err)
	s.Require().NotNil(got.OverloadUntil)
	s.Require().WithinDuration(until, *got.OverloadUntil, time.Second)
	s.Require().Len(cacheRecorder.setAccounts, 1)
	s.Require().Equal(account.ID, cacheRecorder.setAccounts[0].ID)
	s.Require().NotNil(cacheRecorder.setAccounts[0].OverloadUntil)
	s.Require().WithinDuration(until, *cacheRecorder.setAccounts[0].OverloadUntil, time.Second)
	s.requireNoSchedulerOutbox()
}

func (s *AccountRepoSuite) TestSetOverloaded_DoesNotShortenExistingCooldown() {
	account := mustCreateAccount(s.T(), s.client, &service.Account{Name: "acc-over-monotonic"})
	later := time.Now().UTC().Add(2 * time.Hour)
	earlier := later.Add(-time.Hour)
	s.Require().NoError(s.repo.SetOverloaded(s.ctx, account.ID, later))
	cacheRecorder := &schedulerCacheRecorder{}
	s.repo.schedulerCache = cacheRecorder

	s.Require().NoError(s.repo.SetOverloaded(s.ctx, account.ID, earlier))

	got, err := s.repo.GetByID(s.ctx, account.ID)
	s.Require().NoError(err)
	s.Require().NotNil(got.OverloadUntil)
	s.Require().WithinDuration(later, *got.OverloadUntil, time.Microsecond)
	s.Require().Empty(cacheRecorder.setAccounts)
}

func (s *AccountRepoSuite) TestSetOverloaded_RollbackDoesNotPublishSnapshot() {
	client := testEntClient(s.T())
	account := mustCreateAccount(s.T(), client, &service.Account{
		Name: "overload-rollback-" + strconv.FormatInt(time.Now().UnixNano(), 10),
	})
	s.T().Cleanup(func() { _, _ = client.Account.Delete().Where(dbaccount.IDEQ(account.ID)).Exec(context.Background()) })
	cacheRecorder := &schedulerCacheRecorder{}
	repo := newAccountRepositoryWithSQL(client, integrationDB, cacheRecorder)
	tx, err := client.Tx(s.ctx)
	s.Require().NoError(err)
	txCtx := dbent.NewTxContext(s.ctx, tx)

	s.Require().NoError(repo.SetOverloaded(txCtx, account.ID, time.Now().UTC().Add(time.Hour)))
	s.Require().NoError(tx.Rollback())

	s.Require().Empty(cacheRecorder.setAccounts)
	got, err := repo.GetByID(s.ctx, account.ID)
	s.Require().NoError(err)
	s.Require().Nil(got.OverloadUntil)
}

func (s *AccountRepoSuite) TestSetRateLimited() {
	account := mustCreateAccount(s.T(), s.client, &service.Account{Name: "acc-rl"})
	resetAt := time.Date(2025, 6, 15, 14, 0, 0, 0, time.UTC)
	cacheRecorder := &schedulerCacheRecorder{}
	s.repo.schedulerCache = cacheRecorder
	_, err := s.repo.sql.ExecContext(s.ctx, "TRUNCATE scheduler_outbox")
	s.Require().NoError(err)

	s.Require().NoError(s.repo.SetRateLimited(s.ctx, account.ID, resetAt))

	got, err := s.repo.GetByID(s.ctx, account.ID)
	s.Require().NoError(err)
	s.Require().NotNil(got.RateLimitedAt)
	s.Require().NotNil(got.RateLimitResetAt)
	s.Require().WithinDuration(resetAt, *got.RateLimitResetAt, time.Second)
	s.Require().Len(cacheRecorder.setAccounts, 1)
	s.Require().Equal(account.ID, cacheRecorder.setAccounts[0].ID)
	s.requireNoSchedulerOutbox()
}

func (s *AccountRepoSuite) TestSetRateLimited_DoesNotShortenExistingCooldown() {
	account := mustCreateAccount(s.T(), s.client, &service.Account{Name: "acc-rate-limit-monotonic"})
	later := time.Now().UTC().Add(2 * time.Hour)
	earlier := later.Add(-time.Hour)
	s.Require().NoError(s.repo.SetRateLimited(s.ctx, account.ID, later))
	first, err := s.repo.GetByID(s.ctx, account.ID)
	s.Require().NoError(err)
	s.Require().NotNil(first.RateLimitedAt)
	cacheRecorder := &schedulerCacheRecorder{}
	s.repo.schedulerCache = cacheRecorder

	s.Require().NoError(s.repo.SetRateLimited(s.ctx, account.ID, earlier))

	got, err := s.repo.GetByID(s.ctx, account.ID)
	s.Require().NoError(err)
	s.Require().NotNil(got.RateLimitedAt)
	s.Require().NotNil(got.RateLimitResetAt)
	s.Require().WithinDuration(*first.RateLimitedAt, *got.RateLimitedAt, time.Microsecond)
	s.Require().WithinDuration(later, *got.RateLimitResetAt, time.Microsecond)
	s.Require().Empty(cacheRecorder.setAccounts)
}

func (s *AccountRepoSuite) TestSetTempUnschedulable_DoesNotShortenExistingPenalty() {
	later := time.Now().UTC().Add(2 * time.Hour).Truncate(time.Second)
	earlier := later.Add(-time.Hour)
	account := mustCreateAccount(s.T(), s.client, &service.Account{Name: "temp-unsched-monotonic-" + strconv.FormatInt(time.Now().UnixNano(), 10)})
	cacheRecorder := &schedulerCacheRecorder{}
	s.repo.schedulerCache = cacheRecorder

	s.Require().NoError(s.repo.SetTempUnschedulable(s.ctx, account.ID, later, "longer penalty"))
	s.Require().Len(cacheRecorder.setAccounts, 1)
	cacheRecorder.setAccounts = nil

	s.Require().NoError(s.repo.SetTempUnschedulable(s.ctx, account.ID, earlier, "stale penalty"))

	got, err := s.repo.GetByID(s.ctx, account.ID)
	s.Require().NoError(err)
	s.Require().NotNil(got.TempUnschedulableUntil)
	s.Require().WithinDuration(later, *got.TempUnschedulableUntil, time.Microsecond)
	s.Require().Equal("longer penalty", got.TempUnschedulableReason)
	s.Require().Empty(cacheRecorder.setAccounts)
}

func (s *AccountRepoSuite) TestSetTempUnschedulable_RollbackDoesNotPublishSnapshot() {
	client := testEntClient(s.T())
	account := mustCreateAccount(s.T(), client, &service.Account{
		Name: "temp-unsched-rollback-" + strconv.FormatInt(time.Now().UnixNano(), 10),
	})
	s.T().Cleanup(func() { _, _ = client.Account.Delete().Where(dbaccount.IDEQ(account.ID)).Exec(context.Background()) })
	cacheRecorder := &schedulerCacheRecorder{}
	repo := newAccountRepositoryWithSQL(client, integrationDB, cacheRecorder)
	tx, err := client.Tx(s.ctx)
	s.Require().NoError(err)
	txCtx := dbent.NewTxContext(s.ctx, tx)

	s.Require().NoError(repo.SetTempUnschedulable(txCtx, account.ID, time.Now().UTC().Add(time.Hour), "retry"))
	s.Require().NoError(tx.Rollback())

	s.Require().Empty(cacheRecorder.setAccounts)
	got, err := repo.GetByID(s.ctx, account.ID)
	s.Require().NoError(err)
	s.Require().Nil(got.TempUnschedulableUntil)
}

func (s *AccountRepoSuite) TestSetRateLimited_PublishesOnlyAfterCommit() {
	client := testEntClient(s.T())
	account := mustCreateAccount(s.T(), client, &service.Account{
		Name: "rate-limit-transaction-" + strconv.FormatInt(time.Now().UnixNano(), 10),
	})
	s.T().Cleanup(func() { _, _ = client.Account.Delete().Where(dbaccount.IDEQ(account.ID)).Exec(context.Background()) })
	cacheRecorder := &schedulerCacheRecorder{}
	repo := newAccountRepositoryWithSQL(client, integrationDB, cacheRecorder)
	tx, err := client.Tx(s.ctx)
	s.Require().NoError(err)
	txCtx := dbent.NewTxContext(s.ctx, tx)
	resetAt := time.Now().UTC().Add(time.Hour)

	s.Require().NoError(repo.SetRateLimited(txCtx, account.ID, resetAt))
	s.Require().Empty(cacheRecorder.setAccounts)
	s.Require().NoError(tx.Commit())

	s.Require().Len(cacheRecorder.setAccounts, 1)
	s.Require().NotNil(cacheRecorder.setAccounts[0].RateLimitResetAt)
	s.Require().WithinDuration(resetAt, *cacheRecorder.setAccounts[0].RateLimitResetAt, time.Second)
}

func (s *AccountRepoSuite) TestClearRateLimit() {
	account := mustCreateAccount(s.T(), s.client, &service.Account{Name: "acc-clear"})
	until := time.Now().Add(1 * time.Hour)
	s.Require().NoError(s.repo.SetOverloaded(s.ctx, account.ID, until))
	s.Require().NoError(s.repo.SetRateLimited(s.ctx, account.ID, until))
	cacheRecorder := &schedulerCacheRecorder{}
	s.repo.schedulerCache = cacheRecorder
	_, err := s.repo.sql.ExecContext(s.ctx, "TRUNCATE scheduler_outbox")
	s.Require().NoError(err)

	s.Require().NoError(s.repo.ClearRateLimit(s.ctx, account.ID))

	got, err := s.repo.GetByID(s.ctx, account.ID)
	s.Require().NoError(err)
	s.Require().Nil(got.RateLimitedAt)
	s.Require().Nil(got.RateLimitResetAt)
	s.Require().Nil(got.OverloadUntil)
	s.Require().Len(cacheRecorder.setAccounts, 1)
	s.Require().Equal(account.ID, cacheRecorder.setAccounts[0].ID)
	s.requireNoSchedulerOutbox()
}

func (s *AccountRepoSuite) TestClearRateLimit_AvoidsRedundantSnapshotWrite() {
	account := mustCreateAccount(s.T(), s.client, &service.Account{
		Name: "rate-limit-clear-unchanged-" + strconv.FormatInt(time.Now().UnixNano(), 10),
	})
	cacheRecorder := &schedulerCacheRecorder{}
	s.repo.schedulerCache = cacheRecorder

	s.Require().NoError(s.repo.ClearRateLimit(s.ctx, account.ID))

	s.Require().Empty(cacheRecorder.setAccounts)
}

func (s *AccountRepoSuite) TestClearTempUnschedulable_AvoidsRedundantSnapshotWrite() {
	account := mustCreateAccount(s.T(), s.client, &service.Account{
		Name: "temp-unsched-unchanged-" + strconv.FormatInt(time.Now().UnixNano(), 10),
	})
	cacheRecorder := &schedulerCacheRecorder{}
	s.repo.schedulerCache = cacheRecorder

	s.Require().NoError(s.repo.ClearTempUnschedulable(s.ctx, account.ID))

	s.Require().Empty(cacheRecorder.setAccounts)
}

func (s *AccountRepoSuite) TestTempUnschedulableFieldsLoadedByGetByIDAndGetByIDs() {
	acc1 := mustCreateAccount(s.T(), s.client, &service.Account{Name: "acc-temp-1"})
	acc2 := mustCreateAccount(s.T(), s.client, &service.Account{Name: "acc-temp-2"})
	cacheRecorder := &schedulerCacheRecorder{}
	s.repo.schedulerCache = cacheRecorder
	_, err := s.repo.sql.ExecContext(s.ctx, "TRUNCATE scheduler_outbox")
	s.Require().NoError(err)

	until := time.Now().Add(15 * time.Minute).UTC().Truncate(time.Second)
	reason := `{"rule":"429","matched_keyword":"too many requests"}`
	s.Require().NoError(s.repo.SetTempUnschedulable(s.ctx, acc1.ID, until, reason))
	s.Require().Len(cacheRecorder.setAccounts, 1)
	s.Require().Equal(acc1.ID, cacheRecorder.setAccounts[0].ID)
	s.requireNoSchedulerOutbox()

	gotByID, err := s.repo.GetByID(s.ctx, acc1.ID)
	s.Require().NoError(err)
	s.Require().NotNil(gotByID.TempUnschedulableUntil)
	s.Require().WithinDuration(until, *gotByID.TempUnschedulableUntil, time.Second)
	s.Require().Equal(reason, gotByID.TempUnschedulableReason)

	gotByIDs, err := s.repo.GetByIDs(s.ctx, []int64{acc2.ID, acc1.ID})
	s.Require().NoError(err)
	s.Require().Len(gotByIDs, 2)
	s.Require().Equal(acc2.ID, gotByIDs[0].ID)
	s.Require().Nil(gotByIDs[0].TempUnschedulableUntil)
	s.Require().Equal("", gotByIDs[0].TempUnschedulableReason)
	s.Require().Equal(acc1.ID, gotByIDs[1].ID)
	s.Require().NotNil(gotByIDs[1].TempUnschedulableUntil)
	s.Require().WithinDuration(until, *gotByIDs[1].TempUnschedulableUntil, time.Second)
	s.Require().Equal(reason, gotByIDs[1].TempUnschedulableReason)

	cacheRecorder.setAccounts = nil

	s.Require().NoError(s.repo.ClearTempUnschedulable(s.ctx, acc1.ID))
	cleared, err := s.repo.GetByID(s.ctx, acc1.ID)
	s.Require().NoError(err)
	s.Require().Nil(cleared.TempUnschedulableUntil)
	s.Require().Equal("", cleared.TempUnschedulableReason)
	s.Require().Len(cacheRecorder.setAccounts, 1)
	s.Require().Equal(acc1.ID, cacheRecorder.setAccounts[0].ID)
	s.Require().Nil(cacheRecorder.setAccounts[0].TempUnschedulableUntil)
	s.Require().Equal("", cacheRecorder.setAccounts[0].TempUnschedulableReason)
	s.requireNoSchedulerOutbox()
}

func (s *AccountRepoSuite) TestSetModelRateLimit_SyncsWithoutLifecycleOutbox() {
	account := mustCreateAccount(s.T(), s.client, &service.Account{Name: "acc-set-model-rate"})
	cacheRecorder := &schedulerCacheRecorder{}
	s.repo.schedulerCache = cacheRecorder
	_, err := s.repo.sql.ExecContext(s.ctx, "TRUNCATE scheduler_outbox")
	s.Require().NoError(err)
	resetAt := time.Now().UTC().Add(time.Hour)

	s.Require().NoError(s.repo.SetModelRateLimit(s.ctx, account.ID, "gpt-5", resetAt, "runtime cooldown"))

	s.Require().Len(cacheRecorder.setAccounts, 1)
	s.Require().Equal(account.ID, cacheRecorder.setAccounts[0].ID)
	s.Require().Contains(cacheRecorder.setAccounts[0].Extra, "model_rate_limits")
	s.requireNoSchedulerOutbox()
}

func (s *AccountRepoSuite) TestSetModelRateLimit_DoesNotShortenExistingCooldown() {
	later := time.Now().UTC().Add(2 * time.Hour).Truncate(time.Second)
	earlier := later.Add(-time.Hour)
	account := mustCreateAccount(s.T(), s.client, &service.Account{
		Name: "model-rate-monotonic-" + strconv.FormatInt(time.Now().UnixNano(), 10),
		Extra: map[string]any{
			"model_rate_limits": map[string]any{
				"gpt-5": map[string]any{
					"rate_limited_at":     "2026-07-18T00:00:00Z",
					"rate_limit_reset_at": later.Format(time.RFC3339),
					"reason":              "longer cooldown",
				},
			},
		},
	})
	cacheRecorder := &schedulerCacheRecorder{}
	s.repo.schedulerCache = cacheRecorder

	s.Require().NoError(s.repo.SetModelRateLimit(s.ctx, account.ID, "gpt-5", earlier, "stale cooldown"))

	got, err := s.repo.GetByID(s.ctx, account.ID)
	s.Require().NoError(err)
	limits, ok := got.Extra["model_rate_limits"].(map[string]any)
	s.Require().True(ok)
	limit, ok := limits["gpt-5"].(map[string]any)
	s.Require().True(ok)
	s.Require().Equal(later.Format(time.RFC3339), limit["rate_limit_reset_at"])
	s.Require().Equal("longer cooldown", limit["reason"])
	s.Require().Empty(cacheRecorder.setAccounts)
}

func (s *AccountRepoSuite) TestSetModelRateLimit_PublishesOnlyAfterCommit() {
	client := testEntClient(s.T())
	account := mustCreateAccount(s.T(), client, &service.Account{
		Name: "model-rate-transaction-" + strconv.FormatInt(time.Now().UnixNano(), 10),
	})
	s.T().Cleanup(func() { _, _ = client.Account.Delete().Where(dbaccount.IDEQ(account.ID)).Exec(context.Background()) })
	cacheRecorder := &schedulerCacheRecorder{}
	repo := newAccountRepositoryWithSQL(client, integrationDB, cacheRecorder)
	tx, err := client.Tx(s.ctx)
	s.Require().NoError(err)
	txCtx := dbent.NewTxContext(s.ctx, tx)

	s.Require().NoError(repo.SetModelRateLimit(txCtx, account.ID, "gpt-5", time.Now().UTC().Add(time.Hour)))
	s.Require().Empty(cacheRecorder.setAccounts)
	s.Require().NoError(tx.Commit())

	s.Require().Len(cacheRecorder.setAccounts, 1)
	s.Require().Contains(cacheRecorder.setAccounts[0].Extra, "model_rate_limits")
}

func (s *AccountRepoSuite) TestClearAntigravityQuotaScopes_RollbackKeepsExtraAndOutboxAtomic() {
	client := testEntClient(s.T())
	account := mustCreateAccount(s.T(), client, &service.Account{
		Name: "clear-quota-scopes-rollback-" + strconv.FormatInt(time.Now().UnixNano(), 10),
		Extra: map[string]any{
			"antigravity_quota_scopes": map[string]any{"scope": "value"},
		},
	})
	s.T().Cleanup(func() {
		_, _ = client.Account.Delete().Where(dbaccount.IDEQ(account.ID)).Exec(context.Background())
	})
	repo := newAccountRepositoryWithSQL(client, integrationDB, nil)
	_, err := integrationDB.ExecContext(s.ctx, "TRUNCATE scheduler_outbox")
	s.Require().NoError(err)
	tx, err := client.Tx(s.ctx)
	s.Require().NoError(err)
	txCtx := dbent.NewTxContext(s.ctx, tx)

	s.Require().NoError(repo.ClearAntigravityQuotaScopes(txCtx, account.ID))
	s.Require().NoError(tx.Rollback())

	got, err := repo.GetByID(s.ctx, account.ID)
	s.Require().NoError(err)
	s.Require().Contains(got.Extra, "antigravity_quota_scopes")
	var outboxCount int
	s.Require().NoError(integrationDB.QueryRowContext(s.ctx, "SELECT count(*) FROM scheduler_outbox").Scan(&outboxCount))
	s.Require().Zero(outboxCount)
}

func (s *AccountRepoSuite) TestClearAntigravityQuotaScopes_UnchangedAvoidsRedundantOutbox() {
	account := mustCreateAccount(s.T(), s.client, &service.Account{
		Name: "clear-quota-scopes-unchanged-" + strconv.FormatInt(time.Now().UnixNano(), 10),
	})
	_, err := s.repo.sql.ExecContext(s.ctx, "TRUNCATE scheduler_outbox")
	s.Require().NoError(err)

	s.Require().NoError(s.repo.ClearAntigravityQuotaScopes(s.ctx, account.ID))

	s.requireNoSchedulerOutbox()
}

func (s *AccountRepoSuite) TestClearAntigravityQuotaScopes_UnchangedHonorsCallerTransaction() {
	client := testEntClient(s.T())
	account := mustCreateAccount(s.T(), client, &service.Account{
		Name: "clear-quota-scopes-unchanged-tx-" + strconv.FormatInt(time.Now().UnixNano(), 10),
	})
	s.T().Cleanup(func() {
		_, _ = client.Account.Delete().Where(dbaccount.IDEQ(account.ID)).Exec(context.Background())
	})
	repo := newAccountRepositoryWithSQL(client, integrationDB, nil)
	_, err := integrationDB.ExecContext(s.ctx, "TRUNCATE scheduler_outbox")
	s.Require().NoError(err)
	tx, err := client.Tx(s.ctx)
	s.Require().NoError(err)
	txCtx := dbent.NewTxContext(s.ctx, tx)

	s.Require().NoError(repo.ClearAntigravityQuotaScopes(txCtx, account.ID))
	s.Require().NoError(tx.Rollback())

	var outboxCount int
	s.Require().NoError(integrationDB.QueryRowContext(s.ctx, "SELECT count(*) FROM scheduler_outbox").Scan(&outboxCount))
	s.Require().Zero(outboxCount)
}

func (s *AccountRepoSuite) TestClearModelRateLimits_SyncsSchedulerSnapshot() {
	account := mustCreateAccount(s.T(), s.client, &service.Account{
		Name: "acc-clear-model-rate",
		Extra: map[string]any{
			"model_rate_limits": map[string]any{
				"claude-sonnet-4-5": map[string]any{
					"rate_limit_reset_at": "2026-06-03T10:00:00Z",
				},
			},
		},
	})
	cacheRecorder := &schedulerCacheRecorder{}
	s.repo.schedulerCache = cacheRecorder
	_, err := s.repo.sql.ExecContext(s.ctx, "TRUNCATE scheduler_outbox")
	s.Require().NoError(err)

	s.Require().NoError(s.repo.ClearModelRateLimits(s.ctx, account.ID))

	got, err := s.repo.GetByID(s.ctx, account.ID)
	s.Require().NoError(err)
	s.Require().NotContains(got.Extra, "model_rate_limits")
	s.Require().Len(cacheRecorder.setAccounts, 1)
	s.Require().Equal(account.ID, cacheRecorder.setAccounts[0].ID)
	s.Require().NotContains(cacheRecorder.setAccounts[0].Extra, "model_rate_limits")
	var outboxCount int
	s.Require().NoError(scanSingleRow(s.ctx, s.repo.sql, "SELECT COUNT(*) FROM scheduler_outbox", nil, &outboxCount))
	s.Require().Zero(outboxCount)
}

func (s *AccountRepoSuite) TestClearModelRateLimits_AvoidsRedundantSnapshotWrite() {
	account := mustCreateAccount(s.T(), s.client, &service.Account{
		Name: "model-rate-clear-unchanged-" + strconv.FormatInt(time.Now().UnixNano(), 10),
	})
	cacheRecorder := &schedulerCacheRecorder{}
	s.repo.schedulerCache = cacheRecorder

	s.Require().NoError(s.repo.ClearModelRateLimits(s.ctx, account.ID))

	s.Require().Empty(cacheRecorder.setAccounts)
}

func (s *AccountRepoSuite) TestClearModelRateLimits_UnchangedHonorsCallerTransaction() {
	client := testEntClient(s.T())
	account := mustCreateAccount(s.T(), client, &service.Account{
		Name: "model-rate-clear-unchanged-tx-" + strconv.FormatInt(time.Now().UnixNano(), 10),
	})
	s.T().Cleanup(func() {
		_, _ = client.Account.Delete().Where(dbaccount.IDEQ(account.ID)).Exec(context.Background())
	})
	cacheRecorder := &schedulerCacheRecorder{}
	repo := newAccountRepositoryWithSQL(client, integrationDB, cacheRecorder)
	tx, err := client.Tx(s.ctx)
	s.Require().NoError(err)
	txCtx := dbent.NewTxContext(s.ctx, tx)

	s.Require().NoError(repo.ClearModelRateLimits(txCtx, account.ID))
	s.Require().NoError(tx.Rollback())

	s.Require().Empty(cacheRecorder.setAccounts)
}

// --- UpdateLastUsed ---

func (s *AccountRepoSuite) TestUpdateLastUsed() {
	account := mustCreateAccount(s.T(), s.client, &service.Account{Name: "acc-used"})
	s.Require().Nil(account.LastUsedAt)

	s.Require().NoError(s.repo.UpdateLastUsed(s.ctx, account.ID))

	got, err := s.repo.GetByID(s.ctx, account.ID)
	s.Require().NoError(err)
	s.Require().NotNil(got.LastUsedAt)
}

func (s *AccountRepoSuite) TestUpdateLastUsed_RollbackKeepsTimestampAndOutboxAtomic() {
	client := testEntClient(s.T())
	original := time.Now().UTC().Add(-time.Hour).Truncate(time.Second)
	account := mustCreateAccount(s.T(), client, &service.Account{
		Name:       "last-used-rollback-" + strconv.FormatInt(time.Now().UnixNano(), 10),
		LastUsedAt: &original,
	})
	s.T().Cleanup(func() {
		_, _ = client.Account.Delete().Where(dbaccount.IDEQ(account.ID)).Exec(context.Background())
	})
	repo := newAccountRepositoryWithSQL(client, integrationDB, nil)
	_, err := integrationDB.ExecContext(s.ctx, "TRUNCATE scheduler_outbox")
	s.Require().NoError(err)
	tx, err := client.Tx(s.ctx)
	s.Require().NoError(err)
	txCtx := dbent.NewTxContext(s.ctx, tx)

	s.Require().NoError(repo.UpdateLastUsed(txCtx, account.ID))
	s.Require().NoError(tx.Rollback())

	got, err := repo.GetByID(s.ctx, account.ID)
	s.Require().NoError(err)
	s.Require().NotNil(got.LastUsedAt)
	s.Require().WithinDuration(original, *got.LastUsedAt, time.Millisecond)
	var outboxCount int
	s.Require().NoError(integrationDB.QueryRowContext(s.ctx, "SELECT count(*) FROM scheduler_outbox").Scan(&outboxCount))
	s.Require().Zero(outboxCount)
}

func (s *AccountRepoSuite) TestBatchUpdateLastUsed_RollbackKeepsTimestampAndOutboxAtomic() {
	client := testEntClient(s.T())
	original := time.Now().UTC().Add(-time.Hour).Truncate(time.Second)
	account := mustCreateAccount(s.T(), client, &service.Account{
		Name:       "batch-last-used-rollback-" + strconv.FormatInt(time.Now().UnixNano(), 10),
		LastUsedAt: &original,
	})
	s.T().Cleanup(func() {
		_, _ = client.Account.Delete().Where(dbaccount.IDEQ(account.ID)).Exec(context.Background())
	})
	repo := newAccountRepositoryWithSQL(client, integrationDB, nil)
	_, err := integrationDB.ExecContext(s.ctx, "TRUNCATE scheduler_outbox")
	s.Require().NoError(err)
	tx, err := client.Tx(s.ctx)
	s.Require().NoError(err)
	txCtx := dbent.NewTxContext(s.ctx, tx)

	s.Require().NoError(repo.BatchUpdateLastUsed(txCtx, map[int64]time.Time{account.ID: original.Add(30 * time.Minute)}))
	s.Require().NoError(tx.Rollback())

	got, err := repo.GetByID(s.ctx, account.ID)
	s.Require().NoError(err)
	s.Require().NotNil(got.LastUsedAt)
	s.Require().WithinDuration(original, *got.LastUsedAt, time.Millisecond)
	var outboxCount int
	s.Require().NoError(integrationDB.QueryRowContext(s.ctx, "SELECT count(*) FROM scheduler_outbox").Scan(&outboxCount))
	s.Require().Zero(outboxCount)
}

func (s *AccountRepoSuite) TestLastUsedOutboxPublishesStoredTimestampsAndExistingIDsOnly() {
	account1 := mustCreateAccount(s.T(), s.client, &service.Account{Name: "acc-used-monotonic-1"})
	account2 := mustCreateAccount(s.T(), s.client, &service.Account{Name: "acc-used-monotonic-2"})
	stored1 := time.Now().UTC().Add(time.Hour).Truncate(time.Second)
	stored2 := stored1.Add(time.Hour)
	_, err := s.repo.sql.ExecContext(s.ctx, "UPDATE accounts SET last_used_at = $1 WHERE id = $2", stored1, account1.ID)
	s.Require().NoError(err)
	_, err = s.repo.sql.ExecContext(s.ctx, "UPDATE accounts SET last_used_at = $1 WHERE id = $2", stored2, account2.ID)
	s.Require().NoError(err)
	_, err = s.repo.sql.ExecContext(s.ctx, "TRUNCATE scheduler_outbox")
	s.Require().NoError(err)

	s.Require().NoError(s.repo.UpdateLastUsed(s.ctx, account1.ID))
	s.Require().Equal(map[string]int64{strconv.FormatInt(account1.ID, 10): stored1.Unix()}, s.lastUsedOutboxPayload())

	_, err = s.repo.sql.ExecContext(s.ctx, "TRUNCATE scheduler_outbox")
	s.Require().NoError(err)
	missingID := account2.ID + 1000000
	s.Require().NoError(s.repo.BatchUpdateLastUsed(s.ctx, map[int64]time.Time{
		account1.ID: stored1.Add(-time.Hour),
		account2.ID: stored2.Add(-time.Hour),
		missingID:   stored2,
	}))
	s.Require().Equal(map[string]int64{
		strconv.FormatInt(account1.ID, 10): stored1.Unix(),
		strconv.FormatInt(account2.ID, 10): stored2.Unix(),
	}, s.lastUsedOutboxPayload())
}

func (s *AccountRepoSuite) TestBatchUpdateLastUsedChunksLargeInput() {
	const total = accountLastUsedBatchSize + 1
	updates := make(map[int64]time.Time, total)
	ids := make([]int64, 0, total)
	usedAt := time.Now().UTC().Truncate(time.Second)
	for i := 0; i < total; i++ {
		account := mustCreateAccount(s.T(), s.client, &service.Account{Name: "acc-used-large-" + strconv.Itoa(i)})
		ids = append(ids, account.ID)
		updates[account.ID] = usedAt.Add(time.Duration(i) * time.Second)
	}
	_, err := s.repo.sql.ExecContext(s.ctx, "TRUNCATE scheduler_outbox")
	s.Require().NoError(err)

	s.Require().NoError(s.repo.BatchUpdateLastUsed(s.ctx, updates))

	var outboxRows int
	s.Require().NoError(scanSingleRow(s.ctx, s.repo.sql, `SELECT count(*) FROM scheduler_outbox WHERE event_type=$1`, []any{service.SchedulerOutboxEventAccountLastUsed}, &outboxRows))
	s.Require().Equal(2, outboxRows)
	var stored time.Time
	s.Require().NoError(scanSingleRow(s.ctx, s.repo.sql, `SELECT last_used_at FROM accounts WHERE id=$1`, []any{ids[len(ids)-1]}, &stored))
	s.Require().True(updates[ids[len(ids)-1]].Equal(stored))
}

func (s *AccountRepoSuite) lastUsedOutboxPayload() map[string]int64 {
	var raw []byte
	err := scanSingleRow(s.ctx, s.repo.sql, `SELECT payload FROM scheduler_outbox WHERE event_type = $1 ORDER BY id DESC LIMIT 1`, []any{service.SchedulerOutboxEventAccountLastUsed}, &raw)
	s.Require().NoError(err)
	var payload struct {
		LastUsed map[string]int64 `json:"last_used"`
	}
	s.Require().NoError(json.Unmarshal(raw, &payload))
	return payload.LastUsed
}

// --- SetError ---

func (s *AccountRepoSuite) TestSetError() {
	account := mustCreateAccount(s.T(), s.client, &service.Account{Name: "acc-err", Status: service.StatusActive, Schedulable: true})

	s.Require().NoError(s.repo.SetError(s.ctx, account.ID, "something went wrong"))

	got, err := s.repo.GetByID(s.ctx, account.ID)
	s.Require().NoError(err)
	s.Require().Equal(service.StatusError, got.Status)
	s.Require().Equal("something went wrong", got.ErrorMessage)
	s.Require().False(got.Schedulable)
}

func (s *AccountRepoSuite) TestUpdateErrorStatusUnschedulesAccount() {
	account := mustCreateAccount(s.T(), s.client, &service.Account{Name: "acc-update-err", Status: service.StatusActive, Schedulable: true})
	account.Status = service.StatusError
	account.ErrorMessage = "token revoked"
	account.Schedulable = true

	s.Require().NoError(s.repo.Update(s.ctx, account))

	got, err := s.repo.GetByID(s.ctx, account.ID)
	s.Require().NoError(err)
	s.Require().Equal(service.StatusError, got.Status)
	s.Require().Equal("token revoked", got.ErrorMessage)
	s.Require().False(got.Schedulable)
}

func (s *AccountRepoSuite) TestClearError_SyncSchedulerSnapshotOnRecovery() {
	account := mustCreateAccount(s.T(), s.client, &service.Account{
		Name:         "acc-clear-err",
		Status:       service.StatusError,
		ErrorMessage: "temporary error",
	})
	cacheRecorder := &schedulerCacheRecorder{}
	s.repo.schedulerCache = cacheRecorder

	s.Require().NoError(s.repo.ClearError(s.ctx, account.ID))

	got, err := s.repo.GetByID(s.ctx, account.ID)
	s.Require().NoError(err)
	s.Require().Equal(service.StatusActive, got.Status)
	s.Require().Empty(got.ErrorMessage)
	s.Require().Len(cacheRecorder.setAccounts, 1)
	s.Require().Equal(account.ID, cacheRecorder.setAccounts[0].ID)
	s.Require().Equal(service.StatusActive, cacheRecorder.setAccounts[0].Status)
}

func (s *AccountRepoSuite) TestClearError_UnchangedAvoidsRedundantEffects() {
	account := mustCreateAccount(s.T(), s.client, &service.Account{
		Name:   "clear-error-unchanged-" + strconv.FormatInt(time.Now().UnixNano(), 10),
		Status: service.StatusActive,
	})
	cacheRecorder := &schedulerCacheRecorder{}
	s.repo.schedulerCache = cacheRecorder
	_, err := s.repo.sql.ExecContext(s.ctx, "TRUNCATE scheduler_outbox")
	s.Require().NoError(err)

	s.Require().NoError(s.repo.ClearError(s.ctx, account.ID))

	s.Require().Empty(cacheRecorder.setAccounts)
	s.requireNoSchedulerOutbox()
}

func (s *AccountRepoSuite) TestClearError_UnchangedHonorsCallerTransaction() {
	client := testEntClient(s.T())
	account := mustCreateAccount(s.T(), client, &service.Account{
		Name:   "clear-error-unchanged-tx-" + strconv.FormatInt(time.Now().UnixNano(), 10),
		Status: service.StatusActive,
	})
	s.T().Cleanup(func() {
		_, _ = client.Account.Delete().Where(dbaccount.IDEQ(account.ID)).Exec(context.Background())
	})
	cacheRecorder := &schedulerCacheRecorder{}
	repo := newAccountRepositoryWithSQL(client, integrationDB, cacheRecorder)
	tx, err := client.Tx(s.ctx)
	s.Require().NoError(err)
	txCtx := dbent.NewTxContext(s.ctx, tx)

	s.Require().NoError(repo.ClearError(txCtx, account.ID))
	s.Require().NoError(tx.Rollback())

	s.Require().Empty(cacheRecorder.setAccounts)
}

// --- UpdateSessionWindow ---

func (s *AccountRepoSuite) TestClearModelRateLimit_PublishesOnlyAfterCommit() {
	client := testEntClient(s.T())
	account := mustCreateAccount(s.T(), client, &service.Account{
		Name: "clear-model-rate-limit-transaction-" + strconv.FormatInt(time.Now().UnixNano(), 10),
		Extra: map[string]any{
			"model_rate_limits": map[string]any{
				"AICredits": map[string]any{"rate_limit_reset_at": "2099-01-01T00:00:00Z"},
			},
		},
	})
	s.T().Cleanup(func() { _, _ = client.Account.Delete().Where(dbaccount.IDEQ(account.ID)).Exec(context.Background()) })
	cacheRecorder := &schedulerCacheRecorder{}
	repo := newAccountRepositoryWithSQL(client, integrationDB, cacheRecorder)
	tx, err := client.Tx(s.ctx)
	s.Require().NoError(err)
	txCtx := dbent.NewTxContext(s.ctx, tx)

	s.Require().NoError(repo.ClearModelRateLimit(txCtx, account.ID, "AICredits"))
	s.Require().Empty(cacheRecorder.setAccounts)
	s.Require().NoError(tx.Commit())

	s.Require().Len(cacheRecorder.setAccounts, 1)
	limits, ok := cacheRecorder.setAccounts[0].Extra["model_rate_limits"].(map[string]any)
	s.Require().True(ok)
	s.Require().NotContains(limits, "AICredits")
}

func (s *AccountRepoSuite) TestClearModelRateLimit_PreservesConcurrentScopes() {
	account := mustCreateAccount(s.T(), s.client, &service.Account{
		Name: "clear-model-rate-limit-scope",
		Extra: map[string]any{
			"model_rate_limits": map[string]any{
				"AICredits": map[string]any{"rate_limit_reset_at": "2099-01-01T00:00:00Z"},
				"model-a":   map[string]any{"rate_limit_reset_at": "2099-01-02T00:00:00Z"},
			},
		},
	})

	s.Require().NoError(s.repo.ClearModelRateLimit(s.ctx, account.ID, "AICredits"))

	got, err := s.repo.GetByID(s.ctx, account.ID)
	s.Require().NoError(err)
	limits, ok := got.Extra["model_rate_limits"].(map[string]any)
	s.Require().True(ok)
	s.Require().NotContains(limits, "AICredits")
	s.Require().Contains(limits, "model-a")
}

func (s *AccountRepoSuite) TestUpdateSessionWindow() {
	account := mustCreateAccount(s.T(), s.client, &service.Account{Name: "acc-win"})
	start := time.Date(2025, 6, 15, 10, 0, 0, 0, time.UTC)
	end := time.Date(2025, 6, 15, 15, 0, 0, 0, time.UTC)

	s.Require().NoError(s.repo.UpdateSessionWindow(s.ctx, account.ID, &start, &end, "active"))

	got, err := s.repo.GetByID(s.ctx, account.ID)
	s.Require().NoError(err)
	s.Require().NotNil(got.SessionWindowStart)
	s.Require().NotNil(got.SessionWindowEnd)
	s.Require().Equal("active", got.SessionWindowStatus)
}

// --- UpdateExtra ---

func (s *AccountRepoSuite) TestUpdateExtra_MergesFields() {
	account := mustCreateAccount(s.T(), s.client, &service.Account{
		Name:  "acc-extra",
		Extra: map[string]any{"a": "1"},
	})
	s.Require().NoError(s.repo.UpdateExtra(s.ctx, account.ID, map[string]any{"b": "2"}), "UpdateExtra")

	got, err := s.repo.GetByID(s.ctx, account.ID)
	s.Require().NoError(err, "GetByID")
	s.Require().Equal("1", got.Extra["a"])
	s.Require().Equal("2", got.Extra["b"])
}

func (s *AccountRepoSuite) TestUpdateExtra_EmptyUpdates() {
	account := mustCreateAccount(s.T(), s.client, &service.Account{Name: "acc-extra-empty"})
	s.Require().NoError(s.repo.UpdateExtra(s.ctx, account.ID, map[string]any{}))
}

func (s *AccountRepoSuite) TestUpdateExtra_UnchangedRuntimeAvoidsRedundantEffects() {
	account := mustCreateAccount(s.T(), s.client, &service.Account{
		Name: "acc-extra-unchanged-" + strconv.FormatInt(time.Now().UnixNano(), 10),
		Extra: map[string]any{
			"session_window_utilization": 0.5,
		},
	})
	cacheRecorder := &schedulerCacheRecorder{}
	s.repo.schedulerCache = cacheRecorder
	_, err := s.repo.sql.ExecContext(s.ctx, "TRUNCATE scheduler_outbox")
	s.Require().NoError(err)

	s.Require().NoError(s.repo.UpdateExtra(s.ctx, account.ID, map[string]any{
		"session_window_utilization": 0.5,
	}))

	s.Require().Empty(cacheRecorder.setAccounts)
	s.requireNoSchedulerOutbox()
}

func (s *AccountRepoSuite) TestUpdateRuntimeExtra_StaleSnapshotIsNoOp() {
	newer := time.Now().UTC().Truncate(time.Second)
	account := mustCreateAccount(s.T(), s.client, &service.Account{
		Name: "runtime-extra-stale-" + strconv.FormatInt(time.Now().UnixNano(), 10),
		Extra: map[string]any{
			"codex_usage_updated_at": newer.Format(time.RFC3339),
			"codex_5h_used_percent":  20.0,
		},
	})
	cacheRecorder := &schedulerCacheRecorder{}
	s.repo.schedulerCache = cacheRecorder
	_, err := s.repo.sql.ExecContext(s.ctx, "TRUNCATE scheduler_outbox")
	s.Require().NoError(err)

	updated, err := s.repo.UpdateRuntimeExtra(s.ctx, account.ID, map[string]any{
		"codex_usage_updated_at": newer.Add(-time.Minute).Format(time.RFC3339),
		"codex_5h_used_percent":  10.0,
	}, "codex_usage_updated_at", newer.Add(-time.Minute))
	s.Require().NoError(err)
	s.Require().False(updated)

	got, err := s.repo.GetByID(s.ctx, account.ID)
	s.Require().NoError(err)
	s.Require().Equal(20.0, got.Extra["codex_5h_used_percent"])
	s.Require().Equal(newer.Format(time.RFC3339), got.Extra["codex_usage_updated_at"])
	s.Require().Empty(cacheRecorder.setAccounts)
	s.requireNoSchedulerOutbox()
}

func (s *AccountRepoSuite) TestUpdateRuntimeExtra_PersistsComparedObservationTime() {
	observedAt := time.Now().UTC().Truncate(time.Microsecond)
	account := mustCreateAccount(s.T(), s.client, &service.Account{
		Name: "runtime-extra-observation-" + strconv.FormatInt(time.Now().UnixNano(), 10),
		Extra: map[string]any{
			"passive_usage_sampled_at":     observedAt.Add(-time.Hour).Format(time.RFC3339Nano),
			"passive_usage_7d_utilization": 0.1,
		},
	})
	cacheRecorder := &schedulerCacheRecorder{}
	s.repo.schedulerCache = cacheRecorder

	updated, err := s.repo.UpdateRuntimeExtra(s.ctx, account.ID, map[string]any{
		// A producer cannot weaken the repository watermark by supplying a stale
		// payload value that differs from the timestamp used for ordering.
		"passive_usage_sampled_at":     observedAt.Add(-time.Minute).Format(time.RFC3339Nano),
		"passive_usage_7d_utilization": 0.7,
	}, "passive_usage_sampled_at", observedAt)
	s.Require().NoError(err)
	s.Require().True(updated)

	got, err := s.repo.GetByID(s.ctx, account.ID)
	s.Require().NoError(err)
	s.Require().Equal(observedAt.Format(time.RFC3339Nano), got.Extra["passive_usage_sampled_at"])
	s.Require().Equal(0.7, got.Extra["passive_usage_7d_utilization"])
	s.Require().Len(cacheRecorder.setAccounts, 1)
}

func (s *AccountRepoSuite) TestUpdateRuntimeExtra_NewerSnapshotPublishesAfterCommit() {
	older := time.Now().UTC().Add(-time.Minute).Truncate(time.Second)
	newer := older.Add(time.Minute)
	client := testEntClient(s.T())
	account := mustCreateAccount(s.T(), client, &service.Account{
		Name: "runtime-extra-newer-" + strconv.FormatInt(time.Now().UnixNano(), 10),
		Extra: map[string]any{
			"passive_usage_sampled_at":     older.Format(time.RFC3339),
			"passive_usage_7d_utilization": 0.1,
		},
	})
	s.T().Cleanup(func() { _, _ = client.Account.Delete().Where(dbaccount.IDEQ(account.ID)).Exec(context.Background()) })
	cacheRecorder := &schedulerCacheRecorder{}
	repo := newAccountRepositoryWithSQL(client, integrationDB, cacheRecorder)
	tx, err := client.Tx(s.ctx)
	s.Require().NoError(err)
	txCtx := dbent.NewTxContext(s.ctx, tx)

	updated, err := repo.UpdateRuntimeExtra(txCtx, account.ID, map[string]any{
		"passive_usage_sampled_at":     newer.Format(time.RFC3339),
		"passive_usage_7d_utilization": 0.7,
	}, "passive_usage_sampled_at", newer)
	s.Require().NoError(err)
	s.Require().True(updated)
	s.Require().Empty(cacheRecorder.setAccounts)
	s.Require().NoError(tx.Commit())

	s.Require().Len(cacheRecorder.setAccounts, 1)
	s.Require().Equal(0.7, cacheRecorder.setAccounts[0].Extra["passive_usage_7d_utilization"])
}

func (s *AccountRepoSuite) TestUpdateRuntimeExtra_RollbackDoesNotPublishSnapshot() {
	older := time.Now().UTC().Add(-time.Minute).Truncate(time.Second)
	newer := older.Add(time.Minute)
	client := testEntClient(s.T())
	account := mustCreateAccount(s.T(), client, &service.Account{
		Name: "runtime-extra-rollback-" + strconv.FormatInt(time.Now().UnixNano(), 10),
		Extra: map[string]any{
			"passive_usage_sampled_at":     older.Format(time.RFC3339),
			"passive_usage_7d_utilization": 0.1,
		},
	})
	s.T().Cleanup(func() { _, _ = client.Account.Delete().Where(dbaccount.IDEQ(account.ID)).Exec(context.Background()) })
	cacheRecorder := &schedulerCacheRecorder{}
	repo := newAccountRepositoryWithSQL(client, integrationDB, cacheRecorder)
	tx, err := client.Tx(s.ctx)
	s.Require().NoError(err)
	txCtx := dbent.NewTxContext(s.ctx, tx)

	updated, err := repo.UpdateRuntimeExtra(txCtx, account.ID, map[string]any{
		"passive_usage_sampled_at":     newer.Format(time.RFC3339),
		"passive_usage_7d_utilization": 0.7,
	}, "passive_usage_sampled_at", newer)
	s.Require().NoError(err)
	s.Require().True(updated)
	s.Require().NoError(tx.Rollback())

	s.Require().Empty(cacheRecorder.setAccounts)
	got, err := repo.GetByID(s.ctx, account.ID)
	s.Require().NoError(err)
	s.Require().Equal(0.1, got.Extra["passive_usage_7d_utilization"])
}

func (s *AccountRepoSuite) TestUpdateExtra_NilExtra() {
	account := mustCreateAccount(s.T(), s.client, &service.Account{Name: "acc-nil-extra", Extra: nil})
	s.Require().NoError(s.repo.UpdateExtra(s.ctx, account.ID, map[string]any{"key": "val"}))

	got, err := s.repo.GetByID(s.ctx, account.ID)
	s.Require().NoError(err)
	s.Require().Equal("val", got.Extra["key"])
}

func (s *AccountRepoSuite) TestUpdateExtra_UnchangedHonorsCallerTransaction() {
	client := testEntClient(s.T())
	account := mustCreateAccount(s.T(), client, &service.Account{
		Name: "extra-unchanged-transaction-" + strconv.FormatInt(time.Now().UnixNano(), 10),
		Extra: map[string]any{
			"session_window_utilization": 0.5,
		},
	})
	s.T().Cleanup(func() { _, _ = client.Account.Delete().Where(dbaccount.IDEQ(account.ID)).Exec(context.Background()) })
	cacheRecorder := &schedulerCacheRecorder{}
	repo := newAccountRepositoryWithSQL(client, integrationDB, cacheRecorder)
	tx, err := client.Tx(s.ctx)
	s.Require().NoError(err)
	txCtx := dbent.NewTxContext(s.ctx, tx)

	s.Require().NoError(repo.UpdateExtra(txCtx, account.ID, map[string]any{
		"session_window_utilization": 0.5,
	}))
	s.Require().NoError(tx.Rollback())

	s.Require().Empty(cacheRecorder.setAccounts)
}

func (s *AccountRepoSuite) TestUpdateExtraNestedBool_UnchangedAvoidsRedundantEffects() {
	account := mustCreateAccount(s.T(), s.client, &service.Account{
		Name: "nested-extra-unchanged-" + strconv.FormatInt(time.Now().UnixNano(), 10),
		Extra: map[string]any{
			"openai_responses_supported": true,
			"openai_responses_supported_by_model": map[string]any{
				"gpt-5": true,
			},
		},
	})
	cacheRecorder := &schedulerCacheRecorder{}
	s.repo.schedulerCache = cacheRecorder
	_, err := s.repo.sql.ExecContext(s.ctx, "TRUNCATE scheduler_outbox")
	s.Require().NoError(err)

	s.Require().NoError(s.repo.UpdateExtraNestedBool(
		s.ctx,
		account.ID,
		map[string]any{"openai_responses_supported": true},
		"openai_responses_supported_by_model",
		"gpt-5",
		true,
	))

	s.Require().Empty(cacheRecorder.setAccounts)
	s.requireNoSchedulerOutbox()
}

func (s *AccountRepoSuite) TestUpdateExtraNestedBool_UnchangedHonorsCallerTransaction() {
	client := testEntClient(s.T())
	account := mustCreateAccount(s.T(), client, &service.Account{
		Name: "nested-extra-unchanged-tx-" + strconv.FormatInt(time.Now().UnixNano(), 10),
		Extra: map[string]any{
			"openai_responses_supported": true,
			"openai_responses_supported_by_model": map[string]any{
				"gpt-5": true,
			},
		},
	})
	s.T().Cleanup(func() { _, _ = client.Account.Delete().Where(dbaccount.IDEQ(account.ID)).Exec(context.Background()) })
	cacheRecorder := &schedulerCacheRecorder{}
	repo := newAccountRepositoryWithSQL(client, integrationDB, cacheRecorder)
	tx, err := client.Tx(s.ctx)
	s.Require().NoError(err)
	txCtx := dbent.NewTxContext(s.ctx, tx)

	s.Require().NoError(repo.UpdateExtraNestedBool(
		txCtx,
		account.ID,
		map[string]any{"openai_responses_supported": true},
		"openai_responses_supported_by_model",
		"gpt-5",
		true,
	))
	s.Require().NoError(tx.Rollback())

	s.Require().Empty(cacheRecorder.setAccounts)
}

func (s *AccountRepoSuite) TestResetOpenAICodexFingerprint_UpdatesOnlyOAuthLikeAccounts() {
	profile := service.ParseOpenAICodexUAProfile(service.DefaultOpenAICodexUserAgent)
	for _, accountType := range []string{service.AccountTypeOAuth, service.AccountTypeSetupToken} {
		s.Run(accountType, func() {
			fingerprint, _ := service.NormalizeOpenAICodexFingerprint(nil, profile, time.Now())
			account := mustCreateAccount(s.T(), s.client, &service.Account{
				Name:     "acc-reset-fingerprint-" + accountType,
				Platform: service.PlatformOpenAI,
				Type:     accountType,
				Extra:    map[string]any{"keep": "value"},
			})

			s.Require().NoError(s.repo.ResetOpenAICodexFingerprint(s.ctx, account.ID, fingerprint))

			got, err := s.repo.GetByID(s.ctx, account.ID)
			s.Require().NoError(err)
			s.Require().Equal("value", got.Extra["keep"])
			persisted, changed := service.NormalizeOpenAICodexFingerprint(got.Extra[service.OpenAICodexFingerprintExtraKey], profile, time.Now())
			s.Require().False(changed)
			s.Require().Equal(fingerprint.InstallationID, persisted.InstallationID)
			s.Require().Equal(fingerprint.UAProfile, persisted.UAProfile)
		})
	}
}

func (s *AccountRepoSuite) TestResetOpenAICodexFingerprint_PublishesOnlyAfterCommit() {
	client := testEntClient(s.T())
	account := mustCreateAccount(s.T(), client, &service.Account{
		Name:     "fingerprint-transaction-" + strconv.FormatInt(time.Now().UnixNano(), 10),
		Platform: service.PlatformOpenAI,
		Type:     service.AccountTypeOAuth,
	})
	s.T().Cleanup(func() { _, _ = client.Account.Delete().Where(dbaccount.IDEQ(account.ID)).Exec(context.Background()) })
	cacheRecorder := &schedulerCacheRecorder{}
	repo := newAccountRepositoryWithSQL(client, integrationDB, cacheRecorder)
	fingerprint, _ := service.NormalizeOpenAICodexFingerprint(nil, service.ParseOpenAICodexUAProfile(service.DefaultOpenAICodexUserAgent), time.Now())
	tx, err := client.Tx(s.ctx)
	s.Require().NoError(err)
	txCtx := dbent.NewTxContext(s.ctx, tx)

	s.Require().NoError(repo.ResetOpenAICodexFingerprint(txCtx, account.ID, fingerprint))
	s.Require().Empty(cacheRecorder.setAccounts)
	s.Require().NoError(tx.Commit())

	s.Require().Len(cacheRecorder.setAccounts, 1)
	s.Require().Contains(cacheRecorder.setAccounts[0].Extra, service.OpenAICodexFingerprintExtraKey)
}

func (s *AccountRepoSuite) TestResetOpenAICodexFingerprint_UnchangedAvoidsRedundantSnapshotWrite() {
	fingerprint, _ := service.NormalizeOpenAICodexFingerprint(
		nil,
		service.ParseOpenAICodexUAProfile(service.DefaultOpenAICodexUserAgent),
		time.Now(),
	)
	account := mustCreateAccount(s.T(), s.client, &service.Account{
		Name:     "fingerprint-unchanged-" + strconv.FormatInt(time.Now().UnixNano(), 10),
		Platform: service.PlatformOpenAI,
		Type:     service.AccountTypeOAuth,
		Extra: map[string]any{
			service.OpenAICodexFingerprintExtraKey: fingerprint,
		},
	})
	cacheRecorder := &schedulerCacheRecorder{}
	s.repo.schedulerCache = cacheRecorder

	s.Require().NoError(s.repo.ResetOpenAICodexFingerprint(s.ctx, account.ID, fingerprint))

	s.Require().Empty(cacheRecorder.setAccounts)
}

func (s *AccountRepoSuite) TestResetOpenAICodexFingerprint_UnchangedHonorsCallerTransaction() {
	client := testEntClient(s.T())
	fingerprint, _ := service.NormalizeOpenAICodexFingerprint(
		nil,
		service.ParseOpenAICodexUAProfile(service.DefaultOpenAICodexUserAgent),
		time.Now(),
	)
	account := mustCreateAccount(s.T(), client, &service.Account{
		Name:     "fingerprint-unchanged-tx-" + strconv.FormatInt(time.Now().UnixNano(), 10),
		Platform: service.PlatformOpenAI,
		Type:     service.AccountTypeOAuth,
		Extra: map[string]any{
			service.OpenAICodexFingerprintExtraKey: fingerprint,
		},
	})
	s.T().Cleanup(func() { _, _ = client.Account.Delete().Where(dbaccount.IDEQ(account.ID)).Exec(context.Background()) })
	cacheRecorder := &schedulerCacheRecorder{}
	repo := newAccountRepositoryWithSQL(client, integrationDB, cacheRecorder)
	tx, err := client.Tx(s.ctx)
	s.Require().NoError(err)
	txCtx := dbent.NewTxContext(s.ctx, tx)

	s.Require().NoError(repo.ResetOpenAICodexFingerprint(txCtx, account.ID, fingerprint))
	s.Require().NoError(tx.Rollback())

	s.Require().Empty(cacheRecorder.setAccounts)
}

func (s *AccountRepoSuite) TestResetOpenAICodexFingerprint_RejectsAPIKeyAccount() {
	fingerprint, _ := service.NormalizeOpenAICodexFingerprint(nil, service.ParseOpenAICodexUAProfile(service.DefaultOpenAICodexUserAgent), time.Now())
	account := mustCreateAccount(s.T(), s.client, &service.Account{
		Name:     "acc-reset-fingerprint-apikey",
		Platform: service.PlatformOpenAI,
		Type:     service.AccountTypeAPIKey,
		Extra:    map[string]any{"keep": "value"},
	})

	err := s.repo.ResetOpenAICodexFingerprint(s.ctx, account.ID, fingerprint)

	s.Require().ErrorIs(err, service.ErrAccountNotFound)
	got, getErr := s.repo.GetByID(s.ctx, account.ID)
	s.Require().NoError(getErr)
	s.Require().Equal("value", got.Extra["keep"])
	_, ok := got.Extra[service.OpenAICodexFingerprintExtraKey]
	s.Require().False(ok)
}

func (s *AccountRepoSuite) TestEnsureOpenAICodexFingerprint_RejectsNonOAuthLikeAccounts() {
	profile := service.ParseOpenAICodexUAProfile(service.DefaultOpenAICodexUserAgent)
	for _, tc := range []struct {
		name     string
		platform string
		typ      string
	}{
		{name: "apikey", platform: service.PlatformOpenAI, typ: service.AccountTypeAPIKey},
		{name: "non-openai-oauth", platform: service.PlatformAnthropic, typ: service.AccountTypeOAuth},
	} {
		s.Run(tc.name, func() {
			fingerprint, _ := service.NormalizeOpenAICodexFingerprint(nil, profile, time.Now())
			account := mustCreateAccount(s.T(), s.client, &service.Account{
				Name:     "acc-ensure-fingerprint-" + tc.name,
				Platform: tc.platform,
				Type:     tc.typ,
				Extra:    map[string]any{"keep": "value"},
			})

			_, err := s.repo.EnsureOpenAICodexFingerprint(s.ctx, account.ID, fingerprint, nil)

			s.Require().ErrorIs(err, service.ErrAccountNotFound)
			got, getErr := s.repo.GetByID(s.ctx, account.ID)
			s.Require().NoError(getErr)
			s.Require().Equal("value", got.Extra["keep"])
			_, ok := got.Extra[service.OpenAICodexFingerprintExtraKey]
			s.Require().False(ok)
		})
	}
}

func (s *AccountRepoSuite) TestEnsureOpenAICodexFingerprint_ExpectedOldCASPreservesConcurrentCustomWinner() {
	legacyProfile := service.ParseOpenAICodexUAProfile("codex-tui/0.136.0 (Mac OS 26.5.0; arm64) Apple_Terminal/470.2 (codex-tui; 0.136.0)")
	legacy, _ := service.NormalizeOpenAICodexFingerprint(nil, legacyProfile, time.Now())
	account := mustCreateAccount(s.T(), s.client, &service.Account{Name: "acc-fingerprint-cas", Platform: service.PlatformOpenAI, Type: service.AccountTypeOAuth, Extra: map[string]any{service.OpenAICodexFingerprintExtraKey: legacy}})
	custom := legacy
	custom.UAProfile = service.ParseOpenAICodexUAProfile("custom/9 (Custom OS) term (custom; 9)")
	s.Require().NoError(s.repo.UpdateExtra(s.ctx, account.ID, map[string]any{service.OpenAICodexFingerprintExtraKey: custom}))
	migrated := legacy
	migrated.UAProfile = service.ParseOpenAICodexUAProfile(service.DefaultOpenAICodexUserAgent)
	winner, err := s.repo.EnsureOpenAICodexFingerprint(s.ctx, account.ID, migrated, &legacy)
	s.Require().NoError(err)
	s.Require().Equal(custom, winner)
}

func (s *AccountRepoSuite) TestEnsureOpenAICodexFingerprint_ExpectedOldCASMigratesOnce() {
	legacy, _ := service.NormalizeOpenAICodexFingerprint(nil, service.ParseOpenAICodexUAProfile("codex-tui/0.136.0 (Mac OS 26.5.0; arm64) Apple_Terminal/470.2 (codex-tui; 0.136.0)"), time.Now())
	account := mustCreateAccount(s.T(), s.client, &service.Account{Name: "acc-fingerprint-migrate", Platform: service.PlatformOpenAI, Type: service.AccountTypeOAuth, Extra: map[string]any{service.OpenAICodexFingerprintExtraKey: legacy}})
	migrated := legacy
	migrated.UAProfile = service.ParseOpenAICodexUAProfile(service.DefaultOpenAICodexUserAgent)
	winner, err := s.repo.EnsureOpenAICodexFingerprint(s.ctx, account.ID, migrated, &legacy)
	s.Require().NoError(err)
	s.Require().Equal(migrated, winner)
	winner, err = s.repo.EnsureOpenAICodexFingerprint(s.ctx, account.ID, migrated, &legacy)
	s.Require().NoError(err)
	s.Require().Equal(migrated, winner)
}

func (s *AccountRepoSuite) TestUpdateCredentials_PublishesOnlyAfterCommit() {
	client := testEntClient(s.T())
	account := mustCreateAccount(s.T(), client, &service.Account{
		Name:        "credentials-transaction-" + strconv.FormatInt(time.Now().UnixNano(), 10),
		Credentials: map[string]any{"access_token": "old"},
	})
	s.T().Cleanup(func() { _, _ = client.Account.Delete().Where(dbaccount.IDEQ(account.ID)).Exec(context.Background()) })
	cacheRecorder := &schedulerCacheRecorder{}
	repo := newAccountRepositoryWithSQL(client, integrationDB, cacheRecorder)
	tx, err := client.Tx(s.ctx)
	s.Require().NoError(err)
	txCtx := dbent.NewTxContext(s.ctx, tx)

	s.Require().NoError(repo.UpdateCredentials(txCtx, account.ID, map[string]any{"access_token": "new"}))
	s.Require().Empty(cacheRecorder.setAccounts)
	s.Require().NoError(tx.Commit())

	s.Require().Len(cacheRecorder.setAccounts, 1)
	s.Require().Equal("new", cacheRecorder.setAccounts[0].Credentials["access_token"])
}

func (s *AccountRepoSuite) TestUpdateAuthAndMergeExtra_PublishesOnlyAfterCommit() {
	client := testEntClient(s.T())
	account := mustCreateAccount(s.T(), client, &service.Account{
		Name:        "auth-extra-transaction-" + strconv.FormatInt(time.Now().UnixNano(), 10),
		Platform:    service.PlatformOpenAI,
		Type:        service.AccountTypeOAuth,
		Credentials: map[string]any{"access_token": "old"},
	})
	s.T().Cleanup(func() { _, _ = client.Account.Delete().Where(dbaccount.IDEQ(account.ID)).Exec(context.Background()) })
	cacheRecorder := &schedulerCacheRecorder{}
	repo := newAccountRepositoryWithSQL(client, integrationDB, cacheRecorder)
	tx, err := client.Tx(s.ctx)
	s.Require().NoError(err)
	txCtx := dbent.NewTxContext(s.ctx, tx)

	s.Require().NoError(repo.UpdateAuthAndMergeExtra(txCtx, account.ID, service.AccountTypeOAuth,
		map[string]any{"access_token": "new"}, map[string]any{"org_uuid": "org"}, nil))
	s.Require().Empty(cacheRecorder.setAccounts)
	s.Require().NoError(tx.Commit())

	s.Require().Len(cacheRecorder.setAccounts, 1)
	s.Require().Equal("new", cacheRecorder.setAccounts[0].Credentials["access_token"])
	s.Require().Equal("org", cacheRecorder.setAccounts[0].Extra["org_uuid"])
}

func (s *AccountRepoSuite) TestUpdateAuthAndMergeExtraPreservesConcurrentExtra() {
	account := mustCreateAccount(s.T(), s.client, &service.Account{
		Name:        "acc-auth-extra",
		Platform:    service.PlatformOpenAI,
		Type:        service.AccountTypeOAuth,
		Credentials: map[string]any{"refresh_token": "rt-old"},
		Extra:       map[string]any{"existing": "keep"},
	})

	s.Require().NoError(s.repo.UpdateAuthAndMergeExtra(
		s.ctx,
		account.ID,
		service.AccountTypeOAuth,
		map[string]any{"access_token": "at-new"},
		map[string]any{"org_uuid": "org"},
		nil,
	))

	got, err := s.repo.GetByID(s.ctx, account.ID)
	s.Require().NoError(err)
	s.Require().Equal("at-new", got.Credentials["access_token"])
	s.Require().Equal("keep", got.Extra["existing"])
	s.Require().Equal("org", got.Extra["org_uuid"])
}

func (s *AccountRepoSuite) TestUpdateExtra_SchedulerNeutralSkipsOutboxAndSyncsFreshSnapshot() {
	account := mustCreateAccount(s.T(), s.client, &service.Account{
		Name:     "acc-extra-neutral",
		Platform: service.PlatformOpenAI,
		Extra:    map[string]any{"codex_usage_updated_at": "old"},
	})
	cacheRecorder := &schedulerCacheRecorder{
		accounts: map[int64]*service.Account{
			account.ID: {
				ID:       account.ID,
				Platform: account.Platform,
				Status:   service.StatusDisabled,
				Extra: map[string]any{
					"codex_usage_updated_at": "old",
				},
			},
		},
	}
	s.repo.schedulerCache = cacheRecorder

	updates := map[string]any{
		"codex_usage_updated_at":     "2026-03-11T10:00:00Z",
		"codex_5h_used_percent":      88.5,
		"session_window_utilization": 0.42,
	}
	s.Require().NoError(s.repo.UpdateExtra(s.ctx, account.ID, updates))

	got, err := s.repo.GetByID(s.ctx, account.ID)
	s.Require().NoError(err)
	s.Require().Equal("2026-03-11T10:00:00Z", got.Extra["codex_usage_updated_at"])
	s.Require().Equal(88.5, got.Extra["codex_5h_used_percent"])
	s.Require().Equal(0.42, got.Extra["session_window_utilization"])

	var outboxCount int
	s.Require().NoError(scanSingleRow(s.ctx, s.repo.sql, "SELECT COUNT(*) FROM scheduler_outbox", nil, &outboxCount))
	s.Require().Zero(outboxCount)
	s.Require().Len(cacheRecorder.setAccounts, 1)
	s.Require().NotNil(cacheRecorder.accounts[account.ID])
	s.Require().Equal(service.StatusActive, cacheRecorder.accounts[account.ID].Status)
	s.Require().Equal("2026-03-11T10:00:00Z", cacheRecorder.accounts[account.ID].Extra["codex_usage_updated_at"])
}

func (s *AccountRepoSuite) TestUpdateExtra_ExhaustedCodexSnapshotSyncsSchedulerCache() {
	account := mustCreateAccount(s.T(), s.client, &service.Account{
		Name:     "acc-extra-codex-exhausted",
		Platform: service.PlatformOpenAI,
		Type:     service.AccountTypeOAuth,
		Extra:    map[string]any{},
	})
	cacheRecorder := &schedulerCacheRecorder{}
	s.repo.schedulerCache = cacheRecorder
	_, err := s.repo.sql.ExecContext(s.ctx, "TRUNCATE scheduler_outbox")
	s.Require().NoError(err)

	s.Require().NoError(s.repo.UpdateExtra(s.ctx, account.ID, map[string]any{
		"codex_7d_used_percent":        100.0,
		"codex_7d_reset_at":            "2026-03-12T13:00:00Z",
		"codex_7d_reset_after_seconds": 86400,
	}))

	var count int
	err = scanSingleRow(s.ctx, s.repo.sql, "SELECT COUNT(*) FROM scheduler_outbox", nil, &count)
	s.Require().NoError(err)
	s.Require().Equal(0, count)
	s.Require().Len(cacheRecorder.setAccounts, 1)
	s.Require().Equal(account.ID, cacheRecorder.setAccounts[0].ID)
	s.Require().Equal(service.StatusActive, cacheRecorder.setAccounts[0].Status)
	s.Require().Equal(100.0, cacheRecorder.setAccounts[0].Extra["codex_7d_used_percent"])
}

func (s *AccountRepoSuite) TestUpdateExtra_MixedLifecycleAndRuntimePublishesBothEffects() {
	account := mustCreateAccount(s.T(), s.client, &service.Account{
		Name:     "acc-extra-mixed",
		Platform: service.PlatformOpenAI,
		Extra:    map[string]any{},
	})
	cacheRecorder := &schedulerCacheRecorder{}
	s.repo.schedulerCache = cacheRecorder
	_, err := s.repo.sql.ExecContext(s.ctx, "TRUNCATE scheduler_outbox")
	s.Require().NoError(err)

	s.Require().NoError(s.repo.UpdateExtra(s.ctx, account.ID, map[string]any{
		"openai_compact_supported": true,
		"codex_usage_updated_at":   "2026-03-11T10:00:00Z",
		"codex_5h_used_percent":    42.5,
	}))

	var count int
	err = scanSingleRow(s.ctx, s.repo.sql, "SELECT COUNT(*) FROM scheduler_outbox", nil, &count)
	s.Require().NoError(err)
	s.Require().Equal(1, count)
	s.Require().Len(cacheRecorder.setAccounts, 1)
	s.Require().Equal(true, cacheRecorder.setAccounts[0].Extra["openai_compact_supported"])
	s.Require().Equal("2026-03-11T10:00:00Z", cacheRecorder.setAccounts[0].Extra["codex_usage_updated_at"])
	s.Require().Equal(42.5, cacheRecorder.setAccounts[0].Extra["codex_5h_used_percent"])
}

func (s *AccountRepoSuite) TestUpdateExtra_LifecycleOnlyAvoidsDirectSnapshotWrite() {
	account := mustCreateAccount(s.T(), s.client, &service.Account{
		Name:     "acc-extra-lifecycle",
		Platform: service.PlatformOpenAI,
		Extra:    map[string]any{},
	})
	cacheRecorder := &schedulerCacheRecorder{}
	s.repo.schedulerCache = cacheRecorder
	_, err := s.repo.sql.ExecContext(s.ctx, "TRUNCATE scheduler_outbox")
	s.Require().NoError(err)

	s.Require().NoError(s.repo.UpdateExtra(s.ctx, account.ID, map[string]any{
		"openai_compact_supported": true,
	}))

	var count int
	err = scanSingleRow(s.ctx, s.repo.sql, "SELECT COUNT(*) FROM scheduler_outbox", nil, &count)
	s.Require().NoError(err)
	s.Require().Equal(1, count)
	s.Require().Empty(cacheRecorder.setAccounts)
}

// --- GetByCRSAccountID ---

func (s *AccountRepoSuite) TestGetByCRSAccountID() {
	crsID := "crs-12345"
	mustCreateAccount(s.T(), s.client, &service.Account{
		Name:  "acc-crs",
		Extra: map[string]any{"crs_account_id": crsID},
	})

	got, err := s.repo.GetByCRSAccountID(s.ctx, crsID)
	s.Require().NoError(err)
	s.Require().NotNil(got)
	s.Require().Equal("acc-crs", got.Name)
}

func (s *AccountRepoSuite) TestGetByCRSAccountID_NotFound() {
	got, err := s.repo.GetByCRSAccountID(s.ctx, "non-existent")
	s.Require().NoError(err)
	s.Require().Nil(got)
}

func (s *AccountRepoSuite) TestGetByCRSAccountID_EmptyString() {
	got, err := s.repo.GetByCRSAccountID(s.ctx, "")
	s.Require().NoError(err)
	s.Require().Nil(got)
}

// --- BulkUpdate ---

func (s *AccountRepoSuite) TestBulkUpdate() {
	a1 := mustCreateAccount(s.T(), s.client, &service.Account{Name: "bulk1", Priority: 1})
	a2 := mustCreateAccount(s.T(), s.client, &service.Account{Name: "bulk2", Priority: 1})

	newPriority := 99
	affected, err := s.repo.BulkUpdate(s.ctx, []int64{a1.ID, a2.ID}, service.AccountBulkUpdate{
		Priority: &newPriority,
	})
	s.Require().NoError(err)
	s.Require().GreaterOrEqual(affected, int64(1), "expected at least one affected row")

	got1, _ := s.repo.GetByID(s.ctx, a1.ID)
	got2, _ := s.repo.GetByID(s.ctx, a2.ID)
	s.Require().Equal(99, got1.Priority)
	s.Require().Equal(99, got2.Priority)
}

func (s *AccountRepoSuite) TestIncrementQuotaUsed_RollbackKeepsUsageAndOutboxAtomic() {
	client := testEntClient(s.T())
	account := mustCreateAccount(s.T(), client, &service.Account{
		Name: "increment-quota-rollback-" + strconv.FormatInt(time.Now().UnixNano(), 10),
		Extra: map[string]any{
			"quota_limit": 10.0,
			"quota_used":  9.0,
		},
	})
	s.T().Cleanup(func() {
		_, _ = client.Account.Delete().Where(dbaccount.IDEQ(account.ID)).Exec(context.Background())
	})
	repo := newAccountRepositoryWithSQL(client, integrationDB, nil)
	_, err := integrationDB.ExecContext(s.ctx, "TRUNCATE scheduler_outbox")
	s.Require().NoError(err)
	tx, err := client.Tx(s.ctx)
	s.Require().NoError(err)
	txCtx := dbent.NewTxContext(s.ctx, tx)

	s.Require().NoError(repo.IncrementQuotaUsed(txCtx, account.ID, 1))
	s.Require().NoError(tx.Rollback())

	got, err := repo.GetByID(s.ctx, account.ID)
	s.Require().NoError(err)
	s.Require().Equal(9.0, got.Extra["quota_used"])
	var outboxCount int
	s.Require().NoError(integrationDB.QueryRowContext(s.ctx, "SELECT count(*) FROM scheduler_outbox").Scan(&outboxCount))
	s.Require().Zero(outboxCount)
}

func (s *AccountRepoSuite) TestResetQuotaUsed_RollbackKeepsUsageAndOutboxAtomic() {
	client := testEntClient(s.T())
	account := mustCreateAccount(s.T(), client, &service.Account{
		Name: "reset-quota-rollback-" + strconv.FormatInt(time.Now().UnixNano(), 10),
		Extra: map[string]any{
			"quota_limit": 10.0,
			"quota_used":  10.0,
		},
	})
	s.T().Cleanup(func() {
		_, _ = client.Account.Delete().Where(dbaccount.IDEQ(account.ID)).Exec(context.Background())
	})
	repo := newAccountRepositoryWithSQL(client, integrationDB, nil)
	_, err := integrationDB.ExecContext(s.ctx, "TRUNCATE scheduler_outbox")
	s.Require().NoError(err)
	tx, err := client.Tx(s.ctx)
	s.Require().NoError(err)
	txCtx := dbent.NewTxContext(s.ctx, tx)

	s.Require().NoError(repo.ResetQuotaUsed(txCtx, account.ID))
	s.Require().NoError(tx.Rollback())

	got, err := repo.GetByID(s.ctx, account.ID)
	s.Require().NoError(err)
	s.Require().Equal(10.0, got.Extra["quota_used"])
	var outboxCount int
	s.Require().NoError(integrationDB.QueryRowContext(s.ctx, "SELECT count(*) FROM scheduler_outbox").Scan(&outboxCount))
	s.Require().Zero(outboxCount)
}

func (s *AccountRepoSuite) TestBulkUpdate_MergeCredentials() {
	a1 := mustCreateAccount(s.T(), s.client, &service.Account{
		Name:        "bulk-cred",
		Credentials: map[string]any{"existing": "value"},
	})

	_, err := s.repo.BulkUpdate(s.ctx, []int64{a1.ID}, service.AccountBulkUpdate{
		Credentials: map[string]any{"new_key": "new_value"},
	})
	s.Require().NoError(err)

	got, _ := s.repo.GetByID(s.ctx, a1.ID)
	s.Require().Equal("value", got.Credentials["existing"])
	s.Require().Equal("new_value", got.Credentials["new_key"])
}

func (s *AccountRepoSuite) TestBulkUpdate_MergeExtra() {
	a1 := mustCreateAccount(s.T(), s.client, &service.Account{
		Name:  "bulk-extra",
		Extra: map[string]any{"existing": "val"},
	})

	_, err := s.repo.BulkUpdate(s.ctx, []int64{a1.ID}, service.AccountBulkUpdate{
		Extra: map[string]any{"new_key": "new_val"},
	})
	s.Require().NoError(err)

	got, _ := s.repo.GetByID(s.ctx, a1.ID)
	s.Require().Equal("val", got.Extra["existing"])
	s.Require().Equal("new_val", got.Extra["new_key"])
}

func (s *AccountRepoSuite) TestBulkUpdate_DeleteExtraKeysBeforeMerge() {
	a1 := mustCreateAccount(s.T(), s.client, &service.Account{
		Name: "bulk-extra-delete",
		Extra: map[string]any{
			"keep":                     "val",
			"openai_oauth_passthrough": true,
			"openai_oauth_ws_mode":     "off",
		},
	})

	_, err := s.repo.BulkUpdate(s.ctx, []int64{a1.ID}, service.AccountBulkUpdate{
		ExtraDeleteKeys: []string{"openai_oauth_passthrough"},
		Extra:           map[string]any{"openai_oauth_ws_mode": service.OpenAIOAuthWSModeManagedSession},
	})
	s.Require().NoError(err)

	got, _ := s.repo.GetByID(s.ctx, a1.ID)
	s.Require().Equal("val", got.Extra["keep"])
	s.Require().NotContains(got.Extra, "openai_oauth_passthrough")
	s.Require().Equal(service.OpenAIOAuthWSModeManagedSession, got.Extra["openai_oauth_ws_mode"])
}

func (s *AccountRepoSuite) TestBulkUpdate_EmptyIDs() {
	affected, err := s.repo.BulkUpdate(s.ctx, []int64{}, service.AccountBulkUpdate{})
	s.Require().NoError(err)
	s.Require().Zero(affected)
}

func (s *AccountRepoSuite) TestBulkUpdate_EmptyUpdates() {
	a1 := mustCreateAccount(s.T(), s.client, &service.Account{Name: "bulk-empty"})

	affected, err := s.repo.BulkUpdate(s.ctx, []int64{a1.ID}, service.AccountBulkUpdate{})
	s.Require().NoError(err)
	s.Require().Zero(affected)
}

func idsOfAccounts(accounts []service.Account) []int64 {
	out := make([]int64, 0, len(accounts))
	for i := range accounts {
		out = append(out, accounts[i].ID)
	}
	return out
}

func (s *AccountRepoSuite) TestListAccountCredentialSubset() {
	client := testEntClient(s.T())
	repo := newAccountRepositoryWithSQL(client, integrationDB, nil)
	ctx := s.ctx

	account := &service.Account{
		Name:     "cred-subset-" + strconv.FormatInt(time.Now().UnixNano(), 10),
		Platform: service.PlatformOpenAI,
		Type:     service.AccountTypeOAuth,
		Credentials: map[string]any{
			"email":         "subset@example.com",
			"plan_type":     "plus",
			"model_mapping": map[string]any{"gpt-5": "gpt-5-upstream"},
			"access_token":  "secret-token-must-not-leak",
		},
	}
	s.Require().NoError(repo.Create(ctx, account))
	s.T().Cleanup(func() {
		_, _ = integrationDB.ExecContext(ctx, "DELETE FROM accounts WHERE id = $1", account.ID)
	})

	subsets, err := repo.ListAccountCredentialSubset(ctx, []int64{account.ID})
	s.Require().NoError(err)
	subset := subsets[account.ID]
	s.Require().NotNil(subset)
	s.Require().Equal("subset@example.com", subset["email"])
	s.Require().Equal("plus", subset["plan_type"])
	s.Require().Equal(map[string]any{"gpt-5": "gpt-5-upstream"}, subset["model_mapping"])
	// 安全关键：真实凭据不得出现在列表子集里。
	s.Require().NotContains(subset, "access_token")
	s.Require().NotContains(subset, "refresh_token")
}
