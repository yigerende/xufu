# 2026-10-30 18:51 旧协议与成功 HAR 的完整字段差异

## 2026-10-31 修正进度（覆盖下文的“当前代码”计数）

已重新读取三份原始抓包，只提取字段名和普通协议常量。`付款打开包括税率计算` 中同一订单的 `config_id` 在税区更新后发生变化，因此不能只保存初始化值。本轮实现跟随原订单初始化、税区响应及付款前最后一次页面读取更新配置编号和 EID，并拒绝其他订单或格式异常的上下文。创建 PaymentMethod 与 confirm 共用本次最终读取的上下文；只读恢复不需要或重建这些付款参数。

- 两次付款请求都新增 `checkout_session_id`、`checkout_config_id`、`merchant_integration_source=checkout`、`merchant_integration_version=hosted_checkout`、`payment_method_selection_flow=automatic` 五个 attribution 字段。配置编号来自本单上游响应，不从 HAR 或其他订单取得。
- Hosted confirm 新增 `link_brand=link`；初始化地区语言和时区对齐样本 `zh-CN` / `Asia/Shanghai`。Hosted 移除额外的 `Stripe-Version` 请求头，付款表单保留样本 `_stripe_version`。Origin/Referer 取原有效结账 URL 的源，pay.openai.com 的订单对应样本的来源；不跟随外部响应跳转。Custom 短链保持原请求结构和版本。
- 新增字段名诊断白名单，仍不记录参数值、上游 message 或原始请求/响应。Hosted init/read 没有显式版本时，不再把本程序的付款版本伪称为该次请求的已发送版本。
- 与成功样本相比，现在 PaymentMethod **19/24** 个表单字段，confirm **12/19** 个。缺少的仍是 `client_session_id`、`guid/muid/sid`，以及 PaymentMethod 的 `payment_user_agent`、confirm 的 `rv_timestamp` 和两个 passive captcha 字段。普通 HTTP User-Agent 仍为本应用标识，没有冒充真实 Stripe.js/浏览器会话。这些差异没有被掩盖，也没有重放旧单的 SDK/验证码数据。

`internal/platform/testdata/legacy_hosted_capture_fields.json` 直接从成功 HAR 提取，只有字段名。新增离线测试执行真实初始化→税区→报价→最后核价→PaymentMethod→confirm 路径，严格比较请求字段及上述剩余差异，并覆盖配置变更、跨单响应、格式异常、原账单、拒绝不重试、Custom 隔离及诊断隐私。新测试在修改前失败，修改后通过；最终针对性离线回归 4.850s，Go vet 和 Windows 构建通过。涉及数据库的原集成测试本轮未配置测试库，不能声称本轮重跑通过。

10:40 已更新独立本地 http://127.0.0.1:18869/，应用 PID10236、原测试库 18868 PID3168；原实例此前已停止，此次启动前两轮空闲检查为 0，备份/TOC 通过，14 张业务表指纹一致，健康和管理员登录检查通过。构建与备份记录在 `.local/legacy-har-fix-20261031/`。正式环境与 GitHub 未更新，没有发起真实订单或付款。此前 HTTP400 的细化原因仍无原始证据，本轮不能宣称已解决实付失败或已完整等价于浏览器抓包。

最新订单 `1fc065720d17634e4d745a684b9ffa7a` 的付款前出口检查通过，创建 PaymentMethod 仍返回 HTTP 400 / `invalid_request_error`。实际发送版本已经是 `2020-08-27;custom_checkout_beta=v1`。因此，前一次版本修正没有解决真实失败；不能继续把版本当作已经确认的根因。submitted=false、fee_state=released，未发起扣款确认。

本次将当前 `confirmPage → cardForm → stripe → request` 原函数放入仅有内存 HTTP 替身的诊断中，使用虚构测试数据采集实际构造的字段名，与用户的《支付包.har》比较。没有连接业务服务或外网，也没有真实卡片、付款或抓包令牌重放。诊断代码及脱敏 JSON 在 `.local/legacy-har-audit-20261030/`。

## 创建付款方式：成功抓包 24 个字段，当前 14 个

全部 10 个缺失字段如下。这里的“缺失”表示与成功样本有差异，不表示已证明每一个都是 Stripe 当前必填参数。

| 缺失字段 | 来源与当前代码状况 |
| --- | --- |
| `client_attribution_metadata[checkout_session_id]` | 本单 Checkout ID 已有，但创建 PaymentMethod 时未传。 |
| `client_attribution_metadata[checkout_config_id]` | 原初始化响应可返回 `config_id`；当前只读取 `eid`，没有保留此字段。不能拿另一单的初始化响应补。 |
| `client_attribution_metadata[client_session_id]` | 官方 Stripe 客户端的会话标识，当前没有这层客户端上下文。 |
| `client_attribution_metadata[merchant_integration_source]` | 成功样本为 `checkout`，当前未发送。 |
| `client_attribution_metadata[merchant_integration_version]` | 成功样本为 `hosted_checkout`，当前未发送。 |
| `client_attribution_metadata[payment_method_selection_flow]` | 成功样本为 `automatic`，当前未发送。 |
| `payment_user_agent` | 标识真实 Stripe.js 集成及版本，与普通 HTTP `User-Agent` 不同；当前未生成。 |
| `guid` | Stripe 客户端上下文中的标识，当前未生成。 |
| `muid` | Stripe 客户端上下文中的标识，当前未生成。 |
| `sid` | Stripe 客户端上下文中的标识，当前未生成。 |

抓包中的应用层请求还带有 `Origin: https://pay.openai.com` 和 `Referer: https://pay.openai.com/`。当前服务端请求没有这两个浏览器来源头，且普通 User-Agent 为应用标识。当前额外设置了 `Stripe-Version` 请求头，抓包在这两次付款请求中仅用表单 `_stripe_version`。这属于差异证据，不能只靠补写浏览器头就宣称建立了真实 SDK 上下文。

HTTP/2 的伪头、Content-Length、压缩协商以及浏览器 sec-* 提示不计入“缺失 API 参数”：Go 传输层会处理部分头部，其他属于浏览器行为，不能把内存替身捕获的应用层头部误称为完整线上线路抓包。

## 后续确认：成功抓包 19 个字段，当前 6 个

缺少同一组 6 个 `client_attribution_metadata[...]`，以及 `guid`、`muid`、`sid`、`link_brand`、`rv_timestamp`、`passive_captcha_ekey`、`passive_captcha_token`，共 13 个。

成功样本的 `checkout_session_id` 与该样本确认目标一致；不能将两次不同订单的字段拼接。当前这笔失败尚未执行 confirm，所以不能把 confirm 阶段的验证码字段当作本次 PaymentMethod 400 的已证实原因。验证码结果和客户端环境应来自本单真实官方页面/SDK，不能从旧 HAR 重放，也不能合成通过结果。

## 能确认和不能确认的结论

- 旧纯 HTTP 实现未完整复现成功样本。此前“与抓包对齐”不能理解为完整等价，版本字段回归也不足以覆盖这项要求。
- 现有自动测试主要覆盖流程状态、资金防重、代理门槛和已知请求字段；内存替身接受请求不等于 Stripe 接受真实请求。
- 已有错误轨迹没有保留具体上游 message，且本次没有可用的细化 code/param/reason，因此无法事后确定究竟哪项条件导致 400。不能断言缺少上述某一个字段就是唯一原因，也不能声称补足字段数量就一定成功。
- 本轮完整列出差异并纠正结论，没有再进行推测性的运行时代码修改、重启、真实付款或生产更新。若继续实现，应先获得当前官方集成的有效上下文和明确的拒绝分类；仅重放历史抓包或伪造 SDK/验证信号不是有效修复。
