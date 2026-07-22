//go:build unit

package server_test

import (
	"net/http"
	"testing"

	"github.com/stretchr/testify/require"
)

func TestAPIKeyCreateHandlerConcurrencyPropagation(t *testing.T) {
	tests := []struct {
		name       string
		body       string
		wantStatus int
		wantCount  int
		wantValue  int
	}{
		{
			name:       "omitted defaults to zero and invokes service",
			body:       `{"name":"omitted","custom_key":"sk_omitted_1234567890"}`,
			wantStatus: http.StatusOK,
			wantCount:  1,
			wantValue:  0,
		},
		{
			name:       "positive propagates and invokes service",
			body:       `{"name":"positive","custom_key":"sk_positive_1234567890","concurrency":7}`,
			wantStatus: http.StatusOK,
			wantCount:  1,
			wantValue:  7,
		},
		{
			name:       "negative is rejected before service",
			body:       `{"name":"negative","custom_key":"sk_negative_1234567890","concurrency":-1}`,
			wantStatus: http.StatusBadRequest,
			wantCount:  0,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			deps := newContractDeps(t)

			status, _ := doRequest(t, deps.router, http.MethodPost, "/api/v1/keys", tt.body, map[string]string{"Content-Type": "application/json"})

			require.Equal(t, tt.wantStatus, status)
			require.Len(t, deps.apiKeyRepo.byID, tt.wantCount)
			if tt.wantCount == 1 {
				require.Equal(t, tt.wantValue, deps.apiKeyRepo.byID[100].Concurrency)
			}
		})
	}
}
