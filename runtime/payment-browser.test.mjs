import {test} from 'node:test'
import assert from 'node:assert/strict'
import {chromium} from 'playwright'
import {allowedHost,checkoutIdentity,confirmMatches,snapshot,run,paymentProxy,paymentDiagnostics} from './payment-browser.mjs'

const id='cs_test_QA123'
test('hosted diagnostics distinguish pending approval and SDK action without credentials',()=>{
 const result=paymentDiagnostics({status:'open',payment_status:'unpaid',customer_email:'PRIVATE_EMAIL',submission_attempt:{state:'requires_approval',payment_intent:{id:'pi_PRIVATE',status:'requires_action',client_secret:'PRIVATE_SECRET',next_action:{type:'use_stripe_sdk',use_stripe_sdk:{type:'intent_confirmation_challenge',stripe_js:'https://private.example/SECRET'}}}},error:{payment_intent:{status:'requires_payment_method',last_payment_error:{code:'card_declined',decline_code:'insufficient_funds',message:'PRIVATE'}}}})
 assert.equal(result.find(x=>x.diagnostic_source==='submission_attempt').raw_state,'requires_approval')
 assert.deepEqual(result.find(x=>x.diagnostic_source==='submission_payment_intent'),{diagnostic_source:'submission_payment_intent',raw_status:'requires_action',next_action_type:'use_stripe_sdk',sdk_action_type:'intent_confirmation_challenge',sdk_has_stripe_js:true,sdk_has_3ds_source:false})
 assert.equal(result.find(x=>x.diagnostic_source==='error_payment_intent').decline_code,'insufficient_funds')
 assert.equal(JSON.stringify(result).includes('PRIVATE'),false)
 assert.deepEqual(paymentDiagnostics({status:'PRIVATE_FREE_TEXT'}),[{diagnostic_source:'response',raw_status:'redacted'}])
})
function body({paid=false,tax=true,amount=1500,action=false}={}){return {id:'ppage_QA123',object:'checkout.session',session_id:id,currency:'usd',total_summary:{due:amount},state:paid?'succeeded':'active',status:paid?'complete':'open',payment_status:paid?'paid':'unpaid',tax_context:{automatic_tax_enabled:true},tax_meta:{status:tax?'complete':'requires_location_inputs'},payment_intent:paid?{status:'succeeded'}:action?{status:'requires_action'}:null}}
test('payment identity, quote and result require current session and final tax',()=>{
 assert.equal(checkoutIdentity('https://pay.openai.com/c/pay/'+id),id)
 for(const u of ['http://pay.openai.com/c/pay/'+id,'https://pay.openai.com.evil.example/c/pay/'+id,'https://user:pw@pay.openai.com/c/pay/'+id])assert.throws(()=>checkoutIdentity(u))
 assert.equal(snapshot(body({tax:false}),id).taxReady,false)
 assert.equal(snapshot(body({paid:true}),id).paid,true)
 assert.equal(snapshot({...body({paid:true}),payment_intent:{status:'processing'}},id).paid,false)
 assert.equal(snapshot({...body(),id:'cs_test_wrong'},id),null)
 assert.equal(snapshot({...body(),session_id:'cs_test_wrong'},id),null)
 assert.equal(snapshot({...body(),id,session_id:'cs_test_wrong'},id),null)
 assert.equal(snapshot({...body(),session_id:undefined},id),null)
 assert.equal(snapshot({...body(),object:'payment_intent'},id),null)
 assert.equal(snapshot({...body(),id:null},id),null)
 assert.equal(snapshot(body({amount:1.2}),id),null)
 assert.equal(confirmMatches('https://api.stripe.com/v1/payment_pages/'+id+'/confirm',new URLSearchParams({expected_amount:'1500'}),id,{amount:1500}),true)
 assert.equal(confirmMatches('https://api.stripe.com/v1/payment_pages/'+id+'/confirm',new URLSearchParams({expected_amount:'1501'}),id,{amount:1500}),false)
 assert.equal(allowedHost('api.stripe.com.evil.example'),false);assert.equal(allowedHost('127.0.0.1'),false)
})

test('hosted init, tax update and paid confirm use checkout session_id, never page id',()=>{
 const initial=body({tax:false,amount:579464})
 initial.currency='php'
 assert.deepEqual(snapshot(initial,id),{amount:579464,currency:'php',paid:false,taxReady:false,open:true,action:false,authorized:false,evidence:{checkout_id:id,payment_intent_id:'',status:'pending',amount_received:null,currency:'php'}})
 const taxed={...initial,tax_meta:{status:'complete'},total_summary:{due:616550}}
 assert.equal(snapshot(taxed,id).taxReady,true)
 assert.equal(snapshot(taxed,id).amount,616550)
 const paid={...taxed,state:'succeeded',status:'complete',payment_status:'paid',payment_intent:{status:'succeeded'}}
 assert.equal(snapshot(paid,id).paid,true)
 assert.equal(snapshot({...paid,session_id:'cs_test_AnotherCheckout'},id),null)
})

// Real Chromium and real DOM interactions; all provider traffic fulfilled locally.
// No card, payment or verification request ever reaches an external service.
for(const mode of ['success','cancel','price_changed','unknown','action','declined','check_paid','check_unpaid','ip_changed','session_expired','probe_retry','probe_failed','retry_ip_changed','probe_expired'])test('browser flow: '+mode,{timeout:20000},async()=>{
 let probes=0,confirmations=0,external=0,current=body({tax:false}),resolveCommand
 const events=[],commands={next:()=>new Promise(resolve=>{resolveCommand=resolve})}
 let pageRef
 const form=`<!doctype html><html><body>
 <input id="cardNumber"><input id="cardExpiry"><input id="cardCvc"><input id="billingName">
 <select id="billingCountry"><option value="US">US</option></select><input id="billingAddressLine1"><input id="billingLocality">
 <select id="billingAdministrativeArea"><option value="OR">OR</option></select><input id="billingPostalCode">
 <button class="SubmitButton" type="button">Pay</button><script>
 const base='https://api.stripe.com/v1/payment_pages/${id}';
 fetch(base+'/init',{method:'POST'});
 document.querySelector('#billingPostalCode').addEventListener('blur',()=>fetch(base,{method:'POST'}));
 document.querySelector('button').onclick=()=>fetch(base+'/confirm',{method:'POST',body:new URLSearchParams({expected_amount:'1500'})});
 </script></body></html>`
 const deps={exitIP:async()=>{
   probes++
   if(mode==='probe_expired'&&probes===2){input.proxy_expires_at=Date.now()-1;throw Error('PRIVATE_PROXY timeout')}
   if((['probe_retry','retry_ip_changed'].includes(mode)&&probes===2)||(mode==='probe_failed'&&probes>1))throw Error('PRIVATE_PROXY tunnel failed')
   return mode==='ip_changed'&&probes>1||mode==='retry_ip_changed'&&probes>2?'203.0.113.3':'203.0.113.2'
 },playwright:{chromium:{launch:async options=>{
   assert.equal(options.proxy.server,'http://127.0.0.1:18799');assert.equal(options.proxy.username,'order')
   const b=await chromium.launch({headless:true,executablePath:options.executablePath})
   const original=b.newContext.bind(b);b.newContext=async options=>{assert.equal(options.proxy.server,'http://127.0.0.1:18799');const ctx=await original({...options,proxy:undefined});ctx.on('page',p=>pageRef=p);return ctx};return b
 }}},route:async route=>{
   const u=new URL(route.request().url()),headers={'access-control-allow-origin':'*','access-control-allow-headers':'*'}
   if(u.hostname==='pay.openai.com')return route.fulfill({status:200,contentType:'text/html',body:form})
   if(u.hostname!=='api.stripe.com'){external++;return route.abort()}
   if(u.pathname.endsWith('/init'))current=body({tax:false,paid:mode==='check_paid'})
   else if(u.pathname.endsWith('/confirm')){confirmations++;if(mode==='declined')return route.fulfill({status:402,headers,contentType:'application/json',body:JSON.stringify({error:{type:'card_error',code:'card_declined'}})});if(mode==='unknown')return route.abort();current=body({paid:['success','probe_retry'].includes(mode),action:mode==='action'})}
   else current=body({amount:mode==='price_changed'&&events.some(e=>e.type==='quote')?1600:1500})
   return route.fulfill({status:200,headers,contentType:'application/json',body:JSON.stringify(current)})
 }}
 const input={proxy:{server:'http://127.0.0.1:18799',username:'order',password:'TEST_PRIVATE_PROXY'},expected_ip:'203.0.113.2',proxy_expires_at:Date.now()+60000,mode:mode.startsWith('check_')?'check':'prepare',url:'https://pay.openai.com/c/pay/'+id,email:'qa@example.com',card:{number:'4242424242424242',month:'12',year:'2035',cvc:'123'},billing:{first_name:'QA',last_name:'Test',line1:'100 Test Street',city:'Portland',state:'OR',postal_code:'97205'},prepare_seconds:3,quote_seconds:3,result_seconds:1}
 await run(input,commands,event=>{
  events.push(event)
  if(event.type==='quote'&&mode==='session_expired')input.proxy_expires_at=Date.now()-1
  if(event.type==='quote')setTimeout(async()=>{
   if(mode==='price_changed'){await pageRef.evaluate(async id=>{await fetch('https://api.stripe.com/v1/payment_pages/'+id,{method:'POST'})},id);await new Promise(r=>setTimeout(r,100))}
   resolveCommand({value:{type:mode==='cancel'?'cancel':'confirm'}})
  },0)
 },deps)
 const final=events.at(-1)
 assert.equal(final.type,'result')
 const expected={success:'succeeded',cancel:'cancelled',price_changed:'failed',unknown:'review',action:'action_required',declined:'declined',check_paid:'succeeded',check_unpaid:'review',ip_changed:'failed',session_expired:'failed',probe_retry:'succeeded',probe_failed:'failed',retry_ip_changed:'failed',probe_expired:'failed'}
 assert.equal(final.state,expected[mode],JSON.stringify(events))
 assert.equal(confirmations,['success','probe_retry','unknown','action','declined'].includes(mode)?1:0)
 assert.equal(external,0)
 assert.equal(JSON.stringify(events).includes('4242424242424242'),false)
 assert.equal(input.card.number,undefined)
 assert.equal(JSON.stringify(events).includes('TEST_PRIVATE_PROXY'),false)
 if(mode==='probe_retry'){assert.equal(probes,3);assert.equal(events.filter(e=>e.type==='submitted').length,1)}
 if(mode==='probe_failed'){assert.equal(probes,4);assert.equal(final.code,'proxy_session_unavailable')}
 if(mode==='retry_ip_changed'){assert.equal(probes,3);assert.equal(final.code,'proxy_session_changed')}
 if(mode==='probe_expired'){assert.equal(probes,2);assert.equal(final.code,'proxy_session_expired')}
 assert.equal(JSON.stringify(events).includes('PRIVATE_PROXY tunnel'),false)
 if(mode==='ip_changed')assert.equal(final.code,'proxy_session_changed')
 if(mode==='session_expired')assert.equal(final.code,'proxy_session_expired')
})

test('proxy input fails closed without pinned session',()=>{for(const input of [{},{proxy:{server:'http://example.com:80',username:'u',password:'p'},expected_ip:'203.0.113.2',proxy_expires_at:Date.now()+60000},{proxy:{server:'http://127.0.0.1:1',username:'u',password:'p'},expected_ip:'203.0.113.2',proxy_expires_at:0}])assert.throws(()=>paymentProxy(input))})

 test('intent evidence is bound to the checkout, currency and captured amount',()=>{
 const base=body({paid:false});const pi={id:'pi_QA123',status:'succeeded',currency:'usd',amount_received:1500}
 assert.equal(snapshot({...base,payment_intent:pi},id).paid,true)
 assert.equal(snapshot({...base,payment_intent:{...pi,status:'requires_capture'}},id).paid,false)
 assert.equal(snapshot({...base,payment_intent:{...pi,status:'requires_capture'}},id).authorized,true)
 assert.equal(snapshot({...base,payment_intent:{...pi,id:'pi_other_secret_private'}},id).paid,false)
 assert.equal(snapshot({...base,session_id:'cs_test_other',payment_intent:pi},id),null)
 assert.equal(snapshot({...base,payment_intent:{...pi,currency:'php'}},id),null)
 assert.equal(snapshot({...base,payment_intent:{...pi,amount_received:1.5}},id).paid,false)
 const evidence=snapshot({...base,payment_intent:{...pi,client_secret:'PRIVATE_SECRET',billing_details:{name:'PRIVATE_NAME'}}},id).evidence
 assert.equal(JSON.stringify(evidence).includes('PRIVATE'),false)
 })
