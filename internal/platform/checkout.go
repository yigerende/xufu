package platform

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"
)

func (a *App) enqueueCheckout(ctx context.Context, tx pgx.Tx, id string, raw json.RawMessage, workspace string, premium int, flow, checkoutType string) error {
	if !validSubscriptionFlow(flow) {
		return bad("提链流程配置无效")
	}
	if !validCheckoutType(checkoutType) && !(flow == subscriptionFlowCSLive && checkoutType == "auto") {
		return bad("提链类型配置无效")
	}
	info, e := inspectSessionMode(raw, flow == subscriptionFlowCSLive)
	if e != nil {
		return e
	}
	if time.Until(info.ExpiresAt) < time.Minute {
		return bad("Session 即将过期，请重新获取")
	}
	credentials, deadline, e := sessionCredentials(raw, info)
	if e != nil {
		return e
	}
	workspace = strings.TrimSpace(workspace)
	if workspace == "" {
		workspace = "My workspace"
	}
	if len(workspace) > 100 || premium < 1 || premium > 100 {
		return bad("团队名称或高级席位无效")
	}
	var cfg M
	if e = tx.QueryRow(ctx, "SELECT value FROM settings WHERE key='scheduler'").Scan(&cfg); e != nil {
		return e
	}
	if str(cfg, "engine") != "checkout" {
		return bad("结账准备暂未开放")
	}
	// Serialize queue admission separately from order identities.
	if _, e = tx.Exec(ctx, "SELECT pg_advisory_xact_lock(1886002)"); e != nil {
		return e
	}
	var n int
	if e = tx.QueryRow(ctx, "SELECT count(*) FROM jobs WHERE state IN ('queued','running')").Scan(&n); e != nil {
		return e
	}
	if n >= num(cfg, "queue_limit", 500) {
		return businessError{429, "准备队列已满，请稍后再试"}
	}
	minimal, _ := json.Marshal(credentials)
	encrypted, e := a.encrypt(string(minimal))
	if e != nil {
		return e
	}
	if _, e = tx.Exec(ctx, "INSERT INTO checkout_runs(order_id,credentials_enc,credentials_expires_at,workspace_name,premium_seats,flow,checkout_type) VALUES($1,$2,$3,$4,$5,$6,$7)", id, encrypted, deadline, workspace, premium, flow, checkoutType); e != nil {
		return e
	}
	var userID string
	if e = tx.QueryRow(ctx, "SELECT user_id FROM orders WHERE id=$1", id).Scan(&userID); e != nil {
		return e
	}
	if _, e = tx.Exec(ctx, "INSERT INTO jobs(id,order_id,user_id,kind,max_attempts) VALUES($1,$2,$3,'checkout',$4)", uid(), id, userID, 1+num(cfg, "retry_limit", 2)); e != nil {
		return e
	}
	if _, e = tx.Exec(ctx, "UPDATE orders SET status='queued',updated_at=now() WHERE id=$1", id); e != nil {
		return e
	}
	if e = paymentTraceTx(ctx, tx, id, "accepted", "completed", ""); e != nil {
		return e
	}
	if e = paymentTraceTx(ctx, tx, id, "queue", "waiting", ""); e != nil {
		return e
	}
	_, e = tx.Exec(ctx, "INSERT INTO order_events(order_id,stage,message) VALUES($1,'queued','已排队准备结账链接，等待可用代理；本阶段不扣费、不提交银行卡付款。')", id)
	return e
}

func (a *App) isLinkOnly(ctx context.Context, id string) bool {
	var workflow string
	return a.DB.QueryRow(ctx, "SELECT workflow FROM orders WHERE id=$1", id).Scan(&workflow) == nil && workflow == "link"
}

func (a *App) checkoutDetails(ctx context.Context, id string) M {
	if a.isSubscription(ctx, id) {
		return M{"hidden": true, "ready": false}
	}
	result := M{"ready": false, "payment_connected": !a.isLinkOnly(ctx, id) && a.paymentEnabled(ctx)}
	var jobState, code string
	var next time.Time
	var attempts int
	if e := a.DB.QueryRow(ctx, "SELECT state,error_code,next_at,attempts FROM jobs WHERE order_id=$1 AND kind='checkout'", id).Scan(&jobState, &code, &next, &attempts); e == nil {
		result["attempts"] = attempts
		if jobState == "queued" {
			result["next_check_at"] = next
			result["waiting_message"] = "排队准备结账链接"
			if code == "waiting_proxy" {
				result["waiting_message"] = "等待对应订阅地区的可用代理，恢复后自动继续"
			} else if attempts > 0 {
				result["waiting_message"] = "等待配置的重试间隔"
			}
		}
	}
	var enc string
	var expires *time.Time
	e := a.DB.QueryRow(ctx, "SELECT hosted_url_enc,link_expires_at FROM checkout_runs WHERE order_id=$1", id).Scan(&enc, &expires)
	if e != nil {
		return result
	}
	if expires != nil && !expires.After(time.Now()) {
		result["expired"] = true
		return result
	}
	if expires == nil || enc == "" {
		return result
	}
	raw, e := a.decrypt(enc)
	if e != nil || !validCheckoutURL(raw) {
		return result
	}
	result["ready"], result["url"], result["expires_at"] = true, raw, expires
	return result
}

func (a *App) finishCheckout(ctx context.Context, j *Job, result checkoutOutput, cause error) error {
	tx, e := a.DB.Begin(ctx)
	if e != nil {
		return e
	}
	defer tx.Rollback(ctx)
	if _, e = tx.Exec(ctx, "SELECT pg_advisory_xact_lock(1886002)"); e != nil {
		return e
	}
	var attempts, max int
	if e = tx.QueryRow(ctx, "SELECT attempts,max_attempts FROM jobs WHERE id=$1 AND lease_token=$2 AND state='running' AND lease_until>now() FOR UPDATE", j.ID, j.Lease).Scan(&attempts, &max); e != nil {
		if e == pgx.ErrNoRows {
			return ErrLeaseLost
		}
		return e
	}
	state, status, code, msg := "completed", "awaiting_payment", "link_ready", "结账链接已生成；尚未提交付款，未收取服务费。"
	var workflow, paymentState, flow, checkoutType string
	if e = tx.QueryRow(ctx, "SELECT o.workflow,COALESCE(p.state,''),c.flow,c.checkout_type FROM orders o JOIN checkout_runs c ON c.order_id=o.id LEFT JOIN payment_runs p ON p.order_id=o.id WHERE o.id=$1", j.OrderID).Scan(&workflow, &paymentState, &flow, &checkoutType); e != nil {
		return e
	}
	var cfg M
	if e = tx.QueryRow(ctx, "SELECT value FROM settings WHERE key='scheduler'").Scan(&cfg); e != nil {
		return e
	}
	delay := num(cfg, "retry_delay_seconds", 30)
	if cause != nil {
		var networkFailure checkoutError
		if errors.As(cause, &networkFailure) && !networkFailure.Unknown {
			if restarted, err := a.restartCheckoutNetworkTx(ctx, tx, j.OrderID, networkFailure.Code); err != nil {
				return err
			} else if restarted {
				return tx.Commit(ctx)
			}
		}
		state, status, code = "failed", "failed", "preparation_failed"
		msg = "结账准备未完成，请核对信息后重新创建申请。"
		var p checkoutError
		if errors.As(cause, &p) {
			code = p.Code
			switch p.Code {
			case "invalid_session":
				msg = "账号授权已失效，请重新获取 Session。"
			case "proxy_country_mismatch":
				msg = "代理出口与所选订阅地区不符，等待重新检查。"
			case "proxy_session_changed", "proxy_session_expired":
				msg = "本次办理网络会话已变化或失效，已停止继续提交。"
			case "access_denied":
				msg = "上游拒绝了请求，需要人工核对账号或网络。"
			case "network", "upstream_unavailable", "upstream_busy":
				msg = "上游连接暂时不可用。"
			}
			if p.Unknown {
				state, status = "review", "review"
				msg = "链接创建结果未能确认，已停止重复提交；未执行银行卡付款。"
			} else if p.Retry && attempts < max {
				state, status = "queued", "queued"
				msg += " 将按配置间隔重试。"
			}
		}
	}
	if cause == nil {
		if !validHostedCheckoutURL(result.URL) && !((checkoutType == "oaics" || flow == subscriptionFlowCSLive && checkoutType == "auto") && validCustomCheckoutURL(result.URL)) {
			return errors.New("invalid checkout result")
		}
		encrypted, err := a.encrypt(result.URL)
		if err != nil {
			return err
		}
		if workflow == "subscription" && paymentState == "preparing" && result.Credentials != nil {
			raw, err := json.Marshal(result.Credentials)
			if err != nil {
				return err
			}
			sealed, err := a.encrypt(string(raw))
			if err != nil {
				return err
			}
			if _, err = tx.Exec(ctx, "UPDATE checkout_runs SET credentials_enc=$2 WHERE order_id=$1", j.OrderID, sealed); err != nil {
				return err
			}
			if _, err = tx.Exec(ctx, "UPDATE subscription_runs SET credentials_enc=$2,updated_at=now() WHERE order_id=$1", j.OrderID, sealed); err != nil {
				return err
			}
		}
		if _, e = tx.Exec(ctx, "UPDATE checkout_runs SET hosted_url_enc=$2,link_expires_at=now()+interval '30 minutes',credentials_enc=CASE WHEN $3 THEN credentials_enc ELSE '' END WHERE order_id=$1", j.OrderID, encrypted, workflow == "subscription" && paymentState == "preparing"); e != nil {
			return e
		}
		if _, e = tx.Exec(ctx, `INSERT INTO order_checkout_links(order_id,round,url_enc,generated_at,expires_at)
 SELECT c.order_id,COALESCE(p.network_restarts,0)+1,$2,now(),c.link_expires_at
 FROM checkout_runs c LEFT JOIN payment_runs p ON p.order_id=c.order_id WHERE c.order_id=$1
 ON CONFLICT(order_id,round) DO NOTHING`, j.OrderID, encrypted); e != nil {
			return e
		}
	} else if state != "queued" {
		if _, e = tx.Exec(ctx, "UPDATE checkout_runs SET credentials_enc='' WHERE order_id=$1", j.OrderID); e != nil {
			return e
		}
	}
	if workflow == "subscription" {
		if cause == nil {
			status = "processing"
			msg = "订阅准备完成，正在继续办理付款。"
		} else {
			msg = "订阅准备尚未完成，请根据订单状态查看后续处理。"
		}
		if paymentState == "failed" || paymentState == "cancelled" {
			status = paymentState
			msg = "本次订阅流程已结束，未继续提交付款。"
		}
	}
	if _, e = tx.Exec(ctx, "UPDATE jobs SET state=$3,error_code=$4,next_at=now()+($5*interval '1 second'),lease_token=NULL,lease_until=NULL,side_effect_started=CASE WHEN $3='queued' THEN false ELSE side_effect_started END,updated_at=now() WHERE id=$1 AND lease_token=$2", j.ID, j.Lease, state, code, delay); e != nil {
		return e
	}
	if _, e = tx.Exec(ctx, "UPDATE orders SET status=$2,updated_at=now() WHERE id=$1", j.OrderID, status); e != nil {
		return e
	}
	if _, e = tx.Exec(ctx, "INSERT INTO order_events(order_id,stage,message) VALUES($1,$2,$3)", j.OrderID, status, msg); e != nil {
		return e
	}
	if cause == nil && workflow == "subscription" && paymentState == "preparing" {
		_, e = tx.Exec(ctx, "UPDATE proxy_leases SET expires_at=(SELECT expires_at+interval '15 seconds' FROM payment_runs WHERE order_id=$1) WHERE id=(SELECT proxy_lease_id FROM checkout_runs WHERE order_id=$1)", j.OrderID)
	} else {
		_, e = tx.Exec(ctx, "DELETE FROM proxy_leases WHERE id=(SELECT proxy_lease_id FROM checkout_runs WHERE order_id=$1)", j.OrderID)
	}
	if e != nil {
		return e
	}
	if state == "queued" {
		if e = paymentTraceTx(ctx, tx, j.OrderID, "queue", "waiting", "retry"); e != nil {
			return e
		}
	}
	return tx.Commit(ctx)
}

func (a *App) executeCheckout(ctx context.Context, j *Job) {
	if a.isProtocolSubscription(ctx, j.OrderID) {
		a.executeProtocolSubscription(ctx, j)
		return
	}
	started := time.Now()
	a.paymentTrace(ctx, j.OrderID, "checkout", "running", "", 0)
	var enc, proxyEnc, flow, workflow string
	var expiry, proxyExpiry time.Time
	in := checkoutInput{}
	e := a.DB.QueryRow(ctx, `SELECT c.credentials_enc,c.credentials_expires_at,c.workspace_name,c.premium_seats,o.product_id,o.country,o.seats,s.endpoint_enc,s.expires_at,c.flow,o.workflow,c.checkout_type FROM checkout_runs c JOIN orders o ON o.id=c.order_id JOIN order_proxy_sessions s ON s.order_id=o.id WHERE c.order_id=$1`, j.OrderID).Scan(&enc, &expiry, &in.Workspace, &in.PremiumSeats, &in.Product, &in.Country, &in.Seats, &proxyEnc, &proxyExpiry, &flow, &workflow, &in.CheckoutType)
	failJob := func(err error) {
		var failure checkoutError
		errors.As(err, &failure)
		a.paymentTrace(ctx, j.OrderID, "checkout", "failed", failure.Code, time.Since(started))
		a.finishCheckout(ctx, j, checkoutOutput{}, err)
	}
	if e != nil {
		failJob(checkoutFailure("preparation_failed", false, false))
		return
	}
	if !expiry.After(time.Now()) {
		failJob(checkoutFailure("invalid_session", false, false))
		return
	}
	if !proxyExpiry.After(time.Now()) {
		failJob(checkoutFailure("proxy_session_expired", false, false))
		return
	}
	raw, e := a.decrypt(enc)
	if e != nil {
		failJob(checkoutFailure("invalid_session", false, false))
		return
	}
	var credentials checkoutInput
	if json.Unmarshal([]byte(raw), &credentials) != nil {
		failJob(checkoutFailure("invalid_session", false, false))
		return
	}
	in.AccessToken, in.AccountID = credentials.AccessToken, credentials.AccountID
	in.SessionToken, in.Cookies, in.Email = credentials.SessionToken, credentials.Cookies, credentials.Email
	in.TokenExpiresAt, in.RefreshedAt, in.RefreshPurpose = credentials.TokenExpiresAt, credentials.RefreshedAt, credentials.RefreshPurpose
	if in.Email == "" {
		a.DB.QueryRow(ctx, "SELECT email FROM orders WHERE id=$1", j.OrderID).Scan(&in.Email)
	}
	in.ObserveIP = func(ip string) error {
		err := a.recordOrderProxyIP(ctx, j, ip)
		if err == nil {
			a.paymentTrace(ctx, j.OrderID, "network", "completed", "", 0)
		}
		return err
	}
	cfg := a.config(ctx, "scheduler")
	in.Mode = "long"
	if in.CheckoutType == "oaics" {
		in.Mode = "short"
	}
	endpoint, e := a.decrypt(proxyEnc)
	if e != nil {
		failJob(checkoutFailure("proxy_unavailable", false, false))
		return
	}
	ttl := num(cfg, "task_timeout_seconds", 180)
	if flow == subscriptionFlowCSLive {
		ttl = max(ttl, 255)
	}
	task, cancel := context.WithTimeout(ctx, time.Duration(ttl)*time.Second)
	defer cancel()
	heartbeatDone := make(chan struct{})
	go func() {
		defer close(heartbeatDone)
		tick := time.NewTicker(5 * time.Second)
		defer tick.Stop()
		for {
			select {
			case <-task.Done():
				return
			case <-tick.C:
				if a.Heartbeat(task, j) != nil {
					cancel()
					return
				}
			}
		}
	}()
	result, cause := a.runCheckoutFlow(task, j, endpoint, in, flow, workflow)
	cancel()
	<-heartbeatDone
	// Finalization gets its own short context even when the request timed out.
	final, c := context.WithTimeout(context.WithoutCancel(ctx), 10*time.Second)
	defer c()
	a.finishCheckout(final, j, result, cause)
	traceState, code := "completed", ""
	if cause != nil {
		traceState = "failed"
		var failure checkoutError
		if errors.As(cause, &failure) {
			code = failure.Code
		}
		if failure.Unknown {
			traceState = "review"
		}
	}
	a.paymentTrace(final, j.OrderID, "checkout", traceState, code, time.Since(started))
}

func (a *App) runCheckoutFlow(ctx context.Context, j *Job, endpoint string, in checkoutInput, flow, workflow string) (checkoutOutput, error) {
	if flow == subscriptionFlowCSLive {
		return a.generateProtocolCheckout(ctx, j, endpoint, in, workflow == "subscription")
	}
	if flow != subscriptionFlowLegacy {
		return checkoutOutput{}, checkoutFailure("preparation_failed", false, false)
	}
	factory := newLegacyCheckoutClient
	if a.legacyCheckoutClient != nil {
		factory = a.legacyCheckoutClient
	}
	client, closeClient, err := factory(ctx, endpoint)
	if err != nil {
		return checkoutOutput{}, err
	}
	defer closeClient()
	// Check the original proxy lease before each request without probing its exit.
	guard := &protocolCheckoutRun{a: a, job: j, country: in.Country}
	guarded := *client
	guarded.Transport = &legacyCheckoutGuard{base: client.Transport, guard: guard}
	// generator.py uses the supplied access token directly for both modes.
	return generateCheckout(a.providerContext(ctx, j.OrderID, "checkout"), &guarded, in, func() error { return a.BeforeSideEffect(ctx, j) })
}

func (a *App) RunCheckout(ctx context.Context) {
	tick := time.NewTicker(time.Second)
	defer tick.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-tick.C:
			if str(a.config(ctx, "scheduler"), "engine") != "checkout" {
				continue
			}
			// Claim enforces limits in PostgreSQL, including other processes.
			for i := 0; i < 10; i++ {
				j, e := a.claim(ctx, "checkout")
				if e != nil || j == nil {
					break
				}
				go a.executeCheckout(ctx, j)
			}
		}
	}
}
