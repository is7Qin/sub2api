//go:build unit

package service

import (
	"bytes"
	"encoding/binary"
	"encoding/json"
	"testing"

	"github.com/stretchr/testify/require"
)

func TestSupportDecisionCodecIsDeterministic(t *testing.T) {
	leftAccounts := []Account{
		{ID: 2, Platform: PlatformAnthropic, Credentials: map[string]any{"model_mapping": map[string]any{"z-*": "z", "a": "a"}}},
		{ID: 1, Platform: PlatformAnthropic, Credentials: map[string]any{"model_mapping": map[string]any{"b": "b"}}},
	}
	rightAccounts := []Account{leftAccounts[1], leftAccounts[0]}
	left := buildSupportDecisionTestTable(t, supportDecisionTestSnapshot(leftAccounts, PlatformAnthropic, []string{"hot", "other"}))
	right := buildSupportDecisionTestTable(t, supportDecisionTestSnapshot(rightAccounts, PlatformAnthropic, []string{"hot", "other"}))
	leftPayload, err := EncodeSupportDecisionDocument(left)
	require.NoError(t, err)
	rightPayload, err := EncodeSupportDecisionDocument(right)
	require.NoError(t, err)
	require.Equal(t, leftPayload, rightPayload)
}

func TestSupportDecisionCodecRejectsWrongVersionOrGeneration(t *testing.T) {
	table := buildSupportDecisionTestTable(t, supportDecisionTestSnapshot([]Account{{Platform: PlatformAnthropic}}, PlatformAnthropic, []string{"hot"}))
	payload, err := EncodeSupportDecisionDocument(table)
	require.NoError(t, err)
	_, err = DecodeSupportDecisionDocument(payload, table.Generation+1)
	require.ErrorContains(t, err, "generation mismatch")

	wrongVersion := bytes.Replace(payload, []byte(`"schema_version":1`), []byte(`"schema_version":2`), 1)
	_, err = DecodeSupportDecisionDocument(wrongVersion, table.Generation)
	require.ErrorContains(t, err, "schema version")
}

func TestSupportDecisionDocumentDoesNotContainAccountsCredentialsOrIDs(t *testing.T) {
	const secret = "never-serialize-this-secret"
	account := Account{ID: 987654321, Platform: PlatformOpenAI, Type: AccountTypeAPIKey, Credentials: map[string]any{"api_key": secret, "model_mapping": map[string]any{"external": "upstream"}}}
	table := buildSupportDecisionTestTable(t, supportDecisionTestSnapshot([]Account{account}, PlatformOpenAI, []string{"hot"}))
	payload, err := EncodeSupportDecisionDocument(table)
	require.NoError(t, err)
	require.NotContains(t, string(payload), secret)
	require.NotContains(t, string(payload), "api_key")
	require.NotContains(t, string(payload), "credentials")
	idBytes := make([]byte, 8)
	binary.LittleEndian.PutUint64(idBytes, uint64(account.ID))
	require.NotContains(t, payload, idBytes)
}

func TestSupportDecisionCodecRejectsInvalidOrdinalsDimensionsAndDuplicates(t *testing.T) {
	table := buildSupportDecisionTestTable(t, supportDecisionTestSnapshot([]Account{{Platform: PlatformAnthropic, Credentials: map[string]any{"model_mapping": map[string]any{"known": "target"}}}}, PlatformAnthropic, []string{"hot"}))
	payload, err := EncodeSupportDecisionDocument(table)
	require.NoError(t, err)

	var base supportDecisionDocumentEnvelope
	require.NoError(t, json.Unmarshal(payload, &base))
	mutations := map[string]func(*supportDecisionDocumentEnvelope){
		"unsorted strings": func(document *supportDecisionDocumentEnvelope) {
			document.Strings[0], document.Strings[len(document.Strings)-1] = document.Strings[len(document.Strings)-1], document.Strings[0]
		},
		"duplicate strings": func(document *supportDecisionDocumentEnvelope) {
			document.Strings[1] = document.Strings[0]
		},
		"invalid ordinal": func(document *supportDecisionDocumentEnvelope) {
			document.Scopes[0].Hot[0].StringID = uint32(len(document.Strings))
		},
		"invalid profile dimensions": func(document *supportDecisionDocumentEnvelope) {
			document.Scopes[0].Hot[0].Profile.SupportBits = nil
		},
		"overlapping profile bits": func(document *supportDecisionDocumentEnvelope) {
			document.Scopes[0].Hot[0].Profile.SupportBits[0] |= 1
			document.Scopes[0].Hot[0].Profile.EligibleBits[0] |= 1
		},
		"duplicate exact key": func(document *supportDecisionDocumentEnvelope) {
			for i := range document.Scopes {
				if len(document.Scopes[i].ExactWire) > 0 {
					document.Scopes[i].ExactWire = append(document.Scopes[i].ExactWire, document.Scopes[i].ExactWire[0])
					return
				}
			}
			t.Fatal("fixture has no exact entry")
		},
	}
	for name, mutate := range mutations {
		t.Run(name, func(t *testing.T) {
			document := cloneSupportDecisionEnvelope(t, base)
			mutate(&document)
			invalid, marshalErr := json.Marshal(document)
			require.NoError(t, marshalErr)
			_, decodeErr := DecodeSupportDecisionDocument(invalid, table.Generation)
			require.Error(t, decodeErr)
		})
	}
}

func cloneSupportDecisionEnvelope(t *testing.T, input supportDecisionDocumentEnvelope) supportDecisionDocumentEnvelope {
	t.Helper()
	payload, err := json.Marshal(input)
	require.NoError(t, err)
	var cloned supportDecisionDocumentEnvelope
	require.NoError(t, json.Unmarshal(payload, &cloned))
	return cloned
}

func TestSupportDecisionCodecRejectsMalformedChannelFacts(t *testing.T) {
	channel := SupportDecisionChannel{
		Status:             StatusActive,
		GroupIDs:           []int64{42},
		RestrictModels:     true,
		BillingModelSource: BillingModelSourceUpstream,
		PricingModels: []SupportDecisionPricingModels{{
			Platform: PlatformAnthropic,
			Models:   []string{"allowed-exact", "allowed-*"},
		}},
	}
	snapshot := supportDecisionTestSnapshot([]Account{{
		Platform: PlatformAnthropic,
		Credentials: map[string]any{
			"model_mapping": map[string]any{"configured": "configured"},
		},
	}}, PlatformAnthropic, []string{"hot"})
	snapshot.Channels = []SupportDecisionChannel{channel}
	table := buildSupportDecisionTestTable(t, snapshot)
	payload, err := EncodeSupportDecisionDocument(table)
	require.NoError(t, err)
	var base supportDecisionDocumentEnvelope
	require.NoError(t, json.Unmarshal(payload, &base))

	mutations := map[string]func(*supportDecisionScopeTable){
		"invalid exact ordinal": func(scope *supportDecisionScopeTable) {
			scope.ChannelExact[0] = uint32(len(base.Strings))
		},
		"duplicate exact entry": func(scope *supportDecisionScopeTable) {
			scope.ChannelExact = append(scope.ChannelExact, scope.ChannelExact[0])
		},
		"unsorted channel entries": func(scope *supportDecisionScopeTable) {
			scope.ChannelExact = append([]uint32{scope.ChannelWildcard[0]}, scope.ChannelExact...)
		},
		"missing channel profile": func(scope *supportDecisionScopeTable) {
			scope.ChannelAllowed = supportDecisionProfile{}
		},
		"invalid channel profile": func(scope *supportDecisionScopeTable) {
			scope.ChannelAllowed.SupportBits = scope.ChannelAllowed.SupportBits[:0]
		},
	}
	for name, mutate := range mutations {
		t.Run(name, func(t *testing.T) {
			document := cloneSupportDecisionEnvelope(t, base)
			scope := supportDecisionGroupScope(t, &document)
			mutate(scope)
			invalid, marshalErr := json.Marshal(document)
			require.NoError(t, marshalErr)
			require.NotPanics(t, func() {
				_, err = DecodeSupportDecisionDocument(invalid, table.Generation)
			})
			require.Error(t, err)
		})
	}
}

func supportDecisionGroupScope(t *testing.T, document *supportDecisionDocumentEnvelope) *supportDecisionScopeTable {
	t.Helper()
	for i := range document.Scopes {
		if document.Scopes[i].Key.GroupID == 42 {
			return &document.Scopes[i]
		}
	}
	t.Fatal("fixture has no group scope")
	return nil
}

func TestSupportDecisionCodecRoundTrip(t *testing.T) {
	table := buildSupportDecisionTestTable(t, supportDecisionTestSnapshot([]Account{{Platform: PlatformAnthropic, Credentials: map[string]any{"model_mapping": map[string]any{"known": "known"}}}}, PlatformAnthropic, []string{"hot"}))
	payload, err := EncodeSupportDecisionDocument(table)
	require.NoError(t, err)
	decoded, err := DecodeSupportDecisionDocument(payload, table.Generation)
	require.NoError(t, err)
	query := SupportDecisionQuery{Scope: SupportDecisionScope{Platform: PlatformAnthropic, GroupID: 42}, RequestedModel: "unseen"}
	require.Equal(t, table.Lookup(query), decoded.Lookup(query))
}
