package service

import (
	"context"
	"fmt"
	"os"
	"testing"
	"time"

	"github.com/Wei-Shaw/sub2api/internal/config"
	"github.com/Wei-Shaw/sub2api/internal/pkg/openai"
	"github.com/stretchr/testify/require"
)

func TestGPT62ModelIdentityAndReasoning(t *testing.T) {
	for _, model := range []string{"gpt-6.2-astra", "gpt-6.2-astra-pro", "gpt-6.2-sol"} {
		for _, suffix := range []string{"", "-max", "-2026-11-16", "-openai-compact"} {
			alias := "openai/" + model + suffix
			require.Equal(t, model, normalizeOpenAIGPT62Model(alias))
			require.Equal(t, model, normalizeCodexModel(alias))
			require.Equal(t, "max", normalizeOpenAIReasoningEffortForModel("max", alias))
		}
		require.Contains(t, openai.DefaultModelIDs(), model)
		descriptor := newConfiguredCodexModelDescriptor(model)
		require.Equal(t, model, descriptor.Slug)
		var levels []string
		for _, level := range descriptor.SupportedReasoningLevels {
			levels = append(levels, level.Effort)
		}
		require.Equal(t, []string{"low", "medium", "high", "xhigh", "max"}, levels)
	}
	for _, unknown := range []string{"gpt-6.2-astral", "gpt-6.2-astra-prototype", "gpt-6.2-astra-pro-preview", "gpt-6.2-solitude", "gpt-6.20-astra"} {
		require.Empty(t, normalizeOpenAIGPT62Model(unknown), unknown)
	}
}

func TestGPT62DefaultPricingAndCacheTTL(t *testing.T) {
	data, err := os.ReadFile("../../resources/model-pricing/model_prices_and_context_window.json")
	require.NoError(t, err)
	catalog := &PricingService{}
	catalog.pricingData, err = catalog.parsePricingData(data)
	require.NoError(t, err)
	for source, svc := range map[string]*BillingService{
		"builtin":      NewBillingService(&config.Config{}, nil),
		"empty_remote": NewBillingService(&config.Config{}, &PricingService{}),
		"bundled":      NewBillingService(&config.Config{}, catalog),
	} {
		for _, tc := range []struct {
			model                                string
			input, output, write, write12h, read float64
		}{
			{"gpt-6.2-astra", 8e-6, 40e-6, 10e-6, 20e-6, 0.4e-6},
			{"gpt-6.2-astra-pro", 8e-6, 40e-6, 10e-6, 20e-6, 0.4e-6},
			{"gpt-6.2-sol", 2e-6, 10e-6, 2.5e-6, 5e-6, 0.1e-6},
		} {
			t.Run(source+"/"+tc.model, func(t *testing.T) {
				for _, model := range []string{tc.model, "openai/" + tc.model + "-max"} {
					price, err := svc.GetModelPricing(model)
					require.NoError(t, err)
					require.NotNil(t, price.CacheCreation12hPricePerToken)
					require.InDelta(t, tc.write12h, *price.CacheCreation12hPricePerToken, 1e-12)
					tokens := UsageTokens{InputTokens: 269000, OutputTokens: 500, CacheReadTokens: 2000, CacheCreationTokens: 1000, CacheCreation30mTokens: 400, CacheCreation12hTokens: 600}
					for tier, multiplier := range map[string]float64{"": 1, "priority": 2, "flex": 0.5, "ultrafast": 6} {
						cost, err := svc.CalculateCostUnified(CostInput{Model: model, Tokens: tokens, RateMultiplier: 1, ServiceTier: tier, PricingAt: time.Date(2027, time.March, 1, 8, 0, 0, 0, time.UTC)})
						require.NoError(t, err)
						require.InDelta(t, 269000*tc.input*multiplier, cost.InputCost, 1e-10)
						require.InDelta(t, 500*tc.output*multiplier, cost.OutputCost, 1e-10)
						require.InDelta(t, (400*tc.write+600*tc.write12h)*multiplier, cost.CacheCreationCost, 1e-10)
						require.InDelta(t, 2000*tc.read*multiplier, cost.CacheReadCost, 1e-10)
						require.False(t, cost.LongContextBillingApplied)
					}
				}
			})
		}
	}
}

func TestGPT62PricingPreservesIndependentOverrides(t *testing.T) {
	catalog := &PricingService{}
	var err error
	catalog.pricingData, err = catalog.parsePricingData([]byte(`{"gpt-6.2-astra":{"input_cost_per_token":0.000003,"output_cost_per_token":0.000004},"gpt-6.2-astra-pro":{"input_cost_per_token":0.000007,"output_cost_per_token":0.000008,"cache_creation_input_token_cost":0,"cache_creation_input_token_cost_12h":0}}`))
	require.NoError(t, err)
	svc := NewBillingService(&config.Config{}, catalog)
	pro, err := svc.GetModelPricing("openai/gpt-6.2-astra-pro-max")
	require.NoError(t, err)
	require.InDelta(t, 7e-6, pro.InputPricePerToken, 1e-12)
	require.InDelta(t, 8e-6, pro.OutputPricePerToken, 1e-12)
	require.Zero(t, pro.CacheCreationPricePerToken)
	require.NotNil(t, pro.CacheCreation12hPricePerToken)
	require.Zero(t, *pro.CacheCreation12hPricePerToken)
	sol, err := svc.GetModelPricing("gpt-6.2-sol")
	require.NoError(t, err)
	require.InDelta(t, 2e-6, sol.InputPricePerToken, 1e-12)
	zero := 0.0
	custom, err := svc.GetModelPricingWithChannel("gpt-6.2-sol", &ChannelModelPricing{InputPrice: &zero, OutputPrice: &zero, CacheReadPrice: &zero, CacheWritePrice: &zero, CacheWrite12hPrice: &zero})
	require.NoError(t, err)
	require.Zero(t, custom.InputPricePerToken)
	require.Zero(t, custom.OutputPricePerToken)
	require.Zero(t, custom.CacheReadPricePerToken)
	require.Zero(t, custom.CacheCreationPricePerToken)
	require.Zero(t, *custom.CacheCreation12hPricePerToken)
}

func TestGPT62PricingAtRequestTime(t *testing.T) {
	before := time.Date(2027, 3, 1, 7, 59, 59, 0, time.UTC)
	after := before.Add(time.Second)
	for _, model := range []string{"gpt-6.2-sol", "gpt-6.2-astra"} {
		unit := 2e-6
		if model == "gpt-6.2-astra" {
			unit = 8e-6
		}
		for name, svc := range map[string]*BillingService{"fallback": newTestBillingService(), "catalog fallback": NewBillingService(nil, newStubPricingServiceFromJSON(t, `{"other":{"input_cost_per_token":1}}`))} {
			t.Run(model+name, func(t *testing.T) {
				for _, resolver := range []*ModelPricingResolver{nil, NewModelPricingResolver(nil, svc)} {
					enabled := true
					input := CostInput{Model: model, Tokens: UsageTokens{InputTokens: 100, OutputTokens: 50, CacheCreationTokens: 40, CacheReadTokens: 20}, RateMultiplier: 1, PricingAt: before, Resolver: resolver, LongContextBillingEnabled: &enabled}
					for _, tc := range []struct {
						tier  string
						at    time.Time
						ratio float64
					}{{"", before, 1}, {"priority", before, 2}, {"flex", before, 0.5}, {"ultrafast", before, 3}, {"ultrafast", after, 6}, {"ultrafast", before, 3}} {
						input.ServiceTier = tc.tier
						input.PricingAt = tc.at
						cost, err := svc.CalculateCostUnified(input)
						require.NoError(t, err)
						require.InDelta(t, 100*unit*tc.ratio, cost.InputCost, 1e-12)
						require.InDelta(t, 50*unit*5*tc.ratio, cost.OutputCost, 1e-12)
						require.InDelta(t, 40*unit*1.25*tc.ratio, cost.CacheCreationCost, 1e-12)
						require.InDelta(t, 20*unit*0.05*tc.ratio, cost.CacheReadCost, 1e-12)
					}
					input.ServiceTier = ""
					input.Tokens = UsageTokens{InputTokens: 272000, OutputTokens: 50}
					cost, err := svc.CalculateCostUnified(input)
					require.NoError(t, err)
					require.InDelta(t, 272000*unit, cost.InputCost, 1e-12)
					input.Tokens.InputTokens++
					cost, err = svc.CalculateCostUnified(input)
					require.NoError(t, err)
					require.InDelta(t, 272001*unit*2, cost.InputCost, 1e-12)
					require.InDelta(t, 50*unit*5*1.5, cost.OutputCost, 1e-12)
					for _, kind := range []string{AccountTypeOAuth, AccountTypeAPIKey} {
						input.DisableModelLongContextPricing = openAIGPT62SubscriptionLongContextExempt(&Account{Platform: PlatformOpenAI, Type: kind}, &OpenAIForwardResult{Model: "public", UpstreamModel: model})
						cost, err = svc.CalculateCostUnified(input)
						require.NoError(t, err)
						factor := 2.0
						if kind == AccountTypeOAuth {
							factor = 1
						}
						require.InDelta(t, 272001*unit*factor, cost.InputCost, 1e-12)
					}
				}
				// Request-specific calculation must not mutate shared fallback pricing.
				require.Zero(t, svc.fallbackPrices[model].UltrafastMultiplier)
			})
		}
	}
	for _, model := range []string{"gpt-6-astra", "gpt-6.1-sol", "gpt-6.2-astra-pro"} {
		require.Equal(t, 6.0, openAIUltrafastMultiplier(model, before))
		require.Equal(t, 6.0, openAIUltrafastMultiplier(model, after))
	}
}

func TestGPT62RecordUsageUsesRequestPricingAt(t *testing.T) {
	for _, withResolver := range []bool{false, true} {
		for _, at := range []time.Time{time.Date(2027, 3, 1, 7, 59, 59, 0, time.UTC), time.Date(2027, 3, 1, 8, 0, 0, 0, time.UTC)} {
			repo := &openAIRecordUsageLogRepoStub{inserted: true}
			svc := newOpenAIRecordUsageServiceForTest(repo, &openAIRecordUsageUserRepoStub{}, &openAIRecordUsageSubRepoStub{}, nil)
			if withResolver {
				svc.resolver = NewModelPricingResolver(nil, svc.billingService)
			} else {
				svc.resolver = nil
			}
			tier := "ultrafast"
			err := svc.RecordUsage(context.Background(), &OpenAIRecordUsageInput{
				Result: &OpenAIForwardResult{RequestID: fmt.Sprint(at.Unix(), withResolver), Model: "gpt-6.2-sol", UpstreamModel: "gpt-6.2-sol", ServiceTier: &tier, Usage: OpenAIUsage{InputTokens: 1000, OutputTokens: 500}},
				APIKey: &APIKey{ID: 1007, GroupID: i64p(17), Group: &Group{ID: 17, RateMultiplier: 1, SubscriptionType: SubscriptionTypeSubscription}},
				User:   &User{ID: 2007}, Account: &Account{ID: 3007, Platform: PlatformOpenAI, Type: AccountTypeAPIKey}, PricingAt: at,
			})
			require.NoError(t, err)
			require.NotNil(t, repo.lastLog)
			require.InDelta(t, (1000*2e-6+500*10e-6)*openAIUltrafastMultiplier("gpt-6.2-sol", at), repo.lastLog.TotalCost, 1e-12)
			require.WithinDuration(t, time.Now(), repo.lastLog.CreatedAt, time.Minute)
		}
	}
}

func TestGPT62OfficialUsageTierAndContext(t *testing.T) {
	for _, model := range []string{"gpt-6.2-sol", "gpt-6.2-astra"} {
		unit := 2e-6
		if model == "gpt-6.2-astra" {
			unit = 8e-6
		}
		for _, kind := range []string{AccountTypeAPIKey, AccountTypeOAuth, AccountTypeSetupToken} {
			for _, stamp := range []struct {
				at     time.Time
				factor float64
			}{
				{time.Date(2027, 3, 1, 7, 59, 59, 0, time.UTC), 3},
				{time.Date(2027, 3, 1, 8, 0, 0, 0, time.UTC), 6},
			} {
				t.Run(fmt.Sprintf("%s/%s/%g", model, kind, stamp.factor), func(t *testing.T) {
					repo := &openAIRecordUsageLogRepoStub{inserted: true}
					userRepo := &openAIRecordUsageUserRepoStub{}
					svc := newOpenAIRecordUsageServiceForTest(repo, userRepo, &openAIRecordUsageSubRepoStub{}, nil)
					key := openAIRecordUsageAPIKeyWithGroup(svc, 1, true)
					key.GroupID = i64p(1)
					key.Group.RateMultiplier = 1.1
					svc.channelService = newTestChannelServiceForStats(t, &Channel{ID: 1, Status: StatusActive}, 1, PlatformOpenAI)
					tier := "ultrafast"
					err := svc.RecordUsage(context.Background(), &OpenAIRecordUsageInput{
						Result: &OpenAIForwardResult{RequestID: t.Name(), Model: model, UpstreamModel: model, ServiceTier: &tier, Duration: time.Second,
							Usage: OpenAIUsage{InputTokens: 500001, CacheReadInputTokens: 250000, CacheCreationInputTokens: 1000, CacheCreation30mInputTokens: 400, CacheCreation12hInputTokens: 600, OutputTokens: 1000}},
						APIKey: key, User: &User{ID: 2}, PricingAt: stamp.at,
						Account: &Account{ID: 3, Platform: PlatformOpenAI, Type: kind, Extra: map[string]any{openAILongContextBillingEnabledKey: true}},
					})
					require.NoError(t, err)
					im, om := 1.0, 1.0
					if kind == AccountTypeAPIKey {
						im, om = 2, 1.5
					}
					standard := (249001*unit+250000*unit*.05+400*unit*1.25+600*unit*2.5)*im + 1000*unit*5*om
					want := standard * stamp.factor
					require.Equal(t, kind == AccountTypeAPIKey, repo.lastLog.LongContextBillingApplied)
					require.InDelta(t, want, repo.lastLog.TotalCost, 1e-9)
					require.Equal(t, QuantizeUsageBillingAmount(want*1.1), repo.lastLog.ActualCost)
					require.InDelta(t, repo.lastLog.ActualCost, userRepo.lastAmount, 1e-9)
					require.NotNil(t, repo.lastLog.AccountStatsCost)
					require.InDelta(t, want, *repo.lastLog.AccountStatsCost, 1e-9)
				})
			}
		}
	}
}

func TestGPT62ChannelUltrafastOverrideSurvivesPromotionCutoff(t *testing.T) {
	svc := newTestBillingService()
	groupID := int64(8907)
	card := ChannelModelPricing{Platform: PlatformOpenAI, Models: []string{"gpt-6.2-sol"},
		InputPrice: pricingMultiplier(1e-6), UltrafastMultiplier: pricingMultiplier(1.25)}
	resolver := NewModelPricingResolver(newChannelServiceWithPricings(groupID, []ChannelModelPricing{card}), svc)
	for _, at := range []time.Time{time.Date(2027, 3, 1, 7, 59, 59, 0, time.UTC), time.Date(2027, 3, 1, 8, 0, 0, 0, time.UTC)} {
		cost, err := svc.CalculateCostUnified(CostInput{Ctx: context.Background(), Model: "gpt-6.2-sol", GroupID: &groupID,
			Tokens: UsageTokens{InputTokens: 1000}, RateMultiplier: 1, ServiceTier: "ultrafast", Resolver: resolver, PricingAt: at})
		require.NoError(t, err)
		require.InDelta(t, .00125, cost.TotalCost, 1e-12)
	}
}
