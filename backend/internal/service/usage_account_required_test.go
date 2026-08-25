package service

import (
	"context"
	"testing"

	"github.com/stretchr/testify/require"
)

func TestUsageLogAccountRequiredForCreate(t *testing.T) {
	tests := []struct {
		name      string
		accountID *int64
		wantErr   bool
	}{
		{name: "nil", accountID: nil, wantErr: true},
		{name: "zero", accountID: usageAccountIDPointer(0), wantErr: true},
		{name: "negative", accountID: usageAccountIDPointer(-1), wantErr: true},
		{name: "positive", accountID: usageAccountIDPointer(42), wantErr: false},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			err := (&UsageLog{AccountID: tt.accountID}).ValidateForCreate()
			if tt.wantErr {
				require.ErrorIs(t, err, ErrUsageLogAccountRequired)
				return
			}
			require.NoError(t, err)
		})
	}
}

func TestUsageServiceCreateUsageAccountRequiredBeforeTransaction(t *testing.T) {
	for _, accountID := range []int64{0, -1} {
		t.Run(usageAccountIDTestName(accountID), func(t *testing.T) {
			svc := &UsageService{}

			log, err := svc.Create(context.Background(), CreateUsageLogRequest{AccountID: accountID})

			require.Nil(t, log)
			require.ErrorIs(t, err, ErrUsageLogAccountRequired)
		})
	}
}

func usageAccountIDPointer(value int64) *int64 {
	return &value
}

func usageAccountIDTestName(value int64) string {
	if value == 0 {
		return "zero"
	}
	return "negative"
}
