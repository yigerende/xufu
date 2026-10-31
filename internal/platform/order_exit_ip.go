package platform

import (
	"context"
	"encoding/json"
	"io"
	"net"
	"net/http"
	"net/url"
	"strings"
	"time"

	"xupay/internal/cslive"
)

// Sample through the saved supplier endpoint, including the configured local
// proxy chain. Never allocate a replacement session or send account/card data.
// A payment check cannot manufacture a missing checkout baseline.
func (a *App) orderExitIP(ctx context.Context, id string, guard func(context.Context) error, compare bool) (ip net.IP, resultErr error) {
	started := time.Now()
	defer func() {
		code, state := "exit_ip_recorded", "completed"
		if compare {
			code = "exit_ip_matched"
		}
		if resultErr != nil {
			code, state = protocolPublicCode(cslive.ErrorCode(resultErr)), "failed"
		}
		a.paymentTrace(context.WithoutCancel(ctx), id, "network", state, code, time.Since(started))
	}()
	ctx, cancel := context.WithTimeout(ctx, 8*time.Second)
	defer cancel()
	if err := guard(ctx); err != nil {
		return nil, err
	}
	var enc, expected string
	if err := a.DB.QueryRow(ctx, "SELECT endpoint_enc,expected_ip FROM order_proxy_sessions WHERE order_id=$1", id).Scan(&enc, &expected); err != nil || enc == "" {
		return nil, cslive.Failure("proxy_session_unavailable")
	}
	baseline := net.ParseIP(expected)
	if compare && baseline == nil {
		return nil, cslive.Failure("proxy_ip_missing")
	}
	endpoint, err := a.decrypt(enc)
	if err != nil {
		return nil, cslive.Failure("proxy_session_unavailable")
	}
	factory := newPaymentClient
	if a.orderIPClient != nil {
		factory = a.orderIPClient
	}
	client, closeClient, err := factory(endpoint, 8*time.Second)
	if err != nil {
		return nil, cslive.Failure("proxy_ip_check_failed")
	}
	defer closeClient()
	// Enforce no redirects even for an injected transport. Probe failures must
	// not make a second request or fall back to a direct connection.
	probe := *client
	probe.CheckRedirect = func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }
	req, err := http.NewRequestWithContext(ctx, "GET", "https://ipinfo.io/json", nil)
	if err != nil {
		return nil, cslive.Failure("proxy_ip_check_failed")
	}
	req.Header.Set("Accept", "application/json")
	res, err := probe.Do(req)
	if err != nil {
		return nil, cslive.Failure("proxy_ip_check_failed")
	}
	defer res.Body.Close()
	var data struct {
		IP string `json:"ip"`
	}
	if res.StatusCode != http.StatusOK || json.NewDecoder(io.LimitReader(res.Body, 64*1024)).Decode(&data) != nil {
		return nil, cslive.Failure("proxy_ip_check_failed")
	}
	ip = net.ParseIP(data.IP)
	if ip == nil {
		return nil, cslive.Failure("proxy_ip_check_failed")
	}
	if err = guard(ctx); err != nil {
		return nil, err
	}
	if compare && !baseline.Equal(ip) {
		return nil, cslive.Failure("proxy_session_changed")
	}
	return ip, nil
}

// Cover card/token creation and every payment confirmation/approval, including
// historical subscription upgrades. Ordinary reads and cancellation stay on
// the original endpoint with lease checks, without extra exit probes.
func paymentWriteKind(method string, u *url.URL) string {
	if method != "POST" || u == nil {
		return ""
	}
	if u.Hostname() == "api.stripe.com" {
		switch u.Path {
		case "/v1/payment_methods":
			return "payment_method_create"
		case "/v1/confirmation_tokens":
			return "payment_token_create"
		}
		if strings.HasSuffix(u.Path, "/confirm") && (strings.HasPrefix(u.Path, "/v1/payment_pages/") || strings.HasPrefix(u.Path, "/v1/payment_intents/") || strings.HasPrefix(u.Path, "/v1/setup_intents/") || strings.HasPrefix(u.Path, "/v1/billing_portal/sessions/")) {
			return "payment_confirm"
		}
	}
	if u.Hostname() == "chatgpt.com" {
		switch u.Path {
		case "/backend-api/payments/checkout/confirm", "/backend-api/payments/checkout/approve", "/backend-api/subscriptions/update", "/backend-api/subscriptions/renew", "/backend-api/payments/subscription/update":
			return "payment_confirm"
		case "/backend-api/payments/payment_methods":
			return "payment_method_create"
		}
	}
	return ""
}
