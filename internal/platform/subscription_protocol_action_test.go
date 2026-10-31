package platform

import (
	"context"
	"strings"
	"testing"
	"time"

	"xupay/internal/cslive"
)

type fixtureProtocolClient struct {
	pay     func(context.Context, cslive.Session, cslive.Card, cslive.PayOptions) (cslive.Result, error)
	recover func(context.Context, cslive.Checkpoint) (cslive.Result, error)
	proxy   string
}

func (c fixtureProtocolClient) Pay(ctx context.Context, s cslive.Session, card cslive.Card, o cslive.PayOptions) (cslive.Result, error) {
	return c.pay(ctx, s, card, o)
}
func (c fixtureProtocolClient) Recover(ctx context.Context, cp cslive.Checkpoint) (cslive.Result, error) {
	return c.recover(ctx, cp)
}
func (c fixtureProtocolClient) Proxy() string { return c.proxy }

func waitPreparedAction(t *testing.T, h *harness, id string) *paymentAction {
	t.Helper()
	for until := time.Now().Add(5 * time.Second); time.Now().Before(until); {
		h.a.paymentMu.Lock()
		v := h.a.paymentActions[id]
		h.a.paymentMu.Unlock()
		if v != nil {
			return v
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatal("verification entry not prepared")
	return nil
}

func TestSubscriptionWorkerPreparesEntryAndRecoversOriginalPayment(t *testing.T) {
	h, fixture, cp := protocolFixture(t, false, true)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	pays, recovers := 0, 0
	started := make(chan struct{}, 2)
	process := &fixtureAction{events: make(chan actionEvent, 3)}
	h.a.actionStart = func(_ context.Context, in actionInput) (actionProcess, error) {
		if in.Automatic || in.ClientSecret != cp.ClientSecret {
			t.Error("reference interaction or original context changed")
		}
		started <- struct{}{}
		process.events <- actionEvent{Type: "ready"}
		return process, nil
	}
	h.a.protocolClient = func(run *protocolRun, _ int) (protocolPaymentClient, func(), error) {
		return fixtureProtocolClient{proxy: cp.Proxy,
			pay: func(_ context.Context, s cslive.Session, card cslive.Card, o cslive.PayOptions) (cslive.Result, error) {
				pays++
				if s.SessionToken == "" || card.Number != "4242424242424242" || o.CheckoutType != "hosted" {
					t.Fatal("whole-flow input changed")
				}
				if ok, err := o.ApprovePayment(1500, "PHP"); !ok || err != nil {
					t.Fatal(err)
				}
				if _, err := h.a.DB.Exec(run.ctx, "UPDATE order_proxy_sessions SET expected_ip='203.0.113.2' WHERE order_id=$1", run.id); err != nil {
					t.Fatal(err)
				}
				cp.Payment, cp.Phase = "action_required", "payment_result"
				if err := run.save(cp); err != nil {
					t.Fatal(err)
				}
				return cslive.Result{Checkpoint: cp}, nil
			},
			recover: func(_ context.Context, original cslive.Checkpoint) (cslive.Result, error) {
				recovers++
				if original.CheckoutID != cp.CheckoutID || original.PaymentIntentID != cp.PaymentIntentID {
					t.Error("recovery changed original payment")
				}
				original = nativeCharged(original, 1500)
				original.Activation, original.Renewal, original.Phase = "active", "off", "completed"
				return cslive.Result{Checkpoint: original}, nil
			},
		}, func() {}, nil
	}
	done := make(chan struct{})
	go func() { defer close(done); h.a.executeProtocolSubscription(ctx, fixture.job) }()
	t.Cleanup(func() {
		cancel()
		select {
		case <-done:
		case <-time.After(5 * time.Second):
			t.Error("worker did not stop")
		}
	})
	entry := waitPreparedAction(t, h, fixture.id)
	waitPayment(t, h, fixture.id, "action_required")
	paymentBalances(t, h, 90000, 10000)
	select {
	case <-started:
		t.Fatal("browser started before opening entry")
	default:
	}
	path := "/api/admin/orders/" + fixture.id + "/payment/action"
	view := expect(t, h.request("GET", path, nil, h.admin, nil), 200)
	if view["state"] != "available" || entry.expires.After(time.Now().Add(10*time.Minute)) {
		t.Fatal("missing bounded verification entry")
	}
	if strings.Contains(h.request("GET", path, nil, h.admin, nil).Body.String(), cp.ClientSecret) {
		t.Fatal("entry exposed client secret")
	}
	expect(t, h.request("POST", path, M{}, h.member, nil), 403)
	detail := h.a.paymentDetails(ctx, fixture.id)
	if detail["automatic_running"] != false || detail["can_check"] != false {
		t.Fatal("waiting entry mislabeled", detail)
	}
	if releaseActiveCounter(t, h)() == 0 {
		t.Fatal("same task lost release guard")
	}
	expect(t, h.request("POST", path, M{}, h.admin, nil), 202)
	select {
	case <-started:
	case <-time.After(5 * time.Second):
		t.Fatal("opening entry did not start browser")
	}
	expect(t, h.request("POST", path, M{}, h.admin, nil), 202)
	if _, err := h.a.DB.Exec(ctx, "UPDATE users SET role='admin' WHERE id=$1", str(expect(t, h.request("GET", "/api/me", nil, h.other, nil), 200), "id")); err != nil {
		t.Fatal(err)
	}
	expect(t, h.request("POST", path, M{}, h.other, nil), 409)
	if expect(t, h.request("GET", path, nil, h.other, nil), 200)["state"] != "idle" {
		t.Fatal("other admin inherited session")
	}
	paymentBalances(t, h, 90000, 10000)
	process.events <- actionEvent{Type: "result", Outcome: "sdk_returned"}
	close(process.events)
	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("worker did not recover")
	}
	if pays != 1 || recovers != 1 || len(started) != 0 {
		t.Fatal("duplicate payment/browser/recovery", pays, recovers)
	}
	waitPayment(t, h, fixture.id, "succeeded")
	paymentBalances(t, h, 90000, 0)
	var leased bool
	if err := h.a.DB.QueryRow(ctx, "SELECT lease_token IS NOT NULL OR lease_until IS NOT NULL FROM subscription_protocol WHERE order_id=$1", fixture.id).Scan(&leased); err != nil || leased {
		t.Fatal("verification retained worker lease", err)
	}
}

func TestPreparedVerificationCancellationRetainsOriginalPayment(t *testing.T) {
	h, run, cp := nativeFixture(t, false)
	if ok, err := run.approve(1500, "PHP"); err != nil || !ok {
		t.Fatal(err)
	}
	cp.Payment, cp.Phase = "action_required", "payment_result"
	h.a.actionStart = func(context.Context, actionInput) (actionProcess, error) {
		t.Fatal("unattended browser started")
		return nil, nil
	}
	done := make(chan cslive.Result, 1)
	go func() {
		result, err := run.continuePaymentAction(fixtureProtocolClient{}, cslive.Result{Checkpoint: cp}, nil)
		if err != nil {
			t.Error(err)
		}
		done <- result
	}()
	entry := waitPreparedAction(t, h, run.id)
	entry.cancel()
	select {
	case result := <-done:
		if result.Checkpoint.Payment != "action_required" || result.WarningCode != "action_interrupted" {
			t.Fatal("lost original payment")
		}
		run.finish(result)
	case <-time.After(5 * time.Second):
		t.Fatal("entry did not stop")
	}
	waitPayment(t, h, run.id, "action_required")
	paymentBalances(t, h, 90000, 10000)
}

func TestPreparedVerificationExpiryDoesNotInvokeSDK(t *testing.T) {
	h, run, cp := nativeFixture(t, false)
	if ok, err := run.approve(1500, "PHP"); err != nil || !ok {
		t.Fatal(err)
	}
	cp.Payment, cp.Phase = "action_required", "payment_result"
	ctx, cancel := context.WithTimeout(run.ctx, 350*time.Millisecond)
	defer cancel()
	run.ctx = ctx
	h.a.actionStart = func(context.Context, actionInput) (actionProcess, error) {
		t.Error("expired entry started SDK")
		return nil, nil
	}
	result, err := run.continuePaymentAction(fixtureProtocolClient{}, cslive.Result{Checkpoint: cp}, nil)
	if err != nil || result.WarningCode != "action_timeout" || result.Checkpoint.Payment != "action_required" {
		t.Fatal("expiry lost payment", err)
	}
	run.finish(result)
	waitPayment(t, h, run.id, "action_required")
	paymentBalances(t, h, 90000, 10000)
}

func TestPausedProtocolRecoveryPreparesEntryWithoutPayReplay(t *testing.T) {
	h, run, cp := nativeFixture(t, false)
	if ok, err := run.approve(1500, "PHP"); err != nil || !ok {
		t.Fatal(err)
	}
	cp.Payment, cp.Phase = "action_required", "payment_result"
	cp.Entity, cp.DeviceID, cp.StripeVersion = "openai_llc", "fixture-device", "fixture-version"
	cp.Metadata["browser_profile"] = "chrome131-windows-v1"
	cp.StripeIdentity["guid"], cp.StripeIdentity["muid"], cp.StripeIdentity["sid"] = "fixture-guid", "fixture-muid", "fixture-sid"
	cp.Metadata["hosted_url"] = "https://pay.openai.com/c/pay/" + cp.CheckoutID + "#" + strings.Repeat("x", 260)
	if err := cp.ValidateForRecovery(); err != nil {
		t.Fatal("invalid recovery fixture", err)
	}
	run.finish(cslive.Result{Checkpoint: cp})
	if _, err := h.a.DB.Exec(context.Background(), "UPDATE order_proxy_sessions SET expected_ip='203.0.113.2' WHERE order_id=$1", run.id); err != nil {
		t.Fatal(err)
	}
	process := &fixtureAction{events: make(chan actionEvent, 2)}
	started := make(chan struct{}, 1)
	recovered := make(chan struct{}, 2)
	h.a.actionStart = func(context.Context, actionInput) (actionProcess, error) {
		started <- struct{}{}
		process.events <- actionEvent{Type: "ready"}
		return process, nil
	}
	h.a.protocolClient = func(_ *protocolRun, _ int) (protocolPaymentClient, func(), error) {
		return fixtureProtocolClient{proxy: cp.Proxy, pay: func(context.Context, cslive.Session, cslive.Card, cslive.PayOptions) (cslive.Result, error) {
			t.Error("Pay replayed")
			return cslive.Result{}, nil
		}, recover: func(_ context.Context, original cslive.Checkpoint) (cslive.Result, error) {
			if original.CheckoutID != cp.CheckoutID || original.PaymentIntentID != cp.PaymentIntentID {
				t.Error("wrong recovered transaction")
			}
			recovered <- struct{}{}
			return cslive.Result{Checkpoint: original}, nil
		}}, func() {}, nil
	}
	path := "/api/admin/orders/" + run.id + "/payment"
	expect(t, h.request("POST", path+"/check", M{}, h.admin, nil), 202)
	entry := waitPreparedAction(t, h, run.id)
	t.Cleanup(entry.cancel)
	select {
	case <-started:
		t.Fatal("recovery bypassed operator entry")
	default:
	}
	expect(t, h.request("POST", path+"/action", M{}, h.admin, nil), 202)
	select {
	case <-started:
	case <-time.After(5 * time.Second):
		t.Fatal("SDK not started")
	}
	paymentBalances(t, h, 90000, 10000)
	process.events <- actionEvent{Type: "result", Outcome: "sdk_returned"}
	close(process.events)
	for until := time.Now().Add(5 * time.Second); time.Now().Before(until); {
		entry.mu.Lock()
		finished := entry.state == "finished"
		entry.mu.Unlock()
		if finished {
			break
		}
		time.Sleep(10 * time.Millisecond)
	}
	waitPayment(t, h, run.id, "action_required")
	if len(recovered) != 2 {
		t.Fatal("SDK completion did not query original payment exactly once", len(recovered))
	}
	paymentBalances(t, h, 90000, 10000)
}

func TestProtocolClientPreservesReferenceOperationTimeout(t *testing.T) {
	_, run, _ := nativeFixture(t, false)
	c, closeClient, err := run.client(1200)
	if err != nil {
		t.Fatal(err)
	}
	defer closeClient()
	config := c.(nativeProtocolClient).Config
	defaults := cslive.DefaultConfig()
	if config.Timeout != defaults.Timeout || config.Deadline != defaults.Deadline || config.BrowserProfile != defaults.BrowserProfile {
		t.Fatal("worker budget changed protocol defaults")
	}
}
