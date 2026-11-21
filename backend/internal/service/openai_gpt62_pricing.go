package service

import (
	"time"

	"github.com/Wei-Shaw/sub2api/internal/pkg/timezone"
)

// gpt62DefaultPricing uses the official Standard short-context rates checked on
// 2026-11-18: https://developers.openai.com/api/docs/pricing?context-length=short
// The owner confirms Pro uses the same rates as Astra. Exact catalog and
// channel prices take precedence; tier/context policies are separate.
func gpt62DefaultPricing(model string) *LiteLLMModelPricing {
	var input, output, write, read float64
	switch model {
	case "gpt-6.2-astra", "gpt-6.2-astra-pro":
		input, output, write, read = 8e-6, 40e-6, 10e-6, 0.4e-6
	case "gpt-6.2-sol":
		input, output, write, read = 2e-6, 10e-6, 2.5e-6, 0.1e-6
	default:
		return nil
	}
	write12h := write * 2
	pricing := &LiteLLMModelPricing{
		InputCostPerToken:                   input,
		OutputCostPerToken:                  output,
		CacheCreationInputTokenCost:         write,
		CacheCreationInputTokenCost12h:      &write12h,
		CacheCreationInputTokenCostExplicit: true,
		CacheReadInputTokenCost:             read,
		LiteLLMProvider:                     "openai",
		Mode:                                "responses",
		SupportsPromptCaching:               true,
	}
	if isOpenAIGPT62SolOrAstra(model) {
		pricing.LongContextInputTokenThreshold = 272_000
		pricing.LongContextInputCostMultiplier = 2
		pricing.LongContextOutputCostMultiplier = 1.5
	}
	return pricing
}

func isOpenAIGPT62SolOrAstra(model string) bool {
	switch normalizeOpenAIGPT62Model(model) {
	case "gpt-6.2-sol", "gpt-6.2-astra":
		return true
	default:
		return false
	}
}

// The UltraFast promotion ends at midnight Pacific on March 1, 2027.
func openAIUltrafastMultiplier(model string, at time.Time) float64 {
	if at.IsZero() {
		at = timezone.Now()
	}
	if isOpenAIGPT62SolOrAstra(model) && at.Before(time.Date(2027, time.March, 1, 8, 0, 0, 0, time.UTC)) {
		return 3
	}
	return 6
}
