package service

import (
	"context"
	"encoding/hex"
	"time"

	"github.com/Wei-Shaw/sub2api/internal/pkg/logger"
	"go.uber.org/zap"
)

// Administrative disablement releases task affinity. Normal admission still
// validates authorization, capabilities and any stored response references.
func (s *OpenAIGatewayService) releaseDisabledOpenAIAgentOwner(ctx context.Context, request *openAIAgentRecoveryRequest) error {
	if request == nil || request.owner == nil || s.accountRepo == nil {
		return nil
	}
	account, err := s.accountRepo.GetByID(ctx, request.owner.accountID)
	if err != nil || account == nil {
		// Existing admission reports lookup failures; they are not disablement.
		return nil
	}
	if account.Schedulable && account.Status != "inactive" && account.Status != StatusDisabled {
		return nil
	}
	ownerID := request.owner.accountID
	if shared, ok := s.cache.(OpenAIAgentRecoveryCache); ok {
		releaser, ok := shared.(OpenAIAgentRecoveryReleaseCache)
		if !ok {
			return request.refusal("store_unavailable", "owner_release", "binding_release_unsupported")
		}
		storeCtx, cancel := context.WithTimeout(ctx, 3*time.Second)
		defer cancel()
		for key := range request.known {
			released, err := releaser.ReleaseOpenAIAgentRecovery(storeCtx, hex.EncodeToString(key[:]), encodeOpenAIAgentRecoveryBinding(*request.owner))
			if err != nil {
				return request.refusal("store_unavailable", "owner_release", "binding_release_failed")
			}
			if !released {
				return request.refusal("owner_conflict", "owner_release", "binding_changed_before_release")
			}
		}
	} else {
		state := &s.agentMessageRecovery
		state.mu.Lock()
		defer state.mu.Unlock()
		for key := range request.known {
			if binding, found := state.bindings[key]; found &&
				(binding.conflicted || binding.failed || binding.pending != "" || !sameOpenAIAgentRecoveryRoute(binding, *request.owner)) {
				return request.refusal("owner_conflict", "owner_release", "binding_changed_before_release")
			}
		}
		for key := range request.known {
			delete(state.bindings, key)
		}
	}
	request.releasedOwnerID = ownerID
	request.owner = nil
	request.known = nil
	logger.FromContext(ctx).Info("openai.agent_recovery_owner_released",
		zap.Int64("original_account_id", ownerID),
		zap.String("reason", "account_disabled"),
	)
	return nil
}
