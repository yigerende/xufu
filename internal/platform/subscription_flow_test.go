package platform

import (
	"context"
	"net/http"
	"strings"
	"testing"
	"time"
)

func setSubscriptionFlow(t *testing.T, h *harness, flow string) {
	t.Helper()
	cfg := h.a.config(context.Background(), "payment")
	cfg["subscription_flow"] = flow
	delete(cfg, "checkout_flow")
	expect(t, h.request("PUT", "/api/admin/settings/payment", cfg, h.admin, nil), 200)
}

func stopFlowTestWorkers(t *testing.T, h *harness) {
	t.Helper()
	t.Cleanup(func() {
		h.a.paymentMu.Lock()
		for _, s := range h.a.payments {
			s.cancel()
		}
		h.a.paymentMu.Unlock()
		until := time.Now().Add(3 * time.Second)
		for time.Now().Before(until) {
			h.a.paymentMu.Lock()
			n := len(h.a.payments)
			h.a.paymentMu.Unlock()
			if n == 0 {
				return
			}
			time.Sleep(10 * time.Millisecond)
		}
		t.Error("subscription test worker did not stop")
	})
}

func TestSubscriptionFlowSettingsAndMigration(t *testing.T) {
	h := setupTest(t)
	ctx := context.Background()
	if configuredSubscriptionFlow(h.a.config(ctx, "payment")) != subscriptionFlowCSLive {
		t.Fatal("upgrade must retain the deployed new flow")
	}
	setSubscriptionFlow(t, h, subscriptionFlowLegacy)
	for _, value := range []any{"", "unknown", true, nil, float64(1)} {
		cfg := h.a.config(ctx, "payment")
		cfg["subscription_flow"] = value
		expect(t, h.request("PUT", "/api/admin/settings/payment", cfg, h.admin, nil), 400)
	}
	cfg := h.a.config(ctx, "payment")
	cfg["subscription_flow"] = subscriptionFlowCSLive
	expect(t, h.request("PUT", "/api/admin/settings/payment", cfg, h.member, nil), 403)
	expect(t, h.request("PUT", "/api/admin/settings/payment", cfg, nil, nil), 401)
	delete(cfg, "subscription_flow")
	delete(cfg, "network_restart_limit")
	expect(t, h.request("PUT", "/api/admin/settings/payment", cfg, h.admin, nil), 200)
	if err := h.a.seed(ctx); err != nil {
		t.Fatal(err)
	}
	if configuredSubscriptionFlow(h.a.config(ctx, "payment")) != subscriptionFlowLegacy {
		t.Fatal("old admin clients or startup must not reset the saved choice")
	}
	if _, err := h.a.DB.Exec(ctx, "UPDATE settings SET value=value-'subscription_flow' WHERE key='payment'"); err != nil {
		t.Fatal(err)
	}
	if err := h.a.seed(ctx); err != nil {
		t.Fatal(err)
	}
	if configuredSubscriptionFlow(h.a.config(ctx, "payment")) != subscriptionFlowCSLive {
		t.Fatal("existing database did not receive the default")
	}
}

func TestSubscriptionFlowAdmissionReplayAndPrivacy(t *testing.T) {
	for _, flow := range []string{subscriptionFlowLegacy, subscriptionFlowCSLive, subscriptionFlowLegacyProtocol, subscriptionFlowCSLiveBrowser} {
		for _, route := range []string{"/api/orders", "/api/v1/orders"} {
			t.Run(flow+route, func(t *testing.T) {
				h := setupTest(t)
				ctx := context.Background()
				stopFlowTestWorkers(t, h)
				h.saveTestBilling("subscription@example.com")
				h.a.paymentStart = func(context.Context, paymentInput) (paymentProcess, error) {
					t.Error("admission must not reach an upstream payment")
					return nil, nil
				}
				if _, err := h.a.DB.Exec(ctx, "UPDATE users SET balance=100000 WHERE id=$1", h.memberID); err != nil {
					t.Fatal(err)
				}
				setSubscriptionFlow(t, h, flow)
				body := prepareRequest()
				body["workflow"], body["card"] = "subscription", paymentPrepareBody()["card"]
				if configuredCheckoutFlow(M{"subscription_flow": flow}) != subscriptionFlowCSLive {
					delete(body["session"].(M), "sessionToken")
				}
				headers := map[string]string{"Idempotency-Key": uid()}
				cookie := h.member
				if route == "/api/v1/orders" {
					key := expect(t, h.request("POST", "/api/keys", M{"name": "flow contract"}, h.member, nil), 200)["key"].(string)
					headers["Authorization"] = "Bearer " + key
					cookie = nil
				}
				id := str(expect(t, h.request("POST", route, body, cookie, headers), 200), "id")
				wantProtocol := flow == subscriptionFlowCSLive
				if h.a.isProtocolSubscription(ctx, id) != wantProtocol {
					t.Fatal("order did not persist the selected flow")
				}
				h.a.paymentMu.Lock()
				session := h.a.payments[id]
				correct := session != nil && (session.protocol != nil) == wantProtocol
				h.a.paymentMu.Unlock()
				if !correct {
					t.Fatal("wrong subscription worker dispatched")
				}
				other := subscriptionFlowCSLive
				if wantProtocol {
					other = subscriptionFlowLegacy
				}
				setSubscriptionFlow(t, h, other)
				replay := expect(t, h.request("POST", route, body, cookie, headers), 200)
				if replay["id"] != id || replay["replayed"] != true || h.a.isProtocolSubscription(ctx, id) != wantProtocol {
					t.Fatal("switch changed or duplicated an existing order")
				}
				var in paymentInput
				if err := h.a.legacyPaymentInput(ctx, id, session.attempt, &in); err != nil || in.Executor != subscriptionPaymentExecutor(flow) {
					t.Fatal("persisted executor changed after config switch", err)
				}
				if flow == subscriptionFlowLegacyProtocol {
					// No Node/Playwright dependency for protocol payment or recovery.
					t.Setenv("PAYMENT_RUNTIME_PATH", "missing-browser-runtime.mjs")
					if !h.a.orderPaymentEnabled(ctx, id) {
						t.Fatal("HTTP executor depends on browser runtime")
					}
					if err := h.a.seed(ctx); err != nil {
						t.Fatal(err)
					}
					in = paymentInput{Mode: "check"}
					if err := h.a.legacyPaymentInput(ctx, id, session.attempt, &in); err != nil || in.Executor != paymentExecutorLegacyHTTP {
						t.Fatal("restart lost original executor", err)
					}
				}
				detail := expect(t, h.request("GET", "/api/admin/orders/"+id, nil, h.admin, nil), 200)
				if detail["order"].(map[string]any)["subscription_flow"] != flow {
					t.Fatal("monitor shows current setting instead of the order's actual flow")
				}
				for _, view := range []struct {
					path   string
					cookie *http.Cookie
				}{{"/api/orders/" + id, h.member}, {route + "/" + id, cookie}} {
					var viewHeaders map[string]string
					if strings.HasPrefix(view.path, "/api/v1/") {
						viewHeaders = headers
					}
					w := h.request("GET", view.path, nil, view.cookie, viewHeaders)
					expect(t, w, 200)
					if strings.Contains(w.Body.String(), "subscription_flow") || strings.Contains(w.Body.String(), "credentials_enc") {
						t.Fatal("internal flow leaked to customer")
					}
				}
				paymentBalances(t, h, 90000, 10000)
				var holds, billings int
				if err := h.a.DB.QueryRow(ctx, "SELECT (SELECT count(*) FROM ledger WHERE order_id=$1 AND kind='freeze'),(SELECT count(*) FROM order_billing WHERE order_id=$1)", id).Scan(&holds, &billings); err != nil || holds != 1 || billings != 1 {
					t.Fatal("replay replaced billing or duplicated hold", holds, billings, err)
				}
			})
		}
	}
}

func TestSubscriptionFlowSessionValidation(t *testing.T) {
	h := setupTest(t)
	ctx := context.Background()
	body := prepareRequest()
	body["workflow"], body["card"] = "subscription", paymentPrepareBody()["card"]
	delete(body["session"].(M), "sessionToken")
	setSubscriptionFlow(t, h, subscriptionFlowCSLive)
	expect(t, h.request("POST", "/api/session/inspect", M{"session": body["session"], "workflow": "subscription"}, h.member, nil), 400)
	expect(t, h.request("POST", "/api/orders", body, h.member, map[string]string{"Idempotency-Key": uid()}), 400)
	setSubscriptionFlow(t, h, subscriptionFlowLegacy)
	expect(t, h.request("POST", "/api/session/inspect", M{"session": body["session"], "workflow": "subscription"}, h.member, nil), 200)
	// The independent link tool keeps its original Session contract in both modes.
	setSubscriptionFlow(t, h, subscriptionFlowCSLive)
	id := str(expect(t, h.request("POST", "/api/orders", M{"product_id": "pro", "session": body["session"], "country": "PH", "seats": 1, "accepted": true}, h.member, map[string]string{"Idempotency-Key": uid()}), 200), "id")
	if h.a.isProtocolSubscription(ctx, id) {
		t.Fatal("independent checkout tool was switched")
	}
}
