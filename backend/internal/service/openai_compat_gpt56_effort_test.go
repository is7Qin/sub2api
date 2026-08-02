package service

import (
	"context"
	"testing"

	"github.com/stretchr/testify/require"
	"github.com/tidwall/gjson"
)

func TestForwardAsAnthropic_PreservesMaxEffortForMappedGPT56(t *testing.T) {
	body := []byte(`{"model":"luna","max_tokens":16,"messages":[{"role":"user","content":"hello"}],"output_config":{"effort":"max"},"stream":false}`)
	c, _ := newOpenAICompatMessagesTestContext(body)
	account := newOpenAICompatMessagesTestAccount()
	account.Credentials["model_mapping"] = map[string]any{"luna": "gpt-5.6-luna"}
	svc := newOpenAICompatMessagesSSEService(openAICompatSSECompletedResponse("resp_gpt56", "gpt-5.6-luna").Body, "rid_gpt56", nil)

	result, err := svc.ForwardAsAnthropic(context.Background(), c, account, body, "", "")

	require.NoError(t, err)
	require.NotNil(t, result)
	require.Equal(t, "gpt-5.6-luna", gjson.GetBytes(svc.httpUpstream.(*httpUpstreamRecorder).lastBody, "model").String())
	require.Equal(t, "max", gjson.GetBytes(svc.httpUpstream.(*httpUpstreamRecorder).lastBody, "reasoning.effort").String())
	require.NotNil(t, result.ReasoningEffort)
	require.Equal(t, "max", *result.ReasoningEffort)
}
