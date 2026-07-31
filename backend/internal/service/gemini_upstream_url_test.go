package service

import (
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/Wei-Shaw/sub2api/internal/config"
	"github.com/gin-gonic/gin"
	"github.com/stretchr/testify/require"
)

func TestBuildGeminiAIStudioModelActionURL(t *testing.T) {
	const base = "https://generativelanguage.googleapis.com"
	got, err := buildGeminiAIStudioModelActionURL(base+"/", "gemini-2.5-flash", "streamGenerateContent", true)
	require.NoError(t, err)
	require.Equal(t, base+"/v1beta/models/gemini-2.5-flash:streamGenerateContent?alt=sse", got)

	got, err = buildGeminiAIStudioModelActionURL(base, "gemini-2.5-pro", "countTokens", false)
	require.NoError(t, err)
	require.Equal(t, base+"/v1beta/models/gemini-2.5-pro:countTokens", got)
}

func TestAccountTestGeminiAPIKeyRequestRejectsUnsafeModel(t *testing.T) {
	svc := &AccountTestService{cfg: &config.Config{}}
	account := &Account{Credentials: map[string]any{"api_key": "test-key"}}

	req, err := svc.buildGeminiAPIKeyRequest(context.Background(), account, "gemini-2.5-pro", []byte(`{}`))
	require.NoError(t, err)
	require.Equal(t, "https://generativelanguage.googleapis.com/v1beta/models/gemini-2.5-pro:streamGenerateContent?alt=sse", req.URL.String())

	for _, model := range []string{"../x", "gemini/x", "gemini?a=b"} {
		req, err := svc.buildGeminiAPIKeyRequest(context.Background(), account, model, []byte(`{}`))
		require.Error(t, err)
		require.Nil(t, req)
	}
}

func TestGeminiMappedModelIsValidatedAtSink(t *testing.T) {
	stub := &geminiCompatHTTPUpstreamStub{}
	svc := &GeminiMessagesCompatService{httpUpstream: stub, cfg: &config.Config{}}
	account := &Account{ID: 1, Type: AccountTypeAPIKey, Credentials: map[string]any{
		"api_key":       "test-key",
		"model_mapping": map[string]any{"gemini-safe": "../unsafe"},
	}}
	rec := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(rec)
	body := []byte(`{"model":"gemini-safe","max_tokens":8,"messages":[{"role":"user","content":"hello"}]}`)
	c.Request = httptest.NewRequest(http.MethodPost, "/v1/messages", strings.NewReader(string(body)))

	result, err := svc.Forward(context.Background(), c, account, body)
	require.Error(t, err)
	require.Nil(t, result)
	require.Equal(t, 0, stub.calls)
}

func TestForwardAIStudioGETValidatesSuffixAtSink(t *testing.T) {
	stub := &geminiCompatHTTPUpstreamStub{response: &http.Response{
		StatusCode: http.StatusOK,
		Header:     make(http.Header),
		Body:       io.NopCloser(strings.NewReader(`{}`)),
	}}
	svc := &GeminiMessagesCompatService{httpUpstream: stub, cfg: &config.Config{}}
	account := &Account{ID: 1, Type: AccountTypeAPIKey, Credentials: map[string]any{"api_key": "test-key"}}

	_, err := svc.ForwardAIStudioGET(context.Background(), account, "/v1beta/models/gemini-2.5-pro")
	require.NoError(t, err)
	require.Equal(t, "https://generativelanguage.googleapis.com/v1beta/models/gemini-2.5-pro", stub.lastReq.URL.String())

	for _, suffix := range []string{"/v1beta/models/../x", "/v1beta/models/gemini?x=1", "/v1beta/models/gemini#x", "/v1beta/models//x"} {
		stub.lastReq = nil
		_, err := svc.ForwardAIStudioGET(context.Background(), account, suffix)
		require.Error(t, err, suffix)
		require.Nil(t, stub.lastReq, suffix)
	}
}

func TestForwardAIStudioGETRejectsBaseQueryOrFragmentBeforeTransport(t *testing.T) {
	for _, base := range []string{
		"https://proxy.example/gemini?tenant=a",
		"https://proxy.example/gemini#fragment",
	} {
		t.Run(base, func(t *testing.T) {
			stub := &geminiCompatHTTPUpstreamStub{}
			svc := &GeminiMessagesCompatService{httpUpstream: stub, cfg: &config.Config{}}
			account := &Account{ID: 1, Type: AccountTypeAPIKey, Credentials: map[string]any{
				"api_key":  "test-key",
				"base_url": base,
			}}

			result, err := svc.ForwardAIStudioGET(context.Background(), account, "/v1beta/models/gemini-2.5-pro")
			require.Error(t, err)
			require.Nil(t, result)
			require.Equal(t, 0, stub.calls)
			require.Nil(t, stub.lastReq)
		})
	}
}

func TestBuildGeminiAIStudioModelActionURLRejectsBaseQueryOrFragment(t *testing.T) {
	for _, base := range []string{
		"https://generativelanguage.googleapis.com?tenant=x",
		"https://generativelanguage.googleapis.com#fragment",
	} {
		_, err := buildGeminiAIStudioModelActionURL(base, "gemini-2.5-pro", "generateContent", false)
		require.Error(t, err, base)
	}
}

func TestBuildGeminiAIStudioModelActionURLRejectsUnsafeModel(t *testing.T) {
	const base = "https://generativelanguage.googleapis.com"
	for _, model := range []string{
		"../x", "gemini/x", "gemini?a=b", "gemini#x", "gemini\x00pro", "模型", "...", "", strings.Repeat("a", 129),
	} {
		t.Run(model, func(t *testing.T) {
			_, err := buildGeminiAIStudioModelActionURL(base, model, "generateContent", false)
			require.Error(t, err)
			require.False(t, IsSafeGeminiModelPathSegment(model))
		})
	}
	_, err := buildGeminiAIStudioModelActionURL(base, "gemini-2.5-pro", "deleteModel", false)
	require.Error(t, err)
}
