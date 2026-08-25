package service

import "testing"

func TestAPIKeyService_RejectsLegacyAuthSnapshotVersions(t *testing.T) {
	groupID := int64(9)
	svc := &APIKeyService{}

	tests := []struct {
		name    string
		version int
	}{
		{name: "v10 without models_list_config", version: 10},
		{name: "v13 without exclusive group auth fields", version: 13},
		{name: "v15 without OpenAI long-context billing policy", version: 15},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			apiKey, ok, err := svc.applyAuthCacheEntry("k-legacy", &APIKeyAuthCacheEntry{
				Snapshot: &APIKeyAuthSnapshot{
					Version:  tt.version,
					APIKeyID: 1,
					UserID:   2,
					GroupID:  &groupID,
					Status:   StatusActive,
					User: APIKeyAuthUserSnapshot{
						ID:          2,
						Status:      StatusActive,
						Role:        RoleUser,
						Balance:     10,
						Concurrency: 3,
					},
					Group: &APIKeyAuthGroupSnapshot{
						ID:               groupID,
						Name:             "openai",
						Platform:         PlatformOpenAI,
						Status:           StatusActive,
						SubscriptionType: SubscriptionTypeStandard,
						RateMultiplier:   1,
					},
				},
			})

			if err != nil {
				t.Fatalf("expected stale snapshot to be ignored without error, got %v", err)
			}
			if ok {
				t.Fatalf("expected v%d auth snapshot to be rejected after auth schema changed", tt.version)
			}
			if apiKey != nil {
				t.Fatalf("expected no API key from stale snapshot, got %#v", apiKey)
			}
		})
	}
}
