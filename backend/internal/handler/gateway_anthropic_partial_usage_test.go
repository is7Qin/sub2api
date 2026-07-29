package handler

import (
	"errors"
	"testing"

	"github.com/Wei-Shaw/sub2api/internal/service"
	"github.com/stretchr/testify/require"
)

func TestSubmitMessagesUsageOnce_PartialNativeResultWithUsage(t *testing.T) {
	calls := 0
	result := &service.ForwardResult{AttemptID: "attempt-1", Usage: service.ClaudeUsage{InputTokens: 5}}

	require.True(t, submitMessagesUsageOnce(result, errors.New("stream ended early"), service.PlatformAnthropic, func() { calls++ }))
	require.Equal(t, 1, calls)
}

func TestSubmitMessagesUsageOnce_PartialAPIKeyPassthroughResultWithUsage(t *testing.T) {
	calls := 0
	result := &service.ForwardResult{AttemptID: "attempt-1", Usage: service.ClaudeUsage{OutputTokens: 3}}

	require.True(t, submitMessagesUsageOnce(result, errors.New("unexpected EOF"), service.PlatformAnthropic, func() { calls++ }))
	require.Equal(t, 1, calls)
}

func TestSubmitMessagesUsageOnce_IncompleteResultWithoutUsage(t *testing.T) {
	calls := 0

	require.False(t, submitMessagesUsageOnce(&service.ForwardResult{}, errors.New("unexpected EOF"), service.PlatformAnthropic, func() { calls++ }))
	require.Equal(t, 0, calls)
}

func TestSubmitMessagesUsageOnce_PreAdmissionFailureWithoutResult(t *testing.T) {
	calls := 0

	require.False(t, submitMessagesUsageOnce(nil, errors.New("attempt not admitted"), service.PlatformAnthropic, func() { calls++ }))
	require.Equal(t, 0, calls)
}

func TestSubmitMessagesUsageOnce_ErrorResultWithoutAdmittedAttempt(t *testing.T) {
	calls := 0
	result := &service.ForwardResult{Usage: service.ClaudeUsage{InputTokens: 5}}

	require.False(t, submitMessagesUsageOnce(result, errors.New("attempt not admitted"), service.PlatformAnthropic, func() { calls++ }))
	require.Equal(t, 0, calls)
}

func TestSubmitMessagesUsageOnce_SuccessRemainsExactlyOnce(t *testing.T) {
	calls := 0
	result := &service.ForwardResult{AttemptID: "attempt-1", Usage: service.ClaudeUsage{InputTokens: 5, OutputTokens: 3}}

	require.True(t, submitMessagesUsageOnce(result, nil, service.PlatformAnthropic, func() { calls++ }))
	require.Equal(t, 1, calls)
}

func TestSubmitMessagesUsageOnce_AntigravitySuccessExactlyOnce(t *testing.T) {
	calls := 0
	result := &service.ForwardResult{Usage: service.ClaudeUsage{InputTokens: 5, OutputTokens: 3}}

	require.True(t, submitMessagesUsageOnce(result, nil, service.PlatformAntigravity, func() { calls++ }))
	require.Equal(t, 1, calls)
}

func TestSubmitMessagesUsageOnce_AntigravityErrorDoesNotPartialBill(t *testing.T) {
	calls := 0
	result := &service.ForwardResult{AttemptID: "antigravity-attempt", Usage: service.ClaudeUsage{InputTokens: 5, OutputTokens: 3}}

	require.False(t, submitMessagesUsageOnce(result, errors.New("upstream failed"), service.PlatformAntigravity, func() { calls++ }))
	require.Equal(t, 0, calls)
}

func TestRunPostForwardUsageSubmission_SubmitsBeforeContinuation(t *testing.T) {
	tests := []struct {
		name       string
		result     *service.ForwardResult
		forwardErr error
		platform   string
		wantSubmit bool
	}{
		{
			name:       "native result plus error",
			result:     &service.ForwardResult{AttemptID: "native-attempt", Usage: service.ClaudeUsage{InputTokens: 5}},
			forwardErr: errors.New("native stream ended early"),
			platform:   service.PlatformAnthropic,
			wantSubmit: true,
		},
		{
			name:       "api key result plus error",
			result:     &service.ForwardResult{AttemptID: "api-key-attempt", Usage: service.ClaudeUsage{OutputTokens: 3}},
			forwardErr: errors.New("api key stream ended early"),
			platform:   service.PlatformAnthropic,
			wantSubmit: true,
		},
		{
			name:       "pre-admission failure",
			forwardErr: errors.New("attempt not admitted"),
			platform:   service.PlatformAnthropic,
		},
		{
			name:       "admitted result without usage",
			result:     &service.ForwardResult{AttemptID: "attempt-no-usage"},
			forwardErr: errors.New("stream ended early"),
			platform:   service.PlatformAnthropic,
		},
		{
			name:       "antigravity error",
			result:     &service.ForwardResult{AttemptID: "antigravity-attempt", Usage: service.ClaudeUsage{InputTokens: 5}},
			forwardErr: errors.New("antigravity failed"),
			platform:   service.PlatformAntigravity,
		},
		{
			name:       "success",
			result:     &service.ForwardResult{AttemptID: "success-attempt", Usage: service.ClaudeUsage{InputTokens: 5}},
			platform:   service.PlatformAnthropic,
			wantSubmit: true,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			var trace []string
			calls := 0
			submitted := runPostForwardUsageSubmission(
				func() bool {
					return submitMessagesUsageOnce(tt.result, tt.forwardErr, tt.platform, func() {
						calls++
						trace = append(trace, "submit")
					})
				},
				func() { trace = append(trace, "continue") },
			)

			require.Equal(t, tt.wantSubmit, submitted)
			if tt.wantSubmit {
				require.Equal(t, 1, calls)
				require.Equal(t, []string{"submit", "continue"}, trace)
			} else {
				require.Zero(t, calls)
				require.Equal(t, []string{"continue"}, trace)
			}
		})
	}
}
