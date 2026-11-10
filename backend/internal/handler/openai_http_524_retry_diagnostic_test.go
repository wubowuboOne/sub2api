//go:build unit

package handler

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/Wei-Shaw/sub2api/internal/pkg/logger"
	"github.com/Wei-Shaw/sub2api/internal/server/middleware"
	"github.com/Wei-Shaw/sub2api/internal/service"
	"github.com/gin-gonic/gin"
	"github.com/stretchr/testify/require"
	"github.com/tidwall/gjson"
	"go.uber.org/zap"
	"go.uber.org/zap/zaptest/observer"
)

type openAIHTTP524DiagnosticUpstream struct {
	*issue715Upstream
	failureDelay   time.Duration
	failures       int
	failureStatus  int
	recoverAfter   int
	streamFailure  bool
	afterFailure   func(int)
	requestHeaders []http.Header
}

func (u *openAIHTTP524DiagnosticUpstream) Do(req *http.Request, proxy string, accountID int64, concurrency int) (*http.Response, error) {
	if u.recoverAfter > 0 && u.failures >= u.recoverAfter {
		u.mu.Lock()
		u.mode = ""
		u.mu.Unlock()
	}
	u.requestHeaders = append(u.requestHeaders, req.Header.Clone())
	resp, err := u.issue715Upstream.Do(req, proxy, accountID, concurrency)
	if err != nil || resp.StatusCode != 520 {
		return resp, err
	}
	u.failures++
	if u.failures == 1 && u.failureDelay > 0 {
		time.Sleep(u.failureDelay)
	}
	_ = resp.Body.Close()
	resp.StatusCode = 524
	if u.failureStatus != 0 {
		resp.StatusCode = u.failureStatus
	}
	resp.Header.Set("Content-Type", "text/plain")
	resp.Body = io.NopCloser(strings.NewReader(fmt.Sprintf("error code: %d", resp.StatusCode)))
	if u.streamFailure {
		payload, _ := json.Marshal(map[string]any{
			"type": "response.failed",
			"response": map[string]any{
				"id": "resp_fixture_retry", "status": "failed",
				"error": map[string]any{
					"type": "bps_stream_error", "code": "bps_stream_error",
					"message": "Upstream request failed, please try again later",
				},
			},
		})
		var wire strings.Builder
		fmt.Fprintln(&wire, "data: "+string(payload))
		fmt.Fprintln(&wire)
		resp.StatusCode = http.StatusOK
		resp.Header.Set("Content-Type", "text/event-stream")
		resp.Body = io.NopCloser(strings.NewReader(wire.String()))
	}
	if u.afterFailure != nil {
		u.afterFailure(u.failures)
	}
	return resp, nil
}

func TestOpenAIHTTP524AgentToolContinuationUsesConfiguredRetries(t *testing.T) {
	gin.SetMode(gin.TestMode)
	const retryCount = 10
	for _, tc := range []struct {
		name       string
		primeOwner bool
		toolResult bool
		wantCalls  int
	}{
		{name: "unknown_owner_tool_result", toolResult: true, wantCalls: retryCount + 1},
		{name: "known_owner_without_tool_result", primeOwner: true, wantCalls: retryCount + 1},
		{name: "known_owner_tool_result", primeOwner: true, toolResult: true, wantCalls: retryCount + 1},
	} {
		for _, heartbeat := range []bool{false, true} {
			name := tc.name + "/without_heartbeat"
			if heartbeat {
				name = tc.name + "/header_wait_heartbeat"
			}
			t.Run(name, func(t *testing.T) {
				account := issue715Accounts()[0]
				account.Type = service.AccountTypeAPIKey
				// Zero means unlimited: admission capacity cannot stop this request.
				account.Concurrency = 0
				account.Credentials = map[string]any{
					"api_key": "fixture-524-key", "base_url": "https://api.example.test",
					"pool_mode": true, "pool_mode_retry_count": retryCount,
					"pool_mode_retry_status_codes": []any{float64(524)},
				}
				upstream := &openAIHTTP524DiagnosticUpstream{issue715Upstream: &issue715Upstream{}}
				h := newOpenAIResponsesFailoverTestHandler(t, upstream, account)
				if heartbeat {
					h.cfg.Gateway.StreamKeepaliveInterval = 1
					upstream.failureDelay = 1200 * time.Millisecond
				}
				if tc.primeOwner {
					status, answer := issue715Request(t, h, issue715Body(false, true), 99, "fixture-524-thread", false)
					require.Equal(t, http.StatusOK, status, answer)
					require.Contains(t, answer, "update_ledger")
				}
				upstream.mu.Lock()
				upstream.mode = "pool_failure"
				start := len(upstream.ids)
				upstream.mu.Unlock()
				c, rec := newIssue715Context(t, issue715Body(tc.toolResult, true))
				c.Request.Header.Set("session_id", "fixture-524-thread")
				key, _ := c.Get(string(middleware.ContextKeyAPIKey))
				key.(*service.APIKey).ID = 99
				core, logs := observer.New(zap.DebugLevel)
				c.Request = c.Request.WithContext(logger.IntoContext(c.Request.Context(), zap.New(core)))
				h.Responses(c)

				require.Equal(t, tc.primeOwner && tc.toolResult, service.OpenAIAgentToolResultContinuation(c.Request.Context()))
				require.Equal(t, tc.wantCalls, upstream.failures, "524 attempts must follow the replay guard rather than concurrency capacity")
				require.NotContains(t, rec.Body.String(), gatewayQueueFullCode)
				require.Equal(t, tc.wantCalls-1, logs.FilterMessage("openai.pool_mode_same_account_retry").Len())
				stopped := logs.FilterMessage("openai.failover_stopped").All()
				if tc.primeOwner && tc.toolResult {
					require.Len(t, stopped, 1)
					fields := stopped[0].ContextMap()
					require.Equal(t, "agent_tool_result_continuation", fields["reason"])
					require.EqualValues(t, 1, fields["account_id"])
					require.EqualValues(t, 524, fields["upstream_status"])
					require.Equal(t, true, fields["retryable_on_same_account"])
					require.EqualValues(t, retryCount, fields["retry_count"])
					require.EqualValues(t, retryCount, fields["retry_limit"])
				} else {
					require.Empty(t, stopped)
				}
				if heartbeat {
					require.Equal(t, http.StatusOK, rec.Code)
					require.Contains(t, rec.Body.String(), "response.failed")
				} else if tc.primeOwner && !tc.toolResult {
					require.Equal(t, http.StatusConflict, rec.Code)
					require.Contains(t, rec.Body.String(), "encrypted_agent_recovery_owner_unavailable")
				} else {
					require.Equal(t, http.StatusBadGateway, rec.Code)
				}
				upstream.mu.Lock()
				ids := append([]int64(nil), upstream.ids[start:]...)
				bodies := append([][]byte(nil), upstream.bodies[start:]...)
				upstream.mu.Unlock()
				require.Len(t, ids, tc.wantCalls)
				for i, id := range ids {
					require.EqualValues(t, 1, id)
					require.Equal(t, bodies[0], bodies[i], "retry must retain the original task and tool result")
				}
				value, ok := c.Get(service.OpsUpstreamErrorsKey)
				require.True(t, ok)
				events, ok := value.([]*service.OpsUpstreamErrorEvent)
				require.True(t, ok)
				require.Len(t, events, tc.wantCalls)
				for _, event := range events {
					require.Equal(t, 524, event.UpstreamStatusCode)
					require.Equal(t, "failover", event.Kind)
				}
			})
		}
	}
}

func TestOpenAIAgentToolResultConfiguredRetryRecoveryAndStops(t *testing.T) {
	gin.SetMode(gin.TestMode)
	for _, tc := range []struct {
		name          string
		status        int
		retryStatus   int
		retries       int
		recoverAfter  int
		wantCalls     int
		streamFailure bool
		disablePool   bool
		oauth         bool
		stop          string
	}{
		{name: "http_524_recovers", status: 524, retries: 10, recoverAfter: 2, wantCalls: 3},
		{name: "http_502_recovers", status: 502, retries: 10, recoverAfter: 2, wantCalls: 3},
		{name: "http_429_recovers", status: 429, retries: 10, recoverAfter: 2, wantCalls: 3},
		{name: "http_413_recovers", status: 413, retries: 10, recoverAfter: 2, wantCalls: 3},
		{name: "bps_stream_failure_recovers", status: 502, retries: 10, recoverAfter: 2, wantCalls: 3, streamFailure: true},
		{name: "retry_disabled", status: 524, retries: 0, wantCalls: 1},
		{name: "status_not_configured", status: 524, retryStatus: 502, retries: 10, wantCalls: 1},
		{name: "non_pool", status: 524, retries: 10, wantCalls: 1, disablePool: true},
		{name: "oauth_429_keeps_tool_result_guard", status: 429, retries: 10, wantCalls: 1, oauth: true},
		{name: "credential_changed", status: 524, retries: 10, wantCalls: 1, stop: "credentials"},
		{name: "route_changed", status: 524, retries: 10, wantCalls: 1, stop: "route"},
		{name: "client_disconnected", status: 524, retries: 10, wantCalls: 1, stop: "cancel"},
		{name: "output_already_delivered", status: 524, retries: 10, wantCalls: 1, stop: "output"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			accounts := issue715Accounts()
			accounts[0].Type = service.AccountTypeAPIKey
			retryStatus := tc.retryStatus
			if retryStatus == 0 {
				retryStatus = tc.status
			}
			accounts[0].Credentials = map[string]any{
				"api_key": "fixture-524-key", "base_url": "https://api.example.test",
				"pool_mode": !tc.disablePool, "pool_mode_retry_count": tc.retries,
				"pool_mode_retry_status_codes": []any{float64(retryStatus)},
			}
			if tc.oauth {
				accounts[0].Type = service.AccountTypeOAuth
				accounts[0].Credentials["access_token"] = "fixture-oauth-token"
			}
			upstream := &openAIHTTP524DiagnosticUpstream{
				issue715Upstream: &issue715Upstream{}, failureStatus: tc.status,
				recoverAfter: tc.recoverAfter, streamFailure: tc.streamFailure,
			}
			h := newOpenAIResponsesFailoverTestHandler(t, upstream, accounts...)
			withCacheKey := func(body string) string {
				var payload map[string]any
				require.NoError(t, json.Unmarshal([]byte(body), &payload))
				payload["prompt_cache_key"] = "fixture-stable-retry-cache"
				encoded, err := json.Marshal(payload)
				require.NoError(t, err)
				return string(encoded)
			}
			status, answer := issue715Request(t, h, withCacheKey(issue715Body(false, true)), 99, "fixture-retry-thread", false)
			require.Equal(t, http.StatusOK, status, answer)
			require.Contains(t, answer, "update_ledger")
			accounts[0].Priority, accounts[1].Priority = 100, 0
			upstream.mu.Lock()
			upstream.mode = "pool_failure"
			start := len(upstream.ids)
			upstream.mu.Unlock()
			c, rec := newIssue715Context(t, withCacheKey(issue715Body(true, true)))
			c.Request.Header.Set("session_id", "fixture-retry-thread")
			key, _ := c.Get(string(middleware.ContextKeyAPIKey))
			key.(*service.APIKey).ID = 99
			ctx, cancel := context.WithCancel(c.Request.Context())
			defer cancel()
			c.Request = c.Request.WithContext(ctx)
			core, logs := observer.New(zap.DebugLevel)
			c.Request = c.Request.WithContext(logger.IntoContext(c.Request.Context(), zap.New(core)))
			upstream.afterFailure = func(failures int) {
				if failures != 1 {
					return
				}
				switch tc.stop {
				case "credentials":
					accounts[0].Credentials["api_key"] = "fixture-rotated-secret"
				case "route":
					accounts[0].Credentials["base_url"] = "https://changed.example.test"
				case "cancel":
					cancel()
				case "output":
					payload, _ := json.Marshal(map[string]any{
						"type": "response.output_text.delta", "delta": "already delivered",
					})
					c.Writer.Header().Set("Content-Type", "text/event-stream")
					fmt.Fprintln(c.Writer, "data: "+string(payload))
					fmt.Fprintln(c.Writer)
					c.Writer.Flush()
				}
			}
			h.Responses(c)
			require.True(t, service.OpenAIAgentToolResultContinuation(c.Request.Context()))
			upstream.mu.Lock()
			ids := append([]int64(nil), upstream.ids[start:]...)
			bodies := append([][]byte(nil), upstream.bodies[start:]...)
			upstream.mu.Unlock()
			require.Len(t, ids, tc.wantCalls, rec.Body.String())
			for i, id := range ids {
				require.EqualValues(t, 1, id, "a bound tool result must never reach the alternate account")
				require.Equal(t, bodies[0], bodies[i])
				require.Equal(t, "fixture-stable-retry-cache", gjson.GetBytes(bodies[i], "prompt_cache_key").String())
				require.Equal(t, upstream.requestHeaders[start].Get("session_id"), upstream.requestHeaders[start+i].Get("session_id"))
			}
			require.NotContains(t, rec.Body.String(), gatewayQueueFullCode)
			require.NotContains(t, rec.Body.String(), "fixture-rotated-secret")
			if tc.recoverAfter > 0 {
				require.Equal(t, tc.recoverAfter, upstream.failures)
				require.Equal(t, http.StatusOK, rec.Code)
				require.Contains(t, rec.Body.String(), "Same task: ledger balance 18")
				require.NotContains(t, rec.Body.String(), "response.failed")
			} else if tc.stop == "cancel" {
				require.Equal(t, statusClientClosedRequest, c.Writer.Status())
				require.Zero(t, rec.Body.Len())
			} else if tc.stop == "output" {
				require.Equal(t, http.StatusOK, rec.Code)
				require.Contains(t, rec.Body.String(), "already delivered")
				require.Contains(t, rec.Body.String(), "response.failed")
				stopped := logs.FilterMessage("openai.failover_stopped").All()
				require.Len(t, stopped, 1)
				require.Equal(t, "downstream_output_started", stopped[0].ContextMap()["reason"])
			} else if tc.stop != "" {
				require.Equal(t, http.StatusConflict, rec.Code)
				require.Contains(t, rec.Body.String(), "encrypted_agent_recovery_route_changed")
			} else if tc.oauth {
				require.Equal(t, http.StatusTooManyRequests, rec.Code)
			} else {
				require.Equal(t, http.StatusBadGateway, rec.Code)
			}
		})
	}
}
