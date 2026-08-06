//go:build unit

package service

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/Wei-Shaw/sub2api/internal/config"
	"github.com/Wei-Shaw/sub2api/internal/workerruntime"
	"github.com/stretchr/testify/require"
)

func TestSupportDecisionShadowExercisesReachableOverlappingWildcardParent(t *testing.T) {
	snapshot := supportDecisionTestSnapshot([]Account{{Platform: PlatformAnthropic, Credentials: map[string]any{"model_mapping": map[string]any{
		"parent-*": "parent-target", "parent-x*": "child-x", "parent--*": "child-dash",
	}}}}, PlatformAnthropic, nil)
	options := SupportDecisionBuildOptions{Generation: 1}
	table := buildSupportDecisionTestTable(t, snapshot, options)
	broken := cloneSupportDecisionTableForReview(t, table)
	broken.shadowScopes = table.shadowScopes
	scope := supportDecisionScopeForReview(t, broken, PlatformAnthropic, 42)
	for i := range scope.Wildcard {
		if broken.runtimeStrings[scope.Wildcard[i].PrefixID] == "parent-" {
			corruptSupportDecisionProfileForReview(&scope.Wildcard[i].Profile)
		}
	}
	_, err := VerifySupportDecisionShadow(context.Background(), snapshot, options, broken)
	require.ErrorIs(t, err, ErrSupportDecisionShadowMismatch)
}

func TestSupportDecisionShadowDetectsPublishedBranchCorruption(t *testing.T) {
	channel := SupportDecisionChannel{Status: StatusActive, RestrictModels: true, BillingModelSource: BillingModelSourceUpstream, GroupIDs: []int64{42}, PricingModels: []SupportDecisionPricingModels{{Platform: PlatformAnthropic, Models: []string{"DIRECT", "claude-sonnet-4.5*"}}}}
	snapshot := supportDecisionTestSnapshot([]Account{{Platform: PlatformAnthropic, Credentials: map[string]any{"model_mapping": map[string]any{"direct": "direct", "claude-sonnet-4-5-20250929": "claude-sonnet-4-5-20250929", "wild-*": "wild-target", "*": "catch-target"}}}}, PlatformAnthropic, nil)
	snapshot.Channels = []SupportDecisionChannel{channel}
	options := SupportDecisionBuildOptions{Generation: 2}
	table := buildSupportDecisionTestTable(t, snapshot, options)
	_, err := VerifySupportDecisionShadow(context.Background(), snapshot, options, table)
	require.NoError(t, err)
	tests := []struct {
		name       string
		breakTable func(*SupportDecisionTable)
	}{
		{"exact", func(b *SupportDecisionTable) {
			scope := supportDecisionScopeForReview(t, b, PlatformAnthropic, 42)
			corruptSupportDecisionProfileForReview(&scope.ExactWire[0].Profile)
			model := b.runtimeStrings[scope.ExactWire[0].StringID]
			value := scope.Exact[model]
			value.Profile = scope.ExactWire[0].Profile
			scope.Exact[model] = value
		}},
		{"wildcard", func(b *SupportDecisionTable) {
			corruptSupportDecisionProfileForReview(&supportDecisionScopeForReview(t, b, PlatformAnthropic, 42).Wildcard[0].Profile)
		}},
		{"catch_all", func(b *SupportDecisionTable) {
			corruptSupportDecisionProfileForReview(&supportDecisionScopeForReview(t, b, PlatformAnthropic, 42).CatchAll.Profile)
		}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			broken := cloneSupportDecisionTableForReview(t, table)
			broken.shadowScopes = table.shadowScopes
			tt.breakTable(broken)
			_, err := VerifySupportDecisionShadow(context.Background(), snapshot, options, broken)
			require.ErrorIs(t, err, ErrSupportDecisionShadowMismatch)
		})
	}
}

func TestSupportDecisionShadowDetectsChannelCaseAndRevisionAliases(t *testing.T) {
	for _, pattern := range []string{"claude-sonnet-4.5", "claude-sonnet-4.5*"} {
		t.Run(pattern, func(t *testing.T) {
			channel := SupportDecisionChannel{Status: StatusActive, RestrictModels: true, BillingModelSource: BillingModelSourceUpstream, GroupIDs: []int64{42}, PricingModels: []SupportDecisionPricingModels{{Platform: PlatformAnthropic, Models: []string{pattern}}}}
			snapshot := supportDecisionTestSnapshot([]Account{{Platform: PlatformAnthropic, Extra: map[string]any{"privacy_mode": PrivacyModeTrainingOff}, Credentials: map[string]any{"model_mapping": map[string]any{"other": "other"}}}}, PlatformAnthropic, nil)
			snapshot.Channels = []SupportDecisionChannel{channel}
			options := SupportDecisionBuildOptions{Generation: 23}
			table := buildSupportDecisionTestTable(t, snapshot, options)
			broken := cloneSupportDecisionTableForReview(t, table)
			broken.shadowScopes = table.shadowScopes
			profile := &supportDecisionScopeForReview(t, broken, PlatformAnthropic, 42).ChannelAllowed
			profile.EligibleBits[0] ^= 1
			_, err := VerifySupportDecisionShadow(context.Background(), snapshot, options, broken)
			require.ErrorIs(t, err, ErrSupportDecisionShadowMismatch)
		})
	}
}

func TestSupportDecisionShadowDetectsAntigravityAliasesAcrossScopesAndCoordinates(t *testing.T) {
	account := Account{Platform: PlatformAntigravity, Credentials: map[string]any{"model_mapping": map[string]any{"ag-exact": "ag-exact", "ag-wild-*": "ag-target"}}, Extra: map[string]any{"mixed_scheduling": true, "privacy_mode": PrivacyModeTrainingOff}}
	snapshot := supportDecisionTestSnapshot([]Account{account}, PlatformAnthropic, nil)
	options := SupportDecisionBuildOptions{Generation: 24}
	table := buildSupportDecisionTestTable(t, snapshot, options)
	for _, key := range []supportDecisionScopeKey{
		{Platform: PlatformAntigravity, IncludeGrouped: true},
		{Platform: PlatformAnthropic, GroupID: 42, AllowMixedScheduling: true},
		{Platform: PlatformAnthropic, IncludeGrouped: true, AllowMixedScheduling: true},
	} {
		t.Run(fmt.Sprintf("%+v", key), func(t *testing.T) {
			broken := cloneSupportDecisionTableForReview(t, table)
			broken.shadowScopes = table.shadowScopes
			index, ok := broken.scopeIndex[key]
			require.True(t, ok)
			scope := &broken.runtimeScopes[index]
			var corrupted bool
			for i := range scope.Wildcard {
				if strings.HasPrefix(broken.runtimeStrings[scope.Wildcard[i].PrefixID], "models/ag-wild-") {
					for coordinate := 0; coordinate < supportDecisionGenericCoordinateCount; coordinate++ {
						if profileResult(scope.Wildcard[i].Profile, coordinate) != SupportDecisionPureMiss {
							setBit(scope.Wildcard[i].Profile.EligibleBits, coordinate)
							scope.Wildcard[i].Profile.SupportBits[coordinate>>3] &^= 1 << uint(coordinate&7)
							corrupted = true
							break
						}
					}
					break
				}
			}
			require.True(t, corrupted)
			_, err := VerifySupportDecisionShadow(context.Background(), snapshot, options, broken)
			require.ErrorIs(t, err, ErrSupportDecisionShadowMismatch)
		})
	}
}

func TestSupportDecisionShadowDetectsNormalizedFamilyCorruption(t *testing.T) {
	tests := []struct {
		name     string
		snapshot *SupportDecisionConstructionSnapshot
		platform string
		corrupt  func(*supportDecisionScopeTable)
	}{
		{"codex_direct_namespaced_suffix", supportDecisionTestSnapshot([]Account{{Platform: PlatformOpenAI, Type: AccountTypeOAuth}}, PlatformOpenAI, nil), PlatformOpenAI, func(s *supportDecisionScopeTable) { s.KnownCodexSupport[0] ^= 1 }},
		{"bedrock_default_direct_regional", supportDecisionTestSnapshot([]Account{{Platform: PlatformAnthropic, Type: AccountTypeBedrock, Credentials: map[string]any{"model_mapping": map[string]any{"other": "other"}}}}, PlatformAnthropic, nil), PlatformAnthropic, func(s *supportDecisionScopeTable) {
			for i := range s.KnownBedrockSupport {
				s.KnownBedrockSupport[i] = ^s.KnownBedrockSupport[i]
			}
		}},
		{"antigravity_exact_wildcard_alias", supportDecisionTestSnapshot([]Account{{Platform: PlatformAntigravity, Credentials: map[string]any{"model_mapping": map[string]any{"ag-exact": "ag-exact", "ag-wild-*": "ag-target"}}}}, PlatformAntigravity, nil), PlatformAntigravity, func(s *supportDecisionScopeTable) { corruptSupportDecisionProfileForReview(&s.Wildcard[0].Profile) }},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			options := SupportDecisionBuildOptions{Generation: 3}
			table := buildSupportDecisionTestTable(t, tt.snapshot, options)
			broken := cloneSupportDecisionTableForReview(t, table)
			broken.shadowScopes = table.shadowScopes
			scope := supportDecisionScopeForReview(t, broken, tt.platform, 42)
			tt.corrupt(scope)
			_, err := VerifySupportDecisionShadow(context.Background(), tt.snapshot, options, broken)
			require.ErrorIs(t, err, ErrSupportDecisionShadowMismatch)
		})
	}
}

func TestSupportDecisionShadowLegacyOracleWorkIsAggregated(t *testing.T) {
	mapping := make(map[string]any, 64)
	for i := 0; i < 64; i++ {
		mapping[fmt.Sprintf("model-%03d", i)] = "target"
	}
	accounts := make([]Account, 1000)
	for i := range accounts {
		accounts[i] = Account{Platform: PlatformOpenAI, Type: AccountTypeOAuth, Credentials: map[string]any{"model_mapping": mapping}}
	}
	snapshot := supportDecisionTestSnapshot(accounts, PlatformOpenAI, nil)
	options := SupportDecisionBuildOptions{Generation: 4}
	table := buildSupportDecisionTestTable(t, snapshot, options)
	var operations uint64
	ctx := context.WithValue(context.Background(), supportDecisionShadowOperationCounterKey{}, &operations)
	checks, err := VerifySupportDecisionShadow(ctx, snapshot, options, table)
	require.NoError(t, err)
	require.Positive(t, checks)
	require.Less(t, operations, uint64(len(accounts))*20, "oracle account scans must not multiply by models and coordinates")
}

func TestSupportDecisionWildcardComplementWitnessIsDeterministicAndExhaustive(t *testing.T) {
	stringsTable := []string{"p", "p\x00", "p\x01", "p\x00\x00"}
	scope := &supportDecisionScopeTable{Wildcard: []supportDecisionWildcard{{PrefixID: 3}, {PrefixID: 1}, {PrefixID: 2}, {PrefixID: 0}}}
	first, reachable, valid := supportDecisionShadowPrefixWitness(stringsTable, scope, 0)
	require.True(t, valid)
	require.True(t, reachable)
	require.Equal(t, "p\x02", first)
	for range 20 {
		got, ok, valid := supportDecisionShadowPrefixWitness(stringsTable, scope, 0)
		require.True(t, valid)
		require.True(t, ok)
		require.Equal(t, first, got)
	}

	fullyCoveredStrings := []string{"p", "p\x00", "p\x01"}
	fullyCovered := &supportDecisionScopeTable{Wildcard: []supportDecisionWildcard{{PrefixID: 1}, {PrefixID: 2}, {PrefixID: 0}}}
	// Model strings permit every byte, but a bounded table cannot contain 256
	// children here. Exercise a truly covered finite domain at the max length.
	fullyCoveredStrings[0] = strings.Repeat("p", SupportDecisionMaxModelBytes-1)
	fullyCoveredStrings[1] = fullyCoveredStrings[0] + "\x00"
	fullyCoveredStrings[2] = fullyCoveredStrings[0] + "\x01"
	_, reachable, valid = supportDecisionShadowPrefixWitness(fullyCoveredStrings, fullyCovered, 0)
	require.True(t, valid)
	require.True(t, reachable, "unrepresented byte values remain a reachable complement")
}

func TestSupportDecisionReplicaFetchCancellationIdentityAndSanitization(t *testing.T) {
	for _, tt := range []struct {
		name  string
		cause error
	}{{"canceled", context.Canceled}, {"deadline", context.DeadlineExceeded}} {
		t.Run(tt.name, func(t *testing.T) {
			h := newSupportDecisionReplicaHarness(t)
			h.store.setActive(2, nil)
			h.store.documentErr = fmt.Errorf("credential=secret: %w", tt.cause)
			err := h.replica.refresh(context.Background())
			require.ErrorIs(t, err, tt.cause)
			require.NotContains(t, err.Error(), "secret")
		})
	}
}

func TestSupportDecisionPublisherRuntimeStatusFailureThenSuccess(t *testing.T) {
	now := time.Unix(1000, 0)
	generation := &supportDecisionPublisherGenerationStub{generations: []uint64{1, 2}}
	source := &supportDecisionPublisherSourceStub{errs: []error{errors.New("secret"), nil}, snapshots: []*SupportDecisionConstructionSnapshot{nil, emptySupportDecisionPublisherSnapshot()}}
	publisher := newSupportDecisionPublisherTestSubject(generation, source, &supportDecisionPublisherStoreStub{})
	publisher.now = func() time.Time { return now }
	worker := newSchedulerSupportPublisherWorker(nil, nil, nil, publisher, time.Second, immediateSchedulerSupportPublisherClock{})
	_, _ = publisher.Publish(context.Background())
	first := worker.Snapshot().Status.(workerruntime.PeriodicStatus)
	require.Equal(t, workerruntime.OutcomeError, first.LastOutcome)
	now = now.Add(time.Second)
	_, err := publisher.Publish(context.Background())
	require.NoError(t, err)
	status := worker.Snapshot().Status
	require.NotNil(t, status)
	periodic := status.(workerruntime.PeriodicStatus)
	require.Equal(t, workerruntime.OutcomeSuccess, periodic.LastOutcome)
	require.Equal(t, now, periodic.LastRunAt)
	require.Equal(t, uint64(1), periodic.ErrorCount)
}

func TestSupportDecisionReplicaRuntimeStatusFailureThenSuccessAndSubscriptionFailure(t *testing.T) {
	h := newSupportDecisionReplicaHarness(t)
	h.addDocument(t, 1, []Account{{Platform: PlatformAnthropic}})
	h.store.setActive(1, errors.New("credential=secret"))
	require.Error(t, h.replica.refresh(context.Background()))
	worker := NewSupportDecisionReplicaWorker(h.replica)
	failed := worker.Snapshot().Status.(workerruntime.PeriodicStatus)
	require.Equal(t, workerruntime.OutcomeError, failed.LastOutcome)
	require.Equal(t, uint64(1), failed.ErrorCount)

	h.now = h.now.Add(time.Second)
	h.store.setActive(1, nil)
	require.NoError(t, h.replica.refresh(context.Background()))
	succeeded := worker.Snapshot().Status.(workerruntime.PeriodicStatus)
	require.Equal(t, workerruntime.OutcomeSuccess, succeeded.LastOutcome)
	require.Equal(t, uint64(1), succeeded.ErrorCount)
	require.Equal(t, h.now, succeeded.LastRunAt)

	h2 := newSupportDecisionReplicaHarness(t)
	// Inject a concrete subscription failure through a minimal store wrapper.
	failing := &supportDecisionSubscriptionFailureStore{SupportDecisionPublicationStore: h2.store}
	replica := newSupportDecisionReplica(failing, h2.reader, h2.replica.deps)
	require.Error(t, replica.Start(context.Background()))
	status := NewSupportDecisionReplicaWorker(replica).Snapshot().Status.(workerruntime.PeriodicStatus)
	require.Equal(t, uint64(1), status.ErrorCount)
	require.Equal(t, workerruntime.OutcomeError, status.LastOutcome)
	require.False(t, status.LastRunAt.IsZero())
}

type supportDecisionSubscriptionFailureStore struct {
	SupportDecisionPublicationStore
}

func (s *supportDecisionSubscriptionFailureStore) SubscribeWakeups(context.Context) (SupportDecisionWakeupSubscription, error) {
	return nil, errors.New("credential=secret")
}

func TestSupportDecisionSnapshotsConcurrentCoherent(t *testing.T) {
	generation := &supportDecisionPublisherGenerationStub{generations: make([]uint64, 100)}
	for i := range generation.generations {
		generation.generations[i] = uint64(i + 1)
	}
	source := &supportDecisionPublisherSourceStub{snapshots: make([]*SupportDecisionConstructionSnapshot, 100)}
	for i := range source.snapshots {
		source.snapshots[i] = emptySupportDecisionPublisherSnapshot()
	}
	publisher := newSupportDecisionPublisherTestSubject(generation, source, &supportDecisionPublisherStoreStub{})
	var wg sync.WaitGroup
	wg.Add(2)
	go func() {
		defer wg.Done()
		for range 100 {
			_, _ = publisher.Publish(context.Background())
		}
	}()
	go func() {
		defer wg.Done()
		for range 1000 {
			s := publisher.Snapshot()
			require.LessOrEqual(t, s.SuccessfulActivations, s.Attempts)
		}
	}()
	wg.Wait()
}

func corruptSupportDecisionProfileForReview(profile *supportDecisionProfile) {
	for i := range profile.SupportBits {
		profile.SupportBits[i] = 0
	}
	for i := range profile.EligibleBits {
		profile.EligibleBits[i] = 0xff
	}
}

func cloneSupportDecisionTableForReview(t *testing.T, table *SupportDecisionTable) *SupportDecisionTable {
	t.Helper()
	payload, err := EncodeSupportDecisionDocument(table)
	require.NoError(t, err)
	cloned, err := DecodeSupportDecisionDocument(payload, table.Generation)
	require.NoError(t, err)
	return cloned
}

func supportDecisionScopeForReview(t *testing.T, table *SupportDecisionTable, platform string, groupID int64) *supportDecisionScopeTable {
	t.Helper()
	index, ok := table.scopeIndex[supportDecisionScopeKey{Platform: platform, GroupID: groupID}]
	require.True(t, ok)
	return &table.runtimeScopes[index]
}

func BenchmarkSupportDecisionShadowRepresentative(b *testing.B) {
	const modelCount = 4096
	snapshot := supportDecisionTestSnapshot([]Account{{Platform: PlatformOpenAI, Type: AccountTypeOAuth}}, PlatformOpenAI, nil)
	options := SupportDecisionBuildOptions{Generation: 5, OpenAIWS: config.GatewayOpenAIWSConfig{}}
	table := buildSupportDecisionTestTable(b, snapshot, options)
	key := supportDecisionScopeKey{Platform: PlatformOpenAI, GroupID: 42}
	index := table.scopeIndex[key]
	runtimeScope := &table.runtimeScopes[index]
	var aggregate *supportDecisionTemporaryScope
	for i := range table.shadowScopes {
		if table.shadowScopes[i].key == key {
			aggregate = &table.shadowScopes[i]
			break
		}
	}
	require.NotNil(b, aggregate)
	byteCount := (supportDecisionOpenAICoordinateCount + 7) / 8
	for i := 0; i < modelCount; i++ {
		model := fmt.Sprintf("model-%04d", i)
		table.runtimeStrings = append(table.runtimeStrings, model, "target")
		modelID, targetID := uint32(len(table.runtimeStrings)-2), uint32(len(table.runtimeStrings)-1)
		profile := supportDecisionProfile{SupportBits: make([]byte, byteCount), EligibleBits: make([]byte, byteCount)}
		runtimeScope.ExactWire = append(runtimeScope.ExactWire, supportDecisionExactEntry{StringID: modelID, TargetID: targetID, Profile: profile})
		runtimeScope.Exact[model] = supportDecisionExactValue{TargetID: targetID, Profile: profile}
		aggregate.exact[model] = supportDecisionTemporaryRule{target: "target", profile: profile}
	}
	aggregate.sourceAccountCount = 30000
	var operations uint64
	ctx := context.WithValue(context.Background(), supportDecisionShadowOperationCounterKey{}, &operations)
	checks, err := VerifySupportDecisionShadow(ctx, snapshot, options, table)
	require.NoError(b, err)
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		_, _ = VerifySupportDecisionShadow(context.Background(), snapshot, options, table)
	}
	b.StopTimer()
	b.ReportMetric(float64(checks), "checks/run")
	b.ReportMetric(float64(operations), "operations/run")
}

func BenchmarkSupportDecisionAtomicReaderLookup(b *testing.B) {
	table := buildSupportDecisionTestTable(b, supportDecisionTestSnapshot([]Account{{Platform: PlatformAnthropic}}, PlatformAnthropic, []string{"hot"}))
	reader := NewSupportDecisionAtomicReader(30 * time.Second)
	now := time.Unix(1, 0)
	reader.now = func() time.Time { return now }
	reader.Install(table, now)
	query := SupportDecisionQuery{Scope: SupportDecisionScope{Platform: PlatformAnthropic, GroupID: 42}, RequestedModel: "hot"}
	b.ReportAllocs()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		_ = reader.Lookup(query)
	}
}

func BenchmarkSupportDecisionAtomicReaderLookupParallel(b *testing.B) {
	table := buildSupportDecisionTestTable(b, supportDecisionTestSnapshot([]Account{{Platform: PlatformAnthropic}}, PlatformAnthropic, []string{"hot"}))
	reader := NewSupportDecisionAtomicReader(30 * time.Second)
	now := time.Unix(1, 0)
	reader.now = func() time.Time { return now }
	reader.Install(table, now)
	query := SupportDecisionQuery{Scope: SupportDecisionScope{Platform: PlatformAnthropic, GroupID: 42}, RequestedModel: "hot"}
	b.ReportAllocs()
	b.ResetTimer()
	b.RunParallel(func(pb *testing.PB) {
		for pb.Next() {
			_ = reader.Lookup(query)
		}
	})
}
