import test from 'node:test'
import assert from 'node:assert/strict'
import {runAction,safeActionResult} from './payment-action.mjs'

const input=()=>({proxy:{server:'http://127.0.0.1:9',username:'fixture',password:'fixture'},expires_at:Math.floor(Date.now()/1000)+60,publishable_key:'pk_test_Fixture',client_secret:'pi_Fixture_secret_Test'})
for(const mode of ['automatic','interactive','wrong_intent','not_required','sdk_error']){
 test('official SDK original-transaction flow: '+mode,{timeout:20000},async()=>{
  const events=[],requests=[];let handler
  await runAction({...input(),automatic:mode==='automatic'},v=>events.push(v),{onCommand:fn=>{handler=fn}}, {
   route:async route=>{
    requests.push({method:route.request().method(),url:route.request().url()})
    if(route.request().url()==='https://js.stripe.com/v3/'){
     const body=`window.sdkCalls=[];window.Stripe=()=>({retrievePaymentIntent:async secret=>{sdkCalls.push('retrieve');return {paymentIntent:{id:${JSON.stringify(mode==='wrong_intent'?'pi_Other':'pi_Fixture')},status:${JSON.stringify(mode==='not_required'?'succeeded':'requires_action')}}}},handleNextAction:async()=>{sdkCalls.push('handle');${mode==='interactive'?`await new Promise(resolve=>{const b=document.createElement('button');b.id='fixture-interaction';b.textContent='Offline verification fixture';b.onclick=resolve;document.body.append(b)})`:''};return ${mode==='sdk_error'?`{error:{code:'card_declined'}}`:'{paymentIntent:{status:"succeeded"}}'}}})`;new Function(body);await route.fulfill({contentType:'application/javascript',body})
    }else await route.abort()
    return true
   },
   ready:async page=>{
    if(mode!=='automatic'){
     assert.deepEqual(await page.evaluate(()=>window.sdkCalls),[])
     assert.equal(events.some(e=>e.type==='result'),false)
     const start=await page.locator('#start').boundingBox()
     await handler({type:'click',x:start.x+start.width/2,y:start.y+start.height/2})
    }
    if(mode==='interactive'){
     await page.locator('#fixture-interaction').waitFor()
     assert.equal(events.some(e=>e.type==='result'),false)
     // An operator input is required for the fixture; never solve real challenges.
     const box=await page.locator('#fixture-interaction').boundingBox()
     await handler({type:'click',x:box.x+box.width/2,y:box.y+box.height/2})
    }
    await page.waitForFunction(()=>window.sdkCalls?.length>0)
    const calls=await page.evaluate(()=>window.sdkCalls)
    assert.deepEqual(calls,['wrong_intent','not_required'].includes(mode)?['retrieve']:['retrieve','handle'])
   }
  })
  const results=events.filter(e=>e.type==='result')
  assert.equal(results.length,1)
  assert.equal(results[0].code,({wrong_intent:'context_mismatch',not_required:'action_not_required',sdk_error:'card_declined'})[mode]||'')
  assert.equal(requests.some(r=>r.url.includes('/confirm')||r.url.includes('/payment_methods')),false)
  assert.equal(JSON.stringify(events).includes('pi_Fixture_secret_Test'),false)
  if(mode==='automatic')assert.equal(events.some(e=>e.type==='frame'),false)
  const stages=events.filter(e=>e.type==='progress').map(e=>e.stage)
  for(const stage of ['browser_start','page_loading','sdk_loaded','context_ready','preflight','preflight_returned','notifying'])assert.ok(stages.includes(stage),stage)
  assert.equal(stages.includes('sdk_action'),!['wrong_intent','not_required'].includes(mode))
 })
}
test('expired original proxy never starts a browser',async()=>{
 await assert.rejects(runAction({...input(),expires_at:1},()=>{throw Error('unexpected event')},{onCommand(){}}),/expired/)
})
test('SDK messages cannot inject secrets into diagnostics',()=>{
 assert.deepEqual(safeActionResult({outcome:'secret token',code:'pi_live_secret_private'}),{outcome:'page_error',code:'unknown'})
})

test('progress observation never breaks an SDK with frozen methods',{timeout:20000},async()=>{
 const events=[]
 await runAction({...input(),automatic:true},v=>events.push(v),{onCommand(){}},{route:async route=>{
  await route.fulfill({contentType:'application/javascript',body:`window.Stripe=()=>Object.freeze({retrievePaymentIntent:async()=>({paymentIntent:{id:'pi_Fixture',status:'requires_action'}}),handleNextAction:async()=>({})})`});return true
 }})
 assert.equal(events.find(e=>e.type==='result')?.outcome,'sdk_returned')
})

test('every external request needs the original-session guard; denial never falls back',{timeout:20000},async()=>{
 const events=[];let handler,guarded=0
 await runAction(input(),v=>{
  events.push(v)
  if(v.type==='guard'){guarded++;handler({type:'guard',request_id:v.request_id,allow:false})}
 },{onCommand:fn=>handler=fn},{
  route:async route=>{
   if(route.request().url()!=='https://js.stripe.com/v3/')return false
   await route.fulfill({contentType:'application/javascript',body:`window.Stripe=()=>({retrievePaymentIntent:async()=>({paymentIntent:{id:'pi_Fixture',status:'requires_action'}}),handleNextAction:async()=>{try{await fetch('https://api.stripe.com/offline-guard-fixture');throw Error('unguarded request')}catch{}return {error:{code:'api_connection_error'}}}})`});return true
  },
  ready:async page=>{
   const start=await page.locator('#start').boundingBox()
   await handler({type:'click',x:start.x+start.width/2,y:start.y+start.height/2})
  }
 })
 assert.equal(guarded,1)
 assert.equal(events.find(v=>v.type==='result')?.code,'api_connection_error')
 assert.equal(JSON.stringify(events).includes('secret_Test'),false)
})

test('closing while the official SDK is loading interrupts navigation',{timeout:15000},async()=>{
 let handler
 await assert.rejects(runAction(input(),()=>{},{onCommand:fn=>handler=fn},{
  route:async route=>{handler({type:'close'});await route.abort();return true}
 }))
})

test('automatic SDK waits for its own result; timeout never fakes an interaction',{timeout:15000},async()=>{
 const events=[];let handler
 await runAction({...input(),automatic:true,expires_at:Math.floor(Date.now()/1000)+4},v=>events.push(v),{onCommand:fn=>handler=fn},{
  route:async route=>{
   await route.fulfill({contentType:'application/javascript',body:`window.Stripe=()=>({retrievePaymentIntent:async()=>({paymentIntent:{id:'pi_Fixture',status:'requires_action'}}),handleNextAction:async()=>new Promise(resolve=>{const b=document.createElement('button');b.id='fixture-user-required';b.textContent='Offline user-required fixture';b.onclick=()=>resolve({});document.body.append(b)})})`});return true
  },
  ready:async page=>{
   await page.locator('#fixture-user-required').waitFor()
   const box=await page.locator('#fixture-user-required').boundingBox()
   await handler({type:'click',x:box.x+box.width/2,y:box.y+box.height/2})
  }
 })
 assert.equal(events.filter(e=>e.type==='timeout').length,1)
 assert.equal(events.some(e=>e.type==='result'||e.type==='frame'),false)
})

test('browser resource failures retain safe network details without payment secrets',{timeout:20000},async()=>{
 const events=[]
 await runAction({...input(),automatic:true},v=>events.push(v),{onCommand(){}},{
  route:async route=>{
   if(route.request().url()==='https://js.stripe.com/v3/'){
    await route.fulfill({contentType:'application/javascript',body:`window.Stripe=()=>({retrievePaymentIntent:async()=>({paymentIntent:{id:'pi_Fixture',status:'requires_action'}}),handleNextAction:async()=>{try{await fetch('https://api.stripe.com/v1/payment_intents/pi_Private?client_secret=PRIVATE_QUERY')}catch{}await new Promise(r=>setTimeout(r,100));return {error:{code:'api_connection_error'}}}})`})
   }else await route.abort('connectionreset')
   return true
  }
 })
 const failure=events.find(e=>e.code==='resource_failed')
 assert.equal(failure?.details.sdk_network_error,'ERR_CONNECTION_RESET')
 assert.equal(failure?.details.sdk_host,'api.stripe.com')
 assert.equal(failure?.details.sdk_request_type,'fetch')
 assert.equal(failure?.details.sdk_route,'payment_intent')
 assert.ok(failure.details.sdk_request>0)
 assert.ok(failure.details.sdk_duration_ms>=0)
 assert.equal(JSON.stringify(events).includes('PRIVATE_QUERY'),false)
 assert.equal(JSON.stringify(events).includes('pi_Private'),false)
 assert.ok(events.some(e=>e.type==='network'&&e.status===200))
})
