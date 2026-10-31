package platform

import (
	"context"
	"errors"
	"net/http"
	"net/url"
	"regexp"
	"strconv"
	"strings"
	"time"
	"xupay/internal/cslive"

	"github.com/jackc/pgx/v5"
)

type paymentSession struct {
	protocol *protocolCardSlot
	commands chan paymentCommand
	cancel   context.CancelFunc
	attempt  string
}

var paymentMessages = map[string]string{
	"proxy_ip_missing":                "缺少提链时的出口 IP 记录，已停止继续付款。",
	"proxy_ip_check_failed":           "无法确认付款出口 IP，已停止继续付款。",
	"request_rejected":                "上游拒绝了付款请求，请查看管理员付款诊断。",
	"card_prepare":                    "正在准备银行卡付款方式。",
	"admin_closed":                    "管理员已关闭本站订单并释放冻结服务费，后续办理已停止。此操作不撤销已提交的银行卡付款，原付款记录保留供核实。",
	"checkout_fields_unavailable":     "官方付款页面字段尚未就绪，未继续提交付款。",
	"payment_authorized":              "付款已授权，实际扣款尚未确认；系统不会再次提交付款。",
	"payment_limit_exceeded":          "付款报价金额或币种异常，未提交付款，服务费已解冻。",
	"subscription_preparation_failed": "订阅准备未完成或等待超时，未提交付款，服务费已解冻。",
	"payment_declined":                "付款方式被明确拒绝，本次服务费已解冻，请核对银行卡后重新准备。",
	"network_connection_failed":       "本次办理网络无法连接支付服务，未提交付款，请联系运营检查。",
	"proxy_session_unavailable":       "本次办理网络会话不可用，未继续提交付款，请联系运营核实。",
	"proxy_session_changed":           "本次办理网络出口发生变化，未继续提交付款，请联系运营核实。",
	"proxy_session_expired":           "本次办理网络会话已到期，未继续提交付款。",
	"network_restarted":               "本轮未提交扣款，正在重新准备办理网络和结账信息。",
	"network_check":                   "正在核对本次办理网络。",
	"network_verified":                "本次办理原代理会话可用。",
	"result_check":                    "正在核实付款结果。",
	"opening":                         "正在连接付款页面。", "billing": "正在准备账单资料。", "pricing": "正在核对最终金额与税费。", "confirming": "已收到金额确认，正在提交付款。",
	"quoted": "报价已就绪，请核对含税金额后确认付款。", "succeeded": "上游已确认付款成功，服务费已结算。", "preparation_failed": "付款准备未完成，请检查网络或付款资料后重新准备。",
	"quote_unavailable": "尚未取得有效的含税报价，未提交付款。", "amount_changed": "应付金额已变化，本次未提交付款，请重新准备报价。",
	"verification_required": "需要额外验证，请在官方付款页面完成验证后核实结果。", "result_unconfirmed": "付款结果尚未确认，已停止重复提交，请核实结果。",
	"quote_expired": "报价等待已结束，未提交付款，服务费已解冻。", "runtime_unavailable": "付款运行环境不可用，未提交付款。",
	"unsupported_checkout": "当前链接不支持站内付款，请使用长链接。",
	"cancelled":            "付款准备已取消，未提交付款，服务费已解冻。", "already_paid": "该链接已有付款记录，需要核实，未再次付款。",
}

func validCard(c *cardInput) bool {
	c.Number = strings.NewReplacer(" ", "", "-", "").Replace(c.Number)
	if !regexp.MustCompile(`^\d{12,19}$`).MatchString(c.Number) || !regexp.MustCompile(`^\d{3,4}$`).MatchString(c.CVC) {
		return false
	}
	sum := 0
	double := false
	for i := len(c.Number) - 1; i >= 0; i-- {
		n := int(c.Number[i] - '0')
		if double {
			n *= 2
			if n > 9 {
				n -= 9
			}
		}
		sum += n
		double = !double
	}
	if sum == 0 || sum%10 != 0 {
		return false
	}
	month, e := strconv.Atoi(c.Month)
	if e != nil || month < 1 || month > 12 {
		return false
	}
	year, e := strconv.Atoi(c.Year)
	if e != nil {
		return false
	}
	if len(c.Year) == 2 {
		year += 2000
	}
	if year < time.Now().Year() || year > time.Now().Year()+25 {
		return false
	}
	c.Year = strconv.Itoa(year)
	c.Month = strconv.Itoa(month)
	return time.Date(year, time.Month(month)+1, 1, 0, 0, 0, 0, time.UTC).After(time.Now())
}
func hostedPaymentURL(raw string) bool {
	u, e := url.Parse(raw)
	return e == nil && validCheckoutURL(raw) && (u.Hostname() == "pay.openai.com" || u.Hostname() == "checkout.stripe.com") && regexp.MustCompile(`^/(c/)?pay/cs_(live|test)_[A-Za-z0-9]+$`).MatchString(u.Path)
}
func (a *App) paymentRoutes(m *http.ServeMux) {
	for _, prefix := range []string{"/api/orders/", "/api/v1/orders/"} {
		m.HandleFunc("POST "+prefix+"{id}/payment/prepare", a.require(false, a.preparePayment))
		m.HandleFunc("POST "+prefix+"{id}/payment/confirm", a.require(false, a.confirmPayment))
		m.HandleFunc("POST "+prefix+"{id}/payment/check", a.require(false, a.checkPayment))
	}
	m.HandleFunc("POST /api/admin/orders/{id}/payment/check", a.require(true, a.checkPayment))
	a.actionRoutes(m)
}
func (a *App) paymentDetails(ctx context.Context, id string) M {
	if a.isLinkOnly(ctx, id) {
		return M{"enabled": false}
	}
	rows, e := a.query(ctx, "SELECT attempt_id,state,amount,currency,charged_amount,charged_currency,last4,quote_expires_at,fee_state,error_code,updated_at FROM payment_runs WHERE order_id=$1", id)
	result := M{"enabled": a.orderPaymentEnabled(ctx, id)}
	if a.isProtocolSubscription(ctx, id) {
		result["enabled"] = a.config(ctx, "payment")["enabled"] == true
		var active bool
		if a.DB.QueryRow(ctx, "SELECT COALESCE(lease_until>now(),false) FROM subscription_protocol WHERE order_id=$1", id).Scan(&active) == nil {
			a.paymentMu.Lock()
			interactive := a.paymentActions[id]
			a.paymentMu.Unlock()
			manualRunning := false
			if interactive != nil {
				interactive.mu.Lock()
				manualRunning = interactive.expires.After(time.Now()) && interactive.state != "finished" && interactive.state != "closed"
				interactive.mu.Unlock()
			}
			result["automatic_running"] = active && !manualRunning
		}
		var canCheck bool
		if a.DB.QueryRow(ctx, `SELECT s.checkpoint_enc<>'' AND s.expires_at>now() AND (s.lease_until IS NULL OR s.lease_until<now())
 AND (p.state IN ('review','action_required') OR (p.state='succeeded' AND (o.activation='review' OR o.renewal='review')))
 AND u.renewal_state<>'delegated' AND EXISTS(SELECT 1 FROM order_proxy_sessions b JOIN resources r ON r.id=b.resource_id
 WHERE b.order_id=s.order_id AND b.expires_at>now() AND b.endpoint_enc<>'' AND r.enabled)
 FROM subscription_protocol s JOIN payment_runs p ON p.order_id=s.order_id JOIN orders o ON o.id=s.order_id
 JOIN subscription_runs u ON u.order_id=s.order_id WHERE s.order_id=$1`, id).Scan(&canCheck) == nil {
			result["can_check"] = canCheck
		}
	}
	if e == nil && len(rows) == 1 {
		for k, v := range rows[0] {
			result[k] = v
		}
		result["message"] = paymentMessages[str(result, "error_code")]
		if str(result, "state") == "quoted" {
			result["message"] = paymentMessages["quoted"]
		}
		if a.isSubscription(ctx, id) {
			if str(result, "state") == "quoted" {
				result["message"] = "报价已核实，正在自动办理付款。"
			}
			if str(result, "state") == "action_required" {
				result["message"] = "付款等待官方验证，流程已暂停，请联系运营核实。"
			}
			if result["automatic_running"] == true && (str(result, "state") == "checking" || str(result, "state") == "action_required") {
				result["message"] = "系统正在自动处理付款验证及结果核实，请等待。"
			}
		}
	}
	return result
}
func paymentError(w http.ResponseWriter, e error) {
	var b businessError
	if errors.As(e, &b) {
		fail(w, b.status, b.msg)
	} else {
		fail(w, 503, "付款任务未能提交，请刷新核实")
	}
}
func paymentEventTx(ctx context.Context, tx pgx.Tx, id, stage, code string) error {
	msg := paymentMessages[code]
	if msg == "" {
		msg = paymentMessages["result_unconfirmed"]
	}
	_, e := tx.Exec(ctx, "INSERT INTO order_events(order_id,stage,message) VALUES($1,$2,$3)", id, stage, msg)
	return e
}

// Balance and frozen are both updated in the same transaction as the run.
func paymentFunds(ctx context.Context, tx pgx.Tx, id, attempt, action string) error {
	var userID string
	var fee, balance, frozen int64
	if e := tx.QueryRow(ctx, "SELECT user_id,fee FROM orders WHERE id=$1", id).Scan(&userID, &fee); e != nil {
		return e
	}
	if e := tx.QueryRow(ctx, "SELECT balance,frozen FROM users WHERE id=$1 FOR UPDATE", userID).Scan(&balance, &frozen); e != nil {
		return e
	}
	kind, description, amount := "freeze", "付款准备冻结服务费", int64(0)
	switch action {
	case "hold":
		if balance < fee {
			return bad("余额不足以冻结服务费，请先充值")
		}
		balance -= fee
		frozen += fee
	case "release":
		if frozen < fee {
			return errors.New("invalid frozen balance")
		}
		balance += fee
		frozen -= fee
		kind = "unfreeze"
		description = "付款未完成，解冻服务费"
	case "charge":
		if frozen < fee {
			return errors.New("invalid frozen balance")
		}
		frozen -= fee
		kind = "charge"
		amount = -fee
		description = "付款已确认，结算服务费"
	default:
		return errors.New("invalid funds operation")
	}
	if _, e := tx.Exec(ctx, "INSERT INTO ledger(user_id,kind,amount,balance_after,frozen_after,reference,description,order_id) VALUES($1,$2,$3,$4,$5,$6,$7,$8)", userID, kind, amount, balance, frozen, "payment:"+attempt+":"+action, description, id); e != nil {
		return e
	}
	_, e := tx.Exec(ctx, "UPDATE users SET balance=$2,frozen=$3,updated_at=now() WHERE id=$1", userID, balance, frozen)
	if e != nil {
		return e
	}
	return paymentTraceTx(ctx, tx, id, "funds", "completed", action)
}
func (a *App) paymentAdmission(ctx context.Context, tx pgx.Tx, userID string, cfg M) error {
	if _, e := tx.Exec(ctx, "SELECT pg_advisory_xact_lock(1886002)"); e != nil {
		return e
	}
	var scheduler M
	if e := tx.QueryRow(ctx, "SELECT value FROM settings WHERE key='scheduler'").Scan(&scheduler); e != nil {
		return e
	}
	if scheduler["paused"] == true {
		return bad("队列已暂停，请稍后再试")
	}
	var total, own int
	if e := tx.QueryRow(ctx, "SELECT count(*),count(*) FILTER (WHERE o.user_id=$1) FROM payment_runs p JOIN orders o ON o.id=p.order_id WHERE p.state IN ('preparing','quoted','submitting','checking') OR EXISTS(SELECT 1 FROM subscription_protocol s WHERE s.order_id=p.order_id AND s.lease_until>now())", userID).Scan(&total, &own); e != nil {
		return e
	}
	if total >= num(cfg, "max_parallel", 2) || own >= num(cfg, "per_user_parallel", 1) {
		return businessError{429, "付款通道繁忙，请稍后重试；银行卡资料不会保存"}
	}
	return nil
}
func (a *App) preparePayment(w http.ResponseWriter, r *http.Request) {
	var workflow string
	if e := a.DB.QueryRow(r.Context(), "SELECT workflow FROM orders WHERE id=$1 AND user_id=$2", r.PathValue("id"), current(r).ID).Scan(&workflow); e != nil {
		fail(w, 404, "订单不存在")
		return
	}
	if workflow == "link" {
		fail(w, 409, "此订单仅生成结账链接；如需站内付款，请使用订阅办理")
		return
	}
	if a.isSubscription(r.Context(), r.PathValue("id")) {
		fail(w, 409, "此订单由订阅流程统一办理，请查看进度")
		return
	}
	if !a.paymentEnabled(r.Context()) {
		fail(w, 409, "站内付款暂不可用")
		return
	}
	if !a.limit(r.Context(), "payment:"+current(r).ID, num(a.config(r.Context(), "payment"), "requests_per_minute", 3)) {
		a.event(r, "payment_rate", "medium", "付款准备请求过于频繁")
		fail(w, 429, "操作过于频繁，请稍后重试")
		return
	}
	var v struct {
		Card     cardInput `json:"card"`
		Accepted bool      `json:"accepted"`
	}
	if !read(w, r, &v) {
		return
	}
	if !v.Accepted || !validCard(&v.Card) {
		fail(w, 400, "请核对卡号、有效期、安全码并确认使用授权")
		return
	}
	defer func() { v.Card = cardInput{} }()
	ctx := r.Context()
	cfg := a.config(ctx, "payment")
	id := r.PathValue("id")
	attempt := uid()
	tx, e := a.DB.Begin(ctx)
	if e != nil {
		paymentError(w, e)
		return
	}
	defer tx.Rollback(ctx)
	if e = a.paymentAdmission(ctx, tx, current(r).ID, cfg); e != nil {
		paymentError(w, e)
		return
	}
	var owner, status, enc, country, email string
	var expires *time.Time
	e = tx.QueryRow(ctx, "SELECT o.user_id,o.status,o.country,o.email,c.hosted_url_enc,c.link_expires_at FROM orders o JOIN checkout_runs c ON c.order_id=o.id WHERE o.id=$1 FOR UPDATE OF o", id).Scan(&owner, &status, &country, &email, &enc, &expires)
	if e != nil || owner != current(r).ID {
		fail(w, 404, "订单不存在")
		return
	}
	var oldState string
	var submitted bool
	var feeState string
	e = tx.QueryRow(ctx, "SELECT state,submitted,fee_state FROM payment_runs WHERE order_id=$1 FOR UPDATE", id).Scan(&oldState, &submitted, &feeState)
	if e != nil && e != pgx.ErrNoRows {
		paymentError(w, e)
		return
	}
	if e == nil && (submitted || feeState != "released" || !(oldState == "failed" || oldState == "cancelled")) {
		fail(w, 409, "已有付款任务，请查看进度或核实结果，不要重复提交")
		return
	}
	if status != "awaiting_payment" || expires == nil || !expires.After(time.Now()) {
		fail(w, 409, "结账链接已过期或订单状态不允许付款")
		return
	}
	raw, e := a.decrypt(enc)
	if e != nil || !hostedPaymentURL(raw) {
		fail(w, 409, "请生成长链接后使用站内付款")
		return
	}
	billing, e := a.orderBillingTx(ctx, tx, id)
	if e != nil {
		paymentError(w, e)
		return
	}
	ttl := num(cfg, "prepare_timeout_seconds", 120) + num(cfg, "quote_timeout_seconds", 180) + num(cfg, "result_timeout_seconds", 120) + 30
	_, e = tx.Exec(ctx, `INSERT INTO payment_runs(order_id,attempt_id,state,last4,expires_at,hosted_url_enc) VALUES($1,$2,'preparing',$3,now()+($4*interval '1 second'),$5)
 ON CONFLICT(order_id) DO UPDATE SET attempt_id=EXCLUDED.attempt_id,state='preparing',last4=EXCLUDED.last4,expires_at=EXCLUDED.expires_at,hosted_url_enc=EXCLUDED.hosted_url_enc,amount=NULL,currency='',quote_expires_at=NULL,submitted=false,fee_state='held',error_code='',created_at=now(),updated_at=now()`, id, attempt, v.Card.Number[len(v.Card.Number)-4:], ttl, enc)
	if e == nil {
		e = paymentFunds(ctx, tx, id, attempt, "hold")
	}
	if e == nil {
		_, e = tx.Exec(ctx, "UPDATE orders SET status='processing',updated_at=now() WHERE id=$1", id)
	}
	if e == nil {
		e = paymentEventTx(ctx, tx, id, "payment_preparing", "opening")
	}
	if e == nil {
		e = tx.Commit(ctx)
	}
	if e != nil {
		paymentError(w, e)
		return
	}
	in := paymentInput{Mode: "prepare", URL: raw, Email: email, Card: v.Card, Billing: billing, PrepareSeconds: num(cfg, "prepare_timeout_seconds", 120), QuoteSeconds: num(cfg, "quote_timeout_seconds", 180), ResultSeconds: num(cfg, "result_timeout_seconds", 120)}
	a.launchPayment(id, attempt, in, ttl)
	out(w, 202, M{"accepted": true, "attempt_id": attempt})
}
func (a *App) launchPayment(id, attempt string, in paymentInput, ttl int) {
	ctx, cancel := context.WithTimeout(context.Background(), time.Duration(ttl)*time.Second)
	s := &paymentSession{commands: make(chan paymentCommand, 1), cancel: cancel, attempt: attempt}
	a.paymentMu.Lock()
	if a.payments == nil {
		a.payments = map[string]*paymentSession{}
	}
	a.payments[id] = s
	a.paymentMu.Unlock()
	go func() {
		defer cancel()
		observedSubmission := false
		retryCard, retryBilling := in.Card, in.Billing
		defer func() { retryCard = cardInput{}; retryBilling = nil }()
		restart := func(event paymentEvent) bool {
			if observedSubmission || !in.AutoConfirm || retryCard.Number == "" {
				return false
			}
			ok, err := a.restartPaymentNetwork(ctx, id, attempt, event)
			if err != nil || !ok {
				return false
			}
			a.launchSubscription(id, attempt, retryCard, retryBilling)
			return true
		}
		defer func() {
			a.paymentMu.Lock()
			if a.payments[id] == s {
				delete(a.payments, id)
			}
			a.paymentMu.Unlock()
		}()
		start := a.paymentStart
		if err := a.legacyPaymentInput(ctx, id, attempt, &in); err != nil {
			a.finishPayment(ctx, id, attempt, paymentEvent{State: "unknown", Code: "result_unconfirmed"})
			return
		}
		if in.Executor == paymentExecutorBrowser && browserPaymentURL(in.URL) {
			cookies, err := a.loadBrowserSession(ctx, id)
			if err != nil && (validCustomCheckoutURL(in.URL) || in.ResumeApproval) {
				a.finishPayment(ctx, id, attempt, paymentEvent{State: "failed", Code: "invalid_session"})
				return
			}
			in.SessionCookies = cookies
		}
		if start == nil {
			start = startPaymentNode
			if in.Executor == paymentExecutorLegacyHTTP {
				start = startLegacyPayment
			}
		}
		proxy, expectedIP, proxyExpires, proxyErr := a.paymentOrderProxy(ctx, id, attempt, ttl)
		if proxyErr != nil {
			event := paymentEvent{Type: "result", State: "failed", Code: proxyPaymentCode(proxyErr)}
			if restart(event) {
				return
			}
			a.finishPayment(ctx, id, attempt, event)
			return
		}
		defer a.releaseProxy(context.WithoutCancel(ctx), proxy.Lease)
		bridge, closeBridge, proxyErr := openBrowserProxy(ctx, proxy.Endpoint)
		if proxyErr != nil {
			event := paymentEvent{Type: "result", State: "failed", Code: "proxy_session_unavailable"}
			if restart(event) {
				return
			}
			a.finishPayment(ctx, id, attempt, event)
			return
		}
		defer closeBridge()
		in.Proxy, in.ExpectedIP, in.ProxyExpiresAt = bridge, expectedIP, proxyExpires.UnixMilli()
		in.SkipExitIPCheck = false
		in.Guard = a.legacyPaymentGuard(id, attempt, proxy.Lease)
		paymentGuard := in.Guard
		in.CheckExitIP = func(probeCtx context.Context) error {
			_, err := a.orderExitIP(probeCtx, id, func(ctx context.Context) error {
				if err := paymentGuard(ctx); err != nil {
					return cslive.Failure(err.Error())
				}
				return nil
			}, true)
			if err != nil {
				return errors.New(cslive.ErrorCode(err))
			}
			return nil
		}
		proc, e := start(ctx, in)
		in.Card = cardInput{}
		in.Billing = nil
		in.SessionCookies = ""
		in.Authorization = checkoutInput{}
		in.Checkpoint = legacyPaymentCheckpoint{}
		fallback := func() {
			c, stop := context.WithTimeout(context.Background(), 10*time.Second)
			defer stop()
			a.finishPayment(c, id, attempt, paymentEvent{State: "unknown", Code: "result_unconfirmed"})
		}
		if e != nil {
			fallback()
			return
		}
		defer proc.Close()
		phase := ""
		phaseAt := time.Now()
		closePhase := func(state, code string) {
			if phase != "" {
				a.paymentTrace(ctx, id, phase, state, code, time.Since(phaseAt))
				phase = ""
			}
		}
		defer func() { closePhase("review", "result_unconfirmed") }()
		phaseTimer := time.NewTimer(time.Duration(in.PrepareSeconds) * time.Second)
		defer phaseTimer.Stop()
		for {
			select {
			case <-phaseTimer.C:
				fallback()
				return
			case <-ctx.Done():
				fallback()
				return
			case cmd := <-s.commands:
				phaseTimer.Reset(time.Duration(in.ResultSeconds+15) * time.Second)
				if e = proc.Send(cmd); e != nil {
					fallback()
					return
				}
			case event, ok := <-proc.Events():
				if !ok {
					fallback()
					return
				}
				switch event.Type {
				case "diagnostic":
					d := providerDiagnostic{Order: id, Phase: event.Phase, Code: event.Code, Status: event.HTTPStatus, RequestID: event.RequestID, Elapsed: time.Duration(event.ElapsedMS) * time.Millisecond, At: time.Now().UTC(), Details: event.Diagnostics}
					a.enqueueDiagnostic(d)
				case "stage":
					if event.Code == "network_verified" {
						closePhase("completed", "")
					} else {
						p := map[string]string{"opening": "opening", "billing": "billing", "card_prepare": "card_prepare", "pricing": "pricing", "confirming": "confirming", "approval": "confirming", "network_check": "network", "result_check": "checking"}[event.Code]
						if p != "" {
							closePhase("completed", "")
							phase = p
							phaseAt = time.Now()
							a.paymentTrace(ctx, id, p, "running", "", 0)
						}
					}
					if _, exists := paymentMessages[event.Code]; exists {
						a.DB.Exec(ctx, "INSERT INTO order_events(order_id,stage,message) SELECT $1,'payment_progress',$3 WHERE EXISTS(SELECT 1 FROM payment_runs WHERE order_id=$1 AND attempt_id=$2 AND state IN ('preparing','submitting','checking'))", id, attempt, paymentMessages[event.Code])
					}
				case "quote":
					closePhase("completed", "")
					if e = a.savePaymentQuote(ctx, id, attempt, event, in.QuoteSeconds); e != nil {
						fallback()
						return
					}
					if in.AutoConfirm {
						if !a.paymentExecutorEnabled(ctx, in.Executor) || a.authorizeSubscriptionQuote(ctx, id, attempt, event, in.MaxAmount, in.Currency) != nil {
							a.finishPayment(ctx, id, attempt, paymentEvent{State: "failed", Code: "payment_limit_exceeded"})
							return
						}
						if proc.Send(paymentCommand{Type: "confirm"}) != nil {
							fallback()
							return
						}
						phaseTimer.Reset(time.Duration(in.ResultSeconds+15) * time.Second)
					} else {
						phaseTimer.Reset(time.Duration(in.QuoteSeconds+2) * time.Second)
					}
				case "approval_requested":
					command := "approval_denied"
					if (in.Mode != "check" || in.ResumeApproval) && a.authorizeHostedApproval(ctx, id, attempt, event) == nil {
						command = "approve"
					}
					if proc.Send(paymentCommand{Type: command}) != nil {
						fallback()
						return
					}
				case "submitted":
					observedSubmission = true
					a.paymentTrace(ctx, id, "submitted", "completed", "", 0)
				case "result":
					if observedSubmission {
						event.Sent = true
					}
					traceState := "completed"
					if event.State == "failed" || event.State == "declined" {
						traceState = "failed"
					} else if event.State != "succeeded" {
						traceState = "review"
					}
					closePhase(traceState, event.Code)
					proc.Close()
					if restart(event) {
						return
					}
					if e = a.finishPayment(ctx, id, attempt, event); e != nil {
						fallback()
					}
					return
				}
			}
		}
	}()
}
func (a *App) savePaymentQuote(ctx context.Context, id, attempt string, v paymentEvent, seconds int) error {
	if v.Amount <= 0 || v.Amount > 10000000000 || !strings.Contains("|usd|php|jpy|sgd|gbp|egp|try|", "|"+v.Currency+"|") {
		return errors.New("invalid quote")
	}
	tx, e := a.DB.Begin(ctx)
	if e != nil {
		return e
	}
	defer tx.Rollback(ctx)
	tag, e := tx.Exec(ctx, "UPDATE payment_runs SET state='quoted',amount=$3,currency=$4,quote_expires_at=now()+($5*interval '1 second'),error_code='quoted',updated_at=now() WHERE order_id=$1 AND attempt_id=$2 AND state='preparing' AND expires_at>now()", id, attempt, v.Amount, v.Currency, seconds)
	if e != nil {
		return e
	}
	if tag.RowsAffected() != 1 {
		return ErrLeaseLost
	}
	if e = paymentEventTx(ctx, tx, id, "payment_quoted", "quoted"); e != nil {
		return e
	}
	return tx.Commit(ctx)
}
func (a *App) confirmPayment(w http.ResponseWriter, r *http.Request) {
	var v struct {
		Attempt  string `json:"attempt_id"`
		Amount   int64  `json:"amount"`
		Currency string `json:"currency"`
		Accepted bool   `json:"accepted"`
	}
	if !read(w, r, &v) {
		return
	}
	if !v.Accepted {
		fail(w, 400, "请确认最终金额")
		return
	}
	id := r.PathValue("id")
	ctx := r.Context()
	a.paymentMu.Lock()
	s := a.payments[id]
	a.paymentMu.Unlock()
	if s == nil || s.attempt != v.Attempt {
		fail(w, 409, "付款会话已结束，请刷新订单核实")
		return
	}
	tx, e := a.DB.Begin(ctx)
	if e != nil {
		paymentError(w, e)
		return
	}
	defer tx.Rollback(ctx)
	if _, e = tx.Exec(ctx, "SELECT id FROM orders WHERE id=$1 FOR UPDATE", id); e != nil {
		paymentError(w, e)
		return
	}
	var owner, state, attempt, currency string
	var amount *int64
	var expiry *time.Time
	e = tx.QueryRow(ctx, "SELECT o.user_id,p.state,p.attempt_id,p.amount,p.currency,p.quote_expires_at FROM orders o JOIN payment_runs p ON p.order_id=o.id WHERE o.id=$1 FOR UPDATE OF p", id).Scan(&owner, &state, &attempt, &amount, &currency, &expiry)
	if e != nil || owner != current(r).ID {
		fail(w, 404, "订单不存在")
		return
	}
	if attempt != v.Attempt || amount == nil || *amount != v.Amount || currency != v.Currency {
		fail(w, 409, "金额已变化，请刷新并重新确认")
		return
	}
	if state == "submitting" {
		out(w, 200, M{"accepted": true})
		return
	}
	if state != "quoted" || expiry == nil || !expiry.After(time.Now()) {
		fail(w, 409, "报价已到期或正在处理，请刷新订单")
		return
	}
	if !a.paymentEnabled(ctx) {
		fail(w, 409, "付款已暂停")
		return
	}
	_, e = tx.Exec(ctx, "UPDATE payment_runs SET state='submitting',submitted=true,updated_at=now() WHERE order_id=$1", id)
	if e == nil {
		e = paymentEventTx(ctx, tx, id, "payment_submitting", "confirming")
	}
	if e == nil {
		e = tx.Commit(ctx)
	}
	if e != nil {
		paymentError(w, e)
		return
	}
	select {
	case s.commands <- paymentCommand{Type: "confirm"}:
	default:
	}
	out(w, 202, M{"accepted": true})
}
func (a *App) finishPayment(ctx context.Context, id, attempt string, event paymentEvent) error {
	tx, e := a.DB.Begin(ctx)
	if e != nil {
		return e
	}
	defer tx.Rollback(ctx)
	if _, e = tx.Exec(ctx, "SELECT pg_advisory_xact_lock(1886002)"); e != nil {
		return e
	}
	if _, e = tx.Exec(ctx, "SELECT id FROM orders WHERE id=$1 FOR UPDATE", id); e != nil {
		return e
	}
	if event.ProtocolLease != "" {
		var valid bool
		if e = tx.QueryRow(ctx, "SELECT EXISTS(SELECT 1 FROM subscription_protocol WHERE order_id=$1 AND lease_token=$2 AND lease_until>now())", id, event.ProtocolLease).Scan(&valid); e != nil {
			return e
		}
		if !valid {
			return ErrLeaseLost
		}
	} else {
		var activeProtocol bool
		if e = tx.QueryRow(ctx, "SELECT EXISTS(SELECT 1 FROM subscription_protocol WHERE order_id=$1 AND lease_until>now())", id).Scan(&activeProtocol); e != nil {
			return e
		}
		if activeProtocol {
			return ErrLeaseLost
		}
	}
	var state, feeState, currency string
	var submitted bool
	var amount *int64
	e = tx.QueryRow(ctx, "SELECT state,fee_state,submitted,amount,currency FROM payment_runs WHERE order_id=$1 AND attempt_id=$2 FOR UPDATE", id, attempt).Scan(&state, &feeState, &submitted, &amount, &currency)
	if e != nil {
		return e
	}
	if state == "succeeded" && event.ProtocolLease != "" && event.State == "succeeded" && event.Evidence != nil {
		// Recovery may obtain the original intent's captured amount after payment
		// was already confirmed. Update evidence without touching the ledger.
		if e = a.savePaymentEvidence(ctx, tx, id, attempt, event); e != nil {
			return e
		}
		return tx.Commit(ctx)
	}
	if state == "succeeded" || state == "failed" || state == "cancelled" {
		return nil
	}
	target, code, orderState := event.State, event.Code, "review"
	if err := a.savePaymentEvidence(ctx, tx, id, attempt, event); err != nil {
		if providerCode(err) != "invalid_response" {
			return err
		}
		target, code = "review", "result_unconfirmed"
	}
	declined := target == "declined" && code == "payment_declined"
	if declined {
		target = "failed"
	}
	var protocol bool
	if e = tx.QueryRow(ctx, "SELECT EXISTS(SELECT 1 FROM subscription_protocol WHERE order_id=$1)", id).Scan(&protocol); e != nil {
		return e
	}
	if target == "succeeded" && (!submitted || amount == nil || (event.Amount != *amount && !(protocol && event.Evidence != nil)) || event.Currency != currency) {
		target = "review"
		code = "result_unconfirmed"
	}
	if target == "unknown" {
		if submitted {
			target = "review"
			code = "result_unconfirmed"
		} else {
			target = "failed"
			code = "preparation_failed"
		}
	}
	if (target == "failed" || target == "cancelled") && ((event.Sent && !declined) || (state == "checking" && !(protocol && declined))) {
		target = "review"
		code = "result_unconfirmed"
	}
	switch target {
	case "succeeded":
		orderState = "succeeded"
		code = "succeeded"
	case "failed", "cancelled":
		orderState = "awaiting_payment"
	case "action_required":
		code = "verification_required"
	case "review":
		if code != "payment_authorized" {
			code = "result_unconfirmed"
		}
	default:
		target = "review"
		code = "result_unconfirmed"
	}
	if _, ok := paymentMessages[code]; !ok {
		code = "preparation_failed"
	}
	if feeState == "held" && (target == "succeeded" || target == "failed" || target == "cancelled") {
		action := "release"
		feeState = "released"
		if target == "succeeded" {
			action = "charge"
			feeState = "charged"
		}
		if e = paymentFunds(ctx, tx, id, attempt, action); e != nil {
			return e
		}
	}
	// Explicit no-submit failure may restart preparation; uncertain submission never can.
	if target == "failed" || target == "cancelled" {
		submitted = false
	}
	var workflow string
	var keep bool
	if e = tx.QueryRow(ctx, "SELECT workflow,keep_renewal FROM orders WHERE id=$1", id).Scan(&workflow, &keep); e != nil {
		return e
	}
	if workflow == "subscription" {
		if target == "succeeded" || target == "failed" || target == "cancelled" {
			if _, e = tx.Exec(ctx, "UPDATE checkout_runs SET credentials_enc='' WHERE order_id=$1", id); e != nil {
				return e
			}
		}
		if target == "succeeded" {
			orderState = "processing"
			if _, e = tx.Exec(ctx, "UPDATE subscription_runs SET renewal_state='pending',next_at=now(),updated_at=now() WHERE order_id=$1 AND renewal_state='not_started' AND NOT EXISTS(SELECT 1 FROM subscription_protocol WHERE order_id=$1)", id); e != nil {
				return e
			}
		} else if target == "failed" || target == "cancelled" {
			orderState = target
			if _, e = tx.Exec(ctx, "UPDATE subscription_runs SET credentials_enc='',renewal_state='not_started',updated_at=now() WHERE order_id=$1", id); e != nil {
				return e
			}
			if _, e = tx.Exec(ctx, "UPDATE jobs SET state='failed',error_code='cancelled' WHERE order_id=$1 AND state='queued'", id); e != nil {
				return e
			}
		}
	}
	_, e = tx.Exec(ctx, "UPDATE payment_runs SET state=$3,fee_state=$4,error_code=$5,submitted=$6,hosted_url_enc=CASE WHEN $3 IN ('succeeded','failed','cancelled') THEN '' ELSE hosted_url_enc END,legacy_checkpoint_enc=CASE WHEN $3 IN ('succeeded','failed','cancelled') THEN '' ELSE legacy_checkpoint_enc END,updated_at=now() WHERE order_id=$1 AND attempt_id=$2", id, attempt, target, feeState, code, submitted)
	if e == nil {
		_, e = tx.Exec(ctx, "UPDATE orders SET status=$2,updated_at=now() WHERE id=$1", id, orderState)
	}
	if e == nil {
		e = paymentEventTx(ctx, tx, id, "payment_"+target, code)
	}
	if e != nil {
		return e
	}
	traceState := "review"
	if target == "succeeded" {
		traceState = "completed"
	} else if target == "failed" || target == "cancelled" {
		traceState = "failed"
	}
	if e = paymentTraceTx(ctx, tx, id, "result", traceState, code); e != nil {
		return e
	}
	if workflow == "subscription" && target == "succeeded" && keep {
		if e = paymentTraceTx(ctx, tx, id, "renewal", "completed", "kept"); e != nil {
			return e
		}
	}
	if workflow == "subscription" && (orderState == "succeeded" || orderState == "failed" || orderState == "cancelled") {
		if e = paymentTraceTx(ctx, tx, id, "complete", traceState, code); e != nil {
			return e
		}
	}
	return tx.Commit(ctx)
}
func (a *App) checkPayment(w http.ResponseWriter, r *http.Request) {
	if a.isProtocolSubscription(r.Context(), r.PathValue("id")) {
		a.checkProtocolPayment(w, r)
		return
	}
	ctx := r.Context()
	id := r.PathValue("id")
	cfg := a.config(ctx, "payment")
	if !a.orderPaymentEnabled(ctx, id) {
		fail(w, 409, "付款运行环境不可用")
		return
	}
	if !a.limit(ctx, "payment_check:"+current(r).ID, 5) {
		fail(w, 429, "核实过于频繁，请稍后重试")
		return
	}
	tx, e := a.DB.Begin(ctx)
	if e != nil {
		paymentError(w, e)
		return
	}
	defer tx.Rollback(ctx)
	if e = a.paymentAdmission(ctx, tx, current(r).ID, cfg); e != nil {
		paymentError(w, e)
		return
	}
	if _, e = tx.Exec(ctx, "SELECT id FROM orders WHERE id=$1 FOR UPDATE", id); e != nil {
		paymentError(w, e)
		return
	}
	var owner, state, attempt, enc string
	e = tx.QueryRow(ctx, "SELECT o.user_id,p.state,p.attempt_id,p.hosted_url_enc FROM payment_runs p JOIN orders o ON o.id=p.order_id WHERE p.order_id=$1 FOR UPDATE OF p", id).Scan(&owner, &state, &attempt, &enc)
	if e != nil || (owner != current(r).ID && !strings.HasPrefix(r.URL.Path, "/api/admin/")) {
		fail(w, 404, "订单不存在")
		return
	}
	if state != "review" && state != "action_required" {
		fail(w, 409, "当前状态无需核实")
		return
	}
	// Read-only verification must reuse the original order's proxy session too.
	raw, e := a.decrypt(enc)
	if e != nil {
		paymentError(w, e)
		return
	}
	resume := strings.HasPrefix(r.URL.Path, "/api/admin/") && r.URL.Query().Get("continue_approval") == "true"
	var savedQuote M
	if e = tx.QueryRow(ctx, "SELECT jsonb_build_object('amount',amount,'currency',currency) FROM payment_runs WHERE order_id=$1", id).Scan(&savedQuote); e != nil {
		paymentError(w, e)
		return
	}
	// Legacy recovery is always read-only, including the historical approval
	// query flag. Browser approval is no longer part of this executor.
	if resume {
		fail(w, 409, "旧付款仅支持核实原交易；请在官方页面完成额外验证后核实")
		return
	}
	ttl := num(cfg, "prepare_timeout_seconds", 120) + 30
	_, e = tx.Exec(ctx, "UPDATE payment_runs SET state='checking',expires_at=now()+($2*interval '1 second'),updated_at=now() WHERE order_id=$1", id, ttl)
	if e == nil {
		e = tx.Commit(ctx)
	}
	if e != nil {
		paymentError(w, e)
		return
	}
	a.launchPayment(id, attempt, paymentInput{Mode: "check", URL: raw, PrepareSeconds: num(cfg, "prepare_timeout_seconds", 120), ResumeApproval: resume, SavedQuote: savedQuote}, ttl)
	out(w, 202, M{"accepted": true})
}
func (a *App) RecoverPayments(ctx context.Context) error {
	if err := a.recoverProtocolRuns(ctx); err != nil {
		return err
	}
	rows, e := a.query(ctx, "SELECT order_id,attempt_id FROM payment_runs p WHERE state IN ('preparing','quoted','submitting','checking') AND expires_at<now() AND NOT EXISTS(SELECT 1 FROM subscription_protocol s WHERE s.order_id=p.order_id) LIMIT 100")
	if e != nil {
		return e
	}
	for _, r := range rows {
		if e = a.finishPayment(ctx, str(r, "order_id"), str(r, "attempt_id"), paymentEvent{State: "unknown", Code: "result_unconfirmed"}); e != nil {
			return e
		}
	}
	return nil
}
func (a *App) RunPayments(ctx context.Context) {
	t := time.NewTicker(5 * time.Second)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			a.paymentMu.Lock()
			for _, s := range a.payments {
				s.cancel()
			}
			a.paymentMu.Unlock()
			return
		case <-t.C:
			a.RecoverPayments(ctx)
		}
	}
}
