package platform

import (
	"context"
	"encoding/base64"
	"errors"
	"math"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"
	"xupay/internal/cslive"
)

type fixtureAction struct {
	events chan actionEvent
	sent   []actionCommand
	closed bool
}

func (p *fixtureAction) Events() <-chan actionEvent { return p.events }
func (p *fixtureAction) Send(v actionCommand) error { p.sent = append(p.sent, v); return nil }
func (p *fixtureAction) Close()                     { p.closed = true }

func TestActionInputBoundary(t *testing.T) {
	for _, v := range []actionCommand{{Type: "click", X: math.NaN()}, {Type: "click", X: 960}, {Type: "scroll", Y: 601}, {Type: "text", Text: "OTP\tsecret"}, {Type: "text", Text: strings.Repeat("字", 129)}, {Type: "key", Key: "Control+L"}, {Type: "guard", Allow: true}, {Type: "navigate", Text: "https://bank.example"}} {
		if validActionCommand(v) {
			t.Fatalf("accepted invalid command type %s", v.Type)
		}
	}
	for _, v := range []actionCommand{{Type: "click", X: 10, Y: 20}, {Type: "text", Text: strings.Repeat("字", 128)}, {Type: "key", Key: "Tab"}, {Type: "close"}} {
		if !validActionCommand(v) {
			t.Fatal("valid input rejected")
		}
	}
}

func TestActionRoutesActorExpiryAndRestart(t *testing.T) {
	h := setupTest(t)
	actor := str(expect(t, h.request("GET", "/api/me", nil, h.admin, nil), 200), "id")
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	v := &paymentAction{id: "action_fixture", actor: actor, state: "verifying", expires: time.Now().Add(time.Minute), cancel: cancel, frame: []byte{255, 216, 1, 2}}
	p := &fixtureAction{events: make(chan actionEvent)}
	v.process = p
	h.a.paymentActions = map[string]*paymentAction{"order_fixture": v}
	path := "/api/admin/orders/order_fixture/payment/action"
	expect(t, h.request("GET", path, nil, nil, nil), 401)
	expect(t, h.request("GET", path, nil, h.member, nil), 403)
	expect(t, h.request("POST", path, nil, h.member, nil), 403)
	expect(t, h.request("POST", path+"/input", M{"id": v.id, "type": "close"}, h.member, nil), 403)
	expect(t, h.request("POST", path+"/input", M{"id": "stale", "type": "close"}, h.admin, nil), 409)
	expect(t, h.request("POST", path+"/input", M{"id": v.id, "type": "text", "text": "123456"}, h.admin, nil), 200)
	if len(p.sent) != 1 || p.sent[0].Text != "123456" {
		t.Fatal("operator input not delivered")
	}
	view := expect(t, h.request("GET", path, nil, h.admin, nil), 200)
	if !strings.HasPrefix(str(view, "image"), "data:image/jpeg;") {
		t.Fatal("missing live image")
	}
	// Another administrator cannot inherit an operator's verification session.
	_, err := h.a.DB.Exec(ctx, "UPDATE users SET role='admin' WHERE email='other@test.local'")
	if err != nil {
		t.Fatal(err)
	}
	if view = expect(t, h.request("GET", path, nil, h.other, nil), 200); view["state"] != "idle" {
		t.Fatal("session leaked to another operator")
	}
	expect(t, h.request("POST", path+"/input", M{"id": v.id, "type": "close"}, h.other, nil), 404)
	v.expires = time.Now().Add(-time.Second)
	if view = expect(t, h.request("GET", path, nil, h.admin, nil), 200); view["image"] != "" {
		t.Fatal("expired frame retained")
	}
	expect(t, h.request("POST", path+"/input", M{"id": v.id, "type": "click"}, h.admin, nil), 409)
	v.expires = time.Now().Add(time.Minute)
	expect(t, h.request("POST", path+"/input", M{"id": v.id, "type": "close"}, h.admin, nil), 200)
	if ctx.Err() == nil || len(v.frame) != 0 {
		t.Fatal("close retained session")
	}
	h.a.paymentActions = nil // Process restart never revives a browser or verification inputs.
	if view = expect(t, h.request("GET", path, nil, h.admin, nil), 200); view["state"] != "idle" {
		t.Fatal("restart revived action")
	}
}

func TestActionUsesOriginalContextAndNeverSettlesFromBrowser(t *testing.T) {
	for _, mode := range []string{"sdk_returned", "sdk_error", "runtime_error", "interrupted", "ip_drift", "expired", "bad_intent"} {
		t.Run(mode, func(t *testing.T) {
			h, r, cp := nativeFixture(t, false)
			if mode == "ip_drift" {
				mockOrderIP(t, h.a, "203.0.113.3")
			}
			if ok, e := r.approve(1500, "PHP"); !ok || e != nil {
				t.Fatal("approval", e)
			}
			cp.Payment, cp.Phase = "action_required", "payment_result"
			if err := r.save(cp); err != nil {
				t.Fatal(err)
			}
			_, err := h.a.DB.Exec(r.ctx, "UPDATE order_proxy_sessions SET expected_ip='203.0.113.2' WHERE order_id=$1", r.id)
			if err != nil {
				t.Fatal(err)
			}
			var beforeProxy, beforeBilling string
			if err = h.a.DB.QueryRow(r.ctx, "SELECT endpoint_enc,details_enc FROM order_proxy_sessions JOIN order_billing USING(order_id) WHERE order_id=$1", r.id).Scan(&beforeProxy, &beforeBilling); err != nil {
				t.Fatal(err)
			}
			process := &fixtureAction{events: make(chan actionEvent, 6)}
			started := false
			h.a.actionStart = func(ctx context.Context, in actionInput) (actionProcess, error) {
				started = true
				if in.ClientSecret != cp.ClientSecret || in.PublishableKey != cp.PublishableKey || in.Proxy.Password == "" || in.ExpiresAt > time.Now().Add(10*time.Minute).Unix() {
					t.Error("original private context changed")
				}
				if mode == "runtime_error" {
					return nil, errors.New("PRIVATE_RUNTIME_ERROR")
				}
				process.events <- actionEvent{Type: "ready"}
				process.events <- actionEvent{Type: "guard", RequestID: 1}
				process.events <- actionEvent{Type: "frame", Image: base64.StdEncoding.EncodeToString([]byte{255, 216, 1, 2})}
				if mode != "interrupted" {
					process.events <- actionEvent{Type: "result", Outcome: mode, Code: "PRIVATE_UNSAFE_CODE"}
				}
				close(process.events)
				return process, nil
			}
			if mode == "expired" {
				h.a.DB.Exec(r.ctx, "UPDATE order_proxy_sessions SET expires_at=now()-interval '1 second' WHERE order_id=$1", r.id)
			}
			if mode == "bad_intent" {
				cp.PaymentIntentID = "pi_Wrong"
			}
			v := &paymentAction{}
			err = r.runPaymentAction(cp, v)
			want := map[string]string{"ip_drift": "proxy_session_changed", "runtime_error": "action_runtime_unavailable", "interrupted": "action_interrupted", "expired": "proxy_session_expired", "bad_intent": "payment_intent_state_unavailable"}[mode]
			if cslive.ErrorCode(err) != want {
				t.Fatalf("result %v, wanted %s", err, want)
			}
			if (mode == "expired" || mode == "bad_intent" || mode == "ip_drift") && started {
				t.Fatal("invalid context started browser")
			}
			if started && mode != "runtime_error" && !process.closed {
				t.Fatal("process leaked")
			}
			if len(v.frame) != 0 || v.process != nil {
				t.Fatal("finished action retained image/process")
			}
			paymentBalances(t, h, 90000, 10000)
			var afterProxy, afterBilling, fee string
			var charged *int64
			if err = h.a.DB.QueryRow(r.ctx, "SELECT endpoint_enc,details_enc,p.fee_state,p.charged_amount FROM order_proxy_sessions JOIN order_billing USING(order_id) JOIN payment_runs p USING(order_id) WHERE order_id=$1", r.id).Scan(&afterProxy, &afterBilling, &fee, &charged); err != nil {
				t.Fatal(err)
			}
			if beforeProxy != afterProxy || beforeBilling != afterBilling || fee != "held" || charged != nil {
				t.Fatal("browser changed money, proxy or billing")
			}
			admin := h.request("GET", "/api/admin/orders/"+r.id, nil, h.admin, nil)
			if strings.Contains(admin.Body.String(), "PRIVATE_UNSAFE_CODE") || strings.Contains(admin.Body.String(), cp.ClientSecret) {
				t.Fatal("diagnostic exposed secrets")
			}
			r.fail(cslive.Failure("action_interrupted"))
			waitPayment(t, h, r.id, "action_required")
		})
	}
}

func TestActionDiagnosticsRetainSafeChallengeType(t *testing.T) {
	h, r, cp := nativeFixture(t, false)
	r.event(map[string]any{"event": "payment_diagnostic", "paymentState": "action_required", "rawStatus": "requires_action", "nextActionType": "use_stripe_sdk", "sdkActionType": "intent_confirmation_challenge", "sdkHasStripeJS": true, "sdkHasThreeDS2Source": false, "stripe_js": "PRIVATE_CHALLENGE_URL"})
	select {
	case d := <-h.a.providerDiagnostics:
		h.a.saveProviderDiagnostic(d)
	default:
		t.Fatal("diagnostic missing")
	}
	res := h.request("GET", "/api/admin/orders/"+r.id, nil, h.admin, nil)
	for _, s := range []string{"intent_confirmation_challenge", "use_stripe_sdk", "requires_action", "sdk_has_stripe_js"} {
		if !strings.Contains(res.Body.String(), s) {
			t.Fatal("missing diagnostic", s)
		}
	}
	if strings.Contains(res.Body.String(), "PRIVATE_CHALLENGE") {
		t.Fatal("raw challenge exposed")
	}
	res = h.request("GET", "/api/orders/"+r.id, nil, h.member, nil)
	if strings.Contains(res.Body.String(), "sdk_action_type") {
		t.Fatal("customer saw core diagnostics")
	}
	r.finish(cslive.Result{Checkpoint: cp})
}

func TestReleaseGuardPreservesPausedProtocolOrders(t *testing.T) {
	var err error
	h, r, cp := nativeFixture(t, false)
	active := releaseActiveCounter(t, h)
	if active() == 0 {
		t.Fatal("running payment did not block release")
	}
	if ok, e := r.approve(1500, "PHP"); !ok || e != nil {
		t.Fatal(e)
	}
	cp.Payment, cp.Phase = "action_required", "payment_result"
	if err = r.save(cp); err != nil {
		t.Fatal(err)
	}
	r.finish(cslive.Result{Checkpoint: cp})
	if active() != 0 {
		t.Fatal("durable paused order incorrectly blocked release")
	}
	var before string
	h.a.DB.QueryRow(r.ctx, "SELECT checkpoint_enc FROM subscription_protocol WHERE order_id=$1", r.id).Scan(&before)
	if err = h.a.RecoverPayments(r.ctx); err != nil {
		t.Fatal(err)
	}
	var after string
	h.a.DB.QueryRow(r.ctx, "SELECT checkpoint_enc FROM subscription_protocol WHERE order_id=$1", r.id).Scan(&after)
	if before != after {
		t.Fatal("restart modified paused checkpoint")
	}
	paymentBalances(t, h, 90000, 10000)
	h.a.DB.Exec(r.ctx, "UPDATE subscription_protocol SET lease_token='active',lease_until=now()+interval '1 minute' WHERE order_id=$1", r.id)
	if active() == 0 {
		t.Fatal("active verification did not block release")
	}
	h.a.DB.Exec(r.ctx, "UPDATE subscription_protocol SET lease_token=NULL,lease_until=NULL,checkpoint_enc='' WHERE order_id=$1", r.id)
	if active() == 0 {
		t.Fatal("incomplete paused checkpoint allowed release")
	}
}

func releaseActiveCounter(t *testing.T, h *harness) func() int {
	psql, err := exec.LookPath("psql")
	if err != nil {
		psql = `C:\Program Files\PostgreSQL\18\bin\psql.exe`
		if _, err = os.Stat(psql); err != nil {
			t.Skip("psql required for release guard integration")
		}
	}
	script, err := os.ReadFile(filepath.Join("..", "..", "scripts", "active-work.sql"))
	if err != nil {
		t.Fatal(err)
	}
	return func() int {
		t.Helper()
		u, _ := url.Parse(os.Getenv("TEST_DATABASE_URL"))
		q := u.Query()
		q.Del("search_path")
		u.RawQuery = q.Encode()
		cmd := exec.Command(psql, "-X", "-At", "-d", u.String())
		cmd.Env = append(os.Environ(), "PGOPTIONS=-c search_path="+h.a.DB.Config().ConnConfig.RuntimeParams["search_path"])
		cmd.Stdin = strings.NewReader(string(script))
		out, e := cmd.CombinedOutput()
		if e != nil {
			t.Fatalf("guard: %v %s", e, out)
		}
		n, e := strconv.Atoi(strings.TrimSpace(string(out)))
		if e != nil {
			t.Fatal(e)
		}
		return n
	}
}

func TestReleaseGuardPreservesPausedLegacyOrders(t *testing.T) {
	h, id, payment := subscriptionFixture(t, false)
	ctx := context.Background()
	active := releaseActiveCounter(t, h)
	completeSubscriptionCheckout(t, h, id)
	payment.events <- paymentEvent{Type: "quote", Amount: 1500, Currency: "php"}
	waitPayment(t, h, id, "submitting")
	if active() == 0 {
		t.Fatal("running legacy payment did not block release")
	}
	payment.events <- paymentEvent{Type: "result", State: "unknown", Code: "result_unconfirmed", Sent: true}
	waitPayment(t, h, id, "review")
	if active() == 0 {
		t.Fatal("runtime deadline has not passed")
	}
	if _, err := h.a.DB.Exec(ctx, "UPDATE payment_runs SET expires_at=now()-interval '1 second' WHERE order_id=$1", id); err != nil {
		t.Fatal(err)
	}
	if active() != 0 {
		t.Fatal("durable paused legacy order incorrectly blocked release")
	}
	var before, after string
	snapshot := `SELECT jsonb_build_array(to_jsonb(p),to_jsonb(u),to_jsonb(o))::text FROM payment_runs p JOIN subscription_runs u ON u.order_id=p.order_id JOIN orders o ON o.id=p.order_id WHERE p.order_id=$1`
	if err := h.a.DB.QueryRow(ctx, snapshot, id).Scan(&before); err != nil {
		t.Fatal(err)
	}
	if err := h.a.RecoverPayments(ctx); err != nil {
		t.Fatal(err)
	}
	if err := h.a.DB.QueryRow(ctx, snapshot, id).Scan(&after); err != nil {
		t.Fatal(err)
	}
	if before != after {
		t.Fatal("restart altered paused payment or authorization")
	}
	paymentBalances(t, h, 90000, 10000)
	for _, tc := range []struct{ change, restore string }{
		{"UPDATE payment_runs SET state='checking' WHERE order_id=$1", "UPDATE payment_runs SET state='review' WHERE order_id=$1"},
		{"UPDATE jobs SET state='running',lease_token='worker',lease_until=now()+interval '1 minute' WHERE order_id=$1", "UPDATE jobs SET state='completed',lease_token=NULL,lease_until=NULL WHERE order_id=$1"},
		{"UPDATE subscription_runs SET lease_token='worker',lease_until=now()+interval '1 minute' WHERE order_id=$1", "UPDATE subscription_runs SET lease_token=NULL,lease_until=NULL WHERE order_id=$1"},
		{"UPDATE payment_runs SET hosted_url_enc='' WHERE order_id=$1", "UPDATE payment_runs SET hosted_url_enc=(SELECT hosted_url_enc FROM checkout_runs WHERE order_id=$1) WHERE order_id=$1"},
	} {
		if _, err := h.a.DB.Exec(ctx, tc.change, id); err != nil {
			t.Fatal(err)
		}
		if active() == 0 {
			t.Fatal("active or incomplete legacy review allowed release")
		}
		if _, err := h.a.DB.Exec(ctx, tc.restore, id); err != nil {
			t.Fatal(err)
		}
	}
	if active() != 0 {
		t.Fatal("restored paused order did not become idle")
	}
}
