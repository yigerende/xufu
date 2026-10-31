package platform

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"strings"
	"testing"
	"time"
)

func refreshFixture(t *testing.T) (checkoutInput, time.Time) {
	t.Helper()
	v := testSession("refresh@example.com")
	v["sessionToken"] = "TEST_PRIVATE_SESSION"
	raw, _ := json.Marshal(v)
	info, e := inspectSession(raw)
	if e != nil {
		t.Fatal(e)
	}
	in, expiry, e := sessionCredentials(raw, info)
	if e != nil {
		t.Fatal(e)
	}
	return in, expiry
}

func TestSessionRefreshCookieIdentityAndExpiry(t *testing.T) {
	for _, mode := range []string{"success", "wrong_account", "wrong_email", "expired", "missing_token", "cookie_deleted", "redirect"} {
		t.Run(mode, func(t *testing.T) {
			in, expiry := refreshFixture(t)
			original := in.AccessToken
			if time.Until(expiry) < 23*time.Hour {
				t.Fatal("refreshable authorization lost retention")
			}
			calls := 0
			c := &http.Client{Transport: testRoundTrip(func(r *http.Request) (*http.Response, error) {
				calls++
				if r.Method != "GET" || r.URL.String() != sessionEndpoint || r.Header.Get("Authorization") != "" || r.Body != nil && r.ContentLength > 0 {
					t.Fatal("refresh request changed")
				}
				cookie, e := r.Cookie("__Secure-next-auth.session-token")
				if e != nil || cookie.Value != "TEST_PRIVATE_SESSION" {
					t.Fatal("cookie missing")
				}
				v := testSession("refresh@example.com")
				v["accessToken"] = str(v, "accessToken") + "NEW"
				switch mode {
				case "wrong_account":
					v["account"] = M{"id": "other"}
				case "wrong_email":
					v["user"] = M{"email": "other@example.com"}
				case "expired":
					v["expires"] = "2020-01-01T00:00:00Z"
				case "missing_token":
					v = M{"error": "RefreshAccessTokenError"}
				}
				res := providerResponse(200, v)
				res.Header.Add("Set-Cookie", "__Secure-next-auth.session-token=ROTATED_SESSION; Secure; HttpOnly; Path=/")
				if mode == "cookie_deleted" {
					res.Header.Set("Set-Cookie", "__Secure-next-auth.session-token=; Max-Age=0; Secure; Path=/")
					res.Header.Add("Set-Cookie", "__Secure-platform.session-token=; Max-Age=0; Secure; Path=/")
				}
				if mode == "redirect" {
					res.StatusCode = 302
					res.Header.Set("Location", "https://other.example/steal")
				}
				return res, nil
			})}
			err := refreshProviderSession(context.Background(), c, &in, "renewal")
			if mode == "success" || mode == "cookie_deleted" {
				if err != nil || in.AccessToken == original || in.SessionToken != "" {
					t.Fatal("refresh failed", err)
				}
				if mode == "success" && !strings.Contains(in.Cookies, "ROTATED_SESSION") {
					t.Fatal("rotation not saved")
				}
				if mode == "cookie_deleted" && in.Cookies != "" {
					t.Fatal("deleted session resurrected")
				}
			} else if err == nil || in.AccessToken != original {
				t.Fatal("invalid authorization accepted")
			}
			if calls != 1 {
				t.Fatal("refresh followed redirect or retried")
			}
		})
	}
}

func TestRefreshBoundedRecoveryAndLegacyInput(t *testing.T) {
	for _, available := range []bool{true, false} {
		in, _ := refreshFixture(t)
		if !available {
			in.SessionToken = ""
		}
		in.RefreshedAt = time.Now()
		in.RefreshPurpose = "renewal"
		refreshes, reads, saves := 0, 0, 0
		c := &http.Client{Transport: testRoundTrip(func(r *http.Request) (*http.Response, error) {
			if r.URL.Path == "/api/auth/session" {
				refreshes++
				return providerResponse(200, testSession(in.Email)), nil
			}
			reads++
			return providerResponse(401, M{"error": M{"code": "token_expired"}}), nil
		})}
		s := &providerSession{ctx: context.Background(), client: c, in: &in, purpose: "renewal", persist: func(checkoutInput) error { saves++; return nil }}
		if e := s.prepare(); e != nil {
			t.Fatal(e)
		}
		_, e := s.read(context.Background(), subscriptionEndpoint)
		if providerCode(e) != "invalid_session" {
			t.Fatal(e)
		}
		want := 0
		if available {
			want = 1
		}
		if refreshes != want || saves != want || reads != want+1 {
			t.Fatal("unbounded refresh", refreshes, saves, reads)
		}
	}
	// A worker must not use rotated authorization until it has persisted it.
	for _, purpose := range []string{"renewal", "activation"} {
		in, _ := refreshFixture(t)
		requests := 0
		s := &providerSession{ctx: context.Background(), in: &in, purpose: purpose, persist: func(checkoutInput) error { return ErrLeaseLost }, client: &http.Client{Transport: testRoundTrip(func(r *http.Request) (*http.Response, error) {
			requests++
			if r.URL.Path != "/api/auth/session" {
				t.Fatal("read before refreshed credentials were saved")
			}
			return providerResponse(200, testSession(in.Email)), nil
		})}}
		// Activation follows the backup: query first, refresh on an explicit
		// auth failure. Exercise that refresh boundary directly here.
		prepare := s.prepare
		if purpose == "activation" {
			prepare = s.refresh
		}
		if !errors.Is(prepare(), ErrLeaseLost) || requests != 1 {
			t.Fatal("lost lease ignored", purpose, requests)
		}
	}
}

func TestCheckoutPreflightDoesNotReplaceNativeCheckout(t *testing.T) {
	for _, mode := range []string{"free", "active", "wrong_account", "expired", "unauthorized"} {
		t.Run(mode, func(t *testing.T) {
			in, _ := refreshFixture(t)
			in.Product = "pro5x"
			in.Country = "PH"
			in.Mode = "long"
			if mode == "expired" {
				in.TokenExpiresAt = time.Now().Add(-time.Minute)
			}
			originalAT := in.AccessToken
			writes := 0
			c := &http.Client{Transport: testRoundTrip(func(r *http.Request) (*http.Response, error) {
				switch r.URL.Host + r.URL.Path {
				case "ipinfo.io/json":
					return providerResponse(200, M{"country": "PH", "ip": "203.0.113.2"}), nil
				case "chatgpt.com/api/auth/session":
					t.Fatal("checkout must not refresh Session before payment")
					return nil, nil
				case "chatgpt.com/backend-api/accounts/check/v4-2023-04-27":
					if r.Header.Get("Authorization") != "Bearer "+originalAT {
						t.Fatal("checkout must use the supplied AT")
					}
					if mode == "unauthorized" {
						return providerResponse(401, M{"error": M{"code": "token_expired"}}), nil
					}
					id := "test-space"
					if mode == "wrong_account" {
						id = "other"
					}
					return providerResponse(200, M{"accounts": M{id: M{"account": M{"id": id, "account_type": "personal"}, "entitlement": M{"has_active_subscription": mode == "active"}}}}), nil
				case "chatgpt.com/backend-api/payments/checkout":
					writes++
					if r.Header.Get("Authorization") != "Bearer "+originalAT {
						t.Fatal("checkout replaced the supplied AT")
					}
					if r.Header.Get("User-Agent") != iosCheckoutUA || r.Header.Get("OpenAI-Client-Type") != "native" {
						t.Fatal("checkout method replaced")
					}
					return providerResponse(200, M{"checkout_session_id": "cs_live_QA123", "url": "https://pay.openai.com/c/pay/cs_live_QA123", "tag": "hosted_checkout_session", "checkout_ui_mode": "hosted"}), nil
				default:
					t.Fatal("unexpected request")
					return nil, nil
				}
			})}
			in.BeforeCreate = func(v *checkoutInput) error {
				s := &providerSession{ctx: context.Background(), client: c, in: v, purpose: "checkout", persist: func(checkoutInput) error { return nil }}
				if e := s.prepare(); e != nil {
					return e
				}
				return checkAccountForCheckout(context.Background(), s)
			}
			_, e := generateCheckout(context.Background(), c, in, func() error { return nil })
			if mode == "free" {
				if e != nil || writes != 1 {
					t.Fatal(e, writes)
				}
			} else if e == nil || writes != 0 {
				t.Fatal("unsafe checkout", e, writes)
			}
		})
	}
}

func paidRefreshOrder(t *testing.T, keep bool) (*harness, string) {
	h, id, p := subscriptionFixture(t, keep)
	completeSubscriptionCheckout(t, h, id)
	p.events <- paymentEvent{Type: "quote", Amount: 1500, Currency: "php"}
	waitPayment(t, h, id, "submitting")
	p.events <- paymentEvent{Type: "result", State: "succeeded", Sent: true, Amount: 1500, Currency: "php"}
	waitPayment(t, h, id, "succeeded")
	return h, id
}

func TestPaidActivationQueriesBeforeRefreshAndKeepsRenewal(t *testing.T) {
	ctx := context.Background()
	h, id := paidRefreshOrder(t, true)
	var email, enc string
	h.a.DB.QueryRow(ctx, "SELECT o.email,s.credentials_enc FROM orders o JOIN subscription_runs s ON s.order_id=o.id WHERE o.id=$1", id).Scan(&email, &enc)
	raw, e := h.a.decrypt(enc)
	if e != nil {
		t.Fatal(e)
	}
	var in checkoutInput
	json.Unmarshal([]byte(raw), &in)
	in.SessionToken = "TEST_PRIVATE_SESSION"
	in.Email = email
	rawBytes, _ := json.Marshal(in)
	enc, _ = h.a.encrypt(string(rawBytes))
	h.a.DB.Exec(ctx, "UPDATE subscription_runs SET credentials_enc=$2 WHERE order_id=$1", id, enc)
	refreshes, reads, writes := 0, 0, 0
	h.a.renewalClient = func() *http.Client {
		return &http.Client{Transport: testRoundTrip(func(r *http.Request) (*http.Response, error) {
			if r.Method != "GET" {
				writes++
				t.Fatal("keep sent a write")
			}
			if r.URL.Path == "/api/auth/session" {
				refreshes++
				return providerResponse(200, testSession(email)), nil
			}
			reads++
			return providerResponse(200, M{"is_active": reads > 1, "will_renew": true, "plan_type": "premium"}), nil
		})}
	}
	for round := 0; round < 2; round++ {
		h.a.DB.Exec(ctx, "UPDATE subscription_runs SET next_at=now() WHERE order_id=$1", id)
		next, lease, e := h.a.claimRenewal(ctx)
		if e != nil || next != id {
			t.Fatal(e)
		}
		h.a.runRenewal(ctx, id, lease)
		var status, activation string
		h.a.DB.QueryRow(ctx, "SELECT status,activation FROM orders WHERE id=$1", id).Scan(&status, &activation)
		if round == 0 {
			if status != "processing" || activation != "pending" {
				t.Fatal("activation falsely confirmed", status, activation)
			}
		} else if status != "succeeded" || activation != "active" {
			t.Fatal("activation failed", status, activation)
		}
	}
	if refreshes != 0 || reads != 2 || writes != 0 {
		t.Fatal("wrong calls", refreshes, reads, writes)
	}
	paymentBalances(t, h, 90000, 0)
	assertSubscriptionPrivate(t, h, id)
}

func TestActivationFailureDoesNotReplayOrChangeBilling(t *testing.T) {
	ctx := context.Background()
	h, id := paidRefreshOrder(t, false)
	var before string
	h.a.DB.QueryRow(ctx, "SELECT details_enc FROM order_billing WHERE order_id=$1", id).Scan(&before)
	h.a.renewalClient = func() *http.Client {
		return &http.Client{Transport: testRoundTrip(func(r *http.Request) (*http.Response, error) {
			if r.Method != "GET" {
				t.Fatal("wrong plan cancelled")
			}
			return providerResponse(200, M{"is_active": true, "will_renew": true, "plan_type": "standard"}), nil
		})}
	}
	next, lease, _ := h.a.claimRenewal(ctx)
	if next != id {
		t.Fatal("missing job")
	}
	h.a.runRenewal(ctx, id, lease)
	var activation, paid, after string
	h.a.DB.QueryRow(ctx, "SELECT o.activation,p.state,b.details_enc FROM orders o JOIN payment_runs p ON p.order_id=o.id JOIN order_billing b ON b.order_id=o.id WHERE o.id=$1", id).Scan(&activation, &paid, &after)
	if activation != "review" || paid != "succeeded" || before != after {
		t.Fatal("paid or billing changed", activation, paid)
	}
	if id, _, _ := h.a.claimRenewal(ctx); id != "" {
		t.Fatal("terminal task reclaimed")
	}
	paymentBalances(t, h, 90000, 0)
}

func TestAsyncProviderDiagnosticsWhitelistAndAccess(t *testing.T) {
	h, id := paidRefreshOrder(t, true)
	ctx := h.a.providerContext(context.Background(), id, "activation")
	c := &http.Client{Transport: testRoundTrip(func(*http.Request) (*http.Response, error) {
		res := providerResponse(401, M{"error": M{"code": "account_deactivated", "message": "SECRET_SESSION backend-api/private", "card": "4242424242424242"}})
		res.Header.Set("X-Request-ID", "req_QATEST12345")
		return res, nil
	})}
	providerJSON(ctx, c, "GET", subscriptionEndpoint, http.Header{}, nil, false)
	select {
	case d := <-h.a.providerDiagnostics:
		h.a.saveProviderDiagnostic(d)
	default:
		t.Fatal("missing diagnostic")
	}
	admin := h.request("GET", "/api/admin/orders/"+id, nil, h.admin, nil)
	expect(t, admin, 200)
	if !strings.Contains(admin.Body.String(), "account_deactivated") || !strings.Contains(admin.Body.String(), "req_QATEST12345") {
		t.Fatal("missing error evidence")
	}
	for _, secret := range []string{"SECRET_SESSION", "backend-api/private", "4242424242424242"} {
		if strings.Contains(admin.Body.String(), secret) {
			t.Fatal("sensitive diagnostic leak")
		}
	}
	customer := h.request("GET", "/api/orders/"+id, nil, h.member, nil)
	if strings.Contains(customer.Body.String(), "req_QATEST12345") {
		t.Fatal("customer saw diagnostics")
	}
	expect(t, h.request("GET", "/api/admin/orders/"+id, nil, h.member, nil), 403)
	for i := 0; i < cap(h.a.providerDiagnostics)+5; i++ {
		reportProvider(ctx, 502, "SECRET", checkoutFailure("network", true, false), time.Millisecond)
	}
}
