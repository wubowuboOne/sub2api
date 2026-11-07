package service

import (
	"context"
	"math"
)

// ProbeAPIKeyGroup checks scheduler eligibility without reserving slots,
// registering sessions, setting account errors, or binding sticky accounts.
func (s *GatewayService) ProbeAPIKeyGroup(ctx context.Context, group *Group, model, kind string) (APIKeyGroupAvailability, error) {
	availability := APIKeyGroupAvailability{}
	ctx = s.withGroupContext(ctx, group)
	platform, forced, err := s.resolvePlatform(ctx, &group.ID, group, model)
	if err != nil {
		availability.UnavailableReason = "platform_unresolved"
		return availability, nil
	}
	if group.Platform == PlatformComposite {
		decision, ok, err := s.resolveCompositeRouteDecision(ctx, group, model, CompositeRouteEndpointAny)
		if err != nil {
			availability.UnavailableReason = "composite_route_error"
			return availability, err
		}
		if !ok {
			availability.UnavailableReason = "composite_route_unavailable"
			return availability, nil
		}
		model = decision.UpstreamModel
		ctx = WithCompositeRouteDecision(ctx, decision)
	}
	if kind == "token" {
		ctx, _ = WithGatewayTokenRequestPricing(ctx)
		ctx = s.withGatewayProfitControlGate(ctx, &group.ID)
	}
	if s.checkChannelPricingRestriction(ctx, &group.ID, model) {
		availability.UnavailableReason = "channel_model_restricted"
		return availability, nil
	}
	// Match normal scheduling: only upstream billing restricts account-mapped models.
	needsUpstreamCheck := s.needsUpstreamChannelRestrictionCheck(ctx, &group.ID)
	accounts, _, err := s.listSchedulableAccounts(ctx, &group.ID, platform, forced)
	if err != nil {
		availability.UnavailableReason = "account_lookup_failed"
		return availability, err
	}
	availability.AccountCount = len(accounts)
	ctx = s.withWindowCostPrefetch(ctx, accounts)
	ctx = s.withRPMPrefetch(ctx, accounts)
	eligible := make([]AccountWithConcurrency, 0, len(accounts))
	for i := range accounts {
		account := &accounts[i]
		reason := ""
		switch {
		case !s.isAccountSchedulableForSelection(account):
			reason = "not_schedulable"
		case !s.isGatewayAccountProfitEligible(ctx, account):
			reason = "profit_control"
		case group.RequirePrivacySet && !account.IsPrivacySet():
			reason = "privacy_required"
		case model != "" && !s.isModelSupportedByAccountInGroup(ctx, account, &group.ID, model):
			reason = "model_unsupported"
		case !s.isAccountSchedulableForModelSelection(ctx, account, model):
			reason = "model_unavailable"
		case !s.isAccountSchedulableForQuota(account):
			reason = "quota_exhausted"
		case !s.isAccountSchedulableForWindowCost(ctx, account, false):
			reason = "window_cost_limit"
		case !s.isAccountSchedulableForRPM(ctx, account, false):
			reason = "rpm_limit"
		case needsUpstreamCheck && s.isUpstreamModelRestrictedByChannel(ctx, group.ID, account, model):
			reason = "channel_upstream_restricted"
		}
		if reason != "" {
			if availability.AccountExclusions == nil {
				availability.AccountExclusions = make(map[string]int)
			}
			availability.AccountExclusions[reason]++
			continue
		}
		eligible = append(eligible, AccountWithConcurrency{ID: account.ID, MaxConcurrency: account.Concurrency})
	}
	if len(eligible) == 0 {
		availability.UnavailableReason = "no_eligible_accounts"
		if len(accounts) == 0 {
			availability.UnavailableReason = "no_schedulable_accounts"
		}
		return availability, nil
	}
	if s.concurrencyService == nil {
		availability.Available = true
		return availability, nil
	}
	loads, err := s.concurrencyService.GetAccountsLoadBatch(ctx, eligible)
	if err != nil {
		availability.UnavailableReason = "account_load_failed"
		return availability, err
	}
	minLoad := math.Inf(1)
	for _, account := range eligible {
		if account.MaxConcurrency <= 0 {
			availability.Available = true
			return availability, nil
		}
		load, ok := loads[account.ID]
		if !ok || load == nil {
			continue
		}
		ratio := float64(load.CurrentConcurrency) / float64(account.MaxConcurrency)
		if ratio < minLoad {
			minLoad = ratio
		}
	}
	if math.IsInf(minLoad, 1) {
		availability.UnavailableReason = "account_load_missing"
		return availability, nil
	}
	availability.Available, availability.Busy, availability.Load = true, minLoad >= 1, minLoad
	return availability, nil
}
