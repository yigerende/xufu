package platform

import (
	"context"
	"time"

	"github.com/jackc/pgx/v5"
)

var paymentPhases = map[string]string{
	"card_prepare":    "准备银行卡付款",
	"verification":    "官方付款验证",
	"session_refresh": "刷新账号授权", "account_check": "在线核对账号", "activation": "核实订阅生效",
	"accepted": "申请受理与资料校验", "queue": "等待办理资源", "funds": "服务费处理", "complete": "订阅流程结束",
	"proxy": "分配办理网络", "checkout": "生成结账信息", "network": "检查办理网络",
	"opening": "打开付款页面", "billing": "填写账单资料", "pricing": "核对金额与税费",
	"confirming": "提交付款", "submitted": "付款请求已提交", "checking": "核实付款结果",
	"result": "付款处理结果", "restart": "重建会话与结账信息", "renewal": "取消自动续费",
}
var traceCodes = map[string]string{
	"exit_ip_recorded":             "已采集提链时的出口 IP",
	"exit_ip_matched":              "付款前出口 IP 与提链记录一致",
	"hosted_url_missing":           "未取得真实结账长链，已停止继续办理",
	"hosted_url_invalid":           "结账长链校验未通过，已停止继续办理",
	"checkout_runtime_unavailable": "提链运行环境缺少 curl",
	"subscription_absent":          "当前账号尚无订阅，属于新开通前的正常查询结果",
	"payment_pending":              "原付款尚未取得最终结果，保留原交易待核实",
	"verification_continuing":      "原交易需要后续验证，实际接续情况以官方付款验证记录为准",
	"verification_ready":           "原付款验证入口已就绪，请在有效期内打开并点击继续验证",
	"action_guard_overloaded":      "验证资源检查数量异常，已停止本轮验证并保留原交易",
	"automatic_verification":       "服务端自动接续原交易的官方验证", "action_timeout": "官方验证在有效期内未完成，原交易保留待核实",
	"action_runtime_unavailable": "验证窗口暂不可用，原交易保留待核实", "action_interrupted": "验证窗口已结束，原交易仍需核实",
	"account_mismatch": "账号或空间不一致", "account_status_unknown": "暂未取得明确账号状态", "existing_subscription": "账号已有有效订阅", "personal_account_required": "请选择个人空间办理",
	"session_refresh_unavailable": "当前授权不包含自动刷新凭据", "subscription_not_found": "订阅尚未同步", "activation_pending": "已付款，等待订阅同步", "activation_review": "已付款，订阅生效待核实", "plan_mismatch": "订阅套餐与本单不符", "internal_error": "阶段状态保存未完成",
	"hold": "服务费已冻结", "release": "服务费已解冻", "charge": "服务费已结算", "kept": "按客户选择保留自动续费",
	"waiting_proxy": "等待对应地区的可用代理", "retry": "按配置间隔等待重试", "network_restarted": "未提交扣款，重建网络会话后重新提链",
	"": "", "network": "网络连接异常", "upstream_busy": "上游繁忙", "upstream_unavailable": "上游暂不可用",
	"invalid_session": "账号授权失效", "invalid_response": "响应未能确认", "access_denied": "上游拒绝访问",
	"proxy_country_mismatch": "出口地区不匹配", "proxy_unavailable": "办理网络不可用", "request_rejected": "请求被拒绝",
	"disabled": "已确认关闭", "pending": "等待再次核实", "review": "需要核实", "completed": "已完成",
}

// Critical milestones share the business transaction so committed financial or
// terminal outcomes cannot be shown differently from their diagnostic record.
func paymentTraceTx(ctx context.Context, tx pgx.Tx, id, phase, state, code string) error {
	_, err := tx.Exec(ctx, `INSERT INTO payment_trace_events(order_id,phase,state,code,round,elapsed_ms)
 SELECT $1,$2,$3,$4,COALESCE((SELECT network_restarts+1 FROM payment_runs WHERE order_id=$1),1),0`, id, phase, state, code)
	return err
}

// Only fixed labels/codes are stored. No upstream URL, raw error or credential is
// accepted by this path. A short timeout prevents diagnostics holding business work.
func (a *App) paymentTrace(ctx context.Context, id, phase, state, code string, elapsed time.Duration) {
	if _, ok := paymentPhases[phase]; !ok {
		return
	}
	switch state {
	case "running", "completed", "failed", "review", "waiting":
	default:
		return
	}
	if _, ok := traceCodes[code]; !ok {
		if _, ok = paymentMessages[code]; !ok {
			code = "unclassified"
		}
	}
	save, cancel := context.WithTimeout(context.WithoutCancel(ctx), 500*time.Millisecond)
	defer cancel()
	a.DB.Exec(save, `INSERT INTO payment_trace_events(order_id,phase,state,code,round,elapsed_ms)
 SELECT $1,$2,$3,$4,COALESCE((SELECT network_restarts+1 FROM payment_runs WHERE order_id=$1),1),$5
 WHERE EXISTS(SELECT 1 FROM orders WHERE id=$1)`, id, phase, state, code, max(int64(0), elapsed.Milliseconds()))
}

func (a *App) paymentTraceDetails(ctx context.Context, id string) []M {
	rows, e := a.query(ctx, "SELECT phase,state,code,round,elapsed_ms,created_at,http_status,request_id,diagnostics FROM payment_trace_events WHERE order_id=$1 ORDER BY created_at,id", id)
	if e != nil {
		return []M{}
	}
	for _, row := range rows {
		row["label"] = paymentPhases[str(row, "phase")]
		code := str(row, "code")
		diagnostics, _ := row["diagnostics"].(map[string]any)
		httpStatus, _ := row["http_status"].(int32)
		// Historical pending observations describe an unresolved result, not a
		// completed payment or a confirmed decline. Keep stored audit rows intact.
		if httpStatus < 400 && (code == "pending" || code == "payment_pending" || diagnostics["payment_state"] == "pending") {
			row["state"] = "review"
		}
		description, ok := traceCodes[code]
		if !ok {
			description = paymentMessages[code]
		}
		if description == "" && code != "" {
			description = "阶段未完成，请结合前后记录核实"
		}
		// Old progress rows described an intermediate SDK handoff as a request
		// for manual verification. Correct presentation without rewriting audit data.
		if code == "verification_required" && (str(row, "phase") == "confirming" || str(row, "phase") == "checking") {
			description = "原交易要求后续验证；请查看官方付款验证入口及后续记录"
		}
		row["description"] = description
	}
	return rows
}
