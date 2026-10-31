// Adapted from the supplied cs-live-source.zip; see docs/subscription-protocol.md.
package cslive

import (
	context "context"
	fmt "fmt"
	math "math"
	strings "strings"
	time "time"
)

type gptpay_ClientOptions struct {
	Checkpoint   model_CheckpointSink
	EventSink    model_EventSink
	ProgressSink model_ProgressSink
}

type gptpay_PaymentClient struct {
	Config           model_Config
	Checkpoint       model_CheckpointSink
	EventSink        model_EventSink
	ProgressSink     model_ProgressSink
	transportFactory gptpay_transportFactory
	sentinelFactory  func(protocol_HTTPTransport, *model_Checkpoint, fingerprint_Clock) gptpay_tokenSource
}

func gptpay_NewPaymentClient(config model_Config, options ...gptpay_ClientOptions) *gptpay_PaymentClient {
	c := &gptpay_PaymentClient{Config: config}
	if len(options) > 0 {
		c.Checkpoint = options[0].Checkpoint
		c.EventSink = options[0].EventSink
		c.ProgressSink = options[0].ProgressSink
	}
	return c
}

type gptpay_CreateCheckoutOptions struct {
	// PromoCode 仅用于 Team 提链，留空时不发送优惠码。
	PromoCode                             string
	Country, Plan, CheckoutType, Interval string
	Seats, Seats5x                        int
	WithPrice                             bool
	FingerprintClockOffsetHours           int
}

type gptpay_QuoteOptions struct {
	Country, Plan, Interval string
	Seats, Seats5x          int
	Billing                 model_Billing
}

type gptpay_PayOptions struct {
	Country, Plan, CheckoutType, Interval string
	Seats, Seats5x                        int
	Billing                               model_Billing
	ExpectedAmount                        *int64
	CancelRenewal                         *bool
	ApprovePayment                        model_PaymentApproval
	FingerprintClockOffsetHours           int
}

func (c *gptpay_PaymentClient) validateConfig() error {
	if e := protocol_ValidateBrowserProfileSelection(c.Config.BrowserProfile); e != nil {
		return e
	}
	if e := protocol_ValidateProxy(c.Config.Proxy); e != nil {
		return e
	}
	for _, n := range []float64{c.Config.Timeout, c.Config.Deadline} {
		if math.IsNaN(n) || math.IsInf(n, 0) || n <= 0 {
			return fmt.Errorf("invalid timeout or deadline")
		}
	}
	return nil
}

func (c *gptpay_PaymentClient) newCheckpoint(s model_Session, country, plan string, cancel bool, seats, seats5x int, interval string) (model_Checkpoint, error) {
	cp := model_DefaultCheckpoint()
	if e := c.validateConfig(); e != nil {
		return cp, e
	}
	country = strings.ToUpper(country)
	plan = strings.ToLower(plan)
	if model_CheckoutPlan(plan) == "" {
		return cp, fmt.Errorf("unknown plan")
	}
	if !protocol_Matches(`[A-Z]{2}`, country) {
		return cp, fmt.Errorf("invalid country")
	}
	region, ok := model_LookupRegion(country)
	if !ok && (c.Config.Currency == "" || c.Config.Timezone == "") {
		return cp, fmt.Errorf("unknown region: configure currency and timezone")
	}
	if c.Config.Currency != "" {
		region.Currency = strings.ToUpper(c.Config.Currency)
	}
	if c.Config.Timezone != "" {
		region.Timezone = c.Config.Timezone
	}
	if _, e := time.LoadLocation(region.Timezone); e != nil || region.Timezone == "" || region.Timezone == "Local" {
		return cp, fmt.Errorf("invalid timezone")
	}
	if !protocol_Matches(`[A-Z]{3}`, region.Currency) {
		return cp, fmt.Errorf("invalid currency")
	}
	if s.AccessToken == "" || s.SessionToken == "" {
		return cp, fmt.Errorf("Session requires accessToken and sessionToken")
	}
	cfg, err := model_NormalizeTeamSeatConfig(plan, seats, seats5x, interval)
	if err != nil {
		return cp, err
	}
	cp.Plan = plan
	cp.Country = country
	cp.Currency = region.Currency
	cp.Timezone = region.Timezone
	if cfg.Total() > 0 {
		cp.Metadata["team_default_seats"] = int64(cfg.Default)
		cp.Metadata["team_prolite_seats"] = int64(cfg.ProLite)
		cp.Metadata["team_price_interval"] = cfg.Interval
	}
	cp.AccountID = s.AccountID
	cp.Email = s.Email
	cp.AccessToken = s.AccessToken
	cp.SessionToken = s.SessionToken
	cp.Cookies = s.Cookies
	cp.Proxy = c.Config.Proxy
	cp.CancelRequested = cancel
	profile, e := protocol_ResolveBrowserProfile(c.Config.BrowserProfile)
	if e != nil {
		return cp, e
	}
	cp.Metadata["browser_profile"] = profile.ID
	return cp, nil
}

func (c *gptpay_PaymentClient) CreateCheckout(ctx context.Context, s model_Session, o gptpay_CreateCheckoutOptions) (model_CheckoutResult, error) {
	promo, err := gptpay_checkoutPromoCode(o.Plan, o.PromoCode)
	if err != nil {
		return model_CheckoutResult{}, err
	}
	if o.CheckoutType != "" && o.CheckoutType != "oaics" && o.CheckoutType != "hosted" {
		return model_CheckoutResult{}, fmt.Errorf("unknown checkout type")
	}
	clock, e := fingerprint_NewClock(o.FingerprintClockOffsetHours)
	if e != nil {
		return model_CheckoutResult{}, e
	}
	cp, e := c.newCheckpoint(s, o.Country, o.Plan, false, o.Seats, o.Seats5x, o.Interval)
	if e != nil {
		return model_CheckoutResult{}, e
	}
	t, e := gptpay_newTransaction(ctx, c, cp, "create_checkout", clock)
	if e != nil {
		return model_CheckoutResult{}, e
	}
	t.platform.PromoCode = promo
	return t.createCheckout(o.CheckoutType, o.WithPrice), nil
}

func (c *gptpay_PaymentClient) CancelRenewal(ctx context.Context, s model_Session, country string) (model_Result, error) {
	if country == "" {
		return model_Result{}, fmt.Errorf("country is required")
	}
	clock, e := fingerprint_NewClock(0)
	if e != nil {
		return model_Result{}, e
	}
	cp, e := c.newCheckpoint(s, country, "plus", true, 0, 0, "")
	if e != nil {
		return model_Result{}, e
	}
	t, e := gptpay_newTransaction(ctx, c, cp, "pay", clock)
	if e != nil {
		return model_Result{}, e
	}
	return t.cancelRenewalOnly(), nil
}

func (c *gptpay_PaymentClient) Pay(ctx context.Context, s model_Session, card model_Card, o gptpay_PayOptions) (model_Result, error) {
	if c.Checkpoint == nil {
		return model_Result{}, fmt.Errorf("payment requires a synchronous durable checkpoint sink")
	}
	if e := card.Validate(); e != nil {
		return model_Result{}, e
	}
	if e := o.Billing.Validate(); e != nil {
		return model_Result{}, e
	}
	if o.ExpectedAmount != nil && *o.ExpectedAmount < 0 {
		return model_Result{}, fmt.Errorf("invalid expected amount")
	}
	if o.CheckoutType != "" && o.CheckoutType != "oaics" && o.CheckoutType != "hosted" {
		return model_Result{}, fmt.Errorf("invalid checkout type")
	}
	clock, e := fingerprint_NewClock(o.FingerprintClockOffsetHours)
	if e != nil {
		return model_Result{}, e
	}
	cancel := true
	if o.CancelRenewal != nil {
		cancel = *o.CancelRenewal
	}
	cp, e := c.newCheckpoint(s, o.Country, o.Plan, cancel, o.Seats, o.Seats5x, o.Interval)
	if e != nil {
		return model_Result{}, e
	}
	cp.Metadata["fingerprint_clock_offset_hours"] = clock.OffsetHours
	t, e := gptpay_newTransaction(ctx, c, cp, "pay", clock)
	if e != nil {
		return model_Result{}, e
	}
	return t.execute(o.Billing, &card, o.ExpectedAmount, o.ApprovePayment, o.CheckoutType), nil
}

func (c *gptpay_PaymentClient) Quote(ctx context.Context, s model_Session, o gptpay_QuoteOptions) (model_Result, error) {
	if e := o.Billing.Validate(); e != nil {
		return model_Result{}, e
	}
	cp, e := c.newCheckpoint(s, o.Country, o.Plan, false, o.Seats, o.Seats5x, o.Interval)
	if e != nil {
		return model_Result{}, e
	}
	clock, _ := fingerprint_NewClock(0)
	t, e := gptpay_newTransaction(ctx, c, cp, "quote", clock)
	if e != nil {
		return model_Result{}, e
	}
	return t.execute(o.Billing, nil, nil, nil, ""), nil
}

func (c *gptpay_PaymentClient) Recover(ctx context.Context, cp model_Checkpoint) (model_Result, error) {
	if c.Config.BrowserProfile != "" {
		return model_Result{}, fmt.Errorf("recovery uses the checkpoint browser profile; remove the profile override")
	}
	if e := cp.ValidateForRecovery(); e != nil {
		return model_Result{}, e
	}
	if e := c.validateConfig(); e != nil {
		return model_Result{}, e
	}
	if c.Checkpoint == nil {
		return model_Result{}, fmt.Errorf("recovery requires a synchronous durable checkpoint sink")
	}
	clone, e := cp.Clone()
	if e != nil {
		return model_Result{}, e
	}
	offset, _ := protocol_Integer(clone.Metadata["fingerprint_clock_offset_hours"])
	clock, e := fingerprint_NewClock(int(offset))
	if e != nil {
		return model_Result{}, e
	}
	t, e := gptpay_newTransaction(ctx, c, clone, "recover", clock)
	if e != nil {
		return model_Result{}, e
	}
	return t.recover(), nil
}
