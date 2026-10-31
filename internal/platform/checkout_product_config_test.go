package platform

import (
	"context"
	"strings"
	"testing"
)

func TestCheckoutProductSettingsValidationAndUpgrade(t *testing.T) {
	h := setupTest(t)
	ctx := context.Background()
	for _, product := range checkoutProducts {
		if configuredProductCheckoutType(h.a.config(ctx, "payment"), product.ID) != product.Default {
			t.Fatal("wrong product default", product.ID)
		}
	}
	for _, value := range []any{nil, "hosted", true, M{}, M{"pro": "hosted"}, M{"pro": "hosted", "pro5x": "hosted", "team": "oaics", "team_premium": "oaics", "plus": "oaics", "other": "oaics"}} {
		cfg := h.a.config(ctx, "payment")
		cfg["checkout_types"] = value
		expect(t, h.request("PUT", "/api/admin/settings/payment", cfg, h.admin, nil), 400)
	}
	for _, invalid := range []any{"auto", "long", "custom", nil, true} {
		cfg := h.a.config(ctx, "payment")
		mapping := defaultCheckoutTypes()
		mapping["team"] = invalid
		cfg["checkout_types"] = mapping
		expect(t, h.request("PUT", "/api/admin/settings/payment", cfg, h.admin, nil), 400)
	}
	cfg := h.a.config(ctx, "payment")
	mapping := defaultCheckoutTypes()
	mapping["team"] = "hosted"
	mapping["pro"] = "oaics"
	cfg["checkout_types"] = mapping
	expect(t, h.request("PUT", "/api/admin/settings/payment", cfg, h.member, nil), 403)
	expect(t, h.request("PUT", "/api/admin/settings/payment", cfg, h.admin, nil), 200)
	delete(cfg, "checkout_types") // An old admin client cannot reset the product rules.
	expect(t, h.request("PUT", "/api/admin/settings/payment", cfg, h.admin, nil), 200)
	if err := h.a.seed(ctx); err != nil {
		t.Fatal(err)
	}
	cfg = h.a.config(ctx, "payment")
	if configuredProductCheckoutType(cfg, "team") != "hosted" || configuredProductCheckoutType(cfg, "pro") != "oaics" {
		t.Fatal("saved rules reset")
	}
	if _, err := h.a.DB.Exec(ctx, "UPDATE settings SET value=value-'checkout_types' WHERE key='payment'"); err != nil {
		t.Fatal(err)
	}
	if err := h.a.seed(ctx); err != nil {
		t.Fatal(err)
	}
	for _, product := range checkoutProducts {
		if configuredProductCheckoutType(h.a.config(ctx, "payment"), product.ID) != product.Default {
			t.Fatal("migration missed product")
		}
	}
}

func TestCheckoutProductSelectionBothFlowsAndPages(t *testing.T) {
	for _, flow := range []string{subscriptionFlowLegacy, subscriptionFlowCSLive, subscriptionFlowLegacyProtocol, subscriptionFlowCSLiveBrowser} {
		for _, workflow := range []string{"checkout", "subscription"} {
			for _, product := range []string{"pro5x", "pro", "team", "team_premium"} {
				t.Run(flow+"/"+workflow+"/"+product, func(t *testing.T) {
					h := setupTest(t)
					ctx := context.Background()
					stopFlowTestWorkers(t, h)
					h.a.paymentStart = func(context.Context, paymentInput) (paymentProcess, error) {
						t.Error("admission launched payment")
						return nil, nil
					}
					setSubscriptionFlow(t, h, flow)
					cfg := h.a.config(ctx, "payment")
					want := str(defaultCheckoutTypes(), product)
					// Exercise a manual override on one product without changing siblings.
					if product == "team_premium" {
						mapping := defaultCheckoutTypes()
						mapping[product] = "hosted"
						cfg["checkout_types"] = mapping
						want = "hosted"
					}
					cfg["checkout_type"] = map[string]string{"hosted": "oaics", "oaics": "hosted"}[want] // obsolete global option cannot override the product rule
					expect(t, h.request("PUT", "/api/admin/settings/payment", cfg, h.admin, nil), 200)
					body := prepareRequest()
					body["product_id"] = product
					body["workflow"] = workflow
					body["seats"] = 2
					if workflow == "subscription" {
						h.saveTestBilling("subscription@example.com")
						h.a.DB.Exec(ctx, "UPDATE users SET balance=100000 WHERE id=$1", h.memberID)
						body["card"] = paymentPrepareBody()["card"]
					}
					headers := map[string]string{"Idempotency-Key": uid()}
					id := str(expect(t, h.request("POST", "/api/orders", body, h.member, headers), 200), "id")
					var saved, executor string
					if err := h.a.DB.QueryRow(ctx, "SELECT checkout_type,flow FROM checkout_runs WHERE order_id=$1", id).Scan(&saved, &executor); err != nil || saved != want || executor != configuredCheckoutFlow(M{"subscription_flow": flow}) {
						t.Fatal("wrong per-product selection", saved, executor, err)
					}
					cfg = h.a.config(ctx, "payment")
					cfg["checkout_types"].(M)[product] = map[string]string{"hosted": "oaics", "oaics": "hosted"}[want]
					expect(t, h.request("PUT", "/api/admin/settings/payment", cfg, h.admin, nil), 200)
					replayed := expect(t, h.request("POST", "/api/orders", body, h.member, headers), 200)
					if err := h.a.DB.QueryRow(ctx, "SELECT checkout_type FROM checkout_runs WHERE order_id=$1", id).Scan(&saved); err != nil || saved != want || replayed["id"] != id || replayed["replayed"] != true {
						t.Fatal("replay changed original strategy", err)
					}
					view := h.request("GET", "/api/orders/"+id, nil, h.member, nil)
					expect(t, view, 200)
					if strings.Contains(view.Body.String(), "checkout_types") {
						t.Fatal("admin map leaked")
					}
				})
			}
		}
	}
}
