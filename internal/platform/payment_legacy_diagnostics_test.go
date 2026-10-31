package platform

import (
	"context"
	"encoding/json"
	"net/http"
	"net/url"
	"strings"
	"testing"
	"time"
)

func TestLegacyPaymentRejectionDiagnostics(t *testing.T) {
	for _, mode := range []string{"parameter", "raw_card", "permission", "version", "private"} {
		t.Run(mode, func(t *testing.T) {
			problem := M{"type": "invalid_request_error", "code": "parameter_missing", "param": "card[exp_year]", "message": "PRIVATE_CARD_4242424242424242 PRIVATE_CVC_123 PRIVATE_AT"}
			if mode == "raw_card" {
				problem["message"] = "Sending credit card numbers directly to the Stripe API is generally unsafe. PRIVATE_CARD_4242424242424242"
			}
			if mode == "permission" {
				problem["message"] = "This call used a publishable API key but requires a secret API key. PRIVATE_AT"
			}
			if mode == "version" {
				problem["message"] = "Invalid Stripe API version: PRIVATE_VERSION PRIVATE_AT"
			}
			if mode == "private" {
				for _, k := range []string{"type", "code", "param"} {
					problem[k] = "PRIVATE_FIELD"
				}
			}
			p := &legacyPayment{in: legacyTestInput(), ctx: context.Background(), events: make(chan paymentEvent, 8), client: &http.Client{Transport: legacyRoundTrip(func(*http.Request) (*http.Response, error) {
				res := legacyReply(400, M{"error": problem})
				res.Header.Set("Request-ID", "req_Offline12345678")
				return res, nil
			})}}
			_, err := p.stripe(p.ctx, "POST", "/payment_methods", url.Values{}, "card_prepare")
			if err == nil || err.Error() != "request_rejected" || p.sent {
				t.Fatal("incorrect error/submitted classification", err)
			}
			e := <-p.events
			if e.Type != "diagnostic" || e.Phase != "card_prepare" || e.HTTPStatus != 400 || e.RequestID != "req_Offline12345678" || e.Diagnostics["request_kind"] != "payment_method_create" {
				t.Fatal("missing request context")
			}
			if mode != "private" && (e.Diagnostics["provider_error_type"] != "invalid_request_error" || e.Diagnostics["provider_param"] != "card[exp_year]" || e.Diagnostics["provider_code"] != "parameter_missing") {
				t.Fatal("error detail discarded")
			}
			if mode == "raw_card" && e.Diagnostics["provider_reason"] != "raw_card_api_restricted" {
				t.Fatal("missing fixed policy reason")
			}
			if mode == "permission" && e.Diagnostics["provider_reason"] != "secret_key_required" {
				t.Fatal("missing fixed permission reason")
			}
			if mode == "version" && e.Diagnostics["provider_reason"] != "invalid_api_version" {
				t.Fatal("missing fixed API version reason")
			}
			if e.Diagnostics["stripe_api_version"] != "2020-08-27;custom_checkout_beta=v1" {
				t.Fatal("request API version missing")
			}
			raw, _ := json.Marshal(e)
			if strings.Contains(string(raw), "PRIVATE") || strings.Contains(string(raw), "4242424242424242") {
				t.Fatal("raw error data leaked")
			}
		})
	}
}

func TestLegacyPaymentVersionDiagnosticPrivacy(t *testing.T) {
	for _, value := range []string{"PRIVATE_AT", "2020-08-27;custom_checkout_beta=v1 PRIVATE_AT", "", "2026-03-25.dahlia"} {
		if len(safeDiagnosticDetails(M{"stripe_api_version": value})) != 0 {
			t.Fatal("unrecognized API version exposed")
		}
	}
	p := &legacyPayment{custom: true}
	if safeDiagnosticDetails(M{"stripe_api_version": p.version()})["stripe_api_version"] != p.version() {
		t.Fatal("custom API version discarded")
	}
}

func TestLegacyPaymentDiagnosticsPersistencePrivacy(t *testing.T) {
	h, r, _ := nativeFixture(t, false)
	h.a.saveProviderDiagnostic(providerDiagnostic{Order: r.id, Phase: "card_prepare", Code: "request_rejected", Status: 400, At: time.Now(), Details: M{"request_kind": "payment_method_create", "provider_error_type": "invalid_request_error", "provider_param": "card[exp_year]", "provider_code": "parameter_missing", "message": "PRIVATE_CARD_4242424242424242", "raw_body": "PRIVATE_AT"}})
	admin := h.request("GET", "/api/admin/orders/"+r.id, nil, h.admin, nil).Body.String()
	for _, want := range []string{"card_prepare", "payment_method_create", "invalid_request_error", "card[exp_year]", "parameter_missing"} {
		if !strings.Contains(admin, want) {
			t.Fatal("lost persisted diagnostic", want)
		}
	}
	for _, route := range []string{"/api/admin/orders/", "/api/orders/"} {
		cookie := h.member
		if strings.Contains(route, "admin") {
			cookie = h.admin
		}
		raw := h.request("GET", route+r.id, nil, cookie, nil).Body.String()
		if strings.Contains(raw, "PRIVATE") || strings.Contains(raw, "4242424242424242") {
			t.Fatal("sensitive diagnostic projected")
		}
		if cookie == h.member && strings.Contains(raw, "provider_error_type") {
			t.Fatal("customer saw internal diagnostic")
		}
	}
	for _, key := range []string{"provider_error_type", "provider_param", "provider_reason", "provider_code"} {
		if len(safeDiagnosticDetails(M{key: "card[number] PRIVATE"})) != 0 {
			t.Fatal("untrusted enum retained")
		}
	}
}
