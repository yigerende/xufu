package platform

import (
	"context"
	"net/http"
	"reflect"
	"testing"
)

// Complete HAR structure, fictional identities and amounts. No external traffic
// or captured credentials, browser identity or verification tokens are used.
func TestLegacyCompleteHostedPaymentAndRecovery(t *testing.T) {
	for _, scenario := range []string{"immediate", "processing-poll", "challenge", "foreign-poll", "poll-not-paid", "lost-confirm"} {
		t.Run(scenario, func(t *testing.T) {
			in := legacyTestInput()
			var cp legacyPaymentCheckpoint
			var calls []string
			var confirms, methods, ipChecks int
			in.CheckExitIP = func(context.Context) error { ipChecks++; return nil }
			in.SaveCheckpoint = func(_ context.Context, value legacyPaymentCheckpoint) error { cp = value; return nil }
			page := func() M {
				v := legacyTestPage("cs_test_Offline", 1080, false)
				v["config_id"], v["eid"] = "config_Offline", "eid_Offline"
				return v
			}
			recovering := false
			client := &http.Client{Transport: legacyRoundTrip(func(r *http.Request) (*http.Response, error) {
				calls = append(calls, r.Method+" "+r.URL.Path)
				if recovering && r.Method != "GET" {
					t.Fatal("recovery attempted a write")
				}
				_ = r.ParseForm()
				switch r.URL.Path {
				case "/v1/payment_pages/cs_test_Offline/init":
					return legacyReply(200, page()), nil
				case "/v1/payment_pages/cs_test_Offline":
					if r.Method != "POST" {
						t.Fatal("complete page body was ignored")
					}
					if r.Form.Get("tax_region[postal_code]") != in.Billing["postal_code"] {
						t.Fatal("billing snapshot changed")
					}
					return legacyReply(200, page()), nil
				case "/v1/payment_methods":
					methods++
					assertLegacyHostedCapturedFields(t, "payment_method", r.Form)
					return legacyReply(200, M{"id": "pm_Offline"}), nil
				case "/v1/payment_pages/cs_test_Offline/confirm":
					confirms++
					assertLegacyHostedCapturedFields(t, "confirmation", r.Form)
					if cp.PageID != "ppage_Test" || cp.Key == "" {
						t.Fatal("missing durable original checkout")
					}
					if scenario == "lost-confirm" {
						return nil, context.DeadlineExceeded
					}
					if scenario == "immediate" {
						return legacyReply(200, legacyTestPage("cs_test_Offline", 1080, true)), nil
					}
					v := page()
					status := "processing"
					if scenario != "processing-poll" {
						status = "requires_action"
					}
					v["payment_intent"] = M{"id": "pi_Offline", "object": "payment_intent", "amount": 1080, "currency": "php", "status": status, "client_secret": "pi_Offline_secret_Offline"}
					return legacyReply(200, v), nil
				case "/v1/payment_pages/cs_test_Offline/poll":
					v := M{"id": "ppage_Test", "session_id": "cs_test_Offline", "state": "succeeded", "payment_object_status": "succeeded"}
					if scenario == "foreign-poll" {
						v["session_id"] = "cs_test_Foreign"
					}
					if scenario == "poll-not-paid" {
						v["payment_object_status"] = "processing"
					}
					return legacyReply(200, v), nil
				case "/v1/payment_intents/pi_Offline":
					return legacyReply(200, M{"id": "pi_Offline", "object": "payment_intent", "amount": 1080, "currency": "php", "status": "requires_action"}), nil
				default:
					t.Fatal("unrecognized request", r.Method, r.URL.Path)
					return nil, context.Canceled
				}
			})}
			e := legacyCollect(t, startLegacyPaymentClient(context.Background(), in, client, func() {}), nil)
			want := "action_required"
			if scenario == "immediate" || scenario == "processing-poll" || scenario == "lost-confirm" {
				want = "succeeded"
			}
			if e.State != want || methods != 1 || confirms != 1 || ipChecks != 2 {
				t.Fatalf("result %s, methods %d, confirms %d, IP checks %d", e.State, methods, confirms, ipChecks)
			}
			prefix := []string{"POST /v1/payment_pages/cs_test_Offline/init", "POST /v1/payment_pages/cs_test_Offline", "POST /v1/payment_pages/cs_test_Offline", "POST /v1/payment_methods", "POST /v1/payment_pages/cs_test_Offline/confirm"}
			if !reflect.DeepEqual(calls[:5], prefix) {
				t.Fatal("wrong request order", calls)
			}
			if want == "succeeded" {
				return
			}
			recovering = true
			in.Mode, in.Checkpoint, in.Card = "check", cp, cardInput{}
			in.SavedQuote = M{"amount": 1080, "currency": "php"}
			e = legacyCollect(t, startLegacyPaymentClient(context.Background(), in, client, func() {}), nil)
			want = "succeeded"
			if scenario == "foreign-poll" {
				want = "review"
			}
			if scenario == "poll-not-paid" {
				want = "action_required"
			}
			if e.State != want || methods != 1 || confirms != 1 || ipChecks != 2 {
				t.Fatal("recovery changed original transaction", e.State)
			}
			if scenario == "challenge" && (e.Evidence == nil || e.Evidence.AmountReceived != nil) {
				t.Fatal("poll fabricated a captured amount")
			}
		})
	}
}

func TestLegacyPollIdentityAndEvidence(t *testing.T) {
	cp := legacyPaymentCheckpoint{CheckoutID: "cs_test_Offline", PageID: "ppage_Offline"}
	q := &legacyPaymentSnapshot{Amount: 1080, Currency: "php"}
	v := M{"id": cp.PageID, "session_id": cp.CheckoutID, "state": "succeeded", "payment_object_status": "succeeded"}
	if _, err := legacyHostedPollSnapshot(v, cp, nil); err == nil {
		t.Fatal("accepted success without quote")
	}
	for _, field := range []string{"id", "session_id"} {
		foreign := M{}
		for k, x := range v {
			foreign[k] = x
		}
		foreign[field] = "other"
		if _, err := legacyHostedPollSnapshot(foreign, cp, q); err == nil {
			t.Fatal("foreign success accepted")
		}
	}
	delete(v, "payment_object_status")
	if s, err := legacyHostedPollSnapshot(v, cp, q); err != nil || s != nil {
		t.Fatal("incomplete success accepted")
	}
}
