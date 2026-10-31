package cslive

import (
	"context"
	"encoding/json"
	"strings"
	"testing"
)

// Entire public Pay + Recover entry points, with no network and no real card.
func TestPayAndRecoverNeverCreateASecondPayment(t *testing.T) {
	for _, returnedMode := range []string{"custom", "hosted", "redirect", "", "unrecognized"} {
		for _, mode := range []string{"success", "unknown", "action_required", "declined", "authorized"} {
			t.Run(returnedMode+"/"+mode, func(t *testing.T) {
				flow, tr, saved := flowFixture(t, true)
				creates, confirms, cancels, refreshes, approvals := 0, 0, 0, 0, 0
				wantCreates, wantRefreshes := 1, 2
				recovering := false
				tr.request = func(method, path string, opts protocol_RequestOptions) (*protocol_HTTPResponse, error) {
					switch path {
					case "/":
						r := response(200, nil)
						r.Content = []byte(`<html data-build="build" data-seq="42"></html>`)
						r.Headers.Add("Set-Cookie", "oai-did=11111111-1111-4111-8111-111111111111; Path=/; Secure")
						return r, nil
					case "/api/auth/session":
						refreshes++
						return response(200, protocol_Map{"accessToken": "new-at", "account": protocol_Map{"id": "account"}}), nil
					case "/backend-api/accounts/check/v4-2023-04-27":
						return response(200, protocol_Map{"accounts": protocol_Map{"account": protocol_Map{"account": protocol_Map{"id": "account", "account_type": "personal", "plan_type": "free"}, "entitlement": protocol_Map{"has_active_subscription": false}}}}), nil
					case "/backend-api/subscriptions":
						if confirms == 0 {
							return response(404, protocol_Map{"detail": "No subscription found for account"}), nil
						}
						return response(200, protocol_Map{"plan_type": "premium", "will_renew": cancels == 0, "active_until": "2099-01-01T00:00:00Z"}), nil
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
						payload, _ := protocol_CompactJSON(opts.Body)
						if !strings.Contains(payload, `"checkout_ui_mode":"custom"`) {
							t.Fatal("short checkout did not request custom mode")
						}
						if recovering {
							t.Fatal("recovery created checkout")
						}
						return response(200, protocol_Map{"checkout_session_id": "oaics_Fixture", "checkout_ui_mode": returnedMode, "url": "https://pay.openai.com/c/pay/oaics_Fixture#ignored", "publishable_key": "pk_test_Fixture"}), nil
					case "/backend-api/payments/checkout/taxes":
						return response(200, protocol_Map{"checkout_session": protocol_Map{"amount_total": 1500, "currency": "php"}}), nil
					case "/v1/confirmation_tokens":
						if approvals != 1 || recovering {
							t.Fatal("card tokenization outside single approved flow")
						}
						return response(200, protocol_Map{"id": "ctoken_Fixture"}), nil
					case "/backend-api/payments/checkout/confirm":
						if recovering {
							t.Fatal("recovery repeated platform confirmation")
						}
						return response(200, protocol_Map{"client_secret": "pi_Fixture_secret_Fixture", "confirm_return_url": "https://chatgpt.com/checkout/verify?stripe_session_id=oaics_Fixture&processor_entity=openai_llc&plan_type=pro"}), nil
					case "/v1/payment_intents/pi_Fixture":
						if recovering {
							return response(200, protocol_Map{"id": "pi_Fixture", "status": "succeeded", "amount_received": 1500, "currency": "php"}), nil
						}
						return response(200, protocol_Map{"id": "pi_Fixture", "status": "requires_confirmation", "confirmation_method": "automatic", "last_payment_error": nil, "payment_method": nil}), nil
					case "/v1/payment_intents/pi_Fixture/confirm":
						confirms++
						if recovering || confirms != 1 {
							t.Fatal("payment confirmation repeated")
						}
						if (*saved)[len(*saved)-1].Phase != "stripe_confirm" || (*saved)[len(*saved)-1].Payment != "unknown" {
							t.Fatal("payment before durable boundary")
						}
						if mode == "unknown" {
							return nil, Failure("network_error")
						}
						status := map[string]string{"success": "succeeded", "action_required": "requires_action", "declined": "requires_payment_method", "authorized": "requires_capture"}[mode]
						return response(200, protocol_Map{"id": "pi_Fixture", "status": status, "amount_received": 1500, "currency": "php", "last_payment_error": protocol_Map{"code": "card_declined"}}), nil
					case "/checkout/verify":
						return response(200, protocol_Map{}), nil
					case "/backend-api/subscriptions/cancel":
						cancels++
						return response(200, protocol_Map{}), nil
					default:
						t.Fatalf("unexpected request %s %s", method, path)
						return nil, nil
					}
				}
				result, err := flow.client.Pay(context.Background(), model_Session{AccessToken: "old-at", SessionToken: "session", AccountID: "account", Email: "fixture@example.com"}, model_Card{Number: "4242424242424242", CVC: "123", ExpMonth: "12", ExpYear: "2035"}, gptpay_PayOptions{Country: "PH", Plan: "pro20x", CheckoutType: "oaics", Billing: model_Billing{Name: "QA Fixture", Email: "fixture@example.com", Country: "US", Line1: "100 Test Street", City: "New York", State: "NY", PostalCode: "10001"}, ApprovePayment: func(amount int64, currency string) (bool, error) {
					approvals++
					if amount != 1500 || currency != "PHP" {
						t.Fatal("incorrect tax quote")
					}
					return true, nil
				}})
				if err != nil {
					t.Fatal(err)
				}
				if result.Checkpoint.Metadata["checkout_ui_mode"] != "custom" || result.Checkpoint.Metadata["hosted_url"] != nil {
					t.Fatal("upstream mode changed the selected short flow")
				}
				if creates != wantCreates || confirms != 1 || approvals != 1 {
					t.Fatalf("unexpected counts: creates=%d confirms=%d approvals=%d code=%s", creates, confirms, approvals, result.ErrorCode)
				}
				want := map[string]string{"success": "charged", "unknown": "unknown", "action_required": "action_required", "declined": "not_charged", "authorized": "authorized"}[mode]
				if string(result.Payment) != want {
					t.Fatalf("payment=%s want=%s code=%s warning=%s", result.Payment, want, result.ErrorCode, result.WarningCode)
				}
				if mode == "success" {
					if result.Activation != "active" || result.Renewal != "off" || cancels != 1 || refreshes != wantRefreshes {
						t.Fatal("incomplete subscription lifecycle", result.WarningCode, cancels, refreshes)
					}
				} else if cancels != 0 {
					t.Fatal("cancellation before charge")
				}
				for _, cp := range *saved {
					raw, _ := json.Marshal(cp)
					if strings.Contains(string(raw), "4242424242424242") || strings.Contains(string(raw), `"cvc"`) {
						t.Fatal("card persisted in checkpoint")
					}
				}
				if mode == "unknown" || mode == "action_required" {
					recovering = true
					recovered, e := flow.client.Recover(context.Background(), result.Checkpoint)
					if e != nil || recovered.Payment != "charged" || recovered.Renewal != "off" || confirms != 1 || creates != wantCreates {
						t.Fatal("recovery did not verify original payment", e, recovered.ErrorCode, recovered.WarningCode)
					}
				}
			})
		}
	}
}
