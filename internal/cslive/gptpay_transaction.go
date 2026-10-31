// Adapted from the supplied cs-live-source.zip; see docs/subscription-protocol.md.
package cslive

import (
	context "context"
	errors "errors"
	strings "strings"
	time "time"
)

type gptpay_transaction struct {
	providerCode, declineCode string
	client                    *gptpay_PaymentClient
	cp                        *model_Checkpoint
	http, stripeHTTP          protocol_HTTPTransport
	platform                  *chatgpt_Platform
	stripe                    *stripe_Stripe
	sentinel                  gptpay_tokenSource
	observer                  *observability_Observer
	ctx                       context.Context
	cancel                    context.CancelFunc
	started                   time.Time
	progress                  *gptpay_progressTracker
}

func gptpay_newTransaction(ctx context.Context, c *gptpay_PaymentClient, cp model_Checkpoint, operation string, clock fingerprint_Clock) (*gptpay_transaction, error) {
	started := time.Now()
	profile, err := cp.BrowserProfile()
	if err != nil {
		return nil, err
	}
	if ctx == nil {
		ctx = context.Background()
	}
	expires := time.Now().Add(time.Duration(c.Config.Deadline * float64(time.Second)))
	ctx, cancel := context.WithDeadline(ctx, expires)
	if d, ok := ctx.Deadline(); ok {
		expires = d
	}
	phase := ""
	if operation == "recover" {
		phase = cp.Phase
	}
	obs := observability_NewObserver(operation, c.EventSink, protocol_AsString(cp.Metadata["trace_id"]), phase)
	cp.Metadata["trace_id"] = obs.TraceID
	factory := c.transportFactory
	if factory == nil {
		factory = func(ctx context.Context, p string, t float64, d time.Time) (protocol_HTTPTransport, error) {
			return transport_NewTransport(ctx, p, t, d)
		}
	}
	h, e := factory(ctx, cp.Proxy, c.Config.Timeout, expires)
	if e != nil {
		cancel()
		return nil, e
	}
	sh, e := factory(ctx, cp.Proxy, c.Config.Timeout, expires)
	if e != nil {
		h.Close()
		cancel()
		return nil, e
	}
	t := &gptpay_transaction{client: c, cp: &cp, http: h, stripeHTTP: sh, observer: obs, ctx: ctx, cancel: cancel, started: started}
	t.progress = &gptpay_progressTracker{sink: c.ProgressSink, traceID: obs.TraceID, operation: operation, started: started, states: make(map[model_ProgressStage]model_ProgressStatus), cp: &cp}
	t.platform = chatgpt_NewPlatform(h, &cp, clock)
	t.platform.Profile = profile
	t.stripe = stripe_NewStripe(sh, &cp, clock)
	t.stripe.Profile = profile
	// Stripe.js 的 stripe_js_id 与 client_session_id 是同一次页面会话的标识，
	// 浏览器不会在重试或页面重开时换掉它；恢复沿用快照值，缺失时（旧快照）才新建并保存。
	if id := protocol_AsString(cp.Metadata["stripe_client_id"]); id != "" {
		t.stripe.ClientID = id
	} else {
		cp.Metadata["stripe_client_id"] = t.stripe.ClientID
	}
	t.stripe.BindObserver(obs)
	sentinel := challenge_NewSentinel(h, &cp, clock, obs)
	sentinel.Profile = profile
	t.sentinel = sentinel
	if c.sentinelFactory != nil {
		t.sentinel = c.sentinelFactory(h, &cp, clock)
	}
	for _, b := range []struct {
		h       protocol_HTTPTransport
		service string
	}{{h, "chatgpt"}, {sh, "stripe"}} {
		if x, ok := b.h.(interface {
			BindObserver(*observability_Observer, string)
		}); ok {
			x.BindObserver(obs, b.service)
		}
	}
	obs.Emit("transaction", protocol_Map{"phase": cp.Phase, "outcome": "started", "payment_state": cp.Payment, "browser_profile": profile.ID})
	return t, nil
}

func (t *gptpay_transaction) save(phase string) error {
	changed := phase != "" && phase != t.cp.Phase
	if phase != "" {
		t.cp.Phase = phase
	}
	if t.client.Checkpoint != nil {
		snapshot, e := t.cp.Clone()
		if e == nil {
			e = gptpay_callCheckpoint(t.client.Checkpoint, snapshot)
		}
		if e != nil {
			t.observer.Emit("checkpoint", protocol_Map{"phase": t.cp.Phase, "outcome": "failed", "error_code": "checkpoint_failed"})
			return protocol_NewError("checkpoint_failed")
		}
	}
	t.observer.Checkpoint(t.cp.Phase, changed, t.client.Checkpoint != nil)
	return nil
}

func gptpay_callCheckpoint(sink model_CheckpointSink, cp model_Checkpoint) (err error) {
	defer func() {
		if recover() != nil {
			err = protocol_NewError("checkpoint_failed")
		}
	}()
	return sink(cp)
}

// boundary 必须在不可安全重放的请求之前同步落盘；响应丢失后沿用 unknown。
func (t *gptpay_transaction) boundary(phase string) error {
	t.cp.Payment = "unknown"
	return t.save(phase)
}

func (t *gptpay_transaction) complete(err error) model_Result {
	code := protocol_FlowErrorCode(err)
	t.progress.finish(code)
	warning := ""
	if t.cp.Payment == "charged" {
		// 资金结果与到账、续费及持久化等后续处理分开报告。
		warning, code = code, ""
	}
	t.observer.Complete(t.cp.Phase, t.cp.Payment, code, warning)
	cp, e := t.cp.Clone()
	if e != nil {
		cp = *t.cp
	}
	result := model_Result{ProviderCode: t.providerCode, DeclineCode: t.declineCode, Payment: model_PaymentState(cp.Payment), Activation: cp.Activation, Renewal: cp.Renewal, CheckoutID: cp.CheckoutID, PaymentIntentID: cp.PaymentIntentID, Amount: cp.Amount, Currency: cp.Currency, EstimatedPrice: cp.EstimatedPrice(), ChargedPrice: cp.ChargedPrice(), ErrorCode: code, WarningCode: warning, Flow: protocol_AsString(cp.Metadata["payment_flow"]), Checkpoint: cp}
	if elapsed, ok := protocol_Integer(cp.Metadata["payment_elapsed_ms"]); ok && elapsed >= 0 && cp.Payment == "charged" {
		result.PaymentElapsedMS = &elapsed
	}
	return result
}

func (t *gptpay_transaction) close() {
	t.cancel()
	// 关闭是清理边界：一个连接池的失败不能覆盖资金结果，也不能阻止另一个池释放。
	for _, h := range []protocol_HTTPTransport{t.http, t.stripeHTTP} {
		func() {
			defer func() { _ = recover() }()
			if h != nil {
				h.Close()
			}
		}()
	}
}

func (t *gptpay_transaction) createCheckout(kind string, withPrice bool) (result model_CheckoutResult) {
	defer t.close()
	defer func() {
		if recover() != nil {
			result.CheckoutID = t.cp.CheckoutID
			if result.CheckoutID != "" {
				result.ProcessorEntity = t.cp.Entity
			}
			result.ErrorCode = "protocol_error"
			t.progress.finish(result.ErrorCode)
			t.observer.Complete(t.cp.Phase, t.cp.Payment, result.ErrorCode)
		}
	}()
	// Hosted 提链及价格读取只需公钥和 init；完整设备上下文留给付款和可恢复快照。
	stripeRequired := withPrice && kind != "hosted" || t.client.Checkpoint != nil
	checkout, err := t.prepareCheckout(kind, stripeRequired)
	if err == nil && protocol_AsString(checkout["payment_flow"]) == "subscription_update" {
		result.Flow = "subscription_update"
		result.ProcessorEntity = t.cp.Entity
		if withPrice {
			result.Price, err = t.readUpgradePrice()
		}
		result.ErrorCode = protocol_FlowErrorCode(err)
		if err == nil {
			t.progress.emit(model_ProgressStageCheckout, model_ProgressSucceeded, "")
		}
		t.progress.finish(result.ErrorCode)
		t.observer.Complete(t.cp.Phase, t.cp.Payment, result.ErrorCode)
		return result
	}
	if err == nil {
		if kind == "hosted" {
			result.CheckoutURL, err = t.standaloneHostedURL(checkout)
		} else {
			result.CheckoutURL, err = t.checkoutURL(checkout, stripeRequired)
		}
		if err == nil && withPrice {
			result.Price, err = t.readCheckoutPrice(checkout, kind != "hosted" && t.cp.Metadata["checkout_ui_mode"] == "custom", result.CheckoutURL)
		}
	}
	result.CheckoutID = t.cp.CheckoutID
	result.CheckoutURL = gptpay_publicCheckoutURL(result.CheckoutURL, result.CheckoutID)
	if result.CheckoutID != "" {
		result.ProcessorEntity = t.cp.Entity
	}
	result.ErrorCode = protocol_FlowErrorCode(err)
	if err == nil {
		t.progress.emit(model_ProgressStageCheckout, model_ProgressSucceeded, "")
	}
	t.progress.finish(result.ErrorCode)
	t.observer.Complete(t.cp.Phase, t.cp.Payment, result.ErrorCode)
	return result
}

func (t *gptpay_transaction) recover() (result model_Result) {
	defer t.close()
	defer func() {
		if recover() != nil {
			result = t.complete(protocol_NewError("recovery_failed"))
		}
	}()
	err := t.recoverPayment()
	var protocolErr *protocol_ProtocolError
	if err != nil && !errors.As(err, &protocolErr) {
		err = protocol_NewError("recovery_failed")
	}
	return t.complete(err)
}

func (t *gptpay_transaction) execute(billing model_Billing, card *model_Card, expected *int64, approval model_PaymentApproval, checkoutType string) (result model_Result) {
	defer t.close()
	defer func() {
		if recover() != nil {
			result = t.complete(protocol_NewError("protocol_error"))
		}
	}()
	err := t.executeFlow(billing, card, expected, approval, checkoutType)
	return t.complete(err)
}

func (t *gptpay_transaction) executeFlow(billing model_Billing, card *model_Card, expected *int64, approval model_PaymentApproval, checkoutType string) error {
	checkout, e := t.prepareCheckout(checkoutType, true)
	if e != nil {
		return e
	}
	if protocol_AsString(t.cp.Metadata["payment_flow"]) == "subscription_update" {
		return t.runSubscriptionUpdate(card, billing, expected, approval)
	}
	var info protocol_Map
	if t.cp.Metadata["checkout_ui_mode"] == "redirect" {
		if !strings.HasPrefix(t.cp.CheckoutID, "cs_live_") {
			return protocol_NewError("checkout_id_invalid")
		}
		info, e = t.stripe.HostedPrepare(stripe_FindHostedURL(checkout, t.cp.CheckoutID), billing)
		if t.stripe.HostedURL != "" {
			t.cp.Metadata["hosted_url"] = t.stripe.HostedURL
		}
		if e == nil {
			e = t.pricing(info["amount"], info["currency"], expected, model_PriceSourceCheckoutTax)
		}
	} else {
		var tax protocol_Map
		tax, e = t.platform.Taxes(billing)
		if e == nil {
			node := protocol_AsMap(tax["checkout_session"])
			e = t.pricing(node["amount_total"], protocol_FirstTruthy(node["currency"], t.cp.Currency), expected, model_PriceSourceCheckoutTax)
		}
	}
	if e != nil {
		return e
	}
	if card == nil {
		if e = t.save("quoted"); e != nil {
			return e
		}
		t.progress.emit(model_ProgressStageCheckout, model_ProgressSucceeded, "")
		return nil
	}
	t.progress.emit(model_ProgressStageCheckout, model_ProgressSucceeded, "")
	t.progress.emit(model_ProgressStagePayment, model_ProgressRunning, "")
	if approval != nil {
		if e = t.ctx.Err(); e != nil {
			return e
		}
		ok, e := gptpay_callApproval(approval, *t.cp.Amount, t.cp.Currency)
		if e != nil {
			return protocol_NewError("payment_approval_failed")
		}
		if !ok {
			return protocol_NewError("payment_cancelled")
		}
		if e = t.ctx.Err(); e != nil {
			return e
		}
	}
	if t.cp.Metadata["checkout_ui_mode"] == "redirect" {
		e = t.runHostedFlow(*card, billing, info)
	} else {
		e = t.runCustomFlow(*card, billing, checkout)
	}
	if e != nil {
		return e
	}
	if t.cp.Payment == "charged" {
		if e = t.finishPayment(); e != nil {
			return e
		}
	}
	if e = t.save(""); e != nil {
		return e
	}
	if t.cp.Payment != "charged" {
		return protocol_NewError(t.cp.Payment)
	}
	return nil
}

func gptpay_callApproval(approve model_PaymentApproval, amount int64, currency string) (ok bool, err error) {
	defer func() {
		if recover() != nil {
			ok = false
			err = protocol_NewError("payment_approval_failed")
		}
	}()
	return approve(amount, currency)
}

func (t *gptpay_transaction) pricing(total, currency any, expected *int64, source string) error {
	n, ok := protocol_Integer(total)
	if !ok || n < 0 {
		return protocol_NewError("checkout_total_missing")
	}
	if strings.ToUpper(protocol_AsString(currency)) != t.cp.Currency {
		return protocol_NewError("checkout_currency_mismatch")
	}
	if expected != nil && n != *expected {
		return protocol_NewError("checkout_amount_changed")
	}
	t.cp.Amount = &n
	t.cp.Metadata["estimated_price"] = model_EncodePriceMetadata(n, t.cp.Currency, source)
	return t.save("priced")
}

func gptpay_requestedCheckoutUIMode(kind string) string {
	if kind == "oaics" {
		return "custom"
	}
	if kind == "hosted" {
		return "hosted"
	}
	return "custom"
}
