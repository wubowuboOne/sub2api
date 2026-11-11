//go:build unit

package handler

import (
	"encoding/json"
	"errors"
	"net/http"
	"testing"

	"github.com/Wei-Shaw/sub2api/internal/pkg/logger"
	"github.com/Wei-Shaw/sub2api/internal/server/middleware"
	"github.com/Wei-Shaw/sub2api/internal/service"
	"github.com/stretchr/testify/require"
	"github.com/tidwall/gjson"
	"go.uber.org/zap"
	"go.uber.org/zap/zaptest/observer"
)

func TestOpenAIAgentRecoveryRefusalLogsOriginalOwner(t *testing.T) {
	for _, tc := range []struct {
		name, body, code, stage, reason string
		change                          func(*service.Account)
	}{
		{"performance_paused", issue715Body(true, true), "owner_unavailable", "session_selection", "account_performance_paused", func(a *service.Account) { a.PerformancePaused = true }},
		{"compaction_performance_paused", issue715CompactionBody(), "owner_unavailable", "session_selection", "account_performance_paused", func(a *service.Account) { a.PerformancePaused = true }},
		{"credentials", issue715Body(true, true), "route_changed", "account_check", "account_route_or_credentials_changed", func(a *service.Account) { a.Credentials["api_key"] = "private-rotated-key-sentinel" }},
	} {
		t.Run(tc.name, func(t *testing.T) {
			accounts := issue715AccountsOfType(service.AccountTypeAPIKey)
			upstream := &issue715Upstream{}
			h := newOpenAIResponsesFailoverTestHandler(t, upstream, accounts...)
			status, first := issue715Request(t, h, issue715Body(false, true), 99, "fixture-715-thread", false)
			require.Equal(t, http.StatusOK, status, first)
			tc.change(&accounts[0])
			c, rec := newIssue715Context(t, tc.body)
			c.Request.Header.Set("session_id", "fixture-715-thread")
			key, _ := c.Get(string(middleware.ContextKeyAPIKey))
			key.(*service.APIKey).ID = 99
			core, logs := observer.New(zap.WarnLevel)
			c.Request = c.Request.WithContext(logger.IntoContext(c.Request.Context(), zap.New(core).With(zap.String("request_id", "diagnostic-fixture"))))

			h.Responses(c)

			require.Equal(t, http.StatusConflict, rec.Code, rec.Body.String())
			require.Equal(t, "encrypted_agent_recovery_"+tc.code, gjson.Get(rec.Body.String(), "error.code").String())
			require.Equal(t, "invalid_request_error", gjson.Get(rec.Body.String(), "error.type").String())
			require.Equal(t, (&service.OpenAIAgentRecoveryError{Reason: tc.code}).ClientMessage(), gjson.Get(rec.Body.String(), "error.message").String())
			require.Equal(t, []int64{1}, upstream.ids, "diagnostic logging must not send, retry or switch the task")
			entries := logs.FilterMessage("openai.agent_recovery_refused").All()
			require.Len(t, entries, 1)
			fields := entries[0].ContextMap()
			require.EqualValues(t, 1, fields["original_account_id"])
			require.Equal(t, "diagnostic-fixture", fields["request_id"])
			require.Equal(t, tc.stage, fields["rejection_stage"])
			require.Equal(t, tc.reason, fields["rejection_reason"])
			require.EqualValues(t, http.StatusConflict, fields["status_code"])
			encoded, err := json.Marshal(fields)
			require.NoError(t, err)
			require.NotContains(t, string(encoded), "fixture-task-reference-715")
			require.NotContains(t, string(encoded), "private-rotated-key-sentinel")
			require.NotContains(t, string(encoded), "ledger balance")
			require.NotContains(t, rec.Body.String(), tc.reason, "internal diagnostic must stay out of client error")
			_, hasUpstreamEvents := c.Get(service.OpsUpstreamErrorsKey)
			require.False(t, hasUpstreamEvents, "a local refusal must not fabricate upstream attempts")
		})
	}
}

func TestOpenAIAgentRecoveryDiagnosticsIgnoreUnrelatedErrors(t *testing.T) {
	c, rec := newIssue715Context(t, issue715Body(false, false))
	core, logs := observer.New(zap.WarnLevel)
	c.Request = c.Request.WithContext(logger.IntoContext(c.Request.Context(), zap.New(core)))
	h := &OpenAIGatewayHandler{}
	require.False(t, h.handleOpenAIAgentRecoveryError(c, errors.New("ordinary failure"), false))
	require.Empty(t, logs.All())
	require.Empty(t, rec.Body.String())
	require.False(t, c.Writer.Written())
}
