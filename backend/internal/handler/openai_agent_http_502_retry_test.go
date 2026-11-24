//go:build unit

package handler

import (
	"context"
	"net/http"
	"testing"

	"github.com/Wei-Shaw/sub2api/internal/pkg/logger"
	"github.com/Wei-Shaw/sub2api/internal/service"
	"github.com/stretchr/testify/require"
	"github.com/tidwall/sjson"
	"go.uber.org/zap"
	"go.uber.org/zap/zaptest/observer"
)

func TestOpenAIAgentPreservedHistoryHTTP502Retry(t *testing.T) {
	const reason = "openai_agent_http_502_same_account_retry"
	for _, tc := range []struct {
		name         string
		recoverAfter int
		wantAttempts int
	}{
		{name: "recovers", recoverAfter: 2, wantAttempts: 3},
		{name: "exhausted", wantAttempts: 3},
		{name: "not_configured", wantAttempts: 1},
		{name: "retry_disabled", wantAttempts: 1},
		{name: "stored", wantAttempts: 1},
		{name: "hosted_tool", wantAttempts: 1},
		{name: "canceled", wantAttempts: 1},
		{name: "output_delivered", wantAttempts: 1},
		{name: "route_changed", wantAttempts: 1},
		{name: "proxy_changed", wantAttempts: 1},
	} {
		t.Run(tc.name, func(t *testing.T) {
			accounts := issue715Accounts()
			accounts[1].Schedulable = false
			accounts[0].Type = service.AccountTypeAPIKey
			accounts[0].Credentials = map[string]any{
				"api_key": "fixture-502-key", "base_url": "https://api.example.test",
				"pool_mode": true, "pool_mode_retry_count": 2,
				"pool_mode_retry_status_codes": []any{http.StatusBadGateway},
			}
			switch tc.name {
			case "not_configured":
				delete(accounts[0].Credentials, "pool_mode_retry_status_codes")
			case "retry_disabled":
				accounts[0].Credentials["pool_mode_retry_count"] = 0
			}
			upstream := &openAIHTTP524DiagnosticUpstream{
				issue715Upstream: &issue715Upstream{mode: "pool_failure"},
				failureStatus:    http.StatusBadGateway,
				recoverAfter:     tc.recoverAfter,
			}
			h := newOpenAIResponsesFailoverTestHandler(t, upstream, accounts...)
			h.cfg.Gateway.DisableEncryptedAgentOwnerBinding = true
			body, err := sjson.Set(issue715Body(true, true), "store", tc.name == "stored")
			require.NoError(t, err)
			tools := `[{"type":"namespace","name":"ledger","tools":[{"type":"function","name":"inspect"}]}]`
			if tc.name == "hosted_tool" {
				tools = `[{"type":"web_search"}]`
			}
			body, err = sjson.SetRaw(body, "tools", tools)
			require.NoError(t, err)
			c, rec := newIssue715Context(t, body)
			c.Request.Header.Set("session_id", "fixture-http-502")
			ctx, cancel := context.WithCancel(c.Request.Context())
			defer cancel()
			core, logs := observer.New(zap.DebugLevel)
			c.Request = c.Request.WithContext(logger.IntoContext(ctx, zap.New(core)))
			upstream.afterFailure = func(n int) {
				if n != 1 {
					return
				}
				accounts[1].Schedulable = true
				accounts[0].Priority, accounts[1].Priority = 100, 0
				switch tc.name {
				case "canceled":
					cancel()
				case "output_delivered":
					_, _ = c.Writer.WriteString("data: {\"type\":\"response.output_text.delta\",\"delta\":\"visible\"}\n\n")
					c.Writer.Flush()
				case "route_changed":
					accounts[0].Credentials["base_url"] = "https://changed.example.test"
				case "proxy_changed":
					id := int64(7)
					accounts[0].ProxyID = &id
					accounts[0].Proxy = &service.Proxy{ID: id, Protocol: "http", Host: "proxy.example.test", Port: 8080}
				}
			}
			h.Responses(c)

			require.True(t, service.OpenAIAgentRecoveryPreservesHistory(c.Request.Context()))
			require.Len(t, upstream.ids, tc.wantAttempts, rec.Body.String())
			for i, id := range upstream.ids {
				require.EqualValues(t, 1, id)
				require.Equal(t, upstream.bodies[0], upstream.bodies[i])
				require.Equal(t, upstream.requestHeaders[0], upstream.requestHeaders[i])
			}
			retries := logs.FilterMessage("openai.pool_mode_same_account_retry")
			if tc.name == "recovers" || tc.name == "exhausted" {
				require.Equal(t, 2, retries.Len())
				for _, entry := range retries.All() {
					require.Equal(t, reason, entry.ContextMap()["retry_reason"])
				}
				value, ok := c.Get(service.OpsUpstreamErrorsKey)
				require.True(t, ok)
				events := value.([]*service.OpsUpstreamErrorEvent)
				require.Len(t, events, upstream.failures)
				for _, event := range events {
					require.Equal(t, reason, event.Reason)
					require.Equal(t, http.StatusBadGateway, event.UpstreamStatusCode)
				}
			}
			require.Zero(t, logs.FilterMessage("openai.upstream_failover_switching").Len())
			if tc.recoverAfter > 0 {
				require.Equal(t, http.StatusOK, rec.Code, rec.Body.String())
				require.Contains(t, rec.Body.String(), "Same task: ledger balance 18")
			} else if tc.name == "exhausted" {
				require.Equal(t, http.StatusBadGateway, rec.Code, rec.Body.String())
			}
		})
	}
}
