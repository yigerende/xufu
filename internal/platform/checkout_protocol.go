package platform

import (
	"context"
	"net/http"
	"net/url"
	"path"
	"regexp"
	"strings"
	"sync"
	"time"
	"xupay/internal/cslive"
)

type protocolCheckoutClient interface {
	CreateCheckout(context.Context, cslive.Session, cslive.CreateCheckoutOptions) (cslive.CheckoutResult, error)
}

type legacyCheckoutGuard struct {
	base  http.RoundTripper
	guard *protocolCheckoutRun
}

func (t *legacyCheckoutGuard) RoundTrip(req *http.Request) (*http.Response, error) {
	if req.URL.Hostname() != "ipinfo.io" {
		if err := t.guard.guardProxy(req.Context()); err != nil {
			return nil, checkoutFailure(cslive.ErrorCode(err), false, false)
		}
	}
	return t.base.RoundTrip(req)
}

type protocolCheckoutRun struct {
	mu      sync.Mutex
	a       *App
	job     *Job
	country string
	written bool
}

var checkoutInitPath = regexp.MustCompile(`^/v1/payment_pages/cs_(?:live|test)_[A-Za-z0-9]+/init$`)

func (r *protocolCheckoutRun) beforeRequest(ctx context.Context, method, target string) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	u, err := url.Parse(target)
	if err != nil || u.Scheme != "https" || u.User != nil || !browserProxyHost(u.Hostname()) || (u.Port() != "" && u.Port() != "443") {
		return cslive.Failure("invalid_request_url")
	}
	// Standalone checkout has no permission to create a payment method, confirm
	// a charge, upgrade a subscription or cancel renewal.
	allowedWrite := u.Hostname() == "chatgpt.com" && (u.Path == "/backend-api/payments/checkout" || u.Path == "/backend-api/sentinel/req") || u.Hostname() == "api.stripe.com" && checkoutInitPath.MatchString(u.Path)
	if method != "GET" && (method != "POST" || !allowedWrite) {
		return cslive.Failure("invalid_request_url")
	}
	if err = r.guardProxy(ctx); err != nil {
		return err
	}
	if method == "POST" && u.Hostname() == "chatgpt.com" && u.Path == "/backend-api/payments/checkout" {
		if r.written {
			return cslive.Failure("checkout_result_unknown")
		}
		ip, e := r.a.orderExitIP(ctx, r.job.OrderID, r.guardProxy, false)
		if e != nil {
			return e
		}
		if e = r.a.recordOrderProxyIP(ctx, r.job, ip.String()); e != nil {
			return cslive.Failure("proxy_session_unavailable")
		}
		if err = r.a.BeforeSideEffect(ctx, r.job); err != nil {
			return cslive.Failure("checkpoint_failed")
		}
		r.written = true
	}
	return nil
}

func (r *protocolCheckoutRun) guardProxy(ctx context.Context) error {
	var expires time.Time
	var enabled bool
	err := r.a.DB.QueryRow(ctx, `SELECT s.expires_at,p.enabled FROM order_proxy_sessions s
 JOIN resources p ON p.id=s.resource_id JOIN checkout_runs c ON c.order_id=s.order_id
 JOIN proxy_leases l ON l.id=c.proxy_lease_id AND l.resource_id=s.resource_id AND l.expires_at>now()
 JOIN jobs j ON j.order_id=s.order_id AND j.state='running' AND j.lease_token=$2 AND j.lease_until>now()
 WHERE s.order_id=$1 AND s.endpoint_enc<>'' AND c.credentials_expires_at>now()`, r.job.OrderID, r.job.Lease).Scan(&expires, &enabled)
	if err != nil || !enabled {
		return cslive.Failure("proxy_session_unavailable")
	}
	if !expires.After(time.Now()) {
		return cslive.Failure("proxy_session_expired")
	}
	return nil
}

func (a *App) generateProtocolCheckout(ctx context.Context, j *Job, endpoint string, in checkoutInput) (checkoutOutput, error) {
	session, err := protocolSession(in)
	if err != nil {
		return checkoutOutput{}, checkoutFailure("invalid_session", false, false)
	}
	bridge, closeBridge, err := openBrowserProxy(ctx, endpoint)
	if err != nil {
		return checkoutOutput{}, checkoutFailure("proxy_unavailable", false, false)
	}
	defer closeBridge()
	proxyURL, err := url.Parse(bridge.Server)
	if err != nil {
		return checkoutOutput{}, checkoutFailure("proxy_unavailable", false, false)
	}
	proxyURL.User = url.UserPassword(bridge.Username, bridge.Password)
	config := cslive.DefaultConfig()
	config.Proxy, config.Currency = proxyURL.String(), strings.ToUpper(currencyForCountry(in.Country))
	config.Timezone = map[string]string{"PH": "Asia/Manila", "US": "America/New_York", "JP": "Asia/Tokyo", "SG": "Asia/Singapore", "GB": "Europe/London", "EG": "Africa/Cairo", "TR": "Europe/Istanbul"}[in.Country]
	run := &protocolCheckoutRun{a: a, job: j, country: in.Country}
	diagnostics := &protocolRun{a: a, ctx: ctx, id: j.OrderID}
	hooks := cslive.Hooks{BeforeRequest: run.beforeRequest, Progress: diagnostics.progress, Event: diagnostics.event}
	var client protocolCheckoutClient
	if a.checkoutClient != nil {
		client = a.checkoutClient(config, hooks)
	} else {
		client = cslive.NewClient(config, hooks)
	}
	plan := map[string]string{"pro": "pro20x", "team_premium": "team5x"}[in.Product]
	if plan == "" {
		plan = in.Product
	}
	kind := in.CheckoutType
	if kind == "" {
		kind = "hosted"
	}
	if !validCheckoutType(kind) && kind != "auto" {
		return checkoutOutput{}, checkoutFailure("invalid_request", false, false)
	}
	options := cslive.CreateCheckoutOptions{Country: in.Country, Plan: plan, CheckoutType: protocolCheckoutType(kind), Interval: "month"}
	if strings.HasPrefix(plan, "team") {
		options.Seats = in.Seats
		if plan == "team5x" {
			options.Seats5x = in.PremiumSeats
		}
	}
	result, err := client.CreateCheckout(ctx, session, options)
	code := result.ErrorCode
	if err != nil {
		code = cslive.ErrorCode(err)
	}
	if code != "" || err != nil {
		public := protocolPublicCode(code)
		if public == "" {
			public = "invalid_response"
		}
		unknown := run.written || result.CheckoutID != ""
		if result.CheckoutID == "" && (code == "http_400" || code == "http_401" || code == "http_403" || code == "http_404" || code == "http_429") {
			unknown = false
		}
		return checkoutOutput{}, checkoutFailure(public, false, unknown)
	}
	if result.Flow == "subscription_update" {
		return checkoutOutput{}, checkoutFailure("existing_subscription", false, false)
	}
	u, _ := url.Parse(result.CheckoutURL)
	valid := kind != "oaics" && validHostedCheckoutURL(result.CheckoutURL)
	if (kind == "oaics" || kind == "auto") && u != nil && validCustomCheckoutURL(result.CheckoutURL) {
		// Custom links are official checkout routes, never arbitrary redirect URLs.
		valid = u.Path == "/checkout/"+result.ProcessorEntity+"/"+result.CheckoutID
	}
	if !valid || u == nil || path.Base(u.Path) != result.CheckoutID || strings.Contains(result.CheckoutURL, session.AccessToken) || strings.Contains(result.CheckoutURL, session.SessionToken) {
		return checkoutOutput{}, checkoutFailure("checkout_result_unknown", false, run.written || result.CheckoutID != "")
	}
	return checkoutOutput{URL: result.CheckoutURL}, nil
}
