package platform

import (
	"context"
	"errors"
	"net/http"
	"net/url"
	"time"

	"github.com/jackc/pgx/v5"
)

var ErrProxyUnavailable = errors.New("no available payment proxy")

type paymentProxy struct{ ID, Lease, Endpoint, Vendor, IPMode string }

// All processes share the capacity lock. A task retains one proxy throughout
// its upstream requests; the absence of a proxy never permits a direct request.
// The configured default is preferred only within the order's country pool.
func (a *App) acquireProxyTx(ctx context.Context, tx pgx.Tx, country string, duration time.Duration) (*paymentProxy, error) {
	if _, e := tx.Exec(ctx, "SELECT pg_advisory_xact_lock(1886003)"); e != nil {
		return nil, e
	}
	var cfg M
	if e := tx.QueryRow(ctx, "SELECT value FROM settings WHERE key='proxy'").Scan(&cfg); e != nil {
		return nil, e
	}
	p := &paymentProxy{Lease: uid()}
	var enc string
	e := tx.QueryRow(ctx, `SELECT r.id,r.endpoint_enc,r.vendor,r.ip_mode FROM resources r
 WHERE r.enabled AND r.kind='proxy' AND r.country=$2
 AND (SELECT count(*) FROM proxy_leases l WHERE l.resource_id=r.id AND l.expires_at>now())<r.capacity
 ORDER BY (r.id=$1) DESC,(r.last_status='failed'),random()*r.weight DESC,r.id LIMIT 1`, str(cfg, "default_id"), country).Scan(&p.ID, &enc, &p.Vendor, &p.IPMode)
	if e == pgx.ErrNoRows {
		return nil, ErrProxyUnavailable
	}
	if e != nil {
		return nil, e
	}
	p.Endpoint, e = a.decrypt(enc)
	if e != nil {
		return nil, errors.New("proxy credentials unavailable")
	}
	if _, e = tx.Exec(ctx, "INSERT INTO proxy_leases(id,resource_id,expires_at) VALUES($1,$2,now()+($3*interval '1 second'))", p.Lease, p.ID, int(duration.Seconds())); e != nil {
		return nil, e
	}
	return p, nil
}

func (a *App) releaseProxy(ctx context.Context, lease string) {
	if lease != "" {
		a.DB.Exec(ctx, "DELETE FROM proxy_leases WHERE id=$1", lease)
	}
}

type proxyOnlyTransport struct{ transport *http.Transport }

func (t proxyOnlyTransport) RoundTrip(r *http.Request) (*http.Response, error) {
	if r.URL.Scheme != "https" || r.URL.User != nil || (r.URL.Port() != "" && r.URL.Port() != "443") {
		return nil, errors.New("upstream target denied")
	}
	switch r.URL.Hostname() {
	case "chatgpt.com", "api.stripe.com", "ipinfo.io", "api.openai.com":
	default:
		return nil, errors.New("upstream target denied")
	}
	return t.transport.RoundTrip(r)
}

func newPaymentClient(endpoint string, timeout time.Duration) (*http.Client, func(), error) {
	raw, e := normalizeProxy(endpoint)
	if e != nil {
		return nil, nil, e
	}
	proxy, e := url.Parse(raw)
	if e != nil {
		return nil, nil, errors.New("invalid proxy")
	}
	dial, e := configuredProxyDialer()
	if e != nil {
		return nil, nil, e
	}
	tr := &http.Transport{Proxy: http.ProxyURL(proxy), DialContext: dial, TLSHandshakeTimeout: 10 * time.Second, ResponseHeaderTimeout: timeout, MaxConnsPerHost: 2, MaxIdleConns: 2}
	return &http.Client{Transport: proxyOnlyTransport{tr}, Timeout: timeout, CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}, tr.CloseIdleConnections, nil
}
