package service

import (
	"bytes"
	"context"
	"errors"
	"net/http"
	"strings"
	"testing"

	"github.com/Wei-Shaw/sub2api/internal/config"
	"github.com/stretchr/testify/require"
)

func TestIssue715NativeCompactionKeepsProvenOwner(t *testing.T) {
	for _, features := range [][]string{nil, {"remote_compaction_v2"}, {"responses_websockets_v2"}, {"feature_a, feature_b"}, {"feature_a", "feature_b"}} {
		t.Run("features="+strings.Join(features, ";"), func(t *testing.T) {
			svc := &OpenAIGatewayService{cfg: &config.Config{}}
			account := &Account{ID: 1, Platform: PlatformOpenAI, Type: AccountTypeAPIKey, Credentials: map[string]any{"api_key": "fixture"}, Extra: map[string]any{"openai_responses_supported": true}}
			c, _ := newTurnStateTestContext(t, 715, "fixture-session")
			c.Set("api_key", &APIKey{ID: 715, GroupID: int64Ptr(715)})
			ctx := svc.WithOpenAIAgentMessageRecovery(context.Background(), c, []byte(issue715OpaqueBody), "fixture-session", "gpt-6-astra")
			req, err := http.NewRequest(http.MethodPost, "https://native.example/v1/responses", strings.NewReader(issue715OpaqueBody))
			require.NoError(t, err)
			req.Header.Set("Authorization", "Bearer fixture")
			if len(features) > 0 {
				req.Header["X-Codex-Beta-Features"] = append([]string(nil), features...)
			}
			complete, err := svc.prepareOpenAIAgentRecoveryAttempt(ctx, account, req)
			require.NoError(t, err)
			complete(true)
			before := make(map[[32]byte]openAIAgentRecoveryBinding)
			for key, binding := range svc.agentMessageRecovery.bindings {
				before[key] = binding
			}
			body := strings.TrimSuffix(issue715OpaqueBody, "]}") + `,{"type":"compaction_trigger"}]}`
			MarkOpenAINativeCompactionV2(c)
			ctx = svc.WithOpenAIAgentMessageRecovery(context.Background(), c, []byte(body), "fixture-session", "gpt-6-astra")
			require.True(t, openAIAgentRecoveryBound(ctx))
			require.NoError(t, svc.checkOpenAIAgentRecoveryDispatch(ctx, c, account, []byte(body)), "native compaction does not change the established Responses route")
			req, err = http.NewRequest(http.MethodPost, req.URL.String(), bytes.NewBufferString(body))
			require.NoError(t, err)
			req.Header.Set("Authorization", "Bearer fixture")
			if len(features) > 0 {
				req.Header["X-Codex-Beta-Features"] = append([]string(nil), features...)
			}
			req = applyOpenAIAgentRecoveryBetaFeatures(ctx, c, account, req)
			complete, err = svc.prepareOpenAIAgentRecoveryAttempt(ctx, account, req)
			require.NoError(t, err, "the required compaction feature must not look like a credential change")
			complete(true)
			require.Nil(t, openAIAgentRecoveryFromContext(ctx).delivery, "compaction must not publish new agent ownership")
			require.Equal(t, before, svc.agentMessageRecovery.bindings)
		})
	}
}

func TestIssue715NativeCompactionStillChecksFinalWire(t *testing.T) {
	for _, change := range []string{"account", "credentials", "url", "authorization", "organization", "model", "task", "other_beta", "other_beta_before_build", "beta_removed_after_build", "missing_proof", "missing_body"} {
		t.Run(change, func(t *testing.T) {
			svc := &OpenAIGatewayService{cfg: &config.Config{}}
			account := &Account{ID: 1, Platform: PlatformOpenAI, Type: AccountTypeAPIKey, Credentials: map[string]any{"api_key": "fixture"}}
			issue715LearnRoute(t, svc, account, issue715OpaqueBody)
			c, _ := newTurnStateTestContext(t, 715, "fixture-session")
			c.Set("api_key", &APIKey{ID: 715, GroupID: int64Ptr(715)})
			MarkOpenAINativeCompactionV2(c)
			body := strings.TrimSuffix(issue715OpaqueBody, "]}") + `,{"type":"compaction_trigger"}]}`
			ctx := svc.WithOpenAIAgentMessageRecovery(context.Background(), c, []byte(body), "fixture-session", "gpt-6-astra")
			finalBody := body
			if change == "task" {
				finalBody = strings.ReplaceAll(body, "fixture-opaque-reference", "changed-task")
			}
			req, err := http.NewRequest(http.MethodPost, "https://native.example/v1/responses", strings.NewReader(finalBody))
			require.NoError(t, err)
			req.Header.Set("Authorization", "Bearer fixture")
			if change == "other_beta_before_build" {
				req.Header.Set("x-codex-beta-features", "other_feature")
			}
			req = applyOpenAIAgentRecoveryBetaFeatures(ctx, c, account, req)
			want := "wire_changed"
			switch change {
			case "account":
				account.ID++
				want = "owner_unavailable"
			case "credentials":
				account.Credentials["api_key"] = "rotated"
				want = "route_changed"
			case "url":
				req.URL.Host = "different.example"
			case "authorization":
				req.Header.Set("Authorization", "Bearer changed")
			case "organization":
				req.Header["openai-organization"] = []string{"changed-org"}
			case "model":
				req, err = http.NewRequest(http.MethodPost, req.URL.String(), strings.NewReader(strings.ReplaceAll(body, "gpt-6-astra", "other-model")))
				require.NoError(t, err)
				req.Header.Set("Authorization", "Bearer fixture")
				req = applyOpenAIAgentRecoveryBetaFeatures(ctx, c, account, req)
			case "other_beta":
				req.Header.Set("x-codex-beta-features", "other_feature,remote_compaction_v2")
			case "beta_removed_after_build":
				req.Header.Del("x-codex-beta-features")
			case "missing_proof":
				req = req.WithContext(context.Background())
			case "missing_body":
				req.GetBody = nil
			}
			_, err = svc.prepareOpenAIAgentRecoveryAttempt(ctx, account, req)
			var recovery *OpenAIAgentRecoveryError
			require.ErrorAs(t, err, &recovery)
			require.Equal(t, want, recovery.Reason)
		})
	}
}

func TestIssue715CompactionDoesNotAuthorizeOtherDispatches(t *testing.T) {
	for _, route := range []string{"legacy_compact", "chat_fallback", "passthrough"} {
		t.Run(route, func(t *testing.T) {
			svc := &OpenAIGatewayService{cfg: &config.Config{}}
			account := &Account{ID: 1, Platform: PlatformOpenAI, Type: AccountTypeAPIKey, Extra: map[string]any{"openai_responses_supported": true}}
			issue715LearnRoute(t, svc, account, issue715OpaqueBody)
			c, _ := newTurnStateTestContext(t, 715, "fixture-session")
			c.Set("api_key", &APIKey{ID: 715, GroupID: int64Ptr(715)})
			MarkOpenAINativeCompactionV2(c)
			switch route {
			case "legacy_compact":
				c.Request.URL.Path = "/v1/responses/compact"
			case "chat_fallback":
				account.Extra["openai_responses_supported"] = false
			case "passthrough":
				account.Extra["openai_passthrough"] = true
			}
			ctx := svc.WithOpenAIAgentMessageRecovery(context.Background(), c, []byte(issue715OpaqueBody), "fixture-session", "gpt-6-astra")
			var recovery *OpenAIAgentRecoveryError
			require.ErrorAs(t, svc.checkOpenAIAgentRecoveryDispatch(ctx, c, account, []byte(issue715OpaqueBody)), &recovery)
			require.Equal(t, "route_changed", recovery.Reason)
		})
	}
}

func TestIssue715UnknownCompactionDoesNotLearnOwner(t *testing.T) {
	svc := &OpenAIGatewayService{cfg: &config.Config{}}
	account := &Account{ID: 1, Platform: PlatformOpenAI, Type: AccountTypeAPIKey}
	c, _ := newTurnStateTestContext(t, 715, "fixture-session")
	c.Set("api_key", &APIKey{ID: 715, GroupID: int64Ptr(715)})
	MarkOpenAINativeCompactionV2(c)
	ctx := svc.WithOpenAIAgentMessageRecovery(context.Background(), c, []byte(issue715OpaqueBody), "fixture-session", "gpt-6-astra")
	req, err := http.NewRequest(http.MethodPost, "https://native.example/v1/responses", strings.NewReader(issue715OpaqueBody))
	require.NoError(t, err)
	complete, err := svc.prepareOpenAIAgentRecoveryAttempt(ctx, account, req)
	require.NoError(t, err)
	complete(true)
	require.Nil(t, openAIAgentRecoveryFromContext(ctx).delivery)
	require.Empty(t, svc.agentMessageRecovery.bindings)
}

func TestIssue715NativeCompactionRechecksSharedOwner(t *testing.T) {
	for _, state := range []string{"unchanged", "missing", "conflict", "unavailable"} {
		t.Run(state, func(t *testing.T) {
			cache := &issue715SharedOwners{}
			svc := &OpenAIGatewayService{cfg: &config.Config{}, cache: cache}
			account := &Account{ID: 1, Platform: PlatformOpenAI, Type: AccountTypeAPIKey}
			issue715LearnRoute(t, svc, account, issue715OpaqueBody)
			c, _ := newTurnStateTestContext(t, 715, "fixture-session")
			c.Set("api_key", &APIKey{ID: 715, GroupID: int64Ptr(715)})
			MarkOpenAINativeCompactionV2(c)
			ctx := svc.WithOpenAIAgentMessageRecovery(context.Background(), c, []byte(issue715OpaqueBody), "fixture-session", "gpt-6-astra")
			require.True(t, openAIAgentRecoveryBound(ctx))
			before := make(map[string]string)
			for key, value := range cache.values {
				before[key] = value
			}
			want := ""
			switch state {
			case "missing":
				cache.values = make(map[string]string)
				want = "owner_unavailable"
			case "conflict":
				for key := range cache.values {
					cache.values[key] = "conflict"
				}
				want = "owner_conflict"
			case "unavailable":
				cache.readErr = errors.New("fixture store unavailable")
				want = "store_unavailable"
			}
			req, err := http.NewRequest(http.MethodPost, "https://native.example/v1/responses", strings.NewReader(issue715OpaqueBody))
			require.NoError(t, err)
			req.Header.Set("Authorization", "Bearer fixture")
			req = applyOpenAIAgentRecoveryBetaFeatures(ctx, c, account, req)
			headers := req.Header.Clone()
			complete, err := svc.prepareOpenAIAgentRecoveryAttempt(ctx, account, req)
			if want == "" {
				require.NoError(t, err)
				complete(true)
				require.Equal(t, before, cache.values, "compaction must retain existing Redis records")
			} else {
				var recovery *OpenAIAgentRecoveryError
				require.ErrorAs(t, err, &recovery)
				require.Equal(t, want, recovery.Reason)
			}
			require.Equal(t, headers, req.Header, "comparison must not remove the outbound capability")
		})
	}
}

func TestIssue715NormalContinuationStillRequiresExactBetaFeatures(t *testing.T) {
	svc := &OpenAIGatewayService{cfg: &config.Config{}}
	account := &Account{ID: 1, Platform: PlatformOpenAI, Type: AccountTypeAPIKey}
	issue715LearnRoute(t, svc, account, issue715OpaqueBody)
	c, _ := newTurnStateTestContext(t, 715, "fixture-session")
	c.Set("api_key", &APIKey{ID: 715, GroupID: int64Ptr(715)})
	ctx := svc.WithOpenAIAgentMessageRecovery(context.Background(), c, []byte(issue715OpaqueBody), "fixture-session", "gpt-6-astra")
	req, err := http.NewRequest(http.MethodPost, "https://native.example/v1/responses", strings.NewReader(issue715OpaqueBody))
	require.NoError(t, err)
	req.Header.Set("Authorization", "Bearer fixture")
	req.Header.Set("x-codex-beta-features", "remote_compaction_v2")
	_, err = svc.prepareOpenAIAgentRecoveryAttempt(ctx, account, req)
	var recovery *OpenAIAgentRecoveryError
	require.ErrorAs(t, err, &recovery)
	require.Equal(t, "wire_changed", recovery.Reason)
}

func TestIssue715RecoveryBetaWrapperPreservesOrdinaryRequests(t *testing.T) {
	for _, mode := range []string{"no_recovery", "normal_bound", "unknown_compaction"} {
		t.Run(mode, func(t *testing.T) {
			svc := &OpenAIGatewayService{cfg: &config.Config{}}
			account := &Account{ID: 1, Platform: PlatformOpenAI, Type: AccountTypeAPIKey}
			c, _ := newTurnStateTestContext(t, 715, "fixture-session")
			c.Set("api_key", &APIKey{ID: 715, GroupID: int64Ptr(715)})
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			if mode == "normal_bound" {
				issue715LearnRoute(t, svc, account, issue715OpaqueBody)
			}
			if mode == "unknown_compaction" {
				MarkOpenAINativeCompactionV2(c)
			}
			if mode != "no_recovery" {
				ctx = svc.WithOpenAIAgentMessageRecovery(ctx, c, []byte(issue715OpaqueBody), "fixture-session", "gpt-6-astra")
			}
			req, err := http.NewRequestWithContext(ctx, http.MethodPost, "https://native.example/v1/responses", strings.NewReader(issue715OpaqueBody))
			require.NoError(t, err)
			req.Header.Set("Authorization", "Bearer fixture")
			req.Header.Set("x-codex-beta-features", "feature_a, feature_b")
			expected := req.Header.Clone()
			applyOpenAICodexBetaFeatures(c, account, expected)
			actual := applyOpenAIAgentRecoveryBetaFeatures(ctx, c, account, req)
			require.Same(t, req, actual, "ordinary requests must keep the existing request and context")
			require.Equal(t, expected, actual.Header)
			require.Nil(t, actual.Context().Value(openAIAgentRecoveryCompactionHeadersKey{}))
			cancel()
			require.ErrorIs(t, actual.Context().Err(), context.Canceled)
		})
	}
}

func TestIssue715NativeCompactionAcceptsClientCapability(t *testing.T) {
	svc := &OpenAIGatewayService{cfg: &config.Config{}}
	account := &Account{ID: 1, Platform: PlatformOpenAI, Type: AccountTypeAPIKey}
	issue715LearnRoute(t, svc, account, issue715OpaqueBody)
	c, _ := newTurnStateTestContext(t, 715, "fixture-session")
	c.Set("api_key", &APIKey{ID: 715, GroupID: int64Ptr(715)})
	MarkOpenAINativeCompactionV2(c)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	ctx = svc.WithOpenAIAgentMessageRecovery(ctx, c, []byte(issue715OpaqueBody), "fixture-session", "gpt-6-astra")
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, "https://native.example/v1/responses", strings.NewReader(issue715OpaqueBody))
	require.NoError(t, err)
	req.Header.Set("Authorization", "Bearer fixture")
	req.Header.Set("x-codex-beta-features", "remote_compaction_v2")
	req = applyOpenAIAgentRecoveryBetaFeatures(ctx, c, account, req)
	headers := req.Header.Clone()
	complete, err := svc.prepareOpenAIAgentRecoveryAttempt(ctx, account, req)
	require.NoError(t, err)
	complete(true)
	require.Equal(t, headers, req.Header)
	cancel()
	require.ErrorIs(t, req.Context().Err(), context.Canceled, "the proof must preserve upstream cancellation")
}
