# 18:51 版本修正后仍返回 HTTP 400

订单`1fc065720d17634e4d745a684b9ffa7a`已使用修正后的版本，付款前IP检查通过，创建付款方式仍400/invalid_request_error，submitted=false、fee_state=released。此次通过原构造函数的离线请求捕获逐字段对照成功HAR：PaymentMethod少10个字段，confirm少13个，包含结账关联、SDK会话与后续动态验证上下文。完整清单和证据边界见[完整字段差异](legacy-payment-har-gap-20261030.md)。版本修正不是实际失败的充分解决方案，现有日志仍不足以确定唯一拒绝原因；本次没有继续盲改或宣称完整协议已等价。

# 18:29 本地旧旧付款方式创建 HTTP 400

订单 `a68b74fa60df5db55e8b69ebaa26951d`，18:28:12 创建，Pro5x / PH / PHP / Hosted / legacy＋legacy。提链、初始化、账单税费及读取报价均成功；18:29:06 付款前检查记录 `exit_ip_matched`。18:29:11 创建 PaymentMethod 返回 HTTP 400，`invalid_request_error`，请求编号 `req_RpYLKuNd2X5OSR`。submitted=false、fee_state=released，没有发 confirm。

成功的《支付包.har》在 PaymentMethod 及 Hosted confirm 表单中均发送 `_stripe_version=2020-08-27;custom_checkout_beta=v1`。原 Go 实现却统一使用 `2026-03-25.dahlia` 请求头，且 Hosted PaymentMethod 表单缺少版本字段。已将 Hosted 版本及这两次写请求的表单字段对齐参考；Custom 分支的原版本保留，init/tax/read 不增加抓包没有的表单版本字段。

离线回归先验证原实现不满足该请求契约，再验证修正通过；同时覆盖 PaymentMethod 400 后零 confirm、零重试、submitted=false，以及原 IP 门槛、短链和只读恢复。新增请求实际使用的固定版本诊断及 `invalid_api_version` 分类，仍不记录原始错误消息/响应或卡片资料。现存日志没有具体 code/param/reason，无法证明版本差异就是这次 HTTP 400 的唯一原因。成功抓包中的浏览器设备数据及动态验证字段不能重放或伪造；此次没有真实付款验证，不宣称整体办理已成功。实际本地更新时间和健康验证见 verification.md 与运维 runbook。

# 15:02 本地旧旧协议付款 HTTP 400 与后续 IP 门槛

本地订单 `2e29e2f36a5c931f233fc06610ad1ebd`：Pro5x / PH / PHP / Hosted，旧提链＋旧协议付款。提链、原 Payment Page 初始化、账单税区更新和含税报价均 HTTP 200；报价579464分。15:02:11 创建 PaymentMethod 返回 HTTP 400，关联编号 `req_CfbuVWSOSDx82v`，尚未发送实际付款确认。订单/付款 failed，submitted=false，服务费 released，无实收金额。

旧诊断仅保存 request_rejected 和 response_kind=json，没有原始 error.type/param/message，无法事后确定具体参数或权限原因。不能推测为银行拒付、验证码、代理漂移或某一缺失字段。本次补充脱敏白名单诊断，并把创建付款方式从“填写账单/提交付款”中单独展示。历史错误不会补造。

用户随后要求恢复付款前出口一致性检查。新旧提链保存原基准，四组合的卡片资料提交及实际确认/批准前使用保存的原代理检查；缺基准、漂移、探测失败均停止，新流程验证窗口开始前也检查。保留普通读取/后续处理的原代理及租约规则。此次没有重放真实付款或重新使用卡片/session，也不能把离线回归称为真实付款成功。当前本地验证和重启结果见 docs/verification.md 与工作区运维 runbook；以下为此前时点记录。

# 2026-10-30 原版对照、订阅诊断与代理约束调整

本轮正式版本仍为 `c2c166bef116587b4fe35a3e7f90633761a5b421`。下面代码修改均为本地工作区内容，未提交、推送或正式发布；本地隔离18869已更新。12:18用户新开的旧旧Pro5x已确认付款成功，订阅生效查询及关闭续费尚未确认；下方较早“未成功”描述只指相应历史订单。

## 后续修改：旧付款后先刷新并保存授权

用户在 12:21 诊断后要求修改旧执行器：付款成功后，每轮生效核实先调用官方 `/api/auth/session?refresh=true&reason=token_expired`，只携带 Session Cookie，不带旧 Bearer AT；校验账号、邮箱及令牌有效性并加密保存后，再用新 AT 查询订阅生效和套餐。刷新失败、身份不符或保存租约失效时停止本轮，不回退旧 AT；已经刷新的 AT 若仍被 401 拒绝，不循环刷新。历史 AT-only 订单保留只读兼容及无法刷新诊断。取消续费阶段原有主动刷新、原代理端点与会话、单次取消标记和账本规则保留。

两套使用同一个官方刷新接口，但实现并非共用函数。新 cs-live 的生效查询/刷新顺序本轮未改；浏览器路径修复继续保留。付款并不代表旧 AT 必定失效；此前订阅查询是 403 安全挑战，不能将本次时序调整称为已解决真实 403。未重开旧成功订单或已关闭的新新订单。

隔离 PostgreSQL 18870 针对性回归 74.658s 通过，覆盖生效前刷新并持久化、取消前再次刷新、保留续费、AT 过期、401/403、身份不符、保存租约失效、固定原代理及单次取消。`go vet ./internal/platform`、构建和 diff 检查通过；新浏览器路径沿用此前 12 项离线验证，本轮没有重复执行真实付款或挑战。

12:33:55 两轮空闲检查、18868 数据库备份及 TOC 验证后，仅将本地18869更新为 `.local/subscription-local-20261030/xufu-refresh-first.exe`，PID35500，SHA256 `3C96D57317241A127459A389EB2FEC665EF0C7F2B8E6B90793A169D0A1965232`。live/ready/首页及管理员设置、原两笔订单详情均通过；12 张业务表（订单、付款、协议检查点、订阅、设置、账单、账本、关闭记录、任务、资源、套餐、结账链接）全行指纹一致。原配置和已结束订单保持，没有重新付款或重开任务。PG18868原进程31872保留；测试18870已停止，18860/18862无监听。安全结果、备份和指纹在同一本地目录，一次性更新脚本收尾删除。无提交、推送或正式更新。

## 12:13 新新验证浏览器启动故障；12:18 旧旧付款成功后查询受阻

截图对应`58ccd77a32ef9797cb24bf9deacd1766`，实际产品Pro20x、新新Hosted，12:13:53上游PI返回`requires_action/use_stripe_sdk/intent_confirmation_challenge`。启动验证窗口后1127ms返回`action_runtime_unavailable`，仅有browser_start，无page_loading。该单12:16:40被管理员关闭，非本次诊断操作；保留原已提交事实，不重放。

本地已精确复现：服务配置PAYMENT_CHROME_PATH，但payment-action.mjs原只读取PLAYWRIGHT_CHROMIUM_EXECUTABLE，导致查找缺失的Playwright chromium_headless_shell-1248可执行文件并退出。已让验证运行时兼容PAYMENT_CHROME_PATH及Windows已安装Chrome路径，同时保留原显式变量优先级。使用与服务一致的PAYMENT_CHROME_PATH、清空另一个变量，12项真实Chrome离线SDK回归全部通过（25.604s）；不执行真实验证解算。脚本每次启动时读取，当前PID37588无需重启即使用修复，健康ready，diff检查通过。

用户12:18:00另开`4bd44fe26bbc8b7d79a1eb4de6b384ef`，实际旧旧Pro5x/Hosted。12:18:49付款提交，12:18:57记录原PI succeeded，12:18:58付款succeeded、服务费charged，有付款证据；报价PHP5794.64，但charged_amount为空，不将报价称为已核实实收。该单没有经过新流程验证窗口，不能把成功归功于上述启动修复。

旧旧后续按原实现：用原AT查询订阅生效，401才刷新保存后重查一次；确认生效及套餐后，如果不保留续费，则主动刷新授权、再查续费状态、至多一次提交取消、只读复验。整单仍用保存代理，无出口一致性探测。本单连续12次订阅GET403 HTML，明确cf-mitigated/challenge_required；403不触发401刷新分支。每次返回后等待配置5秒，加请求耗时约每10秒一次。12:21:00达到12次上限，停止自动核实：付款及订单succeeded，activation=review、renewal=review、cancel_started=false、无在途worker，终态授权按原规则清空。说明付款成功但生效读取受安全验证阻断，取消续费尚未发出；不是仍在付款或无限循环。需在官方页面确认实际套餐及续费，不能声称已关闭续费。脱敏结果action-runtime-diagnosis.json、pro5x-post-payment-status.json保存在本次.local目录。未修改这一后付款业务顺序，未提交推送或正式发布。

## 11:49 用户新新订单待核实；12:08 本地诊断更新

用户自行在本地界面创建订单`8eb74ef65fc4eb5e25c64cb40a03bf87`，11:49:01受理、11:51:06转review。经数据库与管理员API核对，本单实际为cslive提链＋cslive付款、Pro5x/PH/PHP/Hosted，报价579464分（PHP5794.64）。不同于11:40只读前置诊断，此单已进入真实付款，`submitted=true`，有可恢复检查点，`payment_result/pending`，`fee_state=held`，没有实收金额或成功付款凭证，订阅与续费处理均pending。不能重放或再开付款单。

记录显示11:50:41确认请求返回200，随后三次状态查询均200；11:50:50与11:51:06付款诊断均`open/unpaid/pending`，没有PaymentIntent或submission_attempt的状态记录。截图中多条“提交付款请求/请求已返回”包含后续查询，不能理解为多次成功扣款。`approval_started=false`是共享表字段，本身不能证明新协议从未运行批准步骤；不能据此归因“没有批准”。11:50:21有一条checkout分类403，但后续仍进入付款，原日志未保留具体请求类别，不能直接把它当本单终止原因。

11:57仅通过原订单仍有效的保存代理端点/session进行GET复查，没有执行恢复、验证解算、确认或批准：原Checkout对应Stripe查询返回200、`status=open/state=active/payment_status=unpaid`，`payment_intent/setup_intent/submission_attempt/error/last_payment_error`均null；账号GET200、身份匹配、`has_active_subscription=false`。没有银行拒付/验证失败等明确原因，不能仅据这些状态保证银行无预授权。检查点代理是本地认证桥接URL，和供应商端点文本不同属于现有设计，不能当出口变化证据。Stripe页面响应没有直接匹配的checkout ID字段，因此查询依据是保存的原Checkout路径，不能将`matches_checkout=false`当返回了其他订单。脱敏结果在`.local/subscription-local-20261030/new-new-order-readonly-1154.json`，文件时间为11:57。

确认诊断存在缺陷并作本地修复：

- Hosted确认响应在三次Page轮询后被覆盖，原代码此时才记录状态。现在在轮询前额外记录首次确认响应的白名单诊断，不改变付款状态、错误分类或批准/验证逻辑。历史原响应未保存，无法补回；不能声称已经知道它包含何种错误或挑战。
- `/payment_pages/{id}`查询被套用当前hosted_confirm阶段，错误显示成提交付款。现在映射为付款核实，confirm与init仍分别是提交与报价；历史已落库的请求阶段不能可靠重建，不重写。
- `pending`查询曾显示“完成”，收尾又显示“失败”。新增记录与历史管理员展示都改为“待核实”，HTTP403等实际请求失败仍保持失败。精确保留`active`状态，未知自由文本仍脱敏。这里只改善诊断和显示，不代表付款故障已解决。

验证：cslive全套离线Go测试2.011s；针对性PostgreSQL诊断/客户隐私/结算回归11.450s；补充HTTP错误不可被pending覆盖回归2.784s；go vet、构建、diff检查通过。新增离线用例验证首次确认信息在后续无详情页面中仍可观察、只确认一次、付款仍pending、无敏感信息、诊断不改变资金状态。隔离测试PG18870结束已停止。

12:08:42完成本地空闲检查、18868备份/TOC验证及二次空闲检查后，仅更新18869为`.local/subscription-local-20261030/xufu-payment-diagnostics.exe`，PID37588。原单order/payment/protocol检查点和全部settings指纹保持，未重放原单；ready及管理员详情通过，原pending行现显示review。数据库仍18868，旧18860/18862未启动。正式环境未操作，无提交推送。本单仍未成功，首次确认响应缺失使上游未完成付款的具体原因尚不能确定。

## 11:40 新流程前置请求实测（未进入提链提交和付款）

用户要求“用新新的流程走看看”。本次直接运行新流程 `CreateCheckout` 共享的准备代码，参数为 Pro5x/PH/PHP/hosted，复用11:27本地订单保存且仍有效的原代理端点与供应商session；没有再次查询或比较出口IP。Session从用户附件读取，经匿名stdin进入临时诊断进程；未传入银行卡或CVV。诊断只允许官方首页、Session刷新、账号和订阅的GET请求，并在本地Sentinel/Turnstile解算器入口以明确的停止桩替换实际实现；禁止Checkout及付款写请求。

11:40:07开始，实际请求结果依次为：首页200 HTML（9559ms）、Session刷新200 JSON（2260ms）、账号检查200 JSON（2143ms）、订阅查询404 JSON（1860ms）。四次均无`cf-mitigated: challenge`响应头。订阅404被原代码识别为`subscription_not_found`，正常继续至验证令牌入口，说明本轮没有在该404处失败；不能将“没有挑战响应头”扩大为所有验证已通过。

最终`read_requests=4`、`verification_boundary_reached=true`、`solver_executed=false`、`checkout_created=false`、`payment_requests=0`。`manual_verification_required`是本次诊断桩主动返回的停止码，**不是上游返回的错误或人工挑战结论**。这次没有复现此前Session刷新403，但没有发送Checkout POST或进入新付款，不能声称完整“新新”已测试、提链失败、付款失败或订阅成功。后续需通过官方页面正常完成所需验证，不执行自动解算或伪造令牌。

本地复核新增订单0、排队/运行任务0、测试账号余额/冻结0，后台仍为原`cslive`提链＋`legacy`付款、Pro5x hosted，未改成持久的新新配置。因未进入付款，本轮没有抽取/提交账单；此前旧旧订单使用后台账单的验证结论保持。18869 ready，18868在运行，18860/18862无监听。脱敏结果保存在`.local/subscription-local-20261030/new-preflight-result.jsonl`及`new-preflight-local-state.json`，一次性诊断源码和可执行程序在收尾移除，不重放。未提交、推送或操作正式环境。

## 10:46 起本地旧旧长链实测及页面资源问题

用户授权本地办理后，使用已隔离的 PostgreSQL 18868 和应用18869；未启动已迁移18860/18862。启动及更新前均核对空闲并备份数据库。10:44、10:51、11:18、11:20账号只读查询HTTP200，身份匹配、`has_active_subscription=false`。敏感输入仅经匿名stdin进入运行时，未记录Session、卡号或CVV。

10:46:28正常API受理旧提链＋旧付款、Pro5x/PH/PHP/Hosted订单 `90a062c4e0c7fc28fa9d63d62ca20978`；10:46:50真正Checkout POST200，保存真实长链1条。浏览器填卡和后台账单，最终报价PHP6490.00；点击官方订阅按钮后未观察到付款confirm，10:49:20以`verification_required`结束，`submitted=false`、`approval_started=false`、服务费released。临时本地测试额度已反向撤回，余额/冻结均0。后台当时有10条启用非测试账单，本单姓名、街道、城市、州和邮编逐字段匹配其中一条。

在同一Checkout、原代理端点/供应商session、原账单上做受控页面诊断，所有付款confirm/pay/capture/approve请求均在浏览器边界阻断，没有解算或操作验证码。确认：

- 卡片与账单字段已填入，无字段校验错误；唯一剩余复选框是可选“保存支付信息”，不是服务条款。
- 点击后出现隐藏hCaptcha框架及内部“Please try again”，其checksiteconfig/getcaptcha曾返回200；没有向用户显示可操作题目。仅凭框架加载不能确认是必须人工验证码，也不能将初始化200当通过。
- 原浏览器和Go CONNECT白名单阻断了官方页面实际加载的四个确切资源主机：`stripe-camo.global.ssl.fastly.net`、`d1wqzb5bdbcre6.cloudfront.net`、`maps.googleapis.com`、`applepay.cdn-apple.com`。
- 诊断放行上述资源且保留全部付款阻断后，同一Checkout实际发出Stripe付款confirm，已被诊断边界拦截；没有扣款提交。此对照说明页面可以继续至confirm，不能证明四个主机各自都必需或白名单是所有历史失败的唯一原因。复现还观察到`ERR_NETWORK_CHANGED`，报价曾由649000分变为579464分，未批准扣款。

本地修复同步将上述四个确切主机加入JS和Go白名单，保留443端口、代理认证、原单/金额绑定及单次付款门槛，不放行Fastly、CloudFront、Google或Apple整个域名后缀。46项浏览器/短链/Hosted批准离线测试全部通过，含依赖资源就绪后仅确认一次的回归；Go实际CONNECT/Chromium测试6.553s及`go vet ./internal/platform`通过。11:18完成本地应用空闲检查/备份/更新；未更新正式环境。

修复后订单 `22d44cecf03e8e0d45b5f1f9fe673f8f` 在创建Checkout时网络失败，链接0、未提交付款、服务费释放。后续实测结果另行记录，不能把该网络失败说成修复后付款仍失败。

11:21订单 `c3a240c5509b28a15591bbb41419bb59` 提链POST200、链接1，但浏览器在填卡前报`checkout_fields_unavailable`，没有付款提交，费用已释放。后台准备期限为120秒，浏览器字段等待却写死15秒；11:21:47页面init成功，11:21:49即因字段未就绪结束。同一原单受控复查字段可正常出现，说明不能据此认定不支持该卡。已将必填字段等待改为共享本次配置的准备截止时间，并检查代理有效期/取消。增加超过15秒才出现字段的真实Chromium离线用例。后续是否成功仍以真实结果为准。

47项最终浏览器回归全部通过后，11:27正常API受理订单 `54fd0f78ee4324814fba68ed11829580`。提链、字段、账单及报价均通过，报价PHP6490.00；点击后仍未观察到实际confirm，约11:30以`verification_required`失败，最终`submitted=false`、`approval_started=false`、服务费released。这说明上述修复尚未解决全部阻塞；`verification_required`也是运行时“点击后无确认请求”的通用退出码，不足以证明一定是验证码本身拒绝，也没有银行拒付证据。hCaptcha隐藏错误与网络波动是已观察到的线索，剩余唯一根因未确认。未继续尝试自动解算验证或切换指纹绕过。

11:30:47经最后一单原代理只读复查，账号HTTP200、身份匹配、无有效订阅。四笔本地订阅均未提交付款，服务费全部释放、测试额度全部撤回、余额/冻结0、在途任务0；订阅与提链授权字段已按现有终态逻辑清空。配置已恢复新提链＋旧付款、Pro5x Hosted。本地18869健康ready，PG18868和当前程序保持用于用户查看；未启动18860/18862，未操作正式服务器。一次性真实下单及填卡诊断脚本已移除，保留脱敏结果，禁止重放历史订单。

新新流程代码会调用本地Sentinel/Turnstile解算器；本轮未执行该自动验证路径，不能声称两套均已实测。先前正式09:36失败与本次本地10:46提链成功是不同订单及环境，应分别描述。

## 实际办理证据

09:29 创建独立旧提链订单 `804619ea3dcdc66dcf54a943904bc385`；09:36:38 按正常订阅 API 创建完整订阅订单 `3dbb21f08b10cd901e761b64d958a92a`。两单均为 Pro 5x、PH/PHP、Hosted，实际选择旧提链；完整订阅选择旧付款及后续，`keep_renewal=false`。受理后恢复正式原配置“新提链＋旧付款”。

完整订阅在 09:36:42 出口查询返回200，09:36:50 真正创建 Checkout 的 POST 返回403 HTML / `access_denied`，09:36:51 失败。结束时 `attempts=1`、`side_effect_started=true`、`submitted=false`、`approval_started=false`、`fee_state=released`，链接数0，服务费解冻记录1、结算记录0，无活动任务。本站已接受卡片并冻结后释放服务费，但未向上游提交银行卡付款，也没有银行拒付证据。

独立提链同样在真实 Checkout POST 返回403；其链接、付款和账本记录均为0。前面的 IP 查询200不代表生成链接成功。

附带只读检查：有效格式的 AT 能取得账号 GET200、身份匹配、无有效订阅；登录态与订阅查询 GET403 HTML，明确含 `cf-mitigated: challenge`；私有 Chromium 首页403，代理出口与 curl 一致，长 Cookie 已正确分段。不同 Session 字段的差异不能直接证明令牌无效。实际 Checkout POST 的旧日志没有保留该响应头，因此不能把其他 GET 的挑战标记当成该 POST 的确切原因。

当前可确定阻塞在创建 Checkout 的上游请求；尚未证明是请求参数、会话、代理环境或上游访问策略中的哪一项。未实测全部新旧组合，不能称“所有组合均失败”。

## 用户指定原文件

- 新流程：`D:/wevxin/xwechat_files/wxid_7angrfg49z7s22_7bd2/msg/file/2026-10/cs-live-source.zip`，SHA-256 `55aba3b8dd30114574f482f4c762b88d86958cbd965d1e3716369d47049e3687`。
- 旧提链：同目录 `generator.py`，SHA-256 `31a83d5de82ff0de304f24e3136c96662ea220a956ea9965162f14679c2f5911`。
- 旧付款及后续：用户补充指定本地 xufu 备份，已找到 `.local/backups/before-subscription-rewrite-20261028-203008/source-and-config.zip`，基线提交 `7e1dfd40ec01463a0265985f1b54c90daaa18487`；实际 SHA-256 `61df447baa6ccf43623d47f6d268694bde6655d231b5c5b12df87f16f8904e60` 与备份清单一致。仅提取所需代码对照，没有恢复其配置或数据库。
- `runtime/generator.py` 与用户文件的全部函数 AST 一致；文件内容差异仅默认代理配置已清空。实际服务调用 Go 适配层，因此另核对其请求参数、头、代理传输和链接解析。

旧 Hosted 请求为 `plan_name=chatgptprolite`、`billing_details.country=PH`、`currency=PHP`、`checkout_ui_mode=hosted`，无 `entry_point`；iOS 请求头、Bearer 和账号字段与原脚本一致。短链按原 Web/custom 分支。没有发现本次 Pro5x/PHP 映射错误。

新包逐声明核对：453个函数、56个常量、43个变量、70个类型一致；6个函数、4个类型不同。命令行输入输出、本地恢复文件存储和验证页服务器由网站接入层承接，不能称整包逐字一致。差异如下：

| 函数 | 现有适配与原因 |
| --- | --- |
| `PaymentClient.Pay` | 接收已创建的结账单，支持旧提链＋新付款，不重复创建 Checkout |
| `prepareCheckout` | 按后台显式长短链执行，不按上游模式改选；保留实际长链给管理员；显式选型不走直接升级 |
| `createCheckout` | 将提链时已刷新的授权交给下一段旧付款，保持同一账号会话 |
| `executeFlow` | 混合组合交接、长短链固定执行分支、真实 Hosted 链接留存 |
| `Transport.Request` | 网站任务/代理有效性检查；本轮取消其中附加的出口 IP 探测 |
| `Transport.observe` | 将原包识别的“无订阅”404分类显示，不改变原业务返回 |

新包的 Hosted/Custom 付款、生效核实、刷新和续费业务函数在上述差异之外保持原实现。官方验证HTML/JS与原包常量内容一致（忽略结尾换行）。网站自动接续是现有网站适配，浏览器回调不等于实际扣款结果。

`generator.py` 仅用于旧提链对照；旧浏览器付款和后续按上述备份核对。用户已更正“邀请流程”，本轮不涉及邀请。

## 旧浏览器备份核对与一致性边界

旧结账＋旧付款仍是 generator 提链后，由私有 Chromium 打开原官方结账页、填写卡片及本单账单、等待最终税费与报价、确认金额后单次点击付款、根据原结账单的权威响应判断结果，再执行旧版生效核实及续费处理。没有替换为新包协议付款。

机械核对确认浏览器 `snapshot`（权威结果解析）及 `confirmMatches`（原单确认请求匹配）与备份源码一致；Go AST 核对确认 `savePaymentQuote`、`confirmPayment`、`paymentFunds`、`runRenewal` 等原核心函数一致。`subscription_status.go`、`renewal_recovery.go`、`renewal_standalone.go`、`renewal_proxy.go` 在统一换行后文件一致。付款结果收尾增加协议执行器隔离及 Hosted 批准接续；旧付款的金额/币种、原交易证据和资金防重要求保留。

当前旧浏览器与备份的实际差异包括：

- 支持后台选择的 Custom 短链，增加必要的 Cookie 登录和页面字段适配；备份浏览器只支持 Hosted 长链。
- Hosted 遇到原交易 `requires_approval` 时，保留 Cookie 交接、官方批准回跳、单次批准标记及同单复查；备份没有这一段。
- 保留长 Cookie 分段、官方资源域名修复、多个页面响应顺序处理和脱敏诊断。
- 按用户要求固定原代理端点及供应商 session，取消订阅出口 IP 校验，并取消备份中付款前自动换代理重建的行为；代理到期、缺失或租约失效仍停止。
- 保留新旧两段交接及执行器隔离，使四种组合都能使用同一 Checkout、原账单和原代理；选择新提链＋旧付款时接收提链后的刷新授权。

新结账＋新付款沿用 cs-live 业务函数，但仍受本报告列明的网站适配影响，包括后台固定长短链时不执行直接升级。结论是两套各自核心流程已核对，存在明确保留的适配与修复，不能称“除了账单/IP/长短链外全部逐字相同”，也不能据此保证真实订阅成功。

审计明细保存在 `.local/source-audit-20261030/legacy-parity.json` 和相应源码差异中。本次追加核对未发起真实提链或付款；下述已完成的测试无需因文档更新重复执行。

## 本地修改

1. 取消新旧提链及订阅全链条中的重复出口查询、一致性比较和漂移中断。固定原代理端点与供应商 session；会话到期、资源停用、执行或容量租约失效仍停止，不直连、不换代理。旧脚本初始地区查询保留，初次 IP 只作观察记录；新包不额外查询 IP。历史非订阅付款兼容逻辑和独立代理检测保持原行为。
2. 订阅进入旧浏览器、Hosted 批准、官方 SDK、恢复与付款后处理时不要求保存的 IP 字段非空。改由原代理绑定和原任务有效性决定是否可继续；原单身份、金额、提交/批准防重、账单与账本规则保留。
3. 修复旧 curl 传输丢弃上游响应头的诊断缺陷，只保留内容类型、请求编号与挑战标记；不记录 Cookie 或原始正文。修复出口200误显示为结账完成，管理员可区分出口查询与真正创建 Checkout。

移除出口检查避免这些附加请求的失败或延迟中断办理，但不等于解决此次真实 Checkout403。程序只能固定代理会话，无法保证供应商在会话内绝不换实际出口。

## 验证

针对性 PostgreSQL 回归覆盖同单代理/账单不变、既有选型、混合交接、过期及失效租约阻断、取消出口探测和资金幂等。真实 curl 的本机响应测试覆盖临时响应头、chunked 正文、403挑战标记、响应头白名单、截断与状态不匹配。真实 Chromium 离线用例覆盖订阅零出口探测、原单一次提交、到期停止、短链、Hosted批准和原 SDK 接续。

测试使用独立本地 PostgreSQL 18870 与模拟上游；没有启动已迁移的旧业务队列，也没有在测试中提交真实付款。完整Go测试通过（platform 349.277s），最终提链租约/混合交接/代理观察回归33.536s、go vet及Vue/TS/Vite构建通过。57项真实Chromium离线测试通过；首次SDK组因本机未安装Playwright内置浏览器失败，指定已安装Chrome后该组12项全部通过，其他45项均已通过。单元/模拟成功不能代替真实订阅成功。

本轮收尾：独立测试PG18870已停止；18860/18862/18866/18870未发现监听。真实试单辅助脚本已删除，避免误重放；本轮没有提交Git或部署。
