package service

import (
	"net/url"
	"strings"

	"github.com/Wei-Shaw/sub2api/internal/payment"
	infraerrors "github.com/Wei-Shaw/sub2api/internal/pkg/errors"
)

// PaymentSourceAiBox opts an authenticated extension order into the
// administrator-configured Alipay return page. It conveys navigation intent,
// not authentication or any different billing/fulfillment authority.
const PaymentSourceAiBox = "ai_box"

// resolveProviderReturnURL applies the trusted Alipay instance configuration
// only for Ai box orders, before resume-token creation and gateway signing.
// Website orders retain the original client return URL policy.
func resolveProviderReturnURL(req CreateOrderRequest, sel *payment.InstanceSelection) (string, error) {
	clientURL, err := CanonicalizeReturnURL(req.ReturnURL, req.SrcHost, req.SrcURL)
	if err != nil {
		return "", err
	}
	if sel == nil || sel.ProviderKey != payment.TypeAlipay || NormalizePaymentSource(req.PaymentSource) != PaymentSourceAiBox {
		return clientURL, nil
	}
	configured := strings.TrimSpace(sel.Config["returnUrl"])
	if configured == "" {
		return clientURL, nil
	}
	parsed, err := url.Parse(configured)
	if err != nil {
		return "", infraerrors.ServiceUnavailable("INVALID_PAYMENT_RETURN_URL", "Alipay return URL configuration is invalid")
	}
	// Only administrator-controlled configuration may select another host.
	// Reuse the strict scheme, userinfo, host, path and query validation.
	canonical, err := CanonicalizeReturnURL(configured, parsed.Host, "")
	if err != nil {
		return "", infraerrors.ServiceUnavailable("INVALID_PAYMENT_RETURN_URL", "Alipay return URL configuration is invalid")
	}
	return canonical, nil
}
