package service

import (
	"context"
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

func configuredRetryEvent(payload string) string { return "data: " + payload + "\n\n" }

func configuredRetryOutput(kind string) string {
	switch kind {
	case "reasoning":
		return configuredRetryEvent(`{"type":"response.output_item.added","output_index":0,"item":{"id":"rs_private","type":"reasoning","summary":[]}}`) +
			configuredRetryEvent(`{"type":"response.reasoning_summary_text.delta","output_index":0,"summary_index":0,"delta":"private reasoning"}`)
	case "tool":
		return configuredRetryEvent(`{"type":"response.output_item.added","output_index":0,"item":{"id":"fc_private","type":"function_call","call_id":"call_private","name":"lookup","arguments":"{}"}}`) +
			configuredRetryEvent(`{"type":"response.function_call_arguments.done","output_index":0,"item_id":"fc_private","arguments":"{}"}`) +
			configuredRetryEvent(`{"type":"response.output_item.done","output_index":0,"item":{"id":"fc_private","type":"function_call","call_id":"call_private","name":"lookup","arguments":"{}","status":"completed"}}`)
	default:
		return configuredRetryEvent(`{"type":"response.output_item.added","output_index":0,"item":{"id":"msg_private","type":"message","role":"assistant","content":[]}}`) +
			configuredRetryEvent(`{"type":"response.output_text.delta","output_index":0,"content_index":0,"delta":"private answer"}`)
	}
}

func runConfiguredRetryStream(svc *OpenAIGatewayService, c *gin.Context, resp *http.Response, account *Account, path string) error {
	if path == "chat" {
		_, err := svc.handleChatStreamingResponse(resp, c, account, "gpt-6-sol", "gpt-6-sol", "gpt-6-luna", time.Now(), 32)
		return err
	}
	return runOpenAIOpaquePreambleStream(svc, c, resp, account, path)
}

func TestOpenAIConfiguredStreamRetryBuffersOutputUntilTerminal(t *testing.T) {
	gin.SetMode(gin.TestMode)
	for _, path := range []string{"native", "passthrough", "messages", "chat"} {
		for _, kind := range []string{"reasoning", "tool", "text"} {
			for _, success := range []bool{false, true} {
				t.Run(fmt.Sprintf("%s/%s/success=%t", path, kind, success), func(t *testing.T) {
					c, rec := newOpenAIConfiguredRetryContext()
					body := configuredRetryEvent(`{"type":"response.created","response":{"id":"resp_attempt","status":"in_progress"}}`) + configuredRetryOutput(kind)
					if success {
						body += configuredRetryEvent(`{"type":"response.completed","response":{"id":"resp_attempt","status":"completed","output":[],"usage":{"input_tokens":1,"output_tokens":1}}}`)
					} else {
						body += configuredRetryEvent(messagesBPSFailure)
					}
					resp := &http.Response{StatusCode: http.StatusOK, Header: http.Header{}, Body: io.NopCloser(strings.NewReader(body))}
					err := runConfiguredRetryStream(&OpenAIGatewayService{cfg: &config.Config{}}, c, resp, newConfiguredStreamRetryAccount(), path)
					if success {
						require.NoError(t, err)
						require.True(t, c.Writer.Written())
						if kind != "reasoning" || path != "chat" {
							require.Contains(t, rec.Body.String(), map[string]string{"reasoning": "private reasoning", "tool": "lookup", "text": "private answer"}[kind])
						}
					} else {
						var failover *UpstreamFailoverError
						require.ErrorAs(t, err, &failover)
						require.True(t, failover.RetryableOnSameAccount)
						require.Equal(t, http.StatusBadGateway, failover.StatusCode)
						require.False(t, c.Writer.Written(), "failed reasoning, tool calls and answers must never escape staging")
						require.Empty(t, rec.Body.String())
					}
				})
			}
		}
	}
}

func newConfiguredStreamRetryAccount() *Account {
	return &Account{ID: 59328, Platform: PlatformOpenAI, Type: AccountTypeAPIKey, Credentials: map[string]any{
		"pool_mode": true, "pool_mode_retry_count": float64(10),
		"pool_mode_retry_status_codes": []any{float64(http.StatusBadGateway)},
	}}
}

func newOpenAIConfiguredRetryContext() (*gin.Context, *httptest.ResponseRecorder) {
	rec := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(rec)
	c.Request = httptest.NewRequest(http.MethodPost, "/v1/responses", nil)
	return c, rec
}

func TestOpenAIConfiguredStreamRetryPreservesUnconfiguredDelivery(t *testing.T) {
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
					account.Credentials["pool_mode_retry_status_codes"] = []any{float64(503)}
				}
				c, rec := newOpenAIConfiguredRetryContext()
				body := configuredRetryOutput("text") + configuredRetryEvent(messagesBPSFailure)
				resp := &http.Response{StatusCode: 200, Header: http.Header{}, Body: io.NopCloser(strings.NewReader(body))}
				err := runConfiguredRetryStream(&OpenAIGatewayService{cfg: &config.Config{}}, c, resp, account, path)
				var failover *UpstreamFailoverError
				require.Error(t, err)
				require.NotErrorAs(t, err, &failover)
				require.Contains(t, rec.Body.String(), "private answer")
			})
		}
	}
}

func TestOpenAIConfiguredStreamRetryKeepsProgressWhileBuffered(t *testing.T) {
	for _, path := range []string{"native", "passthrough", "messages", "chat"} {
		t.Run(path, func(t *testing.T) {
			c, rec := newOpenAIConfiguredRetryContext()
			budget := &openAIHTTPBudget{progress: make(chan struct{})}
			c.Request = c.Request.WithContext(context.WithValue(c.Request.Context(), openAIHTTPBudgetKey{}, budget))
			reader, writer := io.Pipe()
			defer reader.Close()
			progress := make(chan bool, 1)
			go func() {
				defer writer.Close()
				_, _ = io.WriteString(writer, configuredRetryOutput("reasoning"))
				select {
				case <-budget.progress:
					progress <- true
				case <-time.After(time.Second):
					progress <- false
				}
				_, _ = io.WriteString(writer, configuredRetryEvent(messagesBPSFailure))
			}()
			resp := &http.Response{StatusCode: 200, Header: http.Header{}, Body: reader}
			err := runConfiguredRetryStream(&OpenAIGatewayService{cfg: &config.Config{}}, c, resp, newConfiguredStreamRetryAccount(), path)
			var failover *UpstreamFailoverError
			require.ErrorAs(t, err, &failover)
			require.True(t, <-progress, "buffering must not keep the upload/pre-output budget running after upstream progress")
			require.Empty(t, rec.Body.String())
		})
	}
}

func TestOpenAIConfiguredStreamRetryStagingRemainsBounded(t *testing.T) {
	for _, path := range []string{"native", "passthrough", "messages", "chat"} {
		t.Run(path, func(t *testing.T) {
			c, rec := newOpenAIConfiguredRetryContext()
			line := configuredRetryEvent(fmt.Sprintf("{\"type\":\"response.output_text.delta\",\"output_index\":0,\"delta\":%q}", strings.Repeat("x", openAIFirstOutputStageMaxBytes/2)))
			resp := &http.Response{StatusCode: 200, Header: http.Header{}, Body: io.NopCloser(strings.NewReader(line + line + line))}
			err := runConfiguredRetryStream(&OpenAIGatewayService{cfg: &config.Config{}}, c, resp, newConfiguredStreamRetryAccount(), path)
			var failover *UpstreamFailoverError
			require.ErrorAs(t, err, &failover)
			require.True(t, failover.RetryableOnSameAccount)
			require.Contains(t, string(failover.ResponseBody), "staging")
			require.Empty(t, rec.Body.String(), "a staging limit must fail closed without leaking tool or answer bytes")
		})
	}
}

func TestOpenAIConfiguredStreamRetryPartialCommitDoesNotReplay(t *testing.T) {
	for _, path := range []string{"native", "passthrough", "messages", "chat"} {
		t.Run(path, func(t *testing.T) {
			rec := &openAIOpaquePartialWriteRecorder{ResponseRecorder: httptest.NewRecorder()}
			c, _ := gin.CreateTestContext(rec)
			c.Request = httptest.NewRequest(http.MethodPost, "/v1/responses", nil)
			body := configuredRetryOutput("text") + configuredRetryEvent("{\"type\":\"response.completed\",\"response\":{\"status\":\"completed\",\"output\":[],\"usage\":{\"input_tokens\":1,\"output_tokens\":1}}}")
			resp := &http.Response{StatusCode: 200, Header: http.Header{}, Body: io.NopCloser(strings.NewReader(body))}
			err := runConfiguredRetryStream(&OpenAIGatewayService{cfg: &config.Config{}}, c, resp, newConfiguredStreamRetryAccount(), path)
			var failover *UpstreamFailoverError
			require.NotErrorAs(t, err, &failover)
			require.Positive(t, c.Writer.Size())
		})
	}
}

type configuredRetryHeartbeatWriter struct {
	gin.ResponseWriter
	beat chan<- struct{}
}

func (w *configuredRetryHeartbeatWriter) Write(p []byte) (int, error) {
	n, err := w.ResponseWriter.Write(p)
	if err == nil && (strings.HasPrefix(string(p), ":") || strings.Contains(string(p), "event: ping")) {
		select {
		case w.beat <- struct{}{}:
		default:
		}
	}
	return n, err
}

func TestOpenAIConfiguredStreamRetryKeepsHeartbeatsReplaySafe(t *testing.T) {
	for _, path := range []string{"native", "passthrough", "messages", "chat"} {
		t.Run(path, func(t *testing.T) {
			c, rec := newOpenAIConfiguredRetryContext()
			beat := make(chan struct{}, 1)
			c.Writer = &configuredRetryHeartbeatWriter{ResponseWriter: c.Writer, beat: beat}
			reader, writer := io.Pipe()
			defer reader.Close()
			heartbeatSeen := make(chan bool, 1)
			go func() {
				defer writer.Close()
				_, _ = io.WriteString(writer, configuredRetryOutput("reasoning"))
				ticker := time.NewTicker(50 * time.Millisecond)
				defer ticker.Stop()
				deadline := time.NewTimer(4 * time.Second)
				defer deadline.Stop()
				seen := false
			loop:
				for {
					select {
					case <-beat:
						seen = true
						break loop
					case <-deadline.C:
						break loop
					case <-ticker.C:
						if _, err := io.WriteString(writer, configuredRetryOutput("reasoning")); err != nil {
							heartbeatSeen <- false
							return
						}
					}
				}
				heartbeatSeen <- seen
				_, _ = io.WriteString(writer, configuredRetryEvent(messagesBPSFailure))
			}()
			resp := &http.Response{StatusCode: 200, Header: http.Header{}, Body: reader}
			cfg := &config.Config{Gateway: config.GatewayConfig{StreamKeepaliveInterval: 1}}
			err := runConfiguredRetryStream(&OpenAIGatewayService{cfg: cfg}, c, resp, newConfiguredStreamRetryAccount(), path)
			var failover *UpstreamFailoverError
			require.ErrorAs(t, err, &failover)
			require.True(t, <-heartbeatSeen, "buffered attempts must keep the downstream connection alive")
			require.NotContains(t, rec.Body.String(), "private reasoning")
			require.Negative(t, OpenAICompactKeepaliveAdjustedWrittenSize(c))
		})
	}
}

type canceledConfiguredStream struct{ *strings.Reader }

func (r canceledConfiguredStream) Read(p []byte) (int, error) {
	n, err := r.Reader.Read(p)
	if err == io.EOF {
		return n, context.Canceled
	}
	return n, err
}

func (canceledConfiguredStream) Close() error { return nil }

func TestOpenAIConfiguredStreamRetryDoesNotReplayCancellation(t *testing.T) {
	for _, path := range []string{"native", "passthrough", "messages", "chat"} {
		t.Run(path, func(t *testing.T) {
			c, rec := newOpenAIConfiguredRetryContext()
			ctx, cancel := context.WithCancel(c.Request.Context())
			c.Request = c.Request.WithContext(ctx)
			cancel()
			body := canceledConfiguredStream{strings.NewReader(configuredRetryOutput("reasoning"))}
			resp := &http.Response{StatusCode: 200, Header: http.Header{}, Body: body}
			err := runConfiguredRetryStream(&OpenAIGatewayService{cfg: &config.Config{}}, c, resp, newConfiguredStreamRetryAccount(), path)
			require.ErrorIs(t, err, context.Canceled)
			var failover *UpstreamFailoverError
			require.NotErrorAs(t, err, &failover)
			require.Empty(t, rec.Body.String())
		})
	}
}

func TestOpenAIConfiguredStreamRetryDiscardsToolsOnNonRetryableFailure(t *testing.T) {
	for _, path := range []string{"native", "passthrough", "messages", "chat"} {
		t.Run(path, func(t *testing.T) {
			c, rec := newOpenAIConfiguredRetryContext()
			failure := configuredRetryEvent(`{"type":"response.failed","response":{"id":"resp_rejected","status":"failed","error":{"code":"context_length_exceeded","type":"invalid_request_error","message":"maximum context length exceeded"}}}`)
			resp := &http.Response{StatusCode: 200, Header: http.Header{}, Body: io.NopCloser(strings.NewReader(configuredRetryOutput("tool") + failure))}
			err := runConfiguredRetryStream(&OpenAIGatewayService{cfg: &config.Config{}}, c, resp, newConfiguredStreamRetryAccount(), path)
			var failover *UpstreamFailoverError
			require.Error(t, err)
			require.NotErrorAs(t, err, &failover)
			require.NotContains(t, rec.Body.String(), "call_private")
			require.NotContains(t, rec.Body.String(), "lookup")
		})
	}
}

func TestOpenAIConfiguredStreamRetryPassthroughBareErrorDoesNotLeakTools(t *testing.T) {
	for _, completed := range []bool{false, true} {
		t.Run(fmt.Sprint(completed), func(t *testing.T) {
			c, rec := newOpenAIConfiguredRetryContext()
			body := configuredRetryOutput("tool") + configuredRetryEvent(`{"type":"error","error":{"code":"transient","message":"retrying"}}`)
			if completed {
				body += configuredRetryEvent(`{"type":"response.completed","response":{"id":"resp_recovered","status":"completed","output":[],"usage":{"input_tokens":1,"output_tokens":1}}}`)
			}
			resp := &http.Response{StatusCode: 200, Header: http.Header{}, Body: io.NopCloser(strings.NewReader(body))}
			err := runConfiguredRetryStream(&OpenAIGatewayService{cfg: &config.Config{}}, c, resp, newConfiguredStreamRetryAccount(), "passthrough")
			require.Error(t, err)
			require.NotContains(t, rec.Body.String(), "call_private")
			require.Contains(t, rec.Body.String(), "response.failed")
		})
	}
}
