package platform

import (
	"context"
	"strings"
	"testing"
)

func TestWholeFlowConfigurationMigrationAndValidation(t *testing.T) {
	h := setupTest(t)
	ctx := context.Background()
	for _, flow := range []string{"legacy", "cslive"} {
		// Old split configurations converge on the saved payment/full-flow choice.
		_, err := h.a.DB.Exec(ctx, `UPDATE settings SET value=value || jsonb_build_object('checkout_flow','legacy','subscription_flow',$1::text) WHERE key='payment'`, flow)
		if err != nil {
			t.Fatal(err)
		}
		if err = h.a.seed(ctx); err != nil {
			t.Fatal(err)
		}
		cfg := h.a.config(ctx, "payment")
		if _, exists := cfg["checkout_flow"]; exists {
			t.Fatal("obsolete setting retained")
		}
		if configuredCheckoutFlow(cfg) != flow || configuredSubscriptionFlow(cfg) != flow {
			t.Fatal("whole-flow choice changed")
		}
		delete(cfg, "subscription_flow")
		expect(t, h.request("PUT", "/api/admin/settings/payment", cfg, h.admin, nil), 200)
		if configuredSubscriptionFlow(h.a.config(ctx, "payment")) != flow {
			t.Fatal("omitted choice reset")
		}
		cfg = h.a.config(ctx, "payment")
		cfg["checkout_flow"] = "legacy"
		cfg["subscription_flow"] = "cslive"
		expect(t, h.request("PUT", "/api/admin/settings/payment", cfg, h.admin, nil), 400)
		cfg["checkout_flow"] = "cslive"
		cfg["subscription_flow"] = "legacy"
		expect(t, h.request("PUT", "/api/admin/settings/payment", cfg, h.admin, nil), 400)
	}
	_, err := h.a.DB.Exec(ctx, "DELETE FROM settings WHERE key='payment'")
	if err != nil {
		t.Fatal(err)
	}
	if err = h.a.seed(ctx); err != nil {
		t.Fatal(err)
	}
	cfg := h.a.config(ctx, "payment")
	if configuredCheckoutFlow(cfg) != "cslive" || configuredSubscriptionFlow(cfg) != "cslive" {
		t.Fatal("new installation must select one complete flow")
	}
}

func TestWholeFlowAdmissionAndLinkIsolation(t *testing.T) {
	for _, payment := range []string{"legacy", "cslive", "legacy_protocol", "cslive_browser"} {
		for _, checkout := range []string{configuredCheckoutFlow(M{"subscription_flow": payment})} {
			t.Run(checkout+"_"+payment, func(t *testing.T) {
				h := setupTest(t)
				stopFlowTestWorkers(t, h)
				ctx := context.Background()
				h.saveTestBilling("subscription@example.com")
				h.a.paymentStart = func(context.Context, paymentInput) (paymentProcess, error) {
					t.Error("unexpected payment")
					return nil, nil
				}
				_, _ = h.a.DB.Exec(ctx, "UPDATE users SET balance=100000 WHERE id=$1", h.memberID)
				setSubscriptionFlow(t, h, payment)
				body := prepareRequest()
				body["workflow"], body["card"] = "subscription", paymentPrepareBody()["card"]
				id := str(expect(t, h.request("POST", "/api/orders", body, h.member, map[string]string{"Idempotency-Key": uid()}), 200), "id")
				var actual string
				if err := h.a.DB.QueryRow(ctx, "SELECT flow FROM checkout_runs WHERE order_id=$1", id).Scan(&actual); err != nil || actual != checkout {
					t.Fatal("wrong checkout stage", err)
				}
				if h.a.isProtocolSubscription(ctx, id) != (payment == "cslive") {
					t.Fatal("wrong payment stage")
				}
				order := expect(t, h.request("GET", "/api/admin/orders/"+id, nil, h.admin, nil), 200)["order"].(M)
				if order["checkout_flow"] != checkout || order["subscription_flow"] != payment {
					t.Fatal("monitor lost combination")
				}
				customer := h.request("GET", "/api/orders/"+id, nil, h.member, nil)
				if strings.Contains(customer.Body.String(), "checkout_flow") || strings.Contains(customer.Body.String(), "subscription_flow") {
					t.Fatal("private flow exposed")
				}
				// Independent link generation depends only on its own stage.
				link := prepareRequest()
				delete(link["session"].(M), "sessionToken")
				want := 400
				if checkout == "legacy" {
					want = 200
				}
				expect(t, h.request("POST", "/api/session/inspect", M{"session": link["session"], "workflow": "checkout"}, h.member, nil), want)
				out := expect(t, h.request("POST", "/api/orders", link, h.member, map[string]string{"Idempotency-Key": uid()}), want)
				if want == 200 && h.a.isProtocolSubscription(ctx, str(out, "id")) {
					t.Fatal("link tool gained payment stage")
				}
			})
		}
	}
}
