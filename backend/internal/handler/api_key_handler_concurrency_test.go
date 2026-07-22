package handler

import (
	"encoding/json"
	"testing"

	"github.com/stretchr/testify/require"
)

func TestCreateAPIKeyRequestConcurrencyPropagation(t *testing.T) {
	tests := []struct {
		name string
		body string
		want int
	}{
		{name: "omitted defaults to zero", body: `{"name":"key"}`, want: 0},
		{name: "positive value propagates", body: `{"name":"key","concurrency":7}`, want: 7},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			var req CreateAPIKeyRequest
			require.NoError(t, json.Unmarshal([]byte(tt.body), &req))

			svcReq := req.toServiceRequest()

			require.Equal(t, tt.want, svcReq.Concurrency)
		})
	}
}
