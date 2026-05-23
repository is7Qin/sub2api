package service

import (
	"testing"
	"time"

	"github.com/stretchr/testify/require"
)

func TestRedeemTimedQuotaPayloadValidation(t *testing.T) {
	require.NoError(t, validateRedeemCodePayload(&RedeemCode{
		Type:         RedeemTypeTimedQuota,
		Value:        3.5,
		ValidityDays: 7,
	}))

	require.Error(t, validateRedeemCodePayload(&RedeemCode{
		Type:         RedeemTypeTimedQuota,
		Value:        0,
		ValidityDays: 7,
	}))
	require.Error(t, validateRedeemCodePayload(&RedeemCode{
		Type:  RedeemTypeTimedQuota,
		Value: 3.5,
	}))
}

func TestRedeemRandomTimedQuotaAmount(t *testing.T) {
	metadata := map[string]any{
		redeemMetadataKeyMinValue:     1.25,
		redeemMetadataKeyMaxValue:     2.5,
		redeemMetadataKeyValidityDays: 3,
	}
	require.NoError(t, validateRedeemCodePayload(&RedeemCode{
		Type:     RedeemTypeRandomTimedQuota,
		Metadata: metadata,
	}))
	amount, err := redeemRandomTimedQuotaAmount(metadata)
	require.NoError(t, err)
	require.GreaterOrEqual(t, amount, 1.25)
	require.LessOrEqual(t, amount, 2.5)
}

func TestRedeemCodeExpiry(t *testing.T) {
	now := time.Now().UTC()
	past := now.Add(-time.Hour)
	future := now.Add(time.Hour)

	tests := []struct {
		name        string
		code        RedeemCode
		wantExpired bool
		wantCanUse  bool
	}{
		{
			name:        "unused without expiry can be used",
			code:        RedeemCode{Status: StatusUnused},
			wantExpired: false,
			wantCanUse:  true,
		},
		{
			name:        "unused before expiry can be used",
			code:        RedeemCode{Status: StatusUnused, ExpiresAt: &future},
			wantExpired: false,
			wantCanUse:  true,
		},
		{
			name:        "unused after expiry cannot be used",
			code:        RedeemCode{Status: StatusUnused, ExpiresAt: &past},
			wantExpired: true,
			wantCanUse:  false,
		},
		{
			name:        "explicit expired status is expired",
			code:        RedeemCode{Status: StatusExpired},
			wantExpired: true,
			wantCanUse:  false,
		},
		{
			name:        "used code remains used even after expiry time",
			code:        RedeemCode{Status: StatusUsed, ExpiresAt: &past},
			wantExpired: false,
			wantCanUse:  false,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			require.Equal(t, tt.wantExpired, tt.code.IsExpiredAt(now))
			require.Equal(t, tt.wantCanUse, tt.code.CanUse())
		})
	}
}
