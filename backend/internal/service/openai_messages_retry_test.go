package service

import (
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/Wei-Shaw/sub2api/internal/config"
	"github.com/gin-gonic/gin"
	"github.com/stretchr/testify/require"
)

const messagesBPSFailure = `{"response":{"created_at":1793894912,"error":{"code":"bps_stream_error","message":"Upstream request failed, please try again later","type":"bps_stream_error"},"id":"resp_bps_messages","model":"gpt-6-luna","object":"response","status":"failed"},"type":"response.failed"}`

func TestOpenAIMessagesBPSFailureAfterPreambleRetries(t *testing.T) {
	gin.SetMode(gin.TestMode)
	for _, prelude := range []string{
		"",
		`{"type":"response.output_item.added","output_index":0,"item":{"type":"reasoning","id":"rs_empty","summary":[]}}`,
		`{"type":"response.output_text.delta","output_index":0,"delta":""}`,
		`{"type":"response.output_item.added","output_index":0,"item":{"type":"function_call","id":"fc_pending","call_id":"call_pending","name":"lookup","arguments":""}}`,
	} {
		for _, async := range []bool{false, true} {
			t.Run(fmt.Sprintf("%s/async=%t", prelude, async), func(t *testing.T) {
				cfg := &config.Config{}
				if async {
					cfg.Gateway.StreamDataIntervalTimeout = 5
				}
				svc := &OpenAIGatewayService{cfg: cfg}
				rec := httptest.NewRecorder()
				c, _ := gin.CreateTestContext(rec)
				c.Request = httptest.NewRequest(http.MethodPost, "/v1/messages", nil)
				body := "data: " + `{"type":"response.created","response":{"id":"resp_bps_messages","status":"in_progress"}}` + "\n\n"
				if prelude != "" {
					body += "data: " + prelude + "\n\n"
				}
				body += "event: response.failed\ndata: " + messagesBPSFailure + "\n\n"
				resp := &http.Response{StatusCode: http.StatusOK, Header: http.Header{"X-Request-Id": []string{"rid-bps-messages"}}, Body: io.NopCloser(strings.NewReader(body))}
				account := &Account{ID: 59328, Platform: PlatformOpenAI, Type: AccountTypeAPIKey, Credentials: map[string]any{
					"pool_mode": true, "pool_mode_retry_status_codes": []any{float64(http.StatusBadGateway)},
				}}
				_, err := svc.handleAnthropicStreamingResponse(resp, c, account, "gpt-6-sol", "gpt-6-sol", "gpt-6-luna", time.Now())
				var failoverErr *UpstreamFailoverError
				require.ErrorAs(t, err, &failoverErr)
				require.Equal(t, http.StatusBadGateway, failoverErr.StatusCode)
				require.True(t, failoverErr.RetryableOnSameAccount)
				require.False(t, c.Writer.Written())
				require.Empty(t, rec.Body.String(), "failed attempt metadata must not reach the client")
			})
		}
	}
}

func TestOpenAIMessagesBPSFailurePreservesOutputBoundary(t *testing.T) {
	gin.SetMode(gin.TestMode)
	for _, content := range []string{
		`{"type":"response.output_text.delta","output_index":0,"delta":"partial"}`,
		`{"type":"response.output_item.added","output_index":0,"item":{"type":"reasoning","summary":[]}}` + "\n\ndata: " + `{"type":"response.reasoning_summary_text.delta","output_index":0,"delta":"thinking"}`,
		`{"type":"response.output_item.added","output_index":0,"item":{"type":"reasoning","encrypted_content":"ciphertext","summary":[]}}`,
		`{"type":"response.output_item.added","output_index":0,"item":{"type":"function_call","id":"fc_done","call_id":"call_done","name":"lookup","arguments":""}}` + "\n\ndata: " + `{"type":"response.function_call_arguments.done","output_index":0,"arguments":""}`,
	} {
		t.Run(content, func(t *testing.T) {
			logSink, restore := captureStructuredLog(t)
			defer restore()
			svc := &OpenAIGatewayService{cfg: &config.Config{}}
			rec := httptest.NewRecorder()
			c, _ := gin.CreateTestContext(rec)
			c.Request = httptest.NewRequest(http.MethodPost, "/v1/messages", nil)
			body := "data: " + `{"type":"response.created","response":{"id":"resp_output"}}` + "\n\ndata: " + content + "\n\ndata: " + messagesBPSFailure + "\n\n"
			resp := &http.Response{StatusCode: http.StatusOK, Header: http.Header{}, Body: io.NopCloser(strings.NewReader(body))}
			account := &Account{ID: 59328, Platform: PlatformOpenAI, Type: AccountTypeAPIKey, Credentials: map[string]any{
				"pool_mode": true, "pool_mode_retry_status_codes": []any{float64(http.StatusBadGateway)},
			}}
			_, err := svc.handleAnthropicStreamingResponse(resp, c, account, "model", "model", "model", time.Now())
			var failoverErr *UpstreamFailoverError
			require.Error(t, err)
			require.NotErrorAs(t, err, &failoverErr)
			require.Equal(t, 1, strings.Count(rec.Body.String(), "event: message_start"))
			require.Equal(t, 1, strings.Count(rec.Body.String(), "event: error"))
			require.NotContains(t, rec.Body.String(), "event: message_stop")
			require.True(t, logSink.ContainsMessage("openai.same_account_retry_suppressed"))
			require.True(t, logSink.ContainsField("first_output_event"))
		})
	}
}

func TestOpenAIMessagesPreambleCommitsOnSuccess(t *testing.T) {
	gin.SetMode(gin.TestMode)
	for _, text := range []string{"", "answer"} {
		t.Run("text="+text, func(t *testing.T) {
			svc := &OpenAIGatewayService{cfg: &config.Config{}}
			rec := httptest.NewRecorder()
			c, _ := gin.CreateTestContext(rec)
			c.Request = httptest.NewRequest(http.MethodPost, "/v1/messages", nil)
			body := "data: " + `{"type":"response.created","response":{"id":"resp_success"}}` + "\n\n"
			if text != "" {
				body += "data: " + `{"type":"response.output_text.delta","output_index":0,"delta":"answer"}` + "\n\n"
			}
			body += "data: " + `{"type":"response.completed","response":{"id":"resp_success","status":"completed","output":[],"usage":{"input_tokens":1,"output_tokens":1}}}` + "\n\n"
			resp := &http.Response{StatusCode: http.StatusOK, Header: http.Header{}, Body: io.NopCloser(strings.NewReader(body))}
			result, err := svc.handleAnthropicStreamingResponse(resp, c, &Account{ID: 1, Platform: PlatformOpenAI}, "model", "model", "model", time.Now())
			require.NoError(t, err)
			require.Equal(t, 1, result.Usage.InputTokens)
			require.Equal(t, 1, strings.Count(rec.Body.String(), "event: message_start"))
			require.Equal(t, 1, strings.Count(rec.Body.String(), "event: message_stop"))
			require.Less(t, strings.Index(rec.Body.String(), "event: message_start"), strings.Index(rec.Body.String(), "event: message_stop"))
			if text != "" {
				require.Contains(t, rec.Body.String(), text)
			}
		})
	}
}
