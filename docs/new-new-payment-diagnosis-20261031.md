# 2026-10-31 本地新新付款停留诊断

订单 `f49d5c66581c09cd24584bf051dcb8a6`，2026-10-31 11:21:56（Asia/Shanghai）创建，PH / Pro5x，cslive 提链＋cslive 付款，hosted 长链。

截至 11:31:06：订单 processing，付款 checking，submitted=true，服务费 held，执行租约有效。含税报价 579464 PHP 最小货币单位，即 PHP 5794.64；这只是报价，不是已确认实收。没有已扣款成功证据。

## 已确认事实

| 时间 | 本地证据 |
| --- | --- |
| 11:22:08 | Session 刷新 HTTP200。 |
| 11:22:42 | 记录本单提链出口基准。 |
| 11:23:38 | 卡资料提交前出口一致。 |
| 11:23:44 | 创建付款方式阶段 HTTP200。 |
| 11:23:50 | 付款确认前出口一致。 |
| 11:23:56 | 确认接口 HTTP200，随后原页面为 open/unpaid/pending；HTTP200不是付款成功。 |
| 11:24:22 | 后续确认/批准前出口一致。 |
| 11:24:30 | 后续确认阶段 HTTP200。 |
| 11:24:33 | 原 PaymentIntent 返回 requires_action，next_action=use_stripe_sdk，sdk_action_type=intent_confirmation_challenge；页面仍unpaid。自动验证启动。 |
| 11:24:37 | SDK窗口启动前出口一致。 |
| 11:25:33 | hCaptcha API 请求 HTTP200。 |
| 11:25:40～11:30:40 | SDK持续停在sdk_action，无SDK结果；累计46个资源请求，45完成、1失败、0待完成；最新等待338918ms。 |

4次付款/SDK前出口比对全部通过，没有proxy_session_changed、proxy_ip_check_failed或proxy_session_expired。没有SDK HTTP>=400或csp_blocked记录。唯一失败是11:25:29的一条hCaptcha静态资源fetch，浏览器报告ERR_ABORTED；其他验证资源和API随后返回200。不能把单个ERR_ABORTED直接当成挑战无法完成的唯一原因。提链准备中另有辅助请求403，但流程随后继续到付款确认，不能将该403表述为创建付款方式失败。

## 代码对应

- `runtime/payment-action-sdk.js:23`：已进入 `await stripe.handleNextAction({clientSecret: ...})`，尚未返回sdk_returned/sdk_error。context读取、SDK加载及原PaymentIntent前置检查已经完成。
- `internal/platform/subscription_protocol_action.go:32`：自动接续使用automatic=true，不发布交互会话或截图。
- `runtime/payment-action.mjs:37`：自动模式不接受点击、文字等人工输入。
- `runtime/payment-action.mjs:51`：启动headless Chromium；最多10分钟并受原代理到期限制。
- `runtime/payment-action.mjs:53`：未完成则超时，保留原交易；不会伪造SDK完成。
- `internal/platform/subscription_protocol_action.go:34`：SDK失败/超时保留先前action_required状态；正常完成才Recover核实原交易。最终结果由真实上游状态决定。

这能解释页面一直显示处理中的机制：新协议已提交原交易，但官方要求额外验证；自动模式正在等待SDK结果。若官方要求本人交互，当前自动模式无法替本人完成。现有日志没有验证码界面的可见性或最终SDK拒绝码，尚不能证明这次一定显示了交互题目，也不能确定上游为何触发挑战。

## 本次操作范围与后续

本次只读本地订单、脱敏轨迹和代码；没有读取或重放用户本次新Session附件，没有使用银行卡，没有取消/重开订单、再次确认付款、触发新的验证、重启在途服务或修改付款代码。当前结果不明，不按“未扣款失败”重新发起。

本单仍有原验证执行租约，不能并行再开原单验证。若自动验证最终转待验证，可由获授权的人通过现有“继续原付款验证”入口处理原交易，再由Recover核实；这是处理原单而非重新订阅。最终订阅付款及真人验证由用户完成。以上为11:31:06现场快照，后续状态应再次只读核对，不能将预计超时写成已经发生。

## 11:41:53 只读复核更新

11:34:38 已实际记录 `action_timeout`，自动验证结束；当前订单 `review`、付款 `action_required`、错误码 `verification_required`，submitted=true、fee_state=held，原协议执行租约为空。没有新增付款成功证据。

再次核对事件687（11:24:33）：`diagnostic_source=payment_intent`、`raw_status=requires_action`、`next_action_type=use_stripe_sdk`、`sdk_action_type=intent_confirmation_challenge`。代码从上游 `next_action.use_stripe_sdk.type` 提取类型，足以确认上游提出追加验证要求；不能据此声称已经看见选图/滑块题目或银行3DS页面。本次仍只读，没有打开或完成验证、再次提交付款或重新下单。

## 付款设备信息核查

结论：有协议侧基础设备/会话参数，但没有证据表明实现了完整、连续的真实浏览器 SDK 环境。此次只核对代码，没有调整设备信号、启动验证或重放付款。

- `internal/cslive/stripe_client.go:123` 从设备注册响应建立并保存 guid/muid/sid；`stripe_hosted.go:347` 和 `:377` 在创建付款方式和确认中复用。付款请求另有 payment_user_agent、time_on_page 和 client_attribution_metadata。
- 协议侧的浏览器画像是代码预设值，设备注册载荷由程序组装；这不同于在真实付款页面中由官方 SDK 完整采集当前环境及交互。
- `internal/cslive/stripe_hosted.go:371` 附近明确没有被动验证码令牌；先前完整原包对照也确认原包 HostedForm 同样缺这些字段。不能把代码注释所称“缺失就会升级验证”当成此次 Stripe 决策的已证实原因。
- `internal/platform/payment_action.go:22` 和 `:301` 交接代理、期限、公钥、原 PaymentIntent secret 和自动模式；没有交接协议侧 Stripe Cookie 或浏览器画像。`runtime/payment-action.mjs:56` 创建全新上下文，未设置与协议画像对应的 UA、语言、时区，也未导入原 Stripe Cookie。原交易和代理得到沿用，但没有继承完整浏览器会话。
- 验证浏览器的创建发生在 requires_action 返回之后，因此该浏览器环境差异不能解释此前已经发生的挑战触发；它对后续验证能否完成的影响也尚未被实测证实。现有证据仍仅能确认“追加验证未完成并超时”。
