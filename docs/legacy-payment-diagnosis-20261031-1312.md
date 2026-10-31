# 2026-10-31 13:12 旧协议失败核对

截图对应本地订单 `7752dc505da7a030d58f33c18004b87e`，13:11:43受理，Pro5x/PH/hosted长链，旧提链＋legacy_http。当前运行PID5012，程序SHA256 `807870DE79EE8ECA6C6D30BE098E7A933578C8E7A54A581A89A82788C53E1928`，与上次本地更新一致。

## 实际记录

- 提链、初始化及两次账单税区更新均HTTP200，报价579464分PHP。
- 13:12:26.804付款前出口检查 `exit_ip_matched`，耗时4337ms。
- 13:12:32.317创建PaymentMethod返回HTTP400、JSON、`invalid_request_error`；Request-ID `req_gRAMkko9fvqYXL`，实际发送版本 `2020-08-27;custom_checkout_beta=v1`。
- 没有confirm请求记录，没有requires_action/use_stripe_sdk；最终submitted=false、failed/request_rejected、fee_state=released、charged_amount为空。没有本流程已确认扣款证据。
- 前一笔 `e43020d8bf178223f53decf7a9ea412c` 于13:11:01在付款前遭遇proxy_session_changed；不是本截图这笔400，不能混为同一原因。

## 当前代码与完整支付包.har的参数比较

使用当前真实form builder与内存HTTP替身，虚构卡片和同单配置，无外部网络、数据库或真实付款。重新对照完整HAR索引308、312，非沿用旧审计统计。原失败请求正文不保留，因此下列是代码路径的离线重建，不是原失败网络包的逐字还原。

| 项目 | 成功HAR | 当前实现 |
|---|---|---|
| PaymentMethod表单字段 | 24 | 19 |
| confirm表单字段 | 18 | 12；本笔尚未执行 |
| 表单Stripe版本 | 2020-08-27;custom_checkout_beta=v1 | 相同，原失败诊断也确认 |
| Origin/Referer | 原Hosted来源 | 离线生成相同 |
| HTTP User-Agent | 浏览器 | Xupay/1.0 |
| SDK运行环境 | 真实页面/Stripe.js | Go HTTP，没有真实JS环境 |

PaymentMethod缺少五项：`client_attribution_metadata[client_session_id]`、`guid`、`muid`、`sid`、`payment_user_agent`。其中表单payment_user_agent与HTTP User-Agent不是同一个字段。当前并未复制抓包的SDK会话/设备数据。

confirm另外缺少的动态字段包括`passive_captcha_token`和`rv_timestamp`，但本单根本没有进入confirm，不能将其判为本次已发生的验证码失败。HTTP/浏览器请求头及传输环境也并非等价，自动传输头或HTTP/2伪头不能简单当成缺失业务参数。

## 能确定与不能确定的原因

能确定的失败点是Stripe拒绝创建PaymentMethod；本笔不是本站IP检查拦截、不是确认付款后的验证失败。当前代码的SDK上下文和环境与成功浏览器样本有明确差异，不能称已完全对齐或已实付成功。

不能确定是上述哪一个差异导致400。现存诊断只有通用invalid_request_error，没有可用的provider_code/provider_param/provider_reason。实现只保留白名单错误字段及三类固定message原因，没有保存完整错误说明；缺少字段既可能是上游未提供，也可能是白名单未收录，不能反推“上游未指出原因”。这些信息无法从现有历史记录恢复。

后续定位需取得这一请求的Stripe错误详情（上述Request-ID）或下一次受控请求的脱敏错误说明，不能用泛化400断言唯一根因、复制旧验证令牌或把模拟回归通过当作真实付款通过。本次没有重试原单、重新付款、改运行实现或重启。

脱敏原单轨迹、当前请求字段输出和对比文件位于 `.local/legacy-diagnosis-20261031-1312/`。离线审计测试通过，1.837秒。
