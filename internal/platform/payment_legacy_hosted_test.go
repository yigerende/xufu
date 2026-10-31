package platform

import (
	"context"
	"encoding/json"
	"net/http"
	"net/url"
	"os"
	"reflect"
	"sort"
	"strings"
	"testing"
)

// Replay the request structure with fictional values and an in-memory server.
// The server rejects missing/stale attribution rather than accepting every POST.
func TestLegacyHostedCaptureContract(t *testing.T) {
	for _, scenario := range []string{"success", "foreign-init", "foreign-tax", "foreign-read", "invalid-config", "method-rejected"} {
		t.Run(scenario, func(t *testing.T) {
			in := legacyTestInput()
			in.URL = strings.Replace(in.URL, "checkout.stripe.com", "pay.openai.com", 1)
			const initial = "11111111-1111-4111-8111-111111111111"
			const taxed = "22222222-2222-4222-8222-222222222222"
			const latest = "33333333-3333-4333-8333-333333333333"
			config, eid := initial, "eid_initial"
			methods, confirms := 0, 0
			approved := false
			var methodMetadata url.Values
			page := func() M {
				m := legacyTestPage("cs_test_Offline", 1080, false)
				m["config_id"], m["eid"] = config, eid
				return m
			}
			client := &http.Client{Transport: legacyRoundTrip(func(r *http.Request) (*http.Response, error) {
				if err := r.ParseForm(); err != nil {
					t.Fatal(err)
				}
				if r.Header.Get("Stripe-Version") != "" || r.Header.Get("Origin") != "https://pay.openai.com" || r.Header.Get("Referer") != "https://pay.openai.com/" {
					t.Error("hosted headers differ from captured contract")
				}
				switch r.Method + " " + r.URL.Path {
				case "POST /v1/payment_pages/cs_test_Offline/init":
					if len(r.Form) != 5 || r.Form.Get("browser_locale") != "zh-CN" || r.Form.Get("browser_timezone") != "Asia/Shanghai" || r.Form.Get("redirect_type") != "url" {
						t.Error("initialization differs from the captured locale/timezone contract")
					}
					m := page()
					if scenario == "foreign-init" {
						m["session_id"] = "cs_test_Other"
					}
					return legacyReply(200, m), nil
				case "POST /v1/payment_pages/cs_test_Offline":
					config, eid = taxed, "eid_taxed"
					if approved {
						config, eid = latest, "eid_latest"
					}
					m := page()
					if approved && scenario == "foreign-read" {
						m["session_id"] = "cs_test_Other"
					}
					if approved && scenario == "invalid-config" {
						m["config_id"] = M{"unexpected": "PRIVATE"}
					}
					if scenario == "foreign-tax" {
						m["session_id"] = "cs_test_Other"
					}
					return legacyReply(200, m), nil
				case "GET /v1/payment_pages/cs_test_Offline":
					m := page()
					if scenario == "foreign-read" {
						m["session_id"] = "cs_test_Other"
					}
					if scenario == "invalid-config" {
						m["config_id"] = M{"unexpected": "PRIVATE"}
					}
					return legacyReply(200, m), nil
				case "POST /v1/payment_methods", "POST /v1/payment_pages/cs_test_Offline/confirm":
					role := "confirmation"
					if r.URL.Path == "/v1/payment_methods" {
						methods++
						role = "payment_method"
					} else {
						confirms++
					}
					assertLegacyHostedCapturedFields(t, role, r.Form)
					metadata := url.Values{}
					for k, v := range r.Form {
						if strings.HasPrefix(k, "client_attribution_metadata[") {
							metadata[k] = v
						}
					}
					want := url.Values{
						"client_attribution_metadata[checkout_session_id]":           {"cs_test_Offline"},
						"client_attribution_metadata[checkout_config_id]":            {latest},
						"client_attribution_metadata[merchant_integration_source]":   {"checkout"},
						"client_attribution_metadata[merchant_integration_version]":  {"hosted_checkout"},
						"client_attribution_metadata[payment_method_selection_flow]": {"automatic"},
					}
					if !reflect.DeepEqual(metadata, want) {
						t.Error("missing or stale attribution from original checkout")
						return legacyReply(400, M{"error": M{"type": "invalid_request_error"}}), nil
					}
					if r.Form.Get("_stripe_version") != "2020-08-27;custom_checkout_beta=v1" {
						t.Error("missing captured form version")
					}
					if r.URL.Path == "/v1/payment_methods" {
						methodMetadata = metadata
						if scenario == "method-rejected" {
							return legacyReply(400, M{"error": M{"type": "invalid_request_error"}}), nil
						}
						return legacyReply(200, M{"id": "pm_Offline"}), nil
					}
					if !reflect.DeepEqual(metadata, methodMetadata) || r.Form.Get("eid") != "eid_latest" || r.Form.Get("link_brand") != "link" || r.Form.Get("payment_method") != "pm_Offline" {
						t.Error("confirmation did not preserve current payment context")
					}
					if r.Header.Get("Idempotency-Key") == "" || r.GetBody != nil {
						t.Error("payment replay protection lost")
					}
					return legacyReply(200, legacyTestPage("cs_test_Offline", 1080, true)), nil
				default:
					t.Error("unexpected request")
					return legacyReply(500, M{}), nil
				}
			})}
			p := startLegacyPaymentClient(context.Background(), in, client, func() {})
			e := legacyCollect(t, p, func(paymentEvent) { approved = true; config, eid = latest, "eid_latest" })
			if scenario == "success" {
				if e.State != "succeeded" || methods != 1 || confirms != 1 {
					t.Fatal("offline contract did not complete once", e.State, methods, confirms)
				}
			} else if e.State != "failed" || e.Sent || confirms != 0 || (scenario != "method-rejected" && methods != 0) || (scenario == "method-rejected" && methods != 1) {
				t.Fatal("invalid context/rejection caused a payment or retry", e.State, methods, confirms)
			}
		})
	}
}

func assertLegacyHostedCapturedFields(t *testing.T, role string, form url.Values) {
	t.Helper()
	for _, filename := range []string{"testdata/legacy_hosted_capture_fields.json", "testdata/legacy_complete_capture_fields.json"} {
		assertLegacyHostedCaptureFile(t, filename, role, form)
	}
}

func assertLegacyHostedCaptureFile(t *testing.T, filename, role string, form url.Values) {
	t.Helper()
	raw, err := os.ReadFile(filename)
	if err != nil {
		t.Fatal(err)
	}
	var captured struct {
		Method  []string `json:"payment_method"`
		Confirm []string `json:"confirmation"`
	}
	if err = json.Unmarshal(raw, &captured); err != nil {
		t.Fatal(err)
	}
	fields := captured.Method
	pending := []string{"client_attribution_metadata[client_session_id]", "guid", "muid", "sid"}
	if role == "payment_method" {
		pending = append(pending, "payment_user_agent")
	} else {
		fields = captured.Confirm
		pending = append(pending, "rv_timestamp", "passive_captcha_ekey", "passive_captcha_token")
	}
	// Keep the unsupported SDK context explicit. Passing this test establishes
	// the ordinary-field contract, not full browser equivalence or acceptance.
	remaining := map[string]bool{}
	for _, field := range pending {
		remaining[field] = true
		if _, present := form[field]; present {
			t.Fatalf("SDK field has no genuine source: %s", field)
		}
	}
	want, got := []string{}, []string{}
	for _, field := range fields {
		if !remaining[field] {
			want = append(want, field)
		}
	}
	for field := range form {
		got = append(got, field)
	}
	sort.Strings(want)
	sort.Strings(got)
	if !reflect.DeepEqual(want, got) {
		t.Errorf("%s field-name contract differs: want %v, got %v", role, want, got)
	}
}

func TestLegacyHostedContextScope(t *testing.T) {
	first := &legacyPayment{cp: legacyPaymentCheckpoint{CheckoutID: "cs_test_First"}, in: legacyTestInput()}
	second := &legacyPayment{cp: legacyPaymentCheckpoint{CheckoutID: "cs_test_Second"}, in: legacyTestInput()}
	if err := first.observeHostedPage(M{"session_id": "cs_test_First", "config_id": "config_First"}); err != nil {
		t.Fatal(err)
	}
	f := url.Values{}
	second.addHostedAttribution(f)
	if f.Get("client_attribution_metadata[checkout_config_id]") != "" || f.Get("client_attribution_metadata[checkout_session_id]") != "cs_test_Second" {
		t.Fatal("context leaked between orders")
	}
	for _, bad := range []M{
		{"config_id": "config_Unbound"},
		{"session_id": "cs_test_Other", "config_id": "config_Other"},
		{"session_id": M{"invalid": true}},
		{"session_id": "cs_test_First", "config_id": ""},
	} {
		if err := first.observeHostedPage(bad); err == nil || first.hostedConfigID != "config_First" {
			t.Fatal("invalid context accepted or overwrote the original context")
		}
	}
	custom := &legacyPayment{custom: true, in: legacyTestInput(), hostedConfigID: "config_Ignored"}
	form := custom.cardForm("payment_method_data")
	before := form.Encode()
	custom.addHostedAttribution(form)
	if before != form.Encode() || custom.hostedOrigin() != "" {
		t.Fatal("hosted contract leaked into custom checkout")
	}
	if err := custom.observeHostedPage(M{"config_id": M{"unrelated": true}}); err != nil {
		t.Fatal("custom checkout interpreted hosted-only context")
	}
}

func TestLegacyCaptureParameterDiagnosticNames(t *testing.T) {
	for _, field := range []string{"checkout_session_id", "checkout_config_id", "client_session_id", "merchant_integration_source", "merchant_integration_version", "payment_method_selection_flow"} {
		param := "client_attribution_metadata[" + field + "]"
		if safeDiagnosticDetails(M{"provider_param": param})["provider_param"] != param {
			t.Errorf("diagnostic lost parameter name: %s", param)
		}
		if len(safeDiagnosticDetails(M{"provider_param": param + "=PRIVATE"})) != 0 {
			t.Fatal("diagnostic exposed parameter value")
		}
	}
}
