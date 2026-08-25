//go:build unit

package service

import (
	"math"
	"testing"

	"github.com/shopspring/decimal"
	"github.com/stretchr/testify/require"
)

func usageBillingDecimalPlaces(v float64) int32 {
	return -decimal.NewFromFloat(v).Exponent()
}

func TestUsageBillingCommandNormalizeQuantizesEveryMonetaryField(t *testing.T) {
	const raw = 0.0000781234567
	cmd := &UsageBillingCommand{
		RequestID:           "req-quantize-fields",
		BalanceCost:         raw,
		SubscriptionCost:    raw,
		APIKeyQuotaCost:     raw,
		APIKeyRateLimitCost: raw,
		AccountQuotaCost:    raw,
	}

	cmd.Normalize()

	for name, got := range map[string]float64{
		"BalanceCost":         cmd.BalanceCost,
		"SubscriptionCost":    cmd.SubscriptionCost,
		"APIKeyQuotaCost":     cmd.APIKeyQuotaCost,
		"APIKeyRateLimitCost": cmd.APIKeyRateLimitCost,
		"AccountQuotaCost":    cmd.AccountQuotaCost,
	} {
		require.LessOrEqual(t, usageBillingDecimalPlaces(got), int32(UsageBillingMonetaryScale), name)
	}
}

func TestQuantizeUsageBillingAmountUsesNumericScaleRounding(t *testing.T) {
	cases := []struct {
		name string
		in   float64
		want float64
	}{
		{name: "below half", in: 0.000078124, want: 0.00007812},
		{name: "positive half", in: 0.000078125, want: 0.00007813},
		{name: "negative half", in: -0.000078125, want: -0.00007813},
		{name: "above half", in: 0.000078126, want: 0.00007813},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			require.Equal(t, tc.want, QuantizeUsageBillingAmount(tc.in))
		})
	}
}

func TestUsageBillingCommandNormalizeFingerprintsRawAmountsBeforeQuantizing(t *testing.T) {
	cmd := &UsageBillingCommand{
		RequestID:       "req-quantize-fingerprint",
		UserID:          1,
		APIKeyID:        2,
		AccountID:       3,
		BalanceCost:     0.000078125,
		APIKeyQuotaCost: 0.000078125,
	}
	expectedFingerprint := buildUsageBillingFingerprint(cmd)

	cmd.Normalize()

	require.Equal(t, expectedFingerprint, cmd.RequestFingerprint)
	require.Equal(t, 0.00007813, cmd.BalanceCost)
	require.Equal(t, cmd.BalanceCost, cmd.APIKeyQuotaCost)
}

func TestQuantizeUsageBillingAmountPassesThroughNonFiniteValues(t *testing.T) {
	require.Equal(t, 0.0, QuantizeUsageBillingAmount(0))
	require.True(t, math.IsNaN(QuantizeUsageBillingAmount(math.NaN())))
	require.True(t, math.IsInf(QuantizeUsageBillingAmount(math.Inf(1)), 1))
	require.True(t, math.IsInf(QuantizeUsageBillingAmount(math.Inf(-1)), -1))
}
