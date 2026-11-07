//go:build unit

package service

import (
	"context"
	"encoding/json"
	"errors"
	"testing"
	"time"

	infraerrors "github.com/Wei-Shaw/sub2api/internal/pkg/errors"
	"github.com/Wei-Shaw/sub2api/internal/pkg/logger"
	"github.com/stretchr/testify/require"
	"go.uber.org/zap"
	"go.uber.org/zap/zaptest/observer"
)

func newAPIKeyGroupProbeFixture(source string, pricedModels ...string) (*GatewayService, *Group) {
	group := &Group{ID: 1, Platform: PlatformOpenAI, Status: StatusActive, Hydrated: true, RateMultiplier: 0.5}
	channel := Channel{
		ID: 1, Status: StatusActive, GroupIDs: []int64{group.ID},
		BillingModelSource: source, RestrictModels: true,
		ModelPricing: []ChannelModelPricing{{Platform: PlatformOpenAI, Models: pricedModels}},
	}
	account := Account{
		ID: 10, Platform: PlatformOpenAI, Type: AccountTypeAPIKey,
		Status: StatusActive, Schedulable: true, Concurrency: 10,
		Credentials: map[string]any{"model_mapping": map[string]any{"gpt-6.1-sol": "provider-private-model"}},
	}
	return &GatewayService{
		accountRepo: &mockAccountRepoForPlatform{
			accounts: []Account{account}, accountsByID: map[int64]*Account{account.ID: &account},
		},
		cfg:            testConfig(),
		channelService: newTestChannelService(makeStandardRepo(channel, map[int64]string{group.ID: PlatformOpenAI})),
	}, group
}

func TestProbeAPIKeyGroupHonorsBillingModelSource(t *testing.T) {
	for _, tc := range []struct {
		name          string
		source        string
		pricedModels  []string
		wantAvailable bool
		wantReason    string
	}{
		{"requested_model_only", BillingModelSourceRequested, []string{"gpt-6.1-sol"}, true, ""},
		{"requested_both_models", BillingModelSourceRequested, []string{"gpt-6.1-sol", "provider-private-model"}, true, ""},
		{"requested_unpriced", BillingModelSourceRequested, []string{"provider-private-model"}, false, "channel_model_restricted"},
		{"channel_mapped", BillingModelSourceChannelMapped, []string{"gpt-6.1-sol"}, true, ""},
		{"response_model", BillingModelSourceResponse, []string{"gpt-6.1-sol"}, true, ""},
		{"default_source", "", []string{"gpt-6.1-sol"}, true, ""},
		{"upstream_unpriced", BillingModelSourceUpstream, []string{"gpt-6.1-sol"}, false, "no_eligible_accounts"},
		{"upstream_priced", BillingModelSourceUpstream, []string{"provider-private-model"}, true, ""},
	} {
		t.Run(tc.name, func(t *testing.T) {
			gateway, group := newAPIKeyGroupProbeFixture(tc.source, tc.pricedModels...)
			availability, err := gateway.ProbeAPIKeyGroup(context.Background(), group, "gpt-6.1-sol", "token")
			require.NoError(t, err)
			require.Equal(t, tc.wantAvailable, availability.Available)
			require.Equal(t, tc.wantReason, availability.UnavailableReason)
			if tc.name == "upstream_unpriced" {
				require.Equal(t, map[string]int{"channel_upstream_restricted": 1}, availability.AccountExclusions)
			}
		})
	}
}

func TestProbeAPIKeyGroupPreservesAccountRestrictions(t *testing.T) {
	for _, tc := range []struct {
		name       string
		mutate     func(*GatewayService, *Group)
		wantReason string
		exclusion  string
	}{
		{"unsupported_model", func(g *GatewayService, _ *Group) {
			g.accountRepo.(*mockAccountRepoForPlatform).accounts[0].Credentials = map[string]any{
				"model_mapping": map[string]any{"another-model": "provider-private-model"},
			}
		}, "no_eligible_accounts", "model_unsupported"},
		{"group_model_limit", func(g *GatewayService, group *Group) {
			g.accountRepo.(*mockAccountRepoForPlatform).accounts[0].AccountGroups = []AccountGroup{
				{GroupID: group.ID, AllowedModels: []string{"another-model"}},
			}
		}, "no_eligible_accounts", "model_unsupported"},
		{"privacy_required", func(_ *GatewayService, group *Group) {
			group.RequirePrivacySet = true
		}, "no_eligible_accounts", "privacy_required"},
		{"account_cooldown", func(g *GatewayService, _ *Group) {
			until := time.Now().Add(time.Hour)
			g.accountRepo.(*mockAccountRepoForPlatform).accounts[0].RateLimitResetAt = &until
		}, "no_schedulable_accounts", ""},
		{"quality_cooldown", func(g *GatewayService, _ *Group) {
			g.accountRepo.(*mockAccountRepoForPlatform).accounts[0].Extra = map[string]any{
				"model_rate_limits": map[string]any{"provider-private-model": map[string]any{
					"reason": "quality_rule:7", "rate_limit_reset_at": time.Now().Add(time.Hour).UTC().Format(time.RFC3339),
				}},
			}
		}, "no_eligible_accounts", "model_unavailable"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			gateway, group := newAPIKeyGroupProbeFixture(BillingModelSourceRequested, "gpt-6.1-sol")
			tc.mutate(gateway, group)
			availability, err := gateway.ProbeAPIKeyGroup(context.Background(), group, "gpt-6.1-sol", "token")
			require.NoError(t, err)
			require.False(t, availability.Available)
			require.Equal(t, tc.wantReason, availability.UnavailableReason)
			if tc.exclusion != "" {
				require.Equal(t, map[string]int{tc.exclusion: 1}, availability.AccountExclusions)
			}
		})
	}
}

func TestProbeAPIKeyGroupUsesChannelMappingForPricing(t *testing.T) {
	for _, source := range []string{BillingModelSourceRequested, BillingModelSourceChannelMapped, BillingModelSourceResponse} {
		t.Run(source, func(t *testing.T) {
			for _, priced := range []string{"gpt-6.1-sol", "channel-model"} {
				t.Run(priced, func(t *testing.T) {
					gateway, group := newAPIKeyGroupProbeFixture(source, priced)
					channel := Channel{
						ID: 1, Status: StatusActive, GroupIDs: []int64{group.ID},
						BillingModelSource: source, RestrictModels: true,
						ModelPricing: []ChannelModelPricing{{Platform: PlatformOpenAI, Models: []string{priced}}},
						ModelMapping: map[string]map[string]string{PlatformOpenAI: {"gpt-6.1-sol": "channel-model"}},
					}
					gateway.channelService = newTestChannelService(makeStandardRepo(channel, map[int64]string{group.ID: PlatformOpenAI}))
					availability, err := gateway.ProbeAPIKeyGroup(context.Background(), group, "gpt-6.1-sol", "token")
					require.NoError(t, err)
					wantAvailable := priced == "channel-model"
					if source == BillingModelSourceRequested {
						wantAvailable = priced == "gpt-6.1-sol"
					}
					require.Equal(t, wantAvailable, availability.Available)
					if !wantAvailable {
						require.Equal(t, "channel_model_restricted", availability.UnavailableReason)
					}
				})
			}
		})
	}
}

func TestProbeAPIKeyGroupPreservesCapacityAndLoadFailures(t *testing.T) {
	loadErr := errors.New("load unavailable")
	for _, tc := range []struct {
		name          string
		loads         map[int64]*AccountLoadInfo
		loadErr       error
		unlimited     bool
		wantAvailable bool
		wantBusy      bool
		wantLoad      float64
		wantReason    string
	}{
		{"idle", map[int64]*AccountLoadInfo{10: {CurrentConcurrency: 2}}, nil, false, true, false, 0.2, ""},
		{"full_still_queues", map[int64]*AccountLoadInfo{10: {CurrentConcurrency: 10}}, nil, false, true, true, 1, ""},
		{"missing_load", nil, nil, false, false, false, 0, "account_load_missing"},
		{"load_error", nil, loadErr, false, false, false, 0, "account_load_failed"},
		{"unlimited", nil, nil, true, true, false, 0, ""},
	} {
		t.Run(tc.name, func(t *testing.T) {
			gateway, group := newAPIKeyGroupProbeFixture(BillingModelSourceRequested, "gpt-6.1-sol")
			if tc.unlimited {
				gateway.accountRepo.(*mockAccountRepoForPlatform).accounts[0].Concurrency = 0
			}
			cache := &stubConcurrencyCacheForTest{loadBatch: tc.loads, loadBatchErr: tc.loadErr}
			gateway.concurrencyService = NewConcurrencyService(cache)
			availability, err := gateway.ProbeAPIKeyGroup(context.Background(), group, "gpt-6.1-sol", "token")
			require.ErrorIs(t, err, tc.loadErr)
			require.Equal(t, tc.wantAvailable, availability.Available)
			require.Equal(t, tc.wantBusy, availability.Busy)
			require.Equal(t, tc.wantLoad, availability.Load)
			require.Equal(t, tc.wantReason, availability.UnavailableReason)
		})
	}
}

func TestAPIKeyMultiGroupRealProbeUsesRequestedPricingAndLogsFailures(t *testing.T) {
	for _, source := range []string{BillingModelSourceRequested, BillingModelSourceUpstream} {
		t.Run(source, func(t *testing.T) {
			gateway, first := newAPIKeyGroupProbeFixture(source, "gpt-6.1-sol")
			second := &Group{ID: 2, Platform: PlatformOpenAI, Status: StatusActive, Hydrated: true, RateMultiplier: 1}
			groups := map[int64]*Group{1: first, 2: second}
			channel := Channel{
				ID: 1, Status: StatusActive, GroupIDs: []int64{1, 2}, BillingModelSource: source, RestrictModels: true,
				ModelPricing: []ChannelModelPricing{{Platform: PlatformOpenAI, Models: []string{"gpt-6.1-sol"}}},
			}
			gateway.channelService = newTestChannelService(makeStandardRepo(channel, map[int64]string{1: PlatformOpenAI, 2: PlatformOpenAI}))
			key := &APIKey{
				ID: 7, UserID: 8, Key: "sk-private-test-key", GroupID: &first.ID, GroupIDs: []int64{1, 2},
				Group: first, User: &User{ID: 8, Balance: 10},
			}
			svc := &APIKeyService{groupRepo: multiGroupRepoStub{groups: groups}, groupRouter: gateway}
			core, logs := observer.New(zap.WarnLevel)
			ctx := logger.IntoContext(context.Background(), zap.New(core).With(zap.String("request_id", "group-probe-test")))
			selected, err := svc.SelectRequestGroup(ctx, key, []string{"gpt-6.1-sol"}, "token", nil, nil, true)
			if source == BillingModelSourceRequested {
				require.NoError(t, err)
				require.Equal(t, first.ID, *selected.GroupID)
				require.Zero(t, logs.Len())
			} else {
				require.Nil(t, selected)
				require.Equal(t, "NO_AVAILABLE_API_KEY_GROUP", infraerrors.Reason(err))
				require.Equal(t, 503, infraerrors.Code(err))
				require.Equal(t, 1, logs.Len())
				encoded, marshalErr := json.Marshal(logs.All()[0].ContextMap())
				require.NoError(t, marshalErr)
				var fields map[string]any
				require.NoError(t, json.Unmarshal(encoded, &fields))
				require.Equal(t, "group-probe-test", fields["request_id"])
				require.Equal(t, float64(key.ID), fields["api_key_id"])
				require.Equal(t, []any{"gpt-6.1-sol"}, fields["models"])
				probes := fields["group_probes"].(map[string]any)
				for _, id := range []string{"1", "2"} {
					probe := probes[id].(map[string]any)
					require.Equal(t, "no_eligible_accounts", probe["unavailable_reason"])
					require.Equal(t, map[string]any{"channel_upstream_restricted": float64(1)}, probe["account_exclusions"])
				}
				require.NotContains(t, string(encoded), key.Key)
			}
			require.Equal(t, first.ID, *key.GroupID)
			require.Equal(t, []int64{1, 2}, key.GroupIDs)
		})
	}
}
