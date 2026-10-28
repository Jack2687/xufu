import {test} from 'node:test'
import assert from 'node:assert/strict'
import {chromium} from 'playwright'
import {allowedHost,checkoutIdentity,confirmMatches,snapshot,run} from './payment-browser.mjs'

const id='cs_test_QA123'
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
 assert.deepEqual(snapshot(initial,id),{amount:579464,currency:'php',paid:false,taxReady:false,open:true,action:false})
 const taxed={...initial,tax_meta:{status:'complete'},total_summary:{due:616550}}
 assert.equal(snapshot(taxed,id).taxReady,true)
 assert.equal(snapshot(taxed,id).amount,616550)
 const paid={...taxed,state:'succeeded',status:'complete',payment_status:'paid',payment_intent:{status:'succeeded'}}
 assert.equal(snapshot(paid,id).paid,true)
 assert.equal(snapshot({...paid,session_id:'cs_test_AnotherCheckout'},id),null)
})

// Real Chromium and real DOM interactions; all provider traffic fulfilled locally.
// No card, payment or verification request ever reaches an external service.
for(const mode of ['success','cancel','price_changed','unknown','action','declined','check_paid','check_unpaid'])test('browser flow: '+mode,{timeout:20000},async()=>{
 let confirmations=0,external=0,current=body({tax:false}),resolveCommand
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
 const deps={playwright:{chromium:{launch:async options=>{
   assert.equal(options.proxy,undefined)
   const b=await chromium.launch({headless:true,executablePath:options.executablePath})
   const original=b.newContext.bind(b);b.newContext=async options=>{const ctx=await original(options);ctx.on('page',p=>pageRef=p);return ctx};return b
 }}},route:async route=>{
   const u=new URL(route.request().url()),headers={'access-control-allow-origin':'*','access-control-allow-headers':'*'}
   if(u.hostname==='pay.openai.com')return route.fulfill({status:200,contentType:'text/html',body:form})
   if(u.hostname!=='api.stripe.com'){external++;return route.abort()}
   if(u.pathname.endsWith('/init'))current=body({tax:false,paid:mode==='check_paid'})
   else if(u.pathname.endsWith('/confirm')){confirmations++;if(mode==='declined')return route.fulfill({status:402,headers,contentType:'application/json',body:JSON.stringify({error:{type:'card_error',code:'card_declined'}})});if(mode==='unknown')return route.abort();current=body({paid:mode==='success',action:mode==='action'})}
   else current=body({amount:mode==='price_changed'&&events.some(e=>e.type==='quote')?1600:1500})
   return route.fulfill({status:200,headers,contentType:'application/json',body:JSON.stringify(current)})
 }}
 const input={mode:mode.startsWith('check_')?'check':'prepare',url:'https://pay.openai.com/c/pay/'+id,email:'qa@example.com',card:{number:'4242424242424242',month:'12',year:'2035',cvc:'123'},billing:{first_name:'QA',last_name:'Test',line1:'100 Test Street',city:'Portland',state:'OR',postal_code:'97205'},prepare_seconds:3,quote_seconds:3,result_seconds:1}
 await run(input,commands,event=>{
  events.push(event)
  if(event.type==='quote')setTimeout(async()=>{
   if(mode==='price_changed'){await pageRef.evaluate(async id=>{await fetch('https://api.stripe.com/v1/payment_pages/'+id,{method:'POST'})},id);await new Promise(r=>setTimeout(r,100))}
   resolveCommand({value:{type:mode==='cancel'?'cancel':'confirm'}})
  },0)
 },deps)
 const final=events.at(-1)
 assert.equal(final.type,'result')
 const expected={success:'succeeded',cancel:'cancelled',price_changed:'failed',unknown:'review',action:'action_required',declined:'declined',check_paid:'succeeded',check_unpaid:'review'}
 assert.equal(final.state,expected[mode],JSON.stringify(events))
 assert.equal(confirmations,['success','unknown','action','declined'].includes(mode)?1:0)
 assert.equal(external,0)
 assert.equal(JSON.stringify(events).includes('4242424242424242'),false)
 assert.equal(input.card.number,undefined)
})
