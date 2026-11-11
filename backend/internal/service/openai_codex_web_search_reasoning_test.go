package service

import (
	"context"
	"testing"

	"github.com/stretchr/testify/require"
)

func codexReasoningCatalogAccount(id int64, priority int, mode, target string, levels []string) Account {
	a := Account{
		ID: id, Priority: priority, Platform: PlatformOpenAI, Type: AccountTypeAPIKey,
		Extra:       map[string]any{OpenAIWebSearchModeKey: mode},
		Credentials: map[string]any{"model_mapping": map[string]any{"public-coder": target}},
	}
	if levels != nil {
		reasoning := true
		a.SetUpstreamModelMetadataSnapshot(UpstreamModelMetadataSnapshot{Models: map[string]UpstreamModelMetadata{
			target: {ID: target, Reasoning: &reasoning, SupportedReasoningLevels: levels, DefaultReasoningLevel: levels[len(levels)-1]},
		}})
	}
	return a
}

func TestCodexCatalogReasoningExcludesWebSearchOnlyAccounts(t *testing.T) {
	for _, mode := range []string{OpenAIWebSearchModeDefault, OpenAIWebSearchModeDisabled} {
		for _, synced := range []bool{false, true} {
			name := mode + "/fallback"
			var levels []string
			if synced {
				name = mode + "/synced"
				levels = []string{"low", "high", "max"}
			}
			t.Run(name, func(t *testing.T) {
				normal := codexReasoningCatalogAccount(2, 10, mode, "gpt-6.1-sol", levels)
				search := codexReasoningCatalogAccount(1, -10, OpenAIWebSearchModeOnly, "gpt-6-astra", []string{"ultra"})
				search.Credentials["model_mapping"].(map[string]any)["search-only-model"] = "gpt-6-astra"
				want := newConfiguredCodexModelDescriptor("gpt-6.1-sol")
				if synced {
					metadata, ok := normal.GetUpstreamModelMetadata("gpt-6.1-sol")
					require.True(t, ok)
					applyUpstreamModelMetadataToCodexDescriptor(&want, codexModelMetadataOverride{UpstreamModelMetadata: metadata})
				}
				var wantLevels []string
				for _, level := range want.SupportedReasoningLevels {
					wantLevels = append(wantLevels, level.Effort)
				}
				for _, accounts := range [][]Account{{search, normal}, {normal, search}} {
					repo := codexModelsVisibilityAccountRepo{byGroup: map[int64][]Account{91: accounts}}
					group := &Group{ID: 91, Platform: PlatformOpenAI, WebSearchAccountID: search.ID}
					svc := &OpenAIGatewayService{accountRepo: repo}
					manifest, configured, err := svc.BuildGroupConfiguredCodexModelsManifest(context.Background(), group, "")
					require.NoError(t, err)
					require.True(t, configured)
					models := decodeCodexManifestModels(t, manifest.Body)
					require.Equal(t, 1, len(models), "a search-only account must not publish normal model aliases")
					require.Equal(t, "public-coder", models[0]["slug"])
					require.Equal(t, wantLevels, effortsFromManifestModel(t, models[0]))
					require.Equal(t, *want.DefaultReasoningLevel, models[0]["default_reasoning_level"])
					cached, _, err := svc.BuildGroupConfiguredCodexModelsManifest(context.Background(), group, manifest.ETag)
					require.NoError(t, err)
					require.True(t, cached.NotModified)
					gateway := &GatewayService{accountRepo: repo}
					require.Equal(t, []string{"public-coder"}, gateway.GetAvailableModels(context.Background(), &group.ID, PlatformOpenAI))
					strictModels, err := gateway.GetGroupModelCatalog(context.Background(), group.ID, PlatformOpenAI)
					require.NoError(t, err)
					require.Equal(t, []string{"public-coder"}, strictModels)
				}
			})
		}
	}
}

func TestCodexCatalogReasoningSelectsOneNormalAccount(t *testing.T) {
	primary := codexReasoningCatalogAccount(20, 0, OpenAIWebSearchModeDisabled, "gpt-6.1-sol", []string{"low", "high"})
	secondary := codexReasoningCatalogAccount(10, 10, OpenAIWebSearchModeDefault, "gpt-6-luna", []string{"high", "max"})
	search := codexReasoningCatalogAccount(1, -10, OpenAIWebSearchModeOnly, "gpt-6-astra", []string{"ultra"})
	for _, platform := range []string{PlatformOpenAI, PlatformComposite} {
		for _, accounts := range [][]Account{{search, secondary, primary}, {primary, secondary, search}} {
			body, err := buildCodexModelsManifestForAccounts(platform, []string{"public-coder"}, accounts, nil, nil, true)
			require.NoError(t, err)
			model := decodeCodexManifestModels(t, body)[0]
			require.Equal(t, []string{"low", "high"}, effortsFromManifestModel(t, model))
			require.Equal(t, "high", model["default_reasoning_level"])
		}
	}
	primary.Priority = secondary.Priority
	body, err := buildCodexModelsManifestForAccounts(PlatformOpenAI, []string{"public-coder"}, []Account{primary, secondary}, nil, nil, true)
	require.NoError(t, err)
	require.Equal(t, []string{"high", "max"}, effortsFromManifestModel(t, decodeCodexManifestModels(t, body)[0]))
}

func TestCodexCatalogReasoningHonorsGroupAccountRestrictions(t *testing.T) {
	primary := codexReasoningCatalogAccount(1, 0, OpenAIWebSearchModeDisabled, "gpt-6.1-sol", []string{"low", "high"})
	secondary := codexReasoningCatalogAccount(2, 10, OpenAIWebSearchModeDefault, "gpt-6-luna", []string{"high", "max"})
	group := &Group{ID: 92, Platform: PlatformOpenAI, ModelRoutingEnabled: true, ModelRouting: map[string][]int64{"public-coder": {secondary.ID}}}
	for _, restriction := range []string{"routing", "group_models"} {
		t.Run(restriction, func(t *testing.T) {
			a := primary
			g := *group
			if restriction == "group_models" {
				g.ModelRoutingEnabled = false
				a.AccountGroups = []AccountGroup{{GroupID: g.ID, AllowedModels: []string{"another-model"}}}
			}
			body, err := buildCodexModelsManifestForAccounts(PlatformOpenAI, []string{"public-coder"}, []Account{a, secondary}, &g, nil, true)
			require.NoError(t, err)
			require.Equal(t, []string{"high", "max"}, effortsFromManifestModel(t, decodeCodexManifestModels(t, body)[0]))
		})
	}
}

func TestGatewayModelsSearchOnlyAccountsHaveEmptyCatalog(t *testing.T) {
	search := codexReasoningCatalogAccount(1, 0, OpenAIWebSearchModeOnly, "gpt-6-astra", nil)
	for _, platform := range []string{"", PlatformOpenAI} {
		models := availableModelsFromAccounts([]Account{search}, nil, platform)
		require.NotNil(t, models, "an empty normal catalog must not fall back to default models")
		require.Empty(t, models)
	}
	repo := codexModelsVisibilityAccountRepo{byGroup: map[int64][]Account{91: {search}}}
	gateway := &GatewayService{accountRepo: repo}
	for _, platform := range []string{PlatformOpenAI, PlatformComposite} {
		models, err := gateway.GetGroupModelCatalog(context.Background(), 91, platform)
		require.NoError(t, err)
		require.Empty(t, models)
	}
}

func TestCodexCatalogReasoningKeepsSelectedAccountNoReasoningAndUnknownLevels(t *testing.T) {
	for _, reasoning := range []bool{false, true} {
		primary := codexReasoningCatalogAccount(1, 0, OpenAIWebSearchModeDefault, "gpt-6-astra", nil)
		primary.SetUpstreamModelMetadataSnapshot(UpstreamModelMetadataSnapshot{Models: map[string]UpstreamModelMetadata{
			"gpt-6-astra": {ID: "gpt-6-astra", Reasoning: &reasoning},
		}})
		secondary := codexReasoningCatalogAccount(2, 0, OpenAIWebSearchModeDefault, "gpt-6-luna", []string{"low", "high"})
		body, err := buildCodexModelsManifestForAccounts(PlatformOpenAI, []string{"public-coder"}, []Account{secondary, primary}, nil, nil, true)
		require.NoError(t, err)
		model := decodeCodexManifestModels(t, body)[0]
		if reasoning {
			require.Empty(t, effortsFromManifestModel(t, model))
			require.NotContains(t, model, "default_reasoning_level")
		} else {
			require.Equal(t, []string{"none"}, effortsFromManifestModel(t, model))
			require.Equal(t, "none", model["default_reasoning_level"])
		}
	}
}
