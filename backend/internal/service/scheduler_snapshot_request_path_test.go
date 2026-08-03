//go:build unit

package service

import (
	"context"
	"testing"

	"github.com/Wei-Shaw/sub2api/internal/config"
	"github.com/stretchr/testify/require"
)

// panicSchedulerDBRepo makes an accidental request-path database read fail the test.
type panicSchedulerDBRepo struct {
	AccountRepository
}

func (panicSchedulerDBRepo) GetByID(context.Context, int64) (*Account, error) {
	panic("request path queried DB")
}

func (panicSchedulerDBRepo) GetByIDs(context.Context, []int64) ([]*Account, error) {
	panic("request path queried DB")
}

func (panicSchedulerDBRepo) ListSchedulableByGroupIDAndPlatform(context.Context, int64, string) ([]Account, error) {
	panic("request path queried DB")
}

func (panicSchedulerDBRepo) ListModelAvailabilityCandidates(context.Context, *int64, []string, bool) ([]Account, error) {
	panic("request path queried DB")
}

type missingSchedulerSnapshotCache struct {
	SchedulerCache
}

func (missingSchedulerSnapshotCache) GetSnapshot(context.Context, SchedulerBucket) ([]*Account, bool, error) {
	return nil, false, nil
}

type schedulerSupportSnapshotCache struct {
	SchedulerCache

	support map[SchedulerBucket][]*Account
}

func (c schedulerSupportSnapshotCache) GetPersistentSupport(_ context.Context, bucket SchedulerBucket) ([]*Account, bool, error) {
	accounts, ok := c.support[bucket]
	return accounts, ok, nil
}

func (schedulerSupportSnapshotCache) SetStaticState(context.Context, SchedulerBucket, []Account, []Account) error {
	return nil
}

func schedulerSupportAccount(id int64, platform string) *Account {
	return &Account{
		ID:          id,
		Platform:    platform,
		Type:        AccountTypeAPIKey,
		Status:      StatusActive,
		Schedulable: true,
		Credentials: map[string]any{
			"model_mapping": map[string]any{"supported-model": "supported-model"},
		},
	}
}

func newSupportSnapshotService(groupID *int64, platform string, hasForcePlatform bool, accounts ...*Account) *SchedulerSnapshotService {
	mode := SchedulerModeSingle
	if platform == PlatformAnthropic || platform == PlatformGemini {
		mode = SchedulerModeMixed
	}
	if hasForcePlatform {
		mode = SchedulerModeForced
	}
	bucket := SchedulerBucket{Platform: platform, Mode: mode}
	if groupID != nil {
		bucket.GroupID = *groupID
	}
	cache := schedulerSupportSnapshotCache{support: map[SchedulerBucket][]*Account{bucket: accounts}}
	return newSchedulerSnapshotService(cache, nil, nil, nil, panicSchedulerDBRepo{}, nil, &config.Config{})
}

func TestListSchedulableAccounts_MissingSnapshotDoesNotQueryDB(t *testing.T) {
	svc := newSchedulerSnapshotService(missingSchedulerSnapshotCache{}, nil, nil, nil, panicSchedulerDBRepo{}, nil, &config.Config{})

	_, _, err := svc.ListSchedulableAccounts(context.Background(), nil, PlatformOpenAI, false)

	require.ErrorIs(t, err, ErrSchedulerCacheNotReady)
}

func TestListSchedulableAccounts_EmptySnapshotDoesNotQueryDB(t *testing.T) {
	cache := &snapshotHydrationCache{snapshot: []*Account{}}
	svc := newSchedulerSnapshotService(cache, nil, nil, nil, panicSchedulerDBRepo{}, nil, &config.Config{})

	accounts, _, err := svc.ListSchedulableAccounts(context.Background(), nil, PlatformOpenAI, false)

	require.NoError(t, err)
	require.Empty(t, accounts)
}

func TestOpenAIModelSupportMiss_MissingSupportSnapshotStaysUnavailable(t *testing.T) {
	svc := &OpenAIGatewayService{
		accountRepo:       panicSchedulerDBRepo{},
		schedulerSnapshot: newSchedulerSnapshotService(missingSchedulerSnapshotCache{}, nil, nil, nil, panicSchedulerDBRepo{}, nil, &config.Config{}),
	}

	miss := isPureOpenAIModelSupportMiss(WithPublicModelSupportMiss404(context.Background()), svc, nil, nil, "gpt-test", nil, false, OpenAIEndpointCapabilityChatCompletions, "", OpenAIUpstreamTransportHTTPSSE, nil)

	require.False(t, miss)
}

func TestOpenAIModelSupportMiss_PublishedSupportSnapshotClassifiesPermanentMiss(t *testing.T) {
	svc := &OpenAIGatewayService{
		accountRepo:       panicSchedulerDBRepo{},
		schedulerSnapshot: newSupportSnapshotService(nil, PlatformOpenAI, false, schedulerSupportAccount(1, PlatformOpenAI)),
	}

	miss := isPureOpenAIModelSupportMiss(WithPublicModelSupportMiss404(context.Background()), svc, nil, nil, "gpt-test", nil, false, OpenAIEndpointCapabilityChatCompletions, "", OpenAIUpstreamTransportHTTPSSE, nil)

	require.True(t, miss)
}

func TestGatewayModelSupportMiss_MissingSupportSnapshotStaysUnavailable(t *testing.T) {
	svc := &GatewayService{
		accountRepo:       panicSchedulerDBRepo{},
		schedulerSnapshot: newSchedulerSnapshotService(missingSchedulerSnapshotCache{}, nil, nil, nil, panicSchedulerDBRepo{}, nil, &config.Config{}),
		cfg:               testConfig(),
	}

	miss := svc.isPureModelSupportMiss(WithPublicModelSupportMiss404(context.Background()), nil, "claude-test", PlatformAnthropic, nil, false, nil, nil)

	require.False(t, miss)
}

func TestGatewayModelSupportMiss_PublishedSupportSnapshotClassifiesSinglePlatformMiss(t *testing.T) {
	svc := &GatewayService{
		accountRepo:       panicSchedulerDBRepo{},
		schedulerSnapshot: newSupportSnapshotService(nil, PlatformAnthropic, true, schedulerSupportAccount(1, PlatformAnthropic)),
		cfg:               testConfig(),
	}

	miss := svc.isPureModelSupportMiss(WithPublicModelSupportMiss404(context.Background()), nil, "claude-test", PlatformAnthropic, nil, false, nil, nil)

	require.True(t, miss)
}

func TestGatewayModelSupportMiss_PublishedSupportSnapshotChecksMixedPlatformScope(t *testing.T) {
	svc := &GatewayService{
		accountRepo: panicSchedulerDBRepo{},
		schedulerSnapshot: newSupportSnapshotService(nil, PlatformAnthropic, true,
			schedulerSupportAccount(1, PlatformAnthropic),
			&Account{ID: 2, Platform: PlatformAntigravity, Status: StatusActive, Schedulable: true, Extra: map[string]any{"mixed_scheduling": true}, Credentials: map[string]any{"model_mapping": map[string]any{"claude-test": "claude-test"}}},
		),
		cfg: testConfig(),
	}

	miss := svc.isPureModelSupportMiss(WithPublicModelSupportMiss404(context.Background()), nil, "claude-test", PlatformAnthropic, nil, true, nil, nil)

	require.False(t, miss)
}
