//go:build unit

package service

import (
	"context"
	"errors"
	"io"
	"net/http"
	"net/url"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
)

func TestHandleOpenAIUpstreamTransportError_ConfiguredPoolRetry(t *testing.T) {
	eof := &url.Error{Op: "Post", URL: "https://vip.qimux.com/v1/responses", Err: io.ErrUnexpectedEOF}
	h2 := &url.Error{Op: "Post", URL: "https://vip.qimux.com/v1/responses", Err: errors.New("stream error: stream ID 1; PROTOCOL_ERROR; received from peer")}
	for _, tt := range []struct {
		name       string
		err        error
		pool       bool
		codes      []any
		wantRetry  bool
		persistent bool
		peerReset  bool
	}{
		{name: "unexpected_eof", err: eof, pool: true, codes: []any{float64(502)}, wantRetry: true},
		{name: "http2_protocol_error", err: h2, pool: true, codes: []any{float64(502)}, peerReset: true},
		{name: "connection_reset", err: errors.New("connection reset by peer"), pool: true, codes: []any{float64(502)}, wantRetry: true},
		{name: "unknown_transport_error", err: errors.New("unexpected transport failure"), pool: true, codes: []any{float64(502)}, wantRetry: true},
		{name: "upstream_timeout", err: context.DeadlineExceeded, pool: true, codes: []any{float64(502)}, wantRetry: true},
		{name: "non_pool", err: eof, codes: []any{float64(502)}},
		{name: "default_statuses", err: eof, pool: true},
		{name: "empty_statuses", err: eof, pool: true, codes: []any{}},
		{name: "502_not_configured", err: eof, pool: true, codes: []any{float64(503)}},
		{name: "expired_proxy", err: errors.New("username/password authentication failed"), pool: true, codes: []any{float64(502)}, persistent: true},
		{name: "dns_not_found", err: errors.New("no such host"), pool: true, codes: []any{float64(502)}, persistent: true},
	} {
		t.Run(tt.name, func(t *testing.T) {
			for _, passthrough := range []bool{false, true} {
				repo := &openaiTransportAccountRepoStub{}
				svc := &OpenAIGatewayService{accountRepo: repo}
				account := &Account{
					ID: 59328, Platform: PlatformOpenAI, Type: AccountTypeAPIKey,
					Credentials: map[string]any{
						"pool_mode": tt.pool, "pool_mode_retry_count": float64(10),
					},
				}
				if tt.codes != nil {
					account.Credentials["pool_mode_retry_status_codes"] = tt.codes
				}
				c, rec := newOpenAITransportErrTestContext()
				err := svc.handleOpenAIUpstreamTransportError(context.Background(), c, account, tt.err, passthrough)
				var failover *UpstreamFailoverError
				if tt.peerReset {
					require.NotErrorAs(t, err, &failover)
					code, _, classified := OpenAIUpstreamStreamReadErrorDetails(err)
					require.True(t, classified)
					require.Equal(t, OpenAIUpstreamHTTP2StreamErrorCode, code)
					require.Equal(t, http.StatusBadGateway, rec.Code)
					require.Contains(t, rec.Body.String(), "Do not automatically retry")
					require.Empty(t, repo.tempUnschedCalls)
					require.False(t, svc.isOpenAIAccountRuntimeBlocked(account))
					ops, marked := GetOpsStreamError(c)
					require.True(t, marked)
					require.NotNil(t, ops.TransportFailure)
					continue
				}
				require.ErrorAs(t, err, &failover)
				require.Equal(t, http.StatusBadGateway, failover.StatusCode)
				require.Equal(t, tt.wantRetry, failover.RetryableOnSameAccount)
				require.Equal(t, tt.persistent, svc.isOpenAIAccountRuntimeBlocked(account))
				if tt.persistent {
					require.Len(t, repo.tempUnschedCalls, 1)
				} else {
					require.Empty(t, repo.tempUnschedCalls)
				}
				require.False(t, c.Writer.Written())
				require.Empty(t, rec.Body.String())
				value, ok := c.Get(OpsUpstreamErrorsKey)
				require.True(t, ok)
				events := value.([]*OpsUpstreamErrorEvent)
				require.Len(t, events, 1)
				require.Equal(t, "request_error", events[0].Kind)
				require.Zero(t, events[0].UpstreamStatusCode, "no HTTP response was received")
				require.Equal(t, passthrough, events[0].Passthrough)
				if tt.wantRetry {
					_, _, eligible := classifyOpenAIAPIKeyHealthFailure(err)
					require.False(t, eligible, "configured retries must not trip account health blocking")
				}
			}
		})
	}
}

func TestHandleOpenAIUpstreamTransportError_PoolRetryPreservesTerminalGuards(t *testing.T) {
	for _, name := range []string{"canceled", "request_deadline", "plugin_already_sent", "ticket_rejected"} {
		t.Run(name, func(t *testing.T) {
			ctx := context.Background()
			var original error
			switch name {
			case "canceled":
				original = &url.Error{Op: "Post", Err: context.Canceled}
			case "request_deadline":
				var cancel context.CancelFunc
				ctx, cancel = context.WithDeadline(ctx, time.Now().Add(-time.Second))
				defer cancel()
				original = &url.Error{Op: "Post", Err: context.DeadlineExceeded}
			case "plugin_already_sent":
				original = &PluginTransportError{Message: "unexpected EOF", RequestSent: true}
			case "ticket_rejected":
				original = ErrCodexTicketResponseRejected
			}
			repo := &openaiTransportAccountRepoStub{}
			svc := &OpenAIGatewayService{accountRepo: repo}
			account := &Account{ID: 59328, Platform: PlatformOpenAI, Type: AccountTypeAPIKey, Credentials: map[string]any{
				"pool_mode": true, "pool_mode_retry_count": float64(10),
				"pool_mode_retry_status_codes": []any{float64(502), float64(504)},
			}}
			c, _ := newOpenAITransportErrTestContext()
			err := svc.handleOpenAIUpstreamTransportError(ctx, c, account, original, false)
			require.ErrorIs(t, err, original)
			var failover *UpstreamFailoverError
			require.False(t, errors.As(err, &failover))
			require.Empty(t, repo.tempUnschedCalls)
			require.False(t, svc.isOpenAIAccountRuntimeBlocked(account))
		})
	}
}
