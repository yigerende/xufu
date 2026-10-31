package platform

import (
	"context"
	"errors"
	"time"

	"xupay/internal/cslive"
)

// Match the reference CLI's TLS-enabled Pay: prepare a ten-minute entry in the
// same task, wait for the operator to open it, then Recover the original payment.
func (r *protocolRun) continuePaymentAction(client protocolPaymentClient, prior cslive.Result, v *paymentAction) (cslive.Result, error) {
	cp := prior.Checkpoint
	if cp.Payment != "action_required" {
		return prior, nil
	}
	if !validPaymentActionCheckpoint(cp) {
		return prior, nil
	}
	if err := r.save(cp); err != nil {
		return prior, err
	}
	if v == nil {
		entry, stop, err := r.preparePaymentAction()
		if err != nil {
			return prior, err
		}
		defer stop()
		select {
		case <-entry.opened:
		case <-entry.ctx.Done():
			prior.WarningCode = "action_interrupted"
			if errors.Is(entry.ctx.Err(), context.DeadlineExceeded) {
				prior.WarningCode = "action_timeout"
			}
			return prior, nil
		}
		if entry.ctx.Err() != nil {
			prior.WarningCode = "action_interrupted"
			return prior, nil
		}
		v = entry
	}
	tx, err := r.a.DB.Begin(r.ctx)
	if err != nil {
		return prior, err
	}
	defer tx.Rollback(r.ctx)
	if err = r.lock(r.ctx, tx); err != nil {
		return prior, err
	}
	if _, err = tx.Exec(r.ctx, "UPDATE payment_runs SET state='checking',error_code='checking',updated_at=now() WHERE order_id=$1 AND attempt_id=$2 AND state<>'succeeded'", r.id, r.attempt); err != nil {
		return prior, err
	}
	if err = tx.Commit(r.ctx); err != nil {
		return prior, err
	}
	actionRun := *r
	if v.ctx != nil {
		actionRun.ctx = v.ctx
	}
	if err = actionRun.runPaymentAction(cp, v); err != nil {
		// Match the package: a lost/expired verification session preserves the
		// original payment. Never restart Pay or fabricate a successful callback.
		prior.WarningCode = cslive.ErrorCode(err)
		return prior, nil
	}
	cp.Proxy = client.Proxy()
	r.a.paymentTrace(r.ctx, r.id, "checking", "running", "", 0)
	// One SDK pass only. Recover may still report action_required; leave that
	// evidence intact instead of repeatedly invoking the same challenge.
	return client.Recover(r.ctx, cp)
}

func (r *protocolRun) preparePaymentAction() (*paymentAction, func(), error) {
	var proxyExpiry time.Time
	if err := r.a.DB.QueryRow(r.ctx, "SELECT expires_at FROM order_proxy_sessions WHERE order_id=$1 AND endpoint_enc<>''", r.id).Scan(&proxyExpiry); err != nil {
		return nil, nil, cslive.Failure("proxy_session_unavailable")
	}
	if !proxyExpiry.After(time.Now()) {
		return nil, nil, cslive.Failure("proxy_session_expired")
	}
	ctx, cancel := context.WithDeadline(r.ctx, minTime(proxyExpiry, time.Now().Add(10*time.Minute)))
	expiry, _ := ctx.Deadline()
	v := &paymentAction{id: uid(), state: "available", expires: expiry, cancel: cancel, ctx: ctx, opened: make(chan struct{})}
	tx, err := r.a.DB.Begin(r.ctx)
	if err != nil {
		cancel()
		return nil, nil, err
	}
	defer tx.Rollback(r.ctx)
	if err = r.lock(r.ctx, tx); err == nil {
		_, err = tx.Exec(r.ctx, "UPDATE payment_runs SET state='action_required',error_code='verification_required',updated_at=now() WHERE order_id=$1 AND attempt_id=$2 AND state<>'succeeded'", r.id, r.attempt)
	}
	if err == nil {
		err = paymentTraceTx(r.ctx, tx, r.id, "verification", "waiting", "verification_ready")
	}
	if err == nil {
		err = tx.Commit(r.ctx)
	}
	if err != nil {
		cancel()
		return nil, nil, err
	}
	r.a.paymentMu.Lock()
	if r.a.paymentActions == nil {
		r.a.paymentActions = map[string]*paymentAction{}
	}
	for key, entry := range r.a.paymentActions {
		entry.mu.Lock()
		expired := !entry.expires.After(time.Now())
		entry.mu.Unlock()
		if expired {
			delete(r.a.paymentActions, key)
		}
	}
	r.a.paymentActions[r.id] = v
	r.a.paymentMu.Unlock()
	stop := func() {
		cancel()
		v.mu.Lock()
		v.state, v.frame, v.process = "finished", nil, nil
		v.mu.Unlock()
	}
	return v, stop, nil
}
