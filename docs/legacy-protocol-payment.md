> 2026-10-31最新：旧协议恢复为独立第三套legacy_protocol，旧浏览器legacy保留；完整HAR差异、动态SDK边界及本地验证见[最新说明](legacy-complete-flow-20261031.md)。

> 2026-10-31 当前规则：已改为一个完整流程选择，旧为脚本提链＋备份浏览器付款，新为 cs-live 完整流程，不再支持混搭。下面保留历史记录，当前实现和验证请以 [整套流程说明](whole-subscription-flows-20261031.md) 为准。

# 旧付款协议执行器

2026-10-30：`payment.subscription_flow=legacy` 改为 Go HTTPS 执行，不启动 Chromium/Playwright/Node，不回退到浏览器。`cslive` 付款未改。独立提链和四种提链/付款组合保持。

2026-10-31 对照修正：Hosted 使用成功抓包的 `2020-08-27;custom_checkout_beta=v1`，仅在 PaymentMethod 与 confirm 表单发送，不再额外发送 `Stripe-Version` 请求头；init/tax/read 不增加表单版本。Hosted 初始化语言/时区采用样本 zh-CN / Asia/Shanghai，Origin/Referer 绑定原结账来源。Custom 分支继续原 Basil 协议版本和请求头。管理员轨迹只显示实际显式发送的版本，已识别的版本无效响应只投影固定原因。

Hosted 的 `config_id` 随本单税区更新及最终核价响应变化；PaymentMethod/confirm 共用最新配置和原 Checkout 的五项业务关联字段，confirm 补 link_brand。配置仅在本次执行器内存中保存，不跨订单、不从历史 HAR 回填。当前对齐的是普通业务字段，SDK 会话、设备及动态验证仍未实现；成功样本字段对比仍为 19/24、12/19，不能称完整等价或已证明解决真实 HTTP400。详细差异与验证见 `legacy-payment-har-gap-20261030.md`。

## 执行顺序

1. 读取原 Checkout、后台随机账单快照、原代理端点及供应商会话。每次请求检查任务、资源启用、代理期限和容量租约；发送卡片资料、每次确认付款前，经原代理检测出口并与提链基准比较。不一致、缺少基准或探测失败均停止，不直连兜底。
2. 长链从原 URL fragment 读取公开 Stripe key，必要时使用当前账号的官方 bootstrap；初始化原 Payment Page，更新真实账单税区，再 GET 原页面取得含税报价。短链保持原类型：`cs_live` 使用原 Payment Page，`oaics` 使用商户 taxes/confirm 和所返回的原 PaymentIntent。
3. 原有数据库报价及提交防重事务先完成，协议执行器才接受 confirm。确认前再次取得报价；金额、币种或税务状态不一致则停止。卡片只在内存中进入 Stripe 请求；不保存卡号/CVV，不输出请求体、Cookie、AT、代理凭据或原始响应。
4. 长链创建 PaymentMethod 后仅提交一次 `/payment_pages/{原 cs}/confirm`；oaics 创建 ConfirmationToken，商户确认一次，保存返回的原 PI secret 后查询原 PI，只有明确处于初次可确认状态才接续一次 PI confirm。两条付款分支互斥。
5. 响应必须绑定原 Checkout/PI、报价及币种。Payment Page 的 `ppage_` 不是 Checkout ID，使用 `session_id` 核对。只有权威 paid/complete/succeeded 或原 PI 的实际成功证据才结算服务费；`amount_received` 缺失时不把报价冒充实收。
6. 付款成功进入原旧版后处理：先刷新 Session、校验并加密保存新 AT，再查询订阅及套餐；续费处理原样保留。上游 webhook 到达可能有延迟，由原有订阅核实轮询处理；不把访问 success 页面当成功证据。

## 中断和恢复

- 新加 `payment_runs.legacy_checkpoint_enc`，仅加密保存原 Checkout、公钥及恢复所需原 PI secret；终态或原代理过期清理。查询接口不投影该字段。数据库迁移只新增默认空列，旧程序可兼容回滚。
- 确认请求没有自动重试。响应丢失时最多补一次原交易 GET；未确认就进入待核实并保留资金状态。之后的核实只 GET，不能重新创建付款方式、更新税区、confirm 或 approve。
- 官方返回验证码、3DS、额外批准或认证挑战时暂停，旧付款不打开验证浏览器。历史 `continue_approval` 入口返回 409，后台旧流程不显示服务端浏览器验证按钮。
- 历史 oaics 单缺少原 PI 检查点时无法安全自动恢复，保留待核实；不会通过再次 confirm 获取 PI。

## 抓包证据边界

两次不同订单的抓包用于确认接口顺序和字段结构，不混用 Checkout、付款方式或动态令牌。成功抓包包含动态 passive captcha 字段，本实现不重放这些字段，也不生成设备指纹或解算验证；因此纯协议实现不保证每笔上游都接受。离线测试通过不等于真实扣款成功，本次改造及发布不进行真实付款或重放历史订单。

## 付款前出口与错误诊断（2026-10-30 后续要求）

新旧提链保存本单首次出口观察值。旧新交接保留该值，新提链在创建结账请求前采集；付款检查只比较，不改写基准。旧协议在 PaymentMethod/ConfirmationToken 创建和每次确认请求前检查，新流程在对应 HTTP 请求钩子检查，官方 SDK 接续开始前检查。原代理端点、供应商 session、账单快照、长短链配置均保持。探测前后复查任务和容量租约，探测超时上限8秒，无重试、无直连及跳转。

出口检查只能证明采样时一致，不能保证代理供应商此后绝不切换。缺少提链基准的历史单不能通过付款前新取值放行；已提交的未知交易仍只能按原规则核实。

供应商会话仅对 `geo.iproyal.com` 自动生成。其他 HTTP/HTTPS/SOCKS5/SOCKS5h 代理原样使用后台端点和凭据，不要求或添加 `session` / `lifetime` 参数；已有凭据中的同名内容也不修改。所有代理仍保存本地订单绑定并检查付款前出口，本地绑定的保留期限不代表给其他供应商开启了粘性会话。

时间线区分“准备银行卡付款”和“提交付款确认”。创建付款方式失败不会记录已发送确认。HTTP 错误只保留白名单错误类型、参数名、错误码及固定原因标签，不保存原始错误消息或正文；只对管理员显示，客户接口不投影内部诊断。
