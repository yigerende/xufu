package cslive

import (
	"context"
	"encoding/json"
	"strings"
	"testing"
)

func TestReferenceHostedPayAndRecovery(t *testing.T) {
	for _, mode := range []string{"direct", "custom_direct", "custom_init", "missing_mode", "invalid_mode", "missing", "short", "other_order", "oaics", "upgrade", "unknown", "action_required", "declined", "authorized"} {
		t.Run(mode, func(t *testing.T) {
			flow, tr, saved := flowFixture(t, true)
			long := "https://pay.openai.com/c/pay/cs_live_Fixture#" + strings.Repeat("a", 260)
			creates, inits, cards, confirms, approvals, cancels := 0, 0, 0, 0, 0, 0
			recovering := false
			charged := func() protocol_Map {
				return protocol_Map{"id": "cs_live_Fixture", "status": "complete", "payment_status": "paid", "payment_intent": protocol_Map{"id": "pi_Fixture", "client_secret": "pi_Fixture_secret_Fixture", "status": "succeeded", "amount_received": 1500, "currency": "php"}}
			}
			unpaid := func() protocol_Map {
				status := "requires_capture"
				if mode == "declined" {
					status = "requires_payment_method"
				}
				return protocol_Map{"id": "cs_live_Fixture", "payment_intent": protocol_Map{"id": "pi_Fixture", "client_secret": "pi_Fixture_secret_Fixture", "status": status, "last_payment_error": protocol_Map{"code": "card_declined"}}}
			}
			tr.request = func(method, path string, opts protocol_RequestOptions) (*protocol_HTTPResponse, error) {
				switch path {
				case "/":
					r := response(200, nil)
					r.Content = []byte(`<html data-build="build" data-seq="42"></html>`)
					r.Headers.Add("Set-Cookie", "oai-did=11111111-1111-4111-8111-111111111111; Path=/; Secure")
					return r, nil
				case "/api/auth/session":
					return response(200, protocol_Map{"accessToken": "new-at", "account": protocol_Map{"id": "account"}}), nil
				case "/backend-api/accounts/check/v4-2023-04-27":
					plan := "free"
					if mode == "upgrade" {
						plan = "plus"
					}
					return response(200, protocol_Map{"accounts": protocol_Map{"account": protocol_Map{"account": protocol_Map{"id": "account", "account_type": "personal", "plan_type": plan}, "entitlement": protocol_Map{"has_active_subscription": mode == "upgrade"}}}}), nil
				case "/backend-api/subscriptions":
					if method != "GET" {
						t.Fatal("unexpected subscription write")
					}
					if confirms == 0 && mode != "upgrade" {
						return response(404, protocol_Map{"detail": "No subscription found for account"}), nil
					}
					plan := "premium"
					if mode == "upgrade" {
						plan = "plus"
					}
					return response(200, protocol_Map{"plan_type": plan, "will_renew": cancels == 0, "active_until": "2099-01-01T00:00:00Z"}), nil
				case "/backend-api/payments/stripe_client_bootstrap":
					return response(200, protocol_Map{"publishable_key": "pk_test_Fixture"}), nil
				case "/basil/stripe.js":
					r := response(200, nil)
					r.Content = []byte(`/*! STRIPE_JS_BUILD_SALT 123456789a */ "abcdef1234"`)
					return r, nil
				case "/6":
					return response(200, protocol_Map{"guid": "fixture-guid", "muid": "fixture-muid", "sid": "fixture-sid"}), nil
				case "/backend-api/payments/checkout":
					creates++
					if recovering || creates != 1 {
						t.Fatal("checkout recreated")
					}
					payload, _ := protocol_CompactJSON(opts.Body)
					if !strings.Contains(payload, `"checkout_ui_mode":"hosted"`) {
						t.Fatal("did not request hosted checkout")
					}
					r := protocol_Map{"checkout_session_id": "cs_live_Fixture", "checkout_ui_mode": "hosted", "publishable_key": "pk_test_Fixture"}
					if strings.HasPrefix(mode, "custom") || mode == "oaics" {
						r["checkout_ui_mode"] = "custom"
					}
					if mode == "missing_mode" {
						delete(r, "checkout_ui_mode")
					}
					if mode == "invalid_mode" {
						r["checkout_ui_mode"] = "unrecognized"
					}
					if mode == "oaics" {
						r["checkout_session_id"] = "oaics_Fixture"
					}
					if mode == "direct" || mode == "custom_direct" || mode == "unknown" || mode == "action_required" {
						r["url"] = long
					}
					return response(200, r), nil
				case "/v1/payment_pages/cs_live_Fixture/init":
					inits++
					r := protocol_Map{"ui_mode": "hosted", "eid": "fixture", "init_checksum": "fixture-checksum", "amount_total": 1500, "currency": "php"}
					switch mode {
					case "missing":
					case "short":
						r["url"] = "https://chatgpt.com/checkout/openai_llc/cs_live_Fixture"
					case "other_order":
						r["stripe_hosted_url"] = strings.Replace(long, "cs_live_Fixture", "cs_live_Other", 1)
					default:
						r["stripe_hosted_url"] = long
					}
					return response(200, r), nil
				case "/c/pay/cs_live_Fixture":
					if (*saved)[len(*saved)-1].Metadata["checkout_ui_mode"] != "redirect" {
						t.Fatal("hosted path not saved before preparing payment")
					}
					return response(200, "hosted page"), nil
				case "/v3":
					r := response(200, nil)
					r.Content = []byte(`/*! STRIPE_JS_BUILD_SALT */ "abcdef1234"; /*! STRIPE_JS_BUILD_SALT */ "` + strings.Repeat("b", 40) + `"; rvTs:"2026-10-29 00:00:00 -0000"; sv:/*! STRIPE_JS_BUILD_SALT */ "` + strings.Repeat("c", 64) + `"`)
					return r, nil
				case "/b", "/edge-internal/card-metadata":
					return response(200, protocol_Map{}), nil
				case "/v1/payment_pages/cs_live_Fixture":
					if confirms > 0 && (mode == "declined" || mode == "authorized") {
						return response(200, unpaid()), nil
					}
					if recovering {
						return response(200, charged()), nil
					}
					return response(200, protocol_Map{"id": "cs_live_Fixture", "amount_total": 1500, "currency": "php"}), nil
				case "/v1/payment_methods":
					cards++
					if approvals != 1 || recovering {
						t.Fatal("card operation outside approved hosted flow")
					}
					return response(200, protocol_Map{"id": "pm_Fixture"}), nil
				case "/v1/payment_pages/cs_live_Fixture/confirm":
					confirms++
					cp := (*saved)[len(*saved)-1]
					if confirms != 1 || recovering || cp.Phase != "hosted_confirm" || cp.Payment != "unknown" || cp.Metadata["checkout_ui_mode"] != "redirect" || cp.Metadata["hosted_url"] != long {
						t.Fatal("wrong payment path or missing durable boundary")
					}
					if mode == "unknown" {
						return nil, Failure("network_timeout")
					}
					if mode == "action_required" {
						return response(200, protocol_Map{"payment_intent": protocol_Map{"id": "pi_Fixture", "client_secret": "pi_Fixture_secret_Fixture", "status": "requires_action"}}), nil
					}
					if mode == "declined" || mode == "authorized" {
						return response(200, unpaid()), nil
					}
					return response(200, charged()), nil
				case "/v1/payment_intents/pi_Fixture":
					if !recovering {
						t.Fatal("unexpected payment intent read")
					}
					return response(200, protocol_Map{"id": "pi_Fixture", "status": "succeeded", "amount_received": 1500, "currency": "php"}), nil
				case "/backend-api/payments/checkout/snapshot", "/checkout/verify":
					return response(200, protocol_Map{}), nil
				case "/backend-api/subscriptions/cancel":
					cancels++
					return response(200, protocol_Map{}), nil
				default:
					t.Fatalf("hosted-only flow attempted unexpected request %s %s", method, path)
					return nil, nil
				}
			}
			plan := "pro20x"
			if mode == "upgrade" {
				plan = "pro5x"
			}
			checkoutType := "hosted"
			if mode == "default" {
				checkoutType = ""
			}
			session := Session{AccessToken: "old-at", SessionToken: "session", AccountID: "account", Email: "fixture@example.com"}
			billing := Billing{Name: "QA Fixture", Email: "fixture@example.com", Country: "US", Line1: "100 Test Street", City: "New York", State: "NY", PostalCode: "10001"}
			if mode == "quote" {
				result, err := flow.client.Quote(context.Background(), session, gptpay_QuoteOptions{Country: "PH", Plan: plan, Billing: billing})
				if err != nil || result.ErrorCode != "" || result.Payment != "not_attempted" || result.Amount == nil || *result.Amount != 1500 || creates != 1 || cards != 0 || confirms != 0 || approvals != 0 || cancels != 0 || result.Checkpoint.Metadata["hosted_url"] != long {
					t.Fatal("quote did not use hosted-only pricing", err, result.ErrorCode)
				}
				return
			}
			wantCreates := 1
			result, err := flow.client.Pay(context.Background(), session, Card{Number: "4242424242424242", CVC: "123", ExpMonth: "12", ExpYear: "2035"}, PayOptions{Country: "PH", Plan: plan, CheckoutType: checkoutType, Billing: billing, ApprovePayment: func(amount int64, currency string) (bool, error) {
				approvals++
				if amount != 1500 || currency != "PHP" {
					t.Fatal("incorrect hosted quote")
				}
				return true, nil
			}})
			if err != nil {
				t.Fatal(err)
			}
			blocked := mode == "missing" || mode == "short" || mode == "other_order" || mode == "oaics" || mode == "upgrade"
			if blocked {
				if result.ErrorCode == "" || result.Payment != "not_attempted" || cards != 0 || confirms != 0 || approvals != 0 || cancels != 0 {
					t.Fatal("non-hosted flow reached payment", result.ErrorCode, result.Payment)
				}
				if mode == "upgrade" && creates != 0 {
					t.Fatal("upgrade created a replacement checkout")
				}
			} else {
				if creates != wantCreates || cards != 1 || confirms != 1 || approvals != 1 || inits > 2 {
					t.Fatal("hosted path failed", result.ErrorCode, result.WarningCode, creates, cards, confirms, approvals, inits)
				}
				if mode == "declined" || mode == "authorized" {
					want := "not_charged"
					if mode == "authorized" {
						want = "authorized"
					}
					if string(result.Payment) != want || cancels != 0 || result.Activation != "pending" {
						t.Fatal("unpaid result treated as success", result.Payment, result.ErrorCode)
					}
					return
				}
				if mode == "unknown" || mode == "action_required" {
					if cancels != 0 {
						t.Fatal("renewal changed before charge confirmation")
					}
					if err = result.Checkpoint.ValidateForRecovery(); err != nil {
						t.Fatal("invalid hosted checkpoint", err)
					}
					recovering = true
					result, err = flow.client.Recover(context.Background(), result.Checkpoint)
				}
				if err != nil || result.Payment != "charged" || result.Activation != "active" || result.Renewal != "off" || cancels != 1 || creates != wantCreates || cards != 1 || confirms != 1 {
					t.Fatal("hosted recovery or subscription lifecycle failed", err, result.ErrorCode, result.WarningCode)
				}
			}
			for _, cp := range *saved {
				raw, _ := json.Marshal(cp)
				if strings.Contains(string(raw), "4242424242424242") || strings.Contains(string(raw), `"cvc"`) {
					t.Fatal("card persisted")
				}
			}
		})
	}
}
