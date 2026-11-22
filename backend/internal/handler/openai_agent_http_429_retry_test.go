//go:build unit

package handler

import (
	"context"
	"fmt"
	"io"
	"net/http"
	"strings"
	"testing"

	"github.com/Wei-Shaw/sub2api/internal/model"
	"github.com/Wei-Shaw/sub2api/internal/pkg/logger"
	"github.com/Wei-Shaw/sub2api/internal/server/middleware"
	"github.com/Wei-Shaw/sub2api/internal/service"
	"github.com/stretchr/testify/require"
	"go.uber.org/zap"
	"go.uber.org/zap/zaptest/observer"
)

const agentHTTP429Body = `{"error":{"message":"Upstream rate limit exceeded, please retry later","type":"rate_limit_error"}}`

type agentHTTP429Upstream struct {
	*openAIHTTP524DiagnosticUpstream
	payload string
	sse     bool
}

func (u *agentHTTP429Upstream) Do(req *http.Request, proxy string, accountID int64, concurrency int) (*http.Response, error) {
	resp, err := u.openAIHTTP524DiagnosticUpstream.Do(req, proxy, accountID, concurrency)
	if err != nil || resp.StatusCode != http.StatusTooManyRequests {
		return resp, err
	}
	_ = resp.Body.Close()
	payload := u.payload
	if payload == "" {
		payload = agentHTTP429Body
	}
	resp.Header.Set("Content-Type", "application/json")
	resp.Header.Set("Retry-After-Ms", "1")
	if u.sse {
		resp.StatusCode = http.StatusOK
		resp.Header.Set("Content-Type", "text/event-stream")
		payload = "event: error\ndata: " + payload + "\n\n"
	}
	resp.Body = io.NopCloser(strings.NewReader(payload))
	return resp, nil
}

func TestOpenAIAgentPreservedHistoryHTTP429Retry(t *testing.T) {
	for _, stream := range []bool{false, true} {
		for _, toolResult := range []bool{false, true} {
			for _, known := range []bool{false, true} {
				for _, recover := range []bool{false, true} {
					t.Run(fmt.Sprintf("stream=%t/tool=%t/known=%t/recover=%t", stream, toolResult, known, recover), func(t *testing.T) {
						retryCount := 2
						if stream && toolResult && known {
							retryCount = 10
						}
						accounts := issue715Accounts()
						accounts[1].Schedulable = false
						accounts[0].Type = service.AccountTypeAPIKey
						accounts[0].Concurrency = 0
						accounts[0].Credentials = map[string]any{
							"api_key": "fixture-429-key", "base_url": "https://api.example.test",
							"pool_mode": true, "pool_mode_retry_count": retryCount,
							"pool_mode_retry_status_codes": []any{float64(429)},
						}
						upstream := &agentHTTP429Upstream{openAIHTTP524DiagnosticUpstream: &openAIHTTP524DiagnosticUpstream{
							issue715Upstream: &issue715Upstream{}, failureStatus: 429,
						}}
						h := newOpenAIResponsesFailoverTestHandler(t, upstream, accounts...)
						if known {
							status, answer := issue715Request(t, h, issue715Body(false, true), 99, "fixture-429-thread", false)
							require.Equal(t, http.StatusOK, status, answer)
						}
						h.cfg.Gateway.DisableEncryptedAgentOwnerBinding = true
						mappedStatus, mappedMessage := http.StatusServiceUnavailable, "Selected model is at capacity. Please try a different model."
						h.errorPassthroughService = service.NewErrorPassthroughService(&configuredHTTPPassthroughRepo{rules: []*model.ErrorPassthroughRule{{
							ID: 5, Enabled: true, Keywords: []string{"Please retry later"}, MatchMode: model.MatchModeAny,
							ResponseCode: &mappedStatus, CustomMessage: &mappedMessage,
						}}}, nil)
						upstream.mode = "pool_failure"
						start := len(upstream.ids)
						if recover {
							upstream.recoverAfter = retryCount
						}
						// Make another account preferable after the first rejection.
						upstream.afterFailure = func(int) {
							accounts[1].Schedulable = true
							accounts[0].Priority, accounts[1].Priority = 100, 0
						}
						c, rec := newIssue715Context(t, issue715Body(toolResult, stream))
						service.BindErrorPassthroughService(c, h.errorPassthroughService)
						c.Request.Header.Set("session_id", "fixture-429-thread")
						key, _ := c.Get(string(middleware.ContextKeyAPIKey))
						key.(*service.APIKey).ID = 99
						core, logs := observer.New(zap.DebugLevel)
						c.Request = c.Request.WithContext(logger.IntoContext(c.Request.Context(), zap.New(core)))
						h.Responses(c)

						require.True(t, service.OpenAIAgentRecoveryPreservesHistory(c.Request.Context()))
						require.Len(t, upstream.ids[start:], retryCount+1, rec.Body.String())
						require.Equal(t, retryCount, logs.FilterMessage("openai.pool_mode_same_account_retry").Len())
						require.Zero(t, logs.FilterMessage("openai.upstream_failover_switching").Len())
						for i, id := range upstream.ids[start:] {
							require.EqualValues(t, 1, id)
							require.Equal(t, upstream.bodies[start], upstream.bodies[start+i], "the entire wire body must remain identical")
							require.Equal(t, upstream.requestHeaders[start], upstream.requestHeaders[start+i])
						}
						if recover {
							require.Equal(t, http.StatusOK, rec.Code, rec.Body.String())
							require.NotContains(t, rec.Body.String(), "rate limit")
						} else {
							require.Equal(t, mappedStatus, rec.Code, rec.Body.String())
							require.Contains(t, rec.Body.String(), mappedMessage)
						}
					})
				}
			}
		}
	}
}

func TestOpenAIAgentPreservedHistoryHTTP429Stops(t *testing.T) {
	for _, stop := range []string{"retry_disabled", "status_excluded", "non_pool", "cancel", "output", "credentials", "route", "model", "disabled", "sse", "quota", "blocked"} {
		t.Run(stop, func(t *testing.T) {
			accounts := issue715Accounts()
			accounts[1].Schedulable = false
			accounts[0].Type = service.AccountTypeAPIKey
			accounts[0].Concurrency = 0
			accounts[0].Credentials = map[string]any{
				"api_key": "fixture-429-key", "base_url": "https://api.example.test",
				"pool_mode": true, "pool_mode_retry_count": 2,
				"pool_mode_retry_status_codes": []any{float64(429)},
			}
			switch stop {
			case "retry_disabled":
				accounts[0].Credentials["pool_mode_retry_count"] = 0
			case "status_excluded":
				accounts[0].Credentials["pool_mode_retry_status_codes"] = []any{float64(524)}
			case "non_pool":
				accounts[0].Credentials["pool_mode"] = false
			}
			upstream := &agentHTTP429Upstream{openAIHTTP524DiagnosticUpstream: &openAIHTTP524DiagnosticUpstream{
				issue715Upstream: &issue715Upstream{mode: "pool_failure"}, failureStatus: 429,
			}}
			if stop == "sse" {
				upstream.sse = true
			}
			if stop == "quota" {
				upstream.payload = `{"error":{"code":"insufficient_quota","type":"insufficient_quota","message":"Quota exceeded"}}`
			}
			if stop == "blocked" {
				upstream.payload = `{"error":{"code":"session_blocked_by_cyber_policy","message":"Session blocked"}}`
			}
			h := newOpenAIResponsesFailoverTestHandler(t, upstream, accounts...)
			h.cfg.Gateway.DisableEncryptedAgentOwnerBinding = true
			c, rec := newIssue715Context(t, issue715Body(false, true))
			c.Request.Header.Set("session_id", "fixture-429-stop")
			ctx, cancel := context.WithCancel(c.Request.Context())
			defer cancel()
			c.Request = c.Request.WithContext(ctx)
			upstream.afterFailure = func(int) {
				accounts[1].Schedulable = true
				switch stop {
				case "cancel":
					cancel()
				case "output":
					_, _ = c.Writer.WriteString("data: {\"type\":\"response.output_text.delta\",\"delta\":\"visible\"}\n\n")
					c.Writer.Flush()
				case "credentials":
					accounts[0].Credentials["api_key"] = "fixture-rotated-key"
				case "route":
					accounts[0].Credentials["base_url"] = "https://changed.example.test"
				case "model":
					accounts[0].Credentials["model_mapping"] = map[string]any{"gpt-5.1": "changed-model"}
				case "disabled":
					accounts[0].Schedulable = false
				}
			}
			h.Responses(c)
			require.Len(t, upstream.ids, 1, rec.Body.String())
			require.EqualValues(t, 1, upstream.ids[0])
		})
	}
}
