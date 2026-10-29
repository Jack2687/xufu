import {actionNetworkError} from './payment-action-diagnostics.mjs'

const hosts=new Set(['chatgpt.com','cdn.oaistatic.com','tcr9i.chat.openai.com','auth.openai.com','auth0.openai.com','sentinel.openai.com','challenges.cloudflare.com'])
const resources=new Set(['document','script','stylesheet','xhr','fetch','image','font'])
export function pageIdentity(raw) {
  try {
    const u=new URL(raw)
    const route=u.hostname!=='chatgpt.com'?'other':u.pathname==='/checkout/verify'?'merchant_return':u.pathname==='/api/auth/session'?'auth_session':/^\/(?:auth|api\/auth|login)(?:\/|$)/.test(u.pathname)?'login':u.pathname.startsWith('/checkout/')?'checkout':'other'
    return {page_host:hosts.has(u.hostname)?u.hostname:'other',page_route:route}
  }catch{return {page_host:'other',page_route:'other'}}
}

// Only fixed categories leave the private browser. Never inspect console text,
// page HTML, URLs in errors, headers, cookies or account fields for diagnostics.
export function observeMerchantPage(context,active,emit) {
  let recorded=0,closed=false
  const seen=new Set(),status=new WeakMap(),pending=new Set()
  const report=(event,identity={},extra={},http=0)=>{
    if(closed||!active())return
    const diagnostics={page_event:event,...identity,...extra}
    const key=JSON.stringify([diagnostics,http])
    if(seen.has(key)||recorded>=48)return
    seen.add(key);recorded++
    emit({type:'diagnostic',phase:'checking',http_status:http,code:http>=400?'request_rejected':'',diagnostics})
  }
  const details=req=>({...pageIdentity(req.url()),page_resource:resources.has(req.resourceType())?req.resourceType():'other'})
  const relevant=req=>active()&&(!['api.stripe.com','js.stripe.com'].includes(new URL(req.url()).hostname))
  context.on('page',page=>{
    page.on('framenavigated',frame=>{if(frame===page.mainFrame())report('navigated',pageIdentity(frame.url()))})
    page.on('domcontentloaded',()=>report('dom_ready',pageIdentity(page.url())))
    page.on('pageerror',()=>report('script_error',pageIdentity(page.url())))
  })
  context.on('request',req=>{if(relevant(req))pending.add(req)})
  context.on('response',res=>{
    const req=res.request()
    if(!pending.has(req))return
    status.set(req,res.status())
    if(req.isNavigationRequest()||res.status()>=400)report('response',details(req),{},res.status())
    if(pageIdentity(req.url()).page_route==='auth_session'&&res.status()===200) {
      // Reading the official auth response never causes a refresh or a write.
      let timer
      Promise.race([res.json(),new Promise(resolve=>{timer=setTimeout(()=>resolve(null),1500)})])
        .then(body=>report('auth_observed',details(req),{page_auth:body&&typeof body==='object'?(body.user&&body.accessToken?'present':'missing'):'unknown'},res.status()))
        .catch(()=>report('auth_observed',details(req),{page_auth:'unknown'},res.status()))
        .finally(()=>clearTimeout(timer))
    }
  })
  context.on('requestfinished',req=>{
    if(!pending.delete(req))return
    if(req.resourceType()==='script')report('resource_loaded',details(req),{},status.get(req)||0)
  })
  context.on('requestfailed',req=>{
    if(!pending.delete(req))return
    report('request_failed',details(req),{page_network_error:actionNetworkError(req.failure()?.errorText)},status.get(req)||0)
  })
  return {
    blocked(req){report('request_blocked',details(req))},
    finish(started){
      if(!closed){
        // Preserve the result even when a noisy page consumes its event budget.
        recorded=Math.min(recorded,47)
        report(started?'approval_observed':'approval_not_requested')
        closed=true;pending.clear()
      }
    },
  }
}
