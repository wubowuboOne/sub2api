package apicompat

import (
	"encoding/json"
	"fmt"
	"testing"

	"github.com/stretchr/testify/require"
)

func TestChatCacheWritePresencePreservesLegacyUsage(t *testing.T) {
	for _, tc := range []struct {
		name  string
		field string
		want  int
	}{
		{"missing", "", 20},
		{"null", `,"cache_write_tokens":null`, 20},
		{"zero", `,"cache_write_tokens":0`, 0},
		{"positive", `,"cache_write_tokens":30`, 30},
	} {
		t.Run(tc.name, func(t *testing.T) {
			wire := fmt.Sprintf(`{"prompt_tokens":100,"completion_tokens":5,"prompt_tokens_details":{"cached_tokens":10,"cache_creation_tokens":20%s}}`, tc.field)
			var usage ChatUsage
			require.NoError(t, json.Unmarshal([]byte(wire), &usage))
			for _, roundTrip := range []bool{false, true} {
				if roundTrip {
					encoded, err := json.Marshal(usage)
					require.NoError(t, err)
					require.NoError(t, json.Unmarshal(encoded, &usage))
				}
				responses := ChatUsageToResponsesUsage(&usage)
				require.Equal(t, tc.want, responses.CacheCreationInputTokens, "round trip: %t", roundTrip)
				anthropic := ChatCompletionsResponseToAnthropic(&ChatCompletionsResponse{Usage: &usage}, "public-model")
				require.Equal(t, tc.want, anthropic.Usage.CacheCreationInputTokens, "round trip: %t", roundTrip)
				require.Equal(t, 90-tc.want, anthropic.Usage.InputTokens)
				require.Equal(t, 10, anthropic.Usage.CacheReadInputTokens)
			}
		})
	}
}
