package platform

import (
	"context"
	"encoding/json"
	"net/http"
	"sort"
	"strings"
	"time"

	"xupay/internal/cslive"
)

type protocolCardSlot struct {
	ctx     context.Context
	card    cardInput
	billing M
	claimed chan struct{}
	done    chan struct{}
}

func (a *App) subscriptionEnabled(ctx context.Context) bool {
	cfg := a.config(ctx, "payment")
	flow := configuredSubscriptionFlow(cfg)
	return cfg["enabled"] == true && validSubscriptionFlow(configuredCheckoutFlow(cfg)) && validSubscriptionFlow(flow) && (flow != subscriptionFlowLegacy || a.paymentEnabled(ctx))
}

func (a *App) isProtocolSubscription(ctx context.Context, id string) bool {
	var exists bool
	return a.DB.QueryRow(ctx, "SELECT EXISTS(SELECT 1 FROM subscription_protocol WHERE order_id=$1)", id).Scan(&exists) == nil && exists
}

func protocolSession(in checkoutInput) (cslive.Session, error) {
	token := in.SessionToken
	if token == "" && in.Cookies != "" {
		cookies := (&http.Request{Header: http.Header{"Cookie": {in.Cookies}}}).Cookies()
		sort.Slice(cookies, func(i, j int) bool { return cookies[i].Name < cookies[j].Name })
		for _, family := range []string{"__Secure-next-auth.session-token", "__Secure-platform.session-token"} {
			for _, cookie := range cookies {
				if cookie.Name == family || strings.HasPrefix(cookie.Name, family+".") {
					token += cookie.Value
				}
			}
			if token != "" {
				break
			}
		}
	}
	if token == "" {
		return cslive.Session{}, bad("订阅办理需要包含 sessionToken 或有效会话 Cookie 的完整 Session，请重新获取")
	}
	return cslive.Session{AccessToken: in.AccessToken, SessionToken: token, AccountID: in.AccountID, Email: in.Email, Cookies: in.Cookies}, nil
}

func (a *App) queueProtocolSubscription(id, attempt string, card cardInput, billing M) {
	ctx, cancel := context.WithCancel(context.Background())
	slot := &protocolCardSlot{ctx: ctx, card: card, billing: billing, claimed: make(chan struct{}), done: make(chan struct{})}
	s := &paymentSession{attempt: attempt, cancel: cancel, protocol: slot}
	a.paymentMu.Lock()
	if a.payments == nil {
		a.payments = map[string]*paymentSession{}
	}
	a.payments[id] = s
	a.paymentMu.Unlock()
	wait := time.Duration(num(a.config(ctx, "payment"), "subscription_wait_seconds", 600)) * time.Second
	go func() {
		defer cancel()
		defer func() {
			a.paymentMu.Lock()
			slot.card, slot.billing = cardInput{}, nil
			if a.payments[id] == s {
				delete(a.payments, id)
			}
			a.paymentMu.Unlock()
		}()
		timer := time.NewTimer(wait)
		defer timer.Stop()
		tick := time.NewTicker(time.Second)
		defer tick.Stop()
	waiting:
		for {
			select {
			case <-slot.done:
				return
			case <-slot.claimed:
				select {
				case <-slot.done:
					return
				case <-ctx.Done():
					// The worker owns finalization once it has taken the card.
					return
				}
			case <-ctx.Done():
				break waiting
			case <-timer.C:
				cancel()
				break waiting
			case <-tick.C:
				var terminal bool
				if a.DB.QueryRow(ctx, "SELECT state IN ('failed','review') FROM jobs WHERE order_id=$1", id).Scan(&terminal) == nil && terminal {
					break waiting
				}
			}
		}
		final, stop := context.WithTimeout(context.Background(), 10*time.Second)
		defer stop()
		a.finishPayment(final, id, attempt, paymentEvent{State: "unknown", Code: "result_unconfirmed"})
	}()
}

type protocolRun struct {
	a                                           *App
	ctx                                         context.Context
	id, attempt, lease, account, email, country string
	keep                                        bool
	checkoutFlow                                string
	maxAmount                                   int64
	currency                                    string
	job                                         *Job
	last                                        cslive.Checkpoint
	stageStarted                                map[string]time.Time
}

func (a *App) executeProtocolSubscription(parent context.Context, j *Job) {
	var slot *protocolCardSlot
	var attempt string
	// Admission commits before the request handler registers the in-memory card.
	for i := 0; i < 20; i++ {
		a.paymentMu.Lock()
		if s := a.payments[j.OrderID]; s != nil {
			slot, attempt = s.protocol, s.attempt
		}
		a.paymentMu.Unlock()
		if slot != nil {
			break
		}
		select {
		case <-parent.Done():
			return
		case <-time.After(50 * time.Millisecond):
		}
	}
	if slot == nil {
		// A restart cannot recover card data. Release this unsubmitted order
		// promptly rather than recreate checkout or wait for the original queue TTL.
		if a.DB.QueryRow(parent, "SELECT attempt_id FROM payment_runs WHERE order_id=$1", j.OrderID).Scan(&attempt) == nil {
			run := &protocolRun{a: a, ctx: parent, id: j.OrderID, attempt: attempt, lease: j.Lease, job: j}
			if run.claim(30) == nil {
				run.finish(cslive.Result{ErrorCode: "payment_not_submitted"})
			}
		}
		return
	}
	// Reserve the original package's ten-minute SDK window as well as time for
	// Pay and the authoritative Recover/activation/renewal sequence afterwards.
	ttl := int(2*cslive.DefaultConfig().Deadline) + 600 + 120
	ctx, cancel := context.WithTimeout(parent, time.Duration(ttl)*time.Second)
	defer cancel()
	stop := context.AfterFunc(slot.ctx, cancel)
	defer stop()
	a.paymentMu.Lock()
	if slot.card.Number == "" {
		a.paymentMu.Unlock()
		return
	}
	card, billing := slot.card, slot.billing
	slot.card, slot.billing = cardInput{}, nil
	close(slot.claimed)
	a.paymentMu.Unlock()
	defer close(slot.done)
	defer func() { card = cardInput{}; billing = nil }()
	run := &protocolRun{a: a, ctx: ctx, id: j.OrderID, attempt: attempt, lease: j.Lease, job: j}
	err := run.claim(ttl)
	if err != nil {
		run.fail(err)
		return
	}
	done := make(chan struct{})
	go run.heartbeat(ctx, cancel, done)
	defer func() { cancel(); <-done }()
	var credentials checkoutInput
	var enc, product, checkoutType, checkoutFlow, workspace string
	var seats, premium int
	err = a.DB.QueryRow(ctx, `SELECT c.credentials_enc,o.product_id,o.seats,c.premium_seats,o.country,o.keep_renewal,o.email,s.max_amount,s.currency,c.checkout_type,c.flow,c.workspace_name
 FROM checkout_runs c JOIN orders o ON o.id=c.order_id JOIN subscription_runs s ON s.order_id=o.id WHERE o.id=$1 AND c.credentials_expires_at>now()`, run.id).
		Scan(&enc, &product, &seats, &premium, &run.country, &run.keep, &run.email, &run.maxAmount, &run.currency, &checkoutType, &checkoutFlow, &workspace)
	if err != nil {
		run.fail(err)
		return
	}
	raw, err := a.decrypt(enc)
	if err != nil || json.Unmarshal([]byte(raw), &credentials) != nil {
		run.fail(cslive.Failure("invalid_session"))
		return
	}
	run.account = credentials.AccountID
	run.checkoutFlow = checkoutFlow
	session, err := protocolSession(credentials)
	if err != nil {
		run.fail(cslive.Failure("session_token_required"))
		return
	}
	if checkoutFlow != subscriptionFlowCSLive {
		run.fail(cslive.Failure("invalid_request"))
		return
	}
	client, closeClient, err := run.client(ttl)
	if err != nil {
		run.fail(err)
		return
	}
	defer closeClient()
	plan := map[string]string{"pro": "pro20x", "pro5": "pro5x", "team_premium": "team5x"}[product]
	if plan == "" {
		plan = product
	}
	options := cslive.PayOptions{Country: run.country, Plan: plan, CheckoutType: protocolCheckoutType(checkoutType), Billing: cslive.Billing{
		Name: strings.TrimSpace(str(billing, "first_name") + " " + str(billing, "last_name")), Email: run.email,
		Line1: str(billing, "line1"), City: str(billing, "city"), State: str(billing, "state"), PostalCode: str(billing, "postal_code"), Country: str(billing, "country"),
	}, ApprovePayment: run.approve}
	if strings.HasPrefix(plan, "team") {
		options.Seats = seats
		if plan == "team5x" {
			options.Seats, options.Seats5x = seats, premium
		}
	}
	cancelRenewal := !run.keep
	options.CancelRenewal = &cancelRenewal
	result, err := client.Pay(ctx, session, cslive.Card{Number: card.Number, CVC: card.CVC, ExpMonth: card.Month, ExpYear: card.Year}, options)
	card = cardInput{}
	if err != nil {
		run.fail(err)
		return
	}
	result, err = run.continuePaymentAction(client, result, nil)
	if err != nil {
		run.fail(err)
		return
	}
	run.finish(result)
}
