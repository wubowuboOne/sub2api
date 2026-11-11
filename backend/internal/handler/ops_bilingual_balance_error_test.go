package handler

import (
	"testing"

	"github.com/Wei-Shaw/sub2api/internal/service"
	"github.com/stretchr/testify/require"
)

func TestNormalizeOpsLocalBalanceTypeSupportsEnglishAndBilingualMessages(t *testing.T) {
	for _, message := range []string{service.InsufficientUserBalanceMessageEnglish, service.InsufficientUserBalanceMessage} {
		require.Equal(t, "billing_error", normalizeOpsLocalBalanceType("api_error", message, "request", "client"))
		require.Equal(t, "api_error", normalizeOpsLocalBalanceType("api_error", message, "upstream", "provider"))
		require.Equal(t, "api_error", normalizeOpsLocalBalanceType("api_error", message, "request", "provider"))
	}
}
