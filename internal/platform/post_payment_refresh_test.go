package platform

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"strings"
	"testing"
	"time"
)

func TestPostPaymentReferenceActivationAndCancellation(t *testing.T) {
	cases := []struct {
		mode, sequence, activation, renewal string
	}{
		{"success", "read0,refresh,read1,cancel1,read1", "active", "disabled"},
		{"activation_401", "read0,refresh,read1,refresh,read2,cancel2,read2", "active", "disabled"},
		{"expired_at", "read0,refresh,read1,refresh,read2,cancel2,read2", "active", "disabled"},
		{"keep", "read0", "active", "kept"},
		{"keep_401", "read0,refresh,read1", "active", "kept"},
		{"cancel_already_sent", "read0,refresh,read1", "active", "disabled"},
		{"refresh_401", "read0,refresh", "active", "review"},
		{"refresh_403", "read0,refresh", "active", "review"},
		{"wrong_account", "read0,refresh", "active", "review"},
		{"activation_refresh_401", "read0,refresh", "review", "review"},
		{"activation_refresh_403", "read0,refresh", "review", "review"},
		{"activation_wrong_account", "read0,refresh", "review", "review"},
		{"activation_403", "read0", "review", "review"},
		{"activation_pending", "read0", "review", "review"},
	}
	for _, tc := range cases {
		t.Run(tc.mode, func(t *testing.T) {
			ctx := context.Background()
			h, id := paidRefreshOrder(t, strings.HasPrefix(tc.mode, "keep"))
			var enc, billingBefore string
			if err := h.a.DB.QueryRow(ctx, "SELECT s.credentials_enc,b.details_enc FROM subscription_runs s JOIN order_billing b ON b.order_id=s.order_id WHERE s.order_id=$1", id).Scan(&enc, &billingBefore); err != nil {
				t.Fatal(err)
			}
			raw, err := h.a.decrypt(enc)
			if err != nil {
				t.Fatal(err)
			}
			var in checkoutInput
			if err = json.Unmarshal([]byte(raw), &in); err != nil {
				t.Fatal(err)
			}
			in.SessionToken = "TEST_POST_PAYMENT_SESSION"
			in.RefreshedAt, in.RefreshPurpose = time.Now(), "checkout"
			if tc.mode == "expired_at" {
				in.TokenExpiresAt = time.Now().Add(-time.Minute)
			}
			saved, _ := json.Marshal(in)
			enc, _ = h.a.encrypt(string(saved))
			if _, err = h.a.DB.Exec(ctx, "UPDATE subscription_runs SET credentials_enc=$2,cancel_started=$3 WHERE order_id=$1", id, enc, tc.mode == "cancel_already_sent"); err != nil {
				t.Fatal(err)
			}
			if _, err = h.a.DB.Exec(ctx, `UPDATE settings SET value=value || '{"activation_check_limit":1,"renewal_check_limit":1}'::jsonb WHERE key='payment'`); err != nil {
				t.Fatal(err)
			}
			refreshes, generation, reads, cancels := 0, 0, 0, 0
			currentAT := in.AccessToken
			sequence := []string{}
			h.a.renewalClient = func() *http.Client {
				return &http.Client{Transport: testRoundTrip(func(r *http.Request) (*http.Response, error) {
					if r.URL.Path == "/api/auth/session" {
						sequence = append(sequence, "refresh")
						refreshes++
						var paid string
						if err := h.a.DB.QueryRow(ctx, "SELECT state FROM payment_runs WHERE order_id=$1", id).Scan(&paid); err != nil || paid != "succeeded" {
							t.Fatal("refresh ran before payment success", err, paid)
						}
						if reads != refreshes || cancels != 0 || r.Method != "GET" || r.Header.Get("Authorization") != "" {
							t.Fatal("refresh must follow a read and precede cancellation")
						}
						if r.URL.RawQuery != "refresh=true&reason=token_expired" || r.Header.Get("X-Openai-Target-Path") != "/api/auth/session" || r.Header.Get("X-Openai-Target-Route") != "/api/auth/session" {
							t.Fatal("refresh request no longer matches the official session route")
						}
						cookie, err := r.Cookie("__Secure-next-auth.session-token")
						if err != nil || cookie.Value != in.SessionToken {
							t.Fatal("refresh cookie missing")
						}
						if tc.mode == "refresh_401" && refreshes == 1 || tc.mode == "activation_refresh_401" {
							return providerResponse(401, M{"error": "rejected"}), nil
						}
						if tc.mode == "refresh_403" && refreshes == 1 || tc.mode == "activation_refresh_403" {
							return providerResponse(403, M{"error": "rejected"}), nil
						}
						fresh := testSession(in.Email)
						if tc.mode == "wrong_account" && refreshes == 1 || tc.mode == "activation_wrong_account" {
							fresh["account"] = M{"id": "other-account"}
						} else {
							generation++
							currentAT = str(fresh, "accessToken") + fmt.Sprint("REFRESHED", generation)
							fresh["accessToken"] = currentAT
						}
						return providerResponse(200, fresh), nil
					}
					wantPath := "/backend-api/subscriptions"
					if r.Method == "POST" {
						wantPath += "/cancel"
					}
					if r.URL.Path != wantPath || r.Header.Get("Authorization") != "Bearer "+currentAT {
						t.Fatal("request did not use the current AT")
					}
					var current string
					if err := h.a.DB.QueryRow(ctx, "SELECT credentials_enc FROM subscription_runs WHERE order_id=$1", id).Scan(&current); err != nil {
						t.Fatal(err)
					}
					decoded, err := h.a.decrypt(current)
					var persisted checkoutInput
					if err != nil || json.Unmarshal([]byte(decoded), &persisted) != nil || persisted.AccessToken != currentAT {
						t.Fatal("request used AT before persisting it")
					}
					if r.Method == "POST" {
						sequence = append(sequence, fmt.Sprint("cancel", generation))
						cancels++
						var started bool
						if err := h.a.DB.QueryRow(ctx, "SELECT cancel_started FROM subscription_runs WHERE order_id=$1", id).Scan(&started); err != nil || !started {
							t.Fatal("cancel fence not persisted", err)
						}
						if tc.mode == "cancel_already_sent" || cancels > 1 || generation == 0 {
							t.Fatal("cancellation replayed or not refreshed")
						}
						return providerResponse(200, M{"success": true}), nil
					}
					sequence = append(sequence, fmt.Sprint("read", generation))
					reads++
					if reads == 1 && (tc.mode == "activation_401" || tc.mode == "keep_401" || tc.mode == "expired_at" || strings.HasPrefix(tc.mode, "activation_refresh_") || tc.mode == "activation_wrong_account") {
						return providerResponse(401, M{"error": M{"code": "token_expired"}}), nil
					}
					if tc.mode == "activation_403" {
						return providerResponse(403, M{"error": "rejected"}), nil
					}
					return providerResponse(200, M{"is_active": tc.mode != "activation_pending", "will_renew": cancels == 0 && tc.mode != "cancel_already_sent", "plan_type": "premium"}), nil
				})}
			}
			next, lease, err := h.a.claimRenewal(ctx)
			if err != nil || next != id {
				t.Fatal("paid renewal not claimed", err)
			}
			h.a.runRenewal(ctx, id, lease)
			if got := strings.Join(sequence, ","); got != tc.sequence {
				t.Fatalf("request order %s; want %s", got, tc.sequence)
			}
			var paid, fee, renewal, activation, billingAfter string
			if err = h.a.DB.QueryRow(ctx, "SELECT p.state,p.fee_state,o.renewal,o.activation,b.details_enc FROM payment_runs p JOIN orders o ON o.id=p.order_id JOIN order_billing b ON b.order_id=p.order_id WHERE p.order_id=$1", id).Scan(&paid, &fee, &renewal, &activation, &billingAfter); err != nil {
				t.Fatal(err)
			}
			if paid != "succeeded" || fee != "charged" || billingBefore != billingAfter || refreshes > 2 {
				t.Fatal("paid state, fee, billing or bounded refresh changed", paid, fee, refreshes)
			}
			if activation != tc.activation || renewal != tc.renewal {
				t.Fatal("incorrect result", activation, renewal)
			}
			paymentBalances(t, h, 90000, 0)
			assertSubscriptionPrivate(t, h, id)
		})
	}
}
