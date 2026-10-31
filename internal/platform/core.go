package platform

import (
	"context"
	"crypto/aes"
	"crypto/cipher"
	"crypto/rand"
	"crypto/sha256"
	_ "embed"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
	"golang.org/x/crypto/bcrypt"
	"io"
	"net"
	"net/http"
	"net/mail"
	"net/url"
	"os"
	urlpath "path"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"time"
	"xupay/internal/cslive"
)

//go:embed schema.sql
var schema string

type M = map[string]any
type App struct {
	DB                   *pgxpool.Pool
	Key                  []byte
	Secure               bool
	Static               string
	paymentMu            sync.Mutex
	payments             map[string]*paymentSession
	paymentActions       map[string]*paymentAction
	actionStart          actionStarter
	protocolClient       func(*protocolRun, int) (protocolPaymentClient, func(), error)
	checkoutClient       func(cslive.Config, cslive.Hooks) protocolCheckoutClient
	legacyCheckoutClient func(context.Context, string) (*http.Client, func(), error)
	providerDiagnostics  chan providerDiagnostic
	diagnosticDrops      atomic.Uint64
	diagnosticFailures   atomic.Uint64
	// Tests replace the process boundary, never a public HTTP payment-success switch.
	paymentStart       paymentStarter
	renewalClient      func() *http.Client
	proxyProbeClient   func(string, time.Duration) (*http.Client, func(), error)
	renewalProxyClient func(string, time.Duration) (*http.Client, func(), error)
	orderIPClient      func(string, time.Duration) (*http.Client, func(), error)
}
type User struct {
	ID      string `json:"id"`
	Email   string `json:"email"`
	Name    string `json:"name"`
	Role    string `json:"role"`
	Status  string `json:"status"`
	Balance int64  `json:"balance"`
	Frozen  int64  `json:"frozen"`
	Invite  string `json:"invite_code"`
	Fee     *int64 `json:"fee_override"`
}
type ctxKey int

const userKey ctxKey = 1
const apiKey ctxKey = 2

func uid() string {
	b := make([]byte, 16)
	if _, e := rand.Read(b); e != nil {
		panic(e)
	}
	return hex.EncodeToString(b)
}
func secret() string {
	b := make([]byte, 32)
	if _, e := rand.Read(b); e != nil {
		panic(e)
	}
	return base64.RawURLEncoding.EncodeToString(b)
}
func hash(s string) string { h := sha256.Sum256([]byte(s)); return hex.EncodeToString(h[:]) }
func (a *App) encrypt(s string) (string, error) {
	block, e := aes.NewCipher(a.Key)
	if e != nil {
		return "", e
	}
	g, e := cipher.NewGCM(block)
	if e != nil {
		return "", e
	}
	n := make([]byte, g.NonceSize())
	if _, e = rand.Read(n); e != nil {
		return "", e
	}
	return base64.StdEncoding.EncodeToString(g.Seal(n, n, []byte(s), nil)), nil
}
func (a *App) decrypt(s string) (string, error) {
	b, e := base64.StdEncoding.DecodeString(s)
	if e != nil {
		return "", e
	}
	block, e := aes.NewCipher(a.Key)
	if e != nil {
		return "", e
	}
	g, e := cipher.NewGCM(block)
	if e != nil {
		return "", e
	}
	if len(b) < g.NonceSize() {
		return "", errors.New("invalid ciphertext")
	}
	v, e := g.Open(nil, b[:g.NonceSize()], b[g.NonceSize():], nil)
	return string(v), e
}
func New(ctx context.Context, dsn, key, static string, secure bool) (*App, error) {
	k, e := base64.StdEncoding.DecodeString(key)
	if e != nil || len(k) != 32 {
		return nil, errors.New("MASTER_KEY must be base64 32 bytes")
	}
	db, e := pgxpool.New(ctx, dsn)
	if e != nil {
		return nil, e
	}
	if e = db.Ping(ctx); e != nil {
		db.Close()
		return nil, e
	}
	a := &App{DB: db, Key: k, Static: static, Secure: secure, providerDiagnostics: make(chan providerDiagnostic, 256)}
	if _, e = db.Exec(ctx, schema); e != nil {
		db.Close()
		return nil, e
	}
	if e = a.migrateProxyVendors(ctx); e != nil {
		db.Close()
		return nil, e
	}
	if e = a.seed(ctx); e != nil {
		db.Close()
		return nil, e
	}
	return a, nil
}
func (a *App) seed(ctx context.Context) error {
	defaults := map[string]M{
		"site":      {"name": "序付", "english_name": "XUPAY", "tagline": "让每一笔订阅，清晰有序。", "registration_open": true, "support_url": "", "terms": "请仅提交本人或已获授权的账号与银行卡。服务费与上游订阅费用分别结算，订单处理结果以订单记录为准。", "billing_country": "US", "billing_mode": "customer_review", "invite_rate": 0},
		"risk":      {"login_per_minute": 10, "api_per_minute": 120, "order_per_minute": 5, "max_open_orders": 10, "max_topup": 10000000, "session_hours": 24, "event_retention_days": 90},
		"scheduler": {"paused": false, "max_parallel": 2, "per_user_parallel": 1, "retry_limit": 2, "retry_delay_seconds": 30, "task_timeout_seconds": 180, "queue_limit": 500, "default_country": "PH", "link_mode": "long", "engine": "not_connected"},
		"billing":   {"country": "US", "allow_customer_edit": true, "address_templates": []M{}},
		"proxy":     {"default_id": "", "global_id": "", "match_country": true, "request_timeout_seconds": 60},
		"payment":   {"enabled": true, "subscription_flow": subscriptionFlowCSLive, "checkout_type": "hosted", "checkout_types": defaultCheckoutTypes(), "max_parallel": 2, "per_user_parallel": 1, "prepare_timeout_seconds": 120, "quote_timeout_seconds": 180, "result_timeout_seconds": 120, "requests_per_minute": 3, "subscription_wait_seconds": 600, "renewal_check_interval_seconds": 30, "renewal_check_limit": 10, "network_restart_limit": 2, "activation_check_interval_seconds": 5, "activation_check_limit": 12},
		"tiers":     {"items": defaultTiers().Items},
	}
	for k, v := range defaults {
		b, _ := json.Marshal(v)
		if _, e := a.DB.Exec(ctx, "INSERT INTO settings(key,value) VALUES($1,$2) ON CONFLICT DO NOTHING", k, b); e != nil {
			return e
		}
	}
	if _, e := a.DB.Exec(ctx, `UPDATE settings SET value='{"checkout_type":"hosted","subscription_flow":"cslive","subscription_wait_seconds":600,"renewal_check_interval_seconds":30,"renewal_check_limit":10,"network_restart_limit":2,"activation_check_interval_seconds":5,"activation_check_limit":12}'::jsonb || value WHERE key='payment'`); e != nil {
		return e
	}
	// Consolidate only future admission settings. Existing orders keep their
	// original executor/checkpoint; never migrate or replay payment records.
	if _, e := a.DB.Exec(ctx, `UPDATE settings SET value=value-'checkout_flow' WHERE key='payment' AND value ? 'checkout_flow'`); e != nil {
		return e
	}
	checkoutTypes, _ := json.Marshal(defaultCheckoutTypes())
	if _, e := a.DB.Exec(ctx, "UPDATE settings SET value=jsonb_set(value,'{checkout_types}',$1::jsonb) WHERE key='payment' AND NOT(value ? 'checkout_types')", checkoutTypes); e != nil {
		return e
	}
	// Retain the obsolete queue field for old admin clients.
	if _, e := a.DB.Exec(ctx, "UPDATE settings SET value=jsonb_set(value,'{link_mode}','\"long\"') WHERE key='scheduler' AND value->>'link_mode' IS DISTINCT FROM 'long'"); e != nil {
		return e
	}
	// Region routing is mandatory; retain the legacy setting for API compatibility.
	if _, e := a.DB.Exec(ctx, "UPDATE settings SET value=jsonb_set(value,'{match_country}','true') WHERE key='proxy' AND value->>'match_country' IS DISTINCT FROM 'true'"); e != nil {
		return e
	}
	// Link generation is now connected. This does not enable card payments.
	if _, e := a.DB.Exec(ctx, "UPDATE settings SET value=jsonb_set(value,'{engine}','\"checkout\"') WHERE key='scheduler' AND value->>'engine'='not_connected'"); e != nil {
		return e
	}
	// Replace only the original placeholder defaults; preserve operator edits.
	legacy := `{"items":[{"name":"标准","threshold":0,"discount":100},{"name":"进阶","threshold":100000,"discount":95},{"name":"伙伴","threshold":500000,"discount":90}]}`
	tierDefaults, _ := json.Marshal(defaultTiers())
	if _, e := a.DB.Exec(ctx, "UPDATE settings SET value=$1 WHERE key='tiers' AND value=$2::jsonb", tierDefaults, legacy); e != nil {
		return e
	}
	for i, p := range []M{{"id": "pro5x", "name": "Pro 5x", "description": "灵活开启，更适合轻量使用", "fee": 10000, "tag": "轻量进阶"}, {"id": "pro", "name": "Pro 20x", "description": "为高频工作提供充足空间", "fee": 10000, "tag": "热门选择"}, {"id": "team", "name": "Team 标准", "description": "将团队协作放到同一个空间", "fee": 10000, "tag": "团队协作"}, {"id": "team_premium", "name": "Team 高级", "description": "为团队成员配置更高使用额度", "fee": 10000, "tag": "弹性席位"}} {
		if _, e := a.DB.Exec(ctx, "INSERT INTO products(id,name,description,fee,sort,tag,features) VALUES($1,$2,$3,$4,$5,$6,'[\"独立订单记录\",\"费用明细可追溯\",\"在线查看办理进度\"]') ON CONFLICT DO NOTHING", p["id"], p["name"], p["description"], p["fee"], i, p["tag"]); e != nil {
			return e
		}
	}
	var n int
	if e := a.DB.QueryRow(ctx, "SELECT count(*) FROM users").Scan(&n); e != nil {
		return e
	}
	if n == 0 {
		email := strings.ToLower(os.Getenv("BOOTSTRAP_EMAIL"))
		pw := os.Getenv("BOOTSTRAP_PASSWORD")
		if !validEmail(email) || len(pw) < 12 {
			return errors.New("bootstrap email/password missing (min 12 characters)")
		}
		h, _ := bcrypt.GenerateFromPassword([]byte(pw), bcrypt.DefaultCost)
		_, e := a.DB.Exec(ctx, "INSERT INTO users(id,email,name,password_hash,role,invite_code) VALUES($1,$2,'运营管理员',$3,'admin',$4)", uid(), email, string(h), uid()[:10])
		return e
	}
	return nil
}
func (a *App) config(ctx context.Context, key string) M {
	var b []byte
	if a.DB.QueryRow(ctx, "SELECT value FROM settings WHERE key=$1", key).Scan(&b) != nil {
		return M{}
	}
	v := M{}
	json.Unmarshal(b, &v)
	return v
}
func num(m M, k string, fallback int) int {
	v, ok := m[k].(float64)
	if !ok {
		return fallback
	}
	return int(v)
}
func str(m M, k string) string { v, _ := m[k].(string); return v }
func validEmail(s string) bool {
	v, e := mail.ParseAddress(s)
	return e == nil && v.Address == s && len(s) <= 254
}
func read(w http.ResponseWriter, r *http.Request, v any) bool {
	r.Body = http.MaxBytesReader(w, r.Body, 1<<20)
	d := json.NewDecoder(r.Body)
	d.DisallowUnknownFields()
	if e := d.Decode(v); e != nil {
		fail(w, 400, "请求内容不正确")
		return false
	}
	var extra any
	if d.Decode(&extra) != io.EOF {
		fail(w, 400, "只允许一个 JSON 对象")
		return false
	}
	return true
}
func out(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.WriteHeader(status)
	json.NewEncoder(w).Encode(v)
}
func fail(w http.ResponseWriter, status int, msg string) { out(w, status, M{"error": msg}) }
func current(r *http.Request) User                       { u, _ := r.Context().Value(userKey).(User); return u }
func ip(r *http.Request) string {
	host, _, e := net.SplitHostPort(r.RemoteAddr)
	if e != nil {
		return r.RemoteAddr
	}
	return host
}
func (a *App) audit(ctx context.Context, tx pgx.Tx, r *http.Request, action, target, detail string) error {
	u := current(r)
	var id any
	if u.ID != "" {
		id = u.ID
	}
	_, e := tx.Exec(ctx, "INSERT INTO audit_events(actor_id,action,target,detail,ip) VALUES($1,$2,$3,$4,$5)", id, action, target, detail, ip(r))
	return e
}
func (a *App) event(r *http.Request, category, severity, detail string) {
	u := current(r)
	var id any
	if u.ID != "" {
		id = u.ID
	}
	a.DB.Exec(r.Context(), "INSERT INTO risk_events(user_id,category,severity,ip,detail) SELECT $1,$2,$3,$4,$5 WHERE NOT EXISTS(SELECT 1 FROM risk_events WHERE category=$2 AND ip=$4 AND created_at>now()-interval '1 minute')", id, category, severity, ip(r), detail)
}
func (a *App) limit(ctx context.Context, key string, n int) bool {
	var hits int
	e := a.DB.QueryRow(ctx, `INSERT INTO rate_buckets(key,hits,expires_at) VALUES($1,1,now()+interval '1 minute') ON CONFLICT(key) DO UPDATE SET hits=CASE WHEN rate_buckets.expires_at<now() THEN 1 ELSE rate_buckets.hits+1 END,expires_at=CASE WHEN rate_buckets.expires_at<now() THEN now()+interval '1 minute' ELSE rate_buckets.expires_at END RETURNING hits`, key).Scan(&hits)
	return e == nil && hits <= n
}
func (a *App) require(admin bool, h http.HandlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		var u User
		var e error
		bearer := strings.TrimPrefix(r.Header.Get("Authorization"), "Bearer ")
		usingAPI := r.Header.Get("Authorization") != ""
		fields := "u.id,u.email,u.name,u.role,u.status,u.balance,u.frozen,u.invite_code,u.fee_override"
		if usingAPI {
			if admin || !strings.HasPrefix(r.URL.Path, "/api/v1/") {
				fail(w, 403, "此接口不接受 API 密钥")
				return
			}
			e = a.DB.QueryRow(r.Context(), "SELECT "+fields+" FROM users u JOIN api_keys k ON k.user_id=u.id WHERE k.hash=$1 AND NOT k.revoked", hash(bearer)).Scan(&u.ID, &u.Email, &u.Name, &u.Role, &u.Status, &u.Balance, &u.Frozen, &u.Invite, &u.Fee)
			if e == nil {
				a.DB.Exec(r.Context(), "UPDATE api_keys SET last_used=now() WHERE hash=$1", hash(bearer))
			}
		} else {
			c, err := r.Cookie("xupay_session")
			if err != nil {
				fail(w, 401, "请先登录")
				return
			}
			e = a.DB.QueryRow(r.Context(), "SELECT "+fields+" FROM users u JOIN sessions s ON s.user_id=u.id WHERE s.hash=$1 AND s.expires_at>now()", hash(c.Value)).Scan(&u.ID, &u.Email, &u.Name, &u.Role, &u.Status, &u.Balance, &u.Frozen, &u.Invite, &u.Fee)
		}
		if e != nil || u.Status != "active" {
			fail(w, 401, "登录已失效或账户已停用")
			return
		}
		r = r.WithContext(context.WithValue(r.Context(), userKey, u))
		r = r.WithContext(context.WithValue(r.Context(), apiKey, usingAPI))
		if admin && u.Role != "admin" {
			a.event(r, "permission_denied", "high", "尝试访问运营接口")
			fail(w, 403, "仅管理员可访问")
			return
		}
		if !a.limit(r.Context(), "api:"+u.ID, num(a.config(r.Context(), "risk"), "api_per_minute", 120)) {
			a.event(r, "rate_limit", "medium", "账户请求频率超限")
			w.Header().Set("Retry-After", "60")
			fail(w, 429, "操作频繁，请稍后再试")
			return
		}
		h(w, r)
	}
}
func (a *App) query(ctx context.Context, q string, args ...any) ([]M, error) {
	rows, e := a.DB.Query(ctx, q, args...)
	if e != nil {
		return nil, e
	}
	defer rows.Close()
	result := []M{}
	for rows.Next() {
		vals, e := rows.Values()
		if e != nil {
			return nil, e
		}
		m := M{}
		for i, f := range rows.FieldDescriptions() {
			m[f.Name] = vals[i]
		}
		result = append(result, m)
	}
	return result, rows.Err()
}
func (a *App) list(w http.ResponseWriter, r *http.Request, q string, args ...any) {
	v, e := a.query(r.Context(), q, args...)
	if e != nil {
		fail(w, 500, "读取失败，请稍后重试")
		return
	}
	out(w, 200, M{"items": v})
}
func (a *App) transaction(w http.ResponseWriter, r *http.Request, fn func(pgx.Tx) (any, error)) bool {
	tx, e := a.DB.Begin(r.Context())
	if e != nil {
		fail(w, 503, "数据库暂不可用")
		return false
	}
	defer tx.Rollback(r.Context())
	v, e := fn(tx)
	if e != nil {
		var be businessError
		if errors.As(e, &be) {
			fail(w, be.status, be.msg)
		} else {
			fail(w, 500, "操作未完成，请稍后重试")
		}
		return false
	}
	if e = tx.Commit(r.Context()); e != nil {
		fail(w, 500, "提交失败，请刷新核实")
		return false
	}
	out(w, 200, v)
	return true
}

type businessError struct {
	status int
	msg    string
}

func (e businessError) Error() string { return e.msg }
func bad(s string) error              { return businessError{400, s} }
func (a *App) Handler() http.Handler {
	m := http.NewServeMux()
	m.HandleFunc("GET /health/live", func(w http.ResponseWriter, r *http.Request) { out(w, 200, M{"status": "ok"}) })
	m.HandleFunc("GET /health/ready", func(w http.ResponseWriter, r *http.Request) {
		if a.DB.Ping(r.Context()) != nil {
			fail(w, 503, "not ready")
			return
		}
		out(w, 200, M{"status": "ready"})
	})
	m.HandleFunc("GET /api/public", a.public)
	m.HandleFunc("POST /api/auth/login", a.login)
	m.HandleFunc("POST /api/auth/register", a.register)
	m.HandleFunc("POST /api/auth/logout", a.logout)
	m.HandleFunc("GET /api/me", a.require(false, a.me))
	m.HandleFunc("PATCH /api/me", a.require(false, a.profile))
	m.HandleFunc("POST /api/session/inspect", a.require(false, a.checkSession))
	m.HandleFunc("GET /api/renewals", a.require(false, a.renewalTasks))
	m.HandleFunc("POST /api/renewals", a.require(false, a.createRenewalTask))
	m.HandleFunc("GET /api/renewals/{id}", a.require(false, a.renewalTask))
	m.HandleFunc("POST /api/v1/session/inspect", a.require(false, a.checkSession))
	m.HandleFunc("GET /api/membership", a.require(false, a.membership))
	m.HandleFunc("GET /api/quote", a.require(false, a.quote))
	m.HandleFunc("GET /api/overview", a.require(false, a.overview))
	m.HandleFunc("GET /api/orders", a.require(false, a.orders))
	m.HandleFunc("POST /api/orders", a.require(false, a.createOrder))
	m.HandleFunc("GET /api/orders/{id}", a.require(false, a.orderDetail))
	m.HandleFunc("POST /api/orders/{id}/cancel", a.require(false, a.cancelOrder))
	a.paymentRoutes(m)
	m.HandleFunc("GET /api/wallet", a.require(false, a.wallet))
	m.HandleFunc("POST /api/wallet/redeem", a.require(false, a.redeem))
	m.HandleFunc("POST /api/topups", a.require(false, a.topup))
	m.HandleFunc("POST /api/topups/{id}/cancel", a.require(false, a.cancelTopup))
	m.HandleFunc("GET /api/keys", a.require(false, a.keys))
	m.HandleFunc("POST /api/keys", a.require(false, a.createKey))
	m.HandleFunc("DELETE /api/keys/{id}", a.require(false, a.revokeKey))
	m.HandleFunc("GET /api/invites", a.require(false, a.invites))
	m.HandleFunc("GET /api/v1/catalog", a.require(false, a.catalog))
	m.HandleFunc("GET /api/v1/wallet", a.require(false, a.apiWallet))
	m.HandleFunc("GET /api/v1/membership", a.require(false, a.membership))
	m.HandleFunc("GET /api/v1/quote", a.require(false, a.quote))
	m.HandleFunc("POST /api/v1/wallet/redeem", a.require(false, a.redeem))
	m.HandleFunc("GET /api/v1/orders", a.require(false, a.orders))
	m.HandleFunc("GET /api/v1/orders/{id}", a.require(false, a.orderDetail))
	m.HandleFunc("POST /api/v1/orders", a.require(false, a.createOrder))
	m.HandleFunc("POST /api/v1/orders/{id}/cancel", a.require(false, a.cancelOrder))
	m.HandleFunc("GET /api/openapi.json", a.openapi)
	a.adminRoutes(m)
	m.HandleFunc("/", func(w http.ResponseWriter, r *http.Request) {
		if strings.HasPrefix(r.URL.Path, "/api/") {
			fail(w, 404, "接口不存在")
			return
		}
		if r.Method != "GET" && r.Method != "HEAD" {
			fail(w, 405, "方法不允许")
			return
		}
		p := urlpath.Clean(r.URL.Path)
		if strings.Contains(p, "..") || strings.Contains(p, "\\") || strings.HasPrefix(p, "/.") {
			http.NotFound(w, r)
			return
		}
		if strings.HasPrefix(p, "/assets/") {
			if strings.HasPrefix(filepath.Base(p), "Admin-") {
				a.require(true, func(w http.ResponseWriter, r *http.Request) { http.FileServer(http.Dir(a.Static)).ServeHTTP(w, r) })(w, r)
				return
			}
			http.FileServer(http.Dir(a.Static)).ServeHTTP(w, r)
			return
		}
		http.ServeFile(w, r, filepath.Join(a.Static, "index.html"))
	})
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		defer func() {
			if recover() != nil {
				fail(w, 500, "服务暂不可用")
			}
		}()
		w.Header().Set("X-Content-Type-Options", "nosniff")
		w.Header().Set("X-Frame-Options", "DENY")
		w.Header().Set("Referrer-Policy", "no-referrer")
		w.Header().Set("Permissions-Policy", "camera=(), microphone=(), geolocation=()")
		w.Header().Set("Content-Security-Policy", "default-src 'self'; script-src 'self'; style-src 'self' 'unsafe-inline'; img-src 'self' data:; connect-src 'self'; frame-ancestors 'none'; base-uri 'self'; form-action 'self'")
		w.Header().Set("Cache-Control", "no-store")
		if r.Method != "GET" && r.Method != "HEAD" && r.Method != "OPTIONS" {
			if origin := r.Header.Get("Origin"); origin != "" {
				u, e := url.Parse(origin)
				if e != nil || u.Host != r.Host {
					fail(w, 403, "不允许跨站请求")
					return
				}
			}
			if r.Header.Get("Sec-Fetch-Site") == "cross-site" {
				fail(w, 403, "不允许跨站请求")
				return
			}
		}
		if strings.HasPrefix(r.URL.Path, "/api/") {
			if !a.limit(r.Context(), "edge:"+ip(r), 600) {
				a.event(r, "ip_rate", "high", "来源请求频率超限")
				w.Header().Set("Retry-After", "60")
				fail(w, 429, "访问频繁，请稍后重试")
				return
			}
			var blocked bool
			a.DB.QueryRow(r.Context(), "SELECT EXISTS(SELECT 1 FROM blocked_ips WHERE ip=$1 AND expires_at>now())", ip(r)).Scan(&blocked)
			if blocked {
				fail(w, 403, "该来源暂时受限")
				return
			}
		}
		m.ServeHTTP(w, r)
	})
}
func (a *App) public(w http.ResponseWriter, r *http.Request) {
	p, e := a.query(r.Context(), "SELECT * FROM products WHERE enabled ORDER BY sort")
	if e != nil {
		fail(w, 503, "读取失败")
		return
	}
	ann, e := a.query(r.Context(), "SELECT id,title,body,tone FROM announcements WHERE published AND (starts_at IS NULL OR starts_at<=now()) AND (ends_at IS NULL OR ends_at>now()) AND login_visible ORDER BY sort,created_at DESC")
	if e != nil {
		fail(w, 503, "读取失败")
		return
	}
	cfg := a.config(r.Context(), "scheduler")
	out(w, 200, M{"site": a.config(r.Context(), "site"), "products": p, "announcements": ann, "checkout_connected": str(cfg, "engine") == "checkout", "default_country": str(cfg, "default_country"), "payment_connected": a.subscriptionEnabled(r.Context()), "tiers": a.config(r.Context(), "tiers")})
}
func (a *App) login(w http.ResponseWriter, r *http.Request) {
	if !a.limit(r.Context(), "login:"+ip(r), num(a.config(r.Context(), "risk"), "login_per_minute", 10)) {
		a.event(r, "login_rate", "high", "登录尝试频繁")
		fail(w, 429, "登录尝试过多，请一分钟后再试")
		return
	}
	var v struct {
		Email    string `json:"email"`
		Password string `json:"password"`
	}
	if !read(w, r, &v) {
		return
	}
	var id, pw, status string
	e := a.DB.QueryRow(r.Context(), "SELECT id,password_hash,status FROM users WHERE email=$1", strings.ToLower(strings.TrimSpace(v.Email))).Scan(&id, &pw, &status)
	if e != nil {
		bcrypt.CompareHashAndPassword([]byte("$2a$10$7EqJtq98hPqEX7fNZaFWoOHIYN0/UVFaP3rCMezjBFCN9l0AsOoaW"), []byte(v.Password))
	}
	if e != nil || bcrypt.CompareHashAndPassword([]byte(pw), []byte(v.Password)) != nil || status != "active" {
		a.event(r, "login_failed", "medium", "账号或密码校验失败")
		fail(w, 401, "账号或密码不正确，或账户已停用")
		return
	}
	a.session(w, r, id)
}
func (a *App) session(w http.ResponseWriter, r *http.Request, id string) {
	token := secret()
	hours := num(a.config(r.Context(), "risk"), "session_hours", 24)
	_, e := a.DB.Exec(r.Context(), "INSERT INTO sessions(hash,user_id,expires_at) VALUES($1,$2,$3)", hash(token), id, time.Now().Add(time.Duration(hours)*time.Hour))
	if e != nil {
		fail(w, 500, "登录失败")
		return
	}
	http.SetCookie(w, &http.Cookie{Name: "xupay_session", Value: token, Path: "/", HttpOnly: true, Secure: a.Secure, SameSite: http.SameSiteStrictMode, MaxAge: hours * 3600})
	out(w, 200, M{"ok": true})
}
func (a *App) logout(w http.ResponseWriter, r *http.Request) {
	if c, e := r.Cookie("xupay_session"); e == nil {
		a.DB.Exec(r.Context(), "DELETE FROM sessions WHERE hash=$1", hash(c.Value))
	}
	http.SetCookie(w, &http.Cookie{Name: "xupay_session", Value: "", Path: "/", MaxAge: -1, HttpOnly: true, Secure: a.Secure, SameSite: http.SameSiteStrictMode})
	out(w, 200, M{"ok": true})
}
func (a *App) register(w http.ResponseWriter, r *http.Request) {
	if a.config(r.Context(), "site")["registration_open"] != true {
		fail(w, 403, "暂未开放注册")
		return
	}
	if !a.limit(r.Context(), "register:"+ip(r), 5) {
		fail(w, 429, "请稍后再试")
		return
	}
	var v struct {
		Email    string `json:"email"`
		Password string `json:"password"`
		Name     string `json:"name"`
		Invite   string `json:"invite"`
	}
	if !read(w, r, &v) {
		return
	}
	v.Email = strings.ToLower(strings.TrimSpace(v.Email))
	if !validEmail(v.Email) || len(v.Password) < 10 || len(v.Password) > 72 || len(strings.TrimSpace(v.Name)) < 1 || len(v.Name) > 80 {
		fail(w, 400, "请填写有效邮箱、称呼和 10～72 位密码")
		return
	}
	h, e := bcrypt.GenerateFromPassword([]byte(v.Password), bcrypt.DefaultCost)
	if e != nil {
		fail(w, 400, "密码不合要求")
		return
	}
	var inviter any
	if v.Invite != "" {
		var x string
		if a.DB.QueryRow(r.Context(), "SELECT id FROM users WHERE invite_code=$1", v.Invite).Scan(&x) != nil {
			fail(w, 400, "邀请标识无效")
			return
		}
		inviter = x
	}
	id := uid()
	_, e = a.DB.Exec(r.Context(), "INSERT INTO users(id,email,name,password_hash,invite_code,invited_by) VALUES($1,$2,$3,$4,$5,$6)", id, v.Email, v.Name, string(h), uid()[:10], inviter)
	if e != nil {
		fail(w, 409, "该邮箱已注册或暂不可用")
		return
	}
	a.session(w, r, id)
}
func (a *App) profile(w http.ResponseWriter, r *http.Request) {
	var v struct {
		Name     string `json:"name"`
		Old      string `json:"old_password"`
		Password string `json:"password"`
	}
	if !read(w, r, &v) {
		return
	}
	if len(v.Name) < 1 || len(v.Name) > 80 {
		fail(w, 400, "称呼长度须为 1～80")
		return
	}
	a.transaction(w, r, func(tx pgx.Tx) (any, error) {
		var pw string
		if e := tx.QueryRow(r.Context(), "SELECT password_hash FROM users WHERE id=$1 FOR UPDATE", current(r).ID).Scan(&pw); e != nil {
			return nil, e
		}
		if v.Password != "" {
			if len(v.Password) < 10 || len(v.Password) > 72 || bcrypt.CompareHashAndPassword([]byte(pw), []byte(v.Old)) != nil {
				return nil, bad("原密码不正确或新密码不足 10 位")
			}
			h, e := bcrypt.GenerateFromPassword([]byte(v.Password), bcrypt.DefaultCost)
			if e != nil {
				return nil, e
			}
			pw = string(h)
			if _, e = tx.Exec(r.Context(), "DELETE FROM sessions WHERE user_id=$1", current(r).ID); e != nil {
				return nil, e
			}
		}
		_, e := tx.Exec(r.Context(), "UPDATE users SET name=$2,password_hash=$3,updated_at=now() WHERE id=$1", current(r).ID, v.Name, pw)
		if e == nil {
			e = a.audit(r.Context(), tx, r, "profile.updated", current(r).ID, "更新个人资料")
		}
		return M{"ok": true, "relogin": v.Password != ""}, e
	})
}
func checkRows(tagRows int64, e error) error {
	if e != nil {
		return e
	}
	if tagRows == 0 {
		return businessError{404, "记录不存在或状态已变化"}
	}
	return nil
}
func requireID(s string) bool { return len(s) == 32 && !strings.ContainsAny(s, "/\\.") }
func csvSafe(s string) string {
	if len(s) > 0 && strings.ContainsAny(s[:1], "=+-@\t\r") {
		return "'" + s
	}
	return s
}

var _ = fmt.Sprint
