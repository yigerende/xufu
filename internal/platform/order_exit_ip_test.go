package platform

import (
	"context"
	"errors"
	"net/http"
	"net/url"
	"strings"
	"testing"
	"time"

	"xupay/internal/cslive"
)

func mockOrderIP(t *testing.T, a *App, ip string) {
	t.Helper()
	a.orderIPClient = func(endpoint string, _ time.Duration) (*http.Client, func(), error) {
		if endpoint == "" {
			t.Fatal("probe lost bound proxy")
		}
		return &http.Client{Transport: legacyRoundTrip(func(req *http.Request) (*http.Response, error) {
			if req.URL.String() != "https://ipinfo.io/json" || req.Method != "GET" || req.Header.Get("Authorization") != "" || req.Header.Get("Cookie") != "" || req.Body != nil {
				t.Fatal("exit probe carried credentials or used another target")
			}
			return legacyReply(200, M{"ip": ip}), nil
		})}, func() {}, nil
	}
}

func TestOrderExitIPGate(t *testing.T) {
	for _, mode := range []string{"same", "ipv6", "changed", "missing", "invalid", "http", "redirect", "network", "expired", "lease", "revoked_during_probe"} {
		t.Run(mode, func(t *testing.T) {
			h, r, _ := nativeFixture(t, false)
			expected, observed := "203.0.113.2", "203.0.113.2"
			want := ""
			switch mode {
			case "ipv6":
				expected, observed = "2001:db8::1", "2001:db8:0:0:0:0:0:1"
			case "changed":
				observed, want = "203.0.113.3", "proxy_session_changed"
			case "missing":
				expected, want = "", "proxy_ip_missing"
			case "invalid":
				observed, want = "PRIVATE_RAW_RESPONSE", "proxy_ip_check_failed"
			case "http", "redirect", "network":
				want = "proxy_ip_check_failed"
			case "expired":
				want = "proxy_session_expired"
			case "lease", "revoked_during_probe":
				want = "proxy_session_unavailable"
			}
			_, err := h.a.DB.Exec(r.ctx, "UPDATE order_proxy_sessions SET expected_ip=$2 WHERE order_id=$1", r.id, expected)
			if err != nil {
				t.Fatal(err)
			}
			var enc string
			h.a.DB.QueryRow(r.ctx, "SELECT endpoint_enc FROM order_proxy_sessions WHERE order_id=$1", r.id).Scan(&enc)
			original, _ := h.a.decrypt(enc)
			calls := 0
			h.a.orderIPClient = func(endpoint string, _ time.Duration) (*http.Client, func(), error) {
				if endpoint != original {
					t.Fatal("changed supplier endpoint")
				}
				return &http.Client{Transport: legacyRoundTrip(func(req *http.Request) (*http.Response, error) {
					calls++
					if req.URL.String() != "https://ipinfo.io/json" || req.Header.Get("Authorization") != "" || req.Header.Get("Cookie") != "" || req.Body != nil {
						t.Fatal("probe boundary violated")
					}
					if mode == "network" {
						return nil, errors.New("PRIVATE_PROXY_PASSWORD")
					}
					status := 200
					if mode == "http" {
						status = 503
					}
					if mode == "redirect" {
						status = 302
					}
					if mode == "revoked_during_probe" {
						h.a.DB.Exec(r.ctx, "UPDATE subscription_protocol SET lease_token='stale' WHERE order_id=$1", r.id)
					}
					res := legacyReply(status, M{"ip": observed})
					res.Header.Set("Location", "https://untrusted.example/PRIVATE")
					return res, nil
				})}, func() {}, nil
			}
			if mode == "expired" {
				h.a.DB.Exec(r.ctx, "UPDATE order_proxy_sessions SET expires_at=now()-interval '1 second' WHERE order_id=$1", r.id)
			}
			if mode == "lease" {
				r.lease = "stale"
			}
			_, err = h.a.orderExitIP(r.ctx, r.id, r.guardProxy, true)
			if cslive.ErrorCode(err) != want {
				t.Fatalf("got %v, want %s", err, want)
			}
			wantCalls := 1
			if mode == "missing" || mode == "expired" || mode == "lease" {
				wantCalls = 0
			}
			if calls != wantCalls {
				t.Fatal("unexpected probe retry", calls)
			}
			var saved, after string
			h.a.DB.QueryRow(r.ctx, "SELECT expected_ip,endpoint_enc FROM order_proxy_sessions WHERE order_id=$1", r.id).Scan(&saved, &after)
			if saved != expected || after != enc {
				t.Fatal("payment check changed baseline or proxy")
			}
			admin := h.request("GET", "/api/admin/orders/"+r.id, nil, h.admin, nil)
			if strings.Contains(admin.Body.String(), "PRIVATE_RAW_RESPONSE") || strings.Contains(admin.Body.String(), "PRIVATE_PROXY_PASSWORD") {
				t.Fatal("probe diagnostic leaked raw data")
			}
		})
	}
}

func TestProtocolPaymentWriteIPGate(t *testing.T) {
	h, r, _ := nativeFixture(t, false)
	mockOrderIP(t, h.a, "203.0.113.2")
	// The real new-checkout hook, not a fixture prefilled baseline.
	if err := r.beforeRequest(r.ctx, "POST", checkoutEndpoint); err != nil {
		t.Fatal(err)
	}
	var saved string
	h.a.DB.QueryRow(r.ctx, "SELECT expected_ip FROM order_proxy_sessions WHERE order_id=$1", r.id).Scan(&saved)
	if saved != "203.0.113.2" {
		t.Fatal("new checkout failed to save baseline")
	}
	for _, target := range []string{
		"https://api.stripe.com/v1/payment_methods", "https://api.stripe.com/v1/confirmation_tokens",
		"https://api.stripe.com/v1/payment_pages/cs_test_Fixture/confirm", "https://api.stripe.com/v1/payment_intents/pi_Fixture/confirm",
		checkoutEndpoint + "/confirm", checkoutEndpoint + "/approve", "https://chatgpt.com/backend-api/subscriptions/update",
	} {
		mockOrderIP(t, h.a, "203.0.113.2")
		if err := r.beforeRequest(r.ctx, "POST", target); err != nil {
			t.Fatal("same exit blocked", err)
		}
		mockOrderIP(t, h.a, "203.0.113.3")
		if err := r.beforeRequest(r.ctx, "POST", target); cslive.ErrorCode(err) != "proxy_session_changed" {
			t.Fatal("changed exit passed payment gate", err)
		}
	}
	h.a.orderIPClient = func(string, time.Duration) (*http.Client, func(), error) {
		t.Fatal("ordinary request probed exit")
		return nil, nil, nil
	}
	for _, target := range []string{"https://chatgpt.com/backend-api/subscriptions", "https://api.stripe.com/v1/payment_intents/pi_Fixture"} {
		if err := r.beforeRequest(r.ctx, "GET", target); err != nil {
			t.Fatal(err)
		}
	}
	if err := r.beforeRequest(r.ctx, "POST", "https://chatgpt.com/backend-api/subscriptions/cancel"); err != nil {
		t.Fatal(err)
	}
}

func TestLegacyPaymentWriteIPGate(t *testing.T) {
	for _, path := range []string{"/payment_methods", "/confirmation_tokens", "/payment_pages/cs_test_Offline/confirm", "/payment_intents/pi_Offline/confirm"} {
		for _, block := range []bool{false, true} {
			in := legacyTestInput()
			checks, writes := 0, 0
			in.CheckExitIP = func(context.Context) error {
				checks++
				if block {
					return errors.New("proxy_session_changed")
				}
				return nil
			}
			p := &legacyPayment{in: in, ctx: context.Background(), events: make(chan paymentEvent, 8), client: &http.Client{Transport: legacyRoundTrip(func(*http.Request) (*http.Response, error) {
				if checks != 1 {
					t.Fatal("payment reached transport before IP check")
				}
				writes++
				return legacyReply(200, M{}), nil
			})}}
			_, err := p.stripe(p.ctx, "POST", path, url.Values{}, "confirming")
			if checks != 1 || (block && (err == nil || writes != 0 || p.sent)) || (!block && (err != nil || writes != 1)) {
				t.Fatalf("unsafe payment gate: %s block=%v writes=%d err=%v", path, block, writes, err)
			}
		}
	}
}
