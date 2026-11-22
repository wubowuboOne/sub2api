package service

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"

	"github.com/Wei-Shaw/sub2api/internal/config"
	"github.com/Wei-Shaw/sub2api/internal/pkg/apicompat"
	"github.com/gin-gonic/gin"
	"github.com/stretchr/testify/require"
	"github.com/tidwall/gjson"
)

const messagesContinuationUnsupported = "previous_response_id requires an OpenAI API-key account for HTTP requests"

func messagesContinuationFailure(eventType, code, message, prelude string) *http.Response {
	errorJSON, _ := json.Marshal(map[string]string{"code": code, "message": message})
	payload := `{"type":"response.failed","response":{"id":"resp_failed_continuation","model":"gpt-6.1-sol","status":"failed","error":` + string(errorJSON) + `}}`
	if eventType == "error" {
		payload = `{"type":"error","error":` + string(errorJSON) + `}`
	}
	stream := "data: " + `{"type":"response.created","response":{"id":"resp_failed_continuation","status":"in_progress"}}` + "\n\n" + prelude
	stream += "event: " + eventType + "\ndata: " + payload + "\n\n"
	return &http.Response{StatusCode: http.StatusOK, Header: http.Header{"Content-Type": {"text/event-stream"}}, Body: io.NopCloser(strings.NewReader(stream))}
}

func messagesContinuationReplayBody(t *testing.T, stream bool) []byte {
	t.Helper()
	messages := []any{map[string]any{"role": "user", "content": "ORIGINAL_TASK_KEEP"}}
	for i := 0; i < 7; i++ {
		messages = append(messages,
			map[string]any{"role": "assistant", "content": fmt.Sprintf("earlier-answer-%d", i)},
			map[string]any{"role": "user", "content": fmt.Sprintf("earlier-question-%d", i)},
		)
	}
	messages = append(messages,
		map[string]any{"role": "assistant", "content": []any{map[string]any{"type": "tool_use", "id": "call_keep", "name": "lookup", "input": map[string]any{"key": "original"}}}},
		map[string]any{"role": "user", "content": []any{map[string]any{"type": "tool_result", "tool_use_id": "call_keep", "content": "TOOL_RESULT_KEEP"}}},
	)
	body, err := json.Marshal(map[string]any{
		"model": "gpt-6.1-sol", "max_tokens": 128, "stream": stream,
		"system": "SYSTEM_KEEP", "messages": messages, "stop_sequences": []string{"STOP_MARKER"},
		"tools": []any{map[string]any{"name": "lookup", "input_schema": map[string]any{"type": "object", "properties": map[string]any{"key": map[string]any{"type": "string"}}}}},
	})
	require.NoError(t, err)
	return body
}

func messagesContinuationSuccess() *http.Response {
	return &http.Response{StatusCode: http.StatusOK, Header: http.Header{"Content-Type": {"text/event-stream"}}, Body: io.NopCloser(strings.NewReader(
		`data: {"type":"response.created","response":{"id":"resp_recovered","model":"gpt-6.1-sol","status":"in_progress"}}` + "\n\n" +
			`data: {"type":"response.output_text.delta","output_index":0,"content_index":0,"delta":"okSTOP_MARKERhidden"}` + "\n\n" +
			`data: {"type":"response.completed","response":{"id":"resp_recovered","model":"gpt-6.1-sol","status":"completed","output":[{"type":"message","id":"msg_recovered","role":"assistant","status":"completed","content":[{"type":"output_text","text":"okSTOP_MARKERhidden"}]}],"usage":{"input_tokens":120,"output_tokens":7,"total_tokens":127,"input_tokens_details":{"cached_tokens":60,"cache_write_tokens":20}}}}` + "\n\n",
	))}
}

func TestForwardAsAnthropic_SSEContinuationRecoveryRestoresHistory(t *testing.T) {
	gin.SetMode(gin.TestMode)
	for _, stream := range []bool{false, true} {
		for _, async := range []bool{false, true} {
			for _, eventType := range []string{"response.failed", "error"} {
				t.Run(fmt.Sprintf("stream=%t/async=%t/%s", stream, async, eventType), func(t *testing.T) {
					body := messagesContinuationReplayBody(t, stream)
					first := messagesContinuationFailure(eventType, "invalid_request", messagesContinuationUnsupported, "")
					firstBody := &passthroughCloseTrackingReadCloser{Reader: first.Body}
					first.Body = firstBody
					upstream := &httpUpstreamRecorder{responses: []*http.Response{first, messagesContinuationSuccess(), messagesContinuationSuccess()}}
					cfg := &config.Config{}
					if async {
						cfg.Gateway.StreamDataIntervalTimeout = 5
					}
					svc := &OpenAIGatewayService{httpUpstream: upstream, cfg: cfg}
					account := rawGPT56ResponsesAPIKeyAccount("gpt-6.1-sol", "gpt-6.1-sol")
					svc.bindOpenAICompatSessionResponseID(context.Background(), nil, account, "continuation-session", "resp_old")
					svc.bindOpenAICompatSessionResponseID(context.Background(), nil, account, "other-session", "resp_other")
					for turn := 0; turn < 2; turn++ {
						rec := httptest.NewRecorder()
						c, _ := gin.CreateTestContext(rec)
						c.Request = httptest.NewRequest(http.MethodPost, "/v1/messages", bytes.NewReader(body))
						result, err := svc.ForwardAsAnthropic(context.Background(), c, account, body, "continuation-session", "gpt-6.1-sol")
						require.NoError(t, err)
						require.Equal(t, http.StatusOK, rec.Code)
						require.Equal(t, "resp_recovered", result.ResponseID)
						require.Equal(t, 120, result.Usage.InputTokens)
						require.Equal(t, 7, result.Usage.OutputTokens)
						require.Equal(t, 60, result.Usage.CacheReadInputTokens)
						require.Equal(t, 20, result.Usage.CacheCreationInputTokens)
						require.NotContains(t, rec.Body.String(), messagesContinuationUnsupported)
						require.NotContains(t, rec.Body.String(), "resp_failed_continuation")
						require.NotContains(t, rec.Body.String(), "hidden")
						require.Contains(t, rec.Body.String(), `"text":"ok"`)
						if stream {
							require.Equal(t, 1, strings.Count(rec.Body.String(), "event: message_start"))
							require.Equal(t, 1, strings.Count(rec.Body.String(), "event: message_stop"))
						}
						_, hasErrors := c.Get(OpsUpstreamErrorsKey)
						require.False(t, hasErrors, "a recovered capability error must not become a client failure")
					}
					require.True(t, firstBody.closed)
					require.True(t, svc.isOpenAICompatSessionContinuationDisabled(context.Background(), nil, account, "continuation-session"))
					require.Equal(t, "resp_other", svc.getOpenAICompatSessionResponseID(context.Background(), nil, account, "other-session"))
					require.Len(t, upstream.bodies, 3, "recover once, then keep continuation disabled for this session")
					require.Equal(t, "resp_old", gjson.GetBytes(upstream.bodies[0], "previous_response_id").String())
					require.NotContains(t, string(upstream.bodies[0]), "ORIGINAL_TASK_KEEP")
					for index, sent := range upstream.bodies[1:] {
						require.False(t, gjson.GetBytes(sent, "previous_response_id").Exists())
						require.Equal(t, "continuation-session", gjson.GetBytes(sent, "prompt_cache_key").String())
						require.Contains(t, string(sent), "ORIGINAL_TASK_KEEP")
						require.Contains(t, string(sent), "SYSTEM_KEEP")
						require.Contains(t, string(sent), "earlier-answer-0")
						require.Contains(t, string(sent), "TOOL_RESULT_KEEP")
						require.Contains(t, string(sent), `"call_id":"call_keep"`)
						require.Equal(t, upstream.requests[0].URL.String(), upstream.requests[index+1].URL.String())
						require.Equal(t, upstream.requests[0].Header.Get("Authorization"), upstream.requests[index+1].Header.Get("Authorization"))
					}
				})
			}
		}
	}
}

func TestOpenAIMessagesContinuationRecoveryHonorsExplicitStatus(t *testing.T) {
	for _, status := range []int{http.StatusUnauthorized, http.StatusForbidden, http.StatusTooManyRequests, http.StatusServiceUnavailable} {
		for _, path := range openAIStreamErrorStatusPaths {
			t.Run(fmt.Sprintf("%s/%d", path, status), func(t *testing.T) {
				rec := httptest.NewRecorder()
				c, _ := gin.CreateTestContext(rec)
				c.Request = httptest.NewRequest(http.MethodPost, "/v1/messages", nil)
				payload := map[string]any{"type": "response.failed", "response": map[string]any{"error": map[string]any{"code": "invalid_request", "message": messagesContinuationUnsupported}}}
				current := payload
				parts := strings.Split(path, ".")
				for _, part := range parts[:len(parts)-1] {
					child, ok := current[part].(map[string]any)
					if !ok {
						child = map[string]any{}
						current[part] = child
					}
					current = child
				}
				current[parts[len(parts)-1]] = status
				encoded, err := json.Marshal(payload)
				require.NoError(t, err)
				require.Nil(t, newOpenAIMessagesContinuationRetryError(c, encoded, "resp_old"))
			})
		}
	}
}

func TestForwardAsAnthropic_SSEContinuationRecoveryPreservesOpaqueHistory(t *testing.T) {
	for _, stream := range []bool{false, true} {
		t.Run(fmt.Sprintf("stream=%t", stream), func(t *testing.T) {
			upstream := &httpUpstreamRecorder{responses: []*http.Response{
				messagesHistoryUpstreamFixture(),
				messagesContinuationFailure("response.failed", "invalid_request", messagesContinuationUnsupported, ""),
				openAICompatSSECompletedResponse("resp_after_history_replay", "gpt-5.5"),
			}}
			svc := &OpenAIGatewayService{httpUpstream: upstream, cfg: &config.Config{}}
			account := rawGPT56ResponsesAPIKeyAccount("gpt-5.5", "gpt-5.5")
			messages := []any{map[string]any{"role": "user", "content": "ORIGINAL_TASK_KEEP"}}
			for turn := 0; turn < 2; turn++ {
				body, err := json.Marshal(map[string]any{"model": "gpt-5.5", "stream": stream, "max_tokens": 1024, "messages": messages,
					"tools": []any{map[string]any{"name": "todo_write", "input_schema": map[string]any{"type": "object"}}}})
				require.NoError(t, err)
				rec := httptest.NewRecorder()
				c, _ := gin.CreateTestContext(rec)
				c.Request = httptest.NewRequest(http.MethodPost, "/v1/messages", bytes.NewReader(body))
				c.Request.Header.Set(apicompat.ResponsesHistoryHeader, "v1")
				_, err = svc.ForwardAsAnthropic(context.Background(), c, account, body, "opaque-history-session", "gpt-5.5")
				require.NoError(t, err)
				if turn == 0 {
					blocks := collectMessagesHistoryResponse(t, rec, stream)
					messages = append(messages, map[string]any{"role": "assistant", "content": blocks},
						map[string]any{"role": "user", "content": []any{map[string]any{"type": "tool_result", "tool_use_id": "call_fixture", "content": "saved"}}})
				}
			}
			require.Len(t, upstream.bodies, 3)
			require.Equal(t, "resp_history_fixture", gjson.GetBytes(upstream.bodies[1], "previous_response_id").String())
			sent := string(upstream.bodies[2])
			require.False(t, gjson.Get(sent, "previous_response_id").Exists())
			for _, preserved := range []string{"ORIGINAL_TASK_KEEP", "gAAAA_fixture", "example.invalid/original-source", `"type":"web_search_call"`, `"type":"function_call"`, `"type":"function_call_output"`, `"call_id":"call_fixture"`} {
				require.Contains(t, sent, preserved)
			}
		})
	}
}

func TestForwardAsAnthropic_SSEContinuationRecoveryIsBounded(t *testing.T) {
	for _, stream := range []bool{false, true} {
		t.Run(fmt.Sprintf("stream=%t", stream), func(t *testing.T) {
			body := messagesContinuationReplayBody(t, stream)
			upstream := &httpUpstreamRecorder{responses: []*http.Response{
				messagesContinuationFailure("response.failed", "invalid_request", messagesContinuationUnsupported, ""),
				messagesContinuationFailure("response.failed", "invalid_request", messagesContinuationUnsupported, ""),
			}}
			svc := &OpenAIGatewayService{httpUpstream: upstream, cfg: &config.Config{}}
			account := rawGPT56ResponsesAPIKeyAccount("gpt-6.1-sol", "gpt-6.1-sol")
			svc.bindOpenAICompatSessionResponseID(context.Background(), nil, account, "bounded-session", "resp_old")
			rec := httptest.NewRecorder()
			c, _ := gin.CreateTestContext(rec)
			c.Request = httptest.NewRequest(http.MethodPost, "/v1/messages", bytes.NewReader(body))
			_, err := svc.ForwardAsAnthropic(context.Background(), c, account, body, "bounded-session", "gpt-6.1-sol")
			require.Error(t, err)
			require.Len(t, upstream.requests, 2)
			require.Contains(t, rec.Body.String(), messagesContinuationUnsupported)
			require.False(t, gjson.GetBytes(upstream.bodies[1], "previous_response_id").Exists())
		})
	}
}

func TestForwardAsAnthropic_SSEContinuationRecoveryRespectsBoundaries(t *testing.T) {
	for _, tc := range []struct {
		name, code, message, prelude string
		bind, cancel                 bool
	}{
		{name: "not-attached", code: "invalid_request", message: messagesContinuationUnsupported},
		{name: "canceled", code: "invalid_request", message: messagesContinuationUnsupported, bind: true, cancel: true},
		{name: "after-output", code: "invalid_request", message: messagesContinuationUnsupported, bind: true,
			prelude: `data: {"type":"response.output_text.delta","output_index":0,"content_index":0,"delta":"partial-visible"}` + "\n\n"},
		{name: "unrelated-invalid-request", code: "invalid_request", message: "Invalid tool schema", bind: true},
		{name: "policy-error", code: "content_policy_violation", message: "previous_response_id is not supported by this policy", bind: true},
		{name: "authentication-error", code: "invalid_api_key", message: messagesContinuationUnsupported, bind: true},
		{name: "rate-limit", code: "rate_limit_error", message: messagesContinuationUnsupported, bind: true},
	} {
		for _, stream := range []bool{false, true} {
			if tc.name == "after-output" && !stream {
				continue
			}
			t.Run(fmt.Sprintf("%s/stream=%t", tc.name, stream), func(t *testing.T) {
				body := messagesContinuationReplayBody(t, stream)
				upstream := &httpUpstreamRecorder{resp: messagesContinuationFailure("response.failed", tc.code, tc.message, tc.prelude)}
				svc := &OpenAIGatewayService{httpUpstream: upstream, cfg: &config.Config{}}
				account := rawGPT56ResponsesAPIKeyAccount("gpt-6.1-sol", "gpt-6.1-sol")
				if tc.bind {
					svc.bindOpenAICompatSessionResponseID(context.Background(), nil, account, "boundary-session", "resp_old")
				}
				rec := httptest.NewRecorder()
				c, _ := gin.CreateTestContext(rec)
				c.Request = httptest.NewRequest(http.MethodPost, "/v1/messages", bytes.NewReader(body))
				if tc.cancel {
					ctx, cancel := context.WithCancel(c.Request.Context())
					c.Request = c.Request.WithContext(ctx)
					cancel()
				}
				_, err := svc.ForwardAsAnthropic(c.Request.Context(), c, account, body, "boundary-session", "gpt-6.1-sol")
				require.Error(t, err)
				require.Len(t, upstream.requests, 1)
				if tc.name == "after-output" {
					require.Contains(t, rec.Body.String(), "partial-visible")
					require.Equal(t, 1, strings.Count(rec.Body.String(), "event: error"))
				}
			})
		}
	}
}

type messagesContinuationCloseHook struct {
	io.ReadCloser
	once    sync.Once
	onClose func()
}

func (r *messagesContinuationCloseHook) Close() error {
	r.once.Do(r.onClose)
	return r.ReadCloser.Close()
}

func TestForwardAsAnthropic_SSEContinuationRecoveryIgnoresConcurrentBinding(t *testing.T) {
	for _, stream := range []bool{false, true} {
		for _, eventType := range []string{"response.failed", "error"} {
			t.Run(fmt.Sprintf("stream=%t/%s", stream, eventType), func(t *testing.T) {
				body := messagesContinuationReplayBody(t, stream)
				first := messagesContinuationFailure(eventType, "previous_response_not_found", "Previous response resp_old not found", "")
				// Exercise the parser's final frame as well as blank-line framing.
				data, err := io.ReadAll(first.Body)
				require.NoError(t, err)
				first.Body = io.NopCloser(strings.NewReader(strings.TrimRight(string(data), "\n")))
				upstream := &httpUpstreamRecorder{responses: []*http.Response{first, messagesContinuationSuccess(), messagesContinuationSuccess()}}
				svc := &OpenAIGatewayService{httpUpstream: upstream, cfg: &config.Config{}}
				account := rawGPT56ResponsesAPIKeyAccount("gpt-6.1-sol", "gpt-6.1-sol")
				svc.bindOpenAICompatSessionResponseID(context.Background(), nil, account, "rebind-session", "resp_old")
				first.Body = &messagesContinuationCloseHook{ReadCloser: first.Body, onClose: func() {
					svc.bindOpenAICompatSessionResponseID(context.Background(), nil, account, "rebind-session", "resp_raced")
				}}
				for turn := 0; turn < 2; turn++ {
					c, _ := gin.CreateTestContext(httptest.NewRecorder())
					c.Request = httptest.NewRequest(http.MethodPost, "/v1/messages", bytes.NewReader(body))
					_, err := svc.ForwardAsAnthropic(context.Background(), c, account, body, "rebind-session", "gpt-6.1-sol")
					require.NoError(t, err)
				}
				require.Len(t, upstream.bodies, 3)
				require.False(t, gjson.GetBytes(upstream.bodies[1], "previous_response_id").Exists())
				require.Contains(t, string(upstream.bodies[1]), "ORIGINAL_TASK_KEEP")
				require.Equal(t, "resp_recovered", gjson.GetBytes(upstream.bodies[2], "previous_response_id").String())
				require.False(t, svc.isOpenAICompatSessionContinuationDisabled(context.Background(), nil, account, "rebind-session"))
			})
		}
	}
}
