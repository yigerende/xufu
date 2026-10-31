package platform

import (
	"context"
	"errors"
	"github.com/jackc/pgx/v5"
	"strings"
	"time"
)

func currencyForCountry(country string) string {
	return map[string]string{"US": "usd", "PH": "php", "JP": "jpy", "SG": "sgd", "GB": "gbp", "EG": "egp", "TR": "try"}[country]
}

func (a *App) reserveSubscription(ctx context.Context, tx pgx.Tx, id, user string, card cardInput, keep bool, limit int64, currency, accountHash, flow string) (string, error) {
	if !validSubscriptionFlow(flow) {
		return "", bad("订阅办理流程配置无效")
	}
	cfg := a.config(ctx, "payment")
	if e := a.paymentAdmission(ctx, tx, user, cfg); e != nil {
		return "", e
	}
	attempt := uid()
	ttl := num(cfg, "subscription_wait_seconds", 600) + num(cfg, "prepare_timeout_seconds", 120) + num(cfg, "result_timeout_seconds", 120) + 60
	_, e := tx.Exec(ctx, "UPDATE orders SET workflow='subscription',activation='pending',keep_renewal=$2,renewal=CASE WHEN $2 THEN 'kept' ELSE 'pending' END WHERE id=$1", id, keep)
	if e == nil {
		_, e = tx.Exec(ctx, `INSERT INTO subscription_runs(order_id,max_amount,currency,credentials_enc,credentials_expires_at,account_hash)
 SELECT order_id,$2,$3,credentials_enc,LEAST(credentials_expires_at,now()+interval '24 hours'),$4 FROM checkout_runs WHERE order_id=$1`, id, limit, currency, accountHash)
	}
	if e == nil {
		_, e = tx.Exec(ctx, "INSERT INTO payment_runs(order_id,attempt_id,state,last4,expires_at,hosted_url_enc,executor) VALUES($1,$2,'preparing',$3,now()+($4*interval '1 second'),'',$5)", id, attempt, card.Number[len(card.Number)-4:], ttl, subscriptionPaymentExecutor(flow))
	}
	if e == nil && flow == subscriptionFlowCSLive {
		_, e = tx.Exec(ctx, "INSERT INTO subscription_protocol(order_id,expires_at) SELECT order_id,credentials_expires_at FROM subscription_runs WHERE order_id=$1", id)
	}
	if e == nil {
		e = paymentFunds(ctx, tx, id, attempt, "hold")
	}
	if e == nil {
		_, e = tx.Exec(ctx, "UPDATE order_events SET message=CASE WHEN stage='queued' THEN '已排队，等待办理资源。' ELSE '订阅方案已收到，正在安排办理。' END WHERE order_id=$1 AND stage IN ('draft','queued')", id)
	}
	if e == nil {
		_, e = tx.Exec(ctx, "INSERT INTO order_events(order_id,stage,message) VALUES($1,'subscription_accepted','订阅申请已受理，服务费已冻结；将按所选方案连续办理付款及续费设置。')", id)
	}
	return attempt, e
}

func (a *App) launchSubscription(id, attempt string, card cardInput, billing M) {
	if a.isProtocolSubscription(context.Background(), id) {
		a.queueProtocolSubscription(id, attempt, card, billing)
		return
	}
	cfg := a.config(context.Background(), "payment")
	ctx, cancel := context.WithTimeout(context.Background(), time.Duration(num(cfg, "subscription_wait_seconds", 600))*time.Second)
	session := &paymentSession{attempt: attempt, cancel: cancel, commands: make(chan paymentCommand, 1)}
	a.paymentMu.Lock()
	if a.payments == nil {
		a.payments = map[string]*paymentSession{}
	}
	a.payments[id] = session
	a.paymentMu.Unlock()
	go func() {
		defer cancel()
		defer func() {
			card = cardInput{}
			a.paymentMu.Lock()
			if a.payments[id] == session {
				delete(a.payments, id)
			}
			a.paymentMu.Unlock()
		}()
		failed := func(code string) {
			c, stop := context.WithTimeout(context.Background(), 10*time.Second)
			defer stop()
			a.finishPayment(c, id, attempt, paymentEvent{State: "failed", Code: code})
		}
		ticker := time.NewTicker(time.Second)
		defer ticker.Stop()
		for {
			var job, state, enc, email, currency string
			var limit int64
			e := a.DB.QueryRow(ctx, `SELECT j.state,p.state,c.hosted_url_enc,o.email,s.max_amount,s.currency FROM jobs j JOIN orders o ON o.id=j.order_id JOIN checkout_runs c ON c.order_id=o.id JOIN payment_runs p ON p.order_id=o.id JOIN subscription_runs s ON s.order_id=o.id WHERE o.id=$1 AND p.attempt_id=$2`, id, attempt).Scan(&job, &state, &enc, &email, &limit, &currency)
			if e == nil {
				if state != "preparing" {
					return
				}
				if job == "failed" || job == "review" {
					failed("subscription_preparation_failed")
					return
				}
				if job == "completed" {
					raw, err := a.decrypt(enc)
					if err != nil || !browserPaymentURL(raw) {
						failed("unsupported_checkout")
						return
					}
					ttl := num(cfg, "prepare_timeout_seconds", 120) + num(cfg, "result_timeout_seconds", 120) + 30
					tx, err := a.DB.Begin(ctx)
					if err != nil {
						failed("preparation_failed")
						return
					}
					_, err = tx.Exec(ctx, "SELECT id FROM orders WHERE id=$1 FOR UPDATE", id)
					if err == nil {
						var tagRows int64
						tag, e := tx.Exec(ctx, "UPDATE payment_runs SET hosted_url_enc=$3,expires_at=now()+($4*interval '1 second') WHERE order_id=$1 AND attempt_id=$2 AND state='preparing'", id, attempt, enc, ttl)
						err = e
						tagRows = tag.RowsAffected()
						if err == nil && tagRows != 1 {
							err = ErrLeaseLost
						}
					}
					if err == nil {
						_, err = tx.Exec(ctx, "UPDATE orders SET status='processing',updated_at=now() WHERE id=$1", id)
					}
					if err == nil {
						err = tx.Commit(ctx)
					} else {
						tx.Rollback(ctx)
					}
					if err != nil {
						failed("preparation_failed")
						return
					}
					a.launchPayment(id, attempt, paymentInput{Mode: "prepare", URL: raw, Email: email, Card: card, Billing: billing, PrepareSeconds: num(cfg, "prepare_timeout_seconds", 120), QuoteSeconds: num(cfg, "quote_timeout_seconds", 180), ResultSeconds: num(cfg, "result_timeout_seconds", 120), AutoConfirm: true, MaxAmount: limit, Currency: currency}, ttl)
					return
				}
			}
			select {
			case <-ctx.Done():
				failed("subscription_preparation_failed")
				return
			case <-ticker.C:
			}
		}
	}()
}

func (a *App) authorizeSubscriptionQuote(ctx context.Context, id, attempt string, event paymentEvent, max int64, currency string) error {
	if event.Amount <= 0 || event.Amount > max || event.Currency != currency {
		return errors.New("amount exceeds authorization")
	}
	tx, e := a.DB.Begin(ctx)
	if e != nil {
		return e
	}
	defer tx.Rollback(ctx)
	if _, e = tx.Exec(ctx, "SELECT id FROM orders WHERE id=$1 FOR UPDATE", id); e != nil {
		return e
	}
	tag, e := tx.Exec(ctx, "UPDATE payment_runs SET state='submitting',submitted=true,updated_at=now() WHERE order_id=$1 AND attempt_id=$2 AND state='quoted' AND quote_expires_at>now()", id, attempt)
	if e != nil {
		return e
	}
	if tag.RowsAffected() != 1 {
		return ErrLeaseLost
	}
	if e = paymentEventTx(ctx, tx, id, "payment_submitting", "confirming"); e != nil {
		return e
	}
	return tx.Commit(ctx)
}

func (a *App) isSubscription(ctx context.Context, id string) bool {
	var workflow string
	a.DB.QueryRow(ctx, "SELECT workflow FROM orders WHERE id=$1", id).Scan(&workflow)
	return strings.EqualFold(workflow, "subscription")
}
