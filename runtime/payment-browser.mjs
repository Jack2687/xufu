// Private, single-use browser process. stdin/stdout are an anonymous pipe to Go.
// Never record HAR, screenshots, storage state, card data, console or raw errors.
import readline from 'node:readline'
import {pathToFileURL} from 'node:url'
import fs from 'node:fs'

const currencies = new Set(['php','usd','jpy','sgd','gbp','egp','try'])
export function checkoutIdentity(raw) {
  const u = new URL(raw)
  if (u.protocol !== 'https:' || u.username || u.password || u.port || !['pay.openai.com','checkout.stripe.com'].includes(u.hostname)) throw Error('unsupported_checkout')
  const m = u.pathname.match(/^\/(?:c\/)?pay\/(cs_(?:live|test)_[a-zA-Z0-9]+)$/)
  if (!m) throw Error('unsupported_checkout')
  return m[1]
}
export function allowedHost(host) {
  return ['pay.openai.com','chatgpt.com','api.stripe.com','js.stripe.com','stripe.com','stripe.network','b.stripecdn.com','hcaptcha.com'].some(h=>host===h||(['stripe.com','stripe.network','hcaptcha.com'].includes(h)&&host.endsWith('.'+h)))
}
export function snapshot(body, id) {
  // Hosted payment_pages responses identify the page with ppage_ and bind it
  // to the checkout through session_id. Never compare the page ID to cs_.
  if (!body || body.object !== 'checkout.session' || body.session_id !== id || typeof body.id !== 'string' || !/^ppage_[a-zA-Z0-9]+$/.test(body.id)) return null
  const amount = body.total_summary?.due, currency = body.currency
  if (!Number.isSafeInteger(amount) || amount <= 0 || amount > 10000000000 || !currencies.has(currency)) return null
  const paid = body.payment_status === 'paid' && body.status === 'complete' && body.state === 'succeeded' && (!body.payment_intent || body.payment_intent.status === 'succeeded')
  const taxReady = body.tax_context?.automatic_tax_enabled === false || body.tax_meta?.status === 'complete'
  return {amount,currency,paid,taxReady,open:body.status==='open'&&body.payment_status==='unpaid',action:body.payment_intent?.status==='requires_action'}
}
export function confirmMatches(url, form, id, quote) {
  return new URL(url).hostname==='api.stripe.com' && new URL(url).pathname===`/v1/payment_pages/${id}/confirm` && form.get('expected_amount')===String(quote.amount)
}
const pause = ms=>new Promise(r=>setTimeout(r,ms))

export async function run(input, commands, emit, dependencies={}) {
  let browser, sent=false, permit=false, latest=null, changedAt=0, quote=null, blocked='', responseCount=0, declined=false, networkFailed=false
  const stage = code=>emit({type:'stage',code})
  const done = (state,code='')=>emit({type:'result',state,code,sent,amount:latest?.amount||0,currency:latest?.currency||''})
  let quoteTimer
  const abort=()=>{browser?.close().catch(()=>{})}
  dependencies.signal?.addEventListener('abort',abort,{once:true})
  try {
    if(dependencies.signal?.aborted)throw Error('cancelled')
    const id=checkoutIdentity(input.url)
    const {chromium}=dependencies.playwright || await import('playwright')
    const chrome=process.env.PAYMENT_CHROME_PATH || (process.platform==='win32' && fs.existsSync('C:/Program Files/Google/Chrome/Application/chrome.exe')?'C:/Program Files/Google/Chrome/Application/chrome.exe':undefined)
    browser=await chromium.launch({headless:true,executablePath:chrome,args:['--disable-quic','--force-webrtc-ip-handling-policy=disable_non_proxied_udp','--disable-background-networking','--disable-breakpad']})
    if(dependencies.signal?.aborted)throw Error('cancelled')
    const ctx=await browser.newContext({locale:'en-US',timezoneId:'America/New_York',serviceWorkers:'block',acceptDownloads:false})
    await ctx.route('**/*',async route=>{
      const req=route.request(), u=new URL(req.url())
      if(u.protocol!=='https:' || !allowedHost(u.hostname) || u.username || u.password || (u.port&&u.port!=='443')) return route.abort()
      if(req.method()==='POST' && /\/(confirm|pay|capture)$/.test(u.pathname)) {
        if(input.mode==='check'||!permit||sent) {blocked='duplicate_blocked';return route.abort()}
        if(!latest || latest.amount!==quote.amount||latest.currency!==quote.currency||!latest.taxReady||!confirmMatches(req.url(),new URLSearchParams(req.postData()||''),id,quote)) {blocked='amount_changed';return route.abort()}
        sent=true
        emit({type:'submitted'})
      }
      return dependencies.route ? dependencies.route(route) : route.continue()
    })
    const page=await ctx.newPage()
    page.on('requestfailed',req=>{if(['api.stripe.com','js.stripe.com'].includes(new URL(req.url()).hostname)&&/TUNNEL|PROXY_CONNECTION/.test(req.failure()?.errorText||''))networkFailed=true})
    page.on('response',async res=>{
      const u=new URL(res.url())
      if(u.hostname!=='api.stripe.com'||!u.pathname.startsWith(`/v1/payment_pages/${id}`)) return
      const body=await res.json().catch(()=>null)
      if(u.pathname===`/v1/payment_pages/${id}/confirm`&&res.status()===402&&body?.error?.type==='card_error'&&['card_declined','expired_card','incorrect_cvc','incorrect_number'].includes(body.error.code)&&(!body.error.payment_intent||body.error.payment_intent.status==='requires_payment_method'))declined=true
      if(res.status()!==200)return
      const s=snapshot(body,id)
      if(s){latest=s;changedAt=Date.now();responseCount++}
    })
    const navigate=()=>page.goto(input.url,{waitUntil:'domcontentloaded',timeout:Math.min(input.prepare_seconds*1000,60000)})
    stage('opening')
    await navigate()
    if(input.mode==='check') {
      const until=Date.now()+input.prepare_seconds*1000
      while(!latest&&Date.now()<until) await pause(250)
      if(latest?.paid) return done('succeeded')
      return done(latest?.action?'action_required':'review','result_unconfirmed')
    }
    // No endpoint parameters or verification tokens are synthesized. The current
    // official checkout renders fields, calculates tax and performs verification.
    const find = async selectors=>{
      for(const f of page.frames())for(const selector of selectors){const x=f.locator(selector).first();if(await x.isVisible().catch(()=>false))return x}
      return null
    }
    const field = async (selectors,value,required=true)=>{
      const until=Date.now()+(required?15000:500)
      do {
        const x=await find(selectors)
        if(x){const tag=await x.evaluate(el=>el.tagName);if(tag==='SELECT')await x.selectOption(value);else await x.fill(value);await x.blur();return true}
        await pause(200)
      }while(Date.now()<until)
      if(required)throw Error('checkout_fields_unavailable')
      return false
    }
    await field(['#cardNumber','input[name="cardnumber"]','input[name="cardNumber"]','input[autocomplete="cc-number"]'],input.card.number)
    await field(['#cardExpiry','input[name="exp-date"]','input[name="cardExpiry"]','input[autocomplete="cc-exp"]'],`${input.card.month}/${input.card.year.slice(-2)}`)
    await field(['#cardCvc','input[name="cvc"]','input[name="cardCvc"]','input[autocomplete="cc-csc"]'],input.card.cvc)
    input.card={}
    stage('billing')
    await field(['#email','input[type="email"]'],input.email,false)
    await field(['#billingName','input[name="billingName"]','input[autocomplete="cc-name"]'],`${input.billing.first_name} ${input.billing.last_name}`)
    await field(['#billingCountry','select[name="billingCountry"]','select[autocomplete="billing country"]'],'US')
    const manual=page.getByText(/enter address manually/i).first()
    if(await manual.isVisible().catch(()=>false)) await manual.click()
    const beforeAddress=responseCount
    await field(['#billingAddressLine1','input[name="billingAddressLine1"]','input[autocomplete="billing address-line1"]','input[name="addressLine1"]','input[autocomplete$=" address-line1"]'],input.billing.line1)
    await field(['#billingLocality','input[name="billingLocality"]','input[autocomplete="billing address-level2"]','input[name="locality"]','input[autocomplete$=" address-level2"]'],input.billing.city)
    await field(['#billingAdministrativeArea','select[name="billingAdministrativeArea"]','input[name="billingAdministrativeArea"]','select[autocomplete="billing address-level1"]','select[name="administrativeArea"]','input[name="administrativeArea"]','select[autocomplete$=" address-level1"]'],input.billing.state)
    await field(['#billingPostalCode','input[name="billingPostalCode"]','input[autocomplete="billing postal-code"]','input[name="postalCode"]','input[autocomplete$=" postal-code"]'],input.billing.postal_code)
    stage('pricing')
    const quoteUntil=Date.now()+input.prepare_seconds*1000
    while(Date.now()<quoteUntil) {
      if(latest?.paid) return done('review','already_paid')
      if(latest?.taxReady&&latest.open&&responseCount>beforeAddress&&Date.now()-changedAt>1500) break
      await pause(250)
    }
    if(!latest?.taxReady||!latest.open||responseCount<=beforeAddress) throw Error('quote_unavailable')
    quote={amount:latest.amount,currency:latest.currency}
    emit({type:'quote',...quote})
    const command=await Promise.race([commands.next(),new Promise(resolve=>{quoteTimer=setTimeout(()=>resolve({value:{type:'expired'}}),input.quote_seconds*1000)})])
    clearTimeout(quoteTimer)
    if(command.value?.type!=='confirm') return done('cancelled','quote_expired')
    if(!latest.taxReady||latest.amount!==quote.amount||latest.currency!==quote.currency) return done('failed','amount_changed')
    permit=true
    stage('confirming')
    const button=await find(['button[data-testid="hosted-payment-submit-button"]','button.SubmitButton','button[type="submit"]'])
    if(!button)throw Error('checkout_fields_unavailable')
    await button.click({timeout:15000})
    const until=Date.now()+input.result_seconds*1000
    while(Date.now()<until) {
      if(latest?.paid) return done('succeeded')
      if(declined) return done('declined','payment_declined')
      if(blocked) return done(sent?'review':'failed',blocked)
      if(latest?.action) return done('action_required','verification_required')
      await pause(300)
    }
    // Even an apparently empty response after clicking may have caused a charge.
    return done(sent?'review':'failed',sent?'result_unconfirmed':'verification_required')
  } catch (e) {
    // Never propagate exception strings: Playwright often includes filled values.
    const safeCodes=new Set(['unsupported_checkout','quote_unavailable'])
    return done(sent?'review':'failed',sent?'result_unconfirmed':networkFailed?'network_connection_failed':safeCodes.has(e.message)?e.message:'preparation_failed')
  } finally {
    clearTimeout(quoteTimer)
    dependencies.signal?.removeEventListener('abort',abort)
    input.card={};input.billing={}
    await browser?.close().catch(()=>{})
  }
}

if(process.argv[1]&&import.meta.url===pathToFileURL(process.argv[1]).href) {
  const lines=readline.createInterface({input:process.stdin,crlfDelay:Infinity})
  const shutdown=new AbortController()
  lines.on('close',()=>shutdown.abort())
  const iterator=lines[Symbol.asyncIterator]()
  const commands={next:async()=>{const r=await iterator.next();try{return {value:JSON.parse(r.value||'{}')}}catch{return {value:{type:'cancel'}}}}}
  let input
  try {input=JSON.parse((await iterator.next()).value);await run(input,commands,v=>process.stdout.write(JSON.stringify(v)+'\n'),{signal:shutdown.signal})}
  catch {process.stdout.write(JSON.stringify({type:'result',state:'failed',code:'runtime_unavailable',sent:false})+'\n')}
  finally{lines.close();process.stdin.destroy()}
}
