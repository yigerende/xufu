package platform

import (
	"context"
	"errors"
	"net"
	"net/url"
	"regexp"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"
)

var royalSession = regexp.MustCompile(`(?i)_session-[^_]*`)
var royalLifetime = regexp.MustCompile(`(?i)_lifetime-([^_]+)`)

// Only IPRoyal's residential gateway needs generated supplier session options.
// Other proxies use their configured credentials unchanged. The order binding
// and its credential retention deadline are local safeguards, not supplier sessions.
// Prepare once per order, never per HTTP request or safe retry.
func prepareOrderProxy(raw, country string) (string, time.Duration, error) {
	normalized, err := normalizeProxy(raw)
	if err != nil {
		return "", 0, checkoutFailure("proxy_unavailable", false, false)
	}
	u, err := url.Parse(normalized)
	if err != nil {
		return "", 0, checkoutFailure("proxy_unavailable", false, false)
	}
	lifetime := time.Hour
	if !strings.EqualFold(strings.TrimSuffix(u.Hostname(), "."), "geo.iproyal.com") {
		// Do not append, replace or validate another provider's password options.
		return normalized, lifetime, nil
	}
	if u.User == nil {
		return "", 0, checkoutFailure("proxy_unavailable", false, false)
	}
	password, ok := u.User.Password()
	if !ok || password == "" || u.User.Username() == "" {
		return "", 0, checkoutFailure("proxy_unavailable", false, false)
	}
	if !strings.Contains(strings.ToLower(password), "_country-") {
		password += "_country-" + strings.ToLower(country)
	}
	token := "_session-" + uid()[:16]
	if royalSession.MatchString(password) {
		password = royalSession.ReplaceAllString(password, token)
	} else {
		password += token
	}
	if matches := royalLifetime.FindStringSubmatch(password); len(matches) > 1 {
		lifetime, err = time.ParseDuration(strings.ToLower(matches[1]))
		if err != nil || lifetime < time.Minute {
			return "", 0, checkoutFailure("proxy_session_expired", false, false)
		}
	} else {
		password += "_lifetime-1h"
	}
	// Retain private credentials only as long as needed for this checkout.
	if lifetime > 24*time.Hour {
		lifetime = 24 * time.Hour
	}
	u.User = url.UserPassword(u.User.Username(), password)
	return u.String(), lifetime, nil
}

func (a *App) acquireOrderProxyTx(ctx context.Context, tx pgx.Tx, order, country string, duration time.Duration) (*paymentProxy, error) {
	if _, err := tx.Exec(ctx, "SELECT pg_advisory_xact_lock(1886003)"); err != nil {
		return nil, err
	}
	var resource, enc string
	var expires time.Time
	err := tx.QueryRow(ctx, "SELECT COALESCE(resource_id,''),endpoint_enc,expires_at FROM order_proxy_sessions WHERE order_id=$1 FOR UPDATE", order).Scan(&resource, &enc, &expires)
	if err == pgx.ErrNoRows {
		p, e := a.acquireProxyTx(ctx, tx, country, duration)
		if e != nil {
			return nil, e
		}
		endpoint, lifetime, e := prepareOrderProxy(p.Endpoint, country)
		if e != nil {
			if _, cleanup := tx.Exec(ctx, "DELETE FROM proxy_leases WHERE id=$1", p.Lease); cleanup != nil {
				return nil, cleanup
			}
			return nil, e
		}
		encrypted, e := a.encrypt(endpoint)
		if e != nil {
			return nil, e
		}
		_, e = tx.Exec(ctx, "INSERT INTO order_proxy_sessions(order_id,resource_id,endpoint_enc,expires_at) VALUES($1,$2,$3,now()+($4*interval '1 second'))", order, p.ID, encrypted, int(lifetime.Seconds()))
		if e != nil {
			return nil, e
		}
		p.Endpoint = endpoint
		return p, nil
	}
	if err != nil {
		return nil, err
	}
	if enc == "" || !expires.After(time.Now().Add(15*time.Second)) {
		return nil, checkoutFailure("proxy_session_expired", false, false)
	}
	var enabled bool
	var capacity int
	if resource == "" {
		return nil, checkoutFailure("proxy_unavailable", false, false)
	}
	if err = tx.QueryRow(ctx, "SELECT enabled,capacity FROM resources WHERE id=$1 AND country=$2", resource, country).Scan(&enabled, &capacity); err != nil || !enabled {
		return nil, checkoutFailure("proxy_unavailable", false, false)
	}
	p := &paymentProxy{ID: resource}
	p.Endpoint, err = a.decrypt(enc)
	if err != nil {
		return nil, checkoutFailure("proxy_unavailable", false, false)
	}
	// The subscription keeps its checkout slot through the payment handoff.
	err = tx.QueryRow(ctx, "SELECT l.id FROM proxy_leases l JOIN checkout_runs c ON c.proxy_lease_id=l.id WHERE c.order_id=$1 AND l.resource_id=$2 AND l.expires_at>now()", order, resource).Scan(&p.Lease)
	if err != nil && err != pgx.ErrNoRows {
		return nil, err
	}
	if p.Lease == "" {
		var used int
		if err = tx.QueryRow(ctx, "SELECT count(*) FROM proxy_leases WHERE resource_id=$1 AND expires_at>now()", resource).Scan(&used); err != nil {
			return nil, err
		}
		if used >= capacity {
			return nil, ErrProxyUnavailable
		}
		p.Lease = uid()
		if _, err = tx.Exec(ctx, "INSERT INTO proxy_leases(id,resource_id,expires_at) VALUES($1,$2,now()+($3*interval '1 second'))", p.Lease, resource, int(duration.Seconds())); err != nil {
			return nil, err
		}
	} else {
		if _, err = tx.Exec(ctx, "UPDATE proxy_leases SET expires_at=now()+($2*interval '1 second') WHERE id=$1", p.Lease, int(duration.Seconds())); err != nil {
			return nil, err
		}
	}
	return p, nil
}

func (a *App) recordOrderProxyIP(ctx context.Context, j *Job, ip string) error {
	parsed := net.ParseIP(ip)
	if parsed == nil {
		return checkoutFailure("proxy_unavailable", true, false)
	}
	tag, err := a.DB.Exec(ctx, `UPDATE order_proxy_sessions s SET expected_ip=CASE WHEN s.expected_ip='' THEN $3 ELSE s.expected_ip END WHERE s.order_id=$1 AND s.expires_at>now()
 AND EXISTS(SELECT 1 FROM jobs j WHERE j.order_id=s.order_id AND j.lease_token=$2 AND j.state='running' AND j.lease_until>now())`, j.OrderID, j.Lease, parsed.String())
	if err != nil {
		return err
	}
	if tag.RowsAffected() != 1 {
		return checkoutFailure("proxy_session_unavailable", false, false)
	}
	return nil
}

// A missing/expired historical session is never replaced with a fresh IP.
func (a *App) paymentOrderProxy(ctx context.Context, id, attempt string, ttl int) (*paymentProxy, string, time.Time, error) {
	tx, err := a.DB.Begin(ctx)
	if err != nil {
		return nil, "", time.Time{}, err
	}
	defer tx.Rollback(ctx)
	var country, expected, workflow string
	var expires time.Time
	err = tx.QueryRow(ctx, `SELECT o.country,s.expected_ip,s.expires_at,o.workflow FROM orders o JOIN order_proxy_sessions s ON s.order_id=o.id JOIN payment_runs p ON p.order_id=o.id
 WHERE o.id=$1 AND p.attempt_id=$2 AND p.state IN ('preparing','checking') AND p.expires_at>now()`, id, attempt).Scan(&country, &expected, &expires, &workflow)
	if err != nil || (workflow != "subscription" && net.ParseIP(expected) == nil) {
		return nil, "", time.Time{}, checkoutFailure("proxy_session_unavailable", false, false)
	}
	p, err := a.acquireOrderProxyTx(ctx, tx, id, country, time.Duration(ttl+15)*time.Second)
	if err != nil {
		return nil, "", time.Time{}, err
	}
	if _, err = tx.Exec(ctx, "UPDATE checkout_runs SET proxy_lease_id=$2 WHERE order_id=$1", id, p.Lease); err != nil {
		return nil, "", time.Time{}, err
	}
	if err = tx.Commit(ctx); err != nil {
		return nil, "", time.Time{}, err
	}
	return p, expected, expires, nil
}

func proxyPaymentCode(err error) string {
	var e checkoutError
	if errors.As(err, &e) && (e.Code == "proxy_session_expired" || e.Code == "proxy_session_changed") {
		return e.Code
	}
	return "proxy_session_unavailable"
}
