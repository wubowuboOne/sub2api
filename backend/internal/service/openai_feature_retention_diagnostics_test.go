package service

import (
	"encoding/json"
	"errors"
	"net/http"
	"strings"
	"testing"

	"github.com/Wei-Shaw/sub2api/internal/pkg/logger"
	"github.com/stretchr/testify/require"
	"go.uber.org/zap"
	"go.uber.org/zap/zaptest/observer"
)

func TestOpenAIAgentRecoveryRetainsSharedStoreFailureDiagnostics(t *testing.T) {
	for _, mode := range []string{"lookup", "invalid"} {
		t.Run(mode, func(t *testing.T) {
			cache := &issue715SharedOwners{values: make(map[string]string)}
			svc, _, ctx, _, account := issue758WaitFixture(t, cache)
			const secret = "private-shared-store-error-sentinel"
			want := "shared_binding_lookup_failed"
			cache.mu.Lock()
			if mode == "lookup" {
				cache.readErr = errors.New(secret)
			} else {
				want = "shared_binding_invalid"
				for key := range cache.values {
					cache.values[key] = secret
				}
			}
			cache.mu.Unlock()
			req, err := http.NewRequest(http.MethodPost, "https://native.example/v1/responses", strings.NewReader(issue715OpaqueBody))
			require.NoError(t, err)
			req.Header.Set("Authorization", "Bearer fixture")
			_, err = svc.prepareOpenAIAgentRecoveryAttempt(ctx, account, req)
			var refused *OpenAIAgentRecoveryError
			require.ErrorAs(t, err, &refused)
			require.Equal(t, "store_unavailable", refused.Reason)
			require.Equal(t, "before_send", refused.RejectionStage)
			require.Equal(t, want, refused.RejectionReason)
			require.EqualValues(t, 1, refused.OriginalAccountID)
			core, logs := observer.New(zap.WarnLevel)
			refused.LogRefusal(logger.IntoContext(ctx, zap.New(core)))
			entries := logs.FilterMessage("openai.agent_recovery_refused").All()
			require.Len(t, entries, 1)
			require.Equal(t, want, entries[0].ContextMap()["rejection_reason"])
			encoded, err := json.Marshal(entries[0].ContextMap())
			require.NoError(t, err)
			require.NotContains(t, string(encoded), secret)
			client, err := json.Marshal(refused)
			require.NoError(t, err)
			require.NotContains(t, string(client), want)
			require.NotContains(t, refused.ClientMessage(), secret)
			require.NotEmpty(t, cache.values, "a diagnostic error must not erase task ownership")
		})
	}
}
