//go:build unit

package handler

import (
	"testing"

	"github.com/Wei-Shaw/sub2api/internal/service"
	"github.com/stretchr/testify/require"
	"github.com/tidwall/gjson"
)

func TestOpenAIResponsesWebSocket_APIKeyModelMappingPreservesEachTurn(t *testing.T) {
	for _, mode := range []string{service.OpenAIWSIngressModePassthrough, service.OpenAIWSIngressModeCtxPool} {
		t.Run(mode, func(t *testing.T) {
			got := runOpenAIResponsesWebSocketUsageLogCase(t, openAIResponsesWSUsageLogCase{
				firstPayload:  `{"type":"response.create","model":"sol","stream":false}`,
				secondPayload: `{"type":"response.create","model":"terra","stream":false}`,
				ingressMode:   mode,
				apiKeyModelMapping: map[string]string{
					"sol": "gpt-5.6-sol", "terra": "gpt-5.6-terra",
				},
				channelMapping: map[string]string{
					"gpt-5.6-terra": "gpt-5.6-sol",
				},
				accountModelMapping: map[string]any{"gpt-5.6-sol": "gpt-5.6-sol"},
			})
			require.Len(t, got.upstreamPayloads, 2)
			require.Len(t, got.clientEvents, 2)
			require.Len(t, got.logs, 2)
			for i, requested := range []string{"sol", "terra"} {
				require.Equal(t, "gpt-5.6-sol", gjson.GetBytes(got.upstreamPayloads[i], "model").String())
				require.Equal(t, requested, gjson.GetBytes(got.clientEvents[i], "response.model").String())
				require.Equal(t, requested, got.logs[i].RequestedModel)
			}
		})
	}
}
