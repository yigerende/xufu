package platform

import (
	"encoding/csv"
	"encoding/json"
	"fmt"
	"github.com/jackc/pgx/v5"
	"golang.org/x/crypto/bcrypt"
	"net"
	"net/http"
	"sort"
	"strconv"
	"strings"
	"time"
)

func (a *App) adminRoutes(m *http.ServeMux) {
	routes := map[string]http.HandlerFunc{
		"POST /api/admin/orders/{id}/close": a.adminCloseOrder,
		"GET /api/admin/membership":         a.adminMembership, "PUT /api/admin/users/{id}/membership": a.setMembership,
		"GET /api/admin/overview": a.adminOverview, "GET /api/admin/users": a.users, "POST /api/admin/users": a.createUser, "PATCH /api/admin/users/{id}": a.updateUser, "POST /api/admin/users/pricing": a.pricing, "POST /api/admin/users/{id}/balance": a.adjustBalance,
		"GET /api/admin/orders": a.adminOrders, "GET /api/admin/orders/{id}": a.adminOrderDetail, "PATCH /api/admin/orders/{id}": a.adminOrderNote, "GET /api/admin/finance": a.finance, "GET /api/admin/finance/export": a.financeExport,
		"GET /api/admin/topups": a.adminTopups, "POST /api/admin/topups/{id}/review": a.reviewTopup,
		"GET /api/admin/codes": a.codes, "POST /api/admin/codes": a.createCodes, "POST /api/admin/codes/batch": a.batchCodes,
		"GET /api/admin/products": func(w http.ResponseWriter, r *http.Request) { a.list(w, r, "SELECT * FROM products ORDER BY sort") }, "PUT /api/admin/products/{id}": a.saveProduct,
		"GET /api/admin/announcements": func(w http.ResponseWriter, r *http.Request) {
			a.list(w, r, "SELECT * FROM announcements ORDER BY sort,created_at DESC")
		}, "POST /api/admin/announcements": a.saveAnnouncement, "PUT /api/admin/announcements/{id}": a.saveAnnouncement, "DELETE /api/admin/announcements/{id}": a.deleteAnnouncement,
		"GET /api/admin/resources": a.resources, "POST /api/admin/resources": a.saveResource, "PUT /api/admin/resources/{id}": a.saveResource, "DELETE /api/admin/resources/{id}": a.deleteResource, "POST /api/admin/resources/{id}/test": a.testResource,
		"GET /api/admin/settings": a.settings, "PUT /api/admin/settings/{key}": a.saveSettings,
		"POST /api/admin/resources/{id}/global": a.setGlobalProxy,
		"POST /api/admin/resources/{id}/checks": a.startProxyCheck,
		"GET /api/admin/proxy-checks/{id}":      a.proxyCheck,
		"GET /api/admin/billing-profiles":       a.billingProfiles, "POST /api/admin/billing-profiles": a.changeBillingProfile, "PUT /api/admin/billing-profiles/{email}": a.changeBillingProfile, "DELETE /api/admin/billing-profiles/{email}": a.changeBillingProfile,
		"POST /api/admin/billing-profiles/sample": a.sampleBillingProfile,
		"GET /api/admin/risk":                     a.risks, "POST /api/admin/risk/{id}/resolve": a.resolveRisk, "POST /api/admin/blocks": a.blockIP, "DELETE /api/admin/blocks/{ip}": a.unblockIP,
		"GET /api/admin/audit": func(w http.ResponseWriter, r *http.Request) {
			a.list(w, r, "SELECT a.*,u.email AS actor FROM audit_events a LEFT JOIN users u ON a.actor_id=u.id ORDER BY a.id DESC LIMIT 300")
		},
	}
	for path, h := range routes {
		m.HandleFunc(path, a.require(true, h))
	}
}
func (a *App) adminOverview(w http.ResponseWriter, r *http.Request) {
	rows, e := a.query(r.Context(), `SELECT (SELECT count(*) FROM users WHERE role='member') AS customers,(SELECT COALESCE(sum(balance),0) FROM users WHERE role='member') AS balance,(SELECT COALESCE(sum(frozen),0) FROM users WHERE role='member') AS frozen,(SELECT count(*) FROM orders) AS orders,(SELECT count(*) FROM topups WHERE status='pending') AS pending_topups,(SELECT count(*) FROM risk_events WHERE NOT resolved) AS risks,(SELECT count(*) FROM resources WHERE enabled) AS resources,(SELECT count(*) FROM orders WHERE status IN ('queued','processing','awaiting_payment') AND NOT(workflow='link' AND status='awaiting_payment')) AS active_orders`)
	if e != nil {
		fail(w, 500, "统计读取失败")
		return
	}
	recent, _ := a.query(r.Context(), "SELECT o.id,o.user_id,o.product_id,o.product_name,o.email,o.country,o.seats,o.status,o.fee,o.source,o.workflow,o.keep_renewal,o.renewal,o.activation,o.note,o.created_at,o.updated_at,o.price_snapshot,u.email AS owner FROM orders o JOIN users u ON u.id=o.user_id ORDER BY o.created_at DESC LIMIT 8")
	risk, _ := a.query(r.Context(), "SELECT * FROM risk_events ORDER BY id DESC LIMIT 5")
	out(w, 200, M{"stats": rows[0], "orders": recent, "risks": risk, "scheduler": a.config(r.Context(), "scheduler"), "payment_connected": a.subscriptionEnabled(r.Context())})
}
func (a *App) users(w http.ResponseWriter, r *http.Request) {
	a.list(w, r, `SELECT u.id,u.email,u.name,u.role,u.status,u.balance,u.frozen,u.fee_override,u.invite_code,u.created_at,(SELECT count(*) FROM orders o WHERE o.user_id=u.id) AS orders FROM users u WHERE ($1='' OR u.email ILIKE '%'||$1||'%' OR u.name ILIKE '%'||$1||'%') ORDER BY u.created_at DESC LIMIT 500`, r.URL.Query().Get("q"))
}
func (a *App) createUser(w http.ResponseWriter, r *http.Request) {
	var v struct {
		Email    string `json:"email"`
		Name     string `json:"name"`
		Password string `json:"password"`
		Role     string `json:"role"`
	}
	if !read(w, r, &v) {
		return
	}
	v.Email = strings.ToLower(strings.TrimSpace(v.Email))
	if !validEmail(v.Email) || len(v.Name) < 1 || len(v.Name) > 80 || len(v.Password) < 10 || len(v.Password) > 72 || (v.Role != "member" && v.Role != "admin") {
		fail(w, 400, "请检查邮箱、称呼、角色和密码（至少 10 位）")
		return
	}
	pw, e := bcrypt.GenerateFromPassword([]byte(v.Password), bcrypt.DefaultCost)
	if e != nil {
		fail(w, 400, "密码不合要求")
		return
	}
	a.transaction(w, r, func(tx pgx.Tx) (any, error) {
		id := uid()
		_, e := tx.Exec(r.Context(), "INSERT INTO users(id,email,name,password_hash,role,invite_code) VALUES($1,$2,$3,$4,$5,$6)", id, v.Email, v.Name, string(pw), v.Role, uid()[:10])
		if e != nil {
			return nil, bad("邮箱已存在或不可用")
		}
		e = a.audit(r.Context(), tx, r, "user.created", id, "新建账户 / "+v.Role)
		return M{"id": id}, e
	})
}
func (a *App) updateUser(w http.ResponseWriter, r *http.Request) {
	var v struct {
		Email    string `json:"email"`
		Name     string `json:"name"`
		Role     string `json:"role"`
		Status   string `json:"status"`
		Fee      *int64 `json:"fee_override"`
		Password string `json:"password"`
	}
	if !read(w, r, &v) {
		return
	}
	v.Email = strings.ToLower(strings.TrimSpace(v.Email))
	if !validEmail(v.Email) || len(v.Name) < 1 || len(v.Name) > 80 || (v.Role != "admin" && v.Role != "member") || (v.Status != "active" && v.Status != "disabled") || (v.Fee != nil && (*v.Fee < 0 || *v.Fee > 100000000)) {
		fail(w, 400, "账户参数无效")
		return
	}
	if v.Password != "" && (len(v.Password) < 10 || len(v.Password) > 72) {
		fail(w, 400, "密码须为 10～72 位")
		return
	}
	id := r.PathValue("id")
	if id == current(r).ID && (v.Role != "admin" || v.Status != "active") {
		fail(w, 400, "不能停用或降级当前管理员")
		return
	}
	a.transaction(w, r, func(tx pgx.Tx) (any, error) {
		_, e := tx.Exec(r.Context(), "SELECT pg_advisory_xact_lock(1886001)")
		if e != nil {
			return nil, e
		}
		var oldRole, oldStatus string
		if e = tx.QueryRow(r.Context(), "SELECT role,status FROM users WHERE id=$1 FOR UPDATE", id).Scan(&oldRole, &oldStatus); e != nil {
			return nil, bad("用户不存在")
		}
		if oldRole == "admin" && oldStatus == "active" && (v.Role != "admin" || v.Status != "active") {
			var n int
			tx.QueryRow(r.Context(), "SELECT count(*) FROM users WHERE role='admin' AND status='active'").Scan(&n)
			if n <= 1 {
				return nil, bad("至少保留一个可用管理员")
			}
		}
		_, e = tx.Exec(r.Context(), "UPDATE users SET email=$2,name=$3,role=$4,status=$5,fee_override=$6,updated_at=now() WHERE id=$1", id, v.Email, v.Name, v.Role, v.Status, v.Fee)
		if e != nil {
			return nil, bad("邮箱已存在或修改失败")
		}
		if v.Password != "" {
			h, e := bcrypt.GenerateFromPassword([]byte(v.Password), bcrypt.DefaultCost)
			if e != nil {
				return nil, e
			}
			if _, e = tx.Exec(r.Context(), "UPDATE users SET password_hash=$2 WHERE id=$1", id, string(h)); e != nil {
				return nil, e
			}
		}
		if v.Password != "" || v.Status != "active" || v.Role != oldRole {
			if _, e = tx.Exec(r.Context(), "DELETE FROM sessions WHERE user_id=$1", id); e != nil {
				return nil, e
			}
			if _, e = tx.Exec(r.Context(), "UPDATE api_keys SET revoked=true WHERE user_id=$1", id); e != nil {
				return nil, e
			}
		}
		e = a.audit(r.Context(), tx, r, "user.updated", id, "资料、角色或单价修改")
		return M{"ok": true}, e
	})
}
func (a *App) pricing(w http.ResponseWriter, r *http.Request) {
	var v struct {
		IDs []string `json:"ids"`
		Fee *int64   `json:"fee_override"`
	}
	if !read(w, r, &v) {
		return
	}
	if len(v.IDs) < 1 || len(v.IDs) > 500 || (v.Fee != nil && (*v.Fee < 0 || *v.Fee > 100000000)) {
		fail(w, 400, "请选择账号并输入有效单价")
		return
	}
	sort.Strings(v.IDs)
	a.transaction(w, r, func(tx pgx.Tx) (any, error) {
		tag, e := tx.Exec(r.Context(), "UPDATE users SET fee_override=$2,updated_at=now() WHERE id=ANY($1)", v.IDs, v.Fee)
		if e != nil {
			return nil, e
		}
		e = a.audit(r.Context(), tx, r, "user.pricing", "batch", fmt.Sprintf("批量调整 %d 个账户单价", tag.RowsAffected()))
		return M{"updated": tag.RowsAffected()}, e
	})
}
func (a *App) adjustBalance(w http.ResponseWriter, r *http.Request) {
	var v struct {
		Amount int64  `json:"amount"`
		Reason string `json:"reason"`
	}
	if !read(w, r, &v) {
		return
	}
	key := r.Header.Get("Idempotency-Key")
	if len(key) < 8 || len(key) > 120 || v.Amount == 0 || len(v.Reason) < 2 || len(v.Reason) > 300 {
		fail(w, 400, "需要非零金额、调整原因和请求标识")
		return
	}
	payload, _ := json.Marshal(v)
	reqHash := hash(r.PathValue("id") + string(payload))
	a.transaction(w, r, func(tx pgx.Tx) (any, error) {
		lock := current(r).ID + ":" + key
		_, e := tx.Exec(r.Context(), "SELECT pg_advisory_xact_lock(hashtextextended($1,0))", lock)
		if e != nil {
			return nil, e
		}
		var old string
		var result []byte
		e = tx.QueryRow(r.Context(), "SELECT request_hash,result FROM mutation_keys WHERE key=$1", lock).Scan(&old, &result)
		if e == nil {
			if old != reqHash {
				return nil, businessError{409, "重复请求内容不一致"}
			}
			return M{"ok": true, "replayed": true}, nil
		}
		if e != pgx.ErrNoRows {
			return nil, e
		}
		if e = a.credit(tx, r, r.PathValue("id"), "adjustment", "adjust:"+lock, v.Reason, v.Amount); e != nil {
			return nil, e
		}
		if e = a.audit(r.Context(), tx, r, "balance.adjusted", r.PathValue("id"), fmt.Sprintf("调整 %d 分；%s", v.Amount, v.Reason)); e != nil {
			return nil, e
		}
		_, e = tx.Exec(r.Context(), "INSERT INTO mutation_keys(key,actor_id,request_hash,result) VALUES($1,$2,$3,'{\"ok\":true}')", lock, current(r).ID, reqHash)
		return M{"ok": true}, e
	})
}
func (a *App) adminOrders(w http.ResponseWriter, r *http.Request) {
	a.list(w, r, `SELECT o.id,o.user_id,o.product_id,o.product_name,o.email,o.country,o.seats,o.status,o.fee,o.source,o.workflow,o.keep_renewal,o.renewal,o.activation,o.note,o.created_at,o.updated_at,o.price_snapshot,u.email AS owner FROM orders o JOIN users u ON o.user_id=u.id WHERE ($1='' OR o.email ILIKE '%'||$1||'%' OR o.id ILIKE '%'||$1||'%' OR u.email ILIKE '%'||$1||'%') AND ($2='' OR o.status=$2) ORDER BY o.created_at DESC LIMIT 500`, r.URL.Query().Get("q"), r.URL.Query().Get("status"))
}
func (a *App) adminOrderDetail(w http.ResponseWriter, r *http.Request) {
	orders, e := a.query(r.Context(), `SELECT o.id,o.user_id,o.product_id,o.product_name,o.email,o.country,o.seats,o.status,o.fee,o.source,o.workflow,o.keep_renewal,o.renewal,o.activation,o.note,o.created_at,o.updated_at,o.price_snapshot,u.email AS owner,
 CASE WHEN o.workflow='subscription' THEN CASE WHEN EXISTS(SELECT 1 FROM subscription_protocol p WHERE p.order_id=o.id) THEN 'cslive'
 WHEN EXISTS(SELECT 1 FROM payment_runs p WHERE p.order_id=o.id AND (p.executor='legacy_http' OR p.legacy_checkpoint_enc<>'')) THEN 'legacy_protocol' ELSE 'legacy' END
 ELSE COALESCE((SELECT c.flow FROM checkout_runs c WHERE c.order_id=o.id),'') END AS subscription_flow,
 COALESCE((SELECT c.flow FROM checkout_runs c WHERE c.order_id=o.id),'') AS checkout_flow
 FROM orders o JOIN users u ON u.id=o.user_id WHERE o.id=$1`, r.PathValue("id"))
	if e != nil || len(orders) == 0 {
		fail(w, 404, "订单不存在")
		return
	}
	events, _ := a.query(r.Context(), "SELECT stage,message,created_at FROM order_events WHERE order_id=$1 ORDER BY id", r.PathValue("id"))
	links, e := a.adminCheckoutLinks(r.Context(), r.PathValue("id"))
	if e != nil {
		fail(w, 503, "结账链接记录暂时无法读取，请稍后刷新")
		return
	}
	closures, _ := a.query(r.Context(), "SELECT c.reason,c.prior_status,c.prior_payment_state,c.payment_submitted,c.created_at,u.email AS actor FROM order_closures c JOIN users u ON u.id=c.actor_id WHERE c.order_id=$1", r.PathValue("id"))
	out(w, 200, M{"order": orders[0], "events": events, "trace": a.paymentTraceDetails(r.Context(), r.PathValue("id")), "closures": closures, "checkout_links": links, "billing": a.adminOrderBilling(r.Context(), r.PathValue("id")), "checkout": a.checkoutDetails(r.Context(), r.PathValue("id")), "payment": a.paymentDetails(r.Context(), r.PathValue("id"))})
}
func (a *App) adminOrderNote(w http.ResponseWriter, r *http.Request) {
	var v struct {
		Note string `json:"note"`
	}
	if !read(w, r, &v) {
		return
	}
	if len(v.Note) > 1000 {
		fail(w, 400, "备注过长")
		return
	}
	a.transaction(w, r, func(tx pgx.Tx) (any, error) {
		tag, e := tx.Exec(r.Context(), "UPDATE orders SET note=$2,updated_at=now() WHERE id=$1", r.PathValue("id"), v.Note)
		if e = checkRows(tag.RowsAffected(), e); e != nil {
			return nil, e
		}
		e = a.audit(r.Context(), tx, r, "order.noted", r.PathValue("id"), "更新运营备注")
		return M{"ok": true}, e
	})
}
func (a *App) adminTopups(w http.ResponseWriter, r *http.Request) {
	a.list(w, r, "SELECT t.*,u.email FROM topups t JOIN users u ON u.id=t.user_id ORDER BY (t.status='pending') DESC,t.created_at DESC LIMIT 500")
}
func (a *App) reviewTopup(w http.ResponseWriter, r *http.Request) {
	var v struct {
		Action string `json:"action"`
		Note   string `json:"note"`
	}
	if !read(w, r, &v) {
		return
	}
	if (v.Action != "approve" && v.Action != "reject") || len(v.Note) < 2 || len(v.Note) > 300 {
		fail(w, 400, "请选择审核结果并填写审核依据")
		return
	}
	a.transaction(w, r, func(tx pgx.Tx) (any, error) {
		var owner, status string
		var amount int64
		e := tx.QueryRow(r.Context(), "SELECT user_id,status,amount FROM topups WHERE id=$1 FOR UPDATE", r.PathValue("id")).Scan(&owner, &status, &amount)
		if e != nil {
			return nil, bad("申请不存在")
		}
		desired := "rejected"
		if v.Action == "approve" {
			desired = "approved"
		}
		if status == desired {
			return M{"ok": true, "replayed": true}, nil
		}
		if status != "pending" {
			return nil, businessError{409, "此申请已处理"}
		}
		if desired == "approved" {
			if e = a.credit(tx, r, owner, "topup", "topup:"+r.PathValue("id"), "人工核实充值到账", amount); e != nil {
				return nil, e
			}
		}
		_, e = tx.Exec(r.Context(), "UPDATE topups SET status=$2,note=$3,reviewed_by=$4,updated_at=now() WHERE id=$1", r.PathValue("id"), desired, v.Note, current(r).ID)
		if e == nil {
			e = a.audit(r.Context(), tx, r, "topup."+desired, r.PathValue("id"), v.Note)
		}
		return M{"ok": true}, e
	})
}
func (a *App) codes(w http.ResponseWriter, r *http.Request) {
	a.list(w, r, "SELECT c.id,c.prefix,c.amount,c.batch,c.status,c.used_at,c.expires_at,c.created_at,u.email AS used_by FROM redeem_codes c LEFT JOIN users u ON c.used_by=u.id ORDER BY c.created_at DESC LIMIT 1000")
}
func (a *App) createCodes(w http.ResponseWriter, r *http.Request) {
	var v struct {
		Count  int   `json:"count"`
		Amount int64 `json:"amount"`
		Days   int   `json:"days"`
	}
	if !read(w, r, &v) {
		return
	}
	if v.Count < 1 || v.Count > 200 || v.Amount < 100 || v.Amount > 10000000 || v.Days < 1 || v.Days > 3660 {
		fail(w, 400, "数量 1～200、面值至少 1 R、有效期 1～3660 天")
		return
	}
	a.transaction(w, r, func(tx pgx.Tx) (any, error) {
		batch := time.Now().Format("20060102") + "-" + uid()[:6]
		codes := []string{}
		for i := 0; i < v.Count; i++ {
			code := "XP-" + strings.ToUpper(uid()[:24])
			enc, e := a.encrypt(code)
			if e != nil {
				return nil, e
			}
			_, e = tx.Exec(r.Context(), "INSERT INTO redeem_codes(id,code_hash,code_enc,prefix,amount,batch,expires_at) VALUES($1,$2,$3,$4,$5,$6,$7)", uid(), hash(code), enc, code[:7]+"…"+code[len(code)-4:], v.Amount, batch, time.Now().Add(time.Duration(v.Days)*24*time.Hour))
			if e != nil {
				return nil, e
			}
			codes = append(codes, code)
		}
		e := a.audit(r.Context(), tx, r, "codes.created", batch, fmt.Sprintf("生成 %d 个兑换码", v.Count))
		return M{"codes": codes, "batch": batch}, e
	})
}
func (a *App) batchCodes(w http.ResponseWriter, r *http.Request) {
	var v struct {
		IDs    []string `json:"ids"`
		Action string   `json:"action"`
	}
	if !read(w, r, &v) {
		return
	}
	if len(v.IDs) < 1 || len(v.IDs) > 1000 {
		fail(w, 400, "请选择兑换码")
		return
	}
	a.transaction(w, r, func(tx pgx.Tx) (any, error) {
		var result any = M{"ok": true}
		switch v.Action {
		case "export":
			rows, e := tx.Query(r.Context(), "SELECT code_enc FROM redeem_codes WHERE id=ANY($1) AND status='active' AND expires_at>now() ORDER BY created_at,id", v.IDs)
			if e != nil {
				return nil, e
			}
			codes := []string{}
			for rows.Next() {
				var enc string
				if e = rows.Scan(&enc); e != nil {
					rows.Close()
					return nil, e
				}
				s, e := a.decrypt(enc)
				if e != nil {
					rows.Close()
					return nil, e
				}
				codes = append(codes, s)
			}
			e = rows.Err()
			rows.Close()
			if e != nil {
				return nil, e
			}
			result = M{"codes": codes}
		case "disable":
			tag, e := tx.Exec(r.Context(), "UPDATE redeem_codes SET status='disabled' WHERE id=ANY($1) AND status='active'", v.IDs)
			if e != nil {
				return nil, e
			}
			result = M{"updated": tag.RowsAffected()}
		case "delete":
			tag, e := tx.Exec(r.Context(), "DELETE FROM redeem_codes WHERE id=ANY($1) AND status!='used'", v.IDs)
			if e != nil {
				return nil, e
			}
			result = M{"deleted": tag.RowsAffected()}
		default:
			return nil, bad("操作不支持")
		}
		e := a.audit(r.Context(), tx, r, "codes."+v.Action, "batch", fmt.Sprintf("选择 %d 条；已使用记录保留", len(v.IDs)))
		return result, e
	})
}
func (a *App) saveProduct(w http.ResponseWriter, r *http.Request) {
	var v struct {
		Name        string   `json:"name"`
		Description string   `json:"description"`
		Fee         int64    `json:"fee"`
		Enabled     bool     `json:"enabled"`
		Sort        int      `json:"sort"`
		Tag         string   `json:"tag"`
		Features    []string `json:"features"`
	}
	if !read(w, r, &v) {
		return
	}
	if len(v.Name) < 1 || len(v.Name) > 80 || len(v.Description) > 300 || v.Fee < 0 || v.Fee > 100000000 || len(v.Features) > 10 {
		fail(w, 400, "套餐参数无效")
		return
	}
	b, _ := json.Marshal(v.Features)
	a.transaction(w, r, func(tx pgx.Tx) (any, error) {
		tag, e := tx.Exec(r.Context(), "UPDATE products SET name=$2,description=$3,fee=$4,enabled=$5,sort=$6,tag=$7,features=$8 WHERE id=$1", r.PathValue("id"), v.Name, v.Description, v.Fee, v.Enabled, v.Sort, v.Tag, b)
		if e = checkRows(tag.RowsAffected(), e); e != nil {
			return nil, e
		}
		e = a.audit(r.Context(), tx, r, "product.updated", r.PathValue("id"), "调整套餐配置")
		return M{"ok": true}, e
	})
}
func (a *App) saveAnnouncement(w http.ResponseWriter, r *http.Request) {
	var v struct {
		Title        string     `json:"title"`
		Body         string     `json:"body"`
		Tone         string     `json:"tone"`
		Published    bool       `json:"published"`
		LoginVisible bool       `json:"login_visible"`
		Sort         int        `json:"sort"`
		Starts       *time.Time `json:"starts_at"`
		Ends         *time.Time `json:"ends_at"`
	}
	if !read(w, r, &v) {
		return
	}
	if len(v.Title) < 1 || len(v.Title) > 150 || len(v.Body) < 1 || len(v.Body) > 5000 || (v.Tone != "info" && v.Tone != "warning" && v.Tone != "success") || (v.Starts != nil && v.Ends != nil && !v.Ends.After(*v.Starts)) {
		fail(w, 400, "公告内容或时间无效")
		return
	}
	id := r.PathValue("id")
	if id == "" {
		id = uid()
	}
	a.transaction(w, r, func(tx pgx.Tx) (any, error) {
		_, e := tx.Exec(r.Context(), `INSERT INTO announcements(id,title,body,tone,published,login_visible,sort,starts_at,ends_at) VALUES($1,$2,$3,$4,$5,$6,$7,$8,$9) ON CONFLICT(id) DO UPDATE SET title=$2,body=$3,tone=$4,published=$5,login_visible=$6,sort=$7,starts_at=$8,ends_at=$9`, id, v.Title, v.Body, v.Tone, v.Published, v.LoginVisible, v.Sort, v.Starts, v.Ends)
		if e == nil {
			e = a.audit(r.Context(), tx, r, "announcement.saved", id, "保存公告")
		}
		return M{"id": id}, e
	})
}
func (a *App) deleteAnnouncement(w http.ResponseWriter, r *http.Request) {
	a.transaction(w, r, func(tx pgx.Tx) (any, error) {
		tag, e := tx.Exec(r.Context(), "DELETE FROM announcements WHERE id=$1", r.PathValue("id"))
		if e = checkRows(tag.RowsAffected(), e); e != nil {
			return nil, e
		}
		return M{"ok": true}, a.audit(r.Context(), tx, r, "announcement.deleted", r.PathValue("id"), "删除公告")
	})
}
func (a *App) risks(w http.ResponseWriter, r *http.Request) {
	events, e := a.query(r.Context(), "SELECT e.*,u.email FROM risk_events e LEFT JOIN users u ON u.id=e.user_id WHERE ($1='' OR e.severity=$1) ORDER BY e.id DESC LIMIT 300", r.URL.Query().Get("severity"))
	if e != nil {
		fail(w, 500, "读取失败")
		return
	}
	blocks, _ := a.query(r.Context(), "SELECT * FROM blocked_ips WHERE expires_at>now() ORDER BY created_at DESC")
	out(w, 200, M{"items": events, "blocks": blocks, "config": a.config(r.Context(), "risk")})
}
func (a *App) resolveRisk(w http.ResponseWriter, r *http.Request) {
	a.transaction(w, r, func(tx pgx.Tx) (any, error) {
		tag, e := tx.Exec(r.Context(), "UPDATE risk_events SET resolved=true WHERE id=$1", r.PathValue("id"))
		if e = checkRows(tag.RowsAffected(), e); e != nil {
			return nil, e
		}
		return M{"ok": true}, a.audit(r.Context(), tx, r, "risk.resolved", r.PathValue("id"), "标记已核查")
	})
}
func (a *App) blockIP(w http.ResponseWriter, r *http.Request) {
	var v struct {
		IP      string `json:"ip"`
		Reason  string `json:"reason"`
		Minutes int    `json:"minutes"`
	}
	if !read(w, r, &v) {
		return
	}
	if net.ParseIP(v.IP) == nil || v.IP == ip(r) || v.Minutes < 1 || v.Minutes > 10080 || len(v.Reason) < 2 || len(v.Reason) > 300 {
		fail(w, 400, "IP/时长/原因无效，不能限制当前来源")
		return
	}
	a.transaction(w, r, func(tx pgx.Tx) (any, error) {
		_, e := tx.Exec(r.Context(), "INSERT INTO blocked_ips(ip,reason,expires_at) VALUES($1,$2,$3) ON CONFLICT(ip) DO UPDATE SET reason=$2,expires_at=$3", v.IP, v.Reason, time.Now().Add(time.Duration(v.Minutes)*time.Minute))
		if e == nil {
			e = a.audit(r.Context(), tx, r, "risk.blocked", v.IP, v.Reason)
		}
		return M{"ok": true}, e
	})
}
func (a *App) unblockIP(w http.ResponseWriter, r *http.Request) {
	a.transaction(w, r, func(tx pgx.Tx) (any, error) {
		_, e := tx.Exec(r.Context(), "DELETE FROM blocked_ips WHERE ip=$1", r.PathValue("ip"))
		if e == nil {
			e = a.audit(r.Context(), tx, r, "risk.unblocked", r.PathValue("ip"), "解除来源限制")
		}
		return M{"ok": true}, e
	})
}
func (a *App) financeData(r *http.Request) (M, error) {
	period := r.URL.Query().Get("period")
	if period != "day" && period != "week" && period != "month" && period != "year" {
		period = "day"
	}
	start := r.URL.Query().Get("start")
	end := r.URL.Query().Get("end")
	if start == "" {
		start = time.Now().AddDate(0, 0, -30).Format("2006-01-02")
	}
	if end == "" {
		end = time.Now().Format("2006-01-02")
	}
	s, e := time.Parse("2006-01-02", start)
	if e != nil {
		return nil, bad("开始日期无效")
	}
	t, e := time.Parse("2006-01-02", end)
	if e != nil || t.Before(s) || t.Sub(s) > 3660*24*time.Hour {
		return nil, bad("统计日期范围无效")
	}
	exclude := r.URL.Query().Get("include_admin") != "true"
	args := []any{start, end, exclude}
	where := ` WHERE l.created_at>=($1::date AT TIME ZONE 'Asia/Shanghai') AND l.created_at<(($2::date+1) AT TIME ZONE 'Asia/Shanghai') AND (NOT $3::bool OR u.role!='admin') `
	totals, e := a.query(r.Context(), `SELECT COALESCE(sum(l.amount) FILTER(WHERE l.kind IN ('topup','redeem')),0) AS credited,COALESCE(sum(-l.amount) FILTER(WHERE l.kind='charge'),0) AS spent,COALESCE(sum(l.amount) FILTER(WHERE l.kind='adjustment'),0) AS adjusted,count(*) AS entries FROM ledger l JOIN users u ON u.id=l.user_id`+where, args...)
	if e != nil {
		return nil, e
	}
	trend, e := a.query(r.Context(), `SELECT to_char(date_trunc('`+period+`',l.created_at AT TIME ZONE 'Asia/Shanghai'),'YYYY-MM-DD') AS period,COALESCE(sum(l.amount) FILTER(WHERE l.kind IN ('topup','redeem')),0) AS credited,COALESCE(sum(-l.amount) FILTER(WHERE l.kind='charge'),0) AS spent,COALESCE(sum(l.amount) FILTER(WHERE l.kind='adjustment'),0) AS adjusted FROM ledger l JOIN users u ON u.id=l.user_id`+where+` GROUP BY 1 ORDER BY 1`, args...)
	if e != nil {
		return nil, e
	}
	users, e := a.query(r.Context(), `SELECT u.id,u.email,u.name,u.role,u.balance,u.frozen,u.fee_override,COALESCE(x.credited,0) AS credited,COALESCE(x.spent,0) AS spent,COALESCE(x.adjusted,0) AS adjusted,(SELECT count(*) FROM orders o WHERE o.user_id=u.id AND o.created_at>=($1::date AT TIME ZONE 'Asia/Shanghai') AND o.created_at<(($2::date+1) AT TIME ZONE 'Asia/Shanghai')) AS orders FROM users u LEFT JOIN (SELECT l.user_id,sum(l.amount) FILTER(WHERE l.kind IN ('topup','redeem')) AS credited,sum(-l.amount) FILTER(WHERE l.kind='charge') AS spent,sum(l.amount) FILTER(WHERE l.kind='adjustment') AS adjusted FROM ledger l WHERE l.created_at>=($1::date AT TIME ZONE 'Asia/Shanghai') AND l.created_at<(($2::date+1) AT TIME ZONE 'Asia/Shanghai') GROUP BY l.user_id) x ON x.user_id=u.id WHERE (NOT $3::bool OR u.role!='admin') ORDER BY credited DESC,u.email`, args...)
	if e != nil {
		return nil, e
	}
	balances, e := a.query(r.Context(), "SELECT COALESCE(sum(balance),0) AS balance,COALESCE(sum(frozen),0) AS frozen FROM users WHERE (NOT $1::bool OR role!='admin')", exclude)
	if e != nil {
		return nil, e
	}
	return M{"totals": totals[0], "current": balances[0], "trend": trend, "users": users, "period": period, "start": start, "end": end, "timezone": "Asia/Shanghai"}, nil
}
func (a *App) finance(w http.ResponseWriter, r *http.Request) {
	v, e := a.financeData(r)
	if e != nil {
		fail(w, 400, "统计日期或查询无效")
		return
	}
	out(w, 200, v)
}
func (a *App) financeExport(w http.ResponseWriter, r *http.Request) {
	v, e := a.financeData(r)
	if e != nil {
		fail(w, 400, "统计查询无效")
		return
	}
	w.Header().Set("Content-Type", "text/csv; charset=utf-8")
	w.Header().Set("Content-Disposition", "attachment; filename=xupay-finance.csv")
	w.Write([]byte{0xef, 0xbb, 0xbf})
	c := csv.NewWriter(w)
	c.Write([]string{"账户", "称呼", "充值分", "使用分", "调整分", "当前可用分", "当前冻结分", "订单数"})
	for _, u := range v["users"].([]M) {
		row := []string{}
		for _, k := range []string{"email", "name", "credited", "spent", "adjusted", "balance", "frozen", "orders"} {
			row = append(row, csvSafe(fmt.Sprint(u[k])))
		}
		c.Write(row)
	}
	c.Flush()
}

var _ = strconv.Itoa
