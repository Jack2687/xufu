import test from 'node:test'
import assert from 'node:assert/strict'
import {EventEmitter} from 'node:events'
import {actionNetworkError,actionResourceIdentity,observeActionNetwork} from './payment-action-diagnostics.mjs'

test('resource identities never retain query, path credentials or arbitrary host tokens',()=>{
 const secret='pi_private_secret_token'
 const identity=actionResourceIdentity('https://api.stripe.com/v1/payment_intents/'+secret+'?client_secret='+secret+'#'+secret)
 assert.equal(identity.sdk_host,'api.stripe.com')
 assert.equal(identity.sdk_route,'payment_intent')
 assert.match(identity.sdk_resource_key,/^[0-9a-f]{16}$/)
 assert.equal(JSON.stringify(identity).includes(secret),false)
 assert.equal(actionResourceIdentity('https://private-token.hcaptcha.com/a').sdk_host,'*.hcaptcha.com')
 assert.equal(actionResourceIdentity('https://secret.example/a').sdk_host,'other')
 assert.equal(actionNetworkError('net::ERR_TUNNEL_CONNECTION_FAILED'),'ERR_TUNNEL_CONNECTION_FAILED')
 assert.equal(actionNetworkError('net::ERR_FAILED '+secret),'unknown')
})

test('network diagnostics distinguish pending, failure and successful responses with request identity',()=>{
 const context=new EventEmitter(),records=[]
 const observer=observeActionNetwork(context,(code,url,status,details)=>records.push({code,status,details}))
 const req={url:()=> 'https://api.stripe.com/v1/payment_intents/pi_Private?client_secret=PRIVATE',method:()=> 'GET',resourceType:()=> 'fetch',failure:()=>({errorText:'net::ERR_CONNECTION_RESET'}),timing:()=>({connectStart:1,connectEnd:101,secureConnectionStart:40,requestStart:105,responseStart:255})}
 context.emit('request',req);observer.guard(req,42);observer.pending()
 assert.equal(records[0].code,'resource_pending')
 assert.equal(records[0].details.sdk_guard_ms,42)
 assert.equal(records[0].details.sdk_connect_ms,100)
 assert.equal(records[0].details.sdk_tls_ms,61)
 assert.equal(records[0].details.sdk_response_ms,150)
 assert.equal(observer.summary().sdk_pending,1)
 context.emit('requestfailed',req)
 assert.equal(records[1].details.sdk_network_error,'ERR_CONNECTION_RESET')
 assert.equal(records[1].details.sdk_request,records[0].details.sdk_request)
 context.emit('request',req)
 context.emit('response',{request:()=>req,status:()=>200})
 context.emit('requestfinished',req)
 assert.equal(records[2].code,'resource_completed')
 assert.equal(records[2].status,200)
 assert.deepEqual(observer.summary(),{sdk_requests:2,sdk_completed:1,sdk_failed:1,sdk_pending:0})
 assert.equal(JSON.stringify(records).includes('PRIVATE'),false)
})
