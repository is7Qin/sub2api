package service

import (
	"encoding/json"
	"fmt"
	"sort"
)

type supportDecisionDocumentEnvelope struct {
	SchemaVersion uint16                      `json:"schema_version"`
	Generation    uint64                      `json:"generation"`
	Strings       []string                    `json:"strings"`
	Scopes        []supportDecisionScopeTable `json:"scopes"`
}

func EncodeSupportDecisionDocument(table *SupportDecisionTable) ([]byte, error) {
	if table == nil || !table.verified {
		return nil, fmt.Errorf("support decision table is not verified")
	}
	if len(table.wirePayload) > 0 {
		return append([]byte(nil), table.wirePayload...), nil
	}
	payload, err := encodeSupportDecisionDocumentUnchecked(table)
	if err != nil {
		return nil, err
	}
	if len(payload) > SupportDecisionMaxDocumentSize {
		return nil, fmt.Errorf("support decision document exceeds %d bytes", SupportDecisionMaxDocumentSize)
	}
	return payload, nil
}

func encodeSupportDecisionDocumentUnchecked(table *SupportDecisionTable) ([]byte, error) {
	return json.Marshal(supportDecisionDocumentEnvelope{
		SchemaVersion: table.SchemaVersion,
		Generation:    table.Generation,
		Strings:       table.Strings,
		Scopes:        table.Scopes,
	})
}

func DecodeSupportDecisionDocument(payload []byte, expectedGeneration uint64) (*SupportDecisionTable, error) {
	if len(payload) == 0 {
		return nil, fmt.Errorf("support decision document is empty")
	}
	if len(payload) > SupportDecisionMaxDocumentSize {
		return nil, fmt.Errorf("support decision document exceeds %d bytes", SupportDecisionMaxDocumentSize)
	}
	var document supportDecisionDocumentEnvelope
	if err := json.Unmarshal(payload, &document); err != nil {
		return nil, fmt.Errorf("decode support decision document: %w", err)
	}
	if document.SchemaVersion != SupportDecisionSchemaVersion {
		return nil, fmt.Errorf("unsupported support decision schema version %d", document.SchemaVersion)
	}
	if expectedGeneration == 0 || document.Generation != expectedGeneration {
		return nil, fmt.Errorf("support decision generation mismatch: got %d want %d", document.Generation, expectedGeneration)
	}
	for i, value := range document.Strings {
		if value == "" || len(value) > SupportDecisionMaxModelBytes {
			return nil, fmt.Errorf("invalid support decision interned string")
		}
		if i > 0 && document.Strings[i-1] >= value {
			return nil, fmt.Errorf("support decision interned strings are not strictly sorted")
		}
	}
	if !sort.SliceIsSorted(document.Scopes, func(i, j int) bool {
		left, right := document.Scopes[i].Key, document.Scopes[j].Key
		if left.Platform != right.Platform {
			return left.Platform < right.Platform
		}
		if left.GroupID != right.GroupID {
			return left.GroupID < right.GroupID
		}
		if left.IncludeGrouped != right.IncludeGrouped {
			return !left.IncludeGrouped && right.IncludeGrouped
		}
		return !left.AllowMixedScheduling && right.AllowMixedScheduling
	}) {
		return nil, fmt.Errorf("support decision scopes are not sorted")
	}
	table := &SupportDecisionTable{
		SchemaVersion: document.SchemaVersion,
		Generation:    document.Generation,
		Strings:       document.Strings,
		Scopes:        document.Scopes,
	}
	if !table.prepareIndexes() {
		return nil, fmt.Errorf("invalid support decision document")
	}
	for i := range table.Scopes {
		if err := validateSupportDecisionScopeBudgets(&table.Scopes[i], table.Strings); err != nil {
			return nil, err
		}
	}
	table.wirePayload = append([]byte(nil), payload...)
	return table, nil
}
