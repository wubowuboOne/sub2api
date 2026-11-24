package service

import (
	"bytes"
	"context"
	"net/http"
	"testing"
	"time"

	"github.com/Wei-Shaw/sub2api/internal/config"
	"github.com/gin-gonic/gin"
	"github.com/stretchr/testify/require"
)

type agentHTTP429AccountRepo struct{ AccountRepository }

func newAgentHTTP429Fixture(t *testing.T) (*OpenAIGatewayService, *gin.Context, *Account, *http.Request, *http.Response) {
	t.Helper()
	c, _ := newTurnStateTestContext(t, 715, "fixture-session")
	c.Set("api_key", &APIKey{ID: 715, GroupID: int64Ptr(715)})
	cfg := &config.Config{}
	cfg.Gateway.DisableEncryptedAgentOwnerBinding = true
	s := &OpenAIGatewayService{cfg: cfg, accountRepo: agentHTTP429AccountRepo{}}
	a := &Account{ID: 1, Platform: PlatformOpenAI, Type: AccountTypeAPIKey, Credentials: map[string]any{
		"api_key": "fixture", "pool_mode": true, "pool_mode_retry_count": 10,
		"pool_mode_retry_status_codes": []any{float64(429)},
	}}
	ctx := s.WithOpenAIAgentMessageRecovery(c.Request.Context(), c, []byte(issue715OpaqueBody), "fixture-session", "gpt-6-astra")
	c.Request = c.Request.WithContext(ctx)
	req, err := http.NewRequest(http.MethodPost, "https://native.example/v1/responses", bytes.NewBufferString(issue715OpaqueBody))
	require.NoError(t, err)
	req.Header.Set("Authorization", "Bearer fixture")
	finish, err := s.prepareOpenAIAgentRecoveryAttempt(ctx, a, req)
	require.NoError(t, err)
	t.Cleanup(func() { finish(false) })
	return s, c, a, req, &http.Response{StatusCode: 429, Header: make(http.Header)}
}

func TestOpenAIAgentHTTP429RetryPolicy(t *testing.T) {
	const payload = `{"error":{"message":"Upstream rate limit exceeded, please retry later","type":"rate_limit_error"}}`
	for _, change := range []string{"none", "omitted", "omitted400", "compaction", "quota_reset", "quota_header", "quota_code", "context_window", "policy", "access", "unknown_code", "other_status", "committed", "canceled", "bare_service"} {
		t.Run(change, func(t *testing.T) {
			s, c, a, req, resp := newAgentHTTP429Fixture(t)
			body := []byte(payload)
			r := openAIAgentRecoveryFromContext(c.Request.Context())
			switch change {
			case "omitted":
				r.omittedHistory = true
			case "omitted400":
				r.omitted400History = true
			case "compaction":
				r.disallowLearning = true
			case "quota_reset":
				body = []byte(`{"error":{"type":"rate_limit_error","resets_at":2000000000}}`)
			case "quota_header":
				resp.Header.Set("X-Codex-Primary-Used-Percent", "100")
				resp.Header.Set("X-Codex-Primary-Reset-After-Seconds", "3600")
				resp.Header.Set("X-Codex-Primary-Window-Minutes", "300")
			case "quota_code":
				body = []byte(`{"error":{"type":"rate_limit_error","code":"insufficient_quota"}}`)
			case "context_window":
				body = []byte(`{"error":{"type":"rate_limit_error","code":"context_length_exceeded"}}`)
			case "policy":
				body = []byte(`{"error":{"type":"rate_limit_error","code":"cyber_policy"}}`)
			case "access":
				body = []byte(`{"error":{"type":"rate_limit_error","code":"account_deactivated"}}`)
			case "unknown_code":
				body = []byte(`{"error":{"type":"rate_limit_error","code":"unrecognized_rejection"}}`)
			case "other_status":
				resp.StatusCode = 524
			case "committed":
				MarkResponseCommitted(c)
			case "canceled":
				ctx, cancel := context.WithCancel(c.Request.Context())
				cancel()
				c.Request = c.Request.WithContext(ctx)
			case "bare_service":
				s.accountRepo = nil
			}
			retry := s.newOpenAIAgentHTTP429RetryError(c.Request.Context(), c, a, req, resp, body)
			if change != "none" {
				require.Nil(t, retry)
				return
			}
			require.NotNil(t, retry)
			require.Equal(t, OpenAIAgentHTTP429RetryReason, retry.Reason)
			require.True(t, retry.RetryableOnSameAccount)
			require.False(t, retry.ShouldRetryNextAccount())
			require.False(t, retry.ShouldReportAccountScheduleFailure())
			require.Equal(t, 10, retry.SameAccountRetryMax)
			require.True(t, retry.SameAccountRetryDeadline.IsZero())
			require.EqualValues(t, 1, openAIAgentPinnedRetryAccountID(c.Request.Context()))
			// A subsequent response cannot reset or enlarge this request's budget.
			a.Credentials["pool_mode_retry_count"] = 20
			resp.Header.Set("Retry-After", "3600")
			next := s.newOpenAIAgentHTTP429RetryError(c.Request.Context(), c, a, req, resp, body)
			require.NotNil(t, next)
			require.Equal(t, 10, next.SameAccountRetryMax)
			require.Equal(t, 8*time.Second, next.SameAccountRetryDelay)
		})
	}
}

func TestOpenAIAgentHTTP429RetryWireFence(t *testing.T) {
	for _, change := range []string{"none", "account", "credentials", "url", "headers", "body"} {
		t.Run(change, func(t *testing.T) {
			s, c, a, req, resp := newAgentHTTP429Fixture(t)
			retry := s.newOpenAIAgentHTTP429RetryError(c.Request.Context(), c, a, req, resp, []byte(`{"error":{"type":"rate_limit_error"}}`))
			require.NotNil(t, retry)
			if change == "body" {
				var err error
				req, err = http.NewRequest(http.MethodPost, req.URL.String(), bytes.NewBufferString(issue715OpaqueBody+" "))
				require.NoError(t, err)
				req.Header.Set("Authorization", "Bearer fixture")
			}
			switch change {
			case "account":
				a.ID++
			case "credentials":
				a.Credentials["api_key"] = "fixture-rotated"
			case "url":
				req.URL.Host = "changed.example"
			case "headers":
				req.Header.Set("OpenAI-Project", "changed-project")
			}
			finish, err := s.prepareOpenAIAgentRecoveryAttempt(c.Request.Context(), a, req)
			finish(false)
			if change == "none" {
				require.NoError(t, err)
			} else {
				require.Error(t, err)
			}
		})
	}
}
