package platform

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"testing"
	"time"
)

func TestPaidOrderRetainsProxyThroughRenewal(t *testing.T) {
	for _, mode := range []string{"success", "activation_401", "keep", "retry", "expired", "missing", "disabled", "drift_before_read", "drift_before_refresh", "drift_before_cancel", "probe_network"} {
		t.Run(mode, func(t *testing.T) {
			ctx := context.Background()
			h, id := paidRefreshOrder(t, mode == "keep")
			var binding, resource, credentials, billing string
			if err := h.a.DB.QueryRow(ctx, `SELECT p.endpoint_enc,p.resource_id,s.credentials_enc,b.details_enc FROM order_proxy_sessions p
 JOIN subscription_runs s ON s.order_id=p.order_id JOIN order_billing b ON b.order_id=p.order_id WHERE p.order_id=$1`, id).Scan(&binding, &resource, &credentials, &billing); err != nil {
				t.Fatal(err)
			}
			endpoint, err := h.a.decrypt(binding)
			if err != nil {
				t.Fatal(err)
			}
			raw, err := h.a.decrypt(credentials)
			if err != nil {
				t.Fatal(err)
			}
			var in checkoutInput
			if json.Unmarshal([]byte(raw), &in) != nil {
				t.Fatal("credentials")
			}
			in.SessionToken = "TEST_RENEWAL_SESSION"
			data, _ := json.Marshal(in)
			credentials, _ = h.a.encrypt(string(data))
			h.a.DB.Exec(ctx, "UPDATE subscription_runs SET credentials_enc=$2 WHERE order_id=$1", id, credentials)
			// Neither the global selector nor editing the resource may replace this order's saved endpoint.
			global := h.addProxy("US", 2)
			chooseGlobal(t, h, global)
			replacement, _ := h.a.encrypt("http://changed:NOT_THE_ORDER_SESSION@changed.example:8080")
			h.a.DB.Exec(ctx, "UPDATE resources SET endpoint_enc=$2 WHERE id=$1", resource, replacement)
			switch mode {
			case "expired":
				h.a.DB.Exec(ctx, "UPDATE order_proxy_sessions SET expires_at=now()-interval '1 second' WHERE order_id=$1", id)
			case "missing":
				h.a.DB.Exec(ctx, "DELETE FROM order_proxy_sessions WHERE order_id=$1", id)
			case "disabled":
				h.a.DB.Exec(ctx, "UPDATE resources SET enabled=false WHERE id=$1", resource)
			}
			factories, probes, reads, refreshes, writes := 0, 0, 0, 0, 0
			h.a.renewalProxyClient = func(selected string, timeout time.Duration) (*http.Client, func(), error) {
				factories++
				if selected != endpoint {
					t.Fatal("did not reuse checkout proxy session")
				}
				c := &http.Client{Transport: testRoundTrip(func(r *http.Request) (*http.Response, error) {
					if r.URL.Host == "ipinfo.io" {
						probes++
						if r.Header.Get("Authorization") != "" || r.Header.Get("Cookie") != "" || r.Header.Get("Chatgpt-Account-Id") != "" {
							t.Fatal("probe leaked credentials")
						}
						if mode == "probe_network" {
							return nil, errors.New("test probe offline")
						}
						ip := "203.0.113.2"
						if mode == "drift_before_read" || mode == "drift_before_refresh" && probes >= 2 || mode == "drift_before_cancel" && probes >= 4 {
							ip = "203.0.113.3"
						}
						return providerResponse(200, M{"ip": ip}), nil
					}
					if r.URL.Path == "/api/auth/session" {
						refreshes++
						return providerResponse(200, testSession(in.Email)), nil
					}
					if r.Method == "POST" {
						writes++
						return providerResponse(200, M{"success": true}), nil
					}
					reads++
					if mode == "activation_401" && reads == 1 {
						return providerResponse(401, M{"error": M{"code": "token_expired"}}), nil
					}
					return providerResponse(200, M{"is_active": mode != "retry" || factories > 1, "will_renew": writes == 0, "plan_type": "premium"}), nil
				})}
				return c, c.CloseIdleConnections, nil
			}
			rounds := 1
			if mode == "retry" {
				rounds = 2
			}
			for i := 0; i < rounds; i++ {
				h.a.DB.Exec(ctx, "UPDATE subscription_runs SET next_at=now() WHERE order_id=$1", id)
				next, lease, e := h.a.claimRenewal(ctx)
				if e != nil || next != id {
					t.Fatal("claim", e)
				}
				h.a.runRenewal(ctx, id, lease)
			}
			var paid, fee, activation, renewal, billingAfter string
			var restarts int
			h.a.DB.QueryRow(ctx, `SELECT p.state,p.fee_state,p.network_restarts,o.activation,o.renewal,b.details_enc FROM orders o
 JOIN payment_runs p ON p.order_id=o.id JOIN order_billing b ON b.order_id=o.id WHERE o.id=$1`, id).Scan(&paid, &fee, &restarts, &activation, &renewal, &billingAfter)
			if paid != "succeeded" || fee != "charged" || restarts != 0 || billingAfter != billing {
				t.Fatal("changed payment, billing or fee")
			}
			switch mode {
			case "success", "retry", "probe_network", "drift_before_read", "drift_before_refresh", "drift_before_cancel":
				if renewal != "disabled" || activation != "active" || writes != 1 {
					t.Fatal("incomplete renewal", renewal, activation, writes)
				}
				want := 1
				if refreshes != want {
					t.Fatal("incorrect refresh order", refreshes)
				}
			case "keep":
				if renewal != "kept" || activation != "active" || refreshes != 0 || writes != 0 {
					t.Fatal("keep changed")
				}
			case "activation_401":
				if activation != "active" || renewal != "disabled" || refreshes != 2 || reads != 4 || writes != 1 {
					t.Fatal("initial rejected authorization did not refresh and recover", activation, renewal, refreshes, reads, writes)
				}
			default:
				if writes != 0 || renewal != "review" {
					t.Fatal("unsafe network continued", mode, renewal, writes)
				}
			}
			if mode != "missing" {
				var after string
				h.a.DB.QueryRow(ctx, "SELECT endpoint_enc FROM order_proxy_sessions WHERE order_id=$1", id).Scan(&after)
				if after != binding {
					t.Fatal("changed stored proxy session")
				}
			}
			var held int
			h.a.DB.QueryRow(ctx, "SELECT count(*) FROM proxy_leases WHERE resource_id=$1", resource).Scan(&held)
			if held != 0 {
				t.Fatal("leaked proxy capacity", held)
			}
			if mode == "expired" || mode == "missing" || mode == "disabled" {
				if factories != 0 {
					t.Fatal("opened fallback client")
				}
			}
			paymentBalances(t, h, 90000, 0)
			assertSubscriptionPrivate(t, h, id)
			if probes != 0 {
				t.Fatal("subscription performed an exit-IP probe")
			}
		})
	}
}
