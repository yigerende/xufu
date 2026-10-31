package platform

import (
	"context"
	"errors"
	"net/url"
	"regexp"
)

// poll omits price and receipt fields. Bind both identities to the saved page,
// and retain the authorized quote without inventing an amount_received receipt.
func legacyHostedPollSnapshot(data M, cp legacyPaymentCheckpoint, quote *legacyPaymentSnapshot) (*legacyPaymentSnapshot, error) {
	if !regexp.MustCompile(`^ppage_[A-Za-z0-9]+$`).MatchString(cp.PageID) || str(data, "id") != cp.PageID || str(data, "session_id") != cp.CheckoutID {
		return nil, errors.New("invalid_response")
	}
	if str(data, "state") != "succeeded" || str(data, "payment_object_status") != "succeeded" {
		return nil, nil
	}
	if quote == nil || quote.Amount <= 0 || !legacyCurrency(quote.Currency) {
		return nil, errors.New("result_unconfirmed")
	}
	return &legacyPaymentSnapshot{Amount: quote.Amount, Currency: quote.Currency, Paid: true, Attempted: true, TaxReady: true,
		Evidence: &paymentEvidence{CheckoutID: cp.CheckoutID, IntentID: cp.IntentID, Status: "succeeded", Currency: quote.Currency}}, nil
}

func (p *legacyPayment) readHostedPoll(ctx context.Context) (*legacyPaymentSnapshot, error) {
	data, err := p.stripe(ctx, "GET", "/payment_pages/"+p.cp.CheckoutID+"/poll", nil, "checking")
	if err != nil {
		return nil, err
	}
	quote := p.quote
	if p.in.Mode == "check" {
		if amount, ok := legacyMinor(p.in.SavedQuote["amount"]); ok {
			quote = &legacyPaymentSnapshot{Amount: amount, Currency: str(p.in.SavedQuote, "currency")}
		}
	}
	return legacyHostedPollSnapshot(data, p.cp, quote)
}

var legacyHostedConfigID = regexp.MustCompile(`^[A-Za-z0-9_-]{1,128}$`)

func (p *legacyPayment) hostedPageResponse(ctx context.Context, data M) (*legacyPaymentSnapshot, error) {
	if err := p.observeHostedPage(data); err != nil {
		return nil, err
	}
	if _, full := data["object"]; full {
		return legacyPageSnapshot(data, p.cp.CheckoutID)
	}
	// Older partial responses need a separate read; never interpret an Elements
	// session or an acknowledgement as a price or a successful payment.
	return p.read(ctx)
}

func (p *legacyPayment) hostedTaxQuote(ctx context.Context) (*legacyPaymentSnapshot, error) {
	form := url.Values{"eid": {p.cp.EID}}
	for k, v := range p.address() {
		form.Set("tax_region["+k+"]", v.(string))
	}
	data, err := p.stripe(ctx, "POST", "/payment_pages/"+p.cp.CheckoutID, form, "billing")
	if err != nil {
		return nil, err
	}
	s, err := p.hostedPageResponse(ctx, data)
	if err != nil || s.TaxReady || s.Attempted || !s.Open {
		return s, err
	}
	return p.waitTaxQuote(ctx)
}

// A tax update can replace config_id. Keep the latest value returned for this
// checkout, including the final price read before creating the PaymentMethod.
// It is transient: recovery never submits another payment.
func (p *legacyPayment) observeHostedPage(data M) error {
	if p.custom {
		return nil
	}
	for _, key := range []string{"session_id", "checkout_session_id"} {
		if value, present := data[key]; present && value != nil && value != p.cp.CheckoutID {
			return errors.New("invalid_response")
		}
	}
	if page := str(data, "id"); page != "" {
		if !regexp.MustCompile(`^ppage_[A-Za-z0-9]+$`).MatchString(page) || str(data, "session_id") != p.cp.CheckoutID || p.cp.PageID != "" && page != p.cp.PageID {
			return errors.New("invalid_response")
		}
		p.cp.PageID = page
	}
	config, present := data["config_id"]
	if present && config != nil {
		value, ok := config.(string)
		if !ok || !legacyHostedConfigID.MatchString(value) || str(data, "session_id") != p.cp.CheckoutID {
			return errors.New("invalid_response")
		}
		p.hostedConfigID = value
	}
	if eid := str(data, "eid"); eid != "" {
		p.cp.EID = eid
	}
	return nil
}

func (p *legacyPayment) addHostedAttribution(form url.Values) {
	if p.custom {
		return
	}
	form.Set("client_attribution_metadata[checkout_session_id]", p.cp.CheckoutID)
	if p.hostedConfigID != "" {
		form.Set("client_attribution_metadata[checkout_config_id]", p.hostedConfigID)
	}
	form.Set("client_attribution_metadata[merchant_integration_source]", "checkout")
	form.Set("client_attribution_metadata[merchant_integration_version]", "hosted_checkout")
	form.Set("client_attribution_metadata[payment_method_selection_flow]", "automatic")
	// client_session_id, payment_user_agent and verification/device fields belong
	// to a real SDK session; a server HTTP client must not invent or replay them.
}

func (p *legacyPayment) hostedOrigin() string {
	if p.custom || !browserPaymentURL(p.in.URL) {
		return ""
	}
	u, err := url.Parse(p.in.URL)
	if err != nil {
		return ""
	}
	return u.Scheme + "://" + u.Host
}
