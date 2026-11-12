//go:build unit

package handler

import (
	"fmt"
	"net/http"
	"testing"

	"github.com/Wei-Shaw/sub2api/internal/server/middleware"
	"github.com/Wei-Shaw/sub2api/internal/service"
	"github.com/stretchr/testify/require"
)

func TestIssue805MergedOwnerOptOutNeverUsesPoolRetry(t *testing.T) {
	for _, known := range []bool{false, true} {
		for _, toolResult := range []bool{false, true} {
			t.Run(fmt.Sprintf("known=%t/tool=%t", known, toolResult), func(t *testing.T) {
				account := issue715Accounts()[0]
				account.Type = service.AccountTypeAPIKey
				account.Concurrency = 0
				account.Credentials = map[string]any{
					"api_key": "fixture-owner-opt-out", "base_url": "https://api.example.test",
					"pool_mode": true, "pool_mode_retry_count": 3,
					"pool_mode_retry_status_codes": []any{float64(524)},
				}
				upstream := &openAIHTTP524DiagnosticUpstream{issue715Upstream: &issue715Upstream{}}
				h := newOpenAIResponsesFailoverTestHandler(t, upstream, account)
				if known {
					status, answer := issue715Request(t, h, issue715Body(false, true), 99, "fixture-merge-thread", false)
					require.Equal(t, http.StatusOK, status, answer)
				}
				h.cfg.Gateway.DisableEncryptedAgentOwnerBinding = true
				upstream.mu.Lock()
				upstream.mode = "pool_failure"
				upstream.mu.Unlock()
				c, rec := newIssue715Context(t, issue715Body(toolResult, true))
				c.Request.Header.Set("session_id", "fixture-merge-thread")
				key, _ := c.Get(string(middleware.ContextKeyAPIKey))
				key.(*service.APIKey).ID = 99
				h.Responses(c)
				require.True(t, service.OpenAIAgentRecoveryPreservesHistory(c.Request.Context()))
				require.Equal(t, 1, upstream.failures, "the opt-out allows one intact attempt even with configured retries")
				require.NotEmpty(t, rec.Body.String())
			})
		}
	}
}
