package platform

import (
	"context"
	"encoding/json"
	"fmt"
	"github.com/jackc/pgx/v5"
	"io"
	"net"
	"net/http"
	"net/url"
	"regexp"
	"strconv"
	"strings"
	"time"
)

func (a *App) settings(w http.ResponseWriter, r *http.Request) {
	items, e := a.query(r.Context(), "SELECT key,value FROM settings ORDER BY key")
	if e != nil {
		fail(w, 500, "读取配置失败")
		return
	}
	m := M{}
	for _, x := range items {
		m[x["key"].(string)] = x["value"]
	}
	out(w, 200, m)
}
func validateConfig(key string, v M) error {
	allowed := map[string][]string{
		"site":      {"name", "english_name", "tagline", "registration_open", "support_url", "terms", "billing_country", "billing_mode", "invite_rate"},
		"risk":      {"login_per_minute", "api_per_minute", "order_per_minute", "max_open_orders", "max_topup", "session_hours", "event_retention_days"},
		"scheduler": {"paused", "max_parallel", "per_user_parallel", "retry_limit", "retry_delay_seconds", "task_timeout_seconds", "queue_limit", "default_country", "link_mode", "engine"},
		"billing":   {"country", "allow_customer_edit", "address_templates"}, "tiers": {"items"},
		"proxy":   {"default_id", "global_id", "match_country", "request_timeout_seconds"},
		"payment": {"enabled", "checkout_flow", "subscription_flow", "checkout_type", "checkout_types", "max_parallel", "per_user_parallel", "prepare_timeout_seconds", "quote_timeout_seconds", "result_timeout_seconds", "requests_per_minute", "subscription_wait_seconds", "renewal_check_interval_seconds", "renewal_check_limit", "network_restart_limit", "activation_check_interval_seconds", "activation_check_limit"},
	}
	keys, ok := allowed[key]
	if !ok {
		return bad("不支持的配置组")
	}
	for k := range v {
		found := false
		for _, x := range keys {
			if x == k {
				found = true
				break
			}
		}
		if !found {
			return bad("未知配置字段：" + k)
		}
	}
	for _, k := range keys {
		if _, ok := v[k]; !ok {
			if (key == "proxy" && k == "global_id") || (key == "payment" && (k == "network_restart_limit" || k == "subscription_flow" || k == "checkout_flow" || k == "checkout_type" || k == "checkout_types")) {
				continue
			}
			return bad("缺少配置字段：" + k)
		}
	}
	number := func(k string, min, max int) error {
		x, ok := v[k].(float64)
		if !ok || x != float64(int(x)) || x < float64(min) || x > float64(max) {
			return bad(fmt.Sprintf("%s 需为 %d～%d 的整数", k, min, max))
		}
		return nil
	}
	boolean := func(k string) error {
		if _, ok := v[k].(bool); !ok {
			return bad(k + " 必须为开关")
		}
		return nil
	}
	text := func(k string, min, max int) error {
		s, ok := v[k].(string)
		if !ok || len(s) < min || len(s) > max {
			return bad(k + " 内容长度无效")
		}
		return nil
	}
	switch key {
	case "payment":
		if value, present := v["checkout_types"]; present {
			types, ok := value.(M)
			if !ok || len(types) != len(checkoutProducts) {
				return bad("请为每个订阅套餐选择长链或短链")
			}
			for _, product := range checkoutProducts {
				if !validCheckoutType(str(types, product.ID)) {
					return bad(product.Name + " 的结账链接类型须选择长链或短链")
				}
			}
		}
		if _, present := v["checkout_type"]; present && !validCheckoutType(str(v, "checkout_type")) {
			return bad("结账链接类型须选择长链或短链")
		}
		if _, present := v["subscription_flow"]; present && !validSubscriptionFlow(str(v, "subscription_flow")) {
			return bad("请选择旧浏览器、旧协议或新流程")
		}
		if _, present := v["checkout_flow"]; present && !validSubscriptionFlow(str(v, "checkout_flow")) {
			return bad("结账链接生成流程须选择旧流程或新流程")
		}
		if _, old := v["checkout_flow"]; old {
			if _, selected := v["subscription_flow"]; !selected || str(v, "checkout_flow") != str(v, "subscription_flow") {
				return bad("请选择同一套完整订阅流程，不再支持新旧提链和付款混搭；请刷新管理页面")
			}
		}
		if e := boolean("enabled"); e != nil {
			return e
		}
		for k, b := range map[string][2]int{"max_parallel": {1, 20}, "per_user_parallel": {1, 5}, "prepare_timeout_seconds": {30, 300}, "quote_timeout_seconds": {30, 600}, "result_timeout_seconds": {30, 600}, "requests_per_minute": {1, 20}, "subscription_wait_seconds": {30, 1800}, "renewal_check_interval_seconds": {5, 300}, "renewal_check_limit": {1, 60}, "network_restart_limit": {0, 5}, "activation_check_interval_seconds": {2, 300}, "activation_check_limit": {1, 120}} {
			if k == "network_restart_limit" {
				if _, exists := v[k]; !exists {
					continue
				}
			}
			if e := number(k, b[0], b[1]); e != nil {
				return e
			}
		}
		if num(v, "per_user_parallel", 1) > num(v, "max_parallel", 2) {
			return bad("用户并发不能大于付款总并发")
		}
	case "proxy":
		if _, exists := v["global_id"]; exists {
			if e := text("global_id", 0, 64); e != nil {
				return e
			}
		}
		if e := text("default_id", 0, 64); e != nil {
			return e
		}
		if e := boolean("match_country"); e != nil {
			return e
		}
		if v["match_country"] != true {
			return bad("代理必须按订阅地区自动匹配")
		}
		if e := number("request_timeout_seconds", 10, 120); e != nil {
			return e
		}
	case "risk":
		for k, b := range map[string][2]int{"login_per_minute": {1, 100}, "api_per_minute": {10, 10000}, "order_per_minute": {1, 100}, "max_open_orders": {1, 500}, "max_topup": {100, 100000000}, "session_hours": {1, 168}, "event_retention_days": {7, 365}} {
			if e := number(k, b[0], b[1]); e != nil {
				return e
			}
		}
	case "scheduler":
		for k, b := range map[string][2]int{"max_parallel": {1, 100}, "per_user_parallel": {1, 20}, "retry_limit": {0, 10}, "retry_delay_seconds": {5, 3600}, "task_timeout_seconds": {30, 1800}, "queue_limit": {1, 10000}} {
			if e := number(k, b[0], b[1]); e != nil {
				return e
			}
		}
		if e := boolean("paused"); e != nil {
			return e
		}
		if num(v, "per_user_parallel", 1) > num(v, "max_parallel", 2) {
			return bad("用户并发不能大于总并发")
		}
		if str(v, "engine") != "not_connected" && str(v, "engine") != "checkout" {
			return bad("当前只支持结账准备，银行卡付款尚未接入")
		}
		if str(v, "link_mode") != "long" {
			return bad("旧队列字段 link_mode 保持 long；实际长短链请在订阅流程配置中选择")
		}
		if !strings.Contains("|PH|US|JP|SG|GB|EG|TR|", "|"+str(v, "default_country")+"|") {
			return bad("默认地区无效")
		}
	case "site":
		for k, b := range map[string][2]int{"name": {1, 50}, "english_name": {1, 50}, "tagline": {1, 200}, "support_url": {0, 500}, "terms": {10, 5000}} {
			if e := text(k, b[0], b[1]); e != nil {
				return e
			}
		}
		if e := boolean("registration_open"); e != nil {
			return e
		}
		if e := number("invite_rate", 0, 0); e != nil {
			return bad("返利结算待支付接入，当前比例只能为 0")
		}
		if str(v, "billing_country") != "US" || str(v, "billing_mode") != "customer_review" {
			return bad("当前账单模式为 US，客户可核对")
		}
		if s := str(v, "support_url"); s != "" {
			u, e := url.Parse(s)
			if e != nil || (u.Scheme != "https" && u.Scheme != "http") || u.Hostname() == "" {
				return bad("客服链接须为 http(s) 地址")
			}
		}
	case "billing":
		if str(v, "country") != "US" {
			return bad("账单国家固定 US")
		}
		if e := boolean("allow_customer_edit"); e != nil {
			return e
		}
		arr, ok := v["address_templates"].([]any)
		if !ok || len(arr) > 100 {
			return bad("地址模版应为最多 100 项的列表")
		}
		emails := map[string]bool{}
		ids := map[string]bool{}
		for _, item := range arr {
			m, ok := item.(map[string]any)
			if !ok {
				return bad("地址模版格式无效")
			}
			for k, val := range m {
				if k == "enabled" || k == "test_only" {
					if _, ok := val.(bool); !ok {
						return bad("账单资料启用状态与测试标记必须为开关")
					}
					continue
				}
				if !strings.Contains("|id|name|email|first_name|last_name|state|city|line1|postal_code|", "|"+k+"|") {
					return bad("地址模版字段无效")
				}
				s, ok := val.(string)
				if !ok || len(s) > 200 {
					return bad("地址模版内容无效")
				}
			}
			address := M{"country": "US"}
			for k, val := range m {
				address[k] = val
			}
			if !validBilling(address) {
				return bad("请填写完整、真实的 US 持卡人账单资料")
			}
			email := strings.ToLower(strings.TrimSpace(str(m, "email")))
			if email != "" && (!validEmail(email) || emails[email]) {
				return bad("如填写订阅邮箱，须为有效且不重复的邮箱")
			}
			id := str(m, "id")
			if id != "" && (!regexp.MustCompile(`^bp_[A-Za-z0-9_-]{1,80}$`).MatchString(id) || ids[id]) {
				return bad("账单资料标识无效或重复")
			}
			if id == "" && email == "" {
				return bad("账单资料缺少标识，请通过列表新增")
			}
			emails[email] = true
			ids[id] = true
		}
	case "tiers":
		arr, ok := v["items"].([]any)
		if !ok || len(arr) < 1 || len(arr) > 10 {
			return bad("等级数量需为 1～10")
		}
		codes, names := map[string]bool{}, map[string]bool{}
		lastThreshold, lastDiscount := float64(-1), float64(100)
		for i, item := range arr {
			m, ok := item.(map[string]any)
			if !ok || strings.TrimSpace(str(m, "name")) == "" || len(str(m, "name")) > 80 || len(str(m, "description")) > 500 {
				return bad("等级名称无效")
			}
			for k := range m {
				if k != "code" && k != "name" && k != "threshold" && k != "discount" && k != "description" {
					return bad("未知等级字段")
				}
			}
			code := str(m, "code")
			name := strings.TrimSpace(str(m, "name"))
			if !regexp.MustCompile(`^[A-Z][A-Z0-9_]{0,15}$`).MatchString(code) || codes[code] || names[name] {
				return bad("等级标识或名称无效、重复")
			}
			codes[code] = true
			names[name] = true
			threshold, ok := m["threshold"].(float64)
			discount, dok := m["discount"].(float64)
			if !ok || !dok || threshold < 0 || threshold > 1e10 || threshold != float64(int64(threshold)) || discount < 1 || discount > 100 || discount != float64(int64(discount)) {
				return bad("等级门槛须为非负整数，折扣须为 1～100 的整数百分比")
			}
			if (i == 0 && threshold != 0) || threshold <= lastThreshold || discount > lastDiscount {
				return bad("第一档门槛须为 0，之后严格递增；等级越高折扣不能变差")
			}
			lastThreshold, lastDiscount = threshold, discount
		}
	}
	return nil
}
func (a *App) saveSettings(w http.ResponseWriter, r *http.Request) {
	v := M{}
	if !read(w, r, &v) {
		return
	}
	key := r.PathValue("key")
	if e := validateConfig(key, v); e != nil {
		fail(w, 400, e.Error())
		return
	}
	b, _ := json.Marshal(v)
	a.transaction(w, r, func(tx pgx.Tx) (any, error) {
		auditMessage := "保存配置组"
		if key == "payment" {
			var previous M
			if e := tx.QueryRow(r.Context(), "SELECT value FROM settings WHERE key='payment' FOR UPDATE").Scan(&previous); e != nil {
				return nil, e
			}
			if _, present := v["network_restart_limit"]; !present {
				v["network_restart_limit"] = num(previous, "network_restart_limit", 2)
			}
			// An older admin page must not reset a selection it did not submit.
			if _, present := v["subscription_flow"]; !present {
				v["subscription_flow"] = configuredSubscriptionFlow(previous)
			}
			delete(v, "checkout_flow")
			if _, present := v["checkout_type"]; !present {
				v["checkout_type"] = configuredCheckoutType(previous)
			}
			if _, present := v["checkout_types"]; !present {
				v["checkout_types"] = configuredCheckoutTypes(previous)
			}
			for _, product := range checkoutProducts {
				before, after := configuredProductCheckoutType(previous, product.ID), configuredProductCheckoutType(v, product.ID)
				if before != after {
					labels := map[string]string{"hosted": "长链", "oaics": "短链"}
					auditMessage += "；" + product.Name + " 结账链接由" + labels[before] + "切换为" + labels[after] + "，新旧流程的新订单生效"
				}
			}
			if before, after := configuredCheckoutType(previous), configuredCheckoutType(v); before != after {
				labels := map[string]string{"hosted": "长链", "oaics": "短链"}
				auditMessage += "；历史兼容字段由" + labels[before] + "改为" + labels[after] + "，实际执行以套餐配置为准"
			}
			if before, after := configuredSubscriptionFlow(previous), configuredSubscriptionFlow(v); before != after {
				labels := map[string]string{subscriptionFlowLegacy: "旧流程（浏览器）", subscriptionFlowLegacyProtocol: "旧流程（协议）", subscriptionFlowCSLive: "新流程"}
				auditMessage += "；完整订阅流程由" + labels[before] + "切换为" + labels[after] + "，订阅与独立提链的新申请生效"
			}
			b, _ = json.Marshal(v)
		}
		if key == "proxy" {
			if _, e := tx.Exec(r.Context(), "SELECT pg_advisory_xact_lock(1886003)"); e != nil {
				return nil, e
			}
			if id := str(v, "default_id"); id != "" {
				var enabled bool
				if e := tx.QueryRow(r.Context(), "SELECT enabled FROM resources WHERE id=$1", id).Scan(&enabled); e != nil || !enabled {
					return nil, bad("默认代理不存在或未启用")
				}
			}
			if _, present := v["global_id"]; !present {
				var previous M
				if e := tx.QueryRow(r.Context(), "SELECT value FROM settings WHERE key='proxy'").Scan(&previous); e != nil {
					return nil, e
				}
				v["global_id"] = str(previous, "global_id")
			} else if id := str(v, "global_id"); id != "" {
				var enabled bool
				if e := tx.QueryRow(r.Context(), "SELECT enabled FROM resources WHERE id=$1 AND kind='proxy'", id).Scan(&enabled); e != nil || !enabled {
					return nil, bad("全局代理不存在或未启用")
				}
			}
			b, _ = json.Marshal(v)
		}
		if key == "tiers" {
			if _, e := tx.Exec(r.Context(), "SELECT pg_advisory_xact_lock(782518860)"); e != nil {
				return nil, e
			}
			codes := []string{}
			for _, x := range v["items"].([]any) {
				codes = append(codes, str(x.(map[string]any), "code"))
			}
			var n int
			if e := tx.QueryRow(r.Context(), "SELECT count(*) FROM users WHERE membership_code<>'' AND (membership_until IS NULL OR membership_until>now()) AND NOT(membership_code=ANY($1::text[]))", codes).Scan(&n); e != nil {
				return nil, e
			}
			if n > 0 {
				return nil, bad("有账户正在使用被移除的指定等级，请先恢复自动等级或重新指定")
			}
		}
		_, e := tx.Exec(r.Context(), "UPDATE settings SET value=$2 WHERE key=$1", key, b)
		if e == nil {
			e = a.audit(r.Context(), tx, r, "settings.updated", key, auditMessage)
		}
		return M{"ok": true}, e
	})
}
func normalizeProxy(raw string) (string, error) {
	raw = strings.TrimSpace(raw)
	if raw == "" {
		return "", bad("代理地址不能为空")
	}
	if !strings.Contains(raw, "://") {
		parts := strings.SplitN(raw, ":", 4)
		if len(parts) == 4 {
			u := url.URL{Scheme: "http", Host: net.JoinHostPort(parts[0], parts[1]), User: url.UserPassword(parts[2], parts[3])}
			raw = u.String()
		} else {
			raw = "http://" + raw
		}
	}
	u, e := url.Parse(raw)
	if e != nil || u.Hostname() == "" || (u.Path != "" && u.Path != "/") || u.RawQuery != "" || u.Fragment != "" || (u.Scheme != "http" && u.Scheme != "https" && u.Scheme != "socks5" && u.Scheme != "socks5h") {
		return "", bad("代理格式应为 http(s)/socks5(h)://user:password@host:port 或 host:port:user:password")
	}
	u.Path = ""
	if strings.EqualFold(u.Hostname(), "localhost") || strings.HasSuffix(strings.ToLower(u.Hostname()), ".localhost") {
		return "", bad("代理需为公网地址")
	}
	port, e := strconv.Atoi(u.Port())
	if e != nil || port < 1 || port > 65535 {
		return "", bad("代理端口无效")
	}
	if p := net.ParseIP(u.Hostname()); p != nil && (p.IsPrivate() || p.IsLoopback() || p.IsUnspecified() || p.IsLinkLocalUnicast() || p.IsMulticast()) {
		return "", bad("代理需为公网地址")
	}
	return u.String(), nil
}
func (a *App) resources(w http.ResponseWriter, r *http.Request) {
	a.list(w, r, `SELECT id,name,kind,country,enabled,weight,capacity,last_status,last_checked,last_ip,last_country,last_latency_ms,quality_result,openai_result,created_at,true AS has_credentials,
 COALESCE(id=(SELECT value->>'global_id' FROM settings WHERE key='proxy'),false) AS is_global,
 (SELECT id FROM proxy_checks c WHERE c.resource_id=resources.id AND c.state IN ('queued','running') ORDER BY c.created_at LIMIT 1) AS active_check,
 (SELECT count(*) FROM proxy_leases l WHERE l.resource_id=resources.id AND l.expires_at>now()) AS in_use FROM resources ORDER BY created_at`)
}
func (a *App) saveResource(w http.ResponseWriter, r *http.Request) {
	var v struct {
		Name     string `json:"name"`
		Endpoint string `json:"endpoint"`
		Country  string `json:"country"`
		Enabled  bool   `json:"enabled"`
		Weight   int    `json:"weight"`
		Capacity int    `json:"capacity"`
		IsGlobal *bool  `json:"is_global"`
	}
	if !read(w, r, &v) {
		return
	}
	id := r.PathValue("id")
	fresh := id == ""
	if fresh {
		id = uid()
	}
	v.Name = strings.TrimSpace(v.Name)
	v.Country = strings.ToUpper(strings.TrimSpace(v.Country))
	if !strings.Contains("|PH|US|JP|SG|GB|EG|TR|", "|"+v.Country+"|") {
		fail(w, 400, "请选择支持的订阅地区")
		return
	}
	if len(v.Name) < 1 || len(v.Name) > 80 || !regexp.MustCompile(`^[A-Z]{2}$`).MatchString(v.Country) || v.Weight < 1 || v.Weight > 100 || v.Capacity < 1 || v.Capacity > 100 {
		fail(w, 400, "资源参数无效")
		return
	}
	enc := ""
	if fresh || v.Endpoint != "" {
		normalized, e := normalizeProxy(v.Endpoint)
		if e != nil {
			fail(w, 400, e.Error())
			return
		}
		enc, e = a.encrypt(normalized)
		if e != nil {
			fail(w, 500, "加密失败")
			return
		}
	}
	a.transaction(w, r, func(tx pgx.Tx) (any, error) {
		var e error
		if _, e = tx.Exec(r.Context(), "SELECT pg_advisory_xact_lock(1886003)"); e != nil {
			return nil, e
		}
		if !fresh {
			var previousCountry string
			if e = tx.QueryRow(r.Context(), "SELECT country FROM resources WHERE id=$1", id).Scan(&previousCountry); e != nil {
				if e == pgx.ErrNoRows {
					return nil, businessError{404, "代理不存在"}
				}
				return nil, e
			}
			var active int
			if e = tx.QueryRow(r.Context(), "SELECT count(*) FROM proxy_leases WHERE resource_id=$1 AND expires_at>now()", id).Scan(&active); e != nil {
				return nil, e
			}
			if active > 0 && (enc != "" || previousCountry != v.Country) {
				return nil, businessError{409, "代理正在执行任务，暂时不能更换连接凭据或订阅地区"}
			}
		}
		if fresh {
			_, e = tx.Exec(r.Context(), "INSERT INTO resources(id,name,country,endpoint_enc,enabled,weight,capacity) VALUES($1,$2,$3,$4,$5,$6,$7)", id, v.Name, strings.ToUpper(v.Country), enc, v.Enabled, v.Weight, v.Capacity)
		} else {
			tag, err := tx.Exec(r.Context(), "UPDATE resources SET name=$2,country=$3,endpoint_enc=CASE WHEN $4='' THEN endpoint_enc ELSE $4 END,enabled=$5,weight=$6,capacity=$7,last_status=CASE WHEN $4='' THEN last_status ELSE 'untested' END WHERE id=$1", id, v.Name, strings.ToUpper(v.Country), enc, v.Enabled, v.Weight, v.Capacity)
			e = checkRows(tag.RowsAffected(), err)
		}
		if e == nil {
			if v.IsGlobal != nil {
				if *v.IsGlobal && !v.Enabled {
					return nil, bad("请先启用代理，再设为全局代理")
				}
				if *v.IsGlobal {
					_, e = tx.Exec(r.Context(), "UPDATE settings SET value=jsonb_set(value,'{global_id}',to_jsonb($1::text)) WHERE key='proxy'", id)
				} else {
					_, e = tx.Exec(r.Context(), `UPDATE settings SET value=jsonb_set(value,'{global_id}','""') WHERE key='proxy' AND value->>'global_id'=$1`, id)
				}
				if e != nil {
					return nil, e
				}
			}
			if enc != "" {
				if _, e = tx.Exec(r.Context(), "UPDATE resources SET quality_result='{}',openai_result='{}',last_ip='',last_country='',last_latency_ms=0,last_checked=NULL WHERE id=$1", id); e != nil {
					return nil, e
				}
			}
			e = a.audit(r.Context(), tx, r, "resource.saved", id, "保存资源，凭据不回显")
		}
		return M{"id": id}, e
	})
}
func (a *App) deleteResource(w http.ResponseWriter, r *http.Request) {
	a.transaction(w, r, func(tx pgx.Tx) (any, error) {
		if _, e := tx.Exec(r.Context(), "SELECT pg_advisory_xact_lock(1886003)"); e != nil {
			return nil, e
		}
		var active int
		if e := tx.QueryRow(r.Context(), "SELECT count(*) FROM proxy_leases WHERE resource_id=$1 AND expires_at>now()", r.PathValue("id")).Scan(&active); e != nil {
			return nil, e
		}
		if active > 0 {
			return nil, businessError{409, "代理正在执行任务，请等待结束后删除"}
		}
		if _, e := tx.Exec(r.Context(), "DELETE FROM proxy_leases WHERE resource_id=$1", r.PathValue("id")); e != nil {
			return nil, e
		}
		tag, e := tx.Exec(r.Context(), "DELETE FROM resources WHERE id=$1", r.PathValue("id"))
		if e = checkRows(tag.RowsAffected(), e); e != nil {
			return nil, e
		}
		if _, e = tx.Exec(r.Context(), "UPDATE settings SET value=jsonb_set(value,'{default_id}','\"\"') WHERE key='proxy' AND value->>'default_id'=$1", r.PathValue("id")); e != nil {
			return nil, e
		}
		if _, e = tx.Exec(r.Context(), `UPDATE settings SET value=jsonb_set(value,'{global_id}','""') WHERE key='proxy' AND value->>'global_id'=$1`, r.PathValue("id")); e != nil {
			return nil, e
		}
		return M{"ok": true}, a.audit(r.Context(), tx, r, "resource.deleted", r.PathValue("id"), "删除资源")
	})
}
func publicDial(ctx context.Context, network, address string) (net.Conn, error) {
	target, e := publicDialAddress(ctx, address)
	if e != nil {
		return nil, e
	}
	d := net.Dialer{Timeout: 8 * time.Second}
	return d.DialContext(ctx, network, target)
}
func publicDialAddress(ctx context.Context, address string) (string, error) {
	host, port, e := net.SplitHostPort(address)
	if e != nil {
		return "", e
	}
	ips, e := net.DefaultResolver.LookupIPAddr(ctx, host)
	if e != nil || len(ips) == 0 {
		return "", fmt.Errorf("代理地址无法解析")
	}
	for _, p := range ips {
		if p.IP.IsPrivate() || p.IP.IsLoopback() || p.IP.IsUnspecified() || p.IP.IsLinkLocalUnicast() || p.IP.IsMulticast() {
			return "", fmt.Errorf("禁止访问内网资源")
		}
	}
	return net.JoinHostPort(ips[0].IP.String(), port), nil
}
func (a *App) testResource(w http.ResponseWriter, r *http.Request) {
	if !a.limit(r.Context(), "proxytest:"+current(r).ID, 10) {
		fail(w, 429, "测试过于频繁")
		return
	}
	var enc string
	if a.DB.QueryRow(r.Context(), "SELECT endpoint_enc FROM resources WHERE id=$1", r.PathValue("id")).Scan(&enc) != nil {
		fail(w, 404, "资源不存在")
		return
	}
	raw, e := a.decrypt(enc)
	if e != nil {
		fail(w, 500, "资源读取失败")
		return
	}
	client, closeClient, e := newPaymentClient(raw, 15*time.Second)
	if e != nil {
		fail(w, 400, "资源无效")
		return
	}
	defer closeClient()
	req, _ := http.NewRequestWithContext(r.Context(), "GET", "https://ipinfo.io/json", nil)
	start := time.Now()
	res, e := client.Do(req)
	status := "failed"
	result := M{"ok": false, "message": "连接失败，请检查代理或凭据"}
	if e == nil {
		defer res.Body.Close()
		b, _ := io.ReadAll(io.LimitReader(res.Body, 8192))
		v := M{}
		if res.StatusCode == 200 && json.Unmarshal(b, &v) == nil && net.ParseIP(str(v, "ip")) != nil {
			status = "healthy"
			result = M{"ok": true, "ip": str(v, "ip"), "country": str(v, "country"), "latency_ms": time.Since(start).Milliseconds()}
		}
	}
	a.transaction(w, r, func(tx pgx.Tx) (any, error) {
		_, e := tx.Exec(r.Context(), "UPDATE resources SET last_status=$2,last_checked=now(),last_ip=$3,last_country=$4,last_latency_ms=$5 WHERE id=$1 AND endpoint_enc=$6", r.PathValue("id"), status, str(result, "ip"), str(result, "country"), time.Since(start).Milliseconds(), enc)
		if e == nil {
			e = a.audit(r.Context(), tx, r, "resource.tested", r.PathValue("id"), status)
		}
		return result, e
	})
}
