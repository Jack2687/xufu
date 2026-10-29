import {test} from 'node:test'
import assert from 'node:assert/strict'
import {EventEmitter} from 'node:events'
import {observeMerchantPage,pageIdentity} from './payment-page-diagnostics.mjs'

test('merchant diagnostics redact identities, raw errors and bound noisy pages',()=>{
  assert.deepEqual(pageIdentity('https://chatgpt.com/checkout/verify?stripe_session_id=PRIVATE'),{page_host:'chatgpt.com',page_route:'merchant_return'})
  assert.deepEqual(pageIdentity('https://PRIVATE.example/PRIVATE'),{page_host:'other',page_route:'other'})
  const context=new EventEmitter(),events=[]
  let active=false
  const observer=observeMerchantPage(context,()=>active,e=>events.push(e))
  const request=(host='chatgpt.com',type='script')=>({url:()=>`https://${host}/PRIVATE?secret=PRIVATE`,resourceType:()=>type,failure:()=>({errorText:'PRIVATE_SECRET'})})
  observer.blocked(request());assert.equal(events.length,0)
  active=true
  for(let i=0;i<500;i++) {
    const req=request(['chatgpt.com','cdn.oaistatic.com','sentinel.openai.com','auth.openai.com','PRIVATE.example'][i%5],['script','document','fetch','image','font','xhr','stylesheet'][i%7])
    context.emit('request',req);observer.blocked(req);context.emit('requestfailed',req)
  }
  observer.finish(false)
  assert.ok(events.length<=49)
  assert.equal(events.at(-1).diagnostics.page_event,'approval_not_requested')
  assert.ok(events.some(e=>e.diagnostics.page_network_error==='unknown'))
  assert.equal(JSON.stringify(events).includes('PRIVATE'),false)
  const count=events.length
  observer.blocked(request());observer.finish(true)
  assert.equal(events.length,count)
})
