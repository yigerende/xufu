import {test} from 'node:test'
import assert from 'node:assert/strict'
import {buildPaymentJourney,currencyAmount} from '../src/paymentJourney.ts'
const event=(phase,state='completed',more={})=>({phase,state,created_at:'2026-10-31T09:26:00Z',round:1,...more})
const base=()=>({order:{id:'offline',workflow:'subscription',status:'succeeded',product_name:'Pro 5x',checkout_flow:'cslive',subscription_flow:'cslive_browser',activation:'active',renewal:'disabled',fee:1000,created_at:'2026-10-31T09:25:00Z'},payment:{state:'succeeded',amount:579464,currency:'php',charged_amount:null,fee_state:'charged'},links:[{round:1,url:'https://pay.openai.com/c/pay/cs_test_Offline'}],events:[event('complete')],billing:{state:'selected',country:'US',region:'OR',city:'Portland'}})
const stage=(j,id)=>j.stages.find(s=>s.id===id)
test('repeated renewal progress becomes one stage backed by the saved renewal result',()=>{
 const data=base();data.events=Array.from({length:9},(_,i)=>event('renewal',i%2?'running':'completed')).concat(event('complete'))
 const j=buildPaymentJourney(data)
 assert.equal(j.stages.length,6);assert.equal(j.stages.filter(s=>s.id==='renewal').length,1)
 assert.equal(stage(j,'renewal').result,'自动续费已关闭');assert.equal(j.attention,'')
 assert.ok(stage(j,'renewal').facts.some(f=>f.label==='取消请求次数'&&f.value.includes('未区分')))
 assert.equal(data.events.length,10)
})
test('HTTP success never proves cancellation, payment or activation',()=>{
 const data=base();data.order={...data.order,status:'review',activation:'review',renewal:'review'};data.payment.state='review'
 data.events=['checking','activation','renewal'].map(p=>event(p,'completed',{http_status:200}))
 const j=buildPaymentJourney(data)
 for(const id of ['payment','activation','renewal'])assert.equal(stage(j,id).state,'review')
 assert.equal(j.state,'review')
})
test('paid orders retain payment success when renewal needs attention',()=>{
 const data=base();data.order.renewal='review'
 const j=buildPaymentJourney(data);assert.equal(stage(j,'payment').state,'completed');assert.equal(j.state,'review');assert.equal(j.attention,'自动续费设置')
})
test('temporary errors do not override later authoritative success',()=>{
 const data=base();data.events=[event('checking','failed',{description:'旧异常'}),event('verification','review'),event('renewal','failed'),event('complete')]
 const j=buildPaymentJourney(data);assert.equal(j.state,'completed');assert.equal(j.attention,'')
})
test('quote is never mislabeled as an actual charge',()=>{
 const j=buildPaymentJourney(base());assert.equal(j.summary.find(s=>s.label==='付款报价').value,'PHP 5,794.64')
 assert.ok(stage(j,'payment').facts.find(f=>f.label==='实收金额').value.includes('未取得'))
})
test('currency uses integer minor units, with zero and missing values distinct',()=>{
 assert.equal(currencyAmount(3000,'jpy'),'JPY 3,000');assert.equal(currencyAmount(0,'php'),'PHP 0.00');assert.equal(currencyAmount(null,'php'),'未记录');assert.equal(currencyAmount(1.2,'php'),'未记录')
})
test('a completed network check without explicit comparison cannot claim matching exits',()=>{
 const data=base();data.events=[event('network'),event('complete')]
 assert.equal(buildPaymentJourney(data).summary.find(s=>s.label==='本轮出口 IP').value,'未记录明确比对结果')
 data.events.unshift(event('network','completed',{code:'exit_ip_matched',round:1}));data.links.push({round:2})
 assert.equal(buildPaymentJourney(data).summary.find(s=>s.label==='本轮出口 IP').value,'未记录明确比对结果')
})
test('overlapping durations use wall time, not the sum of nested events',()=>{
 const data=base();data.events=[event('renewal','completed',{elapsed_ms:12000}),event('renewal','completed',{elapsed_ms:8000}),event('complete')]
 const j=buildPaymentJourney(data);assert.equal(stage(j,'renewal').duration,'12.0 秒');assert.equal(j.total,'1 分 0 秒')
})
test('request counts deduplicate ids and ignore stage completion rows',()=>{
 const data=base();data.events=[event('renewal'),...Array.from({length:3},()=>event('renewal','completed',{http_status:200,request_id:'req_one',diagnostics:{request_kind:'subscription_read'}}))]
 const j=buildPaymentJourney(data);assert.equal(stage(j,'renewal').facts.find(f=>f.label==='订阅查询').value,'已记录 1 次请求返回')
})
test('historical orders without traces do not fabricate intermediate executions',()=>{
 const j=buildPaymentJourney({order:{workflow:'subscription',status:'review'},events:[]})
 assert.equal(stage(j,'checkout').state,'waiting');assert.equal(stage(j,'activation').state,'waiting');assert.equal(j.total,'未记录完整起止时间')
})
test('link-only jobs omit payment, activation and renewal stages',()=>{
 const data=base();data.order.workflow='link';delete data.payment
 const j=buildPaymentJourney(data);assert.deepEqual(j.stages.map(s=>s.id),['checkout','complete'])
})
test('failed tax quote preserves reason and leaves downstream tasks unexecuted',()=>{
 const data=base();data.order={...data.order,status:'failed',activation:'not_checked',renewal:'not_started'};data.payment={...data.payment,state:'failed',error_code:'amount_changed',fee_state:'released',message:'金额变化，本次未提交付款'}
 const j=buildPaymentJourney(data);assert.equal(stage(j,'billing').state,'review');assert.equal(stage(j,'activation').result,'未执行');assert.equal(stage(j,'payment').state,'failed')
})
test('a kept renewal is a customer choice, not a cancellation',()=>{
 const data=base();data.order.keep_renewal=true;data.order.renewal='kept'
 const j=buildPaymentJourney(data);assert.equal(stage(j,'renewal').state,'skipped');assert.match(stage(j,'renewal').result,/保留/)
})
test('refresh is nested in its business stage and does not create extra stages',()=>{
 const data=base();data.events=[event('activation','running'),event('session_refresh'),event('renewal','running'),event('session_refresh'),event('complete')]
 const j=buildPaymentJourney(data);for(const id of ['activation','renewal'])assert.equal(stage(j,id).facts.find(f=>f.label==='账号授权刷新').value,'最近一次刷新完成')
 assert.equal(j.stages.length,6)
})
test('official verification is actionable and does not imply another charge',()=>{
 const data=base();data.order={...data.order,status:'review',activation:'not_checked',renewal:'not_started'};data.payment.state='action_required'
 const j=buildPaymentJourney(data);assert.match(stage(j,'payment').next,/原订单/);assert.equal(stage(j,'payment').state,'review')
})
