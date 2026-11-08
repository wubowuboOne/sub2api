package service

import (
	"context"
	"net/http"
	"testing"

	"github.com/Wei-Shaw/sub2api/internal/config"
	"github.com/stretchr/testify/require"
)

const openAIInvalidURLValueBody = `{"error":{"code":"invalid_value","message":"Upstream rejected the request","param":"url","type":"invalid_request_error"}}`

func TestOpenAIConfiguredHTTPStatusRetryPolicy(t *testing.T) {
	for _, test := range []struct {
		name       string
		pool       bool
		retryCount int
		codes      []any
		status     int
		body       string
		want       bool
	}{
		{name: "configured_400", pool: true, retryCount: 10, codes: []any{float64(400)}, status: 400, body: openAIInvalidURLValueBody, want: true},
		{name: "configured_422", pool: true, retryCount: 2, codes: []any{float64(422)}, status: 422, body: openAIInvalidURLValueBody, want: true},
		{name: "400_not_configured", pool: true, retryCount: 10, codes: []any{float64(502)}, status: 400, body: openAIInvalidURLValueBody},
		{name: "default_codes", pool: true, retryCount: 10, status: 400, body: openAIInvalidURLValueBody},
		{name: "empty_codes", pool: true, retryCount: 10, codes: []any{}, status: 400, body: openAIInvalidURLValueBody},
		{name: "retry_disabled", pool: true, codes: []any{float64(400)}, status: 400, body: openAIInvalidURLValueBody},
		{name: "non_pool", retryCount: 10, codes: []any{float64(400)}, status: 400, body: openAIInvalidURLValueBody},
		{name: "session_blocked", pool: true, retryCount: 10, codes: []any{float64(400)}, status: 400, body: `{"error":{"code":"session_blocked_by_cyber_policy","message":"Session blocked"}}`},
		{name: "cyber_policy", pool: true, retryCount: 10, codes: []any{float64(400)}, status: 400, body: `{"error":{"code":"cyber_policy","message":"Request blocked"}}`},
		{name: "context_window", pool: true, retryCount: 10, codes: []any{float64(400)}, status: 400, body: `{"error":{"code":"context_length_exceeded","message":"Input exceeds the context window"}}`},
		{name: "context_window_413", pool: true, retryCount: 10, codes: []any{float64(413)}, status: 413, body: `{"error":{"code":"context_length_exceeded","message":"Input exceeds the context window"}}`},
	} {
		t.Run(test.name, func(t *testing.T) {
			account := &Account{
				Platform: PlatformOpenAI, Type: AccountTypeAPIKey,
				Credentials: map[string]any{
					"pool_mode": test.pool, "pool_mode_retry_count": float64(test.retryCount),
				},
			}
			if test.codes != nil {
				account.Credentials["pool_mode_retry_status_codes"] = test.codes
			}
			svc := &OpenAIGatewayService{}
			require.Equal(t, test.want, svc.shouldFailoverOpenAIUpstreamResponse(account, test.status, "", []byte(test.body)))
		})
	}
}

func TestOpenAIConfiguredHTTP400ReturnsRetryBeforeOutput(t *testing.T) {
	for _, passthrough := range []bool{false, true} {
		name := "native"
		if passthrough {
			name = "passthrough"
		}
		t.Run(name, func(t *testing.T) {
			c, rec := newOpenAIUpstreamErrorTestContext(t)
			account := &Account{
				ID: 59327, Name: "configured-http-retry", Platform: PlatformOpenAI, Type: AccountTypeAPIKey,
				Status: StatusActive, Schedulable: true, Concurrency: 1,
				Credentials: map[string]any{
					"api_key": "sk-test", "base_url": "https://api.example.test",
					"pool_mode": true, "pool_mode_retry_count": float64(10),
					"pool_mode_retry_status_codes": []any{float64(http.StatusBadRequest)},
				},
				Extra: map[string]any{
					"openai_passthrough": passthrough, "openai_responses_supported": true,
				},
			}
			svc := &OpenAIGatewayService{
				cfg:          &config.Config{},
				httpUpstream: &httpUpstreamRecorder{resp: newOpenAIUpstreamErrorResponse(http.StatusBadRequest, openAIInvalidURLValueBody)},
			}

			result, err := svc.Forward(context.Background(), c, account, []byte(`{"model":"gpt-6.1-sol","input":"hello","stream":true}`))

			require.Nil(t, result)
			var failoverErr *UpstreamFailoverError
			require.ErrorAs(t, err, &failoverErr)
			require.Equal(t, http.StatusBadRequest, failoverErr.StatusCode)
			require.True(t, failoverErr.RetryableOnSameAccount)
			require.JSONEq(t, openAIInvalidURLValueBody, string(failoverErr.ResponseBody))
			require.False(t, IsResponseCommitted(c))
			require.False(t, c.Writer.Written())
			require.Empty(t, rec.Body.String())
		})
	}
}
