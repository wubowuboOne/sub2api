package service

import (
	"context"
	"time"

	"github.com/Wei-Shaw/sub2api/internal/pkg/logger"
	"go.uber.org/zap"
)

func (r *openAIAgentRecoveryRequest) refusal(reason, stage, detail string) *OpenAIAgentRecoveryError {
	err := &OpenAIAgentRecoveryError{Reason: reason, RejectionStage: stage, RejectionReason: detail}
	if r != nil && r.owner != nil {
		err.OriginalAccountID = r.owner.accountID
	} else if r != nil {
		err.OriginalAccountID = r.releasedOwnerID
	}
	return err
}

// LogRefusal records only routing diagnostics, never request bodies, opaque
// item identities, session identifiers, credentials or credential hashes.
func (e *OpenAIAgentRecoveryError) LogRefusal(ctx context.Context) {
	originalID := e.OriginalAccountID
	stage, reason := e.RejectionStage, e.RejectionReason
	if stage == "" {
		stage = "recovery_check"
	}
	if reason == "" {
		reason = e.Reason
	}
	fields := []zap.Field{
		zap.String("platform", PlatformOpenAI),
		zap.String("error_code", e.ClientCode()),
		zap.Int("status_code", e.HTTPStatus()),
		zap.String("rejection_stage", stage),
		zap.String("rejection_reason", reason),
	}
	if request := openAIAgentRecoveryFromContext(ctx); request != nil {
		if originalID == 0 && request.owner != nil {
			originalID = request.owner.accountID
		}
		fields = append(fields,
			zap.Int64("api_key_id", request.keyID),
			zap.Int64("group_id", request.groupID),
			zap.String("model", request.model),
			zap.Int("agent_item_count", len(request.items)),
			zap.Int("known_item_count", len(request.known)),
			zap.Bool("native_compaction", request.nativeCompaction),
			zap.Bool("tool_result", request.toolResult),
		)
	}
	fields = append(fields, zap.Int64("original_account_id", originalID))
	if originalID > 0 {
		// Index the refused owner in system logs without assigning a forwarded
		// account or inventing an upstream attempt in the request error record.
		fields = append(fields, zap.Int64("account_id", originalID))
	}
	if !e.RetryAt.IsZero() {
		fields = append(fields, zap.Time("retry_at", e.RetryAt))
	}
	logger.FromContext(ctx).Warn("openai.agent_recovery_refused", fields...)
}

// Called only after the existing admission check has rejected the account.
func openAIAgentRecoveryAccountRejection(account *Account, now time.Time) string {
	switch {
	case account == nil:
		return "account_missing"
	case !account.IsActive():
		return "account_inactive"
	case !account.Schedulable:
		return "account_not_schedulable"
	case account.PerformancePaused:
		return "account_performance_paused"
	case account.AutoPauseOnExpired && account.ExpiresAt != nil && !account.ExpiresAt.After(now):
		return "account_expired"
	case account.OverloadUntil != nil && account.OverloadUntil.After(now):
		return "account_overloaded"
	case account.RateLimitResetAt != nil && account.RateLimitResetAt.After(now):
		return "account_rate_limited"
	case account.TempUnschedulableUntil != nil && account.TempUnschedulableUntil.After(now):
		return "account_temp_unschedulable"
	case account.SupportsAccountQuota() && account.IsQuotaExceeded():
		return "account_quota_exceeded"
	default:
		return "account_not_eligible"
	}
}
