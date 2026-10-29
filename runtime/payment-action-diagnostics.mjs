import crypto from 'node:crypto'

const hosts=new Set(['js.stripe.com','api.stripe.com','hooks.stripe.com','m.stripe.com','q.stripe.com','r.stripe.com','m.stripe.network','b.stripecdn.com','hcaptcha.com','js.hcaptcha.com','newassets.hcaptcha.com','imgs.hcaptcha.com','api.hcaptcha.com','accounts.hcaptcha.com','challenges.cloudflare.com'])
const errors=new Set(['ERR_ABORTED','ERR_FAILED','ERR_TIMED_OUT','ERR_CONNECTION_TIMED_OUT','ERR_CONNECTION_CLOSED','ERR_CONNECTION_RESET','ERR_CONNECTION_REFUSED','ERR_NAME_NOT_RESOLVED','ERR_TUNNEL_CONNECTION_FAILED','ERR_PROXY_CONNECTION_FAILED','ERR_PROXY_AUTH_UNSUPPORTED','ERR_INVALID_AUTH_CREDENTIALS','ERR_HTTP_RESPONSE_CODE_FAILURE','ERR_HTTP2_PROTOCOL_ERROR','ERR_HTTP2_SERVER_REFUSED_STREAM','ERR_QUIC_PROTOCOL_ERROR','ERR_SSL_PROTOCOL_ERROR','ERR_CERT_AUTHORITY_INVALID','ERR_CERT_DATE_INVALID','ERR_BLOCKED_BY_CLIENT','ERR_BLOCKED_BY_RESPONSE','ERR_NETWORK_CHANGED','ERR_INTERNET_DISCONNECTED','ERR_EMPTY_RESPONSE','ERR_CONTENT_LENGTH_MISMATCH','ERR_CONTENT_DECODING_FAILED'])
export function actionNetworkError(raw){const code=String(raw||'').replace(/^net::/,'');return errors.has(code)?code:'unknown'}
export function actionResourceIdentity(raw){
  try{
    const u=new URL(raw)
    let host=hosts.has(u.hostname)?u.hostname:'other'
    if(host==='other')for(const suffix of ['stripe.com','stripe.network','stripecdn.com','hcaptcha.com','cloudflare.com'])if(u.hostname.endsWith('.'+suffix)){host='*.'+suffix;break}
    const route=u.pathname==='/v3/'?'stripe_v3':u.pathname==='/basil/stripe.js'?'stripe_basil':/^\/v1\/payment_intents\//.test(u.pathname)?'payment_intent':/^\/v1\/3ds2\//.test(u.pathname)?'three_ds':/\.(?:js|css|woff2?|png|svg|jpg|webp)$/.test(u.pathname)?'static_asset':'other'
    // Correlate repeated resources without persisting query strings, paths,
    // transaction identifiers, credentials or arbitrary upstream error text.
    return {sdk_host:host,sdk_route:route,sdk_resource_key:crypto.createHash('sha256').update(u.origin+u.pathname).digest('hex').slice(0,16)}
  }catch{return {sdk_host:'other',sdk_route:'other'}}
}

export function observeActionNetwork(context,report){
  const pending=new Map();let sequence=0,completed=0,failed=0,recorded=0
  const external=req=>{try{return new URL(req.url()).hostname!=='xupay-action.invalid'}catch{return false}}
  const timings=req=>{
    const result={}
    try{
      const t=req.timing()
      for(const [key,start,end] of [['sdk_connect_ms',t.connectStart,t.connectEnd],['sdk_tls_ms',t.secureConnectionStart,t.connectEnd],['sdk_response_ms',t.requestStart,t.responseStart]])if(start>=0&&end>=start)result[key]=Math.round(end-start)
    }catch{}
    return result
  }
  const details=(req,entry)=>({
    ...actionResourceIdentity(req.url()),sdk_request:entry.id,
    ...timings(req),
    sdk_method:['GET','POST','OPTIONS','HEAD'].includes(req.method())?req.method():'other',
    sdk_request_type:['document','script','stylesheet','image','font','xhr','fetch','other'].includes(req.resourceType())?req.resourceType():'other',
    sdk_duration_ms:Math.max(0,Date.now()-entry.at),sdk_guard_ms:entry.guard||0,
    ...(entry.error?{sdk_network_error:entry.error}:{}),
  })
  context.on('request',req=>{if(external(req))pending.set(req,{id:++sequence,at:Date.now()})})
  const finish=(req,error)=>{
    const entry=pending.get(req);if(!entry)return
    if(error){failed++;entry.error=actionNetworkError(req.failure()?.errorText)}else completed++
    if(error||recorded++<120)report(error?'resource_failed':'resource_completed',req.url(),entry.status||0,details(req,entry))
    pending.delete(req)
  }
  context.on('response',res=>{const entry=pending.get(res.request());if(entry)entry.status=res.status()})
  context.on('requestfailed',req=>finish(req,true))
  context.on('requestfinished',req=>finish(req,false))
  return {
    guard(req,elapsed){const entry=pending.get(req);if(entry)entry.guard=elapsed},
    detail(req){const entry=pending.get(req);return entry?details(req,entry):actionResourceIdentity(req.url())},
    summary(){return {sdk_requests:sequence,sdk_completed:completed,sdk_failed:failed,sdk_pending:pending.size}},
    pending(){for(const [req,entry] of [...pending].slice(0,8))report('resource_pending',req.url(),entry.status||0,details(req,entry))},
  }
}
