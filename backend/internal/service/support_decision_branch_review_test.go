//go:build unit

package service

import (
	"context"
	"testing"

	"github.com/stretchr/testify/require"
)

func TestSupportDecisionShadowDetectsDefaultCorruption(t *testing.T) {
	snapshot := supportDecisionTestSnapshot([]Account{{Platform: PlatformAnthropic, Extra: map[string]any{"privacy_mode": PrivacyModeTrainingOff}, Credentials: map[string]any{"model_mapping": map[string]any{"exact": "target"}}}}, PlatformAnthropic, nil)
	options := SupportDecisionBuildOptions{Generation: 21}
	table := buildSupportDecisionTestTable(t, snapshot, options)
	broken := cloneSupportDecisionTableForReview(t, table)
	broken.shadowScopes = table.shadowScopes
	profile := &supportDecisionScopeForReview(t, broken, PlatformAnthropic, 42).Default
	for i := range profile.SupportBits {
		profile.SupportBits[i] = 0xff
	}
	for i := range profile.EligibleBits {
		profile.EligibleBits[i] = 0
	}
	_, err := VerifySupportDecisionShadow(context.Background(), snapshot, options, broken)
	require.ErrorIs(t, err, ErrSupportDecisionShadowMismatch)
}

func TestSupportDecisionShadowDetectsChannelBranches(t *testing.T) {
	channel := SupportDecisionChannel{Status: StatusActive, RestrictModels: true, BillingModelSource: BillingModelSourceUpstream, GroupIDs: []int64{42}, PricingModels: []SupportDecisionPricingModels{{Platform: PlatformAnthropic, Models: []string{"DIRECT", "claude-sonnet-4.5*"}}}}
	snapshot := supportDecisionTestSnapshot([]Account{{Platform: PlatformAnthropic, Extra: map[string]any{"privacy_mode": PrivacyModeTrainingOff}, Credentials: map[string]any{"model_mapping": map[string]any{"other": "other"}}}}, PlatformAnthropic, nil)
	snapshot.Channels = []SupportDecisionChannel{channel}
	options := SupportDecisionBuildOptions{Generation: 22}
	table := buildSupportDecisionTestTable(t, snapshot, options)
	broken := cloneSupportDecisionTableForReview(t, table)
	broken.shadowScopes = table.shadowScopes
	profile := &supportDecisionScopeForReview(t, broken, PlatformAnthropic, 42).ChannelAllowed
	profile.SupportBits[0] ^= 1
	_, err := VerifySupportDecisionShadow(context.Background(), snapshot, options, broken)
	require.ErrorIs(t, err, ErrSupportDecisionShadowMismatch)
}
