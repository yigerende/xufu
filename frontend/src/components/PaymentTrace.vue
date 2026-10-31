<script setup lang="ts">
import {computed} from 'vue'
import {Clock3,RefreshCw,CheckCircle2,AlertTriangle,LoaderCircle,ChevronRight} from 'lucide-vue-next'
import {date,type Row} from '../lib'
const props=defineProps<{events:Row[];links?:Row[];status?:string}>()
const rounds=computed(()=>Math.max(1,...props.events.map(e=>Number(e.round)||1),...(props.links||[]).map(e=>Number(e.round)||1)))
const statusLabel:Record<string,string>={running:'开始',completed:'完成',failed:'失败',review:'待核实',waiting:'等待'}
const requestLabels:Record<string,string>={exit_probe:'检查网络出口',checkout_create:'请求生成结账链接',checkout_initialize:'初始化结账页面',session_read:'读取官方登录态',account_read:'读取订阅账号',subscription_read:'查询订阅状态'}
const eventLabel=(e:Row)=>requestLabels[e.diagnostics?.request_kind]|| (e.diagnostics?.page_event?'商户回跳页面':e.diagnostics?.sdk_request?'官方验证资源':e.phase==='confirming'?'提交付款请求':e.label)
const eventStatus=(e:Row)=>e.diagnostics?.page_event?(e.diagnostics.page_event==='approval_not_requested'?'尚未发起批准':['request_failed','request_blocked','script_error'].includes(e.diagnostics.page_event)?'页面异常':e.http_status>=400?'请求被拒绝':'已记录'):e.diagnostics?.sdk_waiting||e.diagnostics?.sdk_issue==='resource_pending'?'等待中':e.diagnostics?.sdk_event==='network'?'资源已返回':e.diagnostics?.sdk_issue==='resource_failed'||e.diagnostics?.sdk_issue==='resource_http_error'?'资源失败':e.phase==='confirming'&&e.state==='completed'?'请求已返回':statusLabel[e.state]||e.state
const diagnosticLabels:Record<string,string>={page_event:'回跳页面事件',page_host:'页面资源域名',page_route:'页面类型',page_resource:'资源类型',page_auth:'登录信息字段',page_network_error:'浏览器错误码',sdk_connect_ms:'资源连接耗时',sdk_tls_ms:'资源握手耗时',sdk_response_ms:'资源响应等待',sdk_host:'资源域名',sdk_route:'资源路径类型',sdk_resource_key:'资源关联标识',sdk_method:'请求方法',sdk_request_type:'浏览器请求类型',sdk_network_error:'浏览器错误码',sdk_request:'请求序号',sdk_requests:'已发起请求',sdk_completed:'已完成请求',sdk_failed:'失败请求',sdk_pending:'仍在等待请求',sdk_duration_ms:'资源请求耗时',sdk_guard_ms:'出口检查耗时',sdk_stage:'验证阶段',sdk_resource:'资源类型',sdk_event:'记录类型',sdk_issue:'异常原因',sdk_wait_ms:'本阶段等待',sdk_waiting:'仍在等待',sdk_outcome:'SDK 处理结果',sdk_code:'SDK 诊断码',diagnostic_source:'状态来源',payment_state:'付款状态',raw_status:'原交易状态',raw_state:'原流程状态',raw_payment_status:'原付款状态',next_action_type:'后续验证类型',sdk_action_type:'SDK 验证类型',sdk_has_stripe_js:'提供 SDK 验证入口',sdk_has_3ds_source:'提供 3DS 资源',dns_ms:'域名解析',connect_ms:'建立连接',tls_ms:'安全握手',first_byte_ms:'等待首字节',connection_ready_ms:'通道就绪',request_write_ms:'请求发送',response_wait_ms:'响应等待',network_stage_elapsed_ms:'网络阶段耗时',request_attempt:'请求尝试次数',network_stage:'网络阶段',protocol_code:'流程诊断码',network_reason:'网络原因',provider_code:'上游错误码',decline_code:'拒付分类',response_kind:'响应类型'}
const diagnosticValues:Record<string,string>={navigated:'页面已跳转',dom_ready:'页面结构已加载',script_error:'页面脚本异常',response:'页面请求已返回',resource_loaded:'脚本资源已加载',request_failed:'页面请求失败',request_blocked:'请求被允许域名规则拦截',auth_observed:'登录信息已读取',approval_observed:'批准请求已进入授权检查',approval_not_requested:'未观察到商户批准请求',merchant_return:'商户回跳',auth_session:'登录信息',login:'登录页面',checkout:'结账页面',present:'包含登录信息',missing:'未包含登录信息',unknown:'尚未确认',resource_completed:'资源读取完成',resource_pending:'资源请求尚未返回',network:'网络请求',stripe_v3:'Stripe SDK 主脚本',stripe_basil:'Stripe Basil 脚本',payment_intent:'原付款状态查询',three_ds:'3DS 资源',static_asset:'静态资源',browser_start:'启动验证浏览器',page_loading:'加载官方 SDK',sdk_loaded:'SDK 页面脚本就绪',context_ready:'原交易上下文就绪',preflight:'SDK 正在预查原交易',preflight_returned:'原交易预查已返回',preflight_error:'原交易预查异常',sdk_action:'等待官方验证返回',sdk_returned:'官方验证已返回',sdk_exception:'官方验证调用异常',notifying:'回传验证结果',progress:'进展',diagnostic:'诊断',sdk_script:'SDK 主脚本',sdk_frame:'SDK 资源',processor_api:'支付状态接口',verification_resource:'验证资源',resource_failed:'资源加载失败',resource_http_error:'资源返回错误',guard_timeout:'出口检查响应超时',csp_blocked:'资源被页面策略拦截',page_error:'页面脚本异常',timeout:'超时',proxy_connect:'代理隧道连接失败',tls:'安全握手失败',dns:'域名解析失败',connection:'建立连接',request_write:'发送请求',response_headers:'等待响应头',response_body:'读取响应内容',json:'JSON',html:'HTML 页面',empty:'空响应',other:'其他格式'}
Object.assign(requestLabels,{payment_method_create:'创建银行卡付款方式',payment_token_create:'创建付款令牌',payment_confirm:'提交付款确认',payment_read:'读取原付款状态',billing_update:'更新账单与税费'})
Object.assign(diagnosticLabels,{provider_error_type:'上游错误类型',provider_param:'被拒绝的参数名',provider_reason:'上游拒绝原因',request_kind:'请求环节',challenge_required:'上游要求安全验证',edge_request_id:'网关请求编号'})
Object.assign(diagnosticValues,{raw_card_api_restricted:'上游限制直接通过 API 提交卡号',secret_key_required:'该请求需要商户私钥权限',invalid_request_error:'请求参数或接口权限错误',card_error:'银行卡错误',api_error:'上游接口错误',authentication_error:'接口鉴权错误',permission_error:'接口权限错误',idempotency_error:'幂等请求冲突',rate_limit_error:'请求限流'})
Object.assign(diagnosticValues,requestLabels)
Object.assign(diagnosticLabels,{stripe_api_version:'本次请求 API 版本'})
Object.assign(diagnosticValues,{invalid_api_version:'上游不接受指定的 API 版本'})
const diagnosticText=(key:string,value:unknown)=>typeof value==='boolean'?(value?'是':'否'):key.endsWith('_ms')?`${value} 毫秒`:diagnosticValues[String(value)]||String(value)
const duration=(v:unknown)=>Number(v)>=1000?`${(Number(v)/1000).toFixed(1)} 秒`:`${Number(v)||0} 毫秒`
</script>
<template>
  <section class="payment-trace" aria-label="完整支付链条">
    <div class="trace-heading"><div><span class="eyebrow">PAYMENT ACTIVITY</span><h3>完整支付链条</h3></div><span class="trace-rounds"><RefreshCw :size="14"/>共 {{rounds}} 轮 · 重建 {{rounds-1}} 次</span></div>
    <p class="trace-hint">时间线自动更新；点击状态展开详情，查看阶段结果和耗时。</p>
    <p v-if="!events.length" class="trace-empty">此订单尚无新版阶段记录，可查看下方已有业务记录。历史步骤不会补写为已执行。</p>
    <ol v-else>
      <li v-for="(e,i) in events" :key="e.id||i" :class="e.state">
        <span class="trace-icon"><AlertTriangle v-if="['failed','review'].includes(e.state)" :size="16"/><Clock3 v-else-if="e.state==='waiting'||e.diagnostics?.sdk_waiting" :size="16"/><LoaderCircle v-else-if="e.state==='running'" :size="16"/><CheckCircle2 v-else :size="16"/></span>
        <details class="trace-content">
          <summary class="trace-title">
            <b>{{eventLabel(e)}}</b><span class="trace-state">{{eventStatus(e)}}</span>
            <time :datetime="e.created_at">{{date(e.created_at)}}</time>
            <small>第 {{e.round}} 轮</small><small v-if="Number(e.elapsed_ms)>0">耗时 {{duration(e.elapsed_ms)}}</small>
            <ChevronRight class="trace-chevron" :size="14"/>
          </summary>
          <div class="trace-body">
            <p v-if="e.http_status||e.request_id" class="mono">{{e.http_status?'响应 '+e.http_status:''}} {{e.request_id?' · 关联编号 '+e.request_id:''}}</p>
            <p v-if="e.description">{{e.description}}</p>
            <dl v-if="e.diagnostics&&Object.keys(e.diagnostics).length" class="trace-diagnostics"><template v-for="(value,key) in e.diagnostics" :key="key"><dt>{{diagnosticLabels[String(key)]||key}}</dt><dd>{{diagnosticText(String(key),value)}}</dd></template></dl>
            <p v-if="!e.http_status&&!e.request_id&&!e.description&&!Object.keys(e.diagnostics||{}).length">暂无补充详情。</p>
          </div>
        </details>
      </li>
    </ol>
  </section>
</template>
<style scoped>
.payment-trace{margin:22px 0;padding:22px;background:#f7f9fc;border:1px solid #e3e9f0;border-radius:14px}.trace-heading{display:flex;justify-content:space-between;align-items:center;gap:12px}.trace-heading h3{margin:6px 0}.trace-rounds{display:flex;align-items:center;gap:6px;color:#53647b;font-size:12px;white-space:nowrap}.trace-hint,.trace-empty{color:#77849a;font-size:12px;line-height:1.7}.payment-trace ol{list-style:none;margin:20px 0 0;padding:0}.payment-trace li{display:flex;gap:12px;position:relative;padding:0 0 18px}.payment-trace li:not(:last-child):before{content:'';position:absolute;left:12px;top:25px;bottom:0;width:1px;background:#dbe4ee}.trace-icon{width:26px;height:26px;display:grid;place-items:center;border-radius:50%;background:#e5edf8;color:#496c9a;flex-shrink:0}.completed .trace-icon{background:#e1f3e9;color:#268757}.failed .trace-icon,.review .trace-icon{background:#fff0dd;color:#b87014}.trace-title{display:flex;align-items:center;gap:9px;flex-wrap:wrap;font-size:13px}.trace-title small,.trace-content time{font-size:11px;color:#7c899c}.trace-content p{margin:7px 0;color:#65738b;font-size:12px;line-height:1.7}.trace-state{font-size:11px;color:#526880;background:#e8edf3;padding:2px 7px;border-radius:4px}.failed .trace-state,.review .trace-state{color:#a36215;background:#fff0dd}.trace-content{flex:1;min-width:0}.trace-title{list-style:none;cursor:pointer;min-height:26px}.trace-title::-webkit-details-marker{display:none}.trace-title:focus-visible{outline:2px solid #496c9a;outline-offset:4px;border-radius:4px}.trace-content time{white-space:nowrap}.trace-chevron{margin-left:auto;flex-shrink:0;transition:transform .15s}.trace-content[open] .trace-chevron{transform:rotate(90deg)}.trace-body{padding:5px 0 2px}.trace-diagnostics{grid-template-columns:auto minmax(0,1fr)}@media(max-width:650px){.payment-trace{padding:15px}.trace-heading{align-items:flex-start;flex-direction:column}}
.trace-diagnostics{display:grid;grid-template-columns:auto 1fr;gap:4px 12px;margin:8px 0;font-size:11px;color:#65738b}.trace-diagnostics dd{margin:0;overflow-wrap:anywhere}
</style>
