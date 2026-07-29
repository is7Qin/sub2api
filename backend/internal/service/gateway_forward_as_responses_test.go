//go:build unit

package service

import (
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/gin-gonic/gin"
	"github.com/stretchr/testify/require"
)

func TestExtractResponsesReasoningEffortFromBody(t *testing.T) {
	t.Parallel()

	got := ExtractResponsesReasoningEffortFromBody([]byte(`{"model":"claude-sonnet-4.5","reasoning":{"effort":"HIGH"}}`))
	require.NotNil(t, got)
	require.Equal(t, "high", *got)

	got = ExtractResponsesReasoningEffortFromBody([]byte(`{"model":"deepseek-reasoner","reasoning":{"effort":"max"}}`))
	require.NotNil(t, got)
	require.Equal(t, "xhigh", *got)

	require.Nil(t, ExtractResponsesReasoningEffortFromBody([]byte(`{"model":"claude-sonnet-4.5"}`)))
}

func TestHandleResponsesBufferedStreamingResponse_PreservesMessageStartCacheUsage(t *testing.T) {
	t.Parallel()

	rec := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(rec)

	resp := &http.Response{
		Header: http.Header{"x-request-id": []string{"rid_buffered"}},
		Body: io.NopCloser(strings.NewReader(strings.Join([]string{
			`event: message_start`,
			`data: {"type":"message_start","message":{"id":"msg_1","type":"message","role":"assistant","content":[],"model":"claude-sonnet-4.5","stop_reason":"","usage":{"input_tokens":12,"cache_read_input_tokens":9,"cache_creation_input_tokens":3}}}`,
			``,
			`event: content_block_start`,
			`data: {"type":"content_block_start","index":0,"content_block":{"type":"text","text":"hello"}}`,
			``,
			`event: message_delta`,
			`data: {"type":"message_delta","delta":{"stop_reason":"end_turn"},"usage":{"output_tokens":7}}`,
			``,
		}, "\n"))),
	}

	svc := &GatewayService{}
	result, err := svc.handleResponsesBufferedStreamingResponse(resp, c, "claude-sonnet-4.5", "claude-sonnet-4.5", nil, time.Now())
	require.NoError(t, err)
	require.NotNil(t, result)
	require.Equal(t, 12, result.Usage.InputTokens)
	require.Equal(t, 7, result.Usage.OutputTokens)
	require.Equal(t, 9, result.Usage.CacheReadInputTokens)
	require.Equal(t, 3, result.Usage.CacheCreationInputTokens)
	require.Contains(t, rec.Body.String(), `"cached_tokens":9`)
}

func TestHandleResponsesBufferedStreamingResponse_AcceptsCompactSSEFields(t *testing.T) {
	t.Parallel()

	rec := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(rec)
	resp := &http.Response{
		Header: http.Header{"x-request-id": []string{"rid_responses_compact_buffered"}},
		Body: io.NopCloser(&oneByteReader{r: strings.NewReader(strings.Join([]string{
			`event:message_start`,
			`data:{"type":"message_start","message":{"id":"msg_compact","type":"message","role":"assistant","content":[],"model":"claude-sonnet-4.5","stop_reason":"","usage":{"input_tokens":6}}}`,
			`event:content_block_start`,
			`data:{"type":"content_block_start","index":0,"content_block":{"type":"text","text":"compact responses"}}`,
			`event:message_delta`,
			`data:{"type":"message_delta","delta":{"stop_reason":"end_turn"},"usage":{"output_tokens":4}}`,
		}, "\r\n"))}),
	}

	result, err := (&GatewayService{}).handleResponsesBufferedStreamingResponse(resp, c, "gpt-5", "claude-sonnet-4.5", nil, time.Now())
	require.NoError(t, err)
	require.Equal(t, 6, result.Usage.InputTokens)
	require.Equal(t, 4, result.Usage.OutputTokens)
	require.Contains(t, rec.Body.String(), "compact responses")
}

func TestHandleResponsesStreamingResponse_AcceptsCompactSSEFields(t *testing.T) {
	t.Parallel()

	rec := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(rec)
	resp := &http.Response{
		Header: http.Header{"x-request-id": []string{"rid_responses_compact_stream"}},
		Body: io.NopCloser(&oneByteReader{r: strings.NewReader(strings.Join([]string{
			`event:message_start`,
			`data:{"type":"message_start","message":{"id":"msg_compact_stream","type":"message","role":"assistant","content":[],"model":"claude-sonnet-4.5","stop_reason":"","usage":{"input_tokens":7}}}`,
			`event:content_block_start`,
			`data:{"type":"content_block_start","index":0,"content_block":{"type":"text","text":""}}`,
			`event:content_block_delta`,
			`data:{"type":"content_block_delta","index":0,"delta":{"type":"text_delta","text":"compact stream"}}`,
			`event:message_delta`,
			`data:{"type":"message_delta","delta":{"stop_reason":"end_turn"},"usage":{"output_tokens":5}}`,
			`event:message_stop`,
			`data:{"type":"message_stop"}`,
		}, "\n"))}),
	}

	result, err := (&GatewayService{}).handleResponsesStreamingResponse(resp, c, "gpt-5", "claude-sonnet-4.5", nil, time.Now())
	require.NoError(t, err)
	require.Equal(t, 7, result.Usage.InputTokens)
	require.Equal(t, 5, result.Usage.OutputTokens)
	require.Contains(t, rec.Body.String(), `response.created`)
	require.Contains(t, rec.Body.String(), "compact stream")
	require.Contains(t, rec.Body.String(), `response.completed`)
}

func TestHandleResponsesStreamingResponse_PreservesMessageStartCacheUsage(t *testing.T) {
	t.Parallel()

	rec := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(rec)

	resp := &http.Response{
		Header: http.Header{"x-request-id": []string{"rid_stream"}},
		Body: io.NopCloser(strings.NewReader(strings.Join([]string{
			`event: message_start`,
			`data: {"type":"message_start","message":{"id":"msg_2","type":"message","role":"assistant","content":[],"model":"claude-sonnet-4.5","stop_reason":"","usage":{"input_tokens":20,"cache_read_input_tokens":11,"cache_creation_input_tokens":4}}}`,
			``,
			`event: content_block_start`,
			`data: {"type":"content_block_start","index":0,"content_block":{"type":"text","text":"hello"}}`,
			``,
			`event: message_delta`,
			`data: {"type":"message_delta","delta":{"stop_reason":"end_turn"},"usage":{"output_tokens":8}}`,
			``,
			`event: message_stop`,
			`data: {"type":"message_stop"}`,
			``,
		}, "\n"))),
	}

	svc := &GatewayService{}
	result, err := svc.handleResponsesStreamingResponse(resp, c, "claude-sonnet-4.5", "claude-sonnet-4.5", nil, time.Now())
	require.NoError(t, err)
	require.NotNil(t, result)
	require.Equal(t, 20, result.Usage.InputTokens)
	require.Equal(t, 8, result.Usage.OutputTokens)
	require.Equal(t, 11, result.Usage.CacheReadInputTokens)
	require.Equal(t, 4, result.Usage.CacheCreationInputTokens)
	require.Contains(t, rec.Body.String(), `response.completed`)
}
