package service

import (
	"context"
	"encoding/hex"
	"time"
)

// A cooldown is not evidence of incompatible ciphertext. Preserve the existing
// evidence while waiting, without admitting a request or guessing another owner.
func (s *defaultOpenAIAccountScheduler) checkOpenAIAgentRecoveryCooldown(ctx context.Context, req OpenAIAccountScheduleRequest, recovery *openAIAgentRecoveryRequest) error {
	if recovery == nil || recovery.owner == nil || s.service.accountRepo == nil {
		return nil
	}
	// The scheduler snapshot may filter out quota-blocked accounts entirely.
	// Use current authoritative state for this diagnosis; never mutate it.
	account, err := s.service.accountRepo.GetByID(ctx, recovery.owner.accountID)
	if err != nil {
		return recovery.refusal("owner_unavailable", "cooldown", "account_lookup_failed")
	}
	if account == nil {
		return recovery.refusal("owner_unavailable", "cooldown", "account_missing")
	}
	// HTTP continuation requires an API-key owner even when its authorization
	// anchors exist. A quota reset cannot make OAuth previous_response_id valid.
	// Check before retaining any evidence; WS OAuth continuation remains valid.
	if recovery.userID > 0 && req.PreviousResponseID != "" && !account.IsOpenAIApiKey() {
		return recovery.refusal("http_previous_response_unsupported", "cooldown", "http_previous_response_unsupported")
	}
	now := time.Now()
	var retryAt time.Time
	reason := "owner_rate_limited"
	if account.RateLimitResetAt != nil && account.RateLimitResetAt.After(now) {
		retryAt = *account.RateLimitResetAt
	}
	if account.OverloadUntil != nil && account.OverloadUntil.After(now) && account.OverloadUntil.After(retryAt) {
		retryAt, reason = *account.OverloadUntil, "owner_overloaded"
	}
	if retryAt.IsZero() {
		return nil
	}
	// Revocation, manual pauses and credential/route changes still require
	// intervention; a future quota reset must not disguise those conditions.
	var rejection string
	switch {
	case !account.IsActive():
		rejection = "account_inactive"
	case !account.Schedulable:
		rejection = "account_not_schedulable"
	case account.PerformancePaused:
		rejection = "account_performance_paused"
	case account.AutoPauseOnExpired && account.ExpiresAt != nil && !account.ExpiresAt.After(now):
		rejection = "account_expired"
	case account.TempUnschedulableUntil != nil && account.TempUnschedulableUntil.After(now):
		rejection = "account_temp_unschedulable"
	case account.SupportsAccountQuota() && account.IsQuotaExceeded():
		rejection = "account_quota_exceeded"
	case account.HasManagedOpenAIConnections() || account.AuthConnectionID != "":
		rejection = "managed_connection_route"
	case !account.IsOpenAI():
		rejection = "platform_mismatch"
	case !s.service.openAIAccountMatchesSchedulingGroup(account, req.GroupID):
		rejection = "group_mismatch"
	case req.RequirePrivacySet && !account.IsPrivacySet():
		rejection = "privacy_not_set"
	case !supportsOpenAIRequestModel(account, req.RequestedModel, req.RequireCompact):
		rejection = "model_not_supported"
	case !account.IsModelAllowedInGroup(req.GroupID, req.RequestedModel):
		rejection = "model_not_allowed_in_group"
	case !pelicanGroupTestModelAllowed(ctx, account, req.RequestedModel):
		rejection = "model_not_selected_for_pelican"
	case !accountSupportsOpenAICapabilitiesForRequest(ctx, account, req.RequestedModel, req.RequiredCapability, req.RequiredImageCapability):
		rejection = "capability_mismatch"
	case !s.isAccountTransportCompatible(account, req.RequiredTransport, req.RequestedModel):
		rejection = "transport_mismatch"
	}
	if rejection != "" {
		return recovery.refusal("owner_unavailable", "cooldown", rejection)
	}
	if req.GroupID != nil && s.service.needsUpstreamChannelRestrictionCheck(ctx, req.GroupID) &&
		s.service.isUpstreamModelRestrictedByChannel(ctx, *req.GroupID, account, req.RequestedModel, req.RequireCompact) {
		return recovery.refusal("owner_unavailable", "cooldown", "channel_upstream_restricted")
	}
	if vetoed, detail := openAIProfitControlVetoReason(ctx, account); vetoed {
		return recovery.refusal("owner_unavailable", "cooldown", detail)
	}
	if err := s.service.checkOpenAIAgentRecoveryAccount(ctx, account); err != nil {
		return err
	}
	if req.GuardianParentAccountID > 0 && req.GuardianParentAccountID != account.ID {
		return recovery.refusal("owner_unavailable", "cooldown", "guardian_owner_mismatch")
	}
	if _, excluded := req.ExcludedIDs[account.ID]; excluded {
		return recovery.refusal("owner_unavailable", "cooldown", "account_excluded")
	}
	// Leave a full normal affinity window after reset, even if the client waits
	// without polling. These are local observations, not provider-side leases.
	ttl := time.Until(retryAt) + s.service.openAIWSSessionStickyTTL()
	storeCtx, cancel := context.WithTimeout(ctx, 3*time.Second)
	defer cancel()
	if req.PreviousResponseID != "" {
		if recovery.userID > 0 {
			store, ok := s.service.getOpenAIWSStateStore().(OpenAIHTTPResponseOwnerRetentionStore)
			if !ok {
				return recovery.refusal("owner_unavailable", "cooldown", "http_owner_retention_unsupported")
			}
			retained, err := store.RefreshHTTPResponseOwner(storeCtx, derefGroupID(req.GroupID), req.PreviousResponseID, recovery.userID, recovery.keyID, ttl)
			if err != nil {
				return recovery.refusal("store_unavailable", "cooldown", "http_owner_retention_failed")
			}
			if !retained {
				return recovery.refusal("owner_unavailable", "cooldown", "http_owner_not_retained")
			}
		}
		store, ok := s.service.getOpenAIWSStateStore().(OpenAIWSResponseAccountRetentionStore)
		if !ok {
			return recovery.refusal("owner_unavailable", "cooldown", "response_owner_retention_unsupported")
		}
		retained, err := store.RefreshResponseAccount(storeCtx, derefGroupID(req.GroupID), req.PreviousResponseID, account.ID, ttl)
		if err != nil {
			return recovery.refusal("store_unavailable", "cooldown", "response_owner_retention_failed")
		}
		if !retained {
			return recovery.refusal("owner_unavailable", "cooldown", "response_owner_not_retained")
		}
	}
	if err := s.service.retainOpenAIAgentRecovery(storeCtx, recovery, ttl); err != nil {
		return err
	}
	refused := recovery.refusal(reason, "cooldown", reason)
	refused.RetryAt = retryAt
	return refused
}

func (s *OpenAIGatewayService) retainOpenAIAgentRecovery(ctx context.Context, request *openAIAgentRecoveryRequest, ttl time.Duration) error {
	if _, shared := s.cache.(OpenAIAgentRecoveryCache); shared {
		cache, ok := s.cache.(OpenAIAgentRecoveryRetentionCache)
		if !ok {
			return request.refusal("store_unavailable", "binding_retention", "retention_unsupported")
		}
		for key := range request.known {
			retained, err := cache.RefreshOpenAIAgentRecovery(ctx, hex.EncodeToString(key[:]), encodeOpenAIAgentRecoveryBinding(*request.owner), ttl)
			if err != nil {
				return request.refusal("store_unavailable", "binding_retention", "shared_binding_retention_failed")
			}
			if !retained {
				return request.refusal("owner_unavailable", "binding_retention", "shared_binding_not_retained")
			}
		}
		return nil
	}
	state := &s.agentMessageRecovery
	state.mu.Lock()
	defer state.mu.Unlock()
	now, until := time.Now(), time.Now().Add(ttl)
	for key := range request.known {
		binding, found := state.bindings[key]
		var rejection string
		switch {
		case !found:
			rejection = "binding_missing"
		case !binding.expiresAt.After(now):
			rejection = "binding_expired"
		case binding.conflicted:
			rejection = "binding_conflicted"
		case binding.failed:
			rejection = "binding_delivery_failed"
		case binding.pending != "":
			rejection = "binding_delivery_pending"
		case !sameOpenAIAgentRecoveryRoute(binding, *request.owner):
			rejection = "binding_route_mismatch"
		}
		if rejection != "" {
			return request.refusal("owner_unavailable", "binding_retention", rejection)
		}
	}
	for key := range request.known {
		binding := state.bindings[key]
		if until.After(binding.expiresAt) {
			binding.expiresAt = until
			state.bindings[key] = binding
		}
	}
	return nil
}
