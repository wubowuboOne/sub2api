package service

import (
	"context"
	"crypto/sha256"
	"io"
	"net/http"
	"strings"

	"github.com/gin-gonic/gin"
	"github.com/tidwall/gjson"
)

const OpenAIAgentHTTP429RetryReason GatewayFailureReason = "openai_agent_http_429_same_account_retry"

type openAIAgentHTTP429RetryKey struct{}

type openAIAgentHTTP429Retry struct {
	route openAIAgentRecoveryBinding
	body  [32]byte
	limit int
}

func openAIAgentHTTP429RetryFromContext(ctx context.Context) *openAIAgentHTTP429Retry {
	if ctx == nil {
		return nil
	}
	retry, _ := ctx.Value(openAIAgentHTTP429RetryKey{}).(*openAIAgentHTTP429Retry)
	return retry
}

func openAIAgentHTTP429RetryAccountID(ctx context.Context) int64 {
	if retry := openAIAgentHTTP429RetryFromContext(ctx); retry != nil {
		return retry.route.accountID
	}
	return 0
}

func openAIAgentHTTP429BodyDigest(req *http.Request) ([32]byte, error) {
	if req == nil || req.GetBody == nil {
		return [32]byte{}, io.ErrUnexpectedEOF
	}
	body, err := req.GetBody()
	if err != nil {
		return [32]byte{}, err
	}
	defer body.Close()
	hash := sha256.New()
	if _, err := io.Copy(hash, body); err != nil {
		return [32]byte{}, err
	}
	var digest [32]byte
	copy(digest[:], hash.Sum(nil))
	return digest, nil
}

// Only a rejected native HTTP request gets this exception. Stream failures,
// omitted-history recovery, quota/policy errors and unknown codes keep their
// existing no-replay contract.
func (s *OpenAIGatewayService) newOpenAIAgentHTTP429RetryError(ctx context.Context, c *gin.Context, account *Account, req *http.Request, resp *http.Response, payload []byte) *UpstreamFailoverError {
	r := openAIAgentRecoveryFromContext(ctx)
	if ctx.Err() != nil || isControlledExperiment(ctx) || c == nil || c.Request == nil || c.Request.Context().Err() != nil ||
		s == nil || s.accountRepo == nil || account == nil || account.Platform != PlatformOpenAI ||
		!account.IsPoolMode() || account.GetPoolModeRetryCount() <= 0 || !account.IsPoolModeRetryableStatus(http.StatusTooManyRequests) ||
		resp == nil || resp.StatusCode != http.StatusTooManyRequests ||
		r == nil || !r.disableOwnerBinding || len(r.items) == 0 || r.omittedHistory || r.omitted400History || r.disallowLearning || r.delivery == nil ||
		IsResponseCommitted(c) || openAIStreamClientOutputStarted(c, false) {
		return nil
	}
	if disposition, _ := classifyOpenAIOAuth429(resp.Header, payload); disposition != openAIOAuth429Transient {
		return nil
	}
	if !gjson.ValidBytes(payload) || !openAIEncryptedJSONUnambiguous(payload) ||
		!gjson.GetBytes(payload, "error").IsObject() || gjson.GetBytes(payload, "error.resets_at").Exists() {
		return nil
	}
	for _, path := range []string{"error.type", "error.code"} {
		switch strings.ToLower(strings.TrimSpace(gjson.GetBytes(payload, path).String())) {
		case "", "rate_limit_error", "rate_limit_exceeded", "too_many_requests", "server_error", "upstream_error", "server_is_overloaded", "slow_down", OpenAIUpstreamCapacityCode:
		default:
			return nil
		}
	}
	message := sanitizeUpstreamErrorMessage(strings.TrimSpace(extractUpstreamErrorMessage(payload)))
	if !s.shouldFailoverOpenAIUpstreamResponse(account, resp.StatusCode, message, payload) {
		return nil
	}
	digest, err := openAIAgentHTTP429BodyDigest(req)
	if err != nil {
		return nil
	}
	retry := openAIAgentHTTP429RetryFromContext(ctx)
	if retry == nil {
		retry = &openAIAgentHTTP429Retry{route: r.delivery.binding, body: digest, limit: account.GetPoolModeRetryCount()}
		// Forward may use a private recovery context; retain the immutable route
		// on the handler's context so scheduler re-entry cannot select a peer.
		c.Request = c.Request.WithContext(context.WithValue(c.Request.Context(), openAIAgentHTTP429RetryKey{}, retry))
	}
	appendOpsUpstreamError(c, OpsUpstreamErrorEvent{
		Platform: account.Platform, AccountID: account.ID, AccountName: account.Name,
		ProxyID: opsUpstreamProxyID(account), ProxyName: opsUpstreamProxyName(account),
		UpstreamStatusCode: resp.StatusCode, UpstreamRequestID: resp.Header.Get("x-request-id"),
		Kind: "failover", Message: message, Reason: string(OpenAIAgentHTTP429RetryReason),
		Stage: string(GatewayFailureStageInference), Scope: string(GatewayFailureScopeRequest),
	})
	return &UpstreamFailoverError{
		StatusCode: resp.StatusCode, ResponseBody: payload, ResponseHeaders: resp.Header.Clone(),
		RetryableOnSameAccount: true, SameAccountRetryMax: retry.limit,
		SameAccountRetryDelay:  openAIRequestScopedRetryDelay(resp.Header, payload),
		RequestScopedTransient: true, NextAccountAction: NextAccountStop,
		Stage: GatewayFailureStageInference, Scope: GatewayFailureScopeRequest,
		Reason: OpenAIAgentHTTP429RetryReason,
	}
}

func checkOpenAIAgentHTTP429Retry(ctx context.Context, binding openAIAgentRecoveryBinding, req *http.Request) error {
	retry := openAIAgentHTTP429RetryFromContext(ctx)
	if retry == nil {
		return nil
	}
	if !sameOpenAIAgentRecoveryRoute(retry.route, binding) {
		return openAIAgentRecoveryRefusal(ctx, "route_identity")
	}
	digest, err := openAIAgentHTTP429BodyDigest(req)
	if err != nil || digest != retry.body {
		return openAIAgentRecoveryRefusal(ctx, "input_integrity")
	}
	return nil
}
