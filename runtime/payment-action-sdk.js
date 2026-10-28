"use strict";
const button=document.getElementById("start"), status=document.getElementById("status");
button.addEventListener("click",async()=>{
 button.disabled=true;status.textContent="正在处理官方验证，请勿重复提交……";
 let stage="context";
 const started=performance.now(); let cspBlocked=false;
 const onCSP=()=>{cspBlocked=true;};
 document.addEventListener("securitypolicyviolation",onCSP);
 try {
  const response=await fetch("context",{method:"POST",headers:{"X-Action-Request":"1"},cache:"no-store",credentials:"omit"});
  if(!response.ok)throw new Error("context_failed");
  const data=await response.json();
  stage="sdk_load";
  if(typeof Stripe!=="function")throw new Error("sdk_unavailable");
  stage="preflight";
  const stripe=Stripe(data.publishableKey);
  const current=await stripe.retrievePaymentIntent(data.clientSecret);
  if(current.error)throw new Error("preflight_failed");
  const expected=data.clientSecret.split("_secret_")[0];
  if(!current.paymentIntent || current.paymentIntent.id!==expected)throw new Error("context_mismatch");
  if(current.paymentIntent.status!=="requires_action")throw new Error("action_not_required");
  stage="sdk_action";
  const result=await stripe.handleNextAction({clientSecret:data.clientSecret});
  const allowed=new Set(["card_declined","payment_intent_unexpected_state","payment_intent_authentication_failure","payment_intent_invalid_parameter","invalid_client_secret","resource_missing","api_connection_error"]);
  const code=result.error?(allowed.has(result.error.code)?result.error.code:"sdk_error"):"";
  status.textContent=result.error?"验证未完成（"+code+"）。正在通知终端核验原交易，请查看终端结果。":"验证步骤已结束，正在通知终端核验原交易。";
  stage="notify";
  const completed=await fetch("complete",{method:"POST",headers:{"X-Action-Request":"1"},credentials:"omit",body:JSON.stringify({outcome:result.error?"sdk_error":"sdk_returned",code,durationMs:Math.round(performance.now()-started)})});
  if(!completed.ok)throw new Error("notify_failed");
 }catch(error){
 const safeCodes=new Set(["preflight_failed","context_mismatch","action_not_required"]);
 const failureCode=cspBlocked?"csp_blocked":safeCodes.has(error.message)?error.message:stage==="sdk_load"?"sdk_unavailable":"unknown";
 if(stage!=="notify"){try{await fetch("complete",{method:"POST",headers:{"X-Action-Request":"1"},credentials:"omit",body:JSON.stringify({outcome:stage==="sdk_action"?"sdk_exception":"page_error",code:failureCode,durationMs:Math.round(performance.now()-started)})});}catch(_){}}
 const labels={context:"读取本机验证上下文失败",sdk_load:"官方 SDK 未加载",preflight:"原交易状态核验未通过，未启动验证",sdk_action:"官方 SDK 验证未完成",notify:"通知终端失败"};status.textContent=labels[stage]+"。请检查网络和终端，不要重新付款；可关闭本页后核验原交易。";}finally{document.removeEventListener("securitypolicyviolation",onCSP);}
});