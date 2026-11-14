//go:build unit

package service

import (
	"net/url"
	"strings"
	"testing"

	"github.com/Wei-Shaw/sub2api/internal/payment"
)

func TestResolveProviderReturnURL(t *testing.T) {
	t.Parallel()
	const local = "https://billing.example.test/payment/result"
	const external = "https://activation.example.test/payment/result"
	tests := []struct {
		name, source, provider, configured, client, want string
		wantErr                                          bool
	}{
		{"Ai box Alipay uses admin URL", PaymentSourceAiBox, payment.TypeAlipay, external, local, external, false},
		{"Ai box without client URL", PaymentSourceAiBox, payment.TypeAlipay, external, "", external, false},
		{"website retains its own URL", PaymentSourceHostedRedirect, payment.TypeAlipay, external, local, local, false},
		{"legacy website source unchanged", "", payment.TypeAlipay, external, local, local, false},
		{"unknown source unchanged", "other", payment.TypeAlipay, external, local, local, false},
		{"normalized Ai box marker", " AI_BOX ", payment.TypeAlipay, external, local, external, false},
		{"unconfigured preserves client", PaymentSourceAiBox, payment.TypeAlipay, "", local + "?discard=1#fragment", local, false},
		{"both absent", PaymentSourceAiBox, payment.TypeAlipay, "", "", "", false},
		{"non-Alipay unchanged", PaymentSourceAiBox, payment.TypeEasyPay, external, local, local, false},
		{"config query stripped", PaymentSourceAiBox, payment.TypeAlipay, external + "?trade_status=TRADE_SUCCESS#fragment", local, external, false},
		{"invalid client remains rejected", PaymentSourceAiBox, payment.TypeAlipay, external, "https://evil.test/payment/result", "", true},
		{"invalid configured scheme", PaymentSourceAiBox, payment.TypeAlipay, "javascript:alert(1)", local, "", true},
		{"invalid configured path", PaymentSourceAiBox, payment.TypeAlipay, "https://activation.example.test/other", local, "", true},
		{"configured userinfo rejected", PaymentSourceAiBox, payment.TypeAlipay, "https://user@activation.example.test/payment/result", local, "", true},
		{"configured relative URL rejected", PaymentSourceAiBox, payment.TypeAlipay, "/payment/result", local, "", true},
		{"invalid configured host rejected", PaymentSourceAiBox, payment.TypeAlipay, "https://a&b=c.test/payment/result", local, "", true},
		{"invalid config does not affect website", PaymentSourceHostedRedirect, payment.TypeAlipay, "javascript:alert(1)", local, local, false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			sel := &payment.InstanceSelection{ProviderKey: tt.provider, Config: map[string]string{"returnUrl": tt.configured}}
			got, err := resolveProviderReturnURL(CreateOrderRequest{ReturnURL: tt.client, SrcHost: "billing.example.test", PaymentSource: tt.source}, sel)
			if (err != nil) != tt.wantErr || got != tt.want {
				t.Fatalf("got (%q, %v), want (%q, error=%v)", got, err, tt.want, tt.wantErr)
			}
		})
	}
}

func TestWebsiteReturnURLMatchesOriginalBehavior(t *testing.T) {
	t.Parallel()
	for _, source := range []string{"", PaymentSourceHostedRedirect, PaymentSourceWechatInAppResume, "other"} {
		for _, raw := range []string{"", "https://billing.example.test/payment/result?x=1#fragment", "https://evil.test/payment/result", "/payment/result"} {
			req := CreateOrderRequest{PaymentSource: source, SrcHost: "billing.example.test", ReturnURL: raw}
			want, originalErr := CanonicalizeReturnURL(req.ReturnURL, req.SrcHost, req.SrcURL)
			got, err := resolveProviderReturnURL(req, &payment.InstanceSelection{
				ProviderKey: payment.TypeAlipay, Config: map[string]string{"returnUrl": "https://activation.example.test/payment/result"},
			})
			if got != want || (err != nil) != (originalErr != nil) {
				t.Fatalf("website return behavior changed for source=%q URL=%q", source, raw)
			}
		}
	}
}

func TestAlipayExternalReturnURLPreservesSignedResumeAndNotification(t *testing.T) {
	t.Parallel()
	sel := &payment.InstanceSelection{
		ProviderKey: payment.TypeAlipay, InstanceID: "alipay-fixture",
		Config: map[string]string{
			"returnUrl": "https://activation.example.test/payment/result",
			"notifyUrl": "https://billing.example.test/api/v1/payment/webhook/alipay",
		},
	}
	canonical, err := resolveProviderReturnURL(CreateOrderRequest{
		ReturnURL: "https://billing.example.test/payment/result", SrcHost: "billing.example.test",
		PaymentSource: PaymentSourceAiBox,
	}, sel)
	if err != nil {
		t.Fatal(err)
	}
	resume := NewPaymentResumeService([]byte("test-signing-key-only"))
	token, err := resume.CreateToken(ResumeTokenClaims{
		OrderID: 123, UserID: 456, ProviderInstanceID: sel.InstanceID,
		ProviderKey: sel.ProviderKey, PaymentType: payment.TypeAlipay, CanonicalReturnURL: canonical,
	})
	if err != nil {
		t.Fatal(err)
	}
	result, err := buildPaymentReturnURL(canonical, 123, "sub2_fixture", token)
	if err != nil {
		t.Fatal(err)
	}
	parsed, err := url.Parse(result)
	if err != nil {
		t.Fatal(err)
	}
	claims, err := resume.ParseToken(parsed.Query().Get("resume_token"))
	if err != nil {
		t.Fatal(err)
	}
	if parsed.Host != "activation.example.test" || parsed.Query().Get("out_trade_no") != "sub2_fixture" ||
		parsed.Query().Get("order_id") != "123" || claims.CanonicalReturnURL != canonical || claims.OrderID != 123 {
		t.Fatalf("return URL lost signed order context: %s", result)
	}
	providerReq := buildProviderCreatePaymentRequest(CreateOrderRequest{ReturnURL: result}, sel, "sub2_fixture", "1.00", "fixture")
	if providerReq.ReturnURL != result || !strings.HasPrefix(sel.Config["notifyUrl"], "https://billing.example.test/") {
		t.Fatal("provider return or notification configuration changed")
	}
}
