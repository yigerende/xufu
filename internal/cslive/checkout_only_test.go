package cslive

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"testing"
)

func TestStandaloneCheckoutOnlyReturnsRealHostedLink(t *testing.T) {
	for _, mode := range []string{"direct", "default", "custom_cs", "custom_oaics", "custom_hosted", "stripe_init", "missing", "short", "other_order", "timeout"} {
		t.Run(mode, func(t *testing.T) {
			flow, tr, saved := flowFixture(t, false)
			client := flow.client
			client.Checkpoint = nil // Standalone checkout must not initialize payment devices.
			long := "https://checkout.stripe.com/c/pay/cs_live_Fixture#" + strings.Repeat("a", 260)
			creates, inits := 0, 0
			custom := strings.HasPrefix(mode, "custom_") || mode == "default"
			checkoutID := "cs_live_Fixture"
			if mode == "custom_oaics" {
				checkoutID = "oaics_Fixture"
			}
			tr.request = func(method, path string, opts protocol_RequestOptions) (*protocol_HTTPResponse, error) {
				switch path {
				case "/":
					r := response(200, "")
					r.Content = []byte(`<html data-build="build" data-seq="42"></html>`)
					r.Headers.Add("Set-Cookie", "oai-did=11111111-1111-4111-8111-111111111111; Path=/; Secure")
					return r, nil
				case "/api/auth/session":
					return response(200, protocol_Map{"accessToken": "new-at", "account": protocol_Map{"id": "account"}}), nil
				case "/backend-api/accounts/check/v4-2023-04-27":
					return response(200, protocol_Map{"accounts": protocol_Map{"account": protocol_Map{"account": protocol_Map{"id": "account", "account_type": "personal", "plan_type": "free"}, "entitlement": protocol_Map{"has_active_subscription": false}}}}), nil
				case "/backend-api/subscriptions":
					if method != "GET" {
						t.Fatal("standalone checkout modified subscription")
					}
					return response(404, protocol_Map{"detail": "No subscription found for account"}), nil
				case "/backend-api/payments/checkout":
					creates++
					body, _ := json.Marshal(opts.Body)
					wantMode := "hosted"
					if custom {
						wantMode = "custom"
					}
					if !strings.Contains(string(body), `"checkout_ui_mode":"`+wantMode+`"`) {
						t.Fatal("wrong requested checkout mode")
					}
					if opts.Headers.Get("Authorization") != "Bearer new-at" {
						t.Fatal("checkout did not use refreshed token")
					}
					if mode == "timeout" {
						return nil, Failure("network_timeout")
					}
					r := protocol_Map{"checkout_session_id": checkoutID, "checkout_ui_mode": "custom", "publishable_key": "pk_test_Fixture"}
					if mode == "direct" || mode == "custom_hosted" {
						r["checkout_ui_mode"], r["url"] = "hosted", long
					}
					return response(200, r), nil
				case "/v1/payment_pages/cs_live_Fixture/init":
					inits++
					r := protocol_Map{}
					switch mode {
					case "stripe_init":
						r["stripe_hosted_url"] = long
					case "short":
						r["url"] = "https://chatgpt.com/checkout/openai_llc/cs_live_Fixture"
					case "other_order":
						r["stripe_hosted_url"] = strings.Replace(long, "cs_live_Fixture", "cs_live_Other", 1)
					}
					return response(200, r), nil
				default:
					t.Fatalf("standalone checkout attempted unrelated request %s %s", method, path)
					return nil, nil
				}
			}
			checkoutType := "hosted"
			if custom {
				checkoutType = "oaics"
			}
			if mode == "default" {
				checkoutType = ""
			}
			result, err := client.CreateCheckout(context.Background(), Session{AccessToken: "old-at", SessionToken: "session", AccountID: "account", Email: "fixture@example.com"}, CreateCheckoutOptions{Country: "PH", Plan: "pro5x", CheckoutType: checkoutType})
			if err != nil || creates != 1 || len(*saved) != 0 {
				t.Fatal("unexpected creation or payment checkpoint", err, creates, result.ErrorCode, tr.calls)
			}
			if custom {
				if result.ErrorCode != "" || result.CheckoutURL != "https://chatgpt.com/checkout/openai_llc/"+checkoutID || inits != 0 {
					t.Fatal("reference Custom short link not returned", result.ErrorCode)
				}
			} else if mode == "direct" || mode == "stripe_init" {
				if result.ErrorCode != "" || result.CheckoutURL != strings.Replace(long, "checkout.stripe.com", "pay.openai.com", 1) {
					t.Fatal("valid hosted link not returned", result.ErrorCode)
				}
			} else if result.ErrorCode == "" || result.CheckoutURL != "" {
				t.Fatal("fabricated link or ignored unknown result")
			}
			if inits > 1 {
				t.Fatal("repeated init")
			}
			if result.ErrorCode == "" {
				if result.Session == nil || result.Session.AccessToken != "new-at" || result.Session.AccountID != "account" || result.Session.Email != "fixture@example.com" {
					t.Fatal("successful checkout lost refreshed first-party authorization")
				}
			} else if result.Session != nil {
				t.Fatal("failed checkout returned authorization for continuation")
			}
			public, _ := json.Marshal(result)
			for _, text := range []string{string(public), fmt.Sprint(result), fmt.Sprintf("%#v", result)} {
				if strings.Contains(text, "new-at") || strings.Contains(text, "fixture@example.com") || strings.Contains(text, "access_token") {
					t.Fatal("private checkout authorization leaked")
				}
			}
		})
	}
}
