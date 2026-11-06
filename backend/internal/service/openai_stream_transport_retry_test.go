package service

import (
	"context"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/Wei-Shaw/sub2api/internal/config"
	"github.com/gin-gonic/gin"
	"github.com/stretchr/testify/require"
	"github.com/tidwall/gjson"
)

func openAIStreamTransportRetryPayloads() map[string]string {
	payloads := map[string]string{
		"message_only":           `{"type":"error","message":"Upstream response stream was interrupted"}`,
		"generic_code":           `{"type":"error","error":{"code":"upstream_error","message":"Upstream response stream was interrupted"}}`,
		"message_only_http2":     `{"type":"error","message":"Upstream HTTP/2 stream failed"}`,
		"message_only_truncated": `{"type":"error","message":"Upstream response stream ended before completion"}`,
	}
	for _, code := range []string{OpenAIUpstreamStreamReadErrorCode, OpenAIUpstreamHTTP2StreamErrorCode, OpenAIUpstreamStreamTruncatedCode} {
		detail := fmt.Sprintf(`{"code":%q,"message":"The upstream connection closed"}`, code)
		payloads[code+"/flat"] = fmt.Sprintf(`{"type":"error","code":%q,"message":"The upstream connection closed"}`, code)
		payloads[code+"/nested"] = `{"type":"error","error":` + detail + `}`
		payloads[code+"/response"] = `{"type":"error","response":{"error":` + detail + `}}`
	}
	return payloads
}

func TestOpenAIStreamTransportErrorEventsRetryConfiguredAccount(t *testing.T) {
	gin.SetMode(gin.TestMode)
	for name, payload := range openAIStreamTransportRetryPayloads() {
		for _, path := range []string{"native", "passthrough", "messages", "chat"} {
			for _, output := range []string{"reasoning", "tool", "text"} {
				t.Run(name+"/"+path+"/"+output, func(t *testing.T) {
					c, rec := newOpenAIConfiguredRetryContext()
					cfg := &config.Config{}
					cfg.Gateway.LogUpstreamErrorBody = true
					cfg.Gateway.LogUpstreamErrorBodyMaxBytes = 2048
					resp := &http.Response{
						StatusCode: http.StatusOK,
						Header:     http.Header{"X-Request-Id": []string{"stream-transport-retry"}},
						Body:       io.NopCloser(strings.NewReader(configuredRetryOutput(output) + configuredRetryEvent(payload))),
					}
					err := runConfiguredRetryStream(&OpenAIGatewayService{cfg: cfg}, c, resp, newConfiguredStreamRetryAccount(), path)
					var failover *UpstreamFailoverError
					require.ErrorAs(t, err, &failover)
					require.Equal(t, http.StatusBadGateway, failover.StatusCode)
					require.True(t, failover.RetryableOnSameAccount)
					require.False(t, c.Writer.Written())
					require.Empty(t, rec.Body.String(), "failed output must remain private for a safe retry")
					value, ok := c.Get(OpsUpstreamErrorsKey)
					require.True(t, ok)
					events, ok := value.([]*OpsUpstreamErrorEvent)
					require.True(t, ok)
					require.Len(t, events, 1)
					require.Equal(t, "failover", events[0].Kind)
					require.Equal(t, http.StatusBadGateway, events[0].UpstreamStatusCode)
					require.Equal(t, "stream-transport-retry", events[0].UpstreamRequestID)
					require.JSONEq(t, payload, events[0].Detail)
				})
			}
		}
	}
}

func TestOpenAIStreamTransportErrorEventsPreserveTerminalRejections(t *testing.T) {
	for name, payload := range map[string]string{
		"unknown":                           `{"type":"error","error":{"message":"upstream rejected request"}}`,
		"invalid_request":                   `{"type":"error","error":{"type":"invalid_request_error","code":"upstream_stream_read_error","message":"Upstream response stream was interrupted"}}`,
		"flat_invalid_request":              `{"type":"error","code":"invalid_request_error","message":"Upstream response stream was interrupted"}`,
		"content_policy":                    `{"type":"error","error":{"code":"content_policy_violation","message":"Upstream response stream was interrupted"}}`,
		"flat_content_policy":               `{"type":"error","code":"content_policy_violation","message":"Upstream response stream was interrupted"}`,
		"flat_auth_code":                    `{"type":"error","code":"invalid_api_key","message":"Upstream response stream was interrupted"}`,
		"flat_rate_limit_code":              `{"type":"error","code":"rate_limit_exceeded","message":"Upstream response stream was interrupted"}`,
		"flat_capacity_code":                `{"type":"error","code":"server_is_overloaded","message":"Upstream response stream was interrupted"}`,
		"other_flat_code":                   `{"type":"error","code":"unrecognized_rejection","message":"Upstream response stream was interrupted"}`,
		"other_nested_code":                 `{"type":"error","error":{"code":"unrecognized_rejection","message":"Upstream response stream was interrupted"}}`,
		"nested_policy_over_flat_transport": `{"type":"error","code":"upstream_stream_read_error","error":{"code":"content_policy_violation","message":"Upstream response stream was interrupted"}}`,
		"cyber_policy":                      `{"type":"error","error":{"code":"cyber_policy","message":"Upstream response stream was interrupted"}}`,
		"context_limit":                     `{"type":"error","error":{"code":"context_length_exceeded","message":"maximum context length exceeded"}}`,
		"invalid_encrypted":                 `{"type":"error","error":{"code":"invalid_encrypted_content","message":"Invalid encrypted content"}}`,
	} {
		t.Run(name, func(t *testing.T) {
			require.False(t, openAIStreamErrorEventShouldFailover([]byte(payload), extractOpenAISSEErrorMessage([]byte(payload))))
		})
	}
}

func TestOpenAIStreamTransportErrorEventsPreserveSemanticStatus(t *testing.T) {
	for _, tc := range []struct {
		code   string
		status int
	}{
		{"invalid_api_key", http.StatusUnauthorized},
		{"rate_limit_exceeded", http.StatusTooManyRequests},
	} {
		for name, payload := range map[string]string{
			"nested":               fmt.Sprintf(`{"type":"error","error":{"code":%q,"message":"Upstream response stream was interrupted"}}`, tc.code),
			"response":             fmt.Sprintf(`{"type":"error","response":{"error":{"code":%q,"message":"Upstream response stream was interrupted"}}}`, tc.code),
			"flat_explicit_status": fmt.Sprintf(`{"type":"error","code":%q,"status":%d,"message":"Upstream response stream was interrupted"}`, tc.code, tc.status),
		} {
			t.Run(tc.code+"/"+name, func(t *testing.T) {
				message := extractOpenAISSEErrorMessage([]byte(payload))
				require.Equal(t, tc.status, openAIStreamFailedEventSemanticStatus([]byte(payload), message))
				require.True(t, openAIStreamErrorEventShouldFailover([]byte(payload), message))
			})
		}
	}
}

func TestOpenAIStreamTransportErrorEventsWSBridgeReplaySafety(t *testing.T) {
	gin.SetMode(gin.TestMode)
	for name, failure := range openAIStreamTransportRetryPayloads() {
		for _, tc := range []struct {
			name     string
			turn     int
			output   string
			failover bool
		}{
			{"before_output", 1, "", true},
			{"after_text", 1, "text", false},
			{"after_reasoning", 1, "reasoning", false},
			{"after_tool", 1, "tool", false},
			{"followup_turn", 2, "", false},
		} {
			t.Run(name+"/"+tc.name, func(t *testing.T) {
				body := configuredRetryEvent(`{"type":"response.created","response":{"id":"resp_transport_retry"}}`) +
					configuredRetryEvent(`{"type":"keepalive"}`)
				if tc.output != "" {
					body += configuredRetryOutput(tc.output)
				}
				body += configuredRetryEvent(failure)
				upstream := &httpUpstreamRecorder{resp: &http.Response{
					StatusCode: http.StatusOK,
					Header:     http.Header{"X-Request-Id": []string{"ws-transport-retry"}},
					Body:       io.NopCloser(strings.NewReader(body)),
				}}
				svc := &OpenAIGatewayService{cfg: &config.Config{}, httpUpstream: upstream}
				account := newConfiguredStreamRetryAccount()
				account.Status, account.Schedulable, account.Concurrency = StatusActive, true, 1
				c, _ := gin.CreateTestContext(httptest.NewRecorder())
				c.Request = httptest.NewRequest(http.MethodGet, "/v1/responses", nil)
				payload := []byte(`{"type":"response.create","model":"gpt-6.1-sol","input":"hi"}`)
				var writes [][]byte
				result, err := svc.proxyOpenAIWSHTTPBridgeTurn(context.Background(), c, account, "sk-test", payload, len(payload),
					"gpt-6.1-sol", "", "", "", "", tc.turn, func(message []byte) error {
						writes = append(writes, append([]byte(nil), message...))
						return nil
					})
				var failover *UpstreamFailoverError
				if tc.failover {
					require.Nil(t, result)
					require.ErrorAs(t, err, &failover)
					require.Equal(t, http.StatusBadGateway, failover.StatusCode)
					require.True(t, failover.RetryableOnSameAccount)
					require.Len(t, writes, 1, "only the keepalive can escape before a retry")
					require.Equal(t, "keepalive", gjson.GetBytes(writes[0], "type").String())
				} else {
					require.NotErrorAs(t, err, &failover)
					require.NotNil(t, result)
					require.Equal(t, "response.failed", result.UpstreamTerminalEvent)
					require.NotEmpty(t, writes)
					require.Equal(t, "response.failed", gjson.GetBytes(writes[len(writes)-1], "type").String())
				}
			})
		}
	}
}

func TestOpenAIStreamTransportErrorEventsDoNotReplayCommittedOutput(t *testing.T) {
	for _, policy := range []string{"non_pool", "zero_retries", "502_not_configured"} {
		for _, path := range []string{"native", "passthrough", "messages", "chat"} {
			t.Run(policy+"/"+path, func(t *testing.T) {
				account := newConfiguredStreamRetryAccount()
				switch policy {
				case "non_pool":
					account.Credentials["pool_mode"] = false
				case "zero_retries":
					account.Credentials["pool_mode_retry_count"] = float64(0)
				case "502_not_configured":
					account.Credentials["pool_mode_retry_status_codes"] = []any{float64(http.StatusServiceUnavailable)}
				}
				c, rec := newOpenAIConfiguredRetryContext()
				failure := configuredRetryEvent(`{"type":"error","code":"upstream_stream_read_error","message":"Upstream response stream was interrupted"}`)
				resp := &http.Response{
					StatusCode: http.StatusOK,
					Header:     http.Header{},
					Body:       io.NopCloser(strings.NewReader(configuredRetryOutput("text") + failure)),
				}
				err := runConfiguredRetryStream(&OpenAIGatewayService{cfg: &config.Config{}}, c, resp, account, path)
				var failover *UpstreamFailoverError
				require.Error(t, err)
				require.NotErrorAs(t, err, &failover)
				require.Contains(t, rec.Body.String(), "private answer")
			})
		}
	}
}
