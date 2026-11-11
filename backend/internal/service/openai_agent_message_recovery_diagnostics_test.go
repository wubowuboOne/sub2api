package service

import (
	"encoding/json"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/Wei-Shaw/sub2api/internal/pkg/logger"
	"github.com/stretchr/testify/require"
	"go.uber.org/zap"
	"go.uber.org/zap/zaptest/observer"
)

func TestOpenAIAgentRecoverySelectionDiagnostics(t *testing.T) {
	for _, tc := range []struct {
		name, stage, reason string
		change              func(*Account, *OpenAIAccountScheduleRequest)
	}{
		{"inactive", "session_selection", "account_inactive", func(a *Account, _ *OpenAIAccountScheduleRequest) { a.Status = StatusError }},
		{"performance", "session_selection", "account_performance_paused", func(a *Account, _ *OpenAIAccountScheduleRequest) { a.PerformancePaused = true }},
		{"expired", "session_selection", "account_expired", func(a *Account, _ *OpenAIAccountScheduleRequest) {
			past := time.Now().Add(-time.Hour)
			a.AutoPauseOnExpired, a.ExpiresAt = true, &past
		}},
		{"temporary_pause", "session_selection", "account_temp_unschedulable", func(a *Account, _ *OpenAIAccountScheduleRequest) {
			until := time.Now().Add(time.Hour)
			a.TempUnschedulableUntil = &until
		}},
		{"group_removed", "session_selection", "group_mismatch", func(a *Account, _ *OpenAIAccountScheduleRequest) { a.GroupIDs = []int64{716} }},
		{"model_removed", "session_selection", "model_not_supported", func(a *Account, _ *OpenAIAccountScheduleRequest) {
			a.Credentials["model_mapping"] = map[string]any{"other-model": "other-model"}
		}},
		{"excluded", "session_selection", "account_excluded", func(a *Account, r *OpenAIAccountScheduleRequest) { r.ExcludedIDs = map[int64]struct{}{a.ID: {}} }},
		{"guardian", "scheduler", "guardian_owner_mismatch", func(a *Account, r *OpenAIAccountScheduleRequest) { r.GuardianParentAccountID = a.ID + 1 }},
		{"missing_previous_owner", "scheduler", "previous_response_owner_missing", func(_ *Account, r *OpenAIAccountScheduleRequest) { r.PreviousResponseID = "resp_unknown" }},
		{"session", "scheduler", "session_scope_mismatch", func(_ *Account, r *OpenAIAccountScheduleRequest) { r.SessionHash = "other-session" }},
	} {
		t.Run(tc.name, func(t *testing.T) {
			svc, scheduler, ctx, req, account := issue758WaitFixture(t, nil)
			tc.change(account, &req)
			selection, _, err := scheduler.Select(ctx, req)
			require.Nil(t, selection)
			var refused *OpenAIAgentRecoveryError
			require.ErrorAs(t, err, &refused)
			require.Equal(t, "owner_unavailable", refused.Reason)
			require.EqualValues(t, 1, refused.OriginalAccountID)
			require.Equal(t, tc.stage, refused.RejectionStage)
			require.Equal(t, tc.reason, refused.RejectionReason)
			require.Equal(t, http.StatusConflict, refused.HTTPStatus())
			require.EqualError(t, err, (&OpenAIAgentRecoveryError{Reason: "owner_unavailable"}).Error())
			require.NotEmpty(t, svc.agentMessageRecovery.bindings, "diagnostics must not clear task ownership")
		})
	}
}

func TestOpenAIAgentRecoveryRetentionDiagnostics(t *testing.T) {
	for _, missing := range []bool{false, true} {
		t.Run(map[bool]string{false: "expired", true: "missing"}[missing], func(t *testing.T) {
			svc, _, ctx, _, _ := issue758WaitFixture(t, nil)
			request := openAIAgentRecoveryFromContext(ctx)
			for key, binding := range svc.agentMessageRecovery.bindings {
				if missing {
					delete(svc.agentMessageRecovery.bindings, key)
				} else {
					binding.expiresAt = time.Now().Add(-time.Second)
					svc.agentMessageRecovery.bindings[key] = binding
				}
			}
			var refused *OpenAIAgentRecoveryError
			require.ErrorAs(t, svc.retainOpenAIAgentRecovery(ctx, request, time.Hour), &refused)
			require.EqualValues(t, 1, refused.OriginalAccountID)
			require.Equal(t, "binding_retention", refused.RejectionStage)
			require.Equal(t, map[bool]string{false: "binding_expired", true: "binding_missing"}[missing], refused.RejectionReason)
			require.Equal(t, "encrypted_agent_recovery_owner_unavailable", refused.ClientCode())
		})
	}
}

func TestOpenAIAgentRecoveryDiagnosticLogDoesNotExposeTask(t *testing.T) {
	_, _, ctx, _, _ := issue758WaitFixture(t, nil)
	request := openAIAgentRecoveryFromContext(ctx)
	request.session = "private-session-sentinel"
	core, logs := observer.New(zap.WarnLevel)
	ctx = logger.IntoContext(ctx, zap.New(core).With(zap.String("request_id", "request-fixture"), zap.String("client_request_id", "client-fixture")))
	refused := request.refusal("owner_unavailable", "session_selection", "account_not_schedulable")
	refused.LogRefusal(ctx)
	entries := logs.FilterMessage("openai.agent_recovery_refused").All()
	require.Len(t, entries, 1)
	fields := entries[0].ContextMap()
	require.EqualValues(t, 1, fields["original_account_id"])
	require.EqualValues(t, 1, fields["account_id"])
	require.Equal(t, PlatformOpenAI, fields["platform"])
	require.Equal(t, "account_not_schedulable", fields["rejection_reason"])
	require.Equal(t, "request-fixture", fields["request_id"])
	require.Equal(t, "client-fixture", fields["client_request_id"])
	require.EqualValues(t, len(request.known), fields["known_item_count"])
	encoded, err := json.Marshal(fields)
	require.NoError(t, err)
	for _, private := range []string{request.session, "fixture-opaque-reference", "Authorization", "Bearer fixture", "fixture envelope", "encrypted_content"} {
		require.NotContains(t, string(encoded), private)
	}
	client, err := json.Marshal(refused)
	require.NoError(t, err)
	require.NotContains(t, string(client), "session_selection")
	require.NotContains(t, string(client), "account_not_schedulable")
	require.False(t, strings.Contains(refused.ClientMessage(), "original_account_id"))
}

func TestOpenAIAgentRecoveryBeforeSendMissingBindingDiagnostic(t *testing.T) {
	cache := &issue715SharedOwners{values: make(map[string]string)}
	svc, _, ctx, _, account := issue758WaitFixture(t, cache)
	cache.mu.Lock()
	clear(cache.values)
	cache.mu.Unlock()
	req, err := http.NewRequest(http.MethodPost, "https://native.example/v1/responses", strings.NewReader(issue715OpaqueBody))
	require.NoError(t, err)
	req.Header.Set("Authorization", "Bearer fixture")
	_, err = svc.prepareOpenAIAgentRecoveryAttempt(ctx, account, req)
	var refused *OpenAIAgentRecoveryError
	require.ErrorAs(t, err, &refused)
	require.EqualValues(t, 1, refused.OriginalAccountID)
	require.Equal(t, "before_send", refused.RejectionStage)
	require.Equal(t, "known_binding_missing", refused.RejectionReason)
	require.Equal(t, http.StatusConflict, refused.HTTPStatus())
}

func TestOpenAIAgentRecoveryFreshAccountDiagnostics(t *testing.T) {
	svc, _, ctx, req, account := issue758WaitFixture(t, nil)
	stale := *account
	account.Schedulable = false
	svc.schedulerSnapshot = &SchedulerSnapshotService{}
	selected, reason := svc.recheckSelectedOpenAIAccountFromDBWithReason(ctx, &stale, req.GroupID, req.Platform, req.RequestedModel, req.RequireCompact, req.RequiredCapability)
	require.Nil(t, selected)
	require.Equal(t, "account_not_schedulable", reason, "use the rejecting DB observation rather than the stale snapshot")
	require.Nil(t, svc.recheckSelectedOpenAIAccountFromDB(ctx, &stale, req.GroupID, req.Platform, req.RequestedModel, req.RequireCompact, req.RequiredCapability))
}

func TestOpenAIAgentRecoveryWebsocketTurnLogsRefusal(t *testing.T) {
	svc, _, ctx, _, account := issue758WaitFixture(t, nil)
	c, _ := newTurnStateTestContext(t, 715, "fixture-session")
	c.Set("api_key", &APIKey{ID: 715, GroupID: int64Ptr(715)})
	core, logs := observer.New(zap.WarnLevel)
	ctx = logger.IntoContext(ctx, zap.New(core).With(zap.String("request_id", "ws-diagnostic-fixture")))
	r := newOpenAIWSAgentRecovery(svc, ctx, c)
	wrongOwner := *account
	wrongOwner.ID++
	header := http.Header{"Authorization": {"Bearer fixture"}}
	err := r.beginSocket(&wrongOwner, []byte(issue715OpaqueBody), []byte(issue715OpaqueBody), "wss://native.example/v1/responses", header, "passthrough")
	var refused *OpenAIAgentRecoveryError
	require.ErrorAs(t, err, &refused)
	require.Equal(t, "owner_unavailable", refused.Reason)
	entries := logs.FilterMessage("openai.agent_recovery_refused").All()
	require.Len(t, entries, 1, "per-turn failures must be logged before the socket close wrapper")
	fields := entries[0].ContextMap()
	require.EqualValues(t, 1, fields["original_account_id"])
	require.Equal(t, "account_check", fields["rejection_stage"])
	require.Equal(t, "selected_owner_mismatch", fields["rejection_reason"])
	require.Equal(t, "ws-diagnostic-fixture", fields["request_id"])
	require.Nil(t, r.finish)
}
