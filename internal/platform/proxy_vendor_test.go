package platform

import (
	"context"
	"net/url"
	"strings"
	"testing"
	"time"
)

func TestProxyVendorModeSessionPolicy(t *testing.T) {
	raw := "http://user:secret_country-us_session-configured_lifetime-20m@geo.iproyal.com:12321"
	for _, vendor := range []string{"iproyal", "cliproxy", "other"} {
		for _, mode := range []string{"dynamic", "static"} {
			t.Run(vendor+"/"+mode, func(t *testing.T) {
				got, life, err := prepareOrderProxy(raw, "PH", vendor, mode)
				if err != nil {
					t.Fatal(err)
				}
				if vendor == "iproyal" && mode == "dynamic" {
					u, _ := url.Parse(got)
					password, _ := u.User.Password()
					if got == raw || !strings.Contains(password, "_session-") || strings.Contains(password, "_session-configured") || !strings.Contains(password, "_country-us") || life != 20*time.Minute {
						t.Fatal("dynamic IPRoyal did not preserve existing session rules")
					}
				} else if vendor == "cliproxy" && mode == "dynamic" {
					u, _ := url.Parse(got)
					password, _ := u.User.Password()
					if !strings.Contains(u.User.Username(), "-region-PH-sid-") || !strings.HasSuffix(u.User.Username(), "-t-15") || password != "secret_country-us_session-configured_lifetime-20m" || life != 15*time.Minute {
						t.Fatal("Cliproxy modified password or missed fifteen-minute username session")
					}
				} else if got != raw || life != time.Hour {
					t.Fatal("explicit vendor/type rewrote supplied credentials")
				}
			})
		}
	}
	got, _, err := prepareOrderProxy("http://user:secret@gateway.example:8080", "PH", "iproyal", "dynamic")
	if err != nil || !strings.Contains(got, "_session-") {
		t.Fatal("explicit vendor still depended on gateway hostname", err)
	}
	for _, raw := range []string{"http://geo.iproyal.com:12321", "http://u:p_lifetime-invalid@geo.iproyal.com:12321"} {
		if got, _, err := prepareOrderProxy(raw, "PH", "iproyal", "static"); err != nil || got != raw {
			t.Fatal("static connection required dynamic session options", err)
		}
	}
}

func TestCliproxyOrderSessionsPreserveLocationAndRotatePerOrder(t *testing.T) {
	for _, raw := range []string{
		"http://account-region-PH:secret@us.arxlabs.io:3010",
		"socks5h://account-region-US-st-Louisiana-sid-old-t-60:secret@sg.cliproxy.io:3010",
		"https://account-region-PH-t-3-sid-old:secret@proxy.example:443",
	} {
		first, duration, err := prepareOrderProxy(raw, "PH", "cliproxy", "dynamic")
		if err != nil || duration != 15*time.Minute {
			t.Fatal(err, duration)
		}
		second, _, err := prepareOrderProxy(raw, "PH", "cliproxy", "dynamic")
		if err != nil || first == second {
			t.Fatal("different orders reused sid", err)
		}
		before, _ := url.Parse(raw)
		after, _ := url.Parse(first)
		password, _ := after.User.Password()
		username := after.User.Username()
		if before.Scheme != after.Scheme || before.Host != after.Host || password != "secret" || strings.Contains(username, "-sid-old") || strings.Count(username, "-sid-") != 1 || strings.Count(username, "-t-") != 1 || !strings.HasSuffix(username, "-t-15") {
			t.Fatal("invalid replacement or unrelated connection fields changed")
		}
		if strings.Contains(raw, "-st-Louisiana") && !strings.HasPrefix(username, "account-region-US-st-Louisiana-sid-") {
			t.Fatal("location selection changed")
		}
	}
}

func TestCliproxyColonSeparatedInput(t *testing.T) {
	for _, password := range []string{"fixture-password", "fixture:p@ss#word%"} {
		raw := "  us.arxlabs.io:3010:fixture-region-PH:" + password + "  "
		normalized, err := normalizeProxy(raw)
		if err != nil {
			t.Fatal("colon-separated proxy rejected")
		}
		u, err := url.Parse(normalized)
		if err != nil || u.User == nil {
			t.Fatal("normalized proxy is invalid")
		}
		actualPassword, _ := u.User.Password()
		if u.Scheme != "http" || u.Host != "us.arxlabs.io:3010" || u.User.Username() != "fixture-region-PH" || actualPassword != password {
			t.Fatal("normalization changed connection fields")
		}
		sticky, duration, err := prepareOrderProxy(raw, "PH", "cliproxy", "dynamic")
		if err != nil || duration != 15*time.Minute {
			t.Fatal("colon-separated proxy failed session preparation")
		}
		u, err = url.Parse(sticky)
		if err != nil || u.User == nil {
			t.Fatal("prepared proxy is invalid")
		}
		actualPassword, _ = u.User.Password()
		if u.Scheme != "http" || u.Host != "us.arxlabs.io:3010" || actualPassword != password || !strings.HasPrefix(u.User.Username(), "fixture-region-PH-sid-") || !strings.HasSuffix(u.User.Username(), "-t-15") {
			t.Fatal("session preparation changed connection fields")
		}
	}
}

func TestProxyVendorMigrationPreservesExplicitChoiceAndCredentials(t *testing.T) {
	h := setupTest(t)
	ctx := context.Background()
	for _, tc := range []struct{ id, endpoint, want string }{
		{"royal", "http://u:p@geo.iproyal.com:12321", "iproyal"},
		{"ordinary", "http://u:p@proxy.example:80", "other"},
		{"similar", "http://u:p@geo.iproyal.com.example:80", "other"},
	} {
		enc, err := h.a.encrypt(tc.endpoint)
		if err != nil {
			t.Fatal(err)
		}
		if _, err = h.a.DB.Exec(ctx, "INSERT INTO resources(id,name,endpoint_enc) VALUES($1,$1,$2)", tc.id, enc); err != nil {
			t.Fatal(err)
		}
		if err = h.a.migrateProxyVendors(ctx); err != nil {
			t.Fatal(err)
		}
		var vendor, mode, after string
		if err = h.a.DB.QueryRow(ctx, "SELECT vendor,ip_mode,endpoint_enc FROM resources WHERE id=$1", tc.id).Scan(&vendor, &mode, &after); err != nil || vendor != tc.want || mode != "dynamic" || after != enc {
			t.Fatal("migration changed prior connection behavior", err)
		}
	}
	if _, err := h.a.DB.Exec(ctx, "UPDATE resources SET vendor='other',ip_mode='static' WHERE id='royal'"); err != nil {
		t.Fatal(err)
	}
	if _, err := h.a.DB.Exec(ctx, schema); err != nil {
		t.Fatal(err)
	}
	if err := h.a.migrateProxyVendors(ctx); err != nil {
		t.Fatal(err)
	}
	var kept bool
	if err := h.a.DB.QueryRow(ctx, "SELECT vendor='other' AND ip_mode='static' FROM resources WHERE id='royal'").Scan(&kept); err != nil || !kept {
		t.Fatal("restart overwrote administrator choice", err)
	}
}

func TestProxyVendorAdminAndOrderBinding(t *testing.T) {
	h := setupTest(t)
	ctx := context.Background()
	body := M{"name": "Chosen supplier", "country": "PH", "endpoint": "http://user:PRIVATE_PROXY_CREDENTIAL@geo.iproyal.com:12321", "vendor": "iproyal", "ip_mode": "dynamic", "enabled": true, "capacity": 4, "weight": 1}
	expect(t, h.request("POST", "/api/admin/resources", body, h.member, nil), 403)
	for key, value := range map[string]string{"vendor": "unknown", "ip_mode": "unknown"} {
		previous := body[key]
		body[key] = value
		expect(t, h.request("POST", "/api/admin/resources", body, h.admin, nil), 400)
		body[key] = previous
	}
	id := str(expect(t, h.request("POST", "/api/admin/resources", body, h.admin, nil), 200), "id")
	bind := func(order string) *paymentProxy {
		t.Helper()
		tx, err := h.a.DB.Begin(ctx)
		if err != nil {
			t.Fatal(err)
		}
		defer tx.Rollback(ctx)
		p, err := h.a.acquireOrderProxyTx(ctx, tx, order, "PH", time.Minute)
		if err != nil {
			t.Fatal(err)
		}
		if err = tx.Commit(ctx); err != nil {
			t.Fatal(err)
		}
		return p
	}
	newOrder := func() string {
		request := prepareRequest()
		request["prepare"] = false
		return str(expect(t, h.request("POST", "/api/orders", request, h.member, map[string]string{"Idempotency-Key": uid()}), 200), "id")
	}
	order := newOrder()
	first := bind(order)
	if !strings.Contains(first.Endpoint, "_session-") {
		t.Fatal("new order ignored selected dynamic IPRoyal")
	}
	body["endpoint"] = ""
	body["ip_mode"] = "static"
	expect(t, h.request("PUT", "/api/admin/resources/"+id, body, h.admin, nil), 409)
	body["ip_mode"], body["vendor"] = "dynamic", "other"
	expect(t, h.request("PUT", "/api/admin/resources/"+id, body, h.admin, nil), 409)
	h.a.releaseProxy(ctx, first.Lease)
	body["ip_mode"] = "static"
	expect(t, h.request("PUT", "/api/admin/resources/"+id, body, h.admin, nil), 200)
	delete(body, "vendor")
	delete(body, "ip_mode")
	body["name"] = "Renamed via older client"
	expect(t, h.request("PUT", "/api/admin/resources/"+id, body, h.admin, nil), 200)
	view := h.request("GET", "/api/admin/resources", nil, h.admin, nil)
	expect(t, view, 200)
	for _, wanted := range []string{`"vendor":"other"`, `"ip_mode":"static"`} {
		if !strings.Contains(view.Body.String(), wanted) {
			t.Fatal("saved selection missing", wanted)
		}
	}
	if strings.Contains(view.Body.String(), "PRIVATE_PROXY_CREDENTIAL") || strings.Contains(view.Body.String(), "geo.iproyal.com") {
		t.Fatal("proxy credentials exposed")
	}
	if existing := bind(order); existing.Endpoint != first.Endpoint {
		t.Fatal("resource edit replaced an existing order session")
	}
	if next := bind(newOrder()); next.Endpoint != "http://user:PRIVATE_PROXY_CREDENTIAL@geo.iproyal.com:12321" {
		t.Fatal("new order ignored explicit static/other choice or lost retained credentials")
	}
}

func TestCliproxyBindingPersistsFifteenMinuteSession(t *testing.T) {
	h := setupTest(t)
	ctx := context.Background()
	body := M{"name": "Cliproxy", "country": "PH", "endpoint": "us.arxlabs.io:3010:fixture-region-PH:fixture-password", "vendor": "cliproxy", "ip_mode": "dynamic", "enabled": true, "capacity": 4, "weight": 1}
	expect(t, h.request("POST", "/api/admin/resources", body, h.admin, nil), 200)
	order := str(expect(t, h.request("POST", "/api/orders", prepareRequest(), h.member, map[string]string{"Idempotency-Key": uid()}), 200), "id")
	j, err := h.a.claim(ctx, "checkout")
	if err != nil || j == nil || j.OrderID != order {
		t.Fatal("checkout did not acquire Cliproxy", err)
	}
	var encrypted string
	var duration float64
	if err = h.a.DB.QueryRow(ctx, "SELECT endpoint_enc,EXTRACT(epoch FROM expires_at-created_at)::float8 FROM order_proxy_sessions WHERE order_id=$1", order).Scan(&encrypted, &duration); err != nil {
		t.Fatal(err)
	}
	endpoint, err := h.a.decrypt(encrypted)
	if err != nil || !strings.Contains(endpoint, "-sid-") || !strings.Contains(endpoint, "-t-15:") || duration != 900 {
		t.Fatal("order did not freeze fifteen-minute Cliproxy session", err, duration)
	}
	u, err := url.Parse(endpoint)
	if err != nil || u.User == nil {
		t.Fatal("saved order endpoint is invalid")
	}
	password, _ := u.User.Password()
	if u.Scheme != "http" || u.Host != "us.arxlabs.io:3010" || !strings.HasPrefix(u.User.Username(), "fixture-region-PH-sid-") || password != "fixture-password" {
		t.Fatal("resource save or order binding changed proxy connection fields")
	}
	if err = h.a.finishCheckout(ctx, j, checkoutOutput{}, checkoutFailure("network", true, false)); err != nil {
		t.Fatal(err)
	}
	if _, err = h.a.DB.Exec(ctx, "UPDATE jobs SET next_at=now() WHERE order_id=$1", order); err != nil {
		t.Fatal(err)
	}
	j, err = h.a.claim(ctx, "checkout")
	if err != nil || j == nil {
		t.Fatal(err)
	}
	var after string
	if err = h.a.DB.QueryRow(ctx, "SELECT endpoint_enc FROM order_proxy_sessions WHERE order_id=$1", order).Scan(&after); err != nil || after != encrypted {
		t.Fatal("retry replaced original Cliproxy sid", err)
	}
}
