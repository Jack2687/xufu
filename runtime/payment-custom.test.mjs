import {test} from 'node:test'
import assert from 'node:assert/strict'
import {chromium} from 'playwright'
import {customIdentity,loginCookies,CustomPayment} from './payment-custom.mjs'
import {run,snapshot} from './payment-browser.mjs'

const endpoint='https://chatgpt.com/backend-api/payments/checkout',id='oaics_Test',secret='pi_Test_secret_Private'
const req=(url,data={},method='POST')=>({url:()=>url,postData:()=>JSON.stringify(data),method:()=>method})
const quote={amount:1500,currency:'usd'},latest={...quote,taxReady:true}
test('custom browser binds each stage to the checkout and original intent',()=>{
 const flow=new CustomPayment({entity:'openai_llc',id})
 assert.equal(customIdentity('https://chatgpt.com/checkout/openai_llc/'+id).id,id)
 assert.equal(customIdentity('https://chatgpt.com/checkout/openai_llc/'+id+'?other=true'),null)
 const confirm=req(endpoint+'/confirm',{checkout_session_id:id,confirm_token:'ctoken_Test'})
 assert.equal(flow.guard(confirm,'check',true,quote,latest).ok,false)
 assert.equal(flow.guard(confirm,'prepare',false,quote,latest).ok,false)
 assert.equal(flow.guard(req(endpoint+'/confirm',{checkout_session_id:'oaics_Other'}),'prepare',true,quote,latest).ok,false)
 assert.equal(flow.guard(confirm,'prepare',true,quote,latest).submitted,true)
 assert.equal(flow.guard(confirm,'prepare',true,quote,latest).ok,false)
 flow.observe(confirm,{client_secret:secret},200,snapshot)
 assert.equal(flow.intent,'pi_Test')
 const intent=req('https://api.stripe.com/v1/payment_intents/pi_Test/confirm',{client_secret:secret})
 assert.equal(flow.guard(intent,'prepare',true,quote,{...latest,amount:1600}).ok,false)
 assert.equal(flow.guard(intent,'prepare',true,quote,latest).ok,true)
 assert.equal(flow.guard(intent,'prepare',true,quote,latest).ok,false)
 assert.equal(flow.guard(req(endpoint+'/approve',{checkout_session_id:id}),'prepare',true,quote,latest).ok,false)
 const unrelated={object:'payment_intent',id:'pi_Other',currency:'usd',amount_received:1500,status:'succeeded'}
 assert.equal(flow.observe(req('https://api.stripe.com/v1/payment_intents/pi_Other',{},'GET'),unrelated,200,snapshot),null)
 const evidence=flow.observe(req('https://api.stripe.com/v1/payment_intents/pi_Test',{},'GET'),{...unrelated,id:'pi_Test'},200,snapshot)
 assert.equal(evidence.paid,true)
 assert.equal(evidence.evidence.checkout_id,id)
 assert.equal(JSON.stringify(evidence).includes('secret'),false)
 const cookies=loginCookies('__Secure-next-auth.session-token=PRIVATE; unrelated=ignored')
 assert.equal(cookies.length,1);assert.equal(cookies[0].url,'https://chatgpt.com/')
 assert.throws(()=>loginCookies('unrelated=ignored'))
})

for(const variant of ['oaics_success','oaics_unknown','oaics_duplicate','oaics_check','cs_success','cs_approve','cs_check'])test('official short page in Chromium: '+variant,{timeout:20000},async()=>{
 const cs=variant.startsWith('cs_'),checkout=cs?'cs_live_Test':id,pageURL=`https://chatgpt.com/checkout/openai_llc/${checkout}`
 let resolveCommand,confirms=0,intents=0,approves=0,probes=0,pageRef
 const events=[],commands={next:()=>new Promise(r=>resolveCommand=r)}
 const form=`<!doctype html><input name="number"><input name="expiry"><input name="cvc"><input name="name"><select name="country"><option>US</option></select><input name="addressLine1"><input name="locality"><select name="administrativeArea"><option>OR</option></select><input name="postalCode"><button type="submit">Pay</button><script>
 const id=${JSON.stringify(checkout)},base='https://api.stripe.com/v1/payment_pages/'+id,root=${JSON.stringify(endpoint)};
 const json=(u,b)=>fetch(u,{method:'POST',headers:{'Content-Type':'application/json'},body:JSON.stringify(b)});
 ${cs?"fetch(base+'/init',{method:'POST'});":"json(root+'/snapshot',{checkout_session_id:id}).then(()=>fetch('https://api.stripe.com/v1/payment_intents/pi_Test'));"}
 document.querySelector('[name="postalCode"]').onblur=()=>${cs?"fetch(base,{method:'POST'})":"json(root+'/taxes',{checkout_session_id:id,processor_entity:'openai_llc'})"};
 document.querySelector('button').onclick=async()=>{
 ${cs?`await fetch(base+'/confirm',{method:'POST',body:new URLSearchParams({expected_amount:'1500'})});${variant==='cs_approve'?"await json(root+'/approve',{checkout_session_id:id,processor_entity:'openai_llc'});await fetch(base);":''}`:`await json(root+'/confirm',{checkout_session_id:id,confirm_token:'ctoken_Test'}); await fetch('https://api.stripe.com/v1/payment_intents/pi_Test/confirm',{method:'POST',body:new URLSearchParams({client_secret:'${secret}'})});${variant==='oaics_duplicate'?"await json(root+'/confirm',{checkout_session_id:id,confirm_token:'ctoken_Test'});":''}`}
 };</script>`
 const pageBody=(paid=false,tax=true,approval=false)=>({id:'ppage_Test',object:'checkout.session',session_id:checkout,currency:'usd',total_summary:{due:1500},status:paid?'complete':'open',state:paid?'succeeded':'active',payment_status:paid?'paid':'unpaid',tax_context:{automatic_tax_enabled:true},tax_meta:{status:tax?'complete':'requires_location_inputs'},submission_attempt:{state:approval?'requires_approval':'pending'}})
 const deps={exitIP:async()=>{probes++;return '203.0.113.2'},playwright:{chromium:{launch:async options=>{
 const b=await chromium.launch({headless:true,executablePath:options.executablePath})
 const create=b.newContext.bind(b);b.newContext=async options=>{const c=await create({...options,proxy:undefined});c.on('page',p=>pageRef=p);return c};return b
 }}},route:async route=>{
 const r=route.request(),u=new URL(r.url()),headers={'access-control-allow-origin':'*','access-control-allow-headers':'*'}
 const fulfill=body=>route.fulfill({status:200,headers,contentType:'application/json',body:JSON.stringify(body)})
 if(r.method()==='OPTIONS')return fulfill({})
 if(r.url()===pageURL){assert.match(r.headers().cookie||'',/TEST_PRIVATE_SESSION/);return route.fulfill({status:200,contentType:'text/html',body:form})}
 if(u.hostname==='api.stripe.com')assert.equal((r.headers().cookie||'').includes('TEST_PRIVATE_SESSION'),false)
 if(r.url()===endpoint+'/taxes')return fulfill({checkout_session:{amount_total:1500,currency:'usd'}})
 if(r.url()===endpoint+'/snapshot')return fulfill({client_secret:secret})
 if(r.url()===endpoint+'/confirm'){confirms++;return fulfill({client_secret:secret})}
 if(r.url()===endpoint+'/approve'){approves++;return fulfill({result:'approved'})}
 if(u.pathname===`/v1/payment_intents/pi_Test/confirm`){intents++;if(variant==='oaics_unknown')return route.abort();return fulfill({object:'payment_intent',id:'pi_Test',currency:'usd',amount:1500,amount_received:1500,status:'succeeded'})}
 if(u.pathname===`/v1/payment_intents/pi_Test`)return fulfill({object:'payment_intent',id:'pi_Test',currency:'usd',amount:1500,amount_received:variant==='oaics_check'?1500:0,status:variant==='oaics_check'?'succeeded':'requires_confirmation'})
 if(u.pathname===`/v1/payment_pages/${checkout}/confirm`){confirms++;return fulfill(pageBody(variant==='cs_success',true,variant==='cs_approve'))}
 if(u.pathname===`/v1/payment_pages/${checkout}/init`)return fulfill(pageBody(variant==='cs_check',false))
 if(u.pathname===`/v1/payment_pages/${checkout}`)return fulfill(pageBody(approves>0))
 assert.fail('unexpected network request '+u.hostname+u.pathname)
 }}
 const input={url:pageURL,session_cookies:'__Secure-next-auth.session-token=TEST_PRIVATE_SESSION',proxy:{server:'http://127.0.0.1:18799',username:'order',password:'PRIVATE_PROXY'},expected_ip:'203.0.113.2',proxy_expires_at:Date.now()+60000,mode:variant.endsWith('_check')?'check':'prepare',email:'qa@example.com',card:{number:'4242424242424242',month:'12',year:'2035',cvc:'123'},billing:{first_name:'QA',last_name:'Test',line1:'100 Test Street',city:'Portland',state:'OR',postal_code:'97205'},prepare_seconds:3,quote_seconds:3,result_seconds:1}
 await run(input,commands,e=>{events.push(e);if(e.type==='quote')setTimeout(()=>resolveCommand({value:{type:'confirm'}}),0)},deps)
 const result=events.at(-1)
 assert.equal(result.state,variant==='oaics_unknown'?'review':'succeeded',JSON.stringify(events))
 assert.equal(confirms,variant.endsWith('_check')?0:1)
 assert.equal(intents,!cs&&!variant.endsWith('_check')?1:0)
 assert.equal(approves,variant==='cs_approve'?1:0)
 assert.equal(JSON.stringify(events).includes('PRIVATE'),false)
 assert.equal(input.session_cookies,'')
 assert.ok(probes>=1)
})
