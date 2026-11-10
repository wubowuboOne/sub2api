//go:build unit

package handler

import (
	"net/http"
	"strings"
	"testing"

	"github.com/Wei-Shaw/sub2api/internal/server/middleware"
	"github.com/Wei-Shaw/sub2api/internal/service"
	"github.com/gin-gonic/gin"
	"github.com/stretchr/testify/require"
	"github.com/tidwall/gjson"
)

func TestIssue715HTTPNativeCompactionPreservesTaskAndRoute(t *testing.T) {
	gin.SetMode(gin.TestMode)
	for _, accountType := range []string{service.AccountTypeAPIKey, service.AccountTypeOAuth} {
		for _, features := range [][]string{nil, {"remote_compaction_v2"}, {"feature_a, feature_b"}, {"feature_a", "feature_b"}} {
			t.Run(accountType+"/"+strings.Join(features, ";"), func(t *testing.T) {
				accounts := issue715Accounts()
				if accountType == service.AccountTypeAPIKey {
					accounts[0].Type = service.AccountTypeAPIKey
					accounts[0].Credentials = map[string]any{"api_key": "fixture-key"}
					accounts[0].Extra = map[string]any{"openai_responses_supported": true}
				}
				upstream := &issue715Upstream{}
				handler := newOpenAIResponsesFailoverTestHandler(t, upstream, accounts...)
				request := func(body string) (int, string) {
					c, rec := newIssue715Context(t, body)
					c.Request.Header.Set("session_id", "fixture-715-thread")
					if len(features) > 0 {
						c.Request.Header["X-Codex-Beta-Features"] = append([]string(nil), features...)
					}
					key, _ := c.Get(string(middleware.ContextKeyAPIKey))
					key.(*service.APIKey).ID = 99
					handler.Responses(c)
					return rec.Code, rec.Body.String()
				}
				status, answer := request(issue715Body(false, false))
				require.Equal(t, http.StatusOK, status, answer)
				accounts[0].Priority, accounts[1].Priority = 100, 0
				body := strings.TrimSuffix(issue715Body(true, true), "]}") + `,{"type":"compaction_trigger"}]}`
				status, answer = request(body)
				require.Equal(t, http.StatusOK, status, answer)
				require.Contains(t, answer, "fixture-compacted-reference", "native compact output must reach the client")
				require.Equal(t, []int64{1, 1}, upstream.ids, "native compaction must reach the original owner exactly once")
				require.JSONEq(t, issue715Agent, gjson.GetBytes(upstream.bodies[1], "input.#(type==agent_message)").Raw)
				require.Equal(t, "ledger balance 18; applied once", gjson.GetBytes(upstream.bodies[1], "input.#(type==function_call_output).output").String())
				input := gjson.GetBytes(upstream.bodies[1], "input").Array()
				require.NotEmpty(t, input)
				require.Equal(t, "compaction_trigger", input[len(input)-1].Get("type").String())
				status, answer = request(issue715Body(true, false))
				require.Equal(t, http.StatusOK, status, answer)
				require.Contains(t, answer, "Same task: ledger balance 18; update already applied once")
				require.Equal(t, []int64{1, 1, 1}, upstream.ids, "compaction must not overwrite or invalidate the proven normal route")
			})
		}
	}
}
