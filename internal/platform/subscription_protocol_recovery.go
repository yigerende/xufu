package platform

import (
	"context"
	"encoding/json"
	"net/http"
	"strings"
	"time"

	"xupay/internal/cslive"
)

func (a *App) checkProtocolPayment(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	id := r.PathValue("id")
	action := strings.HasSuffix(r.URL.Path, "/payment/action")
	if action && current(r).Role != "admin" {
		fail(w, 403, "仅管理员可处理付款验证")
		return
	}
	if !a.limit(ctx, "payment_check:"+current(r).ID, 5) {
		fail(w, 429, "核实过于频繁，请稍后重试")
		return
	}
	if action && a.openPreparedPaymentAction(w, r) {
		return
	}
	tx, err := a.DB.Begin(ctx)
	if err != nil {
		paymentError(w, err)
		return
	}
	defer tx.Rollback(ctx)
	if err = a.paymentAdmission(ctx, tx, current(r).ID, a.config(ctx, "payment")); err != nil {
		paymentError(w, err)
		return
	}
	if _, err = tx.Exec(ctx, "SELECT id FROM orders WHERE id=$1 FOR UPDATE", id); err != nil {
		paymentError(w, err)
		return
	}
	var owner, state, attempt, enc, activation, renewal, country, email, currency string
	var keep bool
	var expiry time.Time
	var leaseUntil *time.Time
	var limit int64
	err = tx.QueryRow(ctx, `SELECT o.user_id,p.state,p.attempt_id,s.checkpoint_enc,s.expires_at,s.lease_until,o.activation,o.renewal,o.country,o.email,o.keep_renewal,u.max_amount,u.currency
 FROM subscription_protocol s JOIN orders o ON o.id=s.order_id JOIN payment_runs p ON p.order_id=o.id
 JOIN subscription_runs u ON u.order_id=o.id WHERE o.id=$1 FOR UPDATE OF s,p`, id).
		Scan(&owner, &state, &attempt, &enc, &expiry, &leaseUntil, &activation, &renewal, &country, &email, &keep, &limit, &currency)
	if err != nil || (owner != current(r).ID && !strings.HasPrefix(r.URL.Path, "/api/admin/")) {
		fail(w, 404, "订单不存在")
		return
	}
	if (state != "review" && state != "action_required" && !(state == "succeeded" && (activation == "review" || renewal == "review"))) || (leaseUntil != nil && leaseUntil.After(time.Now())) {
		fail(w, 409, "当前状态无需核实或正在处理中")
		return
	}
	if action && state != "action_required" {
		fail(w, 409, "原交易当前不需要交互验证")
		return
	}
	if enc == "" || !expiry.After(time.Now()) {
		fail(w, 409, "本次办理恢复授权已到期，请联系运营核实；不会再次付款")
		return
	}
	var delegated bool
	if tx.QueryRow(ctx, "SELECT renewal_state='delegated' FROM subscription_runs WHERE order_id=$1", id).Scan(&delegated) != nil || delegated {
		fail(w, 409, "此订单已转续费管理接续，请查看续费管理进度")
		return
	}
	raw, err := a.decrypt(enc)
	var cp cslive.Checkpoint
	if err != nil || json.Unmarshal([]byte(raw), &cp) != nil || cp.ValidateForRecovery() != nil {
		fail(w, 409, "恢复记录不完整，无法自动核实；不会再次付款")
		return
	}
	var bound bool
	if tx.QueryRow(ctx, "SELECT EXISTS(SELECT 1 FROM order_proxy_sessions WHERE order_id=$1 AND endpoint_enc<>'' AND expires_at>now())", id).Scan(&bound) != nil || !bound {
		fail(w, 409, "原订单代理会话已失效，已停止自动核实")
		return
	}
	ttl := int(2*cslive.DefaultConfig().Deadline) + 600 + 120
	proxy, err := a.acquireOrderProxyTx(ctx, tx, id, country, time.Duration(ttl+15)*time.Second)
	if err != nil {
		fail(w, 409, "原订单代理暂不可用，请稍后核实")
		return
	}
	lease := secret()
	if _, err = tx.Exec(ctx, "UPDATE subscription_protocol SET lease_token=$2,lease_until=now()+interval '60 seconds' WHERE order_id=$1", id, lease); err == nil {
		_, err = tx.Exec(ctx, "UPDATE checkout_runs SET proxy_lease_id=$2 WHERE order_id=$1", id, proxy.Lease)
	}
	if err == nil {
		_, err = tx.Exec(ctx, "UPDATE payment_runs SET state=CASE WHEN state='succeeded' THEN state ELSE 'checking' END,expires_at=now()+($2*interval '1 second') WHERE order_id=$1", id, ttl)
	}
	if err == nil {
		err = tx.Commit(ctx)
	}
	if err != nil {
		paymentError(w, err)
		return
	}
	runCtx, cancel := context.WithTimeout(context.Background(), time.Duration(ttl)*time.Second)
	run := &protocolRun{a: a, ctx: runCtx, id: id, attempt: attempt, lease: lease, account: cp.AccountID, email: email, country: country, keep: keep, maxAmount: limit, currency: currency, last: cp}
	s := &paymentSession{attempt: attempt, cancel: cancel}
	var verification *paymentAction
	if action {
		verification = &paymentAction{id: uid(), actor: current(r).ID, state: "preparing", expires: time.Now().Add(10 * time.Minute), cancel: cancel}
	}
	a.paymentMu.Lock()
	if a.payments == nil {
		a.payments = map[string]*paymentSession{}
	}
	a.payments[id] = s
	if action {
		if a.paymentActions == nil {
			a.paymentActions = map[string]*paymentAction{}
		}
		for key, entry := range a.paymentActions {
			entry.mu.Lock()
			expired := entry.expires.Before(time.Now())
			entry.mu.Unlock()
			if expired {
				delete(a.paymentActions, key)
			}
		}
		a.paymentActions[id] = verification
	}
	a.paymentMu.Unlock()
	go func() {
		defer cancel()
		if verification != nil {
			defer func() {
				verification.mu.Lock()
				verification.state = "finished"
				verification.frame = nil
				verification.process = nil
				verification.mu.Unlock()
			}()
		}
		defer func() {
			a.paymentMu.Lock()
			if a.payments[id] == s {
				delete(a.payments, id)
			}
			a.paymentMu.Unlock()
		}()
		done := make(chan struct{})
		go run.heartbeat(runCtx, cancel, done)
		defer func() { cancel(); <-done }()
		client, closeClient, err := run.client(ttl)
		if err != nil {
			run.fail(err)
			return
		}
		defer closeClient()
		cp.Proxy = client.Proxy()
		var result cslive.Result
		if verification != nil {
			result, err = run.continuePaymentAction(client, cslive.Result{Checkpoint: cp}, verification)
		} else {
			result, err = client.Recover(runCtx, cp)
			if err == nil {
				result, err = run.continuePaymentAction(client, result, nil)
			}
		}
		if err != nil {
			run.fail(err)
			return
		}
		run.finish(result)
	}()
	response := M{"accepted": true}
	if verification != nil {
		response["action_id"] = verification.id
	}
	out(w, 202, response)
}
