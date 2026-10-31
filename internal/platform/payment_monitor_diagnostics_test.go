package platform

import (
	"strings"
	"testing"
	"time"
	"xupay/internal/cslive"
)

func TestProtocolCustomCheckoutLinkAndLegacyFallback(t *testing.T) {
	h, r, cp := nativeFixture(t, false)
	cp.CheckoutID = "cs_live_CustomFixture"
	cp.Entity = "openai_llc"
	cp.Metadata["checkout_ui_mode"] = "custom"
	delete(cp.Metadata, "hosted_url")
	if _, err := h.a.DB.Exec(r.ctx, "DELETE FROM order_checkout_links WHERE order_id=$1", r.id); err != nil {
		t.Fatal(err)
	}
	if err := r.save(cp); err != nil {
		t.Fatal(err)
	}
	want := "https://chatgpt.com/checkout/openai_llc/cs_live_CustomFixture"
	for _, legacy := range []bool{false, true} {
		if legacy {
			h.a.DB.Exec(r.ctx, "DELETE FROM order_checkout_links WHERE order_id=$1", r.id)
		}
		links, err := h.a.adminCheckoutLinks(r.ctx, r.id)
		if err != nil || len(links) != 1 || links[0]["url"] != want {
			t.Fatalf("legacy=%v links=%v err=%v", legacy, links, err)
		}
		if legacy && links[0]["generated_at"] != nil {
			t.Fatal("invented generation time")
		}
		for _, path := range []string{"/api/orders/", "/api/v1/orders/"} {
			res := h.request("GET", path+r.id, nil, h.member, nil)
			if strings.Contains(res.Body.String(), want) || strings.Contains(res.Body.String(), "checkout_links") {
				t.Fatal("customer URL leak")
			}
		}
	}
	cp.Entity = "../../private"
	if protocolCheckoutURL(cp) != "" {
		t.Fatal("unsafe entity")
	}
	r.finish(cslive.Result{Checkpoint: cp})
}

func TestProtocolMonitorDistinguishesNormalAbsentAndVerificationRequired(t *testing.T) {
	h, r, cp := nativeFixture(t, false)
	if h.a.paymentDetails(r.ctx, r.id)["automatic_running"] != true {
		t.Fatal("active worker not marked automatic")
	}
	h.a.paymentActions = map[string]*paymentAction{r.id: {state: "verifying", expires: time.Now().Add(time.Minute)}}
	if h.a.paymentDetails(r.ctx, r.id)["automatic_running"] != false {
		t.Fatal("manual fallback hidden during active verification")
	}
	h.a.paymentActions = nil
	for _, code := range []string{"subscription_not_found", "http_404"} {
		r.event(map[string]any{"event": "http_request", "path": "/backend-api/subscriptions", "status": 404, "errorCode": code})
		d := <-h.a.providerDiagnostics
		h.a.saveProviderDiagnostic(d)
	}
	r.event(map[string]any{"event": "payment_diagnostic", "paymentState": "action_required"})
	h.a.saveProviderDiagnostic(<-h.a.providerDiagnostics)
	rows := h.a.paymentTraceDetails(r.ctx, r.id)
	var absent, failed, verification bool
	for _, row := range rows {
		if row["code"] == "subscription_absent" {
			absent = row["state"] == "completed"
		}
		if row["code"] == "invalid_response" {
			failed = row["state"] == "failed"
		}
		if row["code"] == "verification_required" {
			verification = row["state"] == "review"
		}
	}
	if !absent || !failed || !verification {
		t.Fatal("incorrect monitoring states")
	}
	secret := "pi_private_secret_value"
	fields := safeDiagnosticDetails(M{"sdk_stage": secret, "sdk_issue": secret, "sdk_resource": secret, "sdk_event": "progress", "sdk_wait_ms": int64(30000)})
	if len(fields) != 2 {
		t.Fatal("unsafe SDK diagnostics", fields)
	}
	r.finish(cslive.Result{Checkpoint: cp})
}

func TestHostedPendingDiagnosticsAndReadPhases(t *testing.T) {
	h, r, cp := nativeFixture(t, false)
	r.last.Phase = "hosted_confirm"
	for _, path := range []string{"/v1/payment_pages/{id}", "/v1/payment_pages/{id}?key=PRIVATE_KEY"} {
		r.event(map[string]any{"event": "http_request", "path": path, "status": 200})
		d := <-h.a.providerDiagnostics
		if d.Phase != "checking" {
			t.Fatal("payment status read mislabeled as confirmation", d.Phase)
		}
	}
	for path, want := range map[string]string{"/v1/payment_pages/{id}/init": "pricing", "/v1/payment_pages/{id}/confirm": "confirming", "/backend-api/payments/checkout/approve": "confirming"} {
		if got := protocolDiagnosticPhase(path, "hosted_confirm"); got != want {
			t.Fatal("write or initialization misclassified", path, got)
		}
	}
	for _, d := range []providerDiagnostic{
		{Code: "pending"},
		{Code: "payment_pending"},
		{Details: M{"payment_state": "pending", "raw_state": "active"}},
	} {
		d.Order, d.Phase, d.At = r.id, "checking", time.Now()
		h.a.saveProviderDiagnostic(d)
	}
	rows := h.a.paymentTraceDetails(r.ctx, r.id)
	found := 0
	for _, row := range rows {
		if row["phase"] == "checking" {
			found++
			if row["state"] != "review" {
				t.Fatal("pending payment reported as complete or failed", row["state"], row["code"])
			}
		}
	}
	if found != 3 || len(safeDiagnosticDetails(M{"raw_state": "PRIVATE_SECRET"})) != 1 || safeDiagnosticDetails(M{"raw_state": "PRIVATE_SECRET"})["raw_state"] != "redacted" {
		t.Fatal("missing diagnostics or lost privacy boundary")
	}
	var storedReviews int
	if err := h.a.DB.QueryRow(r.ctx, "SELECT count(*) FROM payment_trace_events WHERE order_id=$1 AND phase='checking' AND state='review'", r.id).Scan(&storedReviews); err != nil || storedReviews != 3 {
		t.Fatal("new pending observations stored with wrong state", err, storedReviews)
	}
	if _, err := h.a.DB.Exec(r.ctx, "UPDATE payment_trace_events SET state='failed' WHERE order_id=$1 AND code='pending'", r.id); err != nil {
		t.Fatal(err)
	}
	for _, row := range h.a.paymentTraceDetails(r.ctx, r.id) {
		if row["code"] == "pending" && row["state"] != "review" {
			t.Fatal("historical pending observation still shown as failed")
		}
	}
	h.a.saveProviderDiagnostic(providerDiagnostic{Order: r.id, Phase: "checking", Code: "access_denied", Status: 403, At: time.Now(), Details: M{"payment_state": "pending"}})
	for _, row := range h.a.paymentTraceDetails(r.ctx, r.id) {
		if row["code"] == "access_denied" && row["state"] != "failed" {
			t.Fatal("pending metadata hid an HTTP failure")
		}
	}
	r.finish(cslive.Result{Checkpoint: cp})
}

func TestActionNetworkDiagnosticPrivacyAndPersistence(t *testing.T) {
	h, r, cp := nativeFixture(t, false)
	fields := M{"sdk_event": "diagnostic", "sdk_issue": "resource_failed", "sdk_host": "api.stripe.com", "sdk_route": "payment_intent", "sdk_resource_key": "0123456789abcdef", "sdk_request": int64(7), "sdk_method": "GET", "sdk_request_type": "fetch", "sdk_network_error": "ERR_TUNNEL_CONNECTION_FAILED", "sdk_duration_ms": int64(15000), "sdk_guard_ms": int64(1234), "sdk_pending": int64(1), "url": "https://private.example/SECRET", "body": "SECRET"}
	h.a.saveProviderDiagnostic(providerDiagnostic{Order: r.id, Phase: "verification", At: time.Now(), Details: fields})
	admin := expect(t, h.request("GET", "/api/admin/orders/"+r.id, nil, h.admin, nil), 200)
	trace := admin["trace"].([]any)
	var found bool
	for _, item := range trace {
		row := item.(M)
		d := row["diagnostics"].(M)
		if d["sdk_network_error"] == "ERR_TUNNEL_CONNECTION_FAILED" {
			found = true
			if d["sdk_host"] != "api.stripe.com" || d["sdk_guard_ms"] != float64(1234) || d["url"] != nil || d["body"] != nil {
				t.Fatal("unsafe or missing diagnostic")
			}
		}
	}
	if !found {
		t.Fatal("network diagnostic missing")
	}
	for _, path := range []string{"/api/orders/", "/api/v1/orders/"} {
		response := h.request("GET", path+r.id, nil, h.member, nil)
		expect(t, response, 200)
		if strings.Contains(response.Body.String(), "sdk_host") || strings.Contains(response.Body.String(), "ERR_TUNNEL") {
			t.Fatal("customer network diagnostic leak")
		}
	}
	for _, key := range []string{"sdk_host", "sdk_route", "sdk_resource_key", "sdk_network_error", "sdk_method", "sdk_request_type"} {
		if len(safeDiagnosticDetails(M{key: "pi_private_secret_token"})) != 0 {
			t.Fatal("arbitrary diagnostic accepted", key)
		}
	}
	r.finish(cslive.Result{Checkpoint: cp})
}

func TestMerchantPageDiagnosticPrivacyAndPersistence(t *testing.T) {
	h, r, cp := nativeFixture(t, false)
	fields := M{"page_event": "request_failed", "page_host": "cdn.oaistatic.com", "page_route": "other", "page_resource": "script", "page_network_error": "ERR_CONNECTION_RESET", "page_auth": "unknown", "url": "https://private.example/SECRET", "cookie": "SECRET"}
	h.a.saveProviderDiagnostic(providerDiagnostic{Order: r.id, Phase: "checking", At: time.Now(), Details: fields})
	admin := expect(t, h.request("GET", "/api/admin/orders/"+r.id, nil, h.admin, nil), 200)
	var found bool
	for _, item := range admin["trace"].([]any) {
		d := item.(M)["diagnostics"].(M)
		if d["page_event"] == "request_failed" {
			found = true
			if d["page_host"] != "cdn.oaistatic.com" || d["page_network_error"] != "ERR_CONNECTION_RESET" || d["url"] != nil || d["cookie"] != nil {
				t.Fatal("unsafe or missing merchant diagnostic", d)
			}
		}
	}
	if !found {
		t.Fatal("merchant diagnostic missing")
	}
	for _, path := range []string{"/api/orders/", "/api/v1/orders/"} {
		response := h.request("GET", path+r.id, nil, h.member, nil)
		expect(t, response, 200)
		if strings.Contains(response.Body.String(), "page_host") || strings.Contains(response.Body.String(), "ERR_CONNECTION") {
			t.Fatal("customer merchant diagnostic leak")
		}
	}
	for _, key := range []string{"page_event", "page_host", "page_route", "page_resource", "page_auth", "page_network_error"} {
		for _, invalid := range []any{"PRIVATE_SECRET", "", "other other", "other\n", true, []any{"other"}} {
			if len(safeDiagnosticDetails(M{key: invalid})) != 0 {
				t.Fatal("arbitrary merchant diagnostic accepted", key)
			}
		}
	}
	r.finish(cslive.Result{Checkpoint: cp})
}
