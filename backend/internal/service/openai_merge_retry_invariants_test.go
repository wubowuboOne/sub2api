package service

import (
	"context"
	"fmt"
	"io"
	"net/http"
	"strings"
	"testing"
	"testing/synctest"
	"time"

	"github.com/Wei-Shaw/sub2api/internal/config"
	"github.com/Wei-Shaw/sub2api/internal/pkg/requesttiming"
	"github.com/gin-gonic/gin"
	"github.com/stretchr/testify/require"
)

func TestOpenAIMergedRetryHeartbeatsKeepAttemptsPrivate(t *testing.T) {
	gin.SetMode(gin.TestMode)
	for _, path := range []string{"native", "passthrough"} {
		for _, output := range []string{"reasoning", "tool", "text", "opaque_reasoning", "opaque_compaction", "open_event"} {
			t.Run(path+"/"+output, func(t *testing.T) {
				synctest.Test(t, func(t *testing.T) {
					start := time.Now()
					collector := requesttiming.New(start, 0)
					c, rec := newOpenAIConfiguredRetryContext()
					c.Request = c.Request.WithContext(requesttiming.With(context.Background(), collector))
					c.Request, _ = requesttiming.StartAttempt(c.Request, 1, 0)
					preamble := configuredRetryOutput(output)
					if strings.HasPrefix(output, "opaque_") {
						kind := strings.TrimPrefix(output, "opaque_")
						preamble = configuredRetryEvent(fmt.Sprintf(`{"type":"response.output_item.done","item":{"id":"private_item","type":%q,"encrypted_content":"private_opaque","summary":[]}}`, kind))
					}
					if output == "open_event" {
						preamble = "event: response.output_text.delta\n" +
							"data: {\"type\":\"response.output_text.delta\",\"delta\":\"private_open_event\"}\n"
					}
					reader, writer := io.Pipe()
					done := make(chan struct{})
					defer func() { _ = reader.Close(); <-done }()
					go func() {
						defer close(done)
						defer writer.Close()
						if _, err := io.WriteString(writer, preamble); err != nil {
							return
						}
						time.Sleep(2500 * time.Millisecond)
						if output == "open_event" {
							_, _ = io.WriteString(writer, "\n")
						}
						_, _ = io.WriteString(writer, configuredRetryEvent(messagesBPSFailure))
					}()
					resp := &http.Response{
						StatusCode: http.StatusOK,
						Header:     http.Header{"Content-Type": {"text/event-stream"}, "X-Sub2api-Plugin-Preflight-Sse": {"1"}},
						Request:    c.Request,
						Body:       reader,
					}
					markOpenAIPluginSSEPreflight(resp, c.Request, &PluginInstallation{PluginKey: openAIWSSSEAcceleratorPluginKey, SignatureStatus: PluginSignatureTrusted})
					svc := &OpenAIGatewayService{cfg: &config.Config{Gateway: config.GatewayConfig{
						OpenAIPluginSSEEarlyEventsEnabled: true, StreamKeepaliveInterval: 1,
					}}}
					err := runConfiguredRetryStream(svc, c, resp, newConfiguredStreamRetryAccount(), path)
					var failover *UpstreamFailoverError
					require.ErrorAs(t, err, &failover)
					require.True(t, failover.RetryableOnSameAccount)
					require.Equal(t, http.StatusBadGateway, failover.StatusCode)
					require.GreaterOrEqual(t, strings.Count(rec.Body.String(), ":"), 2)
					require.NotContains(t, rec.Body.String(), "private")
					require.NotContains(t, rec.Body.String(), "data:")
					require.NotContains(t, rec.Body.String(), "event:")
					require.Negative(t, OpenAICompactKeepaliveAdjustedWrittenSize(c))
					collector.Finish(http.StatusOK, false)
					collector.WhenFinished(func(s requesttiming.Snapshot) {
						require.NotContains(t, s.Events, "first_sse_event_flush", "comments must not become a delivered data event")
					})
				})
			})
		}
	}
}

func TestOpenAIMergedRetryHeartbeatDisconnectDrainsUsage(t *testing.T) {
	gin.SetMode(gin.TestMode)
	for _, path := range []string{"native", "passthrough"} {
		for _, failure := range []string{"write", "flush"} {
			t.Run(path+"/"+failure, func(t *testing.T) {
				synctest.Test(t, func(t *testing.T) {
					start := time.Now()
					c, rec := newOpenAIConfiguredRetryContext()
					if failure == "write" {
						c.Writer = &passthroughFlushTestWriter{ResponseWriter: c.Writer, recorder: rec, failAfterWrites: 0}
					} else {
						c.Writer = &headerWaitFlushSignalWriter{
							ResponseWriter: c.Writer, flushed: make(chan struct{}, 8), err: io.ErrClosedPipe,
						}
					}
					restoreWriter := WrapOpenAIResponsesHeaderWait(c, true)
					defer restoreWriter()
					reader, writer := io.Pipe()
					done := make(chan struct{})
					defer func() { _ = reader.Close(); <-done }()
					go func() {
						defer close(done)
						defer writer.Close()
						if _, err := io.WriteString(writer, configuredRetryOutput("reasoning")); err != nil {
							return
						}
						time.Sleep(2500 * time.Millisecond)
						_, _ = io.WriteString(writer, configuredRetryEvent(`{"type":"response.completed","response":{"id":"resp_disconnected","status":"completed","output":[],"usage":{"input_tokens":7,"output_tokens":3,"input_tokens_details":{"cached_tokens":6}}}}`))
					}()
					resp := &http.Response{StatusCode: http.StatusOK, Header: http.Header{}, Body: reader}
					svc := &OpenAIGatewayService{cfg: &config.Config{Gateway: config.GatewayConfig{StreamKeepaliveInterval: 1}}}
					account := newConfiguredStreamRetryAccount()
					var usage *OpenAIUsage
					var disconnected bool
					var err error
					if path == "native" {
						var result *openaiStreamingResult
						result, err = svc.handleStreamingResponse(c.Request.Context(), resp, c, account, start, "gpt-6-sol", "gpt-6-sol")
						require.NotNil(t, result)
						usage, disconnected = result.usage, result.clientDisconnect
					} else {
						var result *openaiStreamingResultPassthrough
						result, err = svc.handleStreamingResponsePassthrough(c.Request.Context(), resp, c, account, start, "gpt-6-sol", "gpt-6-sol")
						require.NotNil(t, result)
						usage, disconnected = result.usage, result.clientDisconnect
					}
					require.NoError(t, err)
					require.True(t, disconnected)
					require.True(t, OpenAIHeaderWaitDisconnected(c), "the handler must see the failed transport before another attempt")
					require.NoError(t, c.Request.Context().Err(), "disconnect detection must not depend on context cancellation")
					require.NotNil(t, usage)
					require.Equal(t, 7, usage.InputTokens)
					require.Equal(t, 3, usage.OutputTokens)
					require.Equal(t, 6, usage.CacheReadInputTokens)
					require.NotContains(t, rec.Body.String(), "private")
					require.NotContains(t, rec.Body.String(), "data:")
					require.False(t, OpsStreamCompletedForAccount(c, account.ID))
				})
			})
		}
	}
}
