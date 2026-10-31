package platform

import (
	"context"
	"errors"
	"net/http"
	"net/url"
	"path"
	"regexp"
	"strconv"
	"strings"
	"time"
)

type legacyPayment struct {
	in             paymentInput
	cp             legacyPaymentCheckpoint
	client         *http.Client
	events         chan paymentEvent
	commands       chan paymentCommand
	cancel         context.CancelFunc
	ctx            context.Context
	custom         bool
	entity         string
	hostedConfigID string
	sent           bool
	latest, quote  *legacyPaymentSnapshot
}

func (p *legacyPayment) Events() <-chan paymentEvent { return p.events }
func (p *legacyPayment) Send(cmd paymentCommand) error {
	select {
	case <-p.ctx.Done():
		return errors.New("runtime unavailable")
	case p.commands <- cmd:
		return nil
	default:
		return errors.New("command pending")
	}
}

// Called from the event consumer: cancellation must not wait on that consumer.
func (p *legacyPayment) Close() { p.cancel() }
func (p *legacyPayment) emit(e paymentEvent) {
	select {
	case p.events <- e:
	case <-p.ctx.Done():
	}
}
func (p *legacyPayment) stage(code string) { p.emit(paymentEvent{Type: "stage", Code: code}) }

func startLegacyPayment(ctx context.Context, in paymentInput) (paymentProcess, error) {
	client, closeClient, err := legacyPaymentHTTP(in)
	if err != nil {
		return nil, err
	}
	return startLegacyPaymentClient(ctx, in, client, closeClient), nil
}

// Injectable HTTP client for offline protocol tests; production always uses the
// authenticated relay above, never the default transport or a direct fallback.
func startLegacyPaymentClient(parent context.Context, in paymentInput, client *http.Client, closeClient func()) *legacyPayment {
	ctx, cancel := context.WithCancel(parent)
	p := &legacyPayment{in: in, cp: in.Checkpoint, client: client, ctx: ctx, cancel: cancel, events: make(chan paymentEvent, 32), commands: make(chan paymentCommand, 1)}
	go func() {
		defer close(p.events)
		defer cancel()
		defer closeClient()
		defer func() { p.in = paymentInput{}; p.cp = legacyPaymentCheckpoint{} }()
		err := p.run()
		if err != nil {
			code := err.Error()
			state := "failed"
			if p.sent || p.in.Mode == "check" {
				state = "review"
			}
			if code == "verification_required" {
				state = "action_required"
			} else if code == "payment_declined" {
				state = "declined"
			}
			p.result(state, code)
		}
	}()
	return p
}
func (p *legacyPayment) result(state, code string) {
	e := paymentEvent{Type: "result", State: state, Code: code, Sent: p.sent}
	if p.latest != nil {
		e.Amount = p.latest.Amount
		e.Currency = p.latest.Currency
		e.Evidence = p.latest.Evidence
	}
	p.emit(e)
}
func (p *legacyPayment) save(ctx context.Context) error {
	if p.in.SaveCheckpoint == nil {
		return nil
	}
	if err := p.in.SaveCheckpoint(ctx, p.cp); err != nil {
		return errors.New("result_unconfirmed")
	}
	return nil
}
func (p *legacyPayment) run() error {
	if !browserPaymentURL(p.in.URL) || (p.in.Mode != "prepare" && p.in.Mode != "check") {
		return errors.New("unsupported_checkout")
	}
	u, _ := url.Parse(p.in.URL)
	id := path.Base(u.Path)
	if p.cp.CheckoutID != "" && p.cp.CheckoutID != id {
		return errors.New("invalid_response")
	}
	p.cp.CheckoutID = id
	p.custom = validCustomCheckoutURL(p.in.URL)
	if p.custom {
		p.entity = strings.Split(strings.Trim(u.Path, "/"), "/")[1]
	}
	p.stage("network_check")
	if e := p.guard(p.ctx); e != nil {
		return e
	}
	p.stage("network_verified")
	prepareCtx, stop := context.WithTimeout(p.ctx, time.Duration(p.in.PrepareSeconds)*time.Second)
	defer stop()
	p.stage("opening")
	if p.cp.Key == "" {
		p.cp.Key = legacyURLKey(p.in.URL)
	}
	if p.cp.Key == "" {
		if p.in.Authorization.AccessToken == "" {
			return errors.New("invalid_session")
		}
		data, e := p.request(prepareCtx, "GET", "https://chatgpt.com/backend-api/payments/stripe_client_bootstrap?account_id="+url.QueryEscape(p.in.Authorization.AccountID), nil, nil, "opening")
		if e != nil {
			return e
		}
		p.cp.Key = str(data, "publishable_key")
		if p.cp.Key == "" {
			p.cp.Key = str(data, "publishableKey")
		}
	}
	if !legacyPublishableKey.MatchString(p.cp.Key) {
		return errors.New("invalid_response")
	}
	if p.in.Mode == "check" {
		// Historical oaics orders without an intent checkpoint cannot be safely
		// re-confirmed to discover their intent. Leave them for official review.
		if strings.HasPrefix(id, "oaics_") && p.cp.Secret == "" {
			return errors.New("result_unconfirmed")
		}
		s, e := p.read(prepareCtx)
		if e != nil {
			return e
		}
		p.latest = s
		p.complete()
		return nil
	}
	if !validBilling(p.in.Billing) || !validCard(&p.in.Card) {
		return errors.New("preparation_failed")
	}
	if strings.HasPrefix(id, "oaics_") {
		s, e := p.taxes(prepareCtx)
		if e != nil {
			return e
		}
		p.latest = s
	} else {
		initForm := url.Values{"browser_locale": {"en-US"}, "browser_timezone": {"UTC"}, "eid": {"NA"}, "redirect_type": {"url"}}
		if !p.custom {
			initForm.Set("browser_locale", "zh-CN")
			initForm.Set("browser_timezone", "Asia/Shanghai")
		}
		init, e := p.stripe(prepareCtx, "POST", "/payment_pages/"+id+"/init", initForm, "opening")
		if e != nil {
			return e
		}
		if e = p.observeHostedPage(init); e != nil {
			return e
		}
		p.cp.EID = str(init, "eid")
		if p.cp.EID == "" {
			p.cp.EID = "NA"
		}
		// The captured init and tax POSTs return the full Checkout page. Validate
		// session_id (not Elements' id) and consume that response directly.
		s, e := p.hostedPageResponse(prepareCtx, init)
		if e != nil {
			return e
		}
		p.latest = s
		if s.Paid || s.Attempted || !s.Open {
			return errors.New("already_paid")
		}
		p.stage("billing")
		s, e = p.hostedTaxQuote(prepareCtx)
		if e != nil {
			return e
		}
		p.latest = s
	}
	p.stage("pricing")
	if !p.latest.TaxReady || !p.latest.Open || p.latest.Paid || p.latest.Attempted {
		return errors.New("quote_unavailable")
	}
	if err := p.save(prepareCtx); err != nil {
		return err
	}
	p.quote = p.latest
	p.emit(paymentEvent{Type: "quote", Amount: p.quote.Amount, Currency: p.quote.Currency})
	stop()
	timer := time.NewTimer(time.Duration(p.in.QuoteSeconds) * time.Second)
	defer timer.Stop()
	select {
	case <-p.ctx.Done():
		return errors.New("result_unconfirmed")
	case <-timer.C:
		return errors.New("quote_expired")
	case cmd := <-p.commands:
		if cmd.Type != "confirm" {
			return errors.New("cancelled")
		}
	}
	resultCtx, end := context.WithTimeout(p.ctx, time.Duration(p.in.ResultSeconds)*time.Second)
	defer end()
	// Re-read the original total after the durable database authorization.
	var current *legacyPaymentSnapshot
	var err error
	if strings.HasPrefix(id, "oaics_") {
		current, err = p.taxes(resultCtx)
	} else {
		current, err = p.hostedTaxQuote(resultCtx)
	}
	if err != nil {
		return err
	}
	p.latest = current
	if current.Amount != p.quote.Amount || current.Currency != p.quote.Currency || !current.TaxReady || !current.Open || current.Attempted {
		return errors.New("amount_changed")
	}
	// Persist the latest EID/page identity before any irreversible request.
	if err = p.save(resultCtx); err != nil {
		return err
	}
	p.stage("card_prepare")
	if strings.HasPrefix(id, "oaics_") {
		err = p.confirmCustom(resultCtx)
	} else {
		err = p.confirmPage(resultCtx)
	}
	p.in.Card = cardInput{}
	if err != nil {
		// A lost response is not permission to repeat a confirmation. A single
		// read can still establish the original transaction's authoritative state.
		if p.sent && (err.Error() == "network_connection_failed" || err.Error() == "request_rejected") && (!strings.HasPrefix(id, "oaics_") || p.cp.Secret != "") {
			if s, readErr := p.read(resultCtx); readErr == nil {
				p.latest = s
				if p.terminal() {
					p.complete()
					return nil
				}
			}
		}
		return err
	}
	p.stage("result_check")
	for !p.terminal() {
		t := time.NewTimer(time.Second)
		select {
		case <-resultCtx.Done():
			t.Stop()
			return errors.New("result_unconfirmed")
		case <-t.C:
		}
		s, e := p.read(resultCtx)
		if e != nil {
			return e
		}
		p.latest = s
	}
	p.complete()
	return nil
}

func (p *legacyPayment) waitTaxQuote(ctx context.Context) (*legacyPaymentSnapshot, error) {
	for {
		s, err := p.read(ctx)
		if err != nil {
			return nil, err
		}
		if s.TaxReady || s.Attempted || !s.Open {
			return s, nil
		}
		t := time.NewTimer(time.Second)
		select {
		case <-ctx.Done():
			t.Stop()
			return nil, errors.New("quote_unavailable")
		case <-t.C:
		}
	}
}

func (p *legacyPayment) address() M {
	return M{"line1": str(p.in.Billing, "line1"), "city": str(p.in.Billing, "city"), "state": str(p.in.Billing, "state"), "postal_code": str(p.in.Billing, "postal_code"), "country": str(p.in.Billing, "country")}
}
func (p *legacyPayment) name() string {
	return strings.TrimSpace(str(p.in.Billing, "first_name") + " " + str(p.in.Billing, "last_name"))
}
func (p *legacyPayment) cardForm(prefix string) url.Values {
	f := url.Values{"type": {"card"}, "card[number]": {p.in.Card.Number}, "card[exp_month]": {p.in.Card.Month}, "card[exp_year]": {p.in.Card.Year}, "card[cvc]": {p.in.Card.CVC}, "billing_details[name]": {p.name()}, "billing_details[email]": {p.in.Email}}
	// A nested payment_method_data form uses bracketed field names.
	if prefix != "" {
		f = url.Values{}
		for k, v := range p.cardForm("") {
			pos := strings.Index(k, "[")
			if pos < 0 {
				f[prefix+"["+k+"]"] = v
			} else {
				f[prefix+"["+k[:pos]+"]"+k[pos:]] = v
			}
		}
		return f
	}
	for k, v := range p.address() {
		f.Set("billing_details[address]["+k+"]", v.(string))
	}
	return f
}
func (p *legacyPayment) read(ctx context.Context) (*legacyPaymentSnapshot, error) {
	if !p.custom && p.cp.PageID != "" && (p.sent || p.in.Mode == "check") {
		if snapshot, err := p.readHostedPoll(ctx); err != nil || snapshot != nil {
			return snapshot, err
		}
	}
	if p.cp.Secret != "" {
		m := legacySecret.FindStringSubmatch(p.cp.Secret)
		if len(m) != 2 || m[1] != p.cp.IntentID {
			return nil, errors.New("invalid_response")
		}
		data, e := p.stripe(ctx, "GET", "/payment_intents/"+p.cp.IntentID, url.Values{"client_secret": {p.cp.Secret}}, "checking")
		if e != nil {
			return nil, e
		}
		return legacyIntentSnapshot(data, p.cp)
	}
	data, e := p.stripe(ctx, "GET", "/payment_pages/"+p.cp.CheckoutID, url.Values{"eid": {p.cp.EID}}, "pricing")
	if e != nil {
		return nil, e
	}
	snapshot, e := legacyPageSnapshot(data, p.cp.CheckoutID)
	if e != nil {
		return nil, e
	}
	if p.in.Mode == "prepare" {
		if e = p.observeHostedPage(data); e != nil {
			return nil, e
		}
	}
	return snapshot, nil
}
func (p *legacyPayment) taxes(ctx context.Context) (*legacyPaymentSnapshot, error) {
	currency := p.in.Currency
	if currency == "" {
		currency = currencyForCountry(str(p.in.Billing, "country"))
	}
	body := M{"checkout_session_id": p.cp.CheckoutID, "processor_entity": p.entity, "checkout_email": p.in.Email, "billing_country": str(p.in.Billing, "country"), "billing_name": p.name(), "billing_address": p.address(), "currency": strings.ToLower(currency)}
	data, e := p.request(ctx, "POST", checkoutEndpoint+"/taxes", nil, body, "pricing")
	if e != nil {
		return nil, e
	}
	s := legacyMap(data["checkout_session"])
	// Bind the response when IDs are present; the request itself selects one
	// original checkout and entity (never a newly created checkout).
	for _, node := range []M{data, s} {
		if str(node, "processor_entity") != "" && str(node, "processor_entity") != p.entity {
			return nil, errors.New("invalid_response")
		}
		for _, k := range []string{"checkout_session_id", "session_id"} {
			if str(node, k) != "" && str(node, k) != p.cp.CheckoutID {
				return nil, errors.New("invalid_response")
			}
		}
	}
	if str(s, "id") != "" && str(s, "id") != p.cp.CheckoutID {
		return nil, errors.New("invalid_response")
	}
	n, ok := legacyMinor(s["amount_total"])
	c := str(s, "currency")
	if !ok || !legacyCurrency(c) {
		return nil, errors.New("quote_unavailable")
	}
	if str(s, "payment_status") == "paid" || str(s, "status") == "complete" || s["payment_intent"] != nil {
		return nil, errors.New("already_paid")
	}
	return &legacyPaymentSnapshot{Amount: n, Currency: c, TaxReady: true, Open: true, Evidence: &paymentEvidence{CheckoutID: p.cp.CheckoutID, Status: "pending", Currency: c}}, nil
}
func (p *legacyPayment) confirmPage(ctx context.Context) error {
	form := p.cardForm("")
	p.addHostedAttribution(form)
	pm, e := p.stripe(ctx, "POST", "/payment_methods", form, "card_prepare")
	p.in.Card = cardInput{}
	if e != nil {
		return e
	}
	id := str(pm, "id")
	if !regexp.MustCompile(`^pm_[A-Za-z0-9]+$`).MatchString(id) {
		return errors.New("invalid_response")
	}
	if e = p.guard(ctx); e != nil {
		return e
	}
	f := url.Values{"payment_method": {id}, "expected_amount": {strconv.FormatInt(p.quote.Amount, 10)}, "expected_payment_method_type": {"card"}, "eid": {p.cp.EID}, "_stripe_version": {p.version()}}
	p.addHostedAttribution(f)
	if !p.custom {
		f.Set("link_brand", "link")
	}
	data, e := p.stripe(ctx, "POST", "/payment_pages/"+p.cp.CheckoutID+"/confirm", f, "confirming")
	if e != nil {
		return e
	}
	p.latest, e = legacyPageSnapshot(data, p.cp.CheckoutID)
	if e != nil {
		return e
	}
	if !p.custom {
		if e = p.observeHostedPage(data); e != nil {
			return e
		}
		intent := legacyMap(data["payment_intent"])
		if id := str(intent, "id"); id != "" {
			p.cp.IntentID = id
			if secret := str(intent, "client_secret"); secret != "" {
				m := legacySecret.FindStringSubmatch(secret)
				if len(m) != 2 || m[1] != id {
					return errors.New("invalid_response")
				}
				p.cp.Secret = secret
			}
		}
		return p.save(ctx)
	}
	return nil
}
func (p *legacyPayment) confirmCustom(ctx context.Context) error {
	f := p.cardForm("payment_method_data")
	f.Set("setup_future_usage", "off_session")
	token, e := p.stripe(ctx, "POST", "/confirmation_tokens", f, "card_prepare")
	p.in.Card = cardInput{}
	if e != nil {
		return e
	}
	id := str(token, "id")
	if !regexp.MustCompile(`^ctoken_[A-Za-z0-9]+$`).MatchString(id) {
		return errors.New("invalid_response")
	}
	if e = p.guard(ctx); e != nil {
		return e
	}
	data, e := p.request(ctx, "POST", checkoutEndpoint+"/confirm", nil, M{"checkout_session_id": p.cp.CheckoutID, "confirm_token": id, "selected_payment_method_type": "card"}, "confirming")
	if e != nil {
		return e
	}
	for _, node := range []M{data, legacyMap(data["checkout_session"])} {
		if got := str(node, "checkout_session_id"); got != "" && got != p.cp.CheckoutID {
			return errors.New("invalid_response")
		}
	}
	if got := str(legacyMap(data["checkout_session"]), "id"); got != "" && got != p.cp.CheckoutID {
		return errors.New("invalid_response")
	}
	secret := str(data, "client_secret")
	for _, k := range []string{"payment_intent", "stripe_payment_intent", "checkout_session"} {
		if secret == "" {
			secret = str(legacyMap(data[k]), "client_secret")
		}
	}
	m := legacySecret.FindStringSubmatch(secret)
	if len(m) != 2 {
		return errors.New("verification_required")
	}
	p.cp.IntentID, p.cp.Secret = m[1], secret
	if e = p.save(ctx); e != nil {
		return e
	}
	intent, e := p.stripe(ctx, "GET", "/payment_intents/"+p.cp.IntentID, url.Values{"client_secret": {secret}}, "checking")
	if e != nil {
		return e
	}
	p.latest, e = legacyIntentSnapshot(intent, p.cp)
	if e != nil {
		return e
	}
	if p.latest.Amount != p.quote.Amount || p.latest.Currency != p.quote.Currency {
		return errors.New("amount_changed")
	}
	if p.terminal() {
		return nil
	}
	status := str(intent, "status")
	last, hasLast := intent["last_payment_error"]
	method, hasMethod := intent["payment_method"]
	initial := status == "requires_payment_method" && hasLast && last == nil && hasMethod && method == nil
	if (status != "requires_confirmation" && !initial) || str(intent, "confirmation_method") != "automatic" || last != nil || intent["latest_charge"] != nil || intent["next_action"] != nil {
		return errors.New("result_unconfirmed")
	}
	// This is the next step of the same intent, never a second checkout. Only
	// this uninterrupted run can perform it; recovery is GET-only.
	confirm := url.Values{"confirmation_token": {id}, "client_secret": {secret}}
	if raw := str(data, "confirm_return_url"); raw != "" {
		u, e := url.Parse(raw)
		if e != nil || u.Scheme != "https" || u.Host != "chatgpt.com" || u.User != nil || u.Query().Get("stripe_session_id") != p.cp.CheckoutID {
			return errors.New("invalid_response")
		}
		confirm.Set("return_url", raw)
	}
	intent, e = p.stripe(ctx, "POST", "/payment_intents/"+p.cp.IntentID+"/confirm", confirm, "confirming")
	if e != nil {
		return e
	}
	p.latest, e = legacyIntentSnapshot(intent, p.cp)
	return e
}
func (p *legacyPayment) terminal() bool {
	return p.latest != nil && (p.latest.Paid || p.latest.Action || p.latest.Authorized)
}
func (p *legacyPayment) complete() {
	s := p.latest
	quote := p.quote
	if p.in.Mode == "check" {
		n, ok := legacyMinor(p.in.SavedQuote["amount"])
		if ok {
			quote = &legacyPaymentSnapshot{Amount: n, Currency: str(p.in.SavedQuote, "currency")}
		}
	}
	if s == nil || quote == nil || s.Amount != quote.Amount || s.Currency != quote.Currency {
		p.result("review", "result_unconfirmed")
		return
	}
	if s.Paid {
		p.result("succeeded", "")
	} else if s.Action {
		p.result("action_required", "verification_required")
	} else if s.Authorized {
		p.result("review", "payment_authorized")
	} else {
		p.result("review", "result_unconfirmed")
	}
}
