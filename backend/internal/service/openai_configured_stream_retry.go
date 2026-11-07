package service

import (
	"bufio"
	"errors"
	"net/http"

	"github.com/Wei-Shaw/sub2api/internal/config"
	"github.com/gin-gonic/gin"
)

// A configured retry must not replay reasoning or tool calls already delivered
// to a client. Keep the attempt private until its terminal event instead.
func openAIStreamBuffersConfiguredRetry(account *Account) bool {
	return account != nil && account.Platform == PlatformOpenAI &&
		account.IsPoolMode() && account.GetPoolModeRetryCount() > 0 &&
		account.IsPoolModeRetryableStatus(http.StatusBadGateway)
}

func (s *OpenAIGatewayService) openAIStreamRetryLimits() (int, int64) {
	line := config.DefaultOpenAIStreamRetryMaxLineBytes
	buffer := int64(config.DefaultOpenAIStreamRetryMaxBufferBytes)
	if s != nil && s.cfg != nil {
		if n := s.cfg.Gateway.OpenAIStreamRetryMaxLineBytes; n > 0 {
			line = n
		}
		if n := s.cfg.Gateway.OpenAIStreamRetryMaxBufferBytes; n > 0 {
			buffer = n
		}
	}
	return line, buffer
}

func (s *OpenAIGatewayService) newOpenAIStreamOutputStage(account *Account) *openAIFirstOutputStage {
	if !openAIStreamBuffersConfiguredRetry(account) {
		return newDefaultOpenAIFirstOutputStage()
	}
	_, limit := s.openAIStreamRetryLimits()
	return newOpenAIFirstOutputStage(limit)
}

func (s *OpenAIGatewayService) openAIRetryLineLimitError(c *gin.Context, account *Account, requestID string, err error, disconnected, outputStarted bool) *UpstreamFailoverError {
	if !errors.Is(err, bufio.ErrTooLong) || !openAIStreamBuffersConfiguredRetry(account) || disconnected || c == nil || c.Request == nil || c.Request.Context().Err() != nil || openAIStreamClientOutputStarted(c, outputStarted) {
		return nil
	}
	return s.newOpenAIStreamFailoverError(c, account, false, requestID, nil, "OpenAI SSE line exceeds configured maximum before output")
}
