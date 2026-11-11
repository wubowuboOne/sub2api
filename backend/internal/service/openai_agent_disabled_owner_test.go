package service

import (
	"bytes"
	"context"
	"net/http"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
)

func TestOpenAIAgentDisabledOwnerReleaseConditions(t *testing.T) {
	for _, tc := range []struct {
		name    string
		change  func(*Account)
		release bool
	}{
		{"active", func(*Account) {}, false},
		{"scheduling_disabled", func(a *Account) { a.Schedulable = false }, true},
		{"status_inactive", func(a *Account) { a.Status = "inactive" }, true},
		{"status_disabled", func(a *Account) { a.Status = StatusDisabled }, true},
		{"performance_pause", func(a *Account) { a.PerformancePaused = true }, false},
		{"rate_limit", func(a *Account) { until := time.Now().Add(time.Hour); a.RateLimitResetAt = &until }, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			svc, _, ctx, _, account := issue758WaitFixture(t, nil)
			request := openAIAgentRecoveryFromContext(ctx)
			tc.change(account)
			require.NoError(t, svc.releaseDisabledOpenAIAgentOwner(ctx, request))
			if !tc.release {
				require.NotNil(t, request.owner)
				require.Zero(t, request.releasedOwnerID)
				require.NotEmpty(t, svc.agentMessageRecovery.bindings)
				return
			}
			require.Nil(t, request.owner)
			require.EqualValues(t, 1, request.releasedOwnerID)
			require.Empty(t, request.known)
			require.Empty(t, svc.agentMessageRecovery.bindings)
			require.True(t, openAIAgentRecoveryBound(ctx), "releasing affinity must retain request integrity and replay protections")
		})
	}
}

func TestOpenAIAgentDisabledOwnerPreservesConcurrentBinding(t *testing.T) {
	for _, changed := range []string{"pending", "failed", "conflict", "other_owner"} {
		t.Run(changed, func(t *testing.T) {
			svc, _, ctx, _, account := issue758WaitFixture(t, nil)
			request := openAIAgentRecoveryFromContext(ctx)
			account.Schedulable = false
			for key, binding := range svc.agentMessageRecovery.bindings {
				switch changed {
				case "pending":
					binding.pending = "pending:other-delivery"
				case "failed":
					binding.failed = true
				case "conflict":
					binding.conflicted = true
				case "other_owner":
					binding.accountID = 2
				}
				svc.agentMessageRecovery.bindings[key] = binding
			}
			var refused *OpenAIAgentRecoveryError
			require.ErrorAs(t, svc.releaseDisabledOpenAIAgentOwner(ctx, request), &refused)
			require.Equal(t, "binding_changed_before_release", refused.RejectionReason)
			require.EqualValues(t, 1, refused.OriginalAccountID)
			require.NotNil(t, request.owner)
			require.NotEmpty(t, svc.agentMessageRecovery.bindings)
		})
	}
}

func TestOpenAIAgentDisabledOwnerKeepsIntactHistoryAndRetryProtection(t *testing.T) {
	svc, _, ctx, _, old := issue758WaitFixture(t, nil)
	request := openAIAgentRecoveryFromContext(ctx)
	old.Schedulable = false
	request.toolResult = true
	require.NoError(t, svc.releaseDisabledOpenAIAgentOwner(ctx, request))
	require.True(t, OpenAIAgentToolResultContinuation(ctx))
	next := *old
	next.ID, next.Schedulable = 2, true
	next.Credentials = map[string]any{"api_key": "fixture-replacement"}
	for _, body := range []string{`{"model":"gpt-6-astra","input":[]}`, issue715OpaqueBody} {
		req, err := http.NewRequest(http.MethodPost, "https://replacement.example/v1/responses", bytes.NewBufferString(body))
		require.NoError(t, err)
		req.Header.Set("Authorization", "Bearer fixture-replacement")
		complete, err := svc.prepareOpenAIAgentRecoveryAttempt(ctx, &next, req)
		if body != issue715OpaqueBody {
			var refused *OpenAIAgentRecoveryError
			require.ErrorAs(t, err, &refused)
			require.Equal(t, "agent_item_count_changed", refused.RejectionReason)
			continue
		}
		require.NoError(t, err)
		complete(true)
	}
	c, _ := newTurnStateTestContext(t, 715, "fixture-session")
	c.Set("api_key", &APIKey{ID: 715, GroupID: int64Ptr(715)})
	nextCtx := svc.WithOpenAIAgentMessageRecovery(context.Background(), c, []byte(issue715OpaqueBody), "fixture-session", "gpt-6-astra")
	require.EqualValues(t, 2, openAIAgentRecoveryFromContext(nextCtx).owner.accountID)
}

func TestOpenAIAgentDisabledOwnerWebsocketKeepsIntegrityProtection(t *testing.T) {
	svc, _, ctx, _, account := issue758WaitFixture(t, nil)
	request := openAIAgentRecoveryFromContext(ctx)
	account.Schedulable = false
	require.NoError(t, svc.releaseDisabledOpenAIAgentOwner(ctx, request))
	c, _ := newTurnStateTestContext(t, 715, "fixture-session")
	c.Set("api_key", &APIKey{ID: 715, GroupID: int64Ptr(715)})
	ctx = svc.WithOpenAIWSAgentRecovery(ctx, c, []byte(issue715OpaqueBody), "fixture-session", "gpt-6-astra")
	manager := newOpenAIWSAgentRecovery(svc, ctx, c)
	defer manager.abort()
	next := *account
	next.ID, next.Schedulable = 2, true
	require.NoError(t, issue715WSBegin(t, manager, &next, issue715OpaqueBody, "passthrough"))
	require.EqualValues(t, 1, manager.request.releasedOwnerID)
	require.False(t, OpenAIWSAgentRecoveryCanRetry(ctx))

	req, err := http.NewRequest(http.MethodPost, "https://native.example/v1/responses", bytes.NewBufferString(`{"model":"gpt-6-astra","input":[]}`))
	require.NoError(t, err)
	err = manager.begin(&next, []byte(issue715OpaqueBody), []byte(issue715OpaqueBody), req, "passthrough")
	var refused *OpenAIAgentRecoveryError
	require.ErrorAs(t, err, &refused)
	require.Equal(t, "agent_item_count_changed", refused.RejectionReason)
}
