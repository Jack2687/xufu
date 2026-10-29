// Follow only the original checkout's provider-returned merchant landing page.
export function hostedLanding(body,id) {
  for(const raw of [body?.success_url,body?.return_url,body?.url,body?.after_completion?.redirect?.url,body?.after_completion?.url]) {
    try {
      const u=new URL(raw)
      if(u.protocol!=='https:'||u.host!=='chatgpt.com'||u.username||u.password||u.hash)continue
      const ids=u.searchParams.getAll('stripe_session_id'),entities=u.searchParams.getAll('processor_entity')
      if(u.pathname==='/checkout/verify'&&ids.length===1&&ids[0]===id&&entities.length===1&&/^[a-z0-9_]{2,64}$/.test(entities[0]))return {url:u.href,entity:entities[0]}
      const match=u.pathname.match(/^\/checkout\/([a-z0-9_]{2,64})\/(cs_(?:live|test)_[A-Za-z0-9]+)$/)
      if(match&&match[2]===id&&!u.search)return {url:u.href,entity:match[1]}
    }catch{}
  }
  return null
}

export class HostedApproval {
  constructor(id,resume=false){this.id=id;this.pending=resume;this.landing=null;this.navigated=false;this.started=false}
  observe(body,snapshot){
    if(!snapshot)return
    const landing=hostedLanding(body,this.id)
    if(landing)this.landing=landing
    if(body.submission_attempt?.state==='requires_approval'||body.submission_attempt?.status==='requires_approval')this.pending=true
  }
  matches(req){
    if(req.method()!=='POST')return false
    try {
      const u=new URL(req.url()),v=JSON.parse(req.postData()||'{}')
      return u.origin==='https://chatgpt.com'&&u.pathname==='/backend-api/payments/checkout/approve'&&v.checkout_session_id===this.id&&(!v.processor_entity||v.processor_entity===this.landing?.entity)
    }catch{return false}
  }
  ready(latest,quote){return this.pending&&this.landing&&!this.started&&latest?.open&&!latest.paid&&!latest.action&&latest.taxReady&&latest.amount===quote?.amount&&latest.currency===quote?.currency}
}
