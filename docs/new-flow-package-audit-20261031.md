# 2026-10-31 新提链及付款流程与原包完整对照

结论：当前新流程 cslive 的核心提链请求、Hosted/Custom 付款表单、付款状态判断、生效核实及取消续费代码沿用本次指定的原包；整套应用并非与原包完全相同。明确的行为差异包括按后台套餐配置强制长短链、禁用显式选型时的直接升级、旧新交接、付款前 IP/租约检查、自动 SDK 接续、数据库恢复和金额/费用防重。不能把“核心实现一致”表述成“运行环境、请求前置条件及完整程序逐字一致”。

本次只读审查业务源码，没有改付款实现、设置或数据库，没有创建订单、执行验证或发起真实付款；新增文件为审计工具、脱敏索引和本报告。结论针对当前本地工作区源码，不代表正式环境镜像已经包含全部本地修改，也不代表原包能保证上游付款成功。

## 本次参考及比较方法

- 用户指定：`C:/Users/Administrator/Desktop/中转站/支付/cs-live-source.zip`。
- ZIP SHA-256：`55aba3b8dd30114574f482f4c762b88d86958cbd965d1e3716369d47049e3687`。
- 该包与先前微信目录里的同名包 SHA-256 完全相同，不是另一个版本。
- 包内只有 `cs_live.go`、`go.mod`、`go.sum`；主源码 457925 字节、13345 行，SHA-256 为 `b71ce2d656153910151b7b2b7db80f9866e21c5dc70f1e5f794f75aa2139ecc0`。
- 用 Go parser 分别解析原包与 `internal/cslive` 全部非测试 Go 文件，按声明名称/接收者匹配，比较格式化后的词法序列。忽略注释与排版，保留语句分隔和字符串内容。额外人工核对项目调用层、参数来源、权限/代理门槛、付款结果持久化、恢复与 SDK 启动器。没有运行原包业务入口。
- 比较工具和逐项索引在 `.local/source-audit-20261031/compare.go`、`parity.json`、`common-index.json`；每个不一致声明另存原包/项目文本。SDK/依赖证据在 `assets-and-dependencies.json`。

| 声明类型 | 共有且相同 | 共有但不同 | 原包未直接导入 cslive | 项目新增 |
| --- | ---: | ---: | ---: | ---: |
| 函数 | 450 | 9 | 63 | 12 |
| 类型 | 70 | 4 | 8 | 13 |
| 常量 | 56 | 0 | 2 | 0 |
| 变量 | 43 | 0 | 4 | 0 |

原包共709项声明，当前 cslive 共657项。77项未直接导入的声明属于 main/CLI、终端输入/输出、本地文件检查点和本机 SDK HTTP 服务。其中两个 SDK 页面/脚本常量已迁移到 runtime 文件，并非遗漏。项目25项新增声明包括导出别名/适配接口、原结账交接和诊断辅助。这里的数量不把项目外层 platform/runtime 代码算成原包同名声明。

## 按实际执行阶段核查

| 阶段 | 原包与项目的对应关系 | 影响 |
| --- | --- | --- |
| Session 刷新、账号和订阅检查 | `chatgpt_Platform.Refresh`、账号请求及订阅查询等共有实现一致。正常“新提链＋新付款”继续原包的准备顺序。 | 旧提链交给新付款时是另一条新增分支，不能视为从原包准备入口完整重跑。 |
| 创建结账请求 | `chatgpt_Platform.Checkout` 的请求构造相同，包括套餐/席位/计费周期等；原固定团队名称保持。 | 参数来源改为本站订单和后台配置。 |
| 长短链选择 | 原包可根据上游返回的 `checkout_ui_mode` 决定实际分支；本站对显式 hosted/oaics 使用订单已保存的套餐配置。 | 即使上游返回另一种 mode，也不会自动改走另一个付款分支；这不是与原包完全相同的行为。 |
| 已有订阅升级 | 原包保留可直接更新订阅的分支；本站显式长短链选型时把该分支按已有订阅冲突处理。 | 不绕过用户选定的结账方式去做直接升级；历史 auto 路径保留原逻辑。 |
| Hosted 初始化与税额 | `HostedPrepare`、脚本读取、税区请求、金额提取及配置编号选择实现相同。 | 本次没有把旧协议刚改的“最终读取优先配置编号”逻辑移植到新流程；新流程仍保留原包 tax→init→page 的选择顺序。 |
| Hosted 创建付款方式/确认 | `HostedForm` 全部相同；`runHostedFlow` 只增加确认响应的诊断采集。 | 没有因本次旧协议修正而改写新流程付款字段、确认请求或轮询/批准逻辑。 |
| Custom 短链付款 | `Token`、`ConfirmForm`、`runCustomFlow`、`runCustomStripeCheckout`、商户确认、原 PI 接续等共有函数相同。 | 短链没有被替换成 Hosted HAR 的请求组合。 |
| 付款状态和原单恢复 | 状态分类、仅授权/待验证/拒付判断、`Recover` 与 `recoverPayment` 相同。 | 项目外层另有数据库租约、到期限制及重启后的状态收尾，不重新 Pay 原交易。 |
| SDK 验证内容 | 原包 HTML 和 JS 主体与 runtime 对应文件一致（只忽略文件末尾换行）。 | 启动方式与页面执行环境不同，详见下一节。 |
| 生效和套餐核实 | `confirmActivation` 完全相同：原回跳后有限查询订阅，遇401才刷新并保存后再查。 | 新流程没有改成旧流程现在采用的“每轮先刷新，再查询”。 |
| 取消自动续费 | `disableRenewal` 完全相同：进入取消阶段先刷新，再查询、按原包有限重试并复查。 | 是否取消由本单“保留续费”选项决定。付款、生效及续费结果仍分别记录。 |

关键入口：`internal/platform/subscription_protocol.go:124`（任务）→ `internal/cslive/gptpay_client.go:182`（Pay）→ `internal/cslive/gptpay_transaction.go:248`（执行分支）→ Hosted/Custom → `internal/cslive/gptpay_flow_recovery.go`。独立新提链入口为 `internal/platform/checkout_protocol.go:95`，调用原 CreateCheckout；该入口限制为提链，不具备付款、升级和取消续费权限。

## 全部9个共有函数差异

| 函数 | 当前文件 | 实际变化 |
| --- | --- | --- |
| `gptpay_PaymentClient.Pay` | `internal/cslive/gptpay_client.go:182` | 接收并传递可选 PreparedCheckout；新新组合为空，旧新组合使用。 |
| `gptpay_transaction.createCheckout` | `internal/cslive/gptpay_transaction.go:165` | 向下一阶段私下返回刷新后的 Session；传递原始 checkout kind，支持明确的长短链选型。 |
| `gptpay_transaction.executeFlow` | `internal/cslive/gptpay_transaction.go:248` | 可导入旧流程原 Checkout；明确 Hosted 的 ID 检查；保存真实长链供展示/恢复。 |
| `gptpay_transaction.prepareCheckout` | `internal/cslive/gptpay_flow_checkout.go:10` | 按显式选型固定分支；显式选型阻止直接升级；保存原长链。 |
| `gptpay_transaction.runHostedFlow` | `internal/cslive/gptpay_flow_hosted.go:10` | confirm 后、轮询前增加脱敏诊断；提交及后续交易步骤保持。 |
| `gptpay_transaction.observePaymentDiagnostics` | `internal/cslive/gptpay_payment_diagnostics.go:5` | 将原诊断提取移到辅助函数，供中间响应使用；最终错误分类仍由最终响应赋值。 |
| `transport_Transport.Request` | `internal/cslive/transport_transport.go:124` | 每次请求前调用项目钩子，可因原代理过期、IP变化、无效租约/目标或重复提链而提前停止。 |
| `transport_Transport.observe` | `internal/cslive/transport_transport.go:306` | 将特定“暂无订阅”的响应记录为对应诊断类别，不改变上游调用和付款判断。 |
| `observability_SafePaymentStatus` | `internal/cslive/observability_action.go:15` | 诊断白名单增加 active。 |

4个扩展类型：PayOptions/transaction 增加 PreparedCheckout；CheckoutResult 增加私有 Session（JSON不输出）；Transport 增加 BeforeRequest 钩子。

## 项目外层的行为差异

1. **账单和订单参数**：原 CLI 读取账单文件、Session、卡片及参数；本站使用后台随机账单快照、订单保存的套餐/席位/地区/续费选择。受理后沿用本单账单，不跨阶段重新抽取。本站入口没有逐项暴露原 CLI 的全部可选工具及参数；独立提链默认不请求 WithPrice，Team 使用本站月付配置。
2. **原代理及请求门槛**：原包接受调用方代理；本站由原订单代理绑定和容量租约生成私有桥接，Windows 可再走本机系统代理。只有 IPRoyal 自动追加供应商 session。提链记录基准出口；卡资料/确认请求及 SDK 窗口启动前校验出口。普通请求持续检查原租约、代理期限及允许目标。这些额外门槛可使本站在原包尚未发出同一请求前停止。
3. **金额与防重**：原 CLI 显示金额后同意继续，原包另有可选 expected amount；本站回调检查订单金额上限、币种、服务开关，并在数据库中原子写入提交防重状态。数据库写入失败或租约丢失会停止。
4. **旧新组合**：原包没有本站的两段开关。本站新增 PreparedCheckout，只使用旧提链已经生成的原结账，跳过再次访问首页/刷新/账号预查/创建 Checkout；新提链交旧付款则传递刷新后的授权。正常新新组合不经过这个导入分支。
5. **持久化和恢复入口**：原包使用带锁的本地加密文件；本站使用数据库加密检查点、订单锁及租约。已扣款证据先保存并结算本站服务费，生效/续费失败保持独立状态。失去原代理或恢复授权到期时停止自动接续；进程中断的记录保留为待核实，不重开付款。
6. **监控**：增加白名单诊断、管理员完整流程及原长链留存。原始报文、Cookie、AT、卡片和验证内容不进入报告或公开响应。

## SDK 页面一致，但运行包装不同

- HTML SHA-256（忽略末尾换行）：`6f63e9b14b2d0829e85c8c59cde2617995badcec1b111cdfe38e8aafe0de655e`。
- JS SHA-256（忽略末尾换行）：`a7c39adae40e4ad8585b64a20fb2ba6806967f5d409adefcb7e152178d8184bc`。
- 原包 CLI 在配好证书且遇待验证时提供本机入口，由人在浏览器打开并点击；项目使用服务器私有 Chromium，在虚拟 HTTPS 来源托管同一页面，自动点击初始按钮，并经原订单代理处理外部请求。
- 实际发送给页面的 app.js 在原脚本前拼接了 `payment-action-observer.js`，记录 SDK 阶段和异常；因此“原 JS 文件相同”不等于最终执行脚本字节完全相同。额外还有网络监控、租约检查、页面生命周期和可选管理员交互。
- 原 SDK 的读取原 PaymentIntent→核对身份/状态→handleNextAction 顺序保持；原包和项目最终都以 Recover 的权威结果为准，不以浏览器完成通知直接判定付款成功。
- Pay/Recover 的原45秒单请求、240秒操作预算保持。项目外层为 SDK（最多10分钟）和随后 Recover 另预留时间，仍受原代理有效期限制。

## 与上一轮 HAR 对照的关系

本次包和当前新流程的 Hosted 版本都是 `2026-03-25.dahlia`；本次包的 HostedForm 同样没有两个 passive captcha 字段。其余客户端字段、RV 相关处理及付款表单实现也一致。因此先前指出的新流程与旧付款 HAR 的版本/验证码差异，**不是项目导入时漏掉原包对应实现**。原包自身与该 HAR 不是同一套完全等价的线路，不能把两个比较结论混在一起。

18项共有依赖在两边 go.mod 中声明的版本一致；原包用于终端输入的 `golang.org/x/term` 未作为项目直接依赖。未在此次审计中运行真实上游链路，源码一致和本地静态对照都不构成付款成功证明。
