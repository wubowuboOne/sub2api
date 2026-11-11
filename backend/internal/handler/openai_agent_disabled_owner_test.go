//go:build unit

package handler

import (
	"bytes"
	"io"
	"net/http"
	"testing"
	"time"

	"github.com/Wei-Shaw/sub2api/internal/repository"
	"github.com/Wei-Shaw/sub2api/internal/service"
	"github.com/alicebob/miniredis/v2"
	"github.com/redis/go-redis/v9"
	"github.com/stretchr/testify/require"
	"github.com/tidwall/gjson"
	"github.com/tidwall/sjson"
)

// Both accounts accept the intact task in this fixture. Routing, not a mocked
// encryption failure, determines whether a disabled owner can be replaced.
type disabledAgentOwnerUpstream struct {
	service.HTTPUpstream
	oracle issue715Upstream
	ids    []int64
	bodies [][]byte
	reject bool
}

func (u *disabledAgentOwnerUpstream) Do(req *http.Request, proxy string, accountID int64, concurrency int) (*http.Response, error) {
	body, err := io.ReadAll(req.Body)
	if err != nil {
		return nil, err
	}
	u.ids = append(u.ids, accountID)
	u.bodies = append(u.bodies, body)
	req.Body = io.NopCloser(bytes.NewReader(body))
	oracleAccount := int64(1)
	if u.reject && accountID != 1 {
		oracleAccount = accountID
	}
	return u.oracle.Do(req, proxy, oracleAccount, concurrency)
}

func TestOpenAIAgentDisabledOwnerSelectsEnabledAccount(t *testing.T) {
	for _, storage := range []string{"local", "redis"} {
		for _, accountType := range []string{service.AccountTypeAPIKey, service.AccountTypeOAuth} {
			for _, disabled := range []string{"scheduling", "status", "status_inactive", "scheduling_with_cooldown"} {
				for _, stream := range []bool{false, true} {
					name := storage + "/" + accountType + "/" + disabled
					if stream {
						name += "/stream"
					}
					t.Run(name, func(t *testing.T) {
						accounts := issue715AccountsOfType(accountType)
						upstream := &disabledAgentOwnerUpstream{}
						var cache service.GatewayCache
						if storage == "redis" {
							mr := miniredis.RunT(t)
							client := redis.NewClient(&redis.Options{Addr: mr.Addr(), MaxRetries: -1})
							t.Cleanup(func() { _ = client.Close() })
							cache = repository.NewGatewayCache(client)
						}
						h := newOpenAIResponsesFailoverTestHandlerWithCache(t, upstream, cache, accounts...)
						status, answer := issue715Request(t, h, issue715Body(false, stream), 99, "fixture-disabled-owner", false)
						require.Equal(t, http.StatusOK, status, answer)
						require.Equal(t, []int64{1}, upstream.ids)
						if disabled == "status" {
							accounts[0].Status = service.StatusDisabled
						} else if disabled == "status_inactive" {
							accounts[0].Status = "inactive"
						} else {
							accounts[0].Schedulable = false
							if disabled == "scheduling_with_cooldown" {
								until := time.Now().Add(time.Hour)
								accounts[0].RateLimitResetAt = &until
							}
						}
						body := issue715Body(true, stream)
						status, answer = issue715Request(t, h, body, 99, "fixture-disabled-owner", false)
						require.Equal(t, http.StatusOK, status, answer)
						require.Contains(t, answer, "Same task: ledger balance 18; update already applied once")
						require.NotContains(t, answer, "encrypted_agent_recovery_owner_unavailable")
						require.Equal(t, []int64{1, 2}, upstream.ids)
						require.JSONEq(t, gjson.Get(body, "input").Raw, gjson.GetBytes(upstream.bodies[1], "input").Raw)
						// A completed replacement becomes the owner; re-enabling the old
						// account must not pull the task back to its obsolete binding.
						accounts[0].Schedulable, accounts[0].Status = true, service.StatusActive
						accounts[0].RateLimitResetAt = nil
						if storage == "redis" {
							h = newOpenAIResponsesFailoverTestHandlerWithCache(t, upstream, cache, accounts...)
						}
						status, answer = issue715Request(t, h, body, 99, "fixture-disabled-owner", false)
						require.Equal(t, http.StatusOK, status, answer)
						require.Equal(t, []int64{1, 2, 2}, upstream.ids)
					})
				}
			}
		}
	}
}

func TestOpenAIAgentDisabledOwnerNativeCompactionPreservesHistory(t *testing.T) {
	for _, accountType := range []string{service.AccountTypeAPIKey, service.AccountTypeOAuth} {
		t.Run(accountType, func(t *testing.T) {
			accounts := issue715AccountsOfType(accountType)
			upstream := &disabledAgentOwnerUpstream{}
			h := newOpenAIResponsesFailoverTestHandler(t, upstream, accounts...)
			status, answer := issue715Request(t, h, issue715Body(false, true), 99, "fixture-disabled-compaction", false)
			require.Equal(t, http.StatusOK, status, answer)
			accounts[0].Schedulable = false
			body := issue715CompactionBody()
			status, answer = issue715Request(t, h, body, 99, "fixture-disabled-compaction", false)
			require.Equal(t, http.StatusOK, status, answer)
			require.Contains(t, answer, "fixture-compacted-reference")
			require.Equal(t, []int64{1, 2}, upstream.ids)
			require.JSONEq(t, gjson.Get(body, "input").Raw, gjson.GetBytes(upstream.bodies[1], "input").Raw)
		})
	}
}

func TestOpenAIAgentDisabledOwnerWithoutEnabledCandidate(t *testing.T) {
	accounts := issue715AccountsOfType(service.AccountTypeAPIKey)
	upstream := &disabledAgentOwnerUpstream{}
	h := newOpenAIResponsesFailoverTestHandler(t, upstream, accounts...)
	status, answer := issue715Request(t, h, issue715Body(false, false), 99, "fixture-disabled-empty-pool", false)
	require.Equal(t, http.StatusOK, status, answer)
	accounts[0].Schedulable, accounts[1].Schedulable = false, false
	status, answer = issue715Request(t, h, issue715Body(true, false), 99, "fixture-disabled-empty-pool", false)
	require.Equal(t, http.StatusBadGateway, status, "preserve the existing empty-pool response: %s", answer)
	require.NotContains(t, answer, "encrypted_agent_recovery_owner_unavailable")
	require.Equal(t, []int64{1}, upstream.ids)
}

func TestOpenAIAgentDisabledOwnerReportsActualUpstreamRejection(t *testing.T) {
	accounts := issue715AccountsOfType(service.AccountTypeAPIKey)
	upstream := &disabledAgentOwnerUpstream{reject: true}
	h := newOpenAIResponsesFailoverTestHandler(t, upstream, accounts...)
	status, answer := issue715Request(t, h, issue715Body(false, false), 99, "fixture-disabled-rejection", false)
	require.Equal(t, http.StatusOK, status, answer)
	accounts[0].Schedulable = false
	status, answer = issue715Request(t, h, issue715Body(true, false), 99, "fixture-disabled-rejection", false)
	require.Equal(t, http.StatusBadRequest, status, answer)
	require.NotContains(t, answer, "encrypted_agent_recovery_owner_unavailable")
	require.Equal(t, []int64{1, 2}, upstream.ids)
}

func TestOpenAIAgentDisabledOwnerKeepsToolResultRetryBudget(t *testing.T) {
	for _, failure := range []string{"partial", "pool_failure"} {
		t.Run(failure, func(t *testing.T) {
			accounts := issue715AccountsOfType(service.AccountTypeAPIKey)
			third := accounts[1]
			third.ID, third.Priority = 3, 2
			accounts = append(accounts, third)
			accounts[1].Credentials["pool_mode"] = true
			accounts[1].Credentials["pool_mode_retry_count"] = 2
			accounts[1].Credentials["pool_mode_retry_status_codes"] = []any{520}
			upstream := &disabledAgentOwnerUpstream{}
			h := newOpenAIResponsesFailoverTestHandler(t, upstream, accounts...)
			status, answer := issue715Request(t, h, issue715Body(false, true), 99, "fixture-disabled-retry", false)
			require.Equal(t, http.StatusOK, status, answer)
			accounts[0].Schedulable = false
			upstream.oracle.mode = failure
			_, answer = issue715Request(t, h, issue715Body(true, true), 99, "fixture-disabled-retry", false)
			require.GreaterOrEqual(t, len(upstream.ids), 2)
			replacement := upstream.ids[1]
			require.Contains(t, []int64{2, 3}, replacement)
			want := []int64{1, replacement}
			if failure == "pool_failure" {
				want = []int64{1, replacement, replacement, replacement}
			} else {
				require.Contains(t, answer, "started existing task")
			}
			require.Equal(t, want, upstream.ids, "the replacement uses same-account retries without replaying tool results on another enabled account")
		})
	}
}

func TestOpenAIAgentDisabledOwnerPreservesPreviousResponseReference(t *testing.T) {
	accounts := issue715AccountsOfType(service.AccountTypeAPIKey)
	upstream := &disabledAgentOwnerUpstream{}
	h := newOpenAIResponsesFailoverTestHandler(t, upstream, accounts...)
	first, err := sjson.Set(issue715Body(false, false), "store", true)
	require.NoError(t, err)
	status, answer := issue715Request(t, h, first, 99, "fixture-disabled-previous", false)
	require.Equal(t, http.StatusOK, status, answer)
	previous := gjson.Get(answer, "id").String()
	require.NotEmpty(t, previous)
	accounts[0].Schedulable = false
	body, err := sjson.Set(issue715Body(true, false), "previous_response_id", previous)
	require.NoError(t, err)
	status, answer = issue715Request(t, h, body, 99, "fixture-disabled-previous", false)
	require.Equal(t, http.StatusOK, status, answer)
	require.Equal(t, []int64{1, 2}, upstream.ids)
	require.Equal(t, previous, gjson.GetBytes(upstream.bodies[1], "previous_response_id").String())
	require.JSONEq(t, gjson.Get(body, "input").Raw, gjson.GetBytes(upstream.bodies[1], "input").Raw)
}
