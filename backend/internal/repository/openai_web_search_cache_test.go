package repository

import (
	"testing"

	"github.com/Wei-Shaw/sub2api/internal/service"
	"github.com/stretchr/testify/require"
)

func TestOpenAIWebSearchSchedulerProjectionAndInvalidation(t *testing.T) {
	probe := map[string]any{"supported": false, "route": "fingerprint", "model": "gpt-test"}
	extra := filterSchedulerExtra(map[string]any{service.OpenAIWebSearchModeKey: "only", "openai_web_search_probe": probe, "unrelated_secret": "discard"})
	require.Equal(t, "only", extra[service.OpenAIWebSearchModeKey])
	require.Equal(t, probe, extra["openai_web_search_probe"])
	require.NotContains(t, extra, "unrelated_secret")
	for _, key := range []string{service.OpenAIWebSearchModeKey, "openai_web_search_probe"} {
		require.True(t, shouldEnqueueSchedulerOutboxForExtraUpdates(map[string]any{key: true}), "search capability changes must invalidate scheduler snapshots")
	}
	credentials := filterSchedulerCredentials(map[string]any{"chatgpt_account_id": "account", "api_key": "sk-test", "base_url": "https://upstream.example"})
	require.NotContains(t, credentials, "chatgpt_account_id", "search must preserve the scheduler's credential identity boundary")
	require.Equal(t, "sk-test", credentials["api_key"])
	require.Equal(t, "https://upstream.example", credentials["base_url"])
}
