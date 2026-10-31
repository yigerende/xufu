package platform

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"xupay/internal/cslive"
)

func nativeFixture(t *testing.T, keep bool) (*harness, *protocolRun, cslive.Checkpoint) {
	return protocolFixture(t, keep, false)
}

func protocolFixture(t *testing.T, keep, worker bool) (*harness, *protocolRun, cslive.Checkpoint) {
	t.Helper()
	h := setupTest(t)
	mockOrderIP(t, h.a, "203.0.113.2")
	ctx := context.Background()
	h.saveTestBilling("subscription@example.com")
	h.addProxy("PH", 2)
	if _, err := h.a.DB.Exec(ctx, "UPDATE users SET balance=100000 WHERE id=$1", h.memberID); err != nil {
		t.Fatal(err)
	}
	body := prepareRequest()
	body["workflow"], body["card"], body["keep_renewal"] = "subscription", paymentPrepareBody()["card"], keep
	headers := map[string]string{"Idempotency-Key": uid()}
	id := str(expect(t, h.request("POST", "/api/orders", body, h.member, headers), 200), "id")
	replay := expect(t, h.request("POST", "/api/orders", body, h.member, headers), 200)
	if replay["id"] != id || replay["replayed"] != true {
		t.Fatal("duplicate admission")
	}
	h.a.paymentMu.Lock()
	s := h.a.payments[id]
	if s == nil || s.protocol == nil {
		t.Fatal("new subscription did not use native runtime")
	}
	attempt := s.attempt
	if !worker {
		close(s.protocol.done)
	}
	h.a.paymentMu.Unlock()
	j, err := h.a.Claim(ctx)
	if err != nil || j == nil || j.OrderID != id {
		t.Fatal("claim failed", err)
	}
	r := &protocolRun{a: h.a, ctx: ctx, id: id, attempt: attempt, lease: j.Lease, job: j, account: "test-space", email: "subscription@example.com", country: "PH", keep: keep, maxAmount: 2000, currency: "php"}
	if _, err = h.a.DB.Exec(ctx, "UPDATE subscription_runs SET max_amount=2000 WHERE order_id=$1", id); err != nil {
		t.Fatal(err)
	}
	if !worker {
		if err = r.claim(180); err != nil {
			t.Fatal(err)
		}
	}
	cp := cslive.DefaultCheckpoint()
	cp.AccountID, cp.Email, cp.AccessToken, cp.SessionToken = r.account, r.email, "TEST_PRIVATE_AT", "TEST_PRIVATE_SESSION"
	cp.Country, cp.Currency, cp.Timezone, cp.Plan = "PH", "PHP", "Asia/Manila", "pro20x"
	cp.Proxy = "http://local:PRIVATE_BRIDGE_PASSWORD@127.0.0.1:1"
	cp.CheckoutID, cp.PaymentIntentID, cp.PublishableKey = "cs_live_TestCheckout", "pi_TestIntent", "pk_test_TestKey"
	cp.Metadata["hosted_url"] = "https://pay.openai.com/c/pay/cs_live_TestCheckout#private-returned-fragment"
	cp.Metadata["checkout_ui_mode"] = "redirect"
	cp.ClientSecret = "pi_TestIntent_secret_TestSecret"
	cp.Phase = "priced"
	amount := int64(1500)
	cp.Amount = &amount
	if !worker {
		if err = r.save(cp); err != nil {
			t.Fatal(err)
		}
	}
	return h, r, cp
}

func nativeCharged(cp cslive.Checkpoint, amount int64) cslive.Checkpoint {
	cp.Payment, cp.Phase = "charged", "payment_result"
	cp.Metadata["charged_price"] = map[string]any{"amount_minor": amount, "currency": "PHP", "source": "payment_intent_received"}
	return cp
}

func TestNativeSettlementAndPostPaymentFailures(t *testing.T) {
	for _, mode := range []string{"cancel", "keep", "activation_pending", "cancel_failed", "zero_proration"} {
		t.Run(mode, func(t *testing.T) {
			h, r, cp := nativeFixture(t, mode == "keep")
			if ok, err := r.approve(1500, "PHP"); err != nil || !ok {
				t.Fatal(err)
			}
			if ok, _ := r.approve(1500, "PHP"); ok {
				t.Fatal("second approval allowed")
			}
			charged := int64(1400)
			if mode == "zero_proration" {
				charged = 0
			}
			cp = nativeCharged(cp, charged)
			if err := r.save(cp); err != nil {
				t.Fatal(err)
			}
			paymentBalances(t, h, 90000, 0)
			waitPayment(t, h, r.id, "succeeded")
			cp.Activation = "active"
			cp.Renewal = "off"
			cp.Phase = "completed"
			if mode == "activation_pending" {
				cp.Activation = "pending"
				cp.Phase = "activation"
			}
			if mode == "cancel_failed" {
				cp.Renewal = "on"
				cp.Phase = "cancellation_requested"
			}
			if err := r.save(cp); err != nil {
				t.Fatal(err)
			}
			r.finish(cslive.Result{Checkpoint: cp, WarningCode: "network_error"})
			r.finish(cslive.Result{Checkpoint: cp}) // Stale finalizer cannot settle twice.
			var status, activation, renewal, checkpoint, credentials, job string
			var got int64
			err := h.a.DB.QueryRow(r.ctx, `SELECT o.status,o.activation,o.renewal,s.checkpoint_enc,u.credentials_enc,j.state,p.charged_amount
 FROM orders o JOIN subscription_protocol s ON s.order_id=o.id JOIN subscription_runs u ON u.order_id=o.id
 JOIN jobs j ON j.order_id=o.id JOIN payment_runs p ON p.order_id=o.id WHERE o.id=$1`, r.id).Scan(&status, &activation, &renewal, &checkpoint, &credentials, &job, &got)
			if err != nil {
				t.Fatal(err)
			}
			if status != "succeeded" || job != "completed" || got != charged || credentials != "" {
				t.Fatalf("incorrect settlement %s %s %d", status, job, got)
			}
			pending := mode == "activation_pending" || mode == "cancel_failed"
			if (checkpoint != "") != pending {
				t.Fatal("incorrect recovery retention")
			}
			if mode == "activation_pending" && activation != "review" {
				t.Fatal("lost activation review")
			}
			if mode == "cancel_failed" && renewal != "review" {
				t.Fatal("lost renewal review")
			}
			if mode == "keep" && renewal != "kept" {
				t.Fatal("keep ignored")
			}
			paymentBalances(t, h, 90000, 0)
			var charges int
			h.a.DB.QueryRow(r.ctx, "SELECT count(*) FROM ledger WHERE order_id=$1 AND kind='charge'", r.id).Scan(&charges)
			if charges != 1 {
				t.Fatal("duplicate settlement", charges)
			}
		})
	}
}

func TestNativeNoSubmitAndUnknownRestart(t *testing.T) {
	for _, mode := range []string{"preparation", "cap", "currency", "unknown", "charged", "expired_missing_checkpoint"} {
		t.Run(mode, func(t *testing.T) {
			h, r, cp := nativeFixture(t, false)
			switch mode {
			case "cap":
				if ok, _ := r.approve(2001, "PHP"); ok {
					t.Fatal("cap ignored")
				}
			case "currency":
				if ok, _ := r.approve(1500, "USD"); ok {
					t.Fatal("currency ignored")
				}
			case "unknown", "charged", "expired_missing_checkpoint":
				if ok, err := r.approve(1500, "PHP"); !ok || err != nil {
					t.Fatal(err)
				}
				cp.Payment, cp.Phase = "unknown", "hosted_confirm"
				if mode == "charged" {
					cp = nativeCharged(cp, 1500)
				}
				if err := r.save(cp); err != nil {
					t.Fatal(err)
				}
				if mode == "expired_missing_checkpoint" {
					h.a.DB.Exec(r.ctx, "UPDATE subscription_protocol SET checkpoint_enc='',expires_at=now()-interval '1 second' WHERE order_id=$1", r.id)
				}
			}
			h.a.DB.Exec(r.ctx, "UPDATE subscription_protocol SET lease_until=now()-interval '1 second' WHERE order_id=$1", r.id)
			if err := h.a.RecoverPayments(r.ctx); err != nil {
				t.Fatal(err)
			}
			if err := h.a.RecoverPayments(r.ctx); err != nil {
				t.Fatal(err)
			}
			want := "failed"
			balance, frozen := int64(100000), int64(0)
			if mode == "unknown" || mode == "expired_missing_checkpoint" {
				want = "review"
				balance, frozen = 90000, 10000
			}
			if mode == "charged" {
				want = "succeeded"
				balance = 90000
			}
			waitPayment(t, h, r.id, want)
			paymentBalances(t, h, balance, frozen)
			if err := r.save(cp); !errors.Is(err, ErrLeaseLost) {
				t.Fatal("stale worker saved after restart", err)
			}
			if j, err := h.a.Claim(r.ctx); err != nil || j != nil {
				t.Fatal("restart requeued payment", err)
			}
		})
	}
}

func TestNativeDiagnosticsAndCustomerBoundary(t *testing.T) {
	h, r, cp := nativeFixture(t, false)
	r.event(map[string]any{"event": "http_request", "path": "/api/auth/session?TOKEN_SECRET", "status": 401, "errorCode": "http_401", "responseRequestId": "req_TEST12345678", "durationMs": 120.0, "connectionReadyMs": 20.0, "requestWriteMs": 2.0, "networkStage": "response_headers", "attempt": int64(2), "responseMessage": "RAW_SECRET 4242424242424242", "providerCode": "card_declined", "declineCode": "RAW_SECRET"})
	select {
	case d := <-h.a.providerDiagnostics:
		if d.Phase != "session_refresh" || d.Elapsed != 120*time.Millisecond || d.Status != 401 {
			t.Fatal("lost diagnostic phase/duration")
		}
		h.a.saveProviderDiagnostic(d)
	default:
		t.Fatal("missing diagnostic")
	}
	key := expect(t, h.request("POST", "/api/keys", M{"name": "native-boundary"}, h.member, nil), 200)
	for _, path := range []string{"/api/orders/", "/api/v1/orders/"} {
		var res *httptest.ResponseRecorder
		if path == "/api/v1/orders/" {
			res = h.request("GET", path+r.id, nil, nil, map[string]string{"Authorization": "Bearer " + str(key, "key")})
		} else {
			res = h.request("GET", path+r.id, nil, h.member, nil)
		}
		expect(t, res, 200)
		for _, secret := range []string{"TEST_PRIVATE", "PRIVATE_BRIDGE", "TOKEN_SECRET", "RAW_SECRET", "4242424242424242", "checkout_links", "payment_trace", "req_TEST", "private-returned-fragment"} {
			if strings.Contains(res.Body.String(), secret) {
				t.Fatal("customer saw private data", secret)
			}
		}
	}
	admin := h.request("GET", "/api/admin/orders/"+r.id, nil, h.admin, nil)
	expect(t, admin, 200)
	for _, secret := range []string{"TEST_PRIVATE", "PRIVATE_BRIDGE", "TOKEN_SECRET", "RAW_SECRET", "4242424242424242", "api/auth/session"} {
		if strings.Contains(admin.Body.String(), secret) {
			t.Fatal("admin diagnostic leaked secret", secret)
		}
	}
	for _, need := range []string{"session_refresh", "req_TEST12345678", "connection_ready_ms", "request_attempt", "private-returned-fragment"} {
		if !strings.Contains(admin.Body.String(), need) {
			t.Fatal("admin missing evidence", need)
		}
	}
	expect(t, h.request("GET", "/api/admin/orders/"+r.id, nil, h.member, nil), 403)
	expect(t, h.request("GET", "/api/orders/"+r.id, nil, h.other, nil), 404)
	expect(t, h.request("POST", "/api/orders/"+r.id+"/payment/check", M{}, h.other, nil), 404)
	var billing string
	h.a.DB.QueryRow(r.ctx, "SELECT details_enc FROM order_billing WHERE order_id=$1", r.id).Scan(&billing)
	h.a.DB.Exec(r.ctx, "UPDATE settings SET value=jsonb_set(value,'{address_templates}','[]') WHERE key='billing'")
	if err := r.save(cp); err != nil {
		t.Fatal(err)
	}
	var after string
	h.a.DB.QueryRow(r.ctx, "SELECT details_enc FROM order_billing WHERE order_id=$1", r.id).Scan(&after)
	if after != billing {
		t.Fatal("billing snapshot changed")
	}
	r.finish(cslive.Result{Checkpoint: cp})
}

func TestNativeBoundProxyAndLease(t *testing.T) {
	h, r, cp := nativeFixture(t, false)
	var before string
	h.a.DB.QueryRow(r.ctx, "SELECT endpoint_enc FROM order_proxy_sessions WHERE order_id=$1", r.id).Scan(&before)
	for _, recorded := range []string{"", "203.0.113.1", "203.0.113.2"} {
		h.a.DB.Exec(r.ctx, "UPDATE order_proxy_sessions SET expected_ip=$2 WHERE order_id=$1", r.id, recorded)
		if err := r.guardProxy(r.ctx); err != nil {
			t.Fatal("exit metadata affected bound proxy", err)
		}
	}
	var after string
	h.a.DB.QueryRow(r.ctx, "SELECT endpoint_enc FROM order_proxy_sessions WHERE order_id=$1", r.id).Scan(&after)
	if before == "" || before != after {
		t.Fatal("proxy binding changed")
	}
	tx, err := h.a.DB.Begin(r.ctx)
	if err != nil {
		t.Fatal(err)
	}
	changed, err := h.a.restartCheckoutNetworkTx(r.ctx, tx, r.id, "proxy_session_changed")
	tx.Rollback(r.ctx)
	if err != nil || changed {
		t.Fatal("native IP was replaced")
	}
	h.a.DB.Exec(r.ctx, "UPDATE order_proxy_sessions SET expires_at=now()-interval '1 second' WHERE order_id=$1", r.id)
	if cslive.ErrorCode(r.guardProxy(r.ctx)) != "proxy_session_expired" {
		t.Fatal("expired session sent request")
	}
	stale := *r
	stale.lease = "STALE_LEASE"
	if cslive.ErrorCode(stale.guardProxy(r.ctx)) != "proxy_session_unavailable" {
		t.Fatal("stale lease passed proxy guard")
	}
	if !errors.Is(stale.save(cp), ErrLeaseLost) {
		t.Fatal("stale lease saved credentials")
	}
	cp.AccountID = "another-account"
	if cslive.ErrorCode(r.save(cp)) != "account_mismatch" {
		t.Fatal("changed identity accepted")
	}
	r.finish(cslive.Result{Checkpoint: r.last})
}

func TestNativeSessionRefreshAdmission(t *testing.T) {
	session := testSession("subscription@example.com")
	parts := strings.Split(str(session, "accessToken"), ".")
	claims := M{"exp": time.Now().Add(-time.Hour).Unix()}
	encoded, _ := json.Marshal(claims)
	parts[1] = base64.RawURLEncoding.EncodeToString(encoded)
	session["accessToken"] = strings.Join(parts, ".")
	session["sessionToken"] = "TEST_SESSION"
	session["expires"] = time.Now().Add(time.Hour).Format(time.RFC3339)
	raw, _ := json.Marshal(session)
	if _, err := inspectSessionMode(raw, true); err != nil {
		t.Fatal("refreshable AT rejected", err)
	}
	if _, err := inspectSessionMode(raw, false); err == nil {
		t.Fatal("expired AT allowed for old link flow")
	}
	delete(session, "sessionToken")
	raw, _ = json.Marshal(session)
	if _, err := inspectSessionMode(raw, true); err == nil {
		t.Fatal("new subscription allowed AT-only input")
	}
}
