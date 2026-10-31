package platform

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"strings"
	"testing"
	"time"

	"xupay/internal/cslive"
)

func TestLinkFlowAdmissionReplayAndPrivacy(t *testing.T) {
	for _, flow := range []string{subscriptionFlowLegacy, subscriptionFlowCSLive} {
		for _, route := range []string{"/api/orders", "/api/v1/orders"} {
			t.Run(flow+route, func(t *testing.T) {
				h := setupTest(t)
				ctx := context.Background()
				setSubscriptionFlow(t, h, flow)
				cfg := h.a.config(ctx, "payment")
				cfg["enabled"] = false
				expect(t, h.request("PUT", "/api/admin/settings/payment", cfg, h.admin, nil), 200)
				body := prepareRequest()
				body["workflow"] = "checkout"
				if flow == subscriptionFlowLegacy {
					delete(body["session"].(M), "sessionToken")
				}
				headers := map[string]string{"Idempotency-Key": uid()}
				cookie := h.member
				if route == "/api/v1/orders" {
					key := expect(t, h.request("POST", "/api/keys", M{"name": "link flow"}, cookie, nil), 200)["key"].(string)
					headers["Authorization"] = "Bearer " + key
					cookie = nil
				}
				id := str(expect(t, h.request("POST", route, body, cookie, headers), 200), "id")
				other := subscriptionFlowLegacy
				if flow == other {
					other = subscriptionFlowCSLive
				}
				setSubscriptionFlow(t, h, other)
				replay := expect(t, h.request("POST", route, body, cookie, headers), 200)
				var saved string
				if err := h.a.DB.QueryRow(ctx, "SELECT flow FROM checkout_runs WHERE order_id=$1", id).Scan(&saved); err != nil || saved != flow || replay["id"] != id || replay["replayed"] != true {
					t.Fatal("flow changed or order duplicated", err)
				}
				detail := expect(t, h.request("GET", "/api/admin/orders/"+id, nil, h.admin, nil), 200)
				if detail["order"].(M)["subscription_flow"] != flow {
					t.Fatal("admin flow mismatch")
				}
				view := h.request("GET", route+"/"+id, nil, cookie, headers)
				expect(t, view, 200)
				if strings.Contains(view.Body.String(), "subscription_flow") || strings.Contains(view.Body.String(), "credentials_enc") {
					t.Fatal("private flow leaked")
				}
				var effects int
				err := h.a.DB.QueryRow(ctx, `SELECT (SELECT count(*) FROM payment_runs)+(SELECT count(*) FROM subscription_runs)+(SELECT count(*) FROM subscription_protocol)+(SELECT count(*) FROM order_billing)+(SELECT count(*) FROM ledger)+(SELECT count(*) FROM renewal_tasks)`).Scan(&effects)
				if err != nil || effects != 0 {
					t.Fatal("link created payment side effects", err, effects)
				}
				paymentBalances(t, h, 0, 0)
			})
		}
	}
}

func TestLinkFlowSessionAndLongOnlyMigration(t *testing.T) {
	h := setupTest(t)
	ctx := context.Background()
	body := prepareRequest()
	delete(body["session"].(M), "sessionToken")
	for _, flow := range []string{subscriptionFlowCSLive, subscriptionFlowLegacy} {
		setSubscriptionFlow(t, h, flow)
		want := 400
		if flow == subscriptionFlowLegacy {
			want = 200
		}
		expect(t, h.request("POST", "/api/session/inspect", M{"session": body["session"], "workflow": "checkout"}, h.member, nil), want)
		expect(t, h.request("POST", "/api/orders", body, h.member, map[string]string{"Idempotency-Key": uid()}), want)
	}
	// An expired access token is accepted only by the refresh-capable flow.
	body = prepareRequest()
	session := body["session"].(M)
	parts := strings.Split(session["accessToken"].(string), ".")
	claims, _ := json.Marshal(M{"exp": time.Now().Add(-time.Hour).Unix()})
	parts[1] = base64.RawURLEncoding.EncodeToString(claims)
	session["accessToken"] = strings.Join(parts, ".")
	for _, flow := range []string{subscriptionFlowLegacy, subscriptionFlowCSLive} {
		setSubscriptionFlow(t, h, flow)
		want := 400
		if flow == subscriptionFlowCSLive {
			want = 200
		}
		expect(t, h.request("POST", "/api/session/inspect", M{"session": session, "workflow": "checkout"}, h.member, nil), want)
		expect(t, h.request("POST", "/api/orders", body, h.member, map[string]string{"Idempotency-Key": uid()}), want)
	}
	cfg := h.a.config(ctx, "scheduler")
	cfg["link_mode"] = "short"
	expect(t, h.request("PUT", "/api/admin/settings/scheduler", cfg, h.admin, nil), 400)
	if _, err := h.a.DB.Exec(ctx, `UPDATE settings SET value=jsonb_set(value,'{link_mode}','"short"') WHERE key='scheduler'`); err != nil {
		t.Fatal(err)
	}
	if err := h.a.seed(ctx); err != nil {
		t.Fatal(err)
	}
	if str(h.a.config(ctx, "scheduler"), "link_mode") != "long" {
		t.Fatal("short setting not migrated")
	}
	// Simulate upgrading a pre-flow database; historical runs must stay legacy.
	if _, err := h.a.DB.Exec(ctx, "ALTER TABLE checkout_runs DROP COLUMN flow"); err != nil {
		t.Fatal(err)
	}
	if _, err := h.a.DB.Exec(ctx, schema); err != nil {
		t.Fatal(err)
	}
	var other int
	if err := h.a.DB.QueryRow(ctx, "SELECT count(*) FROM checkout_runs WHERE flow<>'legacy'").Scan(&other); err != nil || other != 0 {
		t.Fatal("historical flow changed", err)
	}
}

type checkoutClientFunc func(context.Context, cslive.Session, cslive.CreateCheckoutOptions) (cslive.CheckoutResult, error)

func (f checkoutClientFunc) CreateCheckout(ctx context.Context, s cslive.Session, o cslive.CreateCheckoutOptions) (cslive.CheckoutResult, error) {
	return f(ctx, s, o)
}

func TestProtocolLinkHostedOnly(t *testing.T) {
	t.Setenv("XUPAY_LOCAL_PROXY", "")
	for product, plan := range map[string]string{"pro": "pro20x", "pro5x": "pro5x", "team": "team", "team_premium": "team5x"} {
		t.Run(product, func(t *testing.T) {
			in := checkoutInput{AccessToken: "PRIVATE_AT", SessionToken: "PRIVATE_SESSION", Country: "PH", Product: product, Seats: 2, PremiumSeats: 3}
			for _, mode := range []string{"long", "short", "mismatched", "missing", "network_after_create", "upgrade"} {
				a := &App{checkoutClient: func(cfg cslive.Config, hooks cslive.Hooks) protocolCheckoutClient {
					if cfg.Proxy == "" || cfg.Currency != "PHP" || cfg.Timezone != "Asia/Manila" || hooks.Checkpoint != nil || hooks.BeforeRequest == nil {
						t.Fatal("incorrect standalone configuration")
					}
					return checkoutClientFunc(func(_ context.Context, _ cslive.Session, o cslive.CreateCheckoutOptions) (cslive.CheckoutResult, error) {
						if o.CheckoutType != "hosted" || o.WithPrice || o.Plan != plan || o.Country != "PH" {
							t.Fatal("wrong checkout mode or product")
						}
						if strings.HasPrefix(plan, "team") && (o.Seats != 2 || plan == "team5x" && o.Seats5x != 3) {
							t.Fatal("wrong seats")
						}
						r := cslive.CheckoutResult{CheckoutID: "cs_live_Test", CheckoutURL: "https://pay.openai.com/c/pay/cs_live_Test#returned"}
						switch mode {
						case "short":
							r.CheckoutURL = "https://chatgpt.com/checkout/openai_llc/cs_live_Test"
						case "mismatched":
							r.CheckoutURL = "https://pay.openai.com/c/pay/cs_live_Other"
						case "missing":
							r.CheckoutURL = ""
						case "network_after_create":
							r.ErrorCode = "network_timeout"
						case "upgrade":
							r = cslive.CheckoutResult{Flow: "subscription_update"}
						}
						return r, nil
					})
				}}
				out, err := a.generateProtocolCheckout(context.Background(), &Job{}, "http://user:pass@proxy.example.com:8080", in)
				if mode == "long" {
					if err != nil || !validHostedCheckoutURL(out.URL) {
						t.Fatal("valid long link rejected", err)
					}
				} else {
					var failure checkoutError
					if !errors.As(err, &failure) || out.URL != "" || failure.Retry || failure.Unknown != (mode != "upgrade") {
						t.Fatal("unsafe failure classification", mode, err)
					}
				}
			}
		})
	}
}

func TestProtocolLinkRequestGuard(t *testing.T) {
	for _, mode := range []string{"normal", "drift", "disabled", "expired", "lease", "capacity"} {
		t.Run(mode, func(t *testing.T) {
			h := setupTest(t)
			ctx := context.Background()
			h.addProxy("PH", 1)
			id := str(expect(t, h.request("POST", "/api/orders", prepareRequest(), h.member, map[string]string{"Idempotency-Key": uid()}), 200), "id")
			job, err := h.a.claim(ctx, "checkout")
			if err != nil || job == nil {
				t.Fatal("claim failed", err)
			}
			mockOrderIP(t, h.a, "203.0.113.2")
			run := &protocolCheckoutRun{a: h.a, job: job, country: "PH"}
			for _, target := range []string{"https://api.stripe.com/v1/payment_methods", "https://chatgpt.com/backend-api/subscriptions", "https://api.stripe.com/v1/payment_pages/cs_live_Test/confirm", "https://chatgpt.com/backend-api/payments/subscription/update", "http://chatgpt.com/backend-api/payments/checkout"} {
				if run.beforeRequest(ctx, "POST", target) == nil {
					t.Fatal("forbidden write permitted")
				}
			}
			sql := map[string]string{
				"drift":    "UPDATE order_proxy_sessions SET expected_ip='203.0.113.9' WHERE order_id=$1",
				"disabled": "UPDATE resources SET enabled=false WHERE id=(SELECT resource_id FROM order_proxy_sessions WHERE order_id=$1)",
				"expired":  "UPDATE order_proxy_sessions SET expires_at=now()-interval '1 second' WHERE order_id=$1",
				"lease":    "UPDATE jobs SET lease_token='other-worker' WHERE order_id=$1",
				"capacity": "DELETE FROM proxy_leases WHERE id=(SELECT proxy_lease_id FROM checkout_runs WHERE order_id=$1)",
			}[mode]
			if sql != "" {
				if _, err = h.a.DB.Exec(ctx, sql, id); err != nil {
					t.Fatal(err)
				}
			}
			err = run.beforeRequest(ctx, "POST", checkoutEndpoint)
			if mode == "normal" || mode == "drift" {
				if err != nil || !run.written {
					t.Fatal("checkout blocked", err)
				}
				if run.beforeRequest(ctx, "POST", checkoutEndpoint) == nil {
					t.Fatal("duplicate checkout allowed")
				}
				if err = run.beforeRequest(ctx, "POST", "https://api.stripe.com/v1/payment_pages/cs_live_Test/init"); err != nil {
					t.Fatal("long link init blocked", err)
				}
			} else if err == nil || run.written {
				t.Fatal("guard ignored", mode)
			}
			var written bool
			if err = h.a.DB.QueryRow(ctx, "SELECT side_effect_started FROM jobs WHERE order_id=$1", id).Scan(&written); err != nil || written != (mode == "normal" || mode == "drift") {
				t.Fatal("wrong durable fence", err)
			}
		})
	}
}

func TestLinkWorkerKeepsSelectedFlowAndNeverPays(t *testing.T) {
	for _, kind := range []string{"hosted", "oaics", "auto_hosted", "auto_custom"} {
		for _, unknown := range []bool{false, true} {
			t.Run(kind+map[bool]string{false: "ready", true: "unknown"}[unknown], func(t *testing.T) {
				h := setupTest(t)
				t.Setenv("XUPAY_LOCAL_PROXY", "")
				ctx := context.Background()
				h.addProxy("PH", 1)
				setCheckoutType(t, h, "oaics")
				id := str(expect(t, h.request("POST", "/api/orders", prepareRequest(), h.member, map[string]string{"Idempotency-Key": uid()}), 200), "id")
				savedKind := kind
				if strings.HasPrefix(kind, "auto_") {
					savedKind = "auto"
				}
				// Historical auto records retain their original SDK defaults.
				if _, err := h.a.DB.Exec(ctx, "UPDATE checkout_runs SET checkout_type=$2 WHERE order_id=$1", id, savedKind); err != nil {
					t.Fatal(err)
				}
				setSubscriptionFlow(t, h, subscriptionFlowLegacy)
				setCheckoutType(t, h, "hosted")
				job, err := h.a.claim(ctx, "checkout")
				if err != nil || job == nil {
					t.Fatal("claim failed", err)
				}
				calls := 0
				h.a.checkoutClient = func(cslive.Config, cslive.Hooks) protocolCheckoutClient {
					return checkoutClientFunc(func(_ context.Context, _ cslive.Session, o cslive.CreateCheckoutOptions) (cslive.CheckoutResult, error) {
						calls++
						wantType := kind
						if strings.HasPrefix(kind, "auto_") {
							wantType = ""
						}
						if o.CheckoutType != wantType {
							t.Fatal("worker used changed preference")
						}
						if err := h.a.BeforeSideEffect(ctx, job); err != nil {
							t.Fatal(err)
						}
						r := cslive.CheckoutResult{CheckoutID: "cs_live_Test", CheckoutURL: "https://pay.openai.com/c/pay/cs_live_Test#returned"}
						if kind == "oaics" || kind == "auto_custom" {
							r.CheckoutID = "oaics_Test"
							r.ProcessorEntity = "openai_llc"
							r.CheckoutURL = "https://chatgpt.com/checkout/openai_llc/oaics_Test"
						}
						if unknown {
							r.ErrorCode = "network_timeout"
						}
						return r, nil
					})
				}
				h.a.executeCheckout(ctx, job)
				var state, credentials string
				var leases, payments int
				err = h.a.DB.QueryRow(ctx, `SELECT j.state,c.credentials_enc,(SELECT count(*) FROM proxy_leases),(SELECT count(*) FROM payment_runs) FROM jobs j JOIN checkout_runs c ON c.order_id=j.order_id WHERE j.order_id=$1`, id).Scan(&state, &credentials, &leases, &payments)
				want := "completed"
				if unknown {
					want = "review"
				}
				if err != nil || state != want || credentials != "" || calls != 1 || leases != 0 || payments != 0 {
					t.Fatal("incorrect standalone finalization", err, state, calls, leases, payments)
				}
				detail := expect(t, h.request("GET", "/api/orders/"+id, nil, h.member, nil), 200)
				if detail["checkout"].(M)["ready"] != !unknown {
					t.Fatal("incorrect link visibility")
				}
				if next, err := h.a.claim(ctx, "checkout"); err != nil || next != nil {
					t.Fatal("completed or unknown result retried", err)
				}
			})
		}
	}
}

// The adapter must return the selected URL shape and bind it to the same order.
func TestProtocolLinkSelectedShortRoute(t *testing.T) {
	t.Setenv("XUPAY_LOCAL_PROXY", "")
	for _, mode := range []string{"short", "hosted", "other_order", "other_entity"} {
		t.Run(mode, func(t *testing.T) {
			in := checkoutInput{AccessToken: "PRIVATE_AT", SessionToken: "PRIVATE_SESSION", Country: "PH", Product: "pro5x", CheckoutType: "oaics"}
			calls := 0
			a := &App{checkoutClient: func(cslive.Config, cslive.Hooks) protocolCheckoutClient {
				return checkoutClientFunc(func(_ context.Context, _ cslive.Session, o cslive.CreateCheckoutOptions) (cslive.CheckoutResult, error) {
					calls++
					if o.CheckoutType != "oaics" {
						t.Fatal("selected short type changed")
					}
					r := cslive.CheckoutResult{CheckoutID: "cs_live_Test", ProcessorEntity: "openai_llc", CheckoutURL: "https://chatgpt.com/checkout/openai_llc/cs_live_Test"}
					switch mode {
					case "hosted":
						r.CheckoutURL = "https://pay.openai.com/c/pay/cs_live_Test#returned"
					case "other_order":
						r.CheckoutURL = "https://chatgpt.com/checkout/openai_llc/cs_live_Other"
					case "other_entity":
						r.CheckoutURL = "https://chatgpt.com/checkout/openai_ie/cs_live_Test"
					}
					return r, nil
				})
			}}
			result, err := a.generateProtocolCheckout(context.Background(), &Job{}, "http://user:pass@proxy.example.com:8080", in)
			if calls != 1 {
				t.Fatal("checkout repeated")
			}
			if mode == "short" {
				if err != nil || !validCustomCheckoutURL(result.URL) {
					t.Fatal("short route rejected", err)
				}
			} else if err == nil || result.URL != "" {
				t.Fatal("wrong checkout URL returned", mode)
			}
		})
	}
}
