package platform

import (
	"context"
	"crypto/tls"
	"errors"
	"net"
	"net/http/httptrace"
	"net/url"
	"strings"
	"sync"
	"time"
	"xupay/internal/cslive"
)

var safeProviderCodes = map[string]bool{
	"parameter_missing": true, "parameter_unknown": true, "parameter_invalid_empty": true, "parameter_invalid_integer": true, "parameter_invalid_string_blank": true,
	"invalid_expiry_month": true, "invalid_expiry_year": true, "invalid_cvc": true, "invalid_number": true, "email_invalid": true, "api_key_expired": true,
	"account_deactivated": true, "account_deleted": true, "invalid_workspace_selected": true, "token_expired": true, "invalid_token": true, "RefreshAccessTokenError": true,
	"subscription_not_found": true, "card_declined": true, "expired_card": true, "incorrect_cvc": true, "incorrect_number": true, "incorrect_zip": true,
	"insufficient_funds": true, "do_not_honor": true, "generic_decline": true, "authentication_required": true, "payment_intent_authentication_failure": true,
	"payment_intent_unexpected_state": true, "billing_automatic_tax_unrecognized_customer_location": true, "rate_limit_exceeded": true, "processing_error": true,
}

var paymentErrorEnums = map[string]string{
	"provider_error_type": "invalid_request_error card_error api_error authentication_error idempotency_error rate_limit_error permission_error",
	"provider_reason":     "raw_card_api_restricted secret_key_required invalid_api_version",
	"provider_param":      "type card card[number] card[exp_month] card[exp_year] card[cvc] billing_details billing_details[name] billing_details[email] billing_details[address] billing_details[address][line1] billing_details[address][line2] billing_details[address][city] billing_details[address][state] billing_details[address][postal_code] billing_details[address][country] key _stripe_version payment_user_agent payment_method payment_method_data confirmation_token client_secret expected_amount expected_payment_method_type setup_future_usage return_url eid client_attribution_metadata client_attribution_metadata[client_session_id] client_attribution_metadata[checkout_session_id] client_attribution_metadata[checkout_config_id] client_attribution_metadata[merchant_integration_source] client_attribution_metadata[merchant_integration_version] client_attribution_metadata[merchant_integration_subtype] client_attribution_metadata[payment_method_selection_flow] guid muid sid link_brand rv_timestamp passive_captcha_token passive_captcha_ekey",
}

var merchantDiagnosticEnums = map[string]string{
	"page_event":    "navigated dom_ready script_error response resource_loaded request_failed request_blocked auth_observed approval_observed approval_not_requested",
	"page_host":     "chatgpt.com cdn.oaistatic.com tcr9i.chat.openai.com auth.openai.com auth0.openai.com sentinel.openai.com challenges.cloudflare.com other",
	"page_route":    "merchant_return auth_session login checkout other",
	"page_resource": "document script stylesheet xhr fetch image font other",
	"page_auth":     "present missing unknown",
}

func networkReason(err error) string {
	if err == nil {
		return ""
	}
	var n net.Error
	if errors.As(err, &n) && n.Timeout() {
		return "timeout"
	}
	message := strings.ToLower(err.Error())
	if strings.Contains(message, "proxyconnect") || strings.Contains(message, "connect tunnel") {
		return "proxy_connect"
	}
	if strings.Contains(message, "tls") || strings.Contains(message, "certificate") {
		return "tls"
	}
	var dns *net.DNSError
	if errors.As(err, &dns) {
		return "dns"
	}
	return "connection"
}
func requestTimings(ctx context.Context) (context.Context, func() M) {
	var mu sync.Mutex
	starts := map[string]time.Time{}
	values := M{}
	began := time.Now()
	start := func(key string) { mu.Lock(); starts[key] = time.Now(); mu.Unlock() }
	end := func(key string) {
		mu.Lock()
		if at, ok := starts[key]; ok {
			values[key+"_ms"] = time.Since(at).Milliseconds()
		}
		mu.Unlock()
	}
	trace := &httptrace.ClientTrace{
		DNSStart: func(httptrace.DNSStartInfo) { start("dns") }, DNSDone: func(httptrace.DNSDoneInfo) { end("dns") },
		ConnectStart: func(string, string) { start("connect") }, ConnectDone: func(string, string, error) { end("connect") },
		TLSHandshakeStart: func() { start("tls") }, TLSHandshakeDone: func(tls.ConnectionState, error) { end("tls") },
		GotFirstResponseByte: func() { mu.Lock(); values["first_byte_ms"] = time.Since(began).Milliseconds(); mu.Unlock() },
	}
	return httptrace.WithClientTrace(ctx, trace), func() M {
		mu.Lock()
		defer mu.Unlock()
		out := M{}
		for k, v := range values {
			out[k] = v
		}
		return out
	}
}
func providerRequestKind(method, target string) string {
	u, err := url.Parse(target)
	if err != nil || u.Scheme != "https" {
		return "other"
	}
	if method == "GET" && u.Hostname() == "ipinfo.io" && u.Path == "/json" {
		return "exit_probe"
	}
	if u.Hostname() == "chatgpt.com" {
		switch {
		case method == "POST" && u.Path == "/backend-api/payments/checkout":
			return "checkout_create"
		case method == "GET" && u.Path == "/api/auth/session":
			return "session_read"
		case method == "GET" && strings.HasPrefix(u.Path, "/backend-api/accounts/check/"):
			return "account_read"
		case method == "GET" && u.Path == "/backend-api/subscriptions":
			return "subscription_read"
		}
	}
	if method == "POST" && u.Hostname() == "api.stripe.com" && checkoutInitPath.MatchString(u.Path) {
		return "checkout_initialize"
	}
	return "other"
}

func safeDiagnosticDetails(input M) M {
	out := M{}
	for k, v := range input {
		switch k {
		case "edge_request_id":
			if s, ok := v.(string); ok && safeEdgeRequestID.MatchString(s) {
				out[k] = s
			}
		case "request_kind":
			if s, ok := v.(string); ok {
				switch s {
				case "exit_probe", "checkout_create", "checkout_initialize", "session_read", "account_read", "subscription_read", "payment_method_create", "payment_token_create", "payment_confirm", "payment_read", "billing_update", "other":
					out[k] = s
				}
			}
		case "page_event", "page_host", "page_route", "page_resource", "page_auth":
			if s, ok := v.(string); ok && s != "" && !strings.ContainsAny(s, " \t\r\n") && strings.Contains(" "+merchantDiagnosticEnums[k]+" ", " "+s+" ") {
				out[k] = s
			}
		case "page_network_error":
			if s := safeActionDiagnostic("sdk_network_error", v); s != "" {
				out[k] = s
			}
		case "sdk_stage", "sdk_resource", "sdk_event", "sdk_issue", "sdk_host", "sdk_route", "sdk_method", "sdk_request_type", "sdk_network_error":
			if s := safeActionDiagnostic(k, v); s != "" {
				out[k] = s
			}
		case "sdk_resource_key":
			if s, ok := v.(string); ok && actionResourceKey.MatchString(s) {
				out[k] = s
			}
		case "sdk_code":
			if s, ok := v.(string); ok {
				out[k] = safeActionCode(s)
			}
		case "sdk_outcome":
			if s, ok := v.(string); ok {
				out[k] = safeActionOutcome(s)
			}
		case "next_action_type", "sdk_action_type":
			if s, ok := v.(string); ok && s != "" {
				out[k] = cslive.SafeActionType(s)
			}
		case "raw_status", "raw_state", "raw_payment_status":
			if s, ok := v.(string); ok && s != "" {
				out[k] = cslive.SafePaymentStatus(s)
			}
		case "diagnostic_source":
			if s, ok := v.(string); ok && s != "" {
				out[k] = cslive.SafeDiagnosticSource(s)
			}
		case "payment_state":
			if s, ok := v.(string); ok && (s == "charged" || s == "not_charged" || s == "action_required" || s == "authorized" || s == "pending" || s == "unknown") {
				out[k] = s
			}
		case "sdk_has_stripe_js", "sdk_has_3ds_source", "sdk_waiting", "challenge_required":
			if b, ok := v.(bool); ok {
				out[k] = b
			}
		case "dns_ms", "connect_ms", "tls_ms", "first_byte_ms", "connection_ready_ms", "request_write_ms", "response_wait_ms", "network_stage_elapsed_ms", "request_attempt", "sdk_wait_ms", "sdk_duration_ms", "sdk_guard_ms", "sdk_connect_ms", "sdk_tls_ms", "sdk_response_ms", "sdk_request", "sdk_requests", "sdk_completed", "sdk_failed", "sdk_pending":
			var n int64
			switch x := v.(type) {
			case int:
				n = int64(x)
			case int64:
				n = x
			case float64:
				n = int64(x)
			default:
				continue
			}
			if n >= 0 && n <= 3600000 {
				if k == "request_attempt" && (n < 1 || n > 10) {
					continue
				}
				out[k] = n
			}
		case "network_stage":
			if s, ok := v.(string); ok && (s == "connection" || s == "request_write" || s == "response_headers" || s == "response_body") {
				out[k] = s
			}
		case "protocol_code":
			if s, ok := v.(string); ok && protocolDiagnosticCodes[s] {
				out[k] = s
			}
		case "network_reason":
			if s, ok := v.(string); ok && (s == "timeout" || s == "proxy_connect" || s == "tls" || s == "dns" || s == "connection") {
				out[k] = s
			}
		case "provider_code", "decline_code":
			if s, ok := v.(string); ok && safeProviderCodes[s] {
				out[k] = s
			}
		case "stripe_api_version":
			// Versions are application constants. Never expose an upstream-supplied
			// value or relax the whitespace filter for other diagnostic fields.
			if s, ok := v.(string); ok && (s == legacyHostedStripeVersion || s == legacyCustomStripeVersion) {
				out[k] = s
			}
		case "provider_error_type", "provider_param", "provider_reason":
			if s, ok := v.(string); ok && s != "" && !strings.ContainsAny(s, " \t\r\n") && strings.Contains(" "+paymentErrorEnums[k]+" ", " "+s+" ") {
				out[k] = s
			}
		case "response_kind":
			if s, ok := v.(string); ok && (s == "json" || s == "html" || s == "empty" || s == "other") {
				out[k] = s
			}
		}
	}
	return out
}
