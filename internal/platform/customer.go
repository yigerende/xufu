package platform

import (
	"encoding/json"
	"fmt"
	"github.com/jackc/pgx/v5"
	"net/http"
	"strings"
)

func (a *App) overview(w http.ResponseWriter, r *http.Request) {
	u := current(r)
	stats := M{}
	var total, success, pending int
	var used, credit int64
	e := a.DB.QueryRow(r.Context(), "SELECT count(*),count(*) FILTER(WHERE status='succeeded'),count(*) FILTER(WHERE status IN ('draft','queued','processing','awaiting_payment') AND NOT(workflow='link' AND status='awaiting_payment')) FROM orders WHERE user_id=$1", u.ID).Scan(&total, &success, &pending)
	if e != nil {
		fail(w, 500, "读取失败")
		return
	}
	a.DB.QueryRow(r.Context(), "SELECT COALESCE(sum(-amount) FILTER(WHERE kind='charge'),0),COALESCE(sum(amount) FILTER(WHERE kind IN ('topup','redeem')),0) FROM ledger WHERE user_id=$1", u.ID).Scan(&used, &credit)
	stats["orders"] = total
	stats["succeeded"] = success
	stats["pending"] = pending
	stats["used"] = used
	stats["credited"] = credit
	orders, _ := a.query(r.Context(), "SELECT id,product_id,product_name,email,country,seats,status,fee,source,workflow,keep_renewal,renewal,activation,created_at,updated_at FROM orders WHERE user_id=$1 ORDER BY created_at DESC LIMIT 5", u.ID)
	ann, _ := a.query(r.Context(), "SELECT id,title,body,tone FROM announcements WHERE published AND (starts_at IS NULL OR starts_at<=now()) AND (ends_at IS NULL OR ends_at>now()) ORDER BY sort,created_at DESC")
	out(w, 200, M{"user": u, "stats": stats, "recent_orders": orders, "announcements": ann})
}
func (a *App) orders(w http.ResponseWriter, r *http.Request) {
	a.list(w, r, customerOrdersQuery, current(r).ID, r.URL.Query().Get("q"), r.URL.Query().Get("status"))
}
func (a *App) createOrder(w http.ResponseWriter, r *http.Request) {
	var v struct {
		Product         string          `json:"product_id"`
		Session         json.RawMessage `json:"session"`
		Country         string          `json:"country"`
		Seats           int             `json:"seats"`
		Accepted        bool            `json:"accepted"`
		Prepare         bool            `json:"prepare"`
		Workspace       string          `json:"workspace_name"`
		PremiumSeats    int             `json:"premium_seats"`
		Workflow        string          `json:"workflow"`
		Card            cardInput       `json:"card"`
		KeepRenewal     bool            `json:"keep_renewal"`
		PaymentLimit    int64           `json:"-"`
		PaymentCurrency string          `json:"-"`
	}
	if !read(w, r, &v) {
		return
	}
	defer func() { v.Card = cardInput{} }()
	full := v.Workflow == "subscription"
	if v.Workflow != "" && v.Workflow != "checkout" && !full {
		fail(w, 400, "办理类型无效")
		return
	}
	if full {
		v.Prepare = true
		v.PaymentCurrency = currencyForCountry(v.Country)
		v.PaymentLimit = 10000000000
		if !validCard(&v.Card) || v.PaymentCurrency == "" {
			fail(w, 400, "请核对银行卡及订阅地区")
			return
		}
	}
	// Parse identity before idempotency lookup without tying a replay to today's
	// admin setting. New orders are validated against the locked choice below.
	sessionInfo, sessionErr := inspectSession(v.Session)
	if sessionErr != nil && v.Prepare {
		sessionInfo, sessionErr = inspectSessionMode(v.Session, true)
	}
	if sessionErr != nil {
		fail(w, 400, sessionErr.Error())
		return
	}
	if !v.Accepted {
		fail(w, 400, "请确认服务条款")
		return
	}
	if !strings.Contains("|PH|US|JP|SG|GB|EG|TR|", "|"+v.Country+"|") {
		fail(w, 400, "地区不支持")
		return
	}
	if v.Seats < 1 || v.Seats > 100 || (strings.HasPrefix(v.Product, "team") && v.Seats < 2) {
		fail(w, 400, "席位须为 1～100")
		return
	}
	key := r.Header.Get("Idempotency-Key")
	if len(key) < 8 || len(key) > 120 {
		fail(w, 400, "请提供 8～120 位 Idempotency-Key")
		return
	}
	u := current(r)
	cfg := a.config(r.Context(), "risk")
	if !a.limit(r.Context(), "order:"+u.ID, num(cfg, "order_per_minute", 5)) {
		a.event(r, "order_rate", "medium", "创建订单过于频繁")
		fail(w, 429, "创建频率超过限制")
		return
	}
	identity := M{"product_id": v.Product, "email": sessionInfo.Email, "account_id": sessionInfo.AccountID, "country": v.Country, "seats": v.Seats, "accepted": v.Accepted}
	if v.PremiumSeats == 0 {
		v.PremiumSeats = 1
	}
	if v.Prepare {
		identity["prepare"] = true
		identity["workspace_name"] = v.Workspace
		identity["premium_seats"] = v.PremiumSeats
	}
	if full {
		identity["workflow"] = "subscription"
		identity["keep_renewal"] = v.KeepRenewal
	}
	b, _ := json.Marshal(identity)
	reqHash := hash(string(b))
	var subscriptionID, subscriptionAttempt string
	var subscriptionBilling M
	committed := a.transaction(w, r, func(tx pgx.Tx) (any, error) {
		if _, e := tx.Exec(r.Context(), "SELECT pg_advisory_xact_lock(1886002)"); e != nil {
			return nil, e
		}
		var dummy string
		if e := tx.QueryRow(r.Context(), "SELECT id FROM users WHERE id=$1 FOR UPDATE", u.ID).Scan(&dummy); e != nil {
			return nil, e
		}
		var existing, oldHash, oldStatus string
		if e := tx.QueryRow(r.Context(), "SELECT id,request_hash,status FROM orders WHERE user_id=$1 AND idempotency_key=$2", u.ID, key).Scan(&existing, &oldHash, &oldStatus); e == nil {
			if oldHash != reqHash {
				return nil, businessError{409, "同一请求标识不能用于不同订单"}
			}
			return M{"id": existing, "status": oldStatus, "replayed": true}, nil
		} else if e != pgx.ErrNoRows {
			return nil, e
		}
		var n int
		flow, paymentFlow := "", ""
		checkoutType := "hosted"
		if v.Prepare {
			var paymentConfig M
			if e := tx.QueryRow(r.Context(), "SELECT value FROM settings WHERE key='payment' FOR SHARE").Scan(&paymentConfig); e != nil {
				return nil, e
			}
			flow = configuredCheckoutFlow(paymentConfig)
			paymentFlow = configuredSubscriptionFlow(paymentConfig)
			checkoutType = configuredProductCheckoutType(paymentConfig, v.Product)
			if !validCheckoutType(checkoutType) {
				return nil, businessError{409, "该套餐的结账链接配置无效，请联系运营"}
			}
			if !validSubscriptionFlow(flow) || full && !validSubscriptionFlow(paymentFlow) {
				return nil, businessError{409, "订阅办理流程配置无效，请联系运营"}
			}
			if full && (paymentConfig["enabled"] != true || (subscriptionUsesBrowser(paymentFlow) && !a.paymentEnabled(r.Context()))) {
				return nil, businessError{409, "订阅付款暂不可用"}
			}
			inspectPaymentFlow := ""
			if full {
				inspectPaymentFlow = paymentFlow
			}
			if _, e := inspectSessionForFlows(v.Session, flow, inspectPaymentFlow); e != nil {
				return nil, e
			}
			if full && subscriptionUsesBrowser(paymentFlow) && checkoutType == "oaics" {
				credentials, _, err := sessionCredentials(v.Session, sessionInfo)
				if err != nil {
					return nil, err
				}
				if browserSessionCookies(credentials) == "" {
					return nil, bad("旧流程短链网页付款需要 Session 中的 sessionToken 或会话 Cookie；仅有 accessToken 时请使用长链")
				}
			}
		}
		if full {
			if e := tx.QueryRow(r.Context(), "SELECT count(*) FROM renewal_tasks WHERE (email=$1 OR account_hash=$2) AND status IN ('queued','processing','checking','review')", sessionInfo.Email, hash(sessionInfo.AccountID)).Scan(&n); e != nil {
				return nil, e
			}
			if n > 0 {
				return nil, businessError{409, "该账号有未结束的续费申请，请先核实续费设置"}
			}
			if e := tx.QueryRow(r.Context(), "SELECT count(*) FROM orders o LEFT JOIN subscription_runs s ON s.order_id=o.id WHERE o.workflow='subscription' AND (o.email=$1 OR s.account_hash=$2) AND (o.status IN ('queued','processing','awaiting_payment','review') OR o.renewal IN ('review','retrying') OR o.activation='review')", sessionInfo.Email, hash(sessionInfo.AccountID)).Scan(&n); e != nil {
				return nil, e
			}
			if n > 0 {
				return nil, businessError{409, "该订阅账号已有办理中或待核实的申请，请先核实原申请"}
			}
			var billingErr error
			subscriptionBilling, billingErr = a.randomBillingTx(r.Context(), tx)
			if billingErr != nil {
				return nil, billingErr
			}
		}
		if e := tx.QueryRow(r.Context(), "SELECT count(*) FROM orders WHERE user_id=$1 AND status IN ('draft','queued','processing','awaiting_payment') AND NOT(workflow='link' AND status='awaiting_payment')", u.ID).Scan(&n); e != nil {
			return nil, e
		}
		if n >= num(cfg, "max_open_orders", 10) {
			return nil, bad("未完成订单已达上限，请先取消不需要的草稿")
		}
		var name string
		var fee int64
		if e := tx.QueryRow(r.Context(), "SELECT name,fee FROM products WHERE id=$1 AND enabled", v.Product).Scan(&name, &fee); e != nil {
			return nil, bad("套餐不可用")
		}
		standard := fee
		membership, e := membershipFor(r.Context(), tx, u.ID)
		if e != nil {
			return nil, e
		}
		fee = feeFor(membership, standard)
		priceSnapshot, _ := json.Marshal(M{"standard": standard, "fee": fee, "tier_code": membership.Current.Code, "tier_name": membership.Current.Name, "discount": membership.Current.Discount, "source": membership.Source, "fee_override": membership.FeeOverride, "spent": membership.Spent})
		source := "web"
		if r.Context().Value(apiKey) == true {
			source = "api"
		}
		id := uid()
		_, e = tx.Exec(r.Context(), "INSERT INTO orders(id,user_id,product_id,product_name,email,country,seats,fee,source,idempotency_key,request_hash,price_snapshot,workflow) VALUES($1,$2,$3,$4,$5,$6,$7,$8,$9,$10,$11,$12,'link')", id, u.ID, v.Product, name, sessionInfo.Email, v.Country, v.Seats, fee, source, key, reqHash, priceSnapshot)
		if e != nil {
			return nil, e
		}
		if full {
			if e = a.saveOrderBillingTx(r.Context(), tx, id, subscriptionBilling); e != nil {
				return nil, e
			}
		}
		_, e = tx.Exec(r.Context(), "INSERT INTO order_events(order_id,stage,message) VALUES($1,'draft','订阅方案已保存；未提交付款，未收取服务费。')", id)
		if e == nil && v.Prepare {
			if e = a.enqueueCheckout(r.Context(), tx, id, v.Session, v.Workspace, v.PremiumSeats, flow, checkoutType); e != nil {
				return nil, e
			}
			if full {
				attempt, err := a.reserveSubscription(r.Context(), tx, id, u.ID, v.Card, v.KeepRenewal, v.PaymentLimit, v.PaymentCurrency, hash(sessionInfo.AccountID), paymentFlow)
				if err != nil {
					return nil, err
				}
				subscriptionID, subscriptionAttempt = id, attempt
			}
			return M{"id": id, "status": "queued", "checkout_connected": true, "payment_connected": full && a.subscriptionEnabled(r.Context())}, nil
		}
		return M{"id": id, "status": "draft", "payment_connected": full && a.subscriptionEnabled(r.Context())}, e
	})
	if committed && subscriptionID != "" {
		a.launchSubscription(subscriptionID, subscriptionAttempt, v.Card, subscriptionBilling)
	}
}
func (a *App) orderDetail(w http.ResponseWriter, r *http.Request) {
	u := current(r)
	items, e := a.query(r.Context(), "SELECT id,product_id,product_name,email,country,seats,status,fee,source,workflow,keep_renewal,renewal,activation,created_at,updated_at,price_snapshot FROM orders WHERE id=$1 AND user_id=$2", r.PathValue("id"), u.ID)
	if e != nil {
		fail(w, 500, "读取失败")
		return
	}
	if len(items) == 0 {
		fail(w, 404, "订单不存在")
		return
	}
	events, e := a.query(r.Context(), "SELECT stage,message,created_at FROM order_events WHERE order_id=$1 ORDER BY id", r.PathValue("id"))
	if e != nil {
		fail(w, 500, "读取失败")
		return
	}
	out(w, 200, M{"order": items[0], "events": events, "checkout": a.checkoutDetails(r.Context(), r.PathValue("id")), "payment": a.paymentDetails(r.Context(), r.PathValue("id"))})
}
func (a *App) cancelOrder(w http.ResponseWriter, r *http.Request) {
	a.transaction(w, r, func(tx pgx.Tx) (any, error) {
		if _, e := tx.Exec(r.Context(), "SELECT pg_advisory_xact_lock(1886002)"); e != nil {
			return nil, e
		}
		var workflow string
		var hasPayment bool
		if e := tx.QueryRow(r.Context(), `SELECT workflow,EXISTS(SELECT 1 FROM payment_runs p WHERE p.order_id=o.id)
 FROM orders o WHERE id=$1 AND user_id=$2 FOR UPDATE`, r.PathValue("id"), current(r).ID).Scan(&workflow, &hasPayment); e != nil {
			if e == pgx.ErrNoRows {
				return nil, businessError{404, "订单不存在"}
			}
			return nil, e
		}
		if workflow == "subscription" || hasPayment {
			return nil, businessError{409, "付款办理不支持手动取消，请查看订单进度"}
		}
		tag, e := tx.Exec(r.Context(), "UPDATE orders SET status='cancelled',updated_at=now() WHERE id=$1 AND user_id=$2 AND status IN ('draft','queued','awaiting_payment')", r.PathValue("id"), current(r).ID)
		if e = checkRows(tag.RowsAffected(), e); e != nil {
			return nil, e
		}
		if _, e = tx.Exec(r.Context(), "UPDATE jobs SET state='failed',error_code='cancelled',updated_at=now() WHERE order_id=$1 AND state='queued'", r.PathValue("id")); e != nil {
			return nil, e
		}
		if _, e = tx.Exec(r.Context(), "UPDATE checkout_runs SET credentials_enc='',hosted_url_enc='' WHERE order_id=$1", r.PathValue("id")); e != nil {
			return nil, e
		}
		_, e = tx.Exec(r.Context(), "INSERT INTO order_events(order_id,stage,message) VALUES($1,'cancelled','用户已撤回本平台方案并清除暂存资料；已复制的上游链接不会因此失效。')", r.PathValue("id"))
		return M{"ok": true}, e
	})
}
func (a *App) wallet(w http.ResponseWriter, r *http.Request) {
	u := current(r)
	ledger, e := a.query(r.Context(), customerLedgerQuery, u.ID)
	if e != nil {
		fail(w, 500, "读取失败")
		return
	}
	topups, e := a.query(r.Context(), "SELECT * FROM topups WHERE user_id=$1 ORDER BY created_at DESC LIMIT 200", u.ID)
	if e != nil {
		fail(w, 500, "读取失败")
		return
	}
	out(w, 200, M{"balance": u.Balance, "frozen": u.Frozen, "ledger": ledger, "topups": topups})
}
func (a *App) credit(tx pgx.Tx, r *http.Request, userID, kind, reference, description string, amount int64) error {
	var balance, frozen int64
	if e := tx.QueryRow(r.Context(), "SELECT balance,frozen FROM users WHERE id=$1 FOR UPDATE", userID).Scan(&balance, &frozen); e != nil {
		return e
	}
	if amount == 0 || amount > 100000000000 || amount < -100000000000 {
		return bad("金额超出允许范围")
	}
	if balance+amount < 0 {
		return bad("可用余额不足，无法扣减")
	}
	var actor any
	if current(r).ID != "" {
		actor = current(r).ID
	}
	_, e := tx.Exec(r.Context(), "INSERT INTO ledger(user_id,kind,amount,balance_after,frozen_after,reference,description,actor_id) VALUES($1,$2,$3,$4,$5,$6,$7,$8)", userID, kind, amount, balance+amount, frozen, reference, description, actor)
	if e != nil {
		return e
	}
	_, e = tx.Exec(r.Context(), "UPDATE users SET balance=balance+$2,updated_at=now() WHERE id=$1", userID, amount)
	return e
}
func (a *App) topup(w http.ResponseWriter, r *http.Request) {
	var v struct {
		Amount int64  `json:"amount"`
		Proof  string `json:"proof"`
	}
	if !read(w, r, &v) {
		return
	}
	if v.Amount < 100 || v.Amount > int64(num(a.config(r.Context(), "risk"), "max_topup", 10000000)) || len(v.Proof) > 500 {
		fail(w, 400, "充值金额或备注超出范围")
		return
	}
	a.transaction(w, r, func(tx pgx.Tx) (any, error) {
		var dummy string
		u := current(r)
		if e := tx.QueryRow(r.Context(), "SELECT id FROM users WHERE id=$1 FOR UPDATE", u.ID).Scan(&dummy); e != nil {
			return nil, e
		}
		var n int
		tx.QueryRow(r.Context(), "SELECT count(*) FROM topups WHERE user_id=$1 AND status='pending'", u.ID).Scan(&n)
		if n >= 3 {
			return nil, bad("已有 3 笔待审核充值，请先等待审核")
		}
		id := uid()
		_, e := tx.Exec(r.Context(), "INSERT INTO topups(id,user_id,amount,proof) VALUES($1,$2,$3,$4)", id, u.ID, v.Amount, v.Proof)
		return M{"id": id}, e
	})
}
func (a *App) cancelTopup(w http.ResponseWriter, r *http.Request) {
	tag, e := a.DB.Exec(r.Context(), "UPDATE topups SET status='cancelled',updated_at=now() WHERE id=$1 AND user_id=$2 AND status='pending'", r.PathValue("id"), current(r).ID)
	if e != nil {
		fail(w, 500, "操作失败")
		return
	}
	if tag.RowsAffected() == 0 {
		fail(w, 409, "充值已处理或不存在")
		return
	}
	out(w, 200, M{"ok": true})
}
func (a *App) redeem(w http.ResponseWriter, r *http.Request) {
	var v struct {
		Code string `json:"code"`
	}
	if !read(w, r, &v) {
		return
	}
	if !a.limit(r.Context(), "redeem:"+current(r).ID, 10) {
		a.event(r, "redeem_rate", "high", "兑换尝试过于频繁")
		fail(w, 429, "兑换过于频繁")
		return
	}
	a.transaction(w, r, func(tx pgx.Tx) (any, error) {
		var id, status string
		var amount int64
		var valid bool
		e := tx.QueryRow(r.Context(), "SELECT id,amount,status,expires_at>now() FROM redeem_codes WHERE code_hash=$1 FOR UPDATE", hash(strings.ToUpper(strings.TrimSpace(v.Code)))).Scan(&id, &amount, &status, &valid)
		if e != nil || status != "active" || !valid {
			return nil, bad("兑换码无效、已过期或已使用")
		}
		if e = a.credit(tx, r, current(r).ID, "redeem", "redeem:"+id, "兑换码入账", amount); e != nil {
			return nil, e
		}
		_, e = tx.Exec(r.Context(), "UPDATE redeem_codes SET status='used',used_by=$2,used_at=now() WHERE id=$1", id, current(r).ID)
		return M{"ok": true, "amount": amount}, e
	})
}
func (a *App) keys(w http.ResponseWriter, r *http.Request) {
	a.list(w, r, "SELECT id,name,prefix,revoked,last_used,created_at FROM api_keys WHERE user_id=$1 ORDER BY created_at DESC", current(r).ID)
}
func (a *App) createKey(w http.ResponseWriter, r *http.Request) {
	var v struct {
		Name string `json:"name"`
	}
	if !read(w, r, &v) {
		return
	}
	if len(v.Name) < 1 || len(v.Name) > 80 {
		fail(w, 400, "请填写密钥名称")
		return
	}
	a.transaction(w, r, func(tx pgx.Tx) (any, error) {
		u := current(r)
		var dummy string
		if e := tx.QueryRow(r.Context(), "SELECT id FROM users WHERE id=$1 FOR UPDATE", u.ID).Scan(&dummy); e != nil {
			return nil, e
		}
		var n int
		tx.QueryRow(r.Context(), "SELECT count(*) FROM api_keys WHERE user_id=$1 AND NOT revoked", u.ID).Scan(&n)
		if n >= 10 {
			return nil, bad("最多保留 10 个有效密钥")
		}
		key := "xp_live_" + secret()
		id := uid()
		_, e := tx.Exec(r.Context(), "INSERT INTO api_keys(id,user_id,name,hash,prefix) VALUES($1,$2,$3,$4,$5)", id, u.ID, v.Name, hash(key), key[:16]+"…"+key[len(key)-4:])
		return M{"id": id, "key": key}, e
	})
}
func (a *App) revokeKey(w http.ResponseWriter, r *http.Request) {
	tag, e := a.DB.Exec(r.Context(), "UPDATE api_keys SET revoked=true WHERE id=$1 AND user_id=$2", r.PathValue("id"), current(r).ID)
	if e != nil || tag.RowsAffected() == 0 {
		fail(w, 404, "密钥不存在")
		return
	}
	out(w, 200, M{"ok": true})
}
func (a *App) invites(w http.ResponseWriter, r *http.Request) {
	a.list(w, r, "SELECT substring(email from 1 for 2)||'***@'||split_part(email,'@',2) AS email,created_at FROM users WHERE invited_by=$1 ORDER BY created_at DESC", current(r).ID)
}

var _ = fmt.Sprint
