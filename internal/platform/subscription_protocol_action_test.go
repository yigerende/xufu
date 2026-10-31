package platform

import (
	"context"
	"testing"

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

// A real worker persists an action requirement without starting an unattended
// browser or invoking Pay/Recover a second time. The admin SDK entry is tested
// separately in payment_action_test.go.
func TestSubscriptionWorkerLeavesOfficialVerificationForOperator(t *testing.T) {
	h, fixture, cp := protocolFixture(t, false, true)
	ctx := context.Background()
	pays := 0
	h.a.actionStart = func(context.Context, actionInput) (actionProcess, error) {
		t.Fatal("unattended browser started")
		return nil, nil
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
				cp.Payment, cp.Phase = "action_required", "payment_result"
				if err := run.save(cp); err != nil {
					t.Fatal(err)
				}
				return cslive.Result{Checkpoint: cp}, nil
			},
			recover: func(context.Context, cslive.Checkpoint) (cslive.Result, error) {
				t.Fatal("unexpected automatic recovery")
				return cslive.Result{}, nil
			},
		}, func() {}, nil
	}
	h.a.executeProtocolSubscription(ctx, fixture.job)
	if pays != 1 {
		t.Fatal("payment execution count", pays)
	}
	waitPayment(t, h, fixture.id, "action_required")
	paymentBalances(t, h, 90000, 10000)
	var leased bool
	if err := h.a.DB.QueryRow(ctx, "SELECT lease_token IS NOT NULL OR lease_until IS NOT NULL FROM subscription_protocol WHERE order_id=$1", fixture.id).Scan(&leased); err != nil || leased {
		t.Fatal("verification retained worker lease", err)
	}
}

func TestPendingVerificationDoesNotStartBrowserWithoutOperator(t *testing.T) {
	h, run, cp := nativeFixture(t, false)
	cp.Payment, cp.Phase = "action_required", "payment_result"
	h.a.actionStart = func(context.Context, actionInput) (actionProcess, error) {
		t.Fatal("unattended browser started")
		return nil, nil
	}
	result, err := run.continuePaymentAction(fixtureProtocolClient{}, cslive.Result{Checkpoint: cp}, nil)
	if err != nil || result.Checkpoint.Payment != "action_required" {
		t.Fatal("pending original transaction changed", err)
	}
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
