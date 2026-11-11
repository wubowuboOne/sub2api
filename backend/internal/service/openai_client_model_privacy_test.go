package service

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/Wei-Shaw/sub2api/internal/config"
	"github.com/Wei-Shaw/sub2api/internal/pkg/ctxkey"
	"github.com/gin-gonic/gin"
	"github.com/stretchr/testify/require"
	"github.com/tidwall/gjson"
)

const reasoningModelPrivacyMessage = "Unsupported value: 'xhigh' is not supported with the 'private-real-model' model. Supported values are: 'low', 'medium', and 'high'."

func reasoningModelPrivacyBody(t *testing.T) []byte {
	t.Helper()
	body, err := json.Marshal(gin.H{"error": gin.H{
		"type": "invalid_request_error", "code": "unsupported_value",
		"param": "reasoning.effort", "message": reasoningModelPrivacyMessage,
	}})
	require.NoError(t, err)
	return body
}

func TestOpenAIClientModelPrivacyZeroUsage(t *testing.T) {
	gin.SetMode(gin.TestMode)
	for _, path := range []string{"native_json", "passthrough_json", "native_stream", "passthrough_stream", "native_sse_to_json", "passthrough_sse_to_json"} {
		for _, mappedModel := range []string{"", "public-model", "account-mapped-model"} {
			t.Run(path+"/"+mappedModel, func(t *testing.T) {
				payload := `{"id":"resp_privacy","object":"response","created_at":1700000000,"status":"completed","model":"private-real-model","output":[],"usage":{"input_tokens":0,"output_tokens":0}}`
				body, contentType := payload, "application/json"
				if !strings.HasSuffix(path, "_json") || strings.Contains(path, "sse_to_json") {
					body = "data: {\"type\":\"response.completed\",\"response\":" + payload + "}\n\ndata: [DONE]\n\n"
					contentType = "text/event-stream"
				}
				c, rec := newOpenAIUpstreamErrorTestContext(t)
				svc := &OpenAIGatewayService{cfg: &config.Config{}}
				account := &Account{ID: 1, Platform: PlatformOpenAI, Type: AccountTypeAPIKey}
				resp := &http.Response{StatusCode: 200, Header: http.Header{"Content-Type": {contentType}}, Body: io.NopCloser(strings.NewReader(body))}
				var err error
				switch path {
				case "native_json", "native_sse_to_json":
					_, err = svc.handleNonStreamingResponse(context.Background(), resp, c, account, "public-model", mappedModel)
				case "passthrough_json", "passthrough_sse_to_json":
					_, err = svc.handleNonStreamingResponsePassthrough(context.Background(), resp, c, account, "public-model", mappedModel)
				case "native_stream":
					_, err = svc.handleStreamingResponse(context.Background(), resp, c, account, time.Now(), "public-model", mappedModel)
				case "passthrough_stream":
					_, err = svc.handleStreamingResponsePassthrough(context.Background(), resp, c, account, time.Now(), "public-model", mappedModel)
				}
				require.NoError(t, err)
				require.Equal(t, 200, rec.Code)
				require.Contains(t, rec.Body.String(), `"model":"public-model"`)
				require.NotContains(t, rec.Body.String(), "private-real-model")
				require.Contains(t, rec.Body.String(), `"input_tokens":0`)
				require.Equal(t, "private-real-model", observedUpstreamResponseModel(c), "internal evidence must retain the real upstream model")
			})
		}
	}
}

func TestOpenAIClientModelPrivacyProtocolFieldsOnly(t *testing.T) {
	svc := &OpenAIGatewayService{}
	body := `{"model":"private-real-model","response":{"model":"private-real-model"},"text":"private-real-model","tool":{"model":"private-real-model","arguments":"{\"model\":\"private-real-model\"}"}}`
	want := `{"model":"public-model","response":{"model":"public-model"},"text":"private-real-model","tool":{"model":"private-real-model","arguments":"{\"model\":\"private-real-model\"}"}}`
	for _, sent := range []string{"", "public-model", "account-mapped-model"} {
		require.Equal(t, want, string(svc.replaceModelInResponseBody([]byte(body), sent, "public-model")))
		require.Equal(t, "data: "+want, svc.replaceModelInSSELine("data: "+body, sent, "public-model"))
		require.Equal(t, want, string(replaceOpenAIWSMessageModel([]byte(body), sent, "public-model")))
	}
}

func TestOpenAIClientModelPrivacyForward(t *testing.T) {
	for _, passthrough := range []bool{false, true} {
		for _, stream := range []bool{false, true} {
			for _, requestModel := range []string{"public-model", "channel-target-model"} {
				name := map[bool]string{false: "native", true: "passthrough"}[passthrough] + "/" + map[bool]string{false: "json", true: "stream"}[stream] + "/" + requestModel
				t.Run(name, func(t *testing.T) {
					c, rec := newOpenAIUpstreamErrorTestContext(t)
					c.Request = c.Request.WithContext(context.WithValue(c.Request.Context(), ctxkey.Model, "public-model"))
					body := `{"id":"resp_privacy","object":"response","created_at":1700000000,"status":"completed","model":"private-real-model","output":[],"usage":{"input_tokens":0,"output_tokens":0}}`
					resp := newOpenAIUpstreamErrorResponse(200, body)
					if stream {
						resp = newOpenAIUpstreamErrorResponse(200, "data: {\"type\":\"response.completed\",\"response\":"+body+"}\n\n")
						resp.Header.Set("Content-Type", "text/event-stream")
					}
					upstream := &httpUpstreamRecorder{resp: resp}
					svc := &OpenAIGatewayService{cfg: &config.Config{}, httpUpstream: upstream}
					account := &Account{
						ID: 1, Platform: PlatformOpenAI, Type: AccountTypeAPIKey, Status: StatusActive, Schedulable: true,
						Credentials: map[string]any{"api_key": "sk-test", "base_url": "https://api.example.test"},
						Extra:       map[string]any{"openai_passthrough": passthrough, "openai_responses_supported": true},
					}
					request, err := json.Marshal(gin.H{"model": requestModel, "input": "hello", "stream": stream})
					require.NoError(t, err)
					result, err := svc.Forward(context.Background(), c, account, request)
					require.NoError(t, err)
					require.NotNil(t, result)
					require.Len(t, upstream.bodies, 1, "response privacy must not trigger another upstream request")
					require.Equal(t, requestModel, gjson.GetBytes(upstream.lastBody, "model").String())
					require.Equal(t, requestModel, result.Model, "wire privacy must not change internal model attribution")
					require.Equal(t, "private-real-model", result.UpstreamResponseModel)
					require.Zero(t, result.Usage.InputTokens)
					require.Zero(t, result.Usage.OutputTokens)
					require.NotContains(t, rec.Body.String(), "private-real-model")
					require.Contains(t, rec.Body.String(), `"model":"public-model"`)
				})
			}
		}
	}
}

func TestOpenAIReasoningErrorPrivacyMessageShapes(t *testing.T) {
	for _, tc := range []struct {
		name, message, param string
		changed              bool
	}{
		{name: "standard_param", message: reasoningModelPrivacyMessage, param: "reasoning.effort", changed: true},
		{name: "flat_param", message: reasoningModelPrivacyMessage, param: "reasoning_effort", changed: true},
		{name: "missing_param", message: reasoningModelPrivacyMessage, changed: true},
		{name: "model_first", message: `Model "private-real-model" does not support this parameter`, changed: true},
		{name: "unquoted_model", message: `private-real-model does not support reasoning effort xhigh`, changed: true},
		{name: "schema_error", message: "Unsupported property 'reasoning_effort' in function schema", param: "tools[0].parameters"},
		{name: "ordinary_error", message: "Request payload is too large"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			got := openAIClientReasoningErrorMessage(tc.message, tc.param)
			if tc.changed {
				require.NotContains(t, got, "private-real-model")
			} else {
				require.Equal(t, tc.message, got)
			}
		})
	}
}

func TestOpenAIReasoningErrorPrivacyMappedMinimal(t *testing.T) {
	const message = "gpt-6.1-sol does not support reasoning effort \"minimal\"; use low, medium, high, xhigh or max"
	const want = "The requested reasoning effort is not supported by this model. Choose a supported reasoning effort and try again."
	for _, param := range []string{"", "model", "reasoning", "reasoning.effort", "reasoning_effort"} {
		name := param
		if name == "" {
			name = "missing_param"
		}
		t.Run(name, func(t *testing.T) {
			errorPayload := gin.H{"type": "invalid_request_error", "code": "unsupported_value", "message": message}
			if param != "" {
				errorPayload["param"] = param
			}
			body, err := json.Marshal(gin.H{"error": errorPayload})
			require.NoError(t, err)
			c, rec := newOpenAIUpstreamErrorTestContext(t)
			WriteOpenAIUpstreamClientError(c, http.StatusBadRequest, body, message)
			require.Equal(t, http.StatusBadRequest, rec.Code)
			require.Equal(t, want, gjson.Get(rec.Body.String(), "error.message").String())
			require.NotContains(t, rec.Body.String(), "gpt-6.1-sol")
			require.Equal(t, "unsupported_value", gjson.Get(rec.Body.String(), "error.code").String())
			require.Equal(t, param, gjson.Get(rec.Body.String(), "error.param").String())
			event, err := json.Marshal(gin.H{"type": "response.failed", "response": gin.H{"model": "gpt-6.1-sol", "error": errorPayload}})
			require.NoError(t, err)
			clientEvent, changed := sanitizeOpenAIClientReasoningError(event)
			require.True(t, changed)
			require.Equal(t, want, gjson.GetBytes(clientEvent, "response.error.message").String())
			clientEvent = rewriteOpenAIClientModel(clientEvent, "gpt-6-astra")
			require.NotContains(t, string(clientEvent), "gpt-6.1-sol")
			require.Equal(t, message, gjson.GetBytes(event, "response.error.message").String())
			require.Equal(t, message, gjson.GetBytes(body, "error.message").String())
		})
	}
}

func TestOpenAIReasoningErrorPrivacyHTTP(t *testing.T) {
	for _, path := range []string{"native", "chat", "messages", "retry_exhausted"} {
		t.Run(path, func(t *testing.T) {
			c, rec := newOpenAIUpstreamErrorTestContext(t)
			body := reasoningModelPrivacyBody(t)
			svc := &OpenAIGatewayService{cfg: &config.Config{}}
			account := &Account{ID: 1, Platform: PlatformOpenAI, Type: AccountTypeAPIKey}
			resp := newOpenAIUpstreamErrorResponse(http.StatusBadRequest, string(body))
			var err error
			switch path {
			case "native":
				_, err = svc.handleErrorResponse(context.Background(), resp, c, account, nil)
			case "chat":
				_, err = svc.handleCompatErrorResponse(resp, c, account, writeChatCompletionsError)
			case "messages":
				_, err = svc.handleCompatErrorResponse(resp, c, account, writeAnthropicError)
			case "retry_exhausted":
				WriteOpenAIUpstreamClientError(c, 400, body, reasoningModelPrivacyMessage)
			}
			require.Equal(t, 400, rec.Code)
			require.Equal(t, "invalid_request_error", gjson.Get(rec.Body.String(), "error.type").String())
			require.NotContains(t, rec.Body.String(), "private-real-model")
			require.Contains(t, strings.ToLower(gjson.Get(rec.Body.String(), "error.message").String()), "reasoning effort")
			if path == "native" || path == "retry_exhausted" {
				require.Equal(t, "reasoning.effort", gjson.Get(rec.Body.String(), "error.param").String())
				require.Equal(t, "unsupported_value", gjson.Get(rec.Body.String(), "error.code").String())
			}
			if path != "retry_exhausted" {
				require.Error(t, err)
				require.Contains(t, c.GetString(OpsUpstreamErrorMessageKey), "private-real-model")
				require.Contains(t, err.Error(), "private-real-model")
				var failoverErr *UpstreamFailoverError
				require.NotErrorAs(t, err, &failoverErr)
			}
		})
	}
}

func TestOpenAIReasoningErrorPrivacyStream(t *testing.T) {
	for _, passthrough := range []bool{false, true} {
		for _, bare := range []bool{false, true} {
			t.Run(strings.Join([]string{map[bool]string{false: "native", true: "passthrough"}[passthrough], map[bool]string{false: "failed", true: "bare"}[bare]}, "/"), func(t *testing.T) {
				c, rec := newOpenAIUpstreamErrorTestContext(t)
				payload := reasoningModelPrivacyBody(t)
				if !bare {
					payload = []byte(`{"type":"response.failed","response":{"id":"resp_privacy","created_at":1700000000,"status":"failed","model":"private-real-model","error":` + gjson.GetBytes(payload, "error").Raw + `}}`)
				}
				body := "data: " + string(payload) + "\n\n"
				resp := newOpenAIUpstreamErrorResponse(200, body)
				resp.Header.Set("Content-Type", "text/event-stream")
				svc := &OpenAIGatewayService{cfg: &config.Config{}}
				account := &Account{ID: 1, Platform: PlatformOpenAI, Type: AccountTypeAPIKey}
				var err error
				if passthrough {
					_, err = svc.handleStreamingResponsePassthrough(context.Background(), resp, c, account, time.Now(), "public-model", "public-model")
				} else {
					_, err = svc.handleStreamingResponse(context.Background(), resp, c, account, time.Now(), "public-model", "public-model")
				}
				require.Error(t, err)
				require.NotContains(t, rec.Body.String(), "private-real-model")
				require.Contains(t, rec.Body.String(), "reasoning effort")
			})
		}
	}
}

func TestOpenAIReasoningErrorPrivacyDoesNotChangeSchemaErrors(t *testing.T) {
	c, rec := newOpenAIUpstreamErrorTestContext(t)
	WriteOpenAIUpstreamClientError(c, 400, []byte(openAIInvalidFunctionParametersBody), gjson.Get(openAIInvalidFunctionParametersBody, "error.message").String())
	require.Equal(t, gjson.Get(openAIInvalidFunctionParametersBody, "error.message").String(), gjson.Get(rec.Body.String(), "error.message").String())
}

func TestOpenAIReasoningErrorPrivacyPreservesConfiguredRetry(t *testing.T) {
	for _, passthrough := range []bool{false, true} {
		name := map[bool]string{false: "native", true: "passthrough"}[passthrough]
		t.Run(name, func(t *testing.T) {
			c, rec := newOpenAIUpstreamErrorTestContext(t)
			body := reasoningModelPrivacyBody(t)
			account := &Account{
				ID: 1, Platform: PlatformOpenAI, Type: AccountTypeAPIKey, Status: StatusActive, Schedulable: true,
				Credentials: map[string]any{
					"api_key": "sk-test", "base_url": "https://api.example.test",
					"pool_mode": true, "pool_mode_retry_count": float64(3), "pool_mode_retry_status_codes": []any{float64(400)},
				},
				Extra: map[string]any{"openai_passthrough": passthrough, "openai_responses_supported": true},
			}
			svc := &OpenAIGatewayService{cfg: &config.Config{}, httpUpstream: &httpUpstreamRecorder{resp: newOpenAIUpstreamErrorResponse(400, string(body))}}
			result, err := svc.Forward(context.Background(), c, account, []byte(`{"model":"public-model","input":"hello","stream":true,"reasoning":{"effort":"xhigh"}}`))
			require.Nil(t, result)
			var failoverErr *UpstreamFailoverError
			require.ErrorAs(t, err, &failoverErr)
			require.Equal(t, 400, failoverErr.StatusCode)
			require.True(t, failoverErr.RetryableOnSameAccount)
			require.JSONEq(t, string(body), string(failoverErr.ResponseBody))
			require.False(t, c.Writer.Written())
			require.Empty(t, rec.Body.String())
		})
	}
}

func TestOpenAIReasoningErrorPrivacySyntheticTerminals(t *testing.T) {
	body := reasoningModelPrivacyBody(t)
	for _, terminal := range []string{
		buildOpenAIResponseFailedSSE("resp_privacy", "public-model", 1700000000, body, ""),
		string(buildOpenAIWSHTTPBridgeFailedEvent("resp_privacy", "public-model", 1700000000, body, "")),
	} {
		require.NotContains(t, terminal, "private-real-model")
		require.Contains(t, terminal, "reasoning effort")
		require.Contains(t, terminal, `"model":"public-model"`)
	}
}

func TestOpenAIReasoningErrorPrivacyDoesNotChangeOutput(t *testing.T) {
	body := []byte(`{"type":"response.completed","response":{"model":"public-model","output":[{"type":"function_call","arguments":"{\"error\":{\"message\":\"Model 'private-real-model' does not support reasoning\"}}"}],"metadata":{"error":{"message":"private-real-model"}}}}`)
	got, changed := sanitizeOpenAIClientReasoningError(body)
	require.False(t, changed)
	require.Equal(t, body, got)
}

func TestOpenAIReasoningErrorPrivacyCompatTerminal(t *testing.T) {
	for _, path := range []string{"chat_buffered", "messages_buffered", "chat_stream", "messages_stream"} {
		t.Run(path, func(t *testing.T) {
			c, rec := newOpenAIUpstreamErrorTestContext(t)
			payload := `{"type":"response.failed","response":{"id":"resp_privacy","status":"failed","model":"private-real-model","error":` + gjson.GetBytes(reasoningModelPrivacyBody(t), "error").Raw + `}}`
			resp := newOpenAIUpstreamErrorResponse(200, "data: "+payload+"\n\n")
			resp.Header.Set("Content-Type", "text/event-stream")
			svc := &OpenAIGatewayService{cfg: &config.Config{}}
			account := &Account{ID: 1, Platform: PlatformOpenAI, Type: AccountTypeAPIKey}
			var err error
			switch path {
			case "chat_buffered":
				_, err = svc.handleChatBufferedStreamingResponse(resp, c, account, "public-model", "public-model", "public-model", time.Now())
			case "messages_buffered":
				_, err = svc.handleAnthropicBufferedStreamingResponse(resp, c, account, "public-model", "public-model", "public-model", time.Now())
			case "chat_stream":
				_, err = svc.handleChatStreamingResponse(resp, c, account, "public-model", "public-model", "public-model", time.Now(), 0)
			case "messages_stream":
				_, err = svc.handleAnthropicStreamingResponse(resp, c, account, "public-model", "public-model", "public-model", time.Now())
			}
			require.Error(t, err)
			if strings.HasSuffix(path, "_buffered") {
				var failoverErr *UpstreamFailoverError
				require.ErrorAs(t, err, &failoverErr, "buffered compatibility errors must keep their existing retry decision")
				require.Contains(t, string(failoverErr.ResponseBody), "private-real-model")
				require.Empty(t, rec.Body.String(), "a retryable attempt must not write a client response")
				return
			}
			require.NotEmpty(t, rec.Body.String())
			require.NotContains(t, rec.Body.String(), "private-real-model")
			require.Contains(t, rec.Body.String(), "reasoning effort")
			require.Contains(t, err.Error(), "private-real-model", "internal errors must retain original diagnostics")
		})
	}
}
