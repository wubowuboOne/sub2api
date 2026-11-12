package service

import (
	"encoding/json"
	"net/http"
	"strings"

	"github.com/gin-gonic/gin"
	"github.com/tidwall/gjson"
)

const OpenAIEncryptedContentRetryReason = GatewayFailureReason("openai_encrypted_content_same_account_retry")

// Ciphertext can be tied to the original upstream. Opted-in pool retries must
// preserve it verbatim, and must never turn a rejection into account rotation.
func openAIConfiguredEncryptedContentRetry(account *Account, status int, payload []byte) bool {
	if account == nil || account.Platform != PlatformOpenAI || account.Type != AccountTypeAPIKey ||
		!account.IsPoolMode() || account.GetPoolModeRetryCount() <= 0 {
		return false
	}
	event := effectiveOpenAISSEEventType(payload, "error")
	if !isOpenAIInvalidEncryptedContentSSE(payload, event) {
		return false
	}
	if status == 0 {
		status = openAIStreamFailedEventSemanticStatus(payload, extractOpenAISSEErrorMessage(payload))
	}
	return account.IsPoolModeRetryableStatus(status)
}

func newOpenAIEncryptedContentRetryError(c *gin.Context, account *Account, status int, headers http.Header, payload []byte, passthrough bool) *UpstreamFailoverError {
	if !openAIConfiguredEncryptedContentRetry(account, status, payload) {
		return nil
	}
	if status == 0 {
		status = openAIStreamFailedEventSemanticStatus(payload, extractOpenAISSEErrorMessage(payload))
	}
	path := ""
	if effectiveOpenAISSEEventType(payload, "error") == "response.failed" {
		path = "response.error."
	} else if gjson.GetBytes(payload, "error").IsObject() {
		path = "error."
	}
	code := strings.ToLower(strings.TrimSpace(gjson.GetBytes(payload, path+"code").String()))
	// Providers can echo ciphertext or private request history in the original
	// message, param, or body. Retain only the recognized code and a fixed message.
	body, _ := json.Marshal(gin.H{"error": gin.H{
		"code": code, "type": "invalid_request_error", "message": openAIEncryptedErrorRedacted,
	}})
	setOpsUpstreamError(c, status, openAIEncryptedErrorRedacted, "")
	appendOpsUpstreamError(c, OpsUpstreamErrorEvent{
		Platform: account.Platform, AccountID: account.ID, AccountName: account.Name,
		ProxyID: opsUpstreamProxyID(account), ProxyName: opsUpstreamProxyName(account),
		UpstreamStatusCode: status, UpstreamRequestID: headers.Get("x-request-id"),
		Passthrough: passthrough, Kind: "failover", Message: openAIEncryptedErrorRedacted,
		Stage: string(GatewayFailureStageInference), Scope: string(GatewayFailureScopeRequest),
		Reason: string(OpenAIEncryptedContentRetryReason),
	})
	return &UpstreamFailoverError{
		StatusCode: status, ResponseBody: body, ResponseHeaders: headers.Clone(),
		RetryableOnSameAccount: true, NextAccountAction: NextAccountStop,
		Stage: GatewayFailureStageInference, Scope: GatewayFailureScopeRequest,
		Reason: OpenAIEncryptedContentRetryReason,
	}
}
