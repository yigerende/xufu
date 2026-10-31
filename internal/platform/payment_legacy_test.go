package platform

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

type legacyRoundTrip func(*http.Request) (*http.Response, error)

func (f legacyRoundTrip) RoundTrip(r *http.Request) (*http.Response, error) { return f(r) }
func legacyReply(status int, body M) *http.Response {
	b, _ := json.Marshal(body)
	return &http.Response{StatusCode: status, Header: http.Header{}, Body: io.NopCloser(strings.NewReader(string(b)))}
}
func legacyTestPage(id string, amount int64, paid bool) M {
	m := M{"object": "checkout.session", "id": "ppage_Test", "session_id": id, "currency": "php", "total_summary": M{"due": amount}, "tax_meta": M{"status": "complete"}, "status": "open", "payment_status": "unpaid", "state": "active"}
	if paid {
		m["payment_status"] = "paid"
		m["status"] = "complete"
		m["state"] = "succeeded"
		m["payment_intent"] = M{"id": "pi_Test", "status": "succeeded", "currency": "php", "amount": amount, "amount_received": amount}
	}
	return m
}
func legacyTestInput() paymentInput {
	key := []byte(`{"apiKey":"pk_test_Offline"}`)
	for i := range key {
		key[i] ^= 5
	}
	return paymentInput{CheckExitIP: func(context.Context) error { return nil }, Mode: "prepare", URL: "https://checkout.stripe.com/c/pay/cs_test_Offline#" + base64.StdEncoding.EncodeToString(key), PrepareSeconds: 3, QuoteSeconds: 3, ResultSeconds: 3, ProxyExpiresAt: time.Now().Add(time.Minute).UnixMilli(), Email: "test@example.com", Card: cardInput{Number: "4242424242424242", Month: "12", Year: "2040", CVC: "123"}, Billing: M{"first_name": "Test", "last_name": "User", "line1": "123 Test Street", "city": "Manila", "state": "CA", "postal_code": "90001", "country": "US", "source": "saved"}, Currency: "php"}
}
func legacyCollect(t *testing.T, p *legacyPayment, authorize func(paymentEvent)) paymentEvent {
	t.Helper()
	defer p.Close()
	timer := time.NewTimer(5 * time.Second)
	defer timer.Stop()
	for {
		select {
		case e, ok := <-p.Events():
			if !ok {
				t.Fatal("runtime ended without result")
			}
			if e.Type == "quote" {
				if authorize != nil {
					authorize(e)
				}
				if err := p.Send(paymentCommand{Type: "confirm"}); err != nil {
					t.Fatal(err)
				}
			}
			if e.Type == "result" {
				return e
			}
		case <-timer.C:
			t.Fatal("runtime did not finish")
		}
	}
}

func TestLegacyProtocolHosted(t *testing.T) {
	for _, scenario := range []string{"success", "page-paid-without-received", "method-rejected", "amount-changed", "foreign-checkout", "foreign-currency", "challenge", "approval", "decline", "timeout", "tax-incomplete", "expired-proxy", "guard-revoked", "save-failed"} {
		t.Run(scenario, func(t *testing.T) {
			in := legacyTestInput()
			var calls, confirms, methods atomic.Int32
			var authorized atomic.Bool
			var changed bool
			var saved legacyPaymentCheckpoint
			in.Guard = func(context.Context) error {
				if scenario == "guard-revoked" && authorized.Load() {
					return errors.New("proxy_session_unavailable")
				}
				return nil
			}
			if scenario == "expired-proxy" {
				in.ProxyExpiresAt = time.Now().Add(-time.Second).UnixMilli()
			}
			in.SaveCheckpoint = func(_ context.Context, cp legacyPaymentCheckpoint) error {
				saved = cp
				if scenario == "save-failed" {
					return errors.New("storage unavailable")
				}
				return nil
			}
			client := &http.Client{Transport: legacyRoundTrip(func(r *http.Request) (*http.Response, error) {
				calls.Add(1)
				if r.URL.Host != "api.stripe.com" || r.Header.Get("Cookie") != "" || r.Header.Get("Authorization") != "" {
					t.Error("credentials or destination escaped")
				}
				if err := r.ParseForm(); err != nil {
					t.Fatal(err)
				}
				for k := range r.Form {
					if strings.Contains(k, "captcha") || strings.Contains(k, "radar") || k == "guid" || k == "muid" || k == "sid" {
						t.Errorf("fabricated verification field: %s", k)
					}
				}
				switch r.Method + " " + r.URL.Path {
				case "POST /v1/payment_pages/cs_test_Offline/init":
					return legacyReply(200, M{"eid": "NA"}), nil
				case "POST /v1/payment_pages/cs_test_Offline":
					if r.Form.Get("tax_region[postal_code]") != "90001" {
						t.Error("saved billing not used")
					}
					changed = true
					return legacyReply(200, M{}), nil
				case "GET /v1/payment_pages/cs_test_Offline/poll":
					return legacyReply(200, M{"id": "ppage_Test", "session_id": "cs_test_Offline", "state": "active", "payment_object_status": "processing"}), nil
				case "GET /v1/payment_pages/cs_test_Offline":
					n := int64(1000)
					if changed {
						n = 1080
					}
					if authorized.Load() && scenario == "amount-changed" {
						n = 1100
					}
					page := legacyTestPage("cs_test_Offline", n, false)
					if scenario == "tax-incomplete" {
						page["tax_meta"] = M{"status": "pending"}
					}
					return legacyReply(200, page), nil
				case "POST /v1/payment_methods":
					methods.Add(1)
					// Contract from the user's successful hosted capture, not the
					// newer custom-checkout client. Hosted uses the form version only.
					if r.Form.Get("_stripe_version") != "2020-08-27;custom_checkout_beta=v1" || r.Header.Get("Stripe-Version") != "" {
						t.Error("hosted payment method uses a different API contract than the capture")
						return legacyReply(400, M{"error": M{"type": "invalid_request_error"}}), nil
					}
					if !authorized.Load() {
						t.Error("card tokenized before authorization")
					}
					if r.Form.Get("card[number]") != in.Card.Number || r.Form.Get("billing_details[address][postal_code]") != "90001" {
						t.Error("card/billing missing")
					}
					if scenario == "method-rejected" {
						return legacyReply(400, M{"error": M{"type": "invalid_request_error"}}), nil
					}
					return legacyReply(200, M{"id": "pm_Test"}), nil
				case "POST /v1/payment_pages/cs_test_Offline/confirm":
					confirms.Add(1)
					if r.Form.Get("_stripe_version") != "2020-08-27;custom_checkout_beta=v1" || r.Header.Get("Stripe-Version") != "" {
						t.Error("hosted confirmation changed API contract after creating the payment method")
					}
					if !authorized.Load() || r.Form.Get("expected_amount") != "1080" || r.Form.Get("payment_method") != "pm_Test" {
						t.Error("wrong confirmation")
					}
					if scenario == "timeout" {
						return nil, errors.New("simulated timeout")
					}
					if scenario == "challenge" {
						return legacyReply(403, M{}), nil
					}
					if scenario == "decline" {
						return legacyReply(402, M{"error": M{"type": "card_error", "code": "card_declined"}}), nil
					}
					page := legacyTestPage("cs_test_Offline", 1080, true)
					if scenario == "page-paid-without-received" {
						delete(legacyMap(page["payment_intent"]), "amount_received")
					}
					if scenario == "foreign-checkout" {
						page["session_id"] = "cs_test_Other"
					}
					if scenario == "foreign-currency" {
						legacyMap(page["payment_intent"])["currency"] = "usd"
					}
					if scenario == "approval" {
						page = legacyTestPage("cs_test_Offline", 1080, false)
						page["submission_attempt"] = M{"state": "requires_approval"}
					}
					return legacyReply(200, page), nil
				default:
					t.Errorf("unexpected protocol request %s %s", r.Method, r.URL.Path)
					return legacyReply(500, M{}), nil
				}
			})}
			p := startLegacyPaymentClient(context.Background(), in, client, func() {})
			e := legacyCollect(t, p, func(q paymentEvent) {
				if q.Amount != 1080 || q.Currency != "php" {
					t.Error("quote did not include tax")
				}
				authorized.Store(true)
			})
			want := "review"
			switch scenario {
			case "success", "page-paid-without-received":
				want = "succeeded"
			case "challenge", "approval":
				want = "action_required"
			case "decline":
				want = "declined"
			case "method-rejected", "amount-changed", "tax-incomplete", "expired-proxy", "guard-revoked", "save-failed":
				want = "failed"
			}
			if e.State != want {
				t.Fatalf("state=%s code=%s want=%s", e.State, e.Code, want)
			}
			if confirms.Load() > 1 {
				t.Fatal("duplicate confirmation")
			}
			if want == "failed" && scenario != "method-rejected" && (confirms.Load() != 0 || methods.Load() != 0) {
				t.Fatal("unsafe preparation submitted")
			}
			if scenario == "method-rejected" && (methods.Load() != 1 || confirms.Load() != 0 || e.Sent || e.Code != "request_rejected") {
				t.Fatal("payment method rejection retried or submitted a payment")
			}
			if scenario == "expired-proxy" && calls.Load() != 0 {
				t.Fatal("expired proxy contacted provider")
			}
			if e.State == "succeeded" && (e.Amount != 1080 || e.Evidence == nil || e.Evidence.CheckoutID != "cs_test_Offline" || saved.Key != "pk_test_Offline") {
				t.Fatal("unbound success")
			}
			if scenario == "page-paid-without-received" && e.Evidence.AmountReceived != nil {
				t.Fatal("quote misreported as captured amount")
			}
		})
	}
}

func TestLegacyProtocolRecoveryNeverWrites(t *testing.T) {
	for _, custom := range []bool{false, true} {
		t.Run(map[bool]string{false: "hosted", true: "custom"}[custom], func(t *testing.T) {
			in := legacyTestInput()
			in.Mode = "check"
			in.ResumeApproval = true
			in.SavedQuote = M{"amount": int64(1080), "currency": "php"}
			in.Card = cardInput{}
			if custom {
				in.URL = "https://chatgpt.com/checkout/openai_llc/oaics_Offline"
				in.Checkpoint = legacyPaymentCheckpoint{CheckoutID: "oaics_Offline", Key: "pk_test_Offline", IntentID: "pi_Test", Secret: "pi_Test_secret_Offline"}
			}
			calls := 0
			client := &http.Client{Transport: legacyRoundTrip(func(r *http.Request) (*http.Response, error) {
				calls++
				if r.Method != "GET" {
					t.Fatal("recovery performed a write")
				}
				if custom {
					return legacyReply(200, M{"object": "payment_intent", "id": "pi_Test", "status": "succeeded", "currency": "php", "amount": 1080, "amount_received": 1080}), nil
				}
				return legacyReply(200, legacyTestPage("cs_test_Offline", 1080, true)), nil
			})}
			e := legacyCollect(t, startLegacyPaymentClient(context.Background(), in, client, func() {}), nil)
			if e.State != "succeeded" || calls != 1 {
				t.Fatal("original payment not verified", e.State, calls)
			}
		})
	}
}

func TestLegacyProtocolCustom(t *testing.T) {
	for _, challenge := range []bool{false, true} {
		t.Run(map[bool]string{false: "success", true: "verification"}[challenge], func(t *testing.T) {
			in := legacyTestInput()
			in.URL = "https://chatgpt.com/checkout/openai_llc/oaics_Offline"
			in.Authorization = checkoutInput{AccessToken: "OFFLINE_AT", AccountID: "test"}
			in.SessionCookies = "__Secure-next-auth.session-token=OFFLINE_COOKIE"
			var saved legacyPaymentCheckpoint
			authorized := false
			merchantConfirms, intentConfirms := 0, 0
			in.SaveCheckpoint = func(_ context.Context, cp legacyPaymentCheckpoint) error { saved = cp; return nil }
			client := &http.Client{Transport: legacyRoundTrip(func(r *http.Request) (*http.Response, error) {
				if r.URL.Host == "api.stripe.com" && (r.Header.Get("Cookie") != "" || r.Header.Get("Authorization") != "") {
					t.Fatal("merchant credentials leaked to Stripe")
				}
				if r.URL.Host == "chatgpt.com" && r.Header.Get("Authorization") != "Bearer OFFLINE_AT" {
					t.Fatal("missing original authorization")
				}
				switch r.Method + " " + r.URL.Path {
				case "GET /backend-api/payments/stripe_client_bootstrap":
					return legacyReply(200, M{"publishable_key": "pk_test_Offline"}), nil
				case "POST /backend-api/payments/checkout/taxes":
					return legacyReply(200, M{"checkout_session": M{"id": "oaics_Offline", "amount_total": 1080, "currency": "php"}}), nil
				case "POST /v1/confirmation_tokens":
					r.ParseForm()
					if r.Form.Get("payment_method_data[card][number]") != in.Card.Number || r.Form.Get("payment_method_data[billing_details][address][postal_code]") != "90001" {
						t.Fatal("incorrect custom card form")
					}
					return legacyReply(200, M{"id": "ctoken_Offline"}), nil
				case "POST /backend-api/payments/checkout/confirm":
					merchantConfirms++
					if !authorized {
						t.Fatal("no authorization")
					}
					if challenge {
						return legacyReply(403, M{}), nil
					}
					return legacyReply(200, M{"client_secret": "pi_Test_secret_Offline"}), nil
				case "GET /v1/payment_intents/pi_Test":
					return legacyReply(200, M{"object": "payment_intent", "id": "pi_Test", "status": "requires_confirmation", "confirmation_method": "automatic", "amount": 1080, "currency": "php"}), nil
				case "POST /v1/payment_intents/pi_Test/confirm":
					intentConfirms++
					if saved.Secret != "pi_Test_secret_Offline" {
						t.Fatal("intent not durably saved before confirmation")
					}
					return legacyReply(200, M{"object": "payment_intent", "id": "pi_Test", "status": "succeeded", "amount": 1080, "currency": "php", "amount_received": 1080}), nil
				default:
					t.Fatalf("unexpected request %s %s", r.Method, r.URL.Path)
					return nil, nil
				}
			})}
			e := legacyCollect(t, startLegacyPaymentClient(context.Background(), in, client, func() {}), func(paymentEvent) { authorized = true })
			if challenge {
				if e.State != "action_required" || intentConfirms != 0 {
					t.Fatal("challenge was advanced")
				}
			} else if e.State != "succeeded" || intentConfirms != 1 {
				t.Fatal("custom flow incomplete", e.State, e.Code)
			}
			if merchantConfirms != 1 {
				t.Fatal("merchant confirmation repeated")
			}
		})
	}
}

func TestLegacyProtocolClientRequiresPrivateRelay(t *testing.T) {
	for _, proxy := range []*browserProxyInput{nil, {Server: "http://evil.test:8080", Username: "test", Password: "test"}, {Server: "http://127.0.0.1:1234", Username: "", Password: "test"}} {
		in := legacyTestInput()
		in.Proxy = proxy
		if _, _, e := legacyPaymentHTTP(in); e == nil {
			t.Fatal("invalid proxy accepted")
		}
	}
}

func TestLegacyProtocolShortPaymentPage(t *testing.T) {
	in := legacyTestInput()
	in.URL = "https://chatgpt.com/checkout/openai_llc/cs_live_Offline"
	in.Authorization = checkoutInput{AccessToken: "OFFLINE_AT", AccountID: "offline"}
	confirmed := 0
	client := &http.Client{Transport: legacyRoundTrip(func(r *http.Request) (*http.Response, error) {
		switch r.Method + " " + r.URL.Path {
		case "GET /backend-api/payments/stripe_client_bootstrap":
			return legacyReply(200, M{"publishable_key": "pk_test_Offline"}), nil
		case "POST /v1/payment_pages/cs_live_Offline/init":
			return legacyReply(200, M{"eid": "NA"}), nil
		case "POST /v1/payment_pages/cs_live_Offline":
			return legacyReply(200, M{}), nil
		case "GET /v1/payment_pages/cs_live_Offline":
			return legacyReply(200, legacyTestPage("cs_live_Offline", 1080, false)), nil
		case "POST /v1/payment_methods":
			return legacyReply(200, M{"id": "pm_Test"}), nil
		case "POST /v1/payment_pages/cs_live_Offline/confirm":
			confirmed++
			return legacyReply(200, legacyTestPage("cs_live_Offline", 1080, true)), nil
		default:
			t.Error("short checkout changed to another flow", r.Method, r.URL.Path)
			return legacyReply(500, M{}), nil
		}
	})}
	e := legacyCollect(t, startLegacyPaymentClient(context.Background(), in, client, func() {}), nil)
	if e.State != "succeeded" || confirmed != 1 || e.Evidence.CheckoutID != "cs_live_Offline" {
		t.Fatal("short payment page failed", e.State, e.Code)
	}
}

func TestLegacyProtocolDoesNotFollowRedirect(t *testing.T) {
	in := legacyTestInput()
	in.Proxy = &browserProxyInput{Server: "http://127.0.0.1:12345", Username: "test", Password: "test"}
	client, cleanup, err := legacyPaymentHTTP(in)
	if err != nil {
		t.Fatal(err)
	}
	defer cleanup()
	calls := 0
	client.Transport = legacyRoundTrip(func(r *http.Request) (*http.Response, error) {
		calls++
		res := legacyReply(302, M{})
		res.Header.Set("Location", "https://untrusted.invalid/capture")
		return res, nil
	})
	e := legacyCollect(t, startLegacyPaymentClient(context.Background(), in, client, func() {}), nil)
	if calls != 1 || e.State != "failed" {
		t.Fatal("upstream redirect followed")
	}
}

func TestLegacyProtocolDatabaseFenceAndPrivacy(t *testing.T) {
	h, id, _ := paymentFixture(t)
	mockOrderIP(t, h.a, "203.0.113.2")
	ctx := context.Background()
	input := legacyTestInput()
	link, err := h.a.encrypt(input.URL)
	if err != nil {
		t.Fatal(err)
	}
	if _, err = h.a.DB.Exec(ctx, "UPDATE checkout_runs SET hosted_url_enc=$2 WHERE order_id=$1", id, link); err != nil {
		t.Fatal(err)
	}
	var confirms atomic.Int32
	h.a.paymentStart = func(ctx context.Context, in paymentInput) (paymentProcess, error) {
		var attempt string
		if e := h.a.DB.QueryRow(ctx, "SELECT attempt_id FROM payment_runs WHERE order_id=$1", id).Scan(&attempt); e != nil {
			return nil, e
		}
		if e := h.a.legacyPaymentInput(ctx, id, attempt, &in); e != nil {
			return nil, e
		}
		client := &http.Client{Transport: legacyRoundTrip(func(r *http.Request) (*http.Response, error) {
			switch r.Method + " " + r.URL.Path {
			case "POST /v1/payment_pages/cs_test_Offline/init":
				return legacyReply(200, M{"eid": "NA"}), nil
			case "POST /v1/payment_pages/cs_test_Offline":
				return legacyReply(200, M{}), nil
			case "GET /v1/payment_pages/cs_test_Offline/poll":
				return legacyReply(200, M{"id": "ppage_Test", "session_id": "cs_test_Offline", "state": "active", "payment_object_status": "processing"}), nil
			case "GET /v1/payment_pages/cs_test_Offline":
				return legacyReply(200, legacyTestPage("cs_test_Offline", 1080, false)), nil
			case "POST /v1/payment_methods":
				return legacyReply(200, M{"id": "pm_Test"}), nil
			case "POST /v1/payment_pages/cs_test_Offline/confirm":
				var submitted bool
				var amount int64
				var state, sealed string
				e := h.a.DB.QueryRow(ctx, "SELECT submitted,amount,state,legacy_checkpoint_enc FROM payment_runs WHERE order_id=$1", id).Scan(&submitted, &amount, &state, &sealed)
				if e != nil || !submitted || amount != 1080 || state != "submitting" || sealed == "" || strings.Contains(sealed, "pk_test") {
					t.Error("submission was not durably fenced and encrypted", e)
				}
				confirms.Add(1)
				return legacyReply(200, legacyTestPage("cs_test_Offline", 1080, true)), nil
			default:
				t.Error("unexpected request", r.Method, r.URL.Path)
				return legacyReply(500, M{}), nil
			}
		})}
		return startLegacyPaymentClient(ctx, in, client, func() {}), nil
	}
	endpoint := "/api/orders/" + id + "/payment/"
	result := expect(t, h.request("POST", endpoint+"prepare", paymentPrepareBody(), h.member, nil), 202)
	waitPayment(t, h, id, "quoted")
	confirm := M{"attempt_id": result["attempt_id"], "amount": 1080, "currency": "php", "accepted": true}
	expect(t, h.request("POST", endpoint+"confirm", confirm, h.member, nil), 202)
	waitPayment(t, h, id, "succeeded")
	paymentBalances(t, h, 9900, 0)
	if confirms.Load() != 1 {
		t.Fatal("confirmation repeated")
	}
	var sealed string
	if err = h.a.DB.QueryRow(ctx, "SELECT legacy_checkpoint_enc FROM payment_runs WHERE order_id=$1", id).Scan(&sealed); err != nil || sealed != "" {
		t.Fatal("terminal checkpoint retained", err)
	}
	for _, route := range []string{"/api/orders/" + id, "/api/admin/orders/" + id} {
		cookie := h.member
		if strings.Contains(route, "/admin/") {
			cookie = h.admin
		}
		res := h.request("GET", route, nil, cookie, nil)
		for _, secret := range []string{"legacy_checkpoint_enc", "pk_test_Offline", "4242424242424242", "pi_Test"} {
			if strings.Contains(res.Body.String(), secret) {
				t.Error("private data projected", secret)
			}
		}
	}
}

func TestLegacyProtocolGuardTracksResourceLeaseAndOrder(t *testing.T) {
	h, id, _ := paymentFixture(t)
	ctx := context.Background()
	result := expect(t, h.request("POST", "/api/orders/"+id+"/payment/prepare", paymentPrepareBody(), h.member, nil), 202)
	attempt := str(result, "attempt_id")
	var lease string
	for deadline := time.Now().Add(3 * time.Second); time.Now().Before(deadline); {
		_ = h.a.DB.QueryRow(ctx, "SELECT COALESCE(proxy_lease_id,'') FROM checkout_runs WHERE order_id=$1", id).Scan(&lease)
		if lease != "" {
			break
		}
		time.Sleep(10 * time.Millisecond)
	}
	guard := h.a.legacyPaymentGuard(id, attempt, lease)
	if err := guard(ctx); err != nil {
		t.Fatal("valid original session rejected", err)
	}
	// Exit observations are not a gate, but disabling the original resource is.
	_, _ = h.a.DB.Exec(ctx, "UPDATE order_proxy_sessions SET expected_ip='' WHERE order_id=$1", id)
	if err := guard(ctx); err != nil {
		t.Fatal("IP gate remains", err)
	}
	_, _ = h.a.DB.Exec(ctx, "UPDATE resources SET enabled=false WHERE id='payment_proxy'")
	if guard(ctx) == nil {
		t.Fatal("disabled resource accepted")
	}
	_, _ = h.a.DB.Exec(ctx, "UPDATE resources SET enabled=true WHERE id='payment_proxy'")
	_, _ = h.a.DB.Exec(ctx, "DELETE FROM proxy_leases WHERE id=$1", lease)
	if guard(ctx) == nil {
		t.Fatal("expired capacity lease accepted")
	}
}
