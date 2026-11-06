package service

import "net/http"

// A configured retry must not replay reasoning or tool calls already delivered
// to a client. Keep the attempt private until its terminal event instead.
func openAIStreamBuffersConfiguredRetry(account *Account) bool {
	return account != nil && account.Platform == PlatformOpenAI &&
		account.IsPoolMode() && account.GetPoolModeRetryCount() > 0 &&
		account.IsPoolModeRetryableStatus(http.StatusBadGateway)
}
