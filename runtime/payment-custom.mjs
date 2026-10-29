// Observe the official page; never synthesize a payment or verification request.
export function customIdentity(raw) {
  const u=new URL(raw)
  if(u.protocol!=='https:'||u.host!=='chatgpt.com'||u.username||u.password||u.search||u.hash)return null
  const m=u.pathname.match(/^\/checkout\/([a-z0-9_]{2,64})\/((?:oaics_|cs_live_)[A-Za-z0-9]+)$/)
  return m?{entity:m[1],id:m[2]}:null
}

export function loginCookies(raw) {
  const cookies=[]
  for(const pair of (raw||'').split(';')) {
    const i=pair.indexOf('='),name=pair.slice(0,i).trim(),value=pair.slice(i+1).trim()
    if(i>0&&/^__Secure-(?:next-auth|platform)\.session-token(?:\.\d{1,2})?$/.test(name)&&value&&!/[\s;,]/.test(value))
      cookies.push({name,value,url:'https://chatgpt.com/',secure:true,httpOnly:true,sameSite:'Lax'})
  }
  if(!cookies.length)throw Error('invalid_session')
  return cookies
}

function requestBody(req) {
  try {return JSON.parse(req.postData()||'{}')}catch {return Object.fromEntries(new URLSearchParams(req.postData()||''))}
}
const amounts=new Set(['usd','php','gbp','jpy','sgd','egp','try'])
const validAmount=n=>Number.isSafeInteger(n)&&n>0&&n<=10000000000
const root='/backend-api/payments/checkout'

export class CustomPayment {
  constructor(identity) {this.identity=identity;this.steps=new Set();this.intent='';this.secret='';this.latest=null;this.approval=false}
  matches(body) {return body?.checkout_session_id===this.identity.id&&(!body.processor_entity||body.processor_entity===this.identity.entity)}
  // Called before forwarding any request. Each charge/approval step runs once,
  // only for this checkout, after Go has durably authorized the quote.
  guard(req,mode,permit,quote,latest) {
    const u=new URL(req.url()),write=!['GET','HEAD','OPTIONS'].includes(req.method()),data=requestBody(req)
    if(!write)return {ok:true}
    let step=''
    if(u.hostname==='chatgpt.com'&&u.pathname.startsWith('/backend-api/payments/')) {
      if([root+'/taxes',root+'/snapshot',root+'/update'].includes(u.pathname))return {ok:this.matches(data)}
      if(u.pathname===root+'/confirm'&&this.identity.id.startsWith('oaics_')&&this.matches(data))step='checkout'
      else if(u.pathname===root+'/approve'&&this.matches(data)&&this.approval&&this.steps.has('page'))step='approve'
      else return {ok:false}
    } else if(u.hostname==='api.stripe.com'&&/\/(confirm|pay|capture)$/.test(u.pathname)) {
      if(u.pathname===`/v1/payment_pages/${this.identity.id}/confirm`&&data.expected_amount===String(quote?.amount))step='page'
      else if(this.intent&&u.pathname===`/v1/payment_intents/${this.intent}/confirm`&&data.client_secret===this.secret&&this.steps.has('checkout'))step='intent'
      else return {ok:false}
    } else if(/\/(confirm|pay|capture|approve)$/.test(u.pathname))return {ok:false}
    if(!step)return {ok:true}
    if(mode==='check'||!permit||this.steps.has(step)||!quote||!latest?.taxReady||latest.amount!==quote.amount||latest.currency!==quote.currency)return {ok:false}
    // A page confirmation and an oaics confirmation must never both charge.
    if(step==='page'&&this.steps.has('checkout')||step==='checkout'&&this.steps.has('page'))return {ok:false}
    this.steps.add(step)
    return {ok:true,submitted:true}
  }
  bind(data) {
    const secret=data?.client_secret||data?.payment_intent?.client_secret||data?.stripe_payment_intent?.client_secret||data?.checkout_session?.client_secret
    if(typeof secret!=='string'||!/^pi_[A-Za-z0-9]+_secret_[A-Za-z0-9]+$/.test(secret))return
    const id=secret.split('_secret_')[0]
    if(this.intent&&this.intent!==id)return
    this.intent=id;this.secret=secret
  }
  observe(req,body,status,hostedSnapshot) {
    if(status!==200||!body)return null
    const u=new URL(req.url()),data=requestBody(req),id=this.identity.id
    if(u.hostname==='chatgpt.com'&&this.matches(data)) {
      if(u.pathname===root+'/taxes') {
        const s=body.checkout_session,currency=s?.currency?.toLowerCase()
        if(s&&(!s.id||s.id===id)&&(!s.checkout_session_id||s.checkout_session_id===id)&&validAmount(s.amount_total)&&amounts.has(currency))
          this.latest={amount:s.amount_total,currency,taxReady:true,open:true,paid:false,action:false,authorized:false,evidence:{checkout_id:id,payment_intent_id:'',status:'pending',amount_received:null,currency}}
      }
      if([root+'/confirm',root+'/snapshot'].includes(u.pathname))this.bind(body)
    }
    if(u.hostname==='api.stripe.com'&&(u.pathname===`/v1/payment_pages/${id}`||u.pathname.startsWith(`/v1/payment_pages/${id}/`))) {
      const s=hostedSnapshot(body,id)
      if(s){this.latest=s;this.approval=['requires_approval','requires_action'].includes(body.submission_attempt?.state||body.submission_attempt?.status)}
    }
    if(u.hostname==='api.stripe.com'&&this.intent&&(u.pathname===`/v1/payment_intents/${this.intent}`||u.pathname===`/v1/payment_intents/${this.intent}/confirm`)&&body.object==='payment_intent'&&body.id===this.intent) {
      const currency=body.currency,received=body.status==='succeeded'&&validAmount(body.amount_received)?body.amount_received:null
      const amount=received??body.amount
      if(!validAmount(amount)||!amounts.has(currency)||this.latest&&currency!==this.latest.currency)return null
      this.latest={amount,currency,taxReady:true,open:false,paid:received!==null,action:body.status==='requires_action',authorized:body.status==='requires_capture',evidence:{checkout_id:id,payment_intent_id:this.intent,status:received!==null?'succeeded':body.status==='requires_action'?'action_required':body.status==='requires_capture'?'authorized':'pending',amount_received:received,currency}}
    }
    return this.latest
  }
}
