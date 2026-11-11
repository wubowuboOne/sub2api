package service

import (
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/Wei-Shaw/sub2api/internal/config"
	"github.com/Wei-Shaw/sub2api/internal/pkg/apicompat"
	"github.com/Wei-Shaw/sub2api/internal/pkg/ctxkey"
	"github.com/gin-gonic/gin"
	"github.com/stretchr/testify/require"
	"github.com/tidwall/gjson"
)

func forwardClientModelBridge(s *OpenAIGatewayService, c *gin.Context, resp *http.Response, account *Account, path string) (*OpenAIForwardResult, error) {
	const original, billing, upstream = "channel-mapped-model", "billing-model", "account-mapped-model"
	start := time.Now()
	switch path {
	case "responses_chat_json":
		return s.bufferChatCompletionsAsResponses(c, resp, account, original, nil, nil, false, nil, billing, upstream, nil, nil, true, start, false, nil)
	case "responses_chat_stream":
		return s.streamChatCompletionsAsResponses(c, resp, account, original, nil, nil, false, nil, billing, upstream, nil, nil, true, start, nil)
	case "messages_chat_json":
		return s.bufferChatCompletionsAsAnthropic(c, resp, account, original, billing, upstream, nil, nil, start)
	case "messages_chat_stream":
		return s.streamChatCompletionsAsAnthropic(c, resp, account, original, billing, upstream, nil, nil, start)
	case "responses_anthropic_json":
		return s.handleResponsesBufferedFromNativeAnthropic(resp, c, original, billing, upstream, nil, start, apicompat.ResponsesClientToolMapping{})
	case "responses_anthropic_stream":
		return s.handleResponsesStreamingFromNativeAnthropic(resp, c, original, billing, upstream, nil, start, apicompat.ResponsesClientToolMapping{})
	case "chat_anthropic_json":
		return s.handleCCBufferedFromNativeAnthropic(resp, c, original, billing, upstream, nil, start)
	case "chat_anthropic_stream":
		return s.handleCCStreamingFromNativeAnthropic(resp, c, original, billing, upstream, nil, start)
	case "messages_anthropic_json":
		return s.handleNativeAnthropicBufferedResponse(c.Request.Context(), resp, c, account, original, billing, upstream, nil, start)
	case "messages_anthropic_stream":
		return s.handleNativeAnthropicStreamingResponse(c.Request.Context(), resp, c, account, original, billing, upstream, nil, start)
	default:
		panic("unknown client model bridge test path: " + path)
	}
}

func clientModelBridgeBody(path string, zeroUsage bool) string {
	body := `{"id":"chatcmpl_model","object":"chat.completion","model":"private-reported-model","choices":[{"index":0,"message":{"role":"assistant","content":"keep private-reported-model"},"finish_reason":"stop"}],"usage":{"prompt_tokens":10,"completion_tokens":5}}`
	if strings.HasSuffix(path, "_stream") {
		body = "data: " + `{"id":"chatcmpl_model","model":"private-reported-model","choices":[{"index":0,"delta":{"role":"assistant","content":"keep private-reported-model"},"finish_reason":null}]}` + "\n\ndata: " + `{"id":"chatcmpl_model","model":"private-reported-model","choices":[{"index":0,"delta":{},"finish_reason":"stop"}],"usage":{"prompt_tokens":10,"completion_tokens":5}}` + "\n\ndata: [DONE]\n\n"
	}
	if strings.Contains(path, "_anthropic_") {
		body = strings.ReplaceAll(miniAnthropicSSEStream(), "glm-4.7", "private-reported-model")
		body = strings.ReplaceAll(body, "Hello", "keep private-reported-model")
		if path == "messages_anthropic_json" {
			body = `{"id":"msg_model","type":"message","role":"assistant","model":"private-reported-model","content":[{"type":"text","text":"keep private-reported-model"}],"stop_reason":"end_turn","usage":{"input_tokens":10,"output_tokens":5}}`
		}
	}
	if zeroUsage {
		body = strings.NewReplacer(`"prompt_tokens":10`, `"prompt_tokens":0`, `"completion_tokens":5`, `"completion_tokens":0`, `"input_tokens":10`, `"input_tokens":0`, `"output_tokens":1`, `"output_tokens":0`, `"output_tokens":5`, `"output_tokens":0`).Replace(body)
	}
	return body
}

func TestOpenAIClientResponseModelProtocolBridges(t *testing.T) {
	for _, path := range []string{
		"responses_chat_json", "responses_chat_stream", "messages_chat_json", "messages_chat_stream",
		"responses_anthropic_json", "responses_anthropic_stream", "chat_anthropic_json", "chat_anthropic_stream",
		"messages_anthropic_json", "messages_anthropic_stream",
	} {
		for _, zeroUsage := range []bool{false, true} {
			for _, ingress := range []struct{ name, opsModel, publicModel string }{
				{name: "fallback"},
				{name: "ops_model", opsModel: "public-request-model"},
				{name: "public_model", opsModel: "channel-mapped-model", publicModel: "public-request-model"},
			} {
				name := path + "/" + ingress.name
				if zeroUsage {
					name += "/zero_usage"
				}
				t.Run(name, func(t *testing.T) {
					c, rec := newOpenAIUpstreamErrorTestContext(t)
					clientModel := "channel-mapped-model"
					if ingress.opsModel != "" {
						c.Request = c.Request.WithContext(context.WithValue(c.Request.Context(), ctxkey.Model, ingress.opsModel))
						clientModel = ingress.opsModel
					}
					if ingress.publicModel != "" {
						c.Request = c.Request.WithContext(context.WithValue(c.Request.Context(), ctxkey.RequestedPublicModel, ingress.publicModel))
						clientModel = ingress.publicModel
					}
					body := clientModelBridgeBody(path, zeroUsage)
					resp := newOpenAIUpstreamErrorResponse(http.StatusOK, body)
					if strings.Contains(body, "data:") {
						resp.Header.Set("Content-Type", "text/event-stream")
					}
					svc := &OpenAIGatewayService{cfg: &config.Config{}}
					account := &Account{ID: 1, Platform: PlatformOpenAI, Type: AccountTypeAPIKey}
					beginUpstreamResponseModelObservation(c)
					result, err := forwardClientModelBridge(svc, c, resp, account, path)
					require.NoError(t, err)
					require.NotNil(t, result)
					require.Equal(t, "channel-mapped-model", result.Model)
					require.Equal(t, "billing-model", result.BillingModel)
					require.Equal(t, "account-mapped-model", result.UpstreamModel)
					require.Equal(t, "private-reported-model", result.UpstreamResponseModel)
					wantInput, wantOutput := 10, 5
					if zeroUsage {
						wantInput, wantOutput = 0, 0
					}
					require.Equal(t, wantInput, result.Usage.InputTokens)
					require.Equal(t, wantOutput, result.Usage.OutputTokens)
					require.Contains(t, rec.Body.String(), "keep private-reported-model", "assistant text must remain intact")
					payloads := []string{rec.Body.String()}
					if strings.Contains(rec.Header().Get("Content-Type"), "text/event-stream") {
						payloads = nil
						for _, line := range strings.Split(rec.Body.String(), "\n") {
							if data, ok := extractOpenAISSEDataLine(line); ok && gjson.Valid(data) {
								payloads = append(payloads, data)
							}
						}
					}
					declarations := 0
					for _, payload := range payloads {
						require.True(t, gjson.Valid(payload))
						for _, field := range []string{"model", "response.model", "message.model"} {
							if model := gjson.Get(payload, field); model.Exists() {
								declarations++
								require.Equal(t, clientModel, model.String(), "wrong client model in %s", field)
							}
						}
					}
					require.Positive(t, declarations)
				})
			}
		}
	}
}

func TestOpenAIClientResponseModelForwardWithAPIKeyMapping(t *testing.T) {
	for _, passthrough := range []bool{false, true} {
		for _, stream := range []bool{false, true} {
			name := "native"
			if passthrough {
				name = "passthrough"
			}
			if stream {
				name += "/stream"
			} else {
				name += "/json"
			}
			t.Run(name, func(t *testing.T) {
				c, rec := newOpenAIUpstreamErrorTestContext(t)
				ctx := context.WithValue(c.Request.Context(), ctxkey.RequestedPublicModel, "public-request-model")
				ctx = context.WithValue(ctx, ctxkey.Model, "key-mapped-model")
				c.Request = c.Request.WithContext(ctx)
				body := `{"id":"resp_model","object":"response","status":"completed","model":"private-reported-model","output":[],"usage":{"input_tokens":0,"output_tokens":0}}`
				if stream {
					body = "data: " + `{"type":"response.completed","response":` + body + "}\n\n"
				}
				resp := newOpenAIUpstreamErrorResponse(http.StatusOK, body)
				if stream {
					resp.Header.Set("Content-Type", "text/event-stream")
				}
				upstream := &httpUpstreamRecorder{resp: resp}
				svc := &OpenAIGatewayService{cfg: &config.Config{}, httpUpstream: upstream}
				account := &Account{
					ID: 1, Platform: PlatformOpenAI, Type: AccountTypeAPIKey, Status: StatusActive, Schedulable: true,
					Credentials: map[string]any{
						"api_key": "sk-test", "base_url": "https://api.example.test",
						"model_mapping": map[string]any{"channel-mapped-model": "account-mapped-model"},
					},
					Extra: map[string]any{"openai_passthrough": passthrough, "openai_responses_supported": true},
				}
				request := []byte(`{"model":"channel-mapped-model","input":"hello","stream":false}`)
				if stream {
					request = []byte(strings.Replace(string(request), "false", "true", 1))
				}
				result, err := svc.Forward(ctx, c, account, request)
				require.NoError(t, err)
				require.NotNil(t, result)
				require.Len(t, upstream.bodies, 1)
				wantUpstream := "account-mapped-model"
				if passthrough {
					wantUpstream = "channel-mapped-model"
				}
				require.Equal(t, wantUpstream, gjson.GetBytes(upstream.lastBody, "model").String())
				require.Equal(t, "channel-mapped-model", result.Model)
				require.Equal(t, "private-reported-model", result.UpstreamResponseModel)
				require.Zero(t, result.Usage.InputTokens)
				require.Zero(t, result.Usage.OutputTokens)
				require.Contains(t, rec.Body.String(), `"model":"public-request-model"`)
				for _, private := range []string{"private-reported-model", "account-mapped-model", "channel-mapped-model", "key-mapped-model"} {
					require.NotContains(t, rec.Body.String(), private)
				}
			})
		}
	}
}

func TestOpenAIClientResponseModelSpecialTransports(t *testing.T) {
	for _, transport := range []string{"bps", "prism"} {
		for _, stream := range []bool{false, true} {
			name := transport + "/json"
			if stream {
				name = transport + "/stream"
			}
			t.Run(name, func(t *testing.T) {
				c, rec := newOpenAIUpstreamErrorTestContext(t)
				ctx := context.WithValue(c.Request.Context(), ctxkey.RequestedPublicModel, "public-request-model")
				ctx = context.WithValue(ctx, ctxkey.Model, "gpt-5.6-sol")
				c.Request = c.Request.WithContext(ctx)
				body := []byte(`{"model":"gpt-5.6-sol","input":"hi","stream":false}`)
				if stream {
					body = []byte(strings.Replace(string(body), "false", "true", 1))
				}
				terminal := `{"id":"resp_identity","status":"completed","model":"gpt-5.6-sol","output":[{"type":"message","role":"assistant","content":[{"type":"output_text","text":"21"}]}],"usage":{"input_tokens":10,"output_tokens":2}}`
				wire := "event: response.completed\ndata: " + `{"type":"response.completed","response":` + terminal + "}\n\n"
				var result *OpenAIForwardResult
				var err error
				if transport == "bps" {
					resp := newOpenAIUpstreamErrorResponse(http.StatusOK, wire)
					resp.Header.Set("Content-Type", "text/event-stream")
					upstream := &httpUpstreamRecorder{resp: resp}
					svc := openAIClientToolsTestService(upstream)
					result, err = svc.Forward(ctx, c, excelAccount(), body)
					require.Len(t, upstream.bodies, 1)
					require.NoError(t, err)
					require.Equal(t, "gpt-5.6-sol", result.UpstreamResponseModel)
					require.Equal(t, 10, result.Usage.InputTokens)
					require.Equal(t, 2, result.Usage.OutputTokens)
				} else {
					terminal = strings.Replace(terminal, `{"input_tokens":10,"output_tokens":2}`, "null", 1)
					var hits atomic.Int32
					server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
						hits.Add(1)
						if stream {
							_, _ = io.WriteString(w, "event: response.completed\ndata: "+`{"type":"response.completed","response":`+terminal+"}\n\n")
						} else {
							_, _ = io.WriteString(w, terminal)
						}
					}))
					t.Cleanup(server.Close)
					svc, account := prismTestService(server.URL)
					result, err = svc.forwardPrismBrowser(ctx, c, account, body, time.Now())
					require.NoError(t, err)
					require.Equal(t, int32(1), hits.Load())
					require.True(t, result.UsageUnavailable)
				}
				require.Equal(t, "gpt-5.6-sol", result.Model)
				require.Contains(t, rec.Body.String(), `"model":"public-request-model"`)
				require.NotContains(t, rec.Body.String(), "gpt-5.6-sol")
			})
		}
	}
}

func TestOpenAIClientResponseModelProtocolFieldScope(t *testing.T) {
	svc := &OpenAIGatewayService{}
	for _, tc := range []struct{ name, body, want string }{
		{
			name: "anthropic_start",
			body: `{"type":"message_start","message":{"model":"private-model","content":[{"text":"private-model"}]},"tool":{"model":"private-model"}}`,
			want: `{"type":"message_start","message":{"model":"public-model","content":[{"text":"private-model"}]},"tool":{"model":"private-model"}}`,
		},
		{
			name: "ordinary_nested_message",
			body: `{"model":"private-model","message":{"model":"private-model"},"choices":[{"message":{"content":"private-model"}}]}`,
			want: `{"model":"public-model","message":{"model":"private-model"},"choices":[{"message":{"content":"private-model"}}]}`,
		},
		{
			name: "malformed_event",
			body: `{"type":"message_start","message":{"model":"private-model"}`,
			want: `{"type":"message_start","message":{"model":"private-model"}`,
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			require.Equal(t, tc.want, string(rewriteOpenAIClientModel([]byte(tc.body), "public-model")))
			require.Equal(t, "data: "+tc.want, svc.replaceModelInSSELine("data: "+tc.body, "account-model", "public-model"))
		})
	}
}

func TestOpenAIClientResponseModelPassthroughJSONPreservesContentAndUsage(t *testing.T) {
	c, rec := newOpenAIUpstreamErrorTestContext(t)
	c.Request = c.Request.WithContext(context.WithValue(c.Request.Context(), ctxkey.Model, "public-request-model"))
	body := `{"model":"private-reported-model","output":[{"type":"message","content":[{"type":"output_text","text":"data: private-reported-model\nevent: message_start"}]}],"usage":{"input_tokens":20,"output_tokens":0,"input_tokens_details":{"cached_tokens":11}}}`
	resp := newOpenAIUpstreamErrorResponse(http.StatusOK, body)
	svc := &OpenAIGatewayService{cfg: &config.Config{}}
	account := &Account{ID: 1, Platform: PlatformOpenAI, Type: AccountTypeAPIKey}
	result, err := svc.handleNonStreamingResponsePassthrough(c.Request.Context(), resp, c, account, "channel-mapped-model", "account-mapped-model")
	require.NoError(t, err)
	require.Equal(t, strings.Replace(body, `"model":"private-reported-model"`, `"model":"public-request-model"`, 1), rec.Body.String())
	require.Equal(t, "application/json", rec.Header().Get("Content-Type"))
	require.Equal(t, 20, result.usage.InputTokens)
	require.Equal(t, 11, result.usage.CacheReadInputTokens)
	require.Zero(t, result.usage.OutputTokens)
	require.Equal(t, "private-reported-model", observedUpstreamResponseModel(c))
}

func TestOpenAIClientResponseModelPassthroughSSEWithoutContentType(t *testing.T) {
	for _, contentType := range []string{"", "application/json", "text/plain"} {
		t.Run(contentType, func(t *testing.T) {
			c, rec := newOpenAIUpstreamErrorTestContext(t)
			c.Request = c.Request.WithContext(context.WithValue(c.Request.Context(), ctxkey.Model, "public-request-model"))
			body := "data: " + `{"type":"response.completed","response":{"id":"resp_model","object":"response","status":"completed","model":"private-reported-model","output":[],"usage":{"input_tokens":0,"output_tokens":0}}}` + "\n\ndata: [DONE]\n\n"
			resp := &http.Response{StatusCode: http.StatusOK, Header: http.Header{}, Body: io.NopCloser(strings.NewReader(body))}
			if contentType != "" {
				resp.Header.Set("Content-Type", contentType)
			}
			svc := &OpenAIGatewayService{cfg: &config.Config{}}
			account := &Account{ID: 1, Platform: PlatformOpenAI, Type: AccountTypeAPIKey}
			result, err := svc.handleNonStreamingResponsePassthrough(c.Request.Context(), resp, c, account, "channel-mapped-model", "account-mapped-model")
			require.NoError(t, err)
			require.NotNil(t, result)
			require.True(t, gjson.Valid(rec.Body.String()), "a synchronous request must receive JSON")
			require.Equal(t, "public-request-model", gjson.Get(rec.Body.String(), "model").String())
			require.NotContains(t, rec.Body.String(), "private-reported-model")
			require.Equal(t, "private-reported-model", observedUpstreamResponseModel(c))
			require.Zero(t, result.usage.InputTokens)
			require.Zero(t, result.usage.OutputTokens)
		})
	}
}

func TestOpenAIClientResponseModelJSONWithEventStreamContentType(t *testing.T) {
	for _, accountType := range []string{AccountTypeAPIKey, AccountTypeOAuth} {
		for _, passthrough := range []bool{false, true} {
			for _, filterHeaders := range []bool{false, true} {
				for _, zeroUsage := range []bool{false, true} {
					name := accountType
					if passthrough {
						name += "/passthrough"
					} else {
						name += "/native"
					}
					if filterHeaders {
						name += "/filtered_headers"
					}
					if zeroUsage {
						name += "/zero_usage"
					}
					t.Run(name, func(t *testing.T) {
						c, rec := newOpenAIUpstreamErrorTestContext(t)
						ctx := context.WithValue(c.Request.Context(), ctxkey.RequestedPublicModel, "public-request-model")
						ctx = context.WithValue(ctx, ctxkey.Model, "key-mapped-model")
						c.Request = c.Request.WithContext(ctx)
						body := `{"id":"resp_mislabeled_json","object":"response","status":"completed","model":"private-reported-model","output":[{"type":"message","content":[{"type":"output_text","text":"data: private-reported-model\nevent: response.completed"}]},{"type":"function_call","name":"inspect","arguments":"{\"model\":\"private-reported-model\"}"}],"usage":{"input_tokens":18469,"output_tokens":5,"input_tokens_details":{"cached_tokens":12000}}}`
						if zeroUsage {
							body = strings.NewReplacer(`"input_tokens":18469`, `"input_tokens":0`, `"output_tokens":5`, `"output_tokens":0`, `"cached_tokens":12000`, `"cached_tokens":0`).Replace(body)
						}
						resp := newOpenAIUpstreamErrorResponse(http.StatusOK, "\r\n"+body+"\r\n")
						resp.Header.Set("Content-Type", "text/event-stream; charset=utf-8")
						cfg := &config.Config{}
						cfg.Security.ResponseHeaders.Enabled = filterHeaders
						svc := &OpenAIGatewayService{cfg: cfg, responseHeaderFilter: compileResponseHeaderFilter(cfg)}
						account := &Account{ID: 1, Platform: PlatformOpenAI, Type: accountType}
						var usage *OpenAIUsage
						var responseID string
						if passthrough {
							result, err := svc.handleNonStreamingResponsePassthrough(ctx, resp, c, account, "channel-mapped-model", "account-mapped-model")
							require.NoError(t, err)
							require.NotNil(t, result)
							usage, responseID = result.usage, result.responseID
						} else {
							result, err := svc.handleNonStreamingResponse(ctx, resp, c, account, "channel-mapped-model", "account-mapped-model")
							require.NoError(t, err)
							require.NotNil(t, result)
							usage, responseID = result.usage, result.responseID
						}
						require.Equal(t, http.StatusOK, rec.Code)
						require.True(t, strings.HasPrefix(rec.Header().Get("Content-Type"), "application/json"), rec.Header().Get("Content-Type"))
						require.True(t, gjson.Valid(rec.Body.String()))
						require.Equal(t, "public-request-model", gjson.Get(rec.Body.String(), "model").String())
						require.Equal(t, "resp_mislabeled_json", responseID)
						require.Equal(t, "private-reported-model", observedUpstreamResponseModel(c))
						for _, path := range []string{"output", "usage"} {
							require.JSONEq(t, gjson.Get(body, path).Raw, gjson.Get(rec.Body.String(), path).Raw)
						}
						require.Equal(t, int(gjson.Get(body, "usage.input_tokens").Int()), usage.InputTokens)
						require.Equal(t, int(gjson.Get(body, "usage.output_tokens").Int()), usage.OutputTokens)
						require.Equal(t, int(gjson.Get(body, "usage.input_tokens_details.cached_tokens").Int()), usage.CacheReadInputTokens)
					})
				}
			}
		}
	}
}

func TestOpenAIClientResponseModelForwardJSONWithEventStreamContentType(t *testing.T) {
	for _, accountType := range []string{AccountTypeAPIKey, AccountTypeOAuth} {
		for _, passthrough := range []bool{false, true} {
			name := accountType + "/native"
			if passthrough {
				name = accountType + "/passthrough"
			}
			t.Run(name, func(t *testing.T) {
				body := `{"id":"resp_7978","object":"response","status":"completed","model":"private-reported-model","output":[{"type":"message","role":"assistant","content":[{"type":"output_text","text":"OK"}]}],"usage":{"input_tokens":37,"output_tokens":9,"input_tokens_details":{"cached_tokens":11}}}`
				header := http.Header{"Content-Type": []string{"text/event-stream"}, "X-Request-Id": []string{"rid_mislabeled_json"}}
				rec, upstream, result := runOpenAINonStreamRegressionForAccount(t, accountType, passthrough,
					`{"model":"gpt-5.4","input":"Reply with exactly OK.","stream":false}`, header, body)
				requireSingleJSONResponsesObject(t, rec)
				require.Len(t, upstream.bodies, 1)
				require.False(t, result.Stream)
				require.Equal(t, "gpt-5.4", gjson.Get(rec.Body.String(), "model").String())
				require.Equal(t, "gpt-5.4", result.Model)
				require.Equal(t, "private-reported-model", result.UpstreamResponseModel)
				require.Equal(t, "resp_7978", result.ResponseID)
				require.Equal(t, 37, result.Usage.InputTokens)
				require.Equal(t, 9, result.Usage.OutputTokens)
				require.Equal(t, 11, result.Usage.CacheReadInputTokens)
			})
		}
	}
}
