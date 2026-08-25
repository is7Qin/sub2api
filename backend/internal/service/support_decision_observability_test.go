//go:build unit

package service

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
)

func TestSupportDecisionPublisherSnapshotTracksBoundedOutcomes(t *testing.T) {
	now := time.Unix(500, 0)
	generation := &supportDecisionPublisherGenerationStub{generations: []uint64{1, 2}}
	source := &supportDecisionPublisherSourceStub{
		snapshots: []*SupportDecisionConstructionSnapshot{emptySupportDecisionPublisherSnapshot()},
		errs:      []error{nil, errors.New("credential=publisher-secret model=leak-model")},
	}
	store := &supportDecisionPublisherStoreStub{}
	publisher := newSupportDecisionPublisherTestSubject(generation, source, store)
	publisher.now = func() time.Time { return now }

	got, err := publisher.Publish(context.Background())
	require.NoError(t, err)
	require.Equal(t, uint64(1), got)
	now = now.Add(time.Second)
	_, err = publisher.Publish(context.Background())
	require.Error(t, err)
	require.NotContains(t, err.Error(), "publisher-secret")
	require.NotContains(t, err.Error(), "leak-model")

	snapshot := publisher.Snapshot()
	require.Equal(t, uint64(2), snapshot.Attempts)
	require.Equal(t, uint64(1), snapshot.SuccessfulActivations)
	require.Equal(t, uint64(1), snapshot.LastSuccessfulGeneration)
	require.Equal(t, time.Unix(500, 0), snapshot.LastSuccessfulAt)
	require.Positive(t, snapshot.DocumentBytes)
	require.Positive(t, snapshot.ScopeCount)
	require.False(t, snapshot.Active)
	require.Equal(t, uint64(1), snapshot.FailureCount(SupportDecisionPublisherStageSource, SupportDecisionErrorOperation))
	for _, secret := range []string{"publisher-secret", "leak-model"} {
		require.NotContains(t, snapshot.String(), secret)
	}
}

func TestSupportDecisionPublisherShadowMismatchFailsClosedAndIsSanitized(t *testing.T) {
	generation := &supportDecisionPublisherGenerationStub{generations: []uint64{7}}
	source := &supportDecisionPublisherSourceStub{snapshots: []*SupportDecisionConstructionSnapshot{emptySupportDecisionPublisherSnapshot()}}
	store := &supportDecisionPublisherStoreStub{}
	publisher := newSupportDecisionPublisherTestSubject(generation, source, store)
	publisher.shadow = func(context.Context, *SupportDecisionConstructionSnapshot, SupportDecisionBuildOptions, *SupportDecisionTable) (uint64, error) {
		return 13, errors.New("group=991 model=private-shadow-model credential=secret")
	}

	_, err := publisher.Publish(context.Background())
	require.Error(t, err)
	require.Contains(t, err.Error(), "stage=shadow")
	require.NotContains(t, err.Error(), "private-shadow-model")
	require.NotContains(t, err.Error(), "991")
	require.NotContains(t, err.Error(), "secret")
	require.Zero(t, store.putCalls)
	require.Zero(t, store.activates)
	snapshot := publisher.Snapshot()
	require.Equal(t, uint64(1), snapshot.FailureCount(SupportDecisionPublisherStageShadow, SupportDecisionErrorMismatch))
}

func TestSupportDecisionPublisherShadowCancellationHasNoRedisSideEffects(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	generation := &supportDecisionPublisherGenerationStub{generations: []uint64{8}}
	source := &supportDecisionPublisherSourceStub{snapshots: []*SupportDecisionConstructionSnapshot{emptySupportDecisionPublisherSnapshot()}}
	store := &supportDecisionPublisherStoreStub{}
	publisher := newSupportDecisionPublisherTestSubject(generation, source, store)
	publisher.shadow = func(context.Context, *SupportDecisionConstructionSnapshot, SupportDecisionBuildOptions, *SupportDecisionTable) (uint64, error) {
		cancel()
		return 0, context.Canceled
	}

	_, err := publisher.Publish(ctx)
	require.ErrorIs(t, err, context.Canceled)
	require.Zero(t, store.putCalls)
	require.Zero(t, store.activates)
}

func TestSupportDecisionShadowMatchesRepresentedRulesAndCoordinates(t *testing.T) {
	snapshot := supportDecisionTestSnapshot([]Account{
		{Platform: PlatformAnthropic, Credentials: map[string]any{"model_mapping": map[string]any{"exact-shadow": "upstream", "wild-shadow*": "target"}}},
		{Platform: PlatformOpenAI, Type: AccountTypeAPIKey, Credentials: map[string]any{"model_mapping": map[string]any{"openai-shadow": "upstream"}}},
	}, PlatformAnthropic, []string{"exact-shadow", "wild-shadow-value"})
	options := SupportDecisionBuildOptions{Generation: 11}
	table := buildSupportDecisionTestTable(t, snapshot, options)

	checks, err := VerifySupportDecisionShadow(context.Background(), snapshot, options, table)
	require.NoError(t, err)
	require.Positive(t, checks)

	broken := *table
	broken.runtimeScopes = cloneSupportDecisionScopes(table.runtimeScopes)
	broken.runtimeScopes[0].Default.EligibleBits[0] ^= 1
	_, err = VerifySupportDecisionShadow(context.Background(), snapshot, options, &broken)
	require.ErrorIs(t, err, ErrSupportDecisionShadowMismatch)
	require.NotContains(t, strings.ToLower(err.Error()), "shadow-value")
}

func TestSupportDecisionReplicaAndReaderSnapshotsAreBounded(t *testing.T) {
	h := newSupportDecisionReplicaHarness(t)
	h.addDocument(t, 1, []Account{{Platform: PlatformAnthropic}})
	h.store.setActive(1, nil)
	h.start(t)

	readerBefore := h.reader.Snapshot()
	require.False(t, readerBefore.Unknown)
	require.False(t, readerBefore.Stale)
	require.Equal(t, uint64(1), readerBefore.Generation)
	_ = h.reader.Lookup(h.query)
	_ = h.reader.Lookup(SupportDecisionQuery{})
	readerAfter := h.reader.Snapshot()
	require.Equal(t, uint64(2), readerAfter.TotalLookups)
	require.Equal(t, uint64(1), readerAfter.NotPureMissLookups)
	require.Equal(t, uint64(1), readerAfter.UnknownLookups)

	h.store.setActive(0, errors.New("redis key customer-123 model secret-model"))
	h.tickAndWait(t)
	replica := h.replica.Snapshot()
	require.Equal(t, uint64(2), replica.Polls)
	require.Equal(t, uint64(1), replica.SuccessfulInstalls)
	require.Equal(t, uint64(1), replica.ActiveGenerationFailures)
	require.Equal(t, uint64(1), replica.InstalledGeneration)
	require.Positive(t, replica.DocumentBytes)
	require.False(t, replica.Unknown)
	require.NotContains(t, replica.String(), "customer-123")
	require.NotContains(t, replica.String(), "secret-model")
}

func TestSupportDecisionAtomicReaderMetricsRemainAllocationFreeAndAccountIndependent(t *testing.T) {
	for _, accountCount := range []int{1, 1000} {
		accounts := make([]Account, accountCount)
		for i := range accounts {
			accounts[i] = Account{Platform: PlatformAnthropic}
		}
		table := buildSupportDecisionTestTable(t, supportDecisionTestSnapshot(accounts, PlatformAnthropic, []string{"hot"}))
		reader := NewSupportDecisionAtomicReader(30 * time.Second)
		now := time.Unix(900, 0)
		reader.now = func() time.Time { return now }
		require.True(t, reader.Install(table, now))
		query := SupportDecisionQuery{Scope: SupportDecisionScope{Platform: PlatformAnthropic, GroupID: 42}, RequestedModel: "hot"}
		require.Zero(t, testing.AllocsPerRun(1000, func() { _ = reader.Lookup(query) }))
	}
}
