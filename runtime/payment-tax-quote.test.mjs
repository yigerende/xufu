import {test} from 'node:test'
import assert from 'node:assert/strict'
import {chromium} from 'playwright'
import {run} from './payment-browser.mjs'

const id='cs_test_TaxQuote',base='https://api.stripe.com/v1/payment_pages/'+id
const billing={first_name:'Offline',last_name:'Test',line1:'100 Test Street',city:'Portland',state:'OR',postal_code:'97205'}
const address={country:'US',line1:billing.line1,city:billing.city,state:billing.state,postal_code:billing.postal_code}
function pageBody(amount,paid=false){return {object:'checkout.session',id:'ppage_TaxQuote',session_id:id,currency:'php',total_summary:{due:amount},tax_context:{automatic_tax_enabled:true},tax_meta:{status:'complete'},status:paid?'complete':'open',payment_status:paid?'paid':'unpaid',state:paid?'succeeded':'active'}}

// All requests, including the official checkout and tax endpoint, are fulfilled
// locally. The fixture contains no real account, card or payment credentials.
for(const mode of ['delayed_final','unchanged_total','higher_tax','partial_only','wrong_billing','wrong_checkout','failed_tax','out_of_order','unstable_at_deadline','change_during_probe','change_during_confirm_probe'])test('hosted billing quote: '+mode,{timeout:20000},async()=>{
 let resolveCommand,confirmations=0,finalTaxReturned=false,probes=0,pageRef,changed=false
 const accepted=['delayed_final','unchanged_total','higher_tax','change_during_probe','change_during_confirm_probe'].includes(mode)
 const amount=mode==='unchanged_total'?649000:mode==='higher_tax'?681450:579464
 const sentAddress={...address,...(mode==='wrong_billing'?{city:'Wrong City'}:{})}
 const events=[],quotes=[]
 const form=`<input id="cardNumber"><input id="cardExpiry"><input id="cardCvc"><input id="billingName">
 <select id="billingCountry"><option>US</option></select><input id="billingAddressLine1"><input id="billingLocality">
 <select id="billingAdministrativeArea"><option>OR</option></select><input id="billingPostalCode"><button type="submit">Pay</button><script>
 fetch('${base}/init',{method:'POST'});

 window.finalTax=()=>fetch('${base}',{method:'POST',body:new URLSearchParams(${JSON.stringify(Object.fromEntries(Object.entries(sentAddress).map(([k,v])=>['tax_region['+k+']',v])))})});
 document.querySelector('#billingPostalCode').onblur=()=>{fetch('${base}',{method:'POST',body:new URLSearchParams({'tax_region[country]':'US'})});${mode==='partial_only'?'':'return finalTax();'}};
 document.querySelector('button').onclick=()=>fetch('${base}/confirm',{method:'POST',body:new URLSearchParams({expected_amount:'${amount}'})});
 </script>`
 const input={proxy:{server:'http://127.0.0.1:18799',username:'offline',password:'offline'},expected_ip:'203.0.113.2',proxy_expires_at:Date.now()+60000,mode:'prepare',url:'https://pay.openai.com/c/pay/'+id,email:'qa@example.com',card:{number:'4242424242424242',month:'12',year:'2035',cvc:'123'},billing:{...billing},prepare_seconds:mode==='delayed_final'?9:4,quote_seconds:3,result_seconds:2}
 await run(input,{next:()=>new Promise(r=>resolveCommand=r)},event=>{
  events.push(event)
  if(event.type==='quote'){quotes.push({amount:event.amount,finalTaxReturned});setTimeout(()=>resolveCommand({value:{type:'confirm'}}),0)}
 },{exitIP:async()=>{
  probes++
  if(mode==='change_during_probe'&&probes===3||mode==='change_during_confirm_probe'&&probes===4){changed=true;await pageRef.evaluate(()=>window.finalTax())}
  return '203.0.113.2'
 },playwright:{chromium:{launch:async options=>{
  const browser=await chromium.launch({headless:true,executablePath:options.executablePath})
  const original=browser.newContext.bind(browser);browser.newContext=async options=>{const ctx=await original({...options,proxy:undefined});ctx.on('page',p=>pageRef=p);return ctx};return browser
 }}},route:async route=>{
  const req=route.request(),u=new URL(req.url()),headers={'access-control-allow-origin':'*','access-control-allow-headers':'*'}
  if(u.hostname==='pay.openai.com')return route.fulfill({status:200,contentType:'text/html',body:form})
  if(u.hostname!=='api.stripe.com')return route.abort()
  let data=pageBody(649000)
  if(u.pathname==='/v1/payment_pages/'+id){
   const fields=new URLSearchParams(req.postData()||'')
   if(fields.has('tax_region[postal_code]')){
    await new Promise(r=>setTimeout(r,mode==='delayed_final'?4500:mode==='unstable_at_deadline'?3200:200))
    data=pageBody(amount+(changed?100:0));finalTaxReturned=true
    if(mode==='wrong_checkout')data.session_id='cs_test_Other'
    if(mode==='failed_tax')return route.fulfill({status:400,headers,contentType:'application/json',body:'{}'})
   }
   else await new Promise(r=>setTimeout(r,mode==='delayed_final'?1200:mode==='out_of_order'?2300:50))
  }
  if(u.pathname.endsWith('/confirm')){confirmations++;data=pageBody(amount,true)}
  return route.fulfill({status:200,headers,contentType:'application/json',body:JSON.stringify(data)})
 }})
 assert.deepEqual(quotes,accepted?[{amount,finalTaxReturned:true}]:[],JSON.stringify(events))
 const success=accepted&&!mode.startsWith('change_during_')
 assert.equal(events.at(-1).state,success?'succeeded':'failed',JSON.stringify(events))
 assert.equal(confirmations,success?1:0)
 assert.ok(events.some(e=>e.diagnostics?.request_kind==='billing_update'&&e.phase==='pricing'))
 if(!success){assert.equal(events.at(-1).code,accepted?'amount_changed':'quote_unavailable');assert.equal(events.at(-1).sent,false)}
 assert.equal(JSON.stringify(events).includes(billing.line1),false)
})
