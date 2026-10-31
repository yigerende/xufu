package platform

import (
	"context"
	"net/url"
	"strconv"
	"strings"
	"testing"
	"time"
)

func TestIPRoyalOrderSession(t *testing.T) {
	for _, raw := range []string{"http://user:secret@geo.iproyal.com:12321", "socks5h://user:secret_country-ph_session-old_lifetime-20m@geo.iproyal.com:12321", "https://user%40mail:pass%3Aword@geo.iproyal.com:12321"} {
		a, life, e := prepareOrderProxy(raw, "PH")
		if e != nil {
			t.Fatal(e)
		}
		b, _, e := prepareOrderProxy(raw, "PH")
		if e != nil || a == b {
			t.Fatal("different orders reused session")
		}
		u, _ := url.Parse(a)
		p, _ := u.User.Password()
		if !strings.Contains(p, "_country-ph") || !strings.Contains(p, "_session-") || strings.Contains(p, "_session-old") {
			t.Fatal("incorrect country or session")
		}
		if life < time.Minute || life > 24*time.Hour {
			t.Fatal("invalid lifetime")
		}
	}
	for _, raw := range []string{"http://u:pass@static.iproyal.com:80", "http://u:pass@geo.iproyal.com.evil.test:80"} {
		got, _, e := prepareOrderProxy(raw, "PH")
		if e != nil || got != raw {
			t.Fatal("modified unrelated supplier")
		}
	}
	for _, raw := range []string{"http://geo.iproyal.com:12321", "http://u:p_lifetime-bad@geo.iproyal.com:12321"} {
		if _, _, e := prepareOrderProxy(raw, "PH"); e == nil {
			t.Fatal("invalid supplier settings accepted")
		}
	}
}

func TestNonIPRoyalOrderProxyPreservesCredentials(t *testing.T) {
	for _, tc := range []struct {
		name, raw, normalized string
	}{
		{"http", "http://user:password@proxy.example:8080", "http://user:password@proxy.example:8080"},
		{"https", "https://user%40mail:pass%3Aword@proxy.example:443", "https://user%40mail:pass%3Aword@proxy.example:443"},
		{"socks5", "socks5://user:password@proxy.example:1080", "socks5://user:password@proxy.example:1080"},
		{"socks5h", "socks5h://user:password@proxy.example:1080", "socks5h://user:password@proxy.example:1080"},
		{"without_auth", "http://proxy.example:8080", "http://proxy.example:8080"},
		{"static_iproyal", "http://u:p@static.iproyal.com:80", "http://u:p@static.iproyal.com:80"},
		{"similar_hostname", "http://u:p@geo.iproyal.com.example:80", "http://u:p@geo.iproyal.com.example:80"},
		{"literal_password_options", "http://u:secret_session-original_lifetime-not-a-duration@proxy.example:80", "http://u:secret_session-original_lifetime-not-a-duration@proxy.example:80"},
		{"compact_format", "proxy.example:8080:user:pass:word", "http://user:pass%3Aword@proxy.example:8080"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			// New orders must not randomize static or independently managed credentials.
			for i := 0; i < 2; i++ {
				got, _, err := prepareOrderProxy(tc.raw, "PH")
				if err != nil {
					t.Fatal("ordinary proxy required supplier session options", err)
				}
				if got != tc.normalized {
					t.Fatal("ordinary proxy credentials or endpoint were rewritten")
				}
			}
		})
	}
}

func TestOrderProxyRetryKeepsSessionWithoutIPComparison(t *testing.T) {
	h := setupTest(t)
	ctx := context.Background()
	resource := h.addProxy("PH", 1)
	endpoint, _ := h.a.encrypt("http://u:secret_country-ph@geo.iproyal.com:12321")
	h.a.DB.Exec(ctx, "UPDATE resources SET endpoint_enc=$2 WHERE id=$1", resource, endpoint)
	v := expect(t, h.request("POST", "/api/orders", prepareRequest(), h.member, map[string]string{"Idempotency-Key": uid()}), 200)
	id := str(v, "id")
	j, e := h.a.claim(ctx, "checkout")
	if e != nil || j == nil {
		t.Fatal(e)
	}
	if e = h.a.recordOrderProxyIP(ctx, j, "203.0.113.2"); e != nil {
		t.Fatal(e)
	}
	var first string
	h.a.DB.QueryRow(ctx, "SELECT endpoint_enc FROM order_proxy_sessions WHERE order_id=$1", id).Scan(&first)
	if e = h.a.finishCheckout(ctx, j, checkoutOutput{}, checkoutFailure("network", true, false)); e != nil {
		t.Fatal(e)
	}
	h.a.DB.Exec(ctx, "UPDATE jobs SET next_at=now() WHERE order_id=$1", id)
	j, e = h.a.claim(ctx, "checkout")
	if e != nil || j == nil {
		t.Fatal(e)
	}
	var next string
	h.a.DB.QueryRow(ctx, "SELECT endpoint_enc FROM order_proxy_sessions WHERE order_id=$1", id).Scan(&next)
	if first != next {
		t.Fatal("safe retry replaced encrypted proxy session")
	}
	if e = h.a.recordOrderProxyIP(ctx, j, "203.0.113.3"); e != nil {
		t.Fatal("observed IP change must not reject the original proxy", e)
	}
	var observed string
	h.a.DB.QueryRow(ctx, "SELECT expected_ip FROM order_proxy_sessions WHERE order_id=$1", id).Scan(&observed)
	if observed != "203.0.113.2" {
		t.Fatal("initial observation was overwritten")
	}
	if e = h.a.recordOrderProxyIP(ctx, j, "203.0.113.2"); e != nil {
		t.Fatal(e)
	}
}

func TestNetworkDriftStopsWithoutRebuilding(t *testing.T) {
	h, id, p := subscriptionFixture(t, true)
	ctx := context.Background()
	completeSubscriptionCheckout(t, h, id)
	time.Sleep(1200 * time.Millisecond)
	var endpoint string
	h.a.DB.QueryRow(ctx, "SELECT endpoint_enc FROM order_proxy_sessions WHERE order_id=$1", id).Scan(&endpoint)
	p.events <- paymentEvent{Type: "result", State: "failed", Code: "proxy_session_changed"}
	waitPayment(t, h, id, "failed")
	paymentBalances(t, h, 100000, 0)
	var restarts, links int
	var after string
	h.a.DB.QueryRow(ctx, "SELECT network_restarts FROM payment_runs WHERE order_id=$1", id).Scan(&restarts)
	h.a.DB.QueryRow(ctx, "SELECT count(*) FROM order_checkout_links WHERE order_id=$1", id).Scan(&links)
	h.a.DB.QueryRow(ctx, "SELECT endpoint_enc FROM order_proxy_sessions WHERE order_id=$1", id).Scan(&after)
	if restarts != 0 || links != 1 || endpoint == "" || endpoint != after {
		t.Fatal("order proxy or checkout replaced")
	}
	assertSubscriptionPrivate(t, h, id)
}

func TestNeverRebuildPaidOrUncertainPayment(t *testing.T) {
	h, id, _ := subscriptionFixture(t, true)
	ctx := context.Background()
	completeSubscriptionCheckout(t, h, id)
	deadline := time.Now().Add(3 * time.Second)
	for {
		var link, cred, fee string
		h.a.DB.QueryRow(ctx, "SELECT p.hosted_url_enc,c.credentials_enc,p.fee_state FROM payment_runs p JOIN checkout_runs c ON c.order_id=p.order_id WHERE p.order_id=$1", id).Scan(&link, &cred, &fee)
		if link != "" {
			if cred == "" || fee != "held" {
				t.Fatal("fixture not eligible for safe restart")
			}
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("runtime not ready")
		}
		time.Sleep(20 * time.Millisecond)
	}
	var attempt string
	h.a.DB.QueryRow(ctx, "SELECT attempt_id FROM payment_runs WHERE order_id=$1", id).Scan(&attempt)
	for _, state := range []string{"succeeded", "review", "checking", "action_required"} {
		h.a.DB.Exec(ctx, "UPDATE payment_runs SET state=$2,submitted=true WHERE order_id=$1", id, state)
		ok, e := h.a.restartPaymentNetwork(ctx, id, attempt, paymentEvent{Type: "result", State: "failed", Code: "proxy_session_changed"})
		if e != nil || ok {
			t.Fatal("unsafe payment restarted", state, e)
		}
	}
	h.a.DB.Exec(ctx, "UPDATE payment_runs SET state='submitting',submitted=true WHERE order_id=$1", id)
	for _, event := range []paymentEvent{{Type: "result", State: "failed", Code: "proxy_session_changed", Sent: true}, {Type: "result", State: "unknown", Code: "proxy_session_changed"}, {Type: "result", State: "failed", Code: "result_unconfirmed"}} {
		ok, e := h.a.restartPaymentNetwork(ctx, id, attempt, event)
		if e != nil || ok {
			t.Fatal("uncertain submission restarted")
		}
	}
}

func TestSubmissionObservedNeverRestartsEvenOnContradictoryFailure(t *testing.T) {
	h, id, p := subscriptionFixture(t, true)
	ctx := context.Background()
	completeSubscriptionCheckout(t, h, id)
	p.events <- paymentEvent{Type: "quote", Amount: 1500, Currency: "php"}
	waitPayment(t, h, id, "submitting")
	p.events <- paymentEvent{Type: "submitted"}
	p.events <- paymentEvent{Type: "result", State: "failed", Code: "proxy_session_changed", Sent: false}
	waitPayment(t, h, id, "review")
	paymentBalances(t, h, 90000, 10000)
	var n int
	h.a.DB.QueryRow(ctx, "SELECT network_restarts FROM payment_runs WHERE order_id=$1", id).Scan(&n)
	if n != 0 {
		t.Fatal("submitted payment rebuilt")
	}
}

func TestCheckoutNetworkDriftNeverRebuilds(t *testing.T) {
	for _, submitted := range []bool{false, true} {
		t.Run(strconv.FormatBool(submitted), func(t *testing.T) {
			h, id, _ := subscriptionFixture(t, true)
			ctx := context.Background()
			h.addProxy("PH", 1)
			j, e := h.a.claim(ctx, "checkout")
			if e != nil || j == nil {
				t.Fatal(e)
			}
			if submitted {
				if e = h.a.BeforeSideEffect(ctx, j); e != nil {
					t.Fatal(e)
				}
			}
			if e = h.a.finishCheckout(ctx, j, checkoutOutput{}, checkoutFailure("proxy_session_changed", false, submitted)); e != nil {
				t.Fatal(e)
			}
			var n int
			h.a.DB.QueryRow(ctx, "SELECT network_restarts FROM payment_runs WHERE order_id=$1", id).Scan(&n)
			if n != 0 {
				t.Fatal("checkout rotated proxy")
			}
			j, e = h.a.claim(ctx, "checkout")
			if e != nil || j != nil {
				t.Fatal("checkout was requeued", e)
			}
		})
	}
}

func TestTraceSanitizesCodesAndHonorsAdminBoundary(t *testing.T) {
	h, id, p := subscriptionFixture(t, true)
	ctx := context.Background()
	completeSubscriptionCheckout(t, h, id)
	h.a.paymentTrace(ctx, id, "billing", "failed", "https://private.example/RAW_SECRET", time.Second)
	p.events <- paymentEvent{Type: "quote", Amount: 1500, Currency: "php"}
	waitPayment(t, h, id, "submitting")
	p.events <- paymentEvent{Type: "result", State: "succeeded", Sent: true, Amount: 1501, Currency: "php"}
	waitPayment(t, h, id, "review")
	var state string
	h.a.DB.QueryRow(ctx, "SELECT state FROM payment_trace_events WHERE order_id=$1 AND phase='result' ORDER BY id DESC LIMIT 1", id).Scan(&state)
	if state != "review" {
		t.Fatal("mismatched paid amount falsely shown as success")
	}
	response := h.request("GET", "/api/admin/orders/"+id, nil, h.admin, nil)
	expect(t, response, 200)
	if strings.Contains(response.Body.String(), "RAW_SECRET") {
		t.Fatal("raw diagnostic exposed")
	}
	expect(t, h.request("GET", "/api/admin/orders/"+id, nil, nil, nil), 401)
	assertSubscriptionPrivate(t, h, id)
}
