package platform

import "xupay/internal/cslive"

// Like the reference CLI, keep action_required until an operator opens the
// official SDK continuation. Never silently start an unattended challenge.
func (r *protocolRun) continuePaymentAction(client protocolPaymentClient, prior cslive.Result, v *paymentAction) (cslive.Result, error) {
	cp := prior.Checkpoint
	if cp.Payment != "action_required" || v == nil {
		return prior, nil
	}
	if err := r.save(cp); err != nil {
		return prior, err
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
	if err = r.runPaymentAction(cp, v); err != nil {
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
