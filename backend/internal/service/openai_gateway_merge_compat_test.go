//go:build unit

package service

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"
	"testing"

	"github.com/Wei-Shaw/sub2api/internal/config"
	"github.com/Wei-Shaw/sub2api/internal/pkg/apicompat"
	"github.com/Wei-Shaw/sub2api/internal/pkg/ctxkey"
	"github.com/Wei-Shaw/sub2api/internal/pkg/groupprompt"
	"github.com/stretchr/testify/require"
	"github.com/tidwall/gjson"
	"github.com/tidwall/sjson"
)

// The opt-in 400 recovery added upstream must preserve prompts selected before
// model mapping, including the Messages developer-message representation.
func TestGroupPrompt400RecoveryPreservesMappedInstructions(t *testing.T) {
	const publicModel, upstreamModel = "public-merge-model", "gpt-5.1"
	for _, format := range []string{"responses", "messages"} {
		for _, mode := range []string{"group_default", "model_override", "bundled_override"} {
			for _, after409 := range []bool{false, true} {
				t.Run(fmt.Sprintf("%s/%s/after409=%t", format, mode, after409), func(t *testing.T) {
					policy := groupprompt.Config{Enabled: true, Content: "GROUP-DEFAULT", ModelPrompts: map[string]string{upstreamModel: "WRONG-UPSTREAM-PROMPT"}}
					if mode == "model_override" {
						policy.ModelPrompts[publicModel] = "PUBLIC-MODEL-PROMPT"
					}
					if mode == "bundled_override" {
						policy.ModelPrompts[publicModel] = ""
					}
					want := policy.Prompt(publicModel)
					body, err := sjson.SetBytes(history400Body(history400Input, false), "model", publicModel)
					require.NoError(t, err)
					body, err = sjson.SetBytes(body, "instructions", "CLIENT-INSTRUCTIONS")
					require.NoError(t, err)
					if format == "responses" {
						body, err = groupprompt.Prepend(body, format, want)
						require.NoError(t, err)
					} else {
						inbound, err := groupprompt.Prepend([]byte(`{"model":"public-merge-model","max_tokens":16,"system":"CLIENT-SYSTEM","messages":[{"role":"user","content":"hello"}]}`), format, want)
						require.NoError(t, err)
						var anthropic apicompat.AnthropicRequest
						require.NoError(t, json.Unmarshal(inbound, &anthropic))
						converted, err := apicompat.AnthropicToResponses(&anthropic)
						require.NoError(t, err)
						var input []json.RawMessage
						require.NoError(t, json.Unmarshal([]byte(gjson.GetBytes(body, "input").Raw), &input))
						developer := json.RawMessage(gjson.GetBytes(converted.Input, "0").Raw)
						input = append([]json.RawMessage{developer}, input...)
						body, err = sjson.SetBytes(body, "input", input)
						require.NoError(t, err)
					}
					canonical := append([]byte(nil), body...)
					response := func(status int, payload string) *http.Response {
						return &http.Response{StatusCode: status, Header: http.Header{"Content-Type": []string{"application/json"}}, Body: io.NopCloser(strings.NewReader(payload))}
					}
					upstream := &httpUpstreamSequenceRecorder{responses: []*http.Response{
						response(400, history400Failure),
						response(200, `{"id":"resp_merge","status":"completed","model":"gpt-5.1","output":[],"usage":{"input_tokens":1,"output_tokens":1}}`),
					}}
					svc := newStreamLineageHTTPService(upstream)
					enableHistory400(svc, true)
					account := newStreamLineageHTTPAccount()
					account.Credentials["model_mapping"] = map[string]any{publicModel: upstreamModel}
					// A preserved-history route must use only the new explicit
					// recovery budget, even when ordinary pool 400 retries exist.
					if after409 {
						account.Credentials["pool_mode"] = true
						account.Credentials["pool_mode_retry_count"] = float64(10)
						account.Credentials["pool_mode_retry_status_codes"] = []any{float64(400)}
					}
					c, rec := newStreamLineageTestContext("merge-prompt-400")
					c.Set("api_key", &APIKey{ID: 7, AllowLongContext: true})
					ctx := context.WithValue(context.Background(), ctxkey.RequestedPublicModel, publicModel)
					ctx = WithOpenAIForwardModel(ctx, upstreamModel, false)
					ctx = svc.WithOpenAIAgentMessageRecovery(ctx, c, body, "merge-prompt-400", upstreamModel)
					if after409 {
						r := openAIAgentRecoveryFromContext(ctx)
						r.omittedHistory, r.inputDigest = true, openAIAgentRecoveryInputDigest(body)
					}
					c.Request = c.Request.WithContext(ctx)
					_, err = svc.Forward(ctx, c, account, body)
					require.NoError(t, err, rec.Body.String())
					require.Equal(t, 2, upstream.callCount)
					require.Equal(t, canonical, body, "the caller's canonical request must remain unchanged")
					diagnostic := openAI400HistoryDiagnostic(c)
					require.NotNil(t, diagnostic)
					require.True(t, diagnostic.Applied)
					for _, sent := range upstream.bodies {
						require.Equal(t, upstreamModel, gjson.GetBytes(sent, "model").String())
						require.NotContains(t, string(sent), "WRONG-UPSTREAM-PROMPT")
						if format == "responses" {
							require.Equal(t, want+"\n\nCLIENT-INSTRUCTIONS", gjson.GetBytes(sent, "instructions").String())
						} else {
							require.Equal(t, "CLIENT-INSTRUCTIONS", gjson.GetBytes(sent, "instructions").String())
							require.Equal(t, want+"\n\nCLIENT-SYSTEM", gjson.GetBytes(sent, "input.0.content.0.text").String())
						}
					}
					require.Contains(t, string(upstream.bodies[1]), openAIHistoricalCiphertextOmitted)
					require.Equal(t, publicModel, gjson.GetBytes(rec.Body.Bytes(), "model").String())
				})
			}
		}
	}
}

// Configured 502 buffering must discard partial content on a peer reset and
// retain the upstream no-replay transport classification at both reader loops.
func TestOpenAIConfiguredStreamRetryHTTP2ResetDoesNotReplay(t *testing.T) {
	for _, path := range []string{"native", "passthrough", "messages", "chat"} {
		for _, keepalive := range []int{0, 1} {
			t.Run(fmt.Sprintf("%s/keepalive=%d", path, keepalive), func(t *testing.T) {
				c, rec := newOpenAIConfiguredRetryContext()
				if path == "messages" {
					c.Request.URL.Path = "/v1/messages"
				}
				if path == "chat" {
					c.Request.URL.Path = "/v1/chat/completions"
				}
				svc := &OpenAIGatewayService{cfg: &config.Config{Gateway: config.GatewayConfig{StreamKeepaliveInterval: keepalive}}}
				resp := http2PeerResetResponse(t, "text/event-stream", configuredRetryOutput("text"))
				err := runConfiguredRetryStream(svc, c, resp, newConfiguredStreamRetryAccount(), path)
				require.Error(t, err)
				var retry *UpstreamFailoverError
				require.NotErrorAs(t, err, &retry)
				code, _, ok := OpenAIUpstreamStreamReadErrorDetails(err)
				require.True(t, ok)
				require.Equal(t, OpenAIUpstreamHTTP2StreamErrorCode, code)
				require.NotContains(t, rec.Body.String(), "private answer")
				require.NotContains(t, rec.Body.String(), "message_stop")
				require.NotContains(t, rec.Body.String(), "response.completed")
				ops, marked := GetOpsStreamError(c)
				require.True(t, marked)
				require.NotNil(t, ops.TransportFailure)
			})
		}
	}
}
