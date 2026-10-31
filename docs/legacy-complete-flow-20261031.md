# 旧提链＋HTTP 付款：完整 HAR 核对

## 证据范围

用户本次文件 `完整支付包.har`，48,400,574 字节，SHA256 `19f981af03c760ffa545121591ab70fe9a2a5121b93c19873797c56a5a8fa11c`。共464条请求，URL中只有一个Checkout。它从打开长链开始，不包含创建Checkout，也不包含本站Session刷新、套餐核实或取消续费。提链继续使用已核对的generator旧实现，后处理继续旧订阅worker。

只读解析原文件，未重放真实请求。提交的 `internal/platform/testdata/legacy_complete_capture_fields.json` 仅保留字段名、脱敏路由及状态；没有Cookie、卡片、地址、真实交易ID、密钥或验证令牌。完整本机脱敏索引在 `.local/legacy-complete-20261031/har-shape.json`。

| HAR索引（从0开始） | 请求 | 证据 |
|---|---|---|
| 7 | POST payment_pages/{原单}/init | 完整checkout.session，tax requires_location_inputs |
| 123 | POST payment_pages/{原单} | 仅国家变化；config_id随税区更新 |
| 284 | POST payment_pages/{原单} | 完整账单地址；税区complete，最新config_id |
| 308 | POST payment_methods | HTTP200，付款方式创建成功 |
| 312 | POST payment_pages/{原单}/confirm | HTTP200但unpaid；原PI requires_action/use_stripe_sdk/intent_confirmation_challenge |
| 346 | POST payment_intents/{原PI}/verify_challenge | HTTP200；HAR未保存响应正文，不能声称已读到验证响应内容 |
| 348 | GET payment_pages/{原单}/poll | 同一page ID和session ID；state及payment_object_status均为succeeded |
| 353～384 | GET completed_webhook_delivered | Webhook送达轮询，不是扣款证据 |
| 389 | GET商户成功页 | 跳转展示，不单独作为付款证据 |

此成功样本经过真实官方验证，不是仅发送confirm即可成功的样本。

## 三套整单流程

同一个 `payment.subscription_flow` 下提供 `legacy`（旧提链＋浏览器付款）、`legacy_protocol`（旧提链＋Go HTTP付款）、`cslive`（原新整套流程）。第三套的checkout_runs.flow固定为legacy，payment_runs.executor固定为legacy_http；旧浏览器默认browser，新流程仍以subscription_protocol记录驱动。

受理事务保存执行器；修改配置、幂等回放、重启和只读核实不改原单。未重新引入PreparedCheckout或新旧交接参数。独立结账链接工具在选择第三套时仅执行旧提链，不产生付款副作用。原长短链选型、正式账单随机快照、整单代理和付款前出口检查保留。HTTP付款及恢复不要求安装浏览器；旧浏览器及新SDK各自保留依赖。

旧协议Hosted消费init/税区POST的完整响应，核对原session及page身份后更新EID/config_id。完整响应不再额外依赖先前的page GET；旧部分响应保留兼容读取。准备报价、冻结确认授权后，再以同一完整账单复核税价，变价停止。随后创建一次PaymentMethod、一次confirm，处理中轮询原交易；成功才交回现有套餐核实/按需刷新/续费worker。核实失败或网络响应丢失不重新确认。

税区表单使用同一账单的country/state/city/line1/postal_code。抓包中仅国家的中间请求来自表单编辑过程；HTTP直接提交最终完整地址，且提交前再次复核税价，并不逐次模拟用户键盘输入。

原page ID及原PI最小恢复资料加密保存。poll须同时匹配原page及Checkout，且两个成功状态均成立，金额/币种沿用已持久授权报价。poll没有实收金额，不伪造amount_received；需要完整实收凭证时仍保留“未返回”的含义。验证响应200、跳转成功页及Webhook已送达均不单独标成功。

## 参数与环境边界

| 请求 | 已实现普通字段 | 动态SDK字段 |
|---|---|---|
| init | browser_locale、browser_timezone、eid、redirect_type、key，5/5 | 不适用 |
| 完整税区更新 | eid、key及5个tax_region地址字段，7/7 | 不适用 |
| PaymentMethod | 卡片、账单、key、type、版本、5项Checkout attribution，共19/24 | client_session_id、guid、muid、sid、payment_user_agent |
| confirm | 原payment_method、expected_amount、类型、eid、key、版本、link_brand、5项attribution，共12/18 | client_session_id、guid、muid、sid、rv_timestamp、passive_captcha_token |
| poll | key及原Checkout路由 | 无验证令牌 |

Hosted版本沿用抓包表单 `2020-08-27;custom_checkout_beta=v1`，Origin/Referer来自原长链；最新Checkout attribution来自本单响应。浏览器/Elements会话、设备遥测、被动验证令牌与verify_challenge的challenge_response_token由真实官方SDK生成。HTTP执行器没有这些SDK数据源，不使用随机值或历史HAR值冒充。因此不能宣称HTTP与浏览器参数完全相同、可保证通过验证或真实付款已经成功。

第三套遇requires_action/认证挑战暂停保存原交易；官方完成验证后，仅GET核实原单。没有后台浏览器兜底、验证码解算或验证码重放。旧浏览器流程继续由真正Chromium执行官方页面及SDK，自然产生其环境信息，没有把用户抓包的浏览器指纹复制成模板。

## 旧浏览器修正

原实现一见requires_action即关闭页面，会截断SDK后续操作。本次保持原页面到既有结果超时，允许官方SDK完成原交易验证，超时仍停为待验证；不自动解题。verify_challenge必须绑定已提交原PI，并再次检查原出口；只读核实禁止此写请求。

原snapshot只识别完整checkout.session，漏掉本次最终poll的精简响应。本次单独识别严格绑定的poll成功、保留原报价且不伪造实收金额；只读浏览器核实等待原页结果。补充捕获中用于地址组件的places.googleapis.com准确主机白名单，未放开任意googleapis子域。

## 验证与发布

验证使用虚构资料和本机模拟服务。新增完整HTTP成功、处理中poll、挑战暂停、官方完成后的只读恢复、异单poll拒绝、非成功poll拒绝、confirm响应丢失后核实及两次付款前IP检查。管理端/Web/API覆盖第三套受理、幂等、冻结、账单、配置切换及重启后的执行器保持，套餐长短链与独立提链隔离。

真实Chromium离线用例模拟requires_action → 同单verify_challenge → poll成功；覆盖超时及异单poll不报成功。模拟请求没有送往Stripe或商户。最终测试和本地更新记录见同日docs/verification.md及工作区runbook。

仅更新独立本地18869/18868。未推送GitHub、未更新正式环境、未创建真实付款或改变当前选中的整套流程。
