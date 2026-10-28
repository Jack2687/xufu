# 付款抓包分析（更新于 2026-10-28）

来源：用户本机 `支付包.har`（125 条请求）与 `付款打开`（157 条请求，无扩展名的 JSON HAR）。只记录协议结构和结果，不复制 HAR、卡号、CVV、账单、Cookie、Session、客户资料、商户密钥、支付标识或验证令牌到源码。

## 2026-10-28 结账身份解析修复

Hosted payment_pages 的初始化、税费更新及付款确认响应均使用 `object=checkout.session`、`id=ppage_...`、`session_id=cs_...`。订单绑定必须比较 `session_id` 与当前结账链接的 cs_ 标识，并校验页面编号格式；不能把 ppage_ 页面编号直接与 cs_ 订单编号比较。身份不匹配、缺少 session_id 或对象类型不正确的响应仍拒绝。

旧代码误比较 id，导致税费已经计算完成的 HTTP 200 响应被忽略，直到准备阶段超时。本地真实未付款订单只读复查已验证修复后正确识别 init 与 tax_region 更新；没有填写真实银行卡或调用付款提交。10 项运行时回归采用真实响应形状，覆盖成功、取消、金额变更、未知结果、额外验证、拒付及只读核实。不能据此宣称已实测真实扣款成功。

两份文件的结账会话 ID 不同，发布公钥相同。它们分别证明页面初始化与一次成功付款的请求结构，不能当成同一笔订单的连续录制，也不能拼接复用会话参数。

## 打开付款页：本次新增证据

1. `POST api.stripe.com/v1/payment_pages/{checkout_session_id}/init`：HTTP 200，表单编码，字段为 `key`、`eid`、`browser_locale`、`browser_timezone`、`redirect_type`。返回 `state=active`、`status=open`、`payment_status=unpaid`、`mode=subscription`、`ui_mode=hosted`、币种 PHP。
2. 初始化响应包含本次的 `eid`、`init_checksum`、`config_id`、`session_id`、`elements_options`、验证配置、税费状态和金额。字段存在不代表后续每个请求都需要发送，不能猜测补齐。
3. `GET api.stripe.com/v1/payment_pages/allowed_origins`：HTTP 200，查询参数为 `key`、`session_id`。
4. `GET api.stripe.com/v1/elements/sessions`：HTTP 200，包含本次结账会话、金额/币种、订阅模式、组件来源、浏览器时区/语言及版本等参数；响应中的 `passive_captcha` 包含 `captcha_provider_payload`、`rqdata`、`site_key`、`token_timeout_seconds` 等实时配置。
5. 包内有官方浏览器组件与 hCaptcha 交互。此事实只能确认现场验证依赖，不能据此宣称后台已能生成有效验证令牌；不复用抓包令牌、不伪造验证结果。

本次没有提交银行卡或付款确认，支付意图为空，不能把初始化成功标记为付款成功。

## 已确认的过程

1. `POST api.stripe.com/v1/payment_methods`：提交真实卡资料与账单信息，HTTP 200 返回本次付款方式标识。这一步不等于扣款成功。
2. `POST api.stripe.com/v1/payment_pages/{checkout_session_id}/confirm`：携带本次付款方式、应付金额、会话参数及验证令牌。该次响应为 HTTP 200，`state=succeeded`、`status=complete`、`payment_status=paid`、`payment_intent.status=succeeded`。币种 PHP。
3. `GET api.stripe.com/v1/checkout/sessions/completed_webhook_delivered/{checkout_session_id}`：共 11 次，前 10 次 `completed=false`，最后为 `true`。这是支付通知交付进度，不应把前面的 false 当作扣款失败，也不能再次提交扣款。
4. 浏览器进入 `chatgpt.com/payments/success`。仅跳转成功页本身不能作为付款成功依据。

## 金额与税费

- 打开包：`total_summary.subtotal/total/due=579464`，即 **5,794.64 PHP**；自动税费开启，`tax_meta.status=requires_location_inputs`，原因为 `no_buyer_location_provided`。这是尚缺账单所在地的初始金额，不能直接锁定为最终扣款金额。
- 成功付款包：小计仍为 579464，税费 `total_tax_exclusive=37086`，合计/应付为 616550，即 **6,165.50 PHP**；税费计算完成，税务国家 US。确认请求的 `expected_amount` 同为 616550。
- 两笔订单不是同一个会话；以上不能证明新订单稍后必定产生相同税额。两份包均未包含完整的“账单填写后金额刷新”接口过程，不能从中确定具体刷新端点或省略该步骤。
- 付款时必须使用当前订单按账单资料重新计算后的金额与币种，并让客户确认最终金额。发生变化应重新确认，不能为了通过校验而覆盖已同意的金额。

## 两份 HAR 仍未覆盖的分支

- 从初始化到按账单资料完成税费重算、确认最终金额的连续过程。
- 成功确认请求中的 `passive_captcha_token` 和 `rv_timestamp` 的完整实时来源。新增包提供验证配置与浏览器验证交互，仍不能将两笔会话的材料混用。实际执行必须通过当前会话的官方组件完成验证。
- 3DS / 额外用户验证分支；本次 `next_action` 为空。
- 拒付、处理中、失败、连接中断后的只读结果核实。
- 支付通知交付之后的订阅生效查询，本包未包含对应后端查询。

## 接入边界

已实现服务端临时浏览器付款运行时，由当前官方页面完成实时初始化、税费刷新、动态参数获取及单次提交；客户确认实时金额后才付款。额外用户验证须按官方流程完成，不以旧令牌替代；当前需要客户打开官方页面完成额外验证。

用户最新要求只有提链使用配置代理。付款各阶段使用机器默认网络，卡资料仅保留于内存，确认前持久化提交标记，未知结果只读核实；原始上游请求/响应不进入客户或管理员接口。运行时模拟分支已验证，尚未进行真实银行卡扣款测试。

## 早期代理连接记录

同轮使用该订单配置的代理读取真实长链，结账 HTML 返回 200，但 `js.stripe.com` / `api.stripe.com` 多次出现 `ERR_TUNNEL_CONNECTION_FAILED`，页面未完成初始化。仅可据此确认这几次连接失败，不能推断代理永久不可用。未填卡、未提交扣款、未切换为直连。用户提供的新 HAR 有成功初始化响应，但不证明本地服务当前代理已能完成同一流程。

## 按最新网络规则补充的实测

使用运行机器默认网络，真实官方页面及 init 均返回 HTTP 200，卡片与账单字段已核对。只填写后台账单资料，未填写卡号/CVV，并阻止付款方式创建及确认接口。

实际观察到 `POST /v1/payment_pages/{id}` 刷新：税费由 requires_location_inputs 变为 complete，当前示例金额由 579464 PHP 最小单位变为 616550，仍为 unpaid。这补充了两份原 HAR 中缺少的税费刷新证据，不能把该数值写死为其他订单金额。

私有验证摘要位于 `.local/payment-init/default-network-summary.json` 和 `default-network-quote.json`，不将原始抓包或上下文凭据加入公开文件。没有实际提交付款，因此不能宣称实测支付成功。

## 补充抓包：付款打开包括税率计算

用户于 2026-10-28 提供，共 264 个请求。包含 init 及付款页 POST 刷新：提交 tax_region 的 country/state/postal_code/line1/city，税费状态由 requires_location_inputs 变为 complete；应付由 579464 变为 616550 PHP 最小单位，税费 37086，仍为 unpaid。本次资料进一步证实官方页面实时税额刷新，不提供真实新支付成功证据，不重用 eid 或其他动态参数。
