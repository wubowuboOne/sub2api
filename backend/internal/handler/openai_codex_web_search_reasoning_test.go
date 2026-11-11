package handler

import (
	"encoding/json"
	"net/http"
	"testing"

	"github.com/Wei-Shaw/sub2api/internal/config"
	"github.com/Wei-Shaw/sub2api/internal/service"
	"github.com/gin-gonic/gin"
	"github.com/stretchr/testify/require"
)

func TestCodexModelsReasoningUsesNormalMappedAccountInsteadOfSearchOnlyAccount(t *testing.T) {
	gin.SetMode(gin.TestMode)
	const groupID int64 = 93
	newAccount := func(id int64, mode, target string, levels []string) service.Account {
		a := service.Account{
			ID: id, Platform: service.PlatformOpenAI, Type: service.AccountTypeAPIKey,
			Status: service.StatusActive, Schedulable: true,
			Extra:       map[string]any{service.OpenAIWebSearchModeKey: mode},
			Credentials: map[string]any{"model_mapping": map[string]any{"public-coder": target}},
		}
		reasoning := true
		a.SetUpstreamModelMetadataSnapshot(service.UpstreamModelMetadataSnapshot{Models: map[string]service.UpstreamModelMetadata{
			target: {ID: target, Reasoning: &reasoning, SupportedReasoningLevels: levels, DefaultReasoningLevel: levels[0]},
		}})
		return a
	}
	normal := newAccount(2, service.OpenAIWebSearchModeDisabled, "gpt-6.1-sol", []string{"low", "high"})
	search := newAccount(1, service.OpenAIWebSearchModeOnly, "gpt-6-astra", []string{"ultra"})
	search.Priority = -10
	search.Credentials["model_mapping"].(map[string]any)["search-only-model"] = "gpt-6-astra"
	group := &service.Group{ID: groupID, Platform: service.PlatformOpenAI, WebSearchAccountID: search.ID}
	var firstETag string
	for _, accounts := range [][]service.Account{{search, normal}, {normal, search}} {
		upstream := &codexModelsFailoverHTTPUpstream{}
		gateway := service.NewOpenAIGatewayService(
			codexModelsFailoverAccountRepo{accounts: accounts}, nil,
			nil, nil, nil, nil, nil, nil, &config.Config{RunMode: config.RunModeSimple}, nil, nil, nil, nil, nil,
			upstream, nil, nil, nil, nil, nil, nil, nil, nil,
		)
		handler := &OpenAIGatewayHandler{gatewayService: gateway}
		response := performCodexModelsRequestForGroup(t, handler, group, "")
		require.Equal(t, http.StatusOK, response.Code)
		var envelope struct {
			Models []struct {
				Slug    string                       `json:"slug"`
				Default string                       `json:"default_reasoning_level"`
				Levels  []codexReasoningLevelForTest `json:"supported_reasoning_levels"`
			} `json:"models"`
		}
		require.NoError(t, json.Unmarshal(response.Body.Bytes(), &envelope))
		require.Len(t, envelope.Models, 1)
		require.Equal(t, "public-coder", envelope.Models[0].Slug)
		require.Equal(t, "low", envelope.Models[0].Default)
		var efforts []string
		for _, level := range envelope.Models[0].Levels {
			efforts = append(efforts, level.Effort)
		}
		require.Equal(t, []string{"low", "high"}, efforts)
		etag := response.Header().Get("ETag")
		require.NotEmpty(t, etag)
		if firstETag == "" {
			firstETag = etag
		}
		require.Equal(t, firstETag, etag)
		cached := performCodexModelsRequestForGroup(t, handler, group, etag)
		require.Equal(t, http.StatusNotModified, cached.Code)
		require.Empty(t, cached.Body.String())
		require.Empty(t, upstream.calls(), "configured discovery must not call the search upstream")
	}
}
