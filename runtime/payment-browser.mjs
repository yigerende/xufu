// Private, single-use browser process. stdin/stdout are an anonymous pipe to Go.
// Never record HAR, screenshots, storage state, card data, console or raw errors.
import readline from 'node:readline'
import {pathToFileURL} from 'node:url'
import fs from 'node:fs'
import {isIP} from 'node:net'
import {customIdentity,loginCookies,CustomPayment} from './payment-custom.mjs'
import {HostedApproval} from './payment-hosted-approval.mjs'
import {observeMerchantPage} from './payment-page-diagnostics.mjs'

const currencies = new Set(['php','usd','jpy','sgd','gbp','egp','try'])
export function checkoutIdentity(raw) {
  const custom=customIdentity(raw)
  if(custom)return custom.id
  const u = new URL(raw)
  if (u.protocol !== 'https:' || u.username || u.password || u.port || !['pay.openai.com','checkout.stripe.com'].includes(u.hostname)) throw Error('unsupported_checkout')
  const m = u.pathname.match(/^\/(?:c\/)?pay\/(cs_(?:live|test)_[a-zA-Z0-9]+)$/)
  if (!m) throw Error('unsupported_checkout')
  return m[1]
}
export function allowedHost(host) {
  return ['pay.openai.com','checkout.stripe.com','chatgpt.com','cdn.oaistatic.com','tcr9i.chat.openai.com','challenges.cloudflare.com','api.stripe.com','js.stripe.com','stripe.com','stripe.network','b.stripecdn.com','hcaptcha.com','stripe-camo.global.ssl.fastly.net','d1wqzb5bdbcre6.cloudfront.net','maps.googleapis.com','places.googleapis.com','applepay.cdn-apple.com'].some(h=>host===h||(['stripe.com','stripe.network','hcaptcha.com'].includes(h)&&host.endsWith('.'+h)))
}
export function paymentProxy(input) {
  const p=input.proxy
  if(!p?.server||!p.username||!p.password||(input.skip_exit_ip_check!==true&&!isIP(input.expected_ip||'')))throw Error('proxy_session_unavailable')
  const u=new URL(p.server)
  if(u.protocol!=='http:'||u.hostname!=='127.0.0.1'||!u.port||u.username||u.password||u.search||u.hash||u.pathname!=='/')throw Error('proxy_session_unavailable')
  if(!Number.isSafeInteger(input.proxy_expires_at)||input.proxy_expires_at<=Date.now())throw Error('proxy_session_expired')
  return {server:p.server,username:p.username,password:p.password}
}
export function snapshot(body, id) {
  // Hosted payment_pages responses identify the page with ppage_ and bind it
  // to the checkout through session_id. Never compare the page ID to cs_.
  if (!body || body.object !== 'checkout.session' || body.session_id !== id || typeof body.id !== 'string' || !/^ppage_[a-zA-Z0-9]+$/.test(body.id)) return null
  const intent=body.payment_intent&&typeof body.payment_intent==='object'?body.payment_intent:null
  const intentID=typeof intent?.id==='string'&&/^pi_[A-Za-z0-9]+$/.test(intent.id)?intent.id:''
  if(intent?.currency&&intent.currency!==body.currency)return null
  const received=intentID&&intent.status==='succeeded'&&Number.isSafeInteger(intent.amount_received)&&intent.amount_received>0?intent.amount_received:null
  const amount = received??body.total_summary?.due, currency = body.currency
  if (!Number.isSafeInteger(amount) || amount <= 0 || amount > 10000000000 || !currencies.has(currency)) return null
  const paid = (received!==null)||(body.payment_status === 'paid' && body.status === 'complete' && body.state === 'succeeded' && (!intent || intent.status === 'succeeded'))
  const taxReady = body.tax_context?.automatic_tax_enabled === false || body.tax_meta?.status === 'complete'
  return {amount,currency,paid,taxReady,open:body.status==='open'&&body.payment_status==='unpaid',action:intent?.status==='requires_action',authorized:intent?.status==='requires_capture',evidence:{checkout_id:id,payment_intent_id:intentID,status:paid?'succeeded':intent?.status==='requires_capture'?'authorized':intent?.status==='requires_action'?'action_required':'pending',amount_received:received,currency}}
}
export function confirmMatches(url, form, id, quote) {
  return new URL(url).hostname==='api.stripe.com' && new URL(url).pathname===`/v1/payment_pages/${id}/confirm` && form.get('expected_amount')===String(quote.amount)
}
export function hostedPollSnapshot(body,id,pageID,latest,quote) {
  if(!/^ppage_[A-Za-z0-9]+$/.test(pageID||'')||body?.id!==pageID||body?.session_id!==id||body.state!=='succeeded'||body.payment_object_status!=='succeeded')return null
  if(!latest||latest.evidence?.checkout_id!==id||!quote||latest.amount!==quote.amount||latest.currency!==quote.currency)return null
  return {...latest,paid:true,open:false,action:false,authorized:false,evidence:{...latest.evidence,status:'succeeded',amount_received:null}}
}
const pause = ms=>new Promise(r=>setTimeout(r,ms))
const safePaymentCodes=new Set(['card_declined','expired_card','incorrect_cvc','incorrect_number','incorrect_zip','insufficient_funds','do_not_honor','generic_decline','authentication_required','payment_intent_authentication_failure','processing_error','billing_automatic_tax_unrecognized_customer_location'])
const diagnosticStatuses=new Set(['open','complete','expired','paid','unpaid','no_payment_required','succeeded','processing','pending','requires_payment_method','requires_confirmation','requires_action','requires_capture','requires_approval','canceled','cancelled','declined','failed','blocked'])
const diagnosticActions=new Set(['intent_confirmation_challenge','use_stripe_sdk','redirect_to_url','three_d_secure_2_fingerprint','three_d_secure_2_challenge','three_d_secure_2_source','stripe_3ds2_fingerprint','stripe_3ds2_challenge'])
export function paymentDiagnostics(body) {
  const rows=[]
  for(const [source,node] of [['response',body],['payment_intent',body?.payment_intent],['submission_attempt',body?.submission_attempt],['submission_payment_intent',body?.submission_attempt?.payment_intent],['error_payment_intent',body?.error?.payment_intent]]) {
    if(!node||typeof node!=='object')continue
    const d={diagnostic_source:source}
    for(const [key,value] of [['raw_status',node.status],['raw_state',node.state],['raw_payment_status',node.payment_status]])if(typeof value==='string')d[key]=diagnosticStatuses.has(value)?value:'redacted'
    const action=node.next_action,sdk=action?.use_stripe_sdk
    if(action?.type)d.next_action_type=diagnosticActions.has(action.type)?action.type:'redacted'
    if(sdk?.type)d.sdk_action_type=diagnosticActions.has(sdk.type)?sdk.type:'redacted'
    if(sdk){d.sdk_has_stripe_js=!!sdk.stripe_js;d.sdk_has_3ds_source=!!sdk.three_d_secure_2_source}
    const error=node.last_payment_error||node.error
    if(safePaymentCodes.has(error?.code))d.provider_code=error.code
    if(safePaymentCodes.has(error?.decline_code))d.decline_code=error.decline_code
    if(Object.keys(d).length>1)rows.push(d)
  }
  return rows
}

export async function run(input, commands, emit, dependencies={}) {
  let finished=false,observations=Promise.resolve()
  const send=emit
  emit=e=>{if(!finished)send(e)}
  let browser, hosted, pageDiagnostics, sent=false, permit=false, latest=null, pageID='', changedAt=0, quote=null, blocked='', responseCount=0, declined=false, networkFailed=false
  const stage = code=>emit({type:'stage',code})
  const done = (state,code='')=>{pageDiagnostics?.finish(hosted?.started===true);emit({type:'result',state,code,sent,amount:latest?.amount||0,currency:latest?.currency||'',evidence:latest?.evidence});finished=true}
  let quoteTimer
  const abort=()=>{browser?.close().catch(()=>{})}
  dependencies.signal?.addEventListener('abort',abort,{once:true})
  try {
    if(dependencies.signal?.aborted)throw Error('cancelled')
    const id=checkoutIdentity(input.url)
    const identity=customIdentity(input.url),custom=identity?new CustomPayment(identity):null
    hosted=custom?null:new HostedApproval(id,input.resume_approval===true)
    if(input.mode==='check')quote=input.saved_quote
    if(input.resume_approval){sent=true;quote=input.saved_quote}
    const proxy=paymentProxy(input)
    const {chromium}=dependencies.playwright || await import('playwright')
    const chrome=process.env.PAYMENT_CHROME_PATH || (process.platform==='win32' && fs.existsSync('C:/Program Files/Google/Chrome/Application/chrome.exe')?'C:/Program Files/Google/Chrome/Application/chrome.exe':undefined)
    browser=await chromium.launch({headless:true,executablePath:chrome,proxy,args:['--disable-quic','--force-webrtc-ip-handling-policy=disable_non_proxied_udp','--disable-background-networking','--disable-breakpad']})
    if(dependencies.signal?.aborted)throw Error('cancelled')
    const ctx=await browser.newContext({locale:'en-US',timezoneId:'America/New_York',serviceWorkers:'block',acceptDownloads:false,proxy})
    if(hosted)pageDiagnostics=observeMerchantPage(ctx,()=>hosted.navigated,emit)
    const hasLogin=!!input.session_cookies
    if(custom||hasLogin)await ctx.addCookies(loginCookies(input.session_cookies))
    input.session_cookies=''
    const checkExit=async()=>{
      if(Date.now()>=input.proxy_expires_at)throw Error('proxy_session_expired')
      if(dependencies.signal?.aborted)throw Error('proxy_session_unavailable')
      if(input.skip_exit_ip_check===true)return
      // Only retry a read-only probe through this unchanged proxy context. A
      // different valid IP is terminal; never replace the expected address.
      for(let attempt=1;attempt<=3;attempt++) {
        if(Date.now()>=input.proxy_expires_at)throw Error('proxy_session_expired')
        if(dependencies.signal?.aborted)throw Error('proxy_session_unavailable')
        let ip,status=0,retryable=true,reason='connection'
        const started=Date.now()
        try {
          if(dependencies.exitIP)ip=await dependencies.exitIP()
          else {
            const response=await ctx.request.get('https://ipinfo.io/json',{timeout:Math.min(10000,input.proxy_expires_at-Date.now()),maxRedirects:0})
            try {
              status=response.status()
              retryable=status===429||status>=500
              if(status===200){retryable=false;ip=(await response.json()).ip}
            }finally{await response.dispose()}
          }
        }catch(e){
          const message=String(e?.message||'').toLowerCase()
          reason=/timeout|timed out/.test(message)?'timeout':/proxy|tunnel/.test(message)?'proxy_connect':/tls|ssl|certificate/.test(message)?'tls':'connection'
        }
        if(Date.now()>=input.proxy_expires_at)throw Error('proxy_session_expired')
        if(isIP(ip||'')) {
          if(ip!==input.expected_ip)throw Error('proxy_session_changed')
          return
        }
        emit({type:'diagnostic',phase:'network',http_status:status,code:'proxy_session_unavailable',elapsed_ms:Date.now()-started,diagnostics:{request_attempt:attempt,...(status?{}:{network_reason:reason})}})
        if(!retryable||attempt===3||dependencies.signal?.aborted)throw Error('proxy_session_unavailable')
        await pause(750)
      }
    }
    stage('network_check')
    await checkExit()
    stage('network_verified')
    await ctx.route('**/*',async route=>{
      const req=route.request(), u=new URL(req.url())
      if(Date.now()>=input.proxy_expires_at){blocked='proxy_session_expired';return route.abort()}
      if(u.protocol!=='https:' || !allowedHost(u.hostname) || u.username || u.password || (u.port&&u.port!=='443')) {pageDiagnostics?.blocked(req);return route.abort()}
      // SDK tokenization may start while fields are being filled. Check the
      // original exit before allowing card data to leave this browser.
      if(req.method()==='POST'&&u.hostname==='api.stripe.com'&&/^\/v1\/(payment_methods|confirmation_tokens|tokens)$/.test(u.pathname)) {
        if(input.mode==='check'){blocked='duplicate_blocked';return route.abort()}
        try{await checkExit()}catch(e){blocked=e.message;return route.abort()}
      }
      if(req.method()==='POST'&&u.hostname==='api.stripe.com'&&u.pathname.endsWith('/verify_challenge')) {
        await observations
        if(input.mode==='check'||!sent||!permit||!latest?.evidence.payment_intent_id||u.pathname!==`/v1/payment_intents/${latest.evidence.payment_intent_id}/verify_challenge`){blocked='duplicate_blocked';return route.abort()}
        try{await checkExit()}catch(e){blocked=e.message;return route.abort()}
      }
      if(hosted&&u.hostname==='chatgpt.com'&&u.pathname.startsWith('/backend-api/payments/')&&!['GET','HEAD','OPTIONS'].includes(req.method())) {
        await observations
        if(!hasLogin||!hosted.matches(req)||!hosted.ready(latest,quote)||!(sent&&permit)){blocked='duplicate_blocked';return route.abort()}
        hosted.started=true
        try{await checkExit()}catch(e){blocked=e.message;return route.abort()}
        emit({type:'approval_requested',amount:quote.amount,currency:quote.currency,evidence:latest.evidence})
        const ack=await commands.next()
        if(ack.value?.type!=='approve'){blocked='duplicate_blocked';return route.abort()}
        return dependencies.route?dependencies.route(route):route.continue()
      }
      if(custom) {
        // A page may advance as soon as headers arrive. Finish observing the
        // previous response before authorizing its next dependent operation.
        await observations
        const decision=custom.guard(req,input.mode,permit,quote,latest)
        if(!decision.ok){blocked='duplicate_blocked';return route.abort()}
        if(decision.submitted) {
          try{await checkExit()}catch(e){blocked=e.message;return route.abort()}
          sent=true;emit({type:'submitted'})
        }
      } else if(req.method()==='POST' && /\/(confirm|pay|capture)$/.test(u.pathname)) {
        if(input.mode==='check'||!permit||sent) {blocked='duplicate_blocked';return route.abort()}
        if(!latest || latest.amount!==quote.amount||latest.currency!==quote.currency||!latest.taxReady||!confirmMatches(req.url(),new URLSearchParams(req.postData()||''),id,quote)) {blocked='amount_changed';return route.abort()}
        try{await checkExit()}catch(e){blocked=e.message;return route.abort()}
        sent=true
        emit({type:'submitted'})
      }
      return dependencies.route ? dependencies.route(route) : route.continue()
    })
    const page=await ctx.newPage()
    page.on('requestfailed',req=>{if(['api.stripe.com','js.stripe.com'].includes(new URL(req.url()).hostname)&&/TUNNEL|PROXY_CONNECTION/.test(req.failure()?.errorText||''))networkFailed=true})
    ctx.on('response',res=>{observations=observations.then(async()=>{
      const u=new URL(res.url())
      if(hosted&&hosted.started&&hosted.matches(res.request())) {
        emit({type:'diagnostic',phase:'confirming',http_status:res.status(),code:res.status()>=400?'request_rejected':''})
        if(res.status()===200)hosted.approvalReturned=true
        return
      }
      if(custom&&['chatgpt.com','api.stripe.com'].includes(u.hostname)) {
        const data=await res.json().catch(()=>null)
        const s=custom.observe(res.request(),data,res.status(),snapshot)
        if(s&&s!==latest){latest=s;changedAt=Date.now();responseCount++}
        if(u.hostname==='chatgpt.com')return
      }
      if(u.hostname!=='api.stripe.com')return
      const related=u.pathname.startsWith(`/v1/payment_pages/${id}`)
      if(!related&&u.pathname!=='/v1/payment_methods')return
      const body=await res.json().catch(()=>null)
      const headers=res.headers(),requestID=headers['request-id']||headers['x-request-id']||''
      const details={response_kind:body?'json':'other'}
      for(const key of ['code','decline_code'])if(safePaymentCodes.has(body?.error?.[key]))details[key==='code'?'provider_code':key]=body.error[key]
      const timing=res.request().timing(),elapsed=Number.isFinite(timing.responseEnd)&&timing.responseEnd>=0?Math.round(timing.responseEnd):0
      emit({type:'diagnostic',phase:u.pathname.endsWith('/confirm')?'confirming':quote?'checking':'pricing',http_status:res.status(),request_id:/^req_[A-Za-z0-9]{8,80}$/.test(requestID)?requestID:'',code:res.status()>=400?'request_rejected':'',elapsed_ms:elapsed,diagnostics:details})
      if(!related)return
      for(const diagnostics of paymentDiagnostics(body))emit({type:'diagnostic',phase:quote?'checking':'pricing',http_status:res.status(),diagnostics})
      if(u.pathname===`/v1/payment_pages/${id}/confirm`&&res.status()===402&&body?.error?.type==='card_error'&&['card_declined','expired_card','incorrect_cvc','incorrect_number'].includes(body.error.code)&&(!body.error.payment_intent||body.error.payment_intent.status==='requires_payment_method'))declined=true
      if(res.status()!==200)return
      const poll=u.pathname===`/v1/payment_pages/${id}/poll`
      const s=poll?hostedPollSnapshot(body,id,pageID,latest,quote):snapshot(body,id)
      hosted?.observe(body,s)
      if(s&&!latest?.paid){if(!poll)pageID=body.id;latest=s;changedAt=Date.now();responseCount++}
    }).catch(()=>{})})
    const navigate=()=>page.goto(input.url,{waitUntil:'domcontentloaded',timeout:Math.min(input.prepare_seconds*1000,60000)})
    const continueApproval=async()=>{
      if(!hasLogin||!hosted?.ready(latest,quote)||hosted.navigated)return
      hosted.navigated=true
      await checkExit()
      stage('approval')
      await page.goto(hosted.landing.url,{waitUntil:'domcontentloaded',timeout:Math.min(input.prepare_seconds*1000,60000)})
    }
    const verifyApproval=async()=>{
      if(!hosted?.approvalReturned||hosted.verificationOpened||latest?.paid)return
      hosted.verificationOpened=true
      await checkExit()
      // Keep the merchant/SDK page alive. Observe a separate copy of the original
      // checkout; the existing request guards forbid another confirmation.
      const verifyPage=await ctx.newPage()
      await verifyPage.goto(input.url,{waitUntil:'domcontentloaded',timeout:Math.min(input.prepare_seconds*1000,60000)})
    }
    stage('opening')
    const preparationDeadline=Date.now()+input.prepare_seconds*1000
    await navigate()
    if(input.mode==='check') {
      const until=Date.now()+input.prepare_seconds*1000
      while(Date.now()<until){
        if(latest?.paid)return done('succeeded')
        if(blocked)return done('review',blocked)
        if(input.resume_approval&&latest?.open&&quote){
          permit=true
          await continueApproval()
          await verifyApproval()
        }
        await pause(300)
      }
      return done(latest?.action?'action_required':'review',latest?.action?'verification_required':'result_unconfirmed')
    }
    // No endpoint parameters or verification tokens are synthesized. The current
    // official checkout renders fields, calculates tax and performs verification.
    const find = async selectors=>{
      for(const f of page.frames())for(const selector of selectors){const x=f.locator(selector).first();if(await x.isVisible().catch(()=>false))return x}
      return null
    }
    const field = async (selectors,value,required=true)=>{
      const until=required?preparationDeadline:Math.min(preparationDeadline,Date.now()+500)
      do {
        if(Date.now()>=input.proxy_expires_at)throw Error('proxy_session_expired')
        if(dependencies.signal?.aborted)throw Error('cancelled')
        const x=await find(selectors)
        if(x){const tag=await x.evaluate(el=>el.tagName);if(tag==='SELECT')await x.selectOption(value);else await x.fill(value);await x.blur();return true}
        await pause(200)
      }while(Date.now()<until)
      if(required)throw Error('checkout_fields_unavailable')
      return false
    }
    await checkExit()
    await field(['#cardNumber','input[name="cardnumber"]','input[name="cardNumber"]','input[name="number"]','input[autocomplete="cc-number"]'],input.card.number)
    await field(['#cardExpiry','input[name="exp-date"]','input[name="cardExpiry"]','input[name="expiry"]','input[autocomplete="cc-exp"]'],`${input.card.month}/${input.card.year.slice(-2)}`)
    await field(['#cardCvc','input[name="cvc"]','input[name="cardCvc"]','input[autocomplete="cc-csc"]'],input.card.cvc)
    input.card={}
    stage('billing')
    await field(['#email','input[type="email"]'],input.email,false)
    await field(['#billingName','input[name="billingName"]','input[autocomplete="cc-name"]','input[name="name"]','input[autocomplete="name"]'],`${input.billing.first_name} ${input.billing.last_name}`)
    await field(['#billingCountry','select[name="billingCountry"]','select[autocomplete="billing country"]','select[name="country"]'],'US')
    const manual=page.getByText(/enter address manually/i).first()
    if(await manual.isVisible().catch(()=>false)) await manual.click()
    const beforeAddress=responseCount
    await field(['#billingAddressLine1','input[name="billingAddressLine1"]','input[autocomplete="billing address-line1"]','input[name="addressLine1"]','input[autocomplete$=" address-line1"]'],input.billing.line1)
    await field(['#billingLocality','input[name="billingLocality"]','input[autocomplete="billing address-level2"]','input[name="locality"]','input[autocomplete$=" address-level2"]'],input.billing.city)
    await field(['#billingAdministrativeArea','select[name="billingAdministrativeArea"]','input[name="billingAdministrativeArea"]','select[autocomplete="billing address-level1"]','select[name="administrativeArea"]','input[name="administrativeArea"]','select[autocomplete$=" address-level1"]'],input.billing.state)
    await field(['#billingPostalCode','input[name="billingPostalCode"]','input[autocomplete="billing postal-code"]','input[name="postalCode"]','input[autocomplete$=" postal-code"]'],input.billing.postal_code)
    stage('pricing')
    const quoteUntil=Date.now()+input.prepare_seconds*1000
    while(Date.now()<quoteUntil) {
      if(latest?.paid) return done('review','already_paid')
      if(latest?.taxReady&&latest.open&&responseCount>beforeAddress&&Date.now()-changedAt>1500) break
      await pause(250)
    }
    if(!latest?.taxReady||!latest.open||responseCount<=beforeAddress) throw Error('quote_unavailable')
    quote={amount:latest.amount,currency:latest.currency}
    emit({type:'quote',...quote})
    const command=await Promise.race([commands.next(),new Promise(resolve=>{quoteTimer=setTimeout(()=>resolve({value:{type:'expired'}}),input.quote_seconds*1000)})])
    clearTimeout(quoteTimer)
    if(command.value?.type!=='confirm') return done('cancelled','quote_expired')
    if(!latest.taxReady||latest.amount!==quote.amount||latest.currency!==quote.currency) return done('failed','amount_changed')
    stage('network_check')
    await checkExit()
    stage('network_verified')
    permit=true
    stage('confirming')
    const button=await find(['button[data-testid="hosted-payment-submit-button"]','button.SubmitButton','button[type="submit"]'])
    if(!button)throw Error('checkout_fields_unavailable')
    await button.click({timeout:15000})
    stage('result_check')
    const until=Date.now()+input.result_seconds*1000
    while(Date.now()<until) {
      if(latest?.paid) return done('succeeded')
      if(declined) return done('declined','payment_declined')
      if(blocked) return done(sent?'review':'failed',blocked)
      // Keep the official page alive while its SDK completes the current
      // challenge. No synthetic token, puzzle solving or second confirm.
      if(latest?.authorized) return done('review','payment_authorized')
      await continueApproval()
      await verifyApproval()
      await pause(300)
    }
    // Even an apparently empty response after clicking may have caused a charge.
    return done(latest?.action?'action_required':sent?'review':'failed',latest?.action?'verification_required':sent?'result_unconfirmed':'verification_required')
  } catch (e) {
    // Never propagate exception strings: Playwright often includes filled values.
    const safeCodes=new Set(['invalid_session','unsupported_checkout','quote_unavailable','checkout_fields_unavailable','proxy_session_unavailable','proxy_session_expired','proxy_session_changed'])
    return done(sent?'review':'failed',sent?'result_unconfirmed':safeCodes.has(blocked)?blocked:networkFailed?'network_connection_failed':safeCodes.has(e.message)?e.message:'preparation_failed')
  } finally {
    clearTimeout(quoteTimer)
    dependencies.signal?.removeEventListener('abort',abort)
    input.card={};input.billing={};input.proxy={};input.session_cookies=''
    await browser?.close().catch(()=>{})
  }
}

if(process.argv[1]&&import.meta.url===pathToFileURL(process.argv[1]).href) {
  const lines=readline.createInterface({input:process.stdin,crlfDelay:Infinity})
  const shutdown=new AbortController()
  lines.on('close',()=>shutdown.abort())
  const iterator=lines[Symbol.asyncIterator]()
  const commands={next:async()=>{const r=await iterator.next();try{return {value:JSON.parse(r.value||'{}')}}catch{return {value:{type:'cancel'}}}}}
  let input
  try {input=JSON.parse((await iterator.next()).value);await run(input,commands,v=>process.stdout.write(JSON.stringify(v)+'\n'),{signal:shutdown.signal})}
  catch {process.stdout.write(JSON.stringify({type:'result',state:'failed',code:'runtime_unavailable',sent:false})+'\n')}
  finally{lines.close();process.stdin.destroy()}
}
