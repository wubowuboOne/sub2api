package service

import (
	"context"
	"testing"
	"time"

	"github.com/Wei-Shaw/sub2api/internal/pkg/groupprompt"
	"github.com/stretchr/testify/require"
	"github.com/tidwall/gjson"
)

// Usage is deliberately supplied by a mock upstream, not estimated from prompt
// length. The gateway must bill that usage exactly once, regardless of where the
// prefix came from, and keep cache tokens out of the ordinary-input bucket.
func TestGroupPromptBilling_UsesOnlyUpstreamUsage(t *testing.T) {
	const model = "gpt-5.1"
	configs := []struct {
		name   string
		config groupprompt.Config
	}{
		{"disabled", groupprompt.Config{Content: "unused prefix"}},
		{"bundled_default", groupprompt.Config{Enabled: true}},
		{"group_custom", groupprompt.Config{Enabled: true, Content: "group prefix"}},
		{"model_custom", groupprompt.Config{Enabled: true, Content: "group prefix", ModelPrompts: map[string]string{model: "model prefix"}}},
	}
	responses := []struct {
		name, format, body, usage string
		input, read, write        int
	}{
		{"responses_uncached", "responses", `{"model":"gpt-5.1","input":"hello"}`, `{"input_tokens":1200,"output_tokens":12}`, 1200, 0, 0},
		{"chat_cached", "chat", `{"model":"gpt-5.1","messages":[{"role":"user","content":"hello"}]}`, `{"prompt_tokens":1200,"completion_tokens":12,"prompt_tokens_details":{"cached_tokens":300}}`, 900, 300, 0},
		{"responses_cache_read_write", "responses", `{"model":"gpt-5.1","input":"hello"}`, `{"input_tokens":1200,"output_tokens":12,"input_tokens_details":{"cached_tokens":300,"cache_write_tokens":100}}`, 800, 300, 100},
	}
	for _, response := range responses {
		for _, config := range configs {
			for _, subscriptionBilling := range []bool{false, true} {
				billingMode := "balance"
				if subscriptionBilling {
					billingMode = "subscription"
				}
				t.Run(response.name+"/"+config.name+"/"+billingMode, func(t *testing.T) {
					usageRepo := &openAIRecordUsageLogRepoStub{inserted: true}
					billingRepo := &openAIRecordUsageBillingRepoStub{}
					userRepo := &openAIRecordUsageUserRepoStub{}
					subRepo := &openAIRecordUsageSubRepoStub{}
					svc := newOpenAIRecordUsageServiceWithBillingRepoForTest(usageRepo, billingRepo, userRepo, subRepo, nil)
					ctx := groupprompt.WithConfig(context.Background(), config.config)
					prompt := groupprompt.FromContext(ctx).Prompt(model)
					body, err := groupprompt.Prepend([]byte(response.body), response.format, prompt)
					require.NoError(t, err)
					if config.config.Enabled {
						field := "instructions"
						if response.format == "chat" {
							field = "messages.0.content"
						}
						require.Equal(t, prompt, gjson.GetBytes(body, field).String())
					} else {
						require.Equal(t, response.body, string(body))
					}
					again, err := groupprompt.Prepend(body, response.format, prompt)
					require.NoError(t, err)
					require.Equal(t, body, again, "gateway retries must not duplicate the prefix")
					require.Zero(t, billingRepo.calls, "injection alone must not settle any charge")
					require.Zero(t, userRepo.deductCalls)
					require.Zero(t, subRepo.incrementCalls)

					usage, ok := openAIUsageFromGJSON(gjson.Parse(response.usage))
					require.True(t, ok)
					group := &Group{ID: 88, RateMultiplier: 1}
					var subscription *UserSubscription
					if subscriptionBilling {
						group.SubscriptionType = SubscriptionTypeSubscription
						subscription = &UserSubscription{ID: 99}
					}
					err = svc.RecordUsage(ctx, &OpenAIRecordUsageInput{
						Result: &OpenAIForwardResult{
							RequestID: "group-prompt-billing-test",
							Usage:     usage,
							Model:     model,
							Duration:  time.Second,
						},
						APIKey:       &APIKey{ID: 100, GroupID: i64p(group.ID), Group: group},
						User:         &User{ID: 200},
						Account:      &Account{ID: 300},
						Subscription: subscription,
					})
					require.NoError(t, err)
					require.Equal(t, 1, billingRepo.calls, "only the existing settlement path may bill")
					require.Equal(t, 1, usageRepo.calls)
					require.Zero(t, userRepo.deductCalls, "atomic billing must not also run the legacy deduction")
					require.Zero(t, subRepo.incrementCalls)
					cmd := billingRepo.lastCmd
					require.NotNil(t, cmd)
					require.Equal(t, response.input, cmd.InputTokens)
					require.Equal(t, response.read, cmd.CacheReadTokens)
					require.Equal(t, response.write, cmd.CacheCreationTokens)
					require.Equal(t, 1200, cmd.InputTokens+cmd.CacheReadTokens+cmd.CacheCreationTokens)
					require.Equal(t, 12, cmd.OutputTokens)
					expected := expectedOpenAICost(t, svc, model, usage, 1)
					require.Greater(t, expected.ActualCost, 0.0)
					require.InDelta(t, expected.ActualCost, usageRepo.lastLog.ActualCost, 1e-12)
					if subscriptionBilling {
						require.Equal(t, BillingTypeSubscription, cmd.BillingType)
						require.Zero(t, cmd.BalanceCost)
						require.Equal(t, QuantizeUsageBillingAmount(expected.ActualCost), cmd.SubscriptionCost)
					} else {
						require.Equal(t, BillingTypeBalance, cmd.BillingType)
						require.Zero(t, cmd.SubscriptionCost)
						require.Equal(t, QuantizeUsageBillingAmount(expected.ActualCost), cmd.BalanceCost)
					}
				})
			}
		}
	}
}
