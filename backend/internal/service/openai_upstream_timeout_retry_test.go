package service

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"os"
	"strings"
	"testing"
	"testing/synctest"
	"time"

	"github.com/Wei-Shaw/sub2api/internal/config"
	"github.com/gin-gonic/gin"
	"github.com/stretchr/testify/require"
)

const upstreamTimeoutFailure = `{"response":{"created_at":1794136949,"error":{"code":"upstream_timeout","message":"Upstream request failed, please try again later","type":"bps_stream_error"},"id":"resp_08c1357e8978d769016af057f1ca2487d1bc4d1f8e6df6c44c","model":"gpt-6-luna","object":"response","status":"failed"},"type":"response.failed"}`

func TestOpenAIUpstreamTimeoutConfiguredRetry(t *testing.T) {
	payload := []byte(upstreamTimeoutFailure)
	message := extractOpenAISSEErrorMessage(payload)
	require.Equal(t, http.StatusBadGateway, openAIStreamFailedEventSemanticStatus(payload, message))
	require.True(t, openAIStreamFailedEventShouldFailover(payload, message))
	require.True(t, openAIStreamFailedEventRetryableOnSameAccount(newConfiguredStreamRetryAccount(), payload, message))
	for _, path := range []string{"native", "passthrough", "messages", "chat"} {
		for _, prelude := range []string{"metadata", "reasoning", "tool", "text"} {
			t.Run(path+"/"+prelude, func(t *testing.T) {
				c, rec := newOpenAIConfiguredRetryContext()
				body := configuredRetryEvent(`{"type":"response.created","response":{"id":"resp_timeout","status":"in_progress"}}`)
				if prelude != "metadata" {
					body += configuredRetryOutput(prelude)
				}
				body += configuredRetryEvent(upstreamTimeoutFailure)
				resp := &http.Response{StatusCode: http.StatusOK, Header: http.Header{}, Body: io.NopCloser(strings.NewReader(body))}
				err := runConfiguredRetryStream(&OpenAIGatewayService{cfg: &config.Config{}}, c, resp, newConfiguredStreamRetryAccount(), path)
				var failover *UpstreamFailoverError
				require.ErrorAs(t, err, &failover)
				require.Equal(t, http.StatusBadGateway, failover.StatusCode)
				require.True(t, failover.RetryableOnSameAccount)
				require.False(t, c.Writer.Written())
				require.Empty(t, rec.Body.String())
			})
		}
	}
}

func TestOpenAIChatConfiguredRetryReadFailures(t *testing.T) {
	for _, failure := range []struct {
		name string
		err  error
	}{
		{"unexpected_eof", io.ErrUnexpectedEOF},
		{"truncated", io.EOF},
		{"http2_reset", errors.New("stream error: stream ID 1; PROTOCOL_ERROR; received from peer")},
		{"read_timeout", &net.OpError{Op: "read", Net: "tcp", Err: os.ErrDeadlineExceeded}},
	} {
		for _, asynchronous := range []bool{false, true} {
			for _, output := range []string{"metadata", "reasoning", "tool", "text"} {
				t.Run(fmt.Sprintf("%s/async=%t/%s", failure.name, asynchronous, output), func(t *testing.T) {
					c, rec := newOpenAIConfiguredRetryContext()
					cfg := &config.Config{}
					if asynchronous {
						cfg.Gateway.StreamKeepaliveInterval = 1
					}
					payload := configuredRetryEvent(`{"type":"response.created","response":{"id":"resp_read_failure"}}`)
					if output != "metadata" {
						payload += configuredRetryOutput(output)
					}
					resp := &http.Response{StatusCode: http.StatusOK,
						Header: http.Header{"X-Request-Id": []string{"chat-read-retry"}},
						Body:   &openAIChatStreamReadErrorCloser{payload: []byte(payload), err: failure.err}}
					result, err := (&OpenAIGatewayService{cfg: cfg}).handleChatStreamingResponse(resp, c, newConfiguredStreamRetryAccount(), "gpt-5.6-terra", "gpt-5.6-terra", "gpt-6-luna", time.Now(), 32)
					var failover *UpstreamFailoverError
					require.ErrorAs(t, err, &failover)
					require.Nil(t, result, "a discarded attempt must not produce a billable result")
					require.True(t, failover.RetryableOnSameAccount)
					require.True(t, failover.ConfiguredStreamRetry)
					require.Equal(t, http.StatusBadGateway, failover.StatusCode)
					require.Equal(t, "chat-read-retry", failover.ResponseHeaders.Get("x-request-id"))
					require.Empty(t, rec.Body.String())
				})
			}
		}
	}
}

func TestOpenAIChatConfiguredRetryReadFailureGuards(t *testing.T) {
	for _, asynchronous := range []bool{false, true} {
		for _, guard := range []string{"non_pool", "zero_retries", "502_not_configured", "client_canceled", "client_deadline", "upstream_canceled", "upstream_phase_deadline", "response_committed", "output_started", "read_canceled", "read_deadline", "response_too_large"} {
			t.Run(fmt.Sprintf("async=%t/%s", asynchronous, guard), func(t *testing.T) {
				c, rec := newOpenAIConfiguredRetryContext()
				account := newConfiguredStreamRetryAccount()
				cfg := &config.Config{}
				if asynchronous {
					cfg.Gateway.StreamKeepaliveInterval = 1
				}
				var readErr error = io.ErrUnexpectedEOF
				resp := &http.Response{StatusCode: http.StatusOK, Header: http.Header{}}
				switch guard {
				case "non_pool":
					account.Credentials["pool_mode"] = false
				case "zero_retries":
					account.Credentials["pool_mode_retry_count"] = float64(0)
				case "502_not_configured":
					account.Credentials["pool_mode_retry_status_codes"] = []any{float64(http.StatusServiceUnavailable)}
				case "client_canceled":
					ctx, cancel := context.WithCancel(c.Request.Context())
					cancel()
					c.Request = c.Request.WithContext(ctx)
				case "client_deadline":
					ctx, cancel := context.WithDeadline(c.Request.Context(), time.Now().Add(-time.Second))
					defer cancel()
					c.Request = c.Request.WithContext(ctx)
				case "upstream_canceled", "upstream_phase_deadline":
					ctx, cancel := context.WithCancelCause(context.Background())
					if guard == "upstream_phase_deadline" {
						cancel(&openAIHTTPPhaseTimeout{phase: "pre_output_budget"})
					} else {
						cancel(context.Canceled)
					}
					resp.Request = c.Request.WithContext(ctx)
				case "response_committed":
					MarkResponseCommitted(c)
				case "output_started":
					_, err := c.Writer.WriteString("existing client output")
					require.NoError(t, err)
				case "read_canceled":
					readErr = context.Canceled
				case "read_deadline":
					readErr = context.DeadlineExceeded
				case "response_too_large":
					readErr = ErrUpstreamResponseBodyTooLarge
				}
				resp.Body = &openAIChatStreamReadErrorCloser{payload: []byte(configuredRetryOutput("text")), err: readErr}
				result, err := (&OpenAIGatewayService{cfg: cfg}).handleChatStreamingResponse(resp, c, account, "gpt-5.6-terra", "gpt-5.6-terra", "gpt-6-luna", time.Now(), 32)
				var failover *UpstreamFailoverError
				require.Error(t, err)
				require.NotErrorAs(t, err, &failover)
				require.NotNil(t, result, "non-replayed streams retain existing partial-result behavior")
				switch guard {
				case "non_pool", "zero_retries", "502_not_configured":
					require.Contains(t, rec.Body.String(), "private answer")
				case "output_started":
					require.Equal(t, "existing client output", rec.Body.String())
				default:
					require.Empty(t, rec.Body.String())
				}
			})
		}
	}
}

func TestOpenAIChatConfiguredRetryIdleTimeout(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		c, rec := newOpenAIConfiguredRetryContext()
		reader, writer := io.Pipe()
		defer reader.Close()
		defer writer.Close()
		go func() {
			_, _ = io.WriteString(writer, configuredRetryEvent(`{"type":"response.created","response":{"id":"resp_idle"}}`))
		}()
		cfg := &config.Config{}
		cfg.Gateway.StreamDataIntervalTimeout = 1
		resp := &http.Response{StatusCode: http.StatusOK, Header: http.Header{}, Body: reader}
		result, err := (&OpenAIGatewayService{cfg: cfg}).handleChatStreamingResponse(resp, c, newConfiguredStreamRetryAccount(), "gpt-5.6-terra", "gpt-5.6-terra", "gpt-6-luna", time.Now(), 32)
		var failover *UpstreamFailoverError
		require.ErrorAs(t, err, &failover)
		require.True(t, failover.RetryableOnSameAccount)
		require.Nil(t, result)
		require.Empty(t, rec.Body.String())
	})
}

func TestOpenAIUpstreamTimeoutAfterLongWaitStillRetries(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		c, rec := newOpenAIConfiguredRetryContext()
		reader, writer := io.Pipe()
		defer reader.Close()
		go func() {
			defer writer.Close()
			time.Sleep(15 * time.Minute)
			_, _ = io.WriteString(writer, configuredRetryEvent(upstreamTimeoutFailure))
		}()
		cfg := &config.Config{}
		cfg.Gateway.StreamKeepaliveInterval = 10
		resp := &http.Response{StatusCode: http.StatusOK, Header: http.Header{}, Body: reader}
		_, err := (&OpenAIGatewayService{cfg: cfg}).handleChatStreamingResponse(resp, c, newConfiguredStreamRetryAccount(), "gpt-5.6-terra", "gpt-5.6-terra", "gpt-6-luna", time.Now(), 32)
		var failover *UpstreamFailoverError
		require.ErrorAs(t, err, &failover)
		require.True(t, failover.RetryableOnSameAccount)
		require.Equal(t, http.StatusBadGateway, failover.StatusCode)
		require.NotEmpty(t, rec.Body.String(), "a long wait still sends heartbeats")
		require.Empty(t, strings.ReplaceAll(rec.Body.String(), ":\n\n", ""))
		require.Negative(t, OpenAICompactKeepaliveAdjustedWrittenSize(c))
	})
}

func TestOpenAIChatConfiguredRetryReadFailureAfterDisconnectDoesNotReplay(t *testing.T) {
	for _, readErr := range []error{io.ErrUnexpectedEOF, io.EOF} {
		t.Run(readErr.Error(), func(t *testing.T) {
			synctest.Test(t, func(t *testing.T) {
				c, rec := newOpenAIConfiguredRetryContext()
				failed := make(chan struct{}, 1)
				c.Writer = &upstreamTimeoutDisconnectedWriter{ResponseWriter: c.Writer, failed: failed}
				reader, writer := io.Pipe()
				defer reader.Close()
				go func() {
					_, _ = io.WriteString(writer, configuredRetryOutput("tool"))
					<-failed
					_ = writer.CloseWithError(readErr)
				}()
				cfg := &config.Config{}
				cfg.Gateway.StreamKeepaliveInterval = 1
				resp := &http.Response{StatusCode: http.StatusOK, Header: http.Header{}, Body: reader}
				result, err := (&OpenAIGatewayService{cfg: cfg}).handleChatStreamingResponse(resp, c, newConfiguredStreamRetryAccount(), "gpt-5.6-terra", "gpt-5.6-terra", "gpt-6-luna", time.Now(), 32)
				var failover *UpstreamFailoverError
				require.Error(t, err)
				require.NotErrorAs(t, err, &failover)
				require.NotNil(t, result)
				require.True(t, result.ClientDisconnect)
				require.Empty(t, rec.Body.String())
			})
		})
	}
}

type upstreamTimeoutDisconnectedWriter struct {
	gin.ResponseWriter
	failed chan struct{}
}

func (w *upstreamTimeoutDisconnectedWriter) Write([]byte) (int, error) {
	select {
	case w.failed <- struct{}{}:
	default:
	}
	return 0, io.ErrClosedPipe
}

func (w *upstreamTimeoutDisconnectedWriter) WriteString(text string) (int, error) {
	return w.Write([]byte(text))
}

func TestOpenAIUpstreamTimeoutDisconnectedChatDoesNotReplay(t *testing.T) {
	c, rec := newOpenAIConfiguredRetryContext()
	ctx, cancel := context.WithTimeout(c.Request.Context(), 5*time.Second)
	defer cancel()
	c.Request = c.Request.WithContext(ctx)
	failed := make(chan struct{}, 1)
	c.Writer = &upstreamTimeoutDisconnectedWriter{ResponseWriter: c.Writer, failed: failed}
	reader, writer := io.Pipe()
	defer reader.Close()
	go func() {
		defer writer.Close()
		select {
		case <-failed:
			_, _ = io.WriteString(writer, configuredRetryEvent(upstreamTimeoutFailure))
		case <-ctx.Done():
			_ = writer.CloseWithError(ctx.Err())
		}
	}()
	cfg := &config.Config{}
	cfg.Gateway.StreamKeepaliveInterval = 1
	svc := &OpenAIGatewayService{cfg: cfg}
	resp := &http.Response{StatusCode: http.StatusOK, Header: http.Header{"X-Request-Id": []string{"upstream-timeout-disconnected"}}, Body: reader}
	result, err := svc.handleChatStreamingResponse(resp, c, newConfiguredStreamRetryAccount(), "gpt-5.6-terra", "gpt-5.6-terra", "gpt-6-luna", time.Now(), 32)
	require.Error(t, err)
	var failover *UpstreamFailoverError
	require.NotErrorAs(t, err, &failover)
	require.NotNil(t, result)
	require.True(t, result.ClientDisconnect)
	require.Empty(t, rec.Body.String())
	events, ok := c.Get(OpsUpstreamErrorsKey)
	require.True(t, ok)
	eventList, ok := events.([]*OpsUpstreamErrorEvent)
	require.True(t, ok)
	require.Len(t, eventList, 1)
	require.Equal(t, "http_error", eventList[0].Kind)
}
