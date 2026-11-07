package handler

import (
	"context"
	"fmt"
	"net/http/httptest"
	"testing"

	"github.com/Wei-Shaw/sub2api/internal/service"
	"github.com/gin-gonic/gin"
	"github.com/stretchr/testify/require"
)

func TestOpenAIForwardMayFailoverOnlyAfterNonSemanticWrite(t *testing.T) {
	gin.SetMode(gin.TestMode)
	rec := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(rec)
	before := service.OpenAICompactKeepaliveAdjustedWrittenSize(c)

	_, err := fmt.Fprint(c.Writer, ":\n\n")
	require.NoError(t, err)
	c.Writer.Flush()

	require.True(t, openAIForwardMayFailover(c, before, &service.UpstreamFailoverError{
		SafeToFailoverAfterWrite: true,
	}))
	require.False(t, openAIForwardMayFailover(c, before, &service.UpstreamFailoverError{}))
}

func TestOpenAIFirstOutputFailoverStopsAfterOneAccountSwitch(t *testing.T) {
	failoverErr := &service.UpstreamFailoverError{SafeToFailoverAfterWrite: true}
	count := 0

	require.False(t, openAIFirstOutputFailoverExhausted(failoverErr, &count))
	require.Equal(t, 1, count)
	require.True(t, openAIFirstOutputFailoverExhausted(failoverErr, &count))
	require.Equal(t, 1, count)
}

func TestOpenAIConfiguredSizeFailureUsesAccountBudget(t *testing.T) {
	f := &service.UpstreamFailoverError{StatusCode: 502, SafeToFailoverAfterWrite: true, RetryableOnSameAccount: true, ConfiguredStreamRetry: true}
	count := 0
	for range 11 {
		require.False(t, openAIFirstOutputFailoverExhausted(f, &count))
	}
	require.Zero(t, count)
}

func TestOpenAISizeBudgetPreservesOtherRecoveryLimits(t *testing.T) {
	for _, f := range []*service.UpstreamFailoverError{
		{StatusCode: 502, SafeToFailoverAfterWrite: true},
		{StatusCode: 502, SafeToFailoverAfterWrite: true, RetryableOnSameAccount: true},
		{StatusCode: 504, SafeToFailoverAfterWrite: true, RetryableOnSameAccount: true},
		{StatusCode: 502, SafeToFailoverAfterWrite: true, ConfiguredStreamRetry: true},
	} {
		count := 0
		require.False(t, openAIFirstOutputFailoverExhausted(f, &count))
		require.True(t, openAIFirstOutputFailoverExhausted(f, &count))
	}
}

func TestOpenAIForwardMayFailoverAfterQueueHeartbeat(t *testing.T) {
	gin.SetMode(gin.TestMode)
	recorder := httptest.NewRecorder()
	requestContext, _ := gin.CreateTestContext(recorder)
	before := service.OpenAICompactKeepaliveAdjustedWrittenSize(requestContext)
	written, err := requestContext.Writer.WriteString(string(SSEPingFormatComment))
	require.NoError(t, err)
	recordGatewayStreamHeartbeat(requestContext, written)
	requestContext.Writer.Flush()

	require.True(t, gatewayStreamHasOnlyHeartbeats(requestContext))
	require.Equal(t, before, service.OpenAICompactKeepaliveAdjustedWrittenSize(requestContext))
	require.True(t, openAIForwardMayFailover(requestContext, before, &service.UpstreamFailoverError{}))

	_, err = requestContext.Writer.WriteString(partialMessageStartSSE)
	require.NoError(t, err)
	require.False(t, gatewayStreamHasOnlyHeartbeats(requestContext))
	require.False(t, openAIForwardMayFailover(requestContext, before, &service.UpstreamFailoverError{}))
}

func TestOpenAIRequestAllowsFailoverReplayStopsCanceledClient(t *testing.T) {
	require.False(t, openAIRequestAllowsFailoverReplay(nil))

	rec := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(rec)
	requestCtx, cancel := context.WithCancel(context.Background())
	c.Request = httptest.NewRequest("POST", "/v1/responses", nil).WithContext(requestCtx)

	require.True(t, openAIRequestAllowsFailoverReplay(c))
	cancel()
	require.False(t, openAIRequestAllowsFailoverReplay(c))
}
