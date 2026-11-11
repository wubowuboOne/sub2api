package service

import (
	"bytes"
	"context"
	"net/http"
	"strings"
	"testing"

	"github.com/Wei-Shaw/sub2api/internal/config"
	"github.com/stretchr/testify/require"
)

func TestIssue715NativeCompactionKeepsProvenOwner(t *testing.T) {
	for _, features := range [][]string{nil, {"remote_compaction_v2"}, {"responses_websockets_v2"}, {"feature_a,feature_b"}, {"feature_a", "remote_compaction_v2", "feature_b"}} {
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
			applyOpenAICodexBetaFeatures(c, account, req.Header)
			complete, err = svc.prepareOpenAIAgentRecoveryAttempt(ctx, account, req)
			require.NoError(t, err, "the required compaction feature must not look like a credential change")
			complete(true)
			require.Nil(t, openAIAgentRecoveryFromContext(ctx).delivery, "compaction must not publish new agent ownership")
			require.Equal(t, before, svc.agentMessageRecovery.bindings)
		})
	}
}

func TestIssue715NativeCompactionStillChecksFinalWire(t *testing.T) {
	for _, change := range []string{"account", "credentials", "url", "authorization", "organization", "model", "task", "other_beta", "other_beta_before_build", "missing_body"} {
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
			applyOpenAICodexBetaFeatures(c, account, req.Header)
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
				applyOpenAICodexBetaFeatures(c, account, req.Header)
			case "other_beta":
				req.Header.Set("x-codex-beta-features", "other_feature,remote_compaction_v2")
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
	applyOpenAICodexBetaFeatures(c, account, req.Header)
	headers := req.Header.Clone()
	complete, err := svc.prepareOpenAIAgentRecoveryAttempt(ctx, account, req)
	require.NoError(t, err)
	complete(true)
	require.Equal(t, headers, req.Header)
	cancel()
	require.ErrorIs(t, req.Context().Err(), context.Canceled, "upstream cancellation must be preserved without an injection proof")
}
