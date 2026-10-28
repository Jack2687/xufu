"""Build the public contract; never include upstream routes or real credentials."""
import json
from pathlib import Path

def obj(properties, required=()):
    return {'type':'object','properties':properties,'required':list(required),'additionalProperties':False}
def ref(name): return {'$ref':'#/components/schemas/'+name}
def arr(item): return {'type':'array','items':item}
def text(description=''): return {'type':'string','description':description}
def money(description='金额单位为分，100 分 = 1 R'): return {'type':'integer','format':'int64','description':description}

schemas={
 'Tier':obj({'code':text('等级标识'),'name':text('会员名称'),'threshold':money('有效消费门槛，单位分'),'discount':{'type':'integer','minimum':1,'maximum':100,'description':'实付比例；95 表示减免 5%'},'description':text('权益说明')}),
 'Membership':obj({'current':ref('Tier'),'automatic':ref('Tier'),'next':{'anyOf':[ref('Tier'),{'type':'null'}]},'spent':money('成功订单扣款减去退费后的有效消费'),'remaining':money('距离下一级自动等级'),'progress':{'type':'integer','minimum':0,'maximum':100},'source':{'enum':['automatic','manual']},'until':{'type':['string','null'],'format':'date-time'},'fee_override':{'type':['integer','null'],'description':'专属单价，非空时优先于等级折扣'},'tiers':arr(ref('Tier'))}),
 'Session':{'type':'object','required':['accessToken','user','account'],'properties':{'accessToken':text('刚获取的完整令牌；不会在响应或日志回显'),'user':obj({'email':{'type':'string','format':'email'}},['email']),'account':obj({'id':text('会话空间标识'),'planType':text('当前方案，可选')},['id']),'expires':{'type':'string','format':'date-time','description':'会话到期时间，与 Token 的 exp 取较早值'}},'additionalProperties':True},
 'SessionInfo':obj({'email':{'type':'string','format':'email'},'account_id':text(),'plan':text(),'expires_at':{'type':'string','format':'date-time'},'status':{'const':'format_valid'},'online_verified':{'const':False}}),
 'OrderInput':obj({'product_id':text('服务目录返回的 id'),'session':ref('Session'),'country':{'enum':['PH','US','JP','SG','GB','EG','TR']},'seats':{'type':'integer','minimum':1,'maximum':100,'description':'Team 最少 2，个人方案填 1'},'accepted':{'const':True,'description':'客户已明确接受服务约定'}},['product_id','session','country','seats','accepted']),
 'Order':obj({'id':text(),'product_id':text(),'product_name':text(),'email':{'type':'string','format':'email','description':'从 Session 自动识别'},'country':text(),'seats':{'type':'integer'},'status':{'enum':['draft','queued','processing','awaiting_payment','succeeded','failed','cancelled','review']},'fee':money('创建时锁定的服务费报价，草稿不扣款'),'source':{'enum':['web','api']},'renewal':text(),'created_at':{'type':'string','format':'date-time'},'updated_at':{'type':'string','format':'date-time'}}),
 'PriceSnapshot':obj({'standard':money(),'fee':money(),'tier_code':text(),'tier_name':text(),'discount':{'type':'integer'},'source':{'enum':['automatic','manual']},'fee_override':{'type':['integer','null']},'spent':money()}),
 'OrderDetail':obj({'order':ref('Order'),'events':arr(obj({'stage':text(),'message':text('对外业务阶段说明，不含原始内部请求'),'created_at':{'type':'string','format':'date-time'}}))}),
 'Product':obj({'id':text(),'name':text(),'description':text(),'fee':money('标准服务费'),'member_fee':money('当前账户实际服务费'),'tag':text(),'features':arr(text())}),
 'Wallet':obj({'balance':money('可用余额'),'frozen':money('冻结金额'),'unit':{'const':'cent_R'},'membership':ref('Membership')}),
 'Error':obj({'error':text('供用户阅读的中文错误原因')},['error']),
}
schemas['OrderInput']['properties'].update({'prepare':{'type':'boolean','default':False,'description':'true：排队生成结账链接；false：仅草稿。均不扣费、不付款。'},'workspace_name':text('Team 空间名称，默认 My workspace'),'premium_seats':{'type':'integer','minimum':1,'maximum':100,'default':1,'description':'Team 高级席位，seats 为普通席位数量'}})
schemas['Checkout'] = obj({'ready':{'type':'boolean'},'url':text('仅本人或管理员可见的最终结账链接'),'expires_at':{'type':'string','format':'date-time','description':'本平台的链接保留期限，不保证上游期限'},'expired':{'type':'boolean'},'payment_connected':{'const':False},'attempts':{'type':'integer'},'waiting_message':text('等待代理或重试的公开说明'),'next_check_at':{'type':'string','format':'date-time'}})
schemas['OrderDetail']['properties']['checkout'] = ref('Checkout')
schemas['Order']['properties']['price_snapshot'] = {**ref('PriceSnapshot'), 'description':'详情返回创建订单时的价格快照；旧订单为空对象，列表不返回此字段'}
for field in ['user', 'account']:
    schemas['Session']['properties'][field]['additionalProperties'] = True
tier={'code':'L1','name':'启程会员','threshold':0,'discount':100,'description':'从第一笔成功订单开始积累'}
membership={'current':tier,'automatic':tier,'next':{'code':'L2','name':'银阶会员','threshold':200000,'discount':95,'description':'服务费减免 5%'},'spent':0,'remaining':200000,'progress':0,'source':'automatic','until':None,'fee_override':None,'tiers':[tier]}
session={'user':{'email':'subscriber@example.com'},'account':{'id':'SESSION_ACCOUNT_ID'},'accessToken':'REPLACE_WITH_FRESH_SESSION_TOKEN','expires':'2030-01-01T00:00:00Z'}
order={'id':'ORDER_ID','product_id':'pro','product_name':'Pro 20x','email':'subscriber@example.com','country':'US','seats':1,'status':'draft','fee':10000,'source':'api','renewal':'not_started','created_at':'2026-10-27T00:00:00Z','updated_at':'2026-10-27T00:00:00Z'}
paths={}
def add(path,method,summary,description,response,example,body=None,body_example=None,parameters=()):
    operation={'summary':summary,'description':description,'operationId':method+'_'+path.replace('/api/v1/','').replace('/','_').replace('{','').replace('}',''),'security':[{'BearerKey':[]}],'parameters':list(parameters),'responses':{'200':{'description':'成功','content':{'application/json':{'schema':response,'example':example}}}}}
    for status,desc in [('400','参数无效、Session 过期或业务校验不通过'),('401','密钥无效、已撤销或账户停用'),('403','权限或来源限制'),('404','资源不存在或不属于当前账户'),('409','幂等内容冲突或状态冲突'),('429','请求过于频繁'),('500','服务暂不可用')]:
        operation['responses'][status]={'description':desc,'content':{'application/json':{'schema':ref('Error'),'example':{'error':desc}}}}
    if body: operation['requestBody']={'required':True,'content':{'application/json':{'schema':body,'example':body_example}}}
    paths.setdefault('/api/v1/'+path,{})[method]=operation

add('wallet','get','余额与会员','返回当前 API 密钥所属账户的余额、冻结金额及会员等级。',ref('Wallet'),{'balance':100000,'frozen':0,'unit':'cent_R','membership':membership})
add('catalog','get','套餐和地区','目录只列出已启用方案，member_fee 为当前客户报价。仅支持返回的国家代码。',obj({'items':arr(ref('Product')),'membership':ref('Membership'),'unit':{'const':'cent_R'},'countries':arr(obj({'code':text(),'name':text(),'currency':text()})),'payment_connected':{'const':False}}),{'items':[{'id':'pro','name':'Pro 20x','fee':10000,'member_fee':10000}],'countries':[{'code':'US','name':'美国','currency':'USD'}],'payment_connected':False,'unit':'cent_R','membership':membership})
add('membership','get','会员权益与成长','充值与兑换不提升等级。仅成功订单已结算的服务费计入，服务费退回会扣减成长。指定等级到期自动恢复；专属单价优先。',obj({'membership':ref('Membership'),'products':arr({'type':'object'}),'growth':arr(obj({'id':text(),'product_name':text(),'spent':money(),'updated_at':{'type':'string','format':'date-time'}})),'unit':{'const':'cent_R'},'basis':{'const':'settled_service_spend'}}),{'membership':membership,'products':[],'growth':[],'unit':'cent_R','basis':'settled_service_spend'})
add('quote','get','获取当前报价','创建订单时服务器重新计算并锁定报价。价格快照不会因后续规则调整而改变。',obj({'fee':money(),'standard':money(),'membership':ref('Membership'),'payment_connected':{'const':False}}),{'fee':10000,'standard':10000,'membership':membership,'payment_connected':False},parameters=[{'name':'product_id','in':'query','required':True,'schema':{'type':'string'},'example':'pro'}])
add('session/inspect','post','识别 Session 账号','仅检查 JSON、账号信息一致性及有效期。取 Token exp 和外层 expires 的较早值；不验证签名或在线状态，不等同于上游登录成功。原始 Session 不持久保存。',ref('SessionInfo'),{'email':'subscriber@example.com','account_id':'SESSION_ACCOUNT_ID','plan':'free','expires_at':'2030-01-01T00:00:00Z','status':'format_valid','online_verified':False},obj({'session':ref('Session')},['session']),{'session':session})
add('wallet/redeem','post','兑换余额','每张兑换码只能入账一次。并发兑换由事务锁保护；同码重复提交返回 400，不重复入账。',obj({'ok':{'const':True},'amount':money()}),{'ok':True,'amount':10000},obj({'code':text('有效兑换码')},['code']),{'code':'XP-REPLACE_WITH_VOUCHER'})
add('orders','post','创建方案或准备结账','prepare=true 时排队生成结账链接，默认长链，无需账单资料，返回 queued；省略时为 draft。均不冻结、不扣费、不提交银行卡付款。邮箱从 Session 提取。仅提链所需令牌加密暂存，任务结束后清除。超时重试复用幂等键，Token 更新不改变幂等判定；账号、套餐和准备参数变化会冲突。',obj({'id':text(),'status':{'type':'string'},'payment_connected':{'const':False},'replayed':{'type':'boolean'},'checkout_connected':{'type':'boolean'}}),{'id':'ORDER_ID','status':'draft','payment_connected':False},ref('OrderInput'),{'product_id':'pro','session':session,'country':'US','seats':1,'accepted':True},[{'name':'Idempotency-Key','in':'header','required':True,'schema':{'type':'string','minLength':8,'maxLength':120},'example':'subscription_20261027_001'}])
add('orders','get','查询订单列表','仅返回当前账户最近 200 笔订单。可按邮箱或编号搜索，以及状态筛选。',obj({'items':arr(ref('Order'))}),{'items':[order]},parameters=[{'name':'q','in':'query','schema':{'type':'string'},'description':'订单编号或订阅邮箱'},{'name':'status','in':'query','schema':{'type':'string'},'description':'例如 draft、cancelled'}])
idparam=[{'name':'id','in':'path','required':True,'schema':{'type':'string'},'example':'ORDER_ID'}]
add('orders/{id}','get','订单详情与进度','返回本人订单、价格快照、公开进度及已生成的结账链接。queued/processing 建议每 5 秒查询；awaiting_payment 仅表示链接已生成，绝不表示付款成功。终态与待核实停止轮询；不要自动重新创建结果不明的任务。',ref('OrderDetail'),{'order':dict(order,price_snapshot={'standard':10000,'fee':10000,'tier_code':'L1','tier_name':'启程会员','discount':100,'source':'automatic','fee_override':None,'spent':0}),'events':[{'stage':'draft','message':'订阅方案已保存；未提交付款。','created_at':'2026-10-27T00:00:00Z'}]},parameters=idparam)
add('orders/{id}/cancel','post','结束结账准备','允许结束 draft、queued、awaiting_payment；执行中的请求不能撤回。该操作不会使已复制的上游链接失效，也不取消已发生的付款。',obj({'ok':{'const':True}}),{'ok':True},parameters=idparam)


spec={'openapi':'3.1.0','info':{'title':'序付 XUPAY 开发者 API','version':'1.2.1','description':'支持代理提链，默认长链，无需账单资料；银行卡付款与取消续费尚未接入。所有金额为整数分，100 分 = 1 R。'},'servers':[{'url':'/','description':'当前站点'}],'paths':paths,'components':{'securitySchemes':{'BearerKey':{'type':'http','scheme':'bearer','description':'在开发接入创建 xp_live_ 密钥；仅显示一次，不放在 URL 或日志中。'}},'schemas':schemas}}
target=Path(__file__).resolve().parents[1]/'internal/platform/openapi.json'
target.write_text(json.dumps(spec,ensure_ascii=False,indent=2)+'\n',encoding='utf-8')
print('OpenAPI contract generated:',len(paths),'paths')
