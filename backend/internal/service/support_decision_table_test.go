//go:build unit

package service

import (
	"testing"
	"time"

	"github.com/stretchr/testify/require"
)

func TestSupportDecisionLookupUsesHotDecision(t *testing.T) {
	table := buildSupportDecisionTestTable(t, supportDecisionTestSnapshot([]Account{{Platform: PlatformAnthropic, Credentials: map[string]any{"model_mapping": map[string]any{"other": "other"}}}}, PlatformAnthropic, []string{"hot-model"}))
	got := table.Lookup(SupportDecisionQuery{Scope: SupportDecisionScope{Platform: PlatformAnthropic, GroupID: 42}, RequestedModel: "hot-model"})
	require.Equal(t, SupportDecisionPureMiss, got)
}

func TestSupportDecisionLookupUsesExactFallback(t *testing.T) {
	account := Account{Platform: PlatformAnthropic, Credentials: map[string]any{"model_mapping": map[string]any{"external-exact": "upstream"}}}
	table := buildSupportDecisionTestTable(t, supportDecisionTestSnapshot([]Account{account}, PlatformAnthropic, []string{"hot"}))
	got := table.Lookup(SupportDecisionQuery{Scope: SupportDecisionScope{Platform: PlatformAnthropic, GroupID: 42}, RequestedModel: "external-exact"})
	require.Equal(t, SupportDecisionNotPureMiss, got)
}

func TestSupportDecisionLookupUsesWildcardFallback(t *testing.T) {
	account := Account{Platform: PlatformAnthropic, Credentials: map[string]any{"model_mapping": map[string]any{"vendor-*": "upstream"}}}
	table := buildSupportDecisionTestTable(t, supportDecisionTestSnapshot([]Account{account}, PlatformAnthropic, []string{"hot"}))
	got := table.Lookup(SupportDecisionQuery{Scope: SupportDecisionScope{Platform: PlatformAnthropic, GroupID: 42}, RequestedModel: "vendor-unseen"})
	require.Equal(t, SupportDecisionNotPureMiss, got)
}

func TestSupportDecisionLookupUsesAllowAllFactsForUnseenModel(t *testing.T) {
	table := buildSupportDecisionTestTable(t, supportDecisionTestSnapshot([]Account{{Platform: PlatformAnthropic}}, PlatformAnthropic, []string{"hot"}))
	got := table.Lookup(SupportDecisionQuery{Scope: SupportDecisionScope{Platform: PlatformAnthropic, GroupID: 42}, RequestedModel: "completely-unseen"})
	require.Equal(t, SupportDecisionNotPureMiss, got)
}

func TestSupportDecisionLookupUsesOpenAIOAuthNormalizationForUnseenAlias(t *testing.T) {
	table := buildSupportDecisionTestTable(t, supportDecisionTestSnapshot([]Account{{Platform: PlatformOpenAI, Type: AccountTypeOAuth}}, PlatformOpenAI, []string{"hot"}))
	got := table.Lookup(SupportDecisionQuery{Scope: SupportDecisionScope{Platform: PlatformOpenAI, GroupID: 42}, RequestedModel: "openai/gpt-5-codex"})
	require.Equal(t, SupportDecisionNotPureMiss, got)
}

func TestSupportDecisionLookupDoesNotReturnUnknownOnlyBecauseModelIsUnseen(t *testing.T) {
	account := Account{Platform: PlatformAnthropic, Credentials: map[string]any{"model_mapping": map[string]any{"known": "known"}}}
	table := buildSupportDecisionTestTable(t, supportDecisionTestSnapshot([]Account{account}, PlatformAnthropic, []string{"hot"}))
	got := table.Lookup(SupportDecisionQuery{Scope: SupportDecisionScope{Platform: PlatformAnthropic, GroupID: 42}, RequestedModel: "unseen"})
	require.Equal(t, SupportDecisionPureMiss, got)
}

func TestSupportDecisionLookupGenericAndOpenAICoordinatesRemainDistinct(t *testing.T) {
	generic := buildSupportDecisionTestTable(t, supportDecisionTestSnapshot([]Account{{Platform: PlatformAnthropic, Credentials: map[string]any{"model_mapping": map[string]any{"known": "known"}}}}, PlatformAnthropic, []string{"unseen"}))
	base := SupportDecisionQuery{Scope: SupportDecisionScope{Platform: PlatformAnthropic, GroupID: 42}, RequestedModel: "unseen"}
	withOpenAIFields := base
	withOpenAIFields.EndpointCapability = OpenAIEndpointCapabilityEmbeddings
	withOpenAIFields.ImageCapability = OpenAIImagesCapabilityNative
	withOpenAIFields.RequireCompact = true
	withOpenAIFields.Transport = OpenAIUpstreamTransportResponsesWebsocketV2
	require.Equal(t, generic.Lookup(base), generic.Lookup(withOpenAIFields))

	openai := buildSupportDecisionTestTable(t, supportDecisionTestSnapshot([]Account{{Platform: PlatformOpenAI, Type: AccountTypeAPIKey, Credentials: map[string]any{"model_mapping": map[string]any{"known": "known"}}}}, PlatformOpenAI, []string{"unseen"}))
	openAIBase := SupportDecisionQuery{Scope: SupportDecisionScope{Platform: PlatformOpenAI, GroupID: 42}, RequestedModel: "unseen"}
	thinking := openAIBase
	thinking.ThinkingEnabled = true
	require.Equal(t, openai.Lookup(openAIBase), openai.Lookup(thinking))
}

func TestSupportDecisionHotLookupAllocations(t *testing.T) {
	table := buildSupportDecisionTestTable(t, supportDecisionTestSnapshot([]Account{{Platform: PlatformAnthropic}}, PlatformAnthropic, []string{"hot"}))
	query := SupportDecisionQuery{Scope: SupportDecisionScope{Platform: PlatformAnthropic, GroupID: 42}, RequestedModel: "hot"}
	allocations := testing.AllocsPerRun(1000, func() {
		if table.Lookup(query) == SupportDecisionUnknown {
			t.Fatal("unexpected unknown result")
		}
	})
	require.Zero(t, allocations)
}

func TestSupportDecisionHotLookupDoesNotConsultSource(t *testing.T) {
	table := buildSupportDecisionTestTable(t, supportDecisionTestSnapshot([]Account{{Platform: PlatformAnthropic}}, PlatformAnthropic, []string{"hot"}))
	snapshot := supportDecisionTestSnapshot(nil, PlatformAnthropic, []string{"other"})
	snapshot.Accounts = nil
	require.Equal(t, SupportDecisionNotPureMiss, table.Lookup(SupportDecisionQuery{Scope: SupportDecisionScope{Platform: PlatformAnthropic, GroupID: 42}, RequestedModel: "hot"}))
}

func TestSupportDecisionHotLookupDoesNotDecodeDocument(t *testing.T) {
	table := buildSupportDecisionTestTable(t, supportDecisionTestSnapshot([]Account{{Platform: PlatformAnthropic}}, PlatformAnthropic, []string{"hot"}))
	payload, err := EncodeSupportDecisionDocument(table)
	require.NoError(t, err)
	payload[0] = '!'
	require.Equal(t, SupportDecisionNotPureMiss, table.Lookup(SupportDecisionQuery{Scope: SupportDecisionScope{Platform: PlatformAnthropic, GroupID: 42}, RequestedModel: "hot"}))
}

func TestSupportDecisionAtomicReaderInstallFreezesMutableInput(t *testing.T) {
	table := buildSupportDecisionTestTable(t, supportDecisionTestSnapshot([]Account{{Platform: PlatformAnthropic}}, PlatformAnthropic, []string{"hot"}))
	query := SupportDecisionQuery{Scope: SupportDecisionScope{Platform: PlatformAnthropic, GroupID: 42}, RequestedModel: "hot"}
	want := table.Lookup(query)
	before, err := EncodeSupportDecisionDocument(table)
	require.NoError(t, err)

	reader := NewSupportDecisionAtomicReader(time.Minute)
	now := time.Unix(100, 0)
	reader.now = func() time.Time { return now }
	require.True(t, reader.Install(table, now))

	table.Strings[0] = "mutated"
	table.Scopes[table.scopeIndex[supportDecisionScopeKey{Platform: PlatformAnthropic, GroupID: 42}]].Hot[0].Profile = supportDecisionProfile{}
	require.Equal(t, want, table.Lookup(query))
	require.Equal(t, want, reader.Lookup(query))
	installed, err := EncodeSupportDecisionDocument(reader.table.Load())
	require.NoError(t, err)
	require.Equal(t, before, installed)
	encoded, err := EncodeSupportDecisionDocument(table)
	require.NoError(t, err)
	require.Equal(t, before, encoded)
}

func TestSupportDecisionLookupRejectsUnavailableStaleInvalidAndOverlongState(t *testing.T) {
	query := SupportDecisionQuery{Scope: SupportDecisionScope{Platform: PlatformAnthropic, GroupID: 42}, RequestedModel: "hot"}
	var missing *SupportDecisionTable
	require.Equal(t, SupportDecisionUnknown, missing.Lookup(query))

	table := buildSupportDecisionTestTable(t, supportDecisionTestSnapshot([]Account{{Platform: PlatformAnthropic}}, PlatformAnthropic, []string{"hot"}))
	invalidScope := query
	invalidScope.Scope.GroupID = 99
	require.Equal(t, SupportDecisionUnknown, table.Lookup(invalidScope))
	overlong := query
	overlong.RequestedModel = stringsOfLength(SupportDecisionMaxModelBytes + 1)
	require.Equal(t, SupportDecisionUnknown, table.Lookup(overlong))

	now := time.Unix(100, 0)
	reader := NewSupportDecisionAtomicReader(30 * time.Second)
	reader.now = func() time.Time { return now }
	require.True(t, reader.Install(table, now))
	require.NotEqual(t, SupportDecisionUnknown, reader.Lookup(query))
	now = now.Add(31 * time.Second)
	require.Equal(t, SupportDecisionUnknown, reader.Lookup(query))
}
