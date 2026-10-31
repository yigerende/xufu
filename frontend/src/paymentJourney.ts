import type {Row} from './lib'

type State='completed'|'running'|'waiting'|'review'|'failed'|'skipped'
type Fact={label:string;value:string}
export type JourneyStage={id:string;title:string;state:State;result:string;facts:Fact[];events:Row[];duration:string;next:string}
type Input={order?:Row;payment?:Row;checkout?:Row;billing?:Row;events?:Row[];links?:Row[]}
const validAmount=(n:unknown)=>typeof n==='number'&&Number.isSafeInteger(n)&&n>=0
export function currencyAmount(amount:unknown,currency:unknown){
 if(!validAmount(amount)||typeof currency!=='string'||!['usd','php','jpy','sgd','gbp','egp','try'].includes(currency.toLowerCase()))return '未记录'
 return currency.toUpperCase()+' '+(Number(amount)/(currency.toLowerCase()==='jpy'?1:100)).toLocaleString('zh-CN',{minimumFractionDigits:currency.toLowerCase()==='jpy'?0:2,maximumFractionDigits:currency.toLowerCase()==='jpy'?0:2})
}
export function durationText(ms:number){return ms>=60000?`${Math.floor(ms/60000)} 分 ${Math.round(ms%60000/1000)} 秒`:ms>=1000?`${(ms/1000).toFixed(1)} 秒`:`${Math.round(ms)} 毫秒`}
const time=(v:unknown)=>typeof v==='string'&&Number.isFinite(Date.parse(v))?Date.parse(v):null
function elapsed(events:Row[]){
 const intervals=events.flatMap(e=>{const t=time(e.created_at);return t===null?[]:[[t-Math.max(0,Number(e.elapsed_ms)||0),t]]})
 return intervals.length?durationText(Math.max(...intervals.map(x=>x[1]))-Math.min(...intervals.map(x=>x[0]))):'未记录'
}
export const flowNames:Record<string,string>={legacy:'旧提链 → 旧浏览器付款',legacy_protocol:'旧提链 → 旧协议付款',cslive:'新提链 → 新流程付款',cslive_browser:'新提链 → 旧浏览器付款'}
const paymentNames:Record<string,string>={legacy:'旧浏览器',legacy_protocol:'旧 HTTP 协议',cslive:'新流程',cslive_browser:'旧浏览器'}
const paymentStates:Record<string,[State,string]>={preparing:['running','准备中'],quoted:['running','报价已就绪'],submitting:['running','付款处理中'],checking:['running','核实付款结果'],succeeded:['completed','付款成功'],failed:['failed','付款未完成'],cancelled:['skipped','已停止'],review:['review','付款结果待核实'],action_required:['review','等待官方验证']}
const activationStates:Record<string,[State,string]>={active:['completed','订阅已生效'],pending:['waiting','等待生效核实'],review:['review','订阅生效待核实'],not_checked:['waiting','未单独记录核实结果']}
const renewalStates:Record<string,[State,string]>={disabled:['completed','自动续费已关闭'],kept:['skipped','按要求保留自动续费'],review:['review','关闭续费待处理'],retrying:['running','正在重新处理续费'],pending:['waiting','等待处理'],not_started:['waiting','尚未开始']}
function requestCount(events:Row[],kind:string){
 const ids=new Set(events.filter(e=>e.diagnostics?.request_kind===kind&&e.http_status>0&&e.request_id).map(e=>e.request_id))
 return ids.size?`已记录 ${ids.size} 次请求返回`:'未单独记录次数'
}

// This is a presentation over existing admin projections. An HTTP 200 or a
// completed progress row alone never proves a payment, activation or cancellation.
export function buildPaymentJourney(input:Input){
 const {order:o={},payment:p={},checkout:c={},billing:b={},links=[],events:raw=[]}=input
 const events=[...raw].sort((a,b)=>(time(a.created_at)??0)-(time(b.created_at)??0))
 const rounds=Math.max(1,...events.map(e=>Number(e.round)||1),...links.map(e=>Number(e.round)||1))
 const stopped=['failed','cancelled'].includes(o.status),subscription=o.workflow==='subscription'
 const stages:JourneyStage[]=[['checkout','生成结账链接'],['billing','账单与税费'],['payment','订阅付款'],['activation','核实订阅'],['renewal','自动续费设置'],['complete','办理结果']].map(([id,title])=>({id,title,state:'waiting',result:'尚未开始',facts:[],events:[],duration:'未记录',next:''}))
 const stage=(id:string)=>stages.find(s=>s.id===id)!
 let context='checkout'
 for(const e of events){
  let id: string
  if(['accepted','queue','proxy','account_check','checkout','restart','opening'].includes(e.phase))id='checkout'
  else if(['billing','pricing'].includes(e.phase))id='billing'
  else if(['card_prepare','confirming','submitted','checking','result','verification'].includes(e.phase))id='payment'
  else if(['activation','renewal'].includes(e.phase))id=e.phase
  else if(e.phase==='complete'||e.phase==='funds')id='complete'
  else id=context
  if(!['session_refresh','network','funds','complete'].includes(e.phase))context=id
  stage(id).events.push(e)
 }
 for(const s of stages){
  s.duration=elapsed(s.events)
  const latest=s.events.filter(e=>(Number(e.round)||1)===rounds).at(-1)
  if(latest){s.state=latest.state==='failed'?'failed':['review','running','waiting'].includes(latest.state)?latest.state:'waiting';s.result=s.state==='failed'?'阶段未完成':s.state==='review'?'需要核实':s.state==='running'?'处理中':'等待结果确认'}
 }
 const set=(id:string,value:[State,string])=>{const s=stage(id);[s.state,s.result]=value}
 const fact=(id:string,label:string,value:string)=>stage(id).facts.push({label,value})
 const currentLinks=links.filter(l=>(Number(l.round)||1)===rounds)
 let linkType='未记录'
 for(const link of currentLinks){try{const u=new URL(link.url);if(['pay.openai.com','checkout.stripe.com'].includes(u.hostname))linkType='长链';else if(u.hostname==='chatgpt.com'&&u.pathname.startsWith('/checkout/'))linkType='短链'}catch{}}
 const checkoutDone=currentLinks.length>0||c.ready===true
 if(checkoutDone)set('checkout',['completed','结账链接已生成'])
 else if(o.status==='queued')set('checkout',['waiting','等待办理资源'])
 fact('checkout','提链流程',o.checkout_flow==='legacy'?'旧提链':o.checkout_flow==='cslive'?'新提链':'未记录')
 fact('checkout','本单链接类型',linkType)
 fact('checkout','结账轮次',`${rounds} 轮${rounds>1?` · 重建 ${rounds-1} 次`:''}`)
 const quoted=currencyAmount(p.amount,p.currency),received=currencyAmount(p.charged_amount,p.charged_currency)
 if(quoted!=='未记录')set('billing',p.error_code==='amount_changed'?['review','报价发生变化']:['completed','报价已记录'])
 else if(b.state==='selected')set('billing',['waiting','账单已选定，等待报价'])
 fact('billing','本单账单',b.state==='selected'?[b.country,b.region,b.city].filter(Boolean).join(' / '):b.state==='unavailable'?'暂时无法读取':'尚未选定')
 if(b.profile_id)fact('billing','账单编号',b.profile_id)
 fact('billing',p.state==='succeeded'?'付款报价':'最近保存的报价',quoted)
 fact('billing','初始金额 / 税额明细','历史记录未单独保存，不从报价差额推算')
 set('payment',paymentStates[p.state]||['waiting','尚无付款结果'])
 fact('payment','付款流程',paymentNames[o.subscription_flow]||'未记录')
 fact('payment','付款报价',quoted)
 fact('payment','实收金额',received==='未记录'?'未取得独立实收金额凭据':received)
 if(p.last4)fact('payment','银行卡',`尾号 ${p.last4}`)
 const submitted=stage('payment').events.filter(e=>e.phase==='submitted'&&e.state==='completed').length
 fact('payment','实际提交记录',submitted?`已记录 ${submitted} 次提交事件`:'未单独记录提交次数')
 fact('payment','确认接口',requestCount(stage('payment').events,'payment_confirm'))
 set('activation',activationStates[o.activation]||['waiting','尚未核实'])
 if(o.activation==='pending'&&p.state==='succeeded'&&!stopped)set('activation',['running','正在核实订阅生效'])
 fact('activation','目标套餐',o.product_name||'未记录')
 fact('activation','核实结果',o.activation==='active'?'已通过本单套餐生效核实':stage('activation').result)
 fact('activation','实际套餐 / 有效期','历史记录未单独保存')
 fact('activation','订阅查询',requestCount(stage('activation').events,'subscription_read'))
 set('renewal',renewalStates[o.renewal]||['waiting','尚未开始'])
 if(o.renewal==='pending'&&o.activation==='active'&&!stopped)set('renewal',['running','正在处理自动续费'])
 fact('renewal','办理要求',o.keep_renewal?'保留自动续费':'关闭自动续费')
 fact('renewal','核实结果',stage('renewal').result)
 fact('renewal','取消请求次数','历史记录未区分提交与查询，不按日志条数计数')
 fact('renewal','订阅查询',requestCount(stage('renewal').events,'subscription_read'))
 for(const s of stages){
  const refresh=s.events.filter(e=>e.phase==='session_refresh')
  if(refresh.length){const last=refresh.at(-1)!;s.facts.push({label:'账号授权刷新',value:last.state==='completed'&&(!last.http_status||last.http_status<400)?'最近一次刷新完成':last.state==='running'?'刷新中':'刷新未完成'})}
 }
 const activeStages=stages.filter(s=>subscription||['checkout','complete'].includes(s.id)||p.state&&['billing','payment'].includes(s.id))
 const problems=activeStages.filter(s=>['failed','review'].includes(s.state)&&s.id!=='complete')
 if(o.status==='succeeded')set('complete',problems.length?['review','付款完成，后续仍需处理']:['completed',subscription?'订阅办理完成':'办理完成'])
 else if(stopped)set('complete',[o.status==='failed'?'failed':'skipped',o.status==='failed'?'本次办理未完成':'本次办理已停止'])
 else if(o.status==='review'||problems.length)set('complete',['review','有待核实事项'])
 else if(o.workflow==='link'&&checkoutDone)set('complete',['completed','结账链接已生成'])
 else set('complete',['running','办理中'])
 const feeState:Record<string,string>={held:'已冻结',released:'已解冻',charged:'已结算'}
 const fee=validAmount(o.fee)?(o.fee/100).toFixed(2)+' R':'未记录'
 fact('complete','服务费',`${fee} · ${feeState[p.fee_state]||'未记录结算状态'}`)
 const finish=events.filter(e=>e.phase==='complete').at(-1)
 const start=time(o.created_at),end=time(finish?.created_at)
 const total=start!==null&&end!==null&&end>=start?durationText(end-start):'未记录完整起止时间'
 fact('complete','全程耗时',total)
 for(const s of activeStages){
  if(stopped&&['waiting','running'].includes(s.state)){s.state='skipped';s.result=s.events.length?'流程已停止，结果未确认':'未执行'}
  if(['failed','review'].includes(s.state)){
   const last=[...s.events].reverse().find(e=>['failed','review'].includes(e.state)&&e.description&&e.code!=='subscription_absent')
   if(last)fact(s.id,'最近异常',last.description)
   s.next=s.id==='renewal'?'核对该账号的续费状态；付款已成功时无需重新订阅。':s.id==='activation'?'核对原账号的实际套餐与生效状态，无需再次付款。':s.id==='billing'?'核对完整账单对应的最终税费报价。':s.id==='payment'&&p.state==='action_required'?'在原订单的官方验证入口完成验证，再核实原交易。':s.id==='payment'&&p.state==='review'?'核实原交易结果，避免再次付款。':'查看下方失败原因与技术日志，确认后再处理。'
   if(s.id==='payment'&&p.message)fact(s.id,'处理说明',p.message)
  }
 }
 const network=events.filter(e=>e.phase==='network'&&(Number(e.round)||1)===rounds)
 const mismatch=network.some(e=>e.code==='proxy_session_changed')
 const matched=network.some(e=>e.code==='exit_ip_matched')
 const ip=mismatch?'曾检测到出口变化':matched?'已记录出口一致':'未记录明确比对结果'
 const summary=[
  {label:'本单办理组合',value:subscription?(flowNames[o.subscription_flow]||'未记录'):`${o.checkout_flow==='legacy'?'旧提链':o.checkout_flow==='cslive'?'新提链':'提链'} · 仅生成链接`},
  {label:'目标套餐',value:o.product_name||'未记录'},
  {label:received!=='未记录'?'实收金额':'付款报价',value:received!=='未记录'?received:quoted},
  {label:'服务费',value:`${fee} · ${feeState[p.fee_state]||'未记录'}`},
  {label:'本轮出口 IP',value:ip},
  ...(subscription?[{label:'订阅 / 自动续费',value:`${stage('activation').result} · ${stage('renewal').result}`}]:[])
 ]
 return {stages:activeStages,summary,rounds,total,result:stage('complete').result,state:stage('complete').state,attention:activeStages.filter(s=>['failed','review'].includes(s.state)&&s.id!=='complete').map(s=>s.title).join('、')}
}
