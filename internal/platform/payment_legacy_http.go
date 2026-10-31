package platform

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/url"
	"strings"
	"time"
)

const (
	legacyHostedStripeVersion = "2020-08-27;custom_checkout_beta=v1"
	legacyCustomStripeVersion = "2025-03-31.basil; custom_checkout_beta=v1; checkout_server_update_beta=v1; checkout_manual_approval_preview=v1"
)

// This client uses ordinary HTTPS through the original order's private relay.
// It has no browser, challenge solver, device telemetry, or retrying transport.
func legacyPaymentHTTP(in paymentInput) (*http.Client, func(), error) {
	if in.Proxy == nil || in.Proxy.Username == "" || in.Proxy.Password == "" {
		return nil, nil, errors.New("proxy_session_unavailable")
	}
	u, err := url.Parse(in.Proxy.Server)
	if err != nil || u.Scheme != "http" || u.Hostname() != "127.0.0.1" || u.Port() == "" || u.User != nil || u.RawQuery != "" || u.Fragment != "" || (u.Path != "" && u.Path != "/") {
		return nil, nil, errors.New("proxy_session_unavailable")
	}
	u.User = url.UserPassword(in.Proxy.Username, in.Proxy.Password)
	tr := &http.Transport{Proxy: http.ProxyURL(u), TLSHandshakeTimeout: 15 * time.Second, ResponseHeaderTimeout: 40 * time.Second, DisableKeepAlives: true}
	return &http.Client{Transport: tr, Timeout: 45 * time.Second, CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}, tr.CloseIdleConnections, nil
}

func (p *legacyPayment) guard(ctx context.Context) error {
	if ctx.Err() != nil {
		return errors.New("result_unconfirmed")
	}
	if p.in.ProxyExpiresAt <= time.Now().UnixMilli() {
		return errors.New("proxy_session_expired")
	}
	if p.in.Guard != nil {
		return p.in.Guard(ctx)
	}
	return nil
}

// Only fixed operations constructed below may carry credentials. No response
// URL is followed, and merchant cookies/AT are never sent to Stripe.
func (p *legacyPayment) request(ctx context.Context, method, target string, form url.Values, body M, phase string) (M, error) {
	u, err := url.Parse(target)
	if err != nil || u.Scheme != "https" || u.User != nil || u.Port() != "" || u.Fragment != "" {
		return nil, errors.New("unsupported_checkout")
	}
	stripe := u.Host == "api.stripe.com"
	merchant := u.Host == "chatgpt.com"
	if !stripe && !merchant {
		return nil, errors.New("unsupported_checkout")
	}
	if p.in.Mode == "check" && method != "GET" {
		return nil, errors.New("result_unconfirmed")
	}
	if err = p.guard(ctx); err != nil {
		return nil, err
	}
	kind := paymentWriteKind(method, u)
	if kind != "" {
		if p.in.CheckExitIP == nil {
			return nil, errors.New("proxy_ip_check_failed")
		}
		if err = p.in.CheckExitIP(ctx); err != nil {
			return nil, err
		}
		if err = p.guard(ctx); err != nil {
			return nil, err
		}
	}
	var payload []byte
	if form != nil {
		payload = []byte(form.Encode())
	} else if body != nil {
		payload, err = json.Marshal(body)
		if err != nil {
			return nil, errors.New("preparation_failed")
		}
	}
	req, err := http.NewRequestWithContext(ctx, method, target, bytes.NewReader(payload))
	if err != nil {
		return nil, errors.New("preparation_failed")
	}
	// Do not allow net/http to replay a payment on a reused connection.
	req.GetBody = nil
	req.Header.Set("Accept", "application/json")
	req.Header.Set("User-Agent", "Xupay/1.0")
	if form != nil {
		req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	} else if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	if merchant {
		if p.in.Authorization.AccessToken != "" {
			req.Header.Set("Authorization", "Bearer "+p.in.Authorization.AccessToken)
		}
		if p.in.SessionCookies != "" {
			req.Header.Set("Cookie", p.in.SessionCookies)
		}
		if p.in.Authorization.AccountID != "" {
			req.Header.Set("ChatGPT-Account-ID", p.in.Authorization.AccountID)
		}
	} else {
		if p.custom {
			req.Header.Set("Stripe-Version", p.version())
		} else if origin := p.hostedOrigin(); origin != "" {
			// Hosted capture negotiates the payment version in the form only.
			// The source is the original checkout URL, never a response redirect.
			req.Header.Set("Origin", origin)
			req.Header.Set("Referer", origin+"/")
		}
		if method == "POST" && strings.HasSuffix(u.Path, "/confirm") {
			req.Header.Set("Idempotency-Key", "xufu-legacy-"+p.cp.CheckoutID+"-"+strings.TrimPrefix(u.Path, "/v1/"))
		}
	}
	started := time.Now()
	if kind == "payment_confirm" {
		p.stage("confirming")
		if !p.sent {
			p.sent = true
			p.emit(paymentEvent{Type: "submitted"})
		}
	}
	res, err := p.client.Do(req)
	if err != nil {
		p.emit(paymentEvent{Type: "diagnostic", Phase: phase, Code: "network_connection_failed", ElapsedMS: time.Since(started).Milliseconds()})
		return nil, errors.New("network_connection_failed")
	}
	defer res.Body.Close()
	raw, readErr := io.ReadAll(io.LimitReader(res.Body, 2*1024*1024+1))
	data := M{}
	dec := json.NewDecoder(bytes.NewReader(raw))
	dec.UseNumber()
	isJSON := readErr == nil && len(raw) <= 2*1024*1024 && dec.Decode(&data) == nil
	if kind == "" {
		kind = "payment_read"
		if method == "POST" {
			kind = "billing_update"
			if checkoutInitPath.MatchString(u.Path) {
				kind = "checkout_initialize"
			}
		}
	}
	details := M{"response_kind": "other", "request_kind": kind}
	if stripe {
		version := req.Header.Get("Stripe-Version")
		if v := form.Get("_stripe_version"); v != "" {
			version = v
		}
		if version != "" {
			details["stripe_api_version"] = version
		}
	}
	if isJSON {
		details["response_kind"] = "json"
	}
	problem := legacyMap(data["error"])
	details["provider_code"], details["decline_code"] = str(problem, "code"), str(problem, "decline_code")
	details["provider_error_type"], details["provider_param"] = str(problem, "type"), str(problem, "param")
	// Map only known policy messages to fixed labels. Never persist raw messages,
	// which can echo card values, emails, credentials or request payloads.
	message := strings.ToLower(str(problem, "message"))
	if strings.Contains(message, "sending credit card numbers directly to the stripe api") {
		details["provider_reason"] = "raw_card_api_restricted"
	} else if strings.Contains(message, "publishable api key") && strings.Contains(message, "secret api key") {
		details["provider_reason"] = "secret_key_required"
	} else if strings.Contains(message, "invalid stripe api version") || strings.Contains(message, "invalid api version") {
		details["provider_reason"] = "invalid_api_version"
	}
	requestID := res.Header.Get("Request-ID")
	if !safeRequestID.MatchString(requestID) {
		requestID = ""
	}
	code := ""
	if res.StatusCode >= 400 {
		code = "request_rejected"
	}
	p.emit(paymentEvent{Type: "diagnostic", Phase: phase, Code: code, HTTPStatus: res.StatusCode, RequestID: requestID, ElapsedMS: time.Since(started).Milliseconds(), Diagnostics: safeDiagnosticDetails(details)})
	if isJSON {
		for _, source := range []struct {
			name string
			node M
		}{{"response", data}, {"payment_intent", legacyMap(data["payment_intent"])}, {"submission_attempt", legacyMap(data["submission_attempt"])}} {
			if source.node == nil {
				continue
			}
			d := safeDiagnosticDetails(M{"diagnostic_source": source.name, "raw_status": str(source.node, "status"), "raw_state": str(source.node, "state"), "raw_payment_status": str(source.node, "payment_status"), "next_action_type": str(legacyMap(source.node["next_action"]), "type")})
			if len(d) > 1 {
				p.emit(paymentEvent{Type: "diagnostic", Phase: phase, HTTPStatus: res.StatusCode, Diagnostics: d})
			}
		}
	}
	if res.StatusCode == 403 || res.StatusCode == 401 && merchant {
		return nil, errors.New("verification_required")
	}
	if !isJSON {
		return nil, errors.New("invalid_response")
	}
	if problem != nil {
		if str(problem, "code") == "authentication_required" || str(problem, "code") == "captcha_required" || str(problem, "code") == "captcha_invalid" || str(problem, "param") == "passive_captcha_token" || str(problem, "param") == "passive_captcha_ekey" {
			return nil, errors.New("verification_required")
		}
		// Only an explicit card rejection can release funds after a submission.
		if res.StatusCode == 402 && str(problem, "type") == "card_error" && (str(problem, "code") == "card_declined" || str(problem, "code") == "expired_card" || str(problem, "code") == "incorrect_cvc" || str(problem, "code") == "incorrect_number") {
			intent := legacyMap(problem["payment_intent"])
			if intent == nil || str(intent, "status") == "requires_payment_method" {
				return nil, errors.New("payment_declined")
			}
		}
		return nil, errors.New("request_rejected")
	}
	if res.StatusCode < 200 || res.StatusCode >= 300 {
		return nil, errors.New("request_rejected")
	}
	return data, nil
}

func (p *legacyPayment) stripe(ctx context.Context, method, path string, form url.Values, phase string) (M, error) {
	if form == nil {
		form = url.Values{}
	}
	form.Set("key", p.cp.Key)
	// Hosted PaymentMethod creation and confirmation negotiate their version in
	// the form too. The captured init/tax/read requests do not include this field.
	if p.custom || method == "POST" && (path == "/payment_methods" || strings.HasSuffix(path, "/confirm")) {
		form.Set("_stripe_version", p.version())
	}
	target := "https://api.stripe.com/v1" + path
	if method == "GET" {
		target += "?" + form.Encode()
		form = nil
	}
	return p.request(ctx, method, target, form, nil, phase)
}

func (p *legacyPayment) version() string {
	if p.custom {
		return legacyCustomStripeVersion
	}
	// The legacy hosted browser capture uses this contract for both card
	// tokenization and confirmation; do not substitute the newer SDK's version.
	return legacyHostedStripeVersion
}
