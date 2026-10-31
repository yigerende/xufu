package platform

import (
	"context"
	"encoding/json"
	"net/http"
	"strings"
	"testing"
	"time"
	"xupay/internal/cslive"
)

func TestCSLiveBrowserCompleteFlow(t *testing.T) {
	for _, kind := range []string{"hosted", "oaics"} {
		for _, keep := range []bool{true, false} {
			t.Run(kind+map[bool]string{true: "/keep", false: "/cancel"}[keep], func(t *testing.T) {
				h := setupTest(t)
				stopFlowTestWorkers(t, h)
				ctx := context.Background()
				t.Setenv("XUPAY_LOCAL_PROXY", "")
				h.saveTestBilling("subscription@example.com")
				h.addProxy("PH", 2)
				mockOrderIP(t, h.a, "203.0.113.2")
				_, _ = h.a.DB.Exec(ctx, "UPDATE users SET balance=100000 WHERE id=$1", h.memberID)
				setSubscriptionFlow(t, h, subscriptionFlowCSLiveBrowser)
				setCheckoutType(t, h, kind)
				link := "https://pay.openai.com/c/pay/cs_live_BrowserFlow#original"
				if kind == "oaics" {
					link = "https://chatgpt.com/checkout/openai_llc/oaics_BrowserFlow"
				}
				fresh := testSession("subscription@example.com")
				freshAT := str(fresh, "accessToken") + "REFRESHED"
				const freshCookies = "__Secure-next-auth.session-token=REFRESHED_BROWSER_SESSION"
				creates := 0
				h.a.checkoutClient = func(cfg cslive.Config, hooks cslive.Hooks) protocolCheckoutClient {
					if cfg.Proxy == "" || hooks.Checkpoint != nil {
						t.Error("checkout lost proxy or gained a payment checkpoint")
					}
					return checkoutClientFunc(func(ctx context.Context, session cslive.Session, options cslive.CreateCheckoutOptions) (cslive.CheckoutResult, error) {
						creates++
						if options.CheckoutType != kind || options.WithPrice {
							t.Error("checkout selection changed")
						}
						if err := hooks.BeforeRequest(ctx, "POST", checkoutEndpoint); err != nil {
							return cslive.CheckoutResult{}, err
						}
						if hooks.BeforeRequest(ctx, "POST", "https://api.stripe.com/v1/payment_methods") == nil {
							t.Error("checkout could submit card")
						}
						id := "cs_live_BrowserFlow"
						if kind == "oaics" {
							id = "oaics_BrowserFlow"
						}
						session.AccessToken, session.Cookies = freshAT, freshCookies
						return cslive.CheckoutResult{CheckoutID: id, CheckoutURL: link, ProcessorEntity: "openai_llc", Session: &session}, nil
					})
				}
				proc := &fakePayment{events: make(chan paymentEvent, 8)}
				opened := make(chan struct{}, 1)
				h.a.paymentStart = func(ctx context.Context, in paymentInput) (paymentProcess, error) {
					if in.Executor != paymentExecutorBrowser || in.URL != link || !in.AutoConfirm || in.ExpectedIP != "203.0.113.2" || in.SkipExitIPCheck || in.Proxy == nil {
						t.Error("browser did not receive the fixed original checkout/proxy")
					}
					if in.Authorization.AccessToken != freshAT || in.SessionCookies != freshCookies || !validBilling(in.Billing) || in.Billing["source"] != "saved" {
						t.Error("browser authorization/billing not preserved")
					}
					if err := in.CheckExitIP(ctx); err != nil {
						t.Error("fixed exit failed", err)
					}
					opened <- struct{}{}
					return proc, nil
				}
				body := prepareRequest()
				body["workflow"], body["card"], body["keep_renewal"] = "subscription", paymentPrepareBody()["card"], keep
				headers := map[string]string{"Idempotency-Key": uid()}
				id := str(expect(t, h.request("POST", "/api/orders", body, h.member, headers), 200), "id")
				var billing, proxy string
				h.a.DB.QueryRow(ctx, "SELECT details_enc FROM order_billing WHERE order_id=$1", id).Scan(&billing)
				setSubscriptionFlow(t, h, subscriptionFlowCSLive)
				setCheckoutType(t, h, map[string]string{"hosted": "oaics", "oaics": "hosted"}[kind])
				job, err := h.a.claim(ctx, "checkout")
				if err != nil || job == nil || job.OrderID != id {
					t.Fatal("new checkout not claimed", err)
				}
				h.a.DB.QueryRow(ctx, "SELECT endpoint_enc FROM order_proxy_sessions WHERE order_id=$1", id).Scan(&proxy)
				h.a.executeCheckout(ctx, job)
				select {
				case <-opened:
				case <-time.After(4 * time.Second):
					t.Fatal("browser never started")
				}
				if h.a.isProtocolSubscription(ctx, id) || creates != 1 {
					t.Fatal("used full protocol or duplicated checkout")
				}
				proc.events <- paymentEvent{Type: "quote", Amount: 1500, Currency: "php"}
				waitPayment(t, h, id, "submitting")
				until := time.Now().Add(time.Second)
				for proc.commits.Load() != 1 && time.Now().Before(until) {
					time.Sleep(10 * time.Millisecond)
				}
				if proc.commits.Load() != 1 {
					t.Fatal("confirmation not exactly once")
				}
				proc.events <- paymentEvent{Type: "result", State: "succeeded", Sent: true, Amount: 1500, Currency: "php"}
				waitPayment(t, h, id, "succeeded")
				refreshes, cancels := 0, 0
				currentAT := freshAT
				h.a.renewalClient = func() *http.Client {
					return &http.Client{Transport: testRoundTrip(func(r *http.Request) (*http.Response, error) {
						if r.URL.Path == "/api/auth/session" {
							refreshes++
							if !strings.Contains(r.Header.Get("Cookie"), freshCookies) {
								t.Error("renewal lost refreshed cookie")
							}
							value := testSession("subscription@example.com")
							currentAT = str(value, "accessToken") + "CANCEL"
							value["accessToken"] = currentAT
							return providerResponse(200, value), nil
						}
						if r.Header.Get("Authorization") != "Bearer "+currentAT {
							t.Error("post-payment used stale AT")
						}
						if r.Method == "POST" {
							cancels++
							return providerResponse(200, M{"success": true}), nil
						}
						return providerResponse(200, M{"id": "fixture-subscription", "is_active": true, "will_renew": cancels == 0, "plan_type": "premium"}), nil
					})}
				}
				next, lease, err := h.a.claimRenewal(ctx)
				if err != nil || next != id {
					t.Fatal("activation task missing", err)
				}
				h.a.runRenewal(ctx, id, lease)
				var status, activation, renewal, billingAfter, proxyAfter string
				err = h.a.DB.QueryRow(ctx, `SELECT o.status,o.activation,o.renewal,b.details_enc,p.endpoint_enc FROM orders o JOIN order_billing b ON b.order_id=o.id JOIN order_proxy_sessions p ON p.order_id=o.id WHERE o.id=$1`, id).Scan(&status, &activation, &renewal, &billingAfter, &proxyAfter)
				wantRenewal := "disabled"
				wantWrites := 1
				if keep {
					wantRenewal = "kept"
					wantWrites = 0
				}
				if err != nil || status != "succeeded" || activation != "active" || renewal != wantRenewal || cancels != wantWrites || refreshes != wantWrites || billingAfter != billing || proxyAfter != proxy {
					t.Fatal("full flow did not preserve post-payment rules", err, status, activation, renewal, cancels, refreshes)
				}
				replay := expect(t, h.request("POST", "/api/orders", body, h.member, headers), 200)
				if replay["id"] != id || replay["replayed"] != true || creates != 1 || proc.commits.Load() != 1 {
					t.Fatal("replay repeated payment")
				}
				paymentBalances(t, h, 90000, 0)
				assertSubscriptionPrivate(t, h, id)
			})
		}
	}
}

func TestCSLiveBrowserAdmissionRequiresRuntimeAndSession(t *testing.T) {
	h := setupTest(t)
	ctx := context.Background()
	setSubscriptionFlow(t, h, subscriptionFlowCSLiveBrowser)
	t.Setenv("PAYMENT_RUNTIME_PATH", "missing-payment-runtime.mjs")
	if h.a.subscriptionEnabled(ctx) {
		t.Fatal("browser flow enabled without runtime")
	}
	body := prepareRequest()
	body["workflow"], body["card"] = "subscription", paymentPrepareBody()["card"]
	expect(t, h.request("POST", "/api/orders", body, h.member, map[string]string{"Idempotency-Key": uid()}), 409)
	delete(body["session"].(M), "sessionToken")
	expect(t, h.request("POST", "/api/session/inspect", M{"session": body["session"], "workflow": "subscription"}, h.member, nil), 400)
	var count int
	h.a.DB.QueryRow(ctx, "SELECT count(*) FROM payment_runs").Scan(&count)
	if count != 0 {
		t.Fatal("rejected admission created payment")
	}
}

func TestCSLiveBrowserCheckoutFailureNeverStartsPayment(t *testing.T) {
	for _, mode := range []string{"unknown", "wrong_account", "missing_authorization"} {
		t.Run(mode, func(t *testing.T) {
			h := setupTest(t)
			stopFlowTestWorkers(t, h)
			ctx := context.Background()
			t.Setenv("XUPAY_LOCAL_PROXY", "")
			h.saveTestBilling("subscription@example.com")
			h.addProxy("PH", 1)
			mockOrderIP(t, h.a, "203.0.113.2")
			_, _ = h.a.DB.Exec(ctx, "UPDATE users SET balance=100000 WHERE id=$1", h.memberID)
			setSubscriptionFlow(t, h, subscriptionFlowCSLiveBrowser)
			h.a.paymentStart = func(context.Context, paymentInput) (paymentProcess, error) {
				t.Error("failed checkout started payment")
				return nil, nil
			}
			creates := 0
			h.a.checkoutClient = func(_ cslive.Config, hooks cslive.Hooks) protocolCheckoutClient {
				return checkoutClientFunc(func(ctx context.Context, session cslive.Session, _ cslive.CreateCheckoutOptions) (cslive.CheckoutResult, error) {
					creates++
					if err := hooks.BeforeRequest(ctx, "POST", checkoutEndpoint); err != nil {
						return cslive.CheckoutResult{}, err
					}
					result := cslive.CheckoutResult{CheckoutID: "cs_live_Failed", CheckoutURL: "https://pay.openai.com/c/pay/cs_live_Failed#original", Session: &session}
					switch mode {
					case "unknown":
						result.ErrorCode = "network_timeout"
					case "wrong_account":
						session.AccountID = "other-account"
					case "missing_authorization":
						result.Session = nil
					}
					return result, nil
				})
			}
			body := prepareRequest()
			body["workflow"], body["card"] = "subscription", paymentPrepareBody()["card"]
			id := str(expect(t, h.request("POST", "/api/orders", body, h.member, map[string]string{"Idempotency-Key": uid()}), 200), "id")
			job, err := h.a.claim(ctx, "checkout")
			if err != nil || job == nil {
				t.Fatal(err)
			}
			h.a.executeCheckout(ctx, job)
			waitPayment(t, h, id, "failed")
			var state string
			var submitted bool
			if err = h.a.DB.QueryRow(ctx, "SELECT j.state,p.submitted FROM jobs j JOIN payment_runs p ON p.order_id=j.order_id WHERE j.order_id=$1", id).Scan(&state, &submitted); err != nil || state != "review" || submitted || creates != 1 {
				t.Fatal("unknown creation not retained without payment", err, state, submitted, creates)
			}
			if next, err := h.a.claim(ctx, "checkout"); err != nil || next != nil {
				t.Fatal("unknown checkout was retried", err)
			}
			paymentBalances(t, h, 100000, 0)
		})
	}
}

func TestProtocolCheckoutCredentialsValidation(t *testing.T) {
	raw, _ := json.Marshal(testSession("subscription@example.com"))
	info, err := inspectSession(raw)
	if err != nil {
		t.Fatal(err)
	}
	original, _, err := sessionCredentials(raw, info)
	if err != nil {
		t.Fatal(err)
	}
	for _, kind := range []string{"valid", "missing", "account", "email", "token", "cookies"} {
		t.Run(kind, func(t *testing.T) {
			session := &cslive.Session{AccessToken: original.AccessToken + "FRESH", AccountID: original.AccountID, Email: original.Email, Cookies: "__Secure-next-auth.session-token=FRESH; unrelated=PRIVATE_OTHER"}
			switch kind {
			case "missing":
				session = nil
			case "account":
				session.AccountID = "different"
			case "email":
				session.Email = "different@example.com"
			case "token":
				session.AccessToken = "invalid"
			case "cookies":
				session.Cookies = "unrelated=PRIVATE_OTHER"
			}
			got, err := protocolCheckoutCredentials(original, session)
			if kind != "valid" {
				if err == nil {
					t.Fatal("invalid authorization accepted")
				}
				return
			}
			if err != nil || got.AccessToken != session.AccessToken || got.Cookies != "__Secure-next-auth.session-token=FRESH" || got.RefreshPurpose != "checkout" {
				t.Fatal("refreshed authorization not retained", err)
			}
		})
	}
}
