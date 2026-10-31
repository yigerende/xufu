import {test} from 'node:test'
import assert from 'node:assert/strict'
import {chromium} from 'playwright'
import {run} from './payment-browser.mjs'
import {hostedLanding} from './payment-hosted-approval.mjs'

const id='cs_test_HostedApproval',base='https://api.stripe.com/v1/payment_pages/'+id
const landing='https://chatgpt.com/checkout/verify?stripe_session_id='+id+'&processor_entity=openai_llc&plan_type=pro'
test('hosted approval accepts only a returned official callback for this checkout',()=>{
 assert.equal(hostedLanding({success_url:landing},id).url,landing)
 for(const url of [landing.replace('chatgpt.com','evil.example'),landing.replace(id,'cs_test_Other'),landing+'&stripe_session_id='+id,landing.replace('https:','http:'),'https://chatgpt.com/api/anything?stripe_session_id='+id])assert.equal(hostedLanding({success_url:url},id),null)
})

for(const mode of ['success','separate_verify','resume','readonly','duplicate','wrong_order','ip_changed','missing_login','return_denied','missing_auth','resource_failed','resource_blocked','script_error','merchant_resource'])test('hosted merchant approval: '+mode,{timeout:25000},async()=>{
 let confirm=0,approve=0,approvalRequests=0,landings=0,probes=0,merchantResources=0
 const events=[],buffer=[],pending=[]
 const commands={next:()=>buffer.length?Promise.resolve(buffer.shift()):new Promise(r=>pending.push(r))}
 const send=type=>{const v={value:{type}};pending.length?pending.shift()(v):buffer.push(v)}
 const body=(tax=true,paid=false)=>({id:'ppage_Hosted',object:'checkout.session',session_id:id,currency:'usd',total_summary:{due:1500},status:paid?'complete':'open',state:paid?'succeeded':'active',payment_status:paid?'paid':'unpaid',tax_context:{automatic_tax_enabled:true},tax_meta:{status:tax?'complete':'requires_location_inputs'},success_url:landing,...(confirm?{submission_attempt:{state:'requires_approval'}}:{})})
 const form=`<input id="cardNumber"><input id="cardExpiry"><input id="cardCvc"><input id="billingName"><select id="billingCountry"><option>US</option></select><input id="billingAddressLine1"><input id="billingLocality"><select id="billingAdministrativeArea"><option>OR</option></select><input id="billingPostalCode"><button type="submit">Pay</button><script>
 fetch('${base}/init',{method:'POST'});
 document.querySelector('#billingPostalCode').onblur=()=>fetch('${base}',{method:'POST',body:new URLSearchParams({'tax_region[country]':'US','tax_region[line1]':'Test Street','tax_region[city]':'Portland','tax_region[state]':'OR','tax_region[postal_code]':'97205'})});
 document.querySelector('button').onclick=()=>fetch('${base}/confirm',{method:'POST',body:new URLSearchParams({expected_amount:'1500'})});</script>`
 const verify=`<script>(async()=>{const p=()=>fetch('/backend-api/payments/checkout/approve',{method:'POST',headers:{'content-type':'application/json'},body:JSON.stringify({checkout_session_id:'${mode==='wrong_order'?'cs_test_Other':id}',processor_entity:'openai_llc'})});await p();${mode==='duplicate'?'try{await p()}catch{}':''}${mode==='separate_verify'?'':"await fetch('"+base+"')"}})()</script>`
 const deps={exitIP:async()=>{probes++;return mode==='ip_changed'&&confirm>0?'203.0.113.3':'203.0.113.2'},playwright:{chromium:{launch:async options=>{
   const b=await chromium.launch({headless:true,executablePath:options.executablePath}),create=b.newContext.bind(b)
   b.newContext=options=>create({...options,proxy:undefined});return b
 }}},route:async route=>{
   const req=route.request(),u=new URL(req.url()),headers={'access-control-allow-origin':'*','access-control-allow-headers':'*'}
   const json=v=>route.fulfill({status:200,headers,contentType:'application/json',body:JSON.stringify(v)})
   if(req.method()==='OPTIONS')return json({})
   if(u.hostname==='pay.openai.com'){assert.equal((req.headers().cookie||'').includes('PRIVATE_COOKIE'),false);return route.fulfill({status:200,contentType:'text/html',body:form})}
   if(req.url()===landing){
     landings++;assert.match(req.headers().cookie||'',/PRIVATE_COOKIE/)
     const pages={return_denied:'Access denied',missing_auth:"<script>fetch('/api/auth/session')</script>",resource_failed:'<script src="https://cdn.oaistatic.com/assets/PRIVATE.js?token=PRIVATE_SECRET"></script>',resource_blocked:'<script src="https://sentinel.openai.com/PRIVATE?token=PRIVATE_SECRET"></script>',script_error:'<script>throw Error("PRIVATE_SECRET")</script>'}
     return route.fulfill({status:mode==='return_denied'?403:200,contentType:'text/html',body:mode==='merchant_resource'?'<script src="https://challenges.cloudflare.com/turnstile/v0/api.js?PRIVATE_SECRET"></script>':pages[mode]||verify})
   }
   if(u.hostname==='challenges.cloudflare.com') {
     merchantResources++;assert.equal((req.headers().cookie||'').includes('PRIVATE_COOKIE'),false)
     // Local fixture verifies resource delivery only; no real challenge runs.
     return route.fulfill({status:200,contentType:'text/javascript',body:verify.replace(/^<script>|<\/script>$/g,'')})
   }
   if(u.pathname==='/api/auth/session')return json({user:null,private:'PRIVATE_SECRET'})
   if(u.hostname==='cdn.oaistatic.com')return route.abort('connectionreset')
   if(u.pathname==='/backend-api/payments/checkout/approve'){approve++;return json({result:'approved'})}
   if(u.hostname==='api.stripe.com')assert.equal((req.headers().cookie||'').includes('PRIVATE_COOKIE'),false)
   if(u.pathname.endsWith('/confirm')){confirm++;return json(body())}
   if(u.pathname.endsWith('/init'))return json(body(mode==='resume'||mode==='readonly'||approve>0,approve>0))
   if(req.url()===base)return json(body(true,approve===1))
   return route.abort()
 }}
 const check=['resume','readonly'].includes(mode)
 const input={url:'https://pay.openai.com/c/pay/'+id,session_cookies:mode==='missing_login'?'':'__Secure-next-auth.session-token=PRIVATE_COOKIE',resume_approval:mode==='resume',saved_quote:{amount:1500,currency:'usd'},mode:check?'check':'prepare',proxy:{server:'http://127.0.0.1:18799',username:'order',password:'PRIVATE_PROXY'},expected_ip:'203.0.113.2',proxy_expires_at:Date.now()+60000,card:{number:'4242424242424242',month:'12',year:'2035',cvc:'123'},billing:{first_name:'QA',last_name:'Test',line1:'Test Street',city:'Portland',state:'OR',postal_code:'97205'},prepare_seconds:5,quote_seconds:4,result_seconds:3}
 await run(input,commands,e=>{events.push(e);if(e.type==='quote')send('confirm');if(e.type==='approval_requested'){approvalRequests++;send('approve')}},deps)
 const success=['success','separate_verify','resume','duplicate','merchant_resource'].includes(mode)
 assert.equal(events.at(-1).state,success?'succeeded':'review',JSON.stringify(events))
 assert.equal(confirm,check?0:1)
 assert.equal(approve,success?1:0)
 assert.equal(approvalRequests,success?1:0)
 if(['readonly','missing_login','ip_changed'].includes(mode))assert.equal(landings,0)
 const pageEvents=events.filter(e=>e.diagnostics?.page_event)
 if(success)assert.ok(pageEvents.some(e=>e.diagnostics.page_event==='approval_observed'))
 if(mode==='merchant_resource') {
   assert.equal(merchantResources,1)
   assert.ok(pageEvents.some(e=>e.diagnostics.page_host==='challenges.cloudflare.com'&&e.diagnostics.page_event==='resource_loaded'))
 }
 if(['return_denied','missing_auth','resource_failed','resource_blocked','script_error'].includes(mode)) {
   assert.equal(pageEvents.at(-1).diagnostics.page_event,'approval_not_requested')
   assert.equal(confirm,1);assert.equal(approve,0)
 }
 if(mode==='return_denied')assert.ok(pageEvents.some(e=>e.http_status===403&&e.diagnostics.page_route==='merchant_return'))
 if(mode==='missing_auth')assert.ok(pageEvents.some(e=>e.diagnostics.page_auth==='missing'))
 if(mode==='resource_failed')assert.ok(pageEvents.some(e=>e.diagnostics.page_network_error==='ERR_CONNECTION_RESET'))
 if(mode==='resource_blocked')assert.ok(pageEvents.some(e=>e.diagnostics.page_event==='request_blocked'&&e.diagnostics.page_host==='sentinel.openai.com'))
 if(mode==='script_error')assert.ok(pageEvents.some(e=>e.diagnostics.page_event==='script_error'))
 assert.equal(JSON.stringify(events).includes('PRIVATE_'),false)
})
