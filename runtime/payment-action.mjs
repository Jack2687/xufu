// Original cs-live official SDK page runs in a private server browser.
// Only a live image and operator input cross the admin boundary; no HAR/files.
import fs from 'node:fs'
import crypto from 'node:crypto'
import readline from 'node:readline'
import {fileURLToPath} from 'node:url'
import path from 'node:path'
import {isIP} from 'node:net'
import {chromium} from 'playwright'

const html=fs.readFileSync(new URL('./payment-action.html',import.meta.url),'utf8')
const script=fs.readFileSync(new URL('./payment-action-sdk.js',import.meta.url),'utf8')
const outcomes=new Set(['sdk_returned','sdk_error','sdk_exception','page_error'])
const codes=new Set(['','authentication_required','payment_intent_authentication_failure','card_declined','expired_card','incorrect_cvc','processing_error','preflight_failed','context_mismatch','action_not_required','csp_blocked','sdk_unavailable','payment_intent_unexpected_state','payment_intent_invalid_parameter','invalid_client_secret','resource_missing','api_connection_error','sdk_error','unknown'])
export function safeActionResult(v={}){return {outcome:outcomes.has(v.outcome)?v.outcome:'page_error',code:codes.has(v.code)?v.code:'unknown'}}
export async function runAction(input,emit,commands,fixture={}) {
  const proxy=new URL(input.proxy?.server||'')
  if(proxy.protocol!=='http:'||proxy.hostname!=='127.0.0.1'||!proxy.port||!input.proxy.username||!input.proxy.password||!isIP(input.expected_ip||'')||!/^pk_[A-Za-z0-9_]+$/.test(input.publishable_key)||!/^pi_[A-Za-z0-9]+_secret_[A-Za-z0-9]+$/.test(input.client_secret))throw Error('invalid_context')
  const expires=Math.min(Date.now()+600000,input.expires_at*1000)
  if(expires<=Date.now())throw Error('expired')
  const base='https://xupay-action.invalid/'+crypto.randomBytes(24).toString('hex')+'/'
  let timer,screenTimer,finished=false,closed=false,frameBusy=false,page,browser,sequence=0
  const guards=new Map()
  let resolveDone
  const done=new Promise(resolve=>{resolveDone=resolve})
  const finish=value=>{if(finished)return;finished=true;emit({type:'result',...safeActionResult(value)});resolveDone()}
  let commandQueue=Promise.resolve(),queued=0
  commands.onCommand(v=>{
    if(v.type==='guard') { const resolve=guards.get(v.request_id);if(resolve){guards.delete(v.request_id);resolve(v.allow===true)};return }
    if(v.type==='close'){closed=true;resolveDone();return}
    if(input.automatic||finished||closed||!page||queued>=32)return
    queued++
    commandQueue=commandQueue.then(async()=>{
      if(closed||finished)return
      if(v.type==='click'&&Number.isFinite(v.x)&&Number.isFinite(v.y)&&v.x>=0&&v.x<960&&v.y>=0&&v.y<720)await page.mouse.click(v.x,v.y)
      else if(v.type==='text'&&typeof v.text==='string'&&[...v.text].length<=128&&!/[\x00-\x1f\x7f]/.test(v.text))await page.keyboard.insertText(v.text)
      else if(v.type==='key'&&['Tab','Enter','Escape','Backspace','Delete','ArrowUp','ArrowDown','ArrowLeft','ArrowRight','Space'].includes(v.key))await page.keyboard.press(v.key)
      else if(v.type==='scroll'&&Number.isFinite(v.y))await page.mouse.wheel(0,Math.max(-600,Math.min(600,v.y)))
    }).catch(()=>{}).finally(()=>{queued--})
    return commandQueue
  })
  try {
    browser=await chromium.launch({headless:true,executablePath:process.env.PLAYWRIGHT_CHROMIUM_EXECUTABLE||undefined,args:['--disable-quic','--force-webrtc-ip-handling-policy=disable_non_proxied_udp'],proxy:input.proxy})
    if(closed)return
    timer=setTimeout(()=>{if(!finished)emit({type:'timeout'});closed=true;resolveDone();void browser.close()},Math.max(1,expires-Date.now()))
    // Closing stdin or an operator close also interrupts navigation/SDK loading.
    void done.then(()=>{if(closed)void browser.close()})
    const context=await browser.newContext({viewport:{width:960,height:720},acceptDownloads:false,serviceWorkers:'block'})
    const csp="default-src 'none'; script-src 'self' https://js.stripe.com https://*.js.stripe.com; frame-src https://js.stripe.com https://*.stripe.com; connect-src 'self' https://*.stripe.com; style-src 'unsafe-inline'; base-uri 'none'; form-action 'none'; frame-ancestors 'none'"
    const headers={'cache-control':'no-store','referrer-policy':'no-referrer','x-content-type-options':'nosniff','content-security-policy':csp}
    await context.route('**/*',async route=>{
      try {
        const req=route.request(),u=new URL(req.url())
        if(req.url().startsWith(base)) {
          const relative=req.url().slice(base.length)
          if(relative===''&&req.method()==='GET')return route.fulfill({status:200,headers,contentType:'text/html',body:html})
          if(relative==='app.js'&&req.method()==='GET')return route.fulfill({status:200,headers,contentType:'application/javascript',body:script})
          if(req.method()==='POST'&&req.headers()['x-action-request']==='1'&&req.headers().origin===new URL(base).origin){
            if(relative==='context')return route.fulfill({status:200,headers,contentType:'application/json',body:JSON.stringify({publishableKey:input.publishable_key,clientSecret:input.client_secret})})
            if(relative==='complete'){
              const value=JSON.parse(req.postData()||'{}')
              await route.fulfill({status:204,headers})
              finish(value);return
            }
          }
          return route.abort()
        }
        // Every external connection uses the authenticated order relay. The
        // relay checks public targets, lease/expiry and the original exit IP.
        if(u.protocol!=='https:'||u.username||u.password||(u.port&&u.port!=='443')||isIP(u.hostname)||u.hostname==='localhost'||u.hostname.endsWith('.localhost')||Date.now()>=expires)return route.abort()
        if(fixture.route&&await fixture.route(route))return
        // Reused HTTPS connections must recheck the original IP and lease too.
        const requestID=++sequence
        const allowed=await new Promise(resolve=>{
          const timeout=setTimeout(()=>{guards.delete(requestID);resolve(false)},15000)
          guards.set(requestID,value=>{clearTimeout(timeout);resolve(value)})
          emit({type:'guard',request_id:requestID})
        })
        if(!allowed||closed||finished||Date.now()>=expires)return route.abort()
        return route.continue()
      }catch{await route.abort().catch(()=>{})}
    })
    page=await context.newPage()
    context.on('page',p=>{const previous=page;page=p;p.on('close',()=>{if(page===p&&!previous.isClosed())page=previous})})
    await page.goto(base,{waitUntil:'networkidle',timeout:45000})
    // Same retrievePaymentIntent -> identity/status check -> handleNextAction
    // as the supplied page. Only its initial continue button is automated.
    await page.locator('#start').click()
    emit({type:'ready'})
    if(!input.automatic)screenTimer=setInterval(async()=>{
      if(frameBusy||finished||closed)return
      frameBusy=true
      try{const data=await page.screenshot({type:'jpeg',quality:70,timeout:3000});if(data.length<=1000000)emit({type:'frame',image:data.toString('base64')})}catch{}
      finally{frameBusy=false}
    },1200)
    if(fixture.ready)await fixture.ready(page)
    await done
  }finally{
    closed=true;clearInterval(screenTimer);clearTimeout(timer)
    for(const resolve of guards.values())resolve(false)
    guards.clear()
    await browser?.close()
  }
}

if(process.argv[1]&&fileURLToPath(import.meta.url)===path.resolve(process.argv[1])){
  let input,handler=()=>{},running
  const lines=readline.createInterface({input:process.stdin,crlfDelay:Infinity})
  const emit=v=>process.stdout.write(JSON.stringify(v)+'\n')
  lines.on('line',line=>{
    let value;try{value=JSON.parse(line)}catch{return}
    if(!input){input=value;running=runAction(input,emit,{onCommand:fn=>{handler=fn}}).catch(()=>emit({type:'error',code:'runtime_unavailable'})).finally(()=>{lines.close();process.exit()})}
    else handler(value)
  })
  lines.on('close',()=>handler({type:'close'}))
}
