import assert from 'node:assert/strict';
import fs from 'node:fs/promises';
import http from 'node:http';
import path from 'node:path';
const {chromium}=await import(process.env.PLAYWRIGHT_MODULE||'playwright');
const root=path.resolve('internal/adminui/static');
const server=http.createServer(async(req,res)=>{try{const file=path.join(root,req.url==='/'?'index.html':req.url);res.setHeader('Content-Security-Policy',"default-src 'none'; script-src 'self'; style-src 'self'; connect-src 'self'; img-src 'self'; frame-ancestors 'none'; base-uri 'none'; form-action 'self'");res.setHeader('Content-Type',file.endsWith('.js')?'text/javascript':file.endsWith('.css')?'text/css':'text/html');res.end(await fs.readFile(file));}catch{res.writeHead(404);res.end();}});
await new Promise(resolve=>server.listen(0,'127.0.0.1',resolve));
const browser=await chromium.launch({headless:true,executablePath:process.env.CHROME_EXECUTABLE});
const page=await browser.newPage({viewport:{width:390,height:920}}),errors=[];
page.on('pageerror',e=>errors.push(e.message));
let item=null,createCalls=0,firstKey='',stock=500,outage=false,mode='ready',loseResponse=true,batchCalls=0,batchKey='',loseBatchResponse=true,failNextList=false;const now=new Date().toISOString();
await page.route('**/api/**',async route=>{
 const req=route.request(),url=new URL(req.url());let status=200,data={};
 if(url.pathname.endsWith('/session')){status=401;data={error:{message:'login'}};}
 else if(url.pathname.endsWith('/metrics/offers'))data={metrics:[]};
 else if(url.pathname.endsWith('/offers'))data={deliveryAvailable:true,offers:[{id:'friends',name:'朋友体验',productID:'health.premium.subscription.monthly',subscriptionID:'456',active:true,productionCodeCount:500,sandboxCodeCount:0}],writesEnabled:false,creationActiveCount:1,activeCount:1,activeLimit:10,syncedAt:now};
 else if(url.pathname.endsWith('/code-pools'))data={codePools:[{id:'batch-500',kind:'oneTime',environment:'PRODUCTION',numberOfCodes:500,active:true,expirationDate:'2030-01-01'},{id:'shared',kind:'custom',environment:'PRODUCTION',numberOfCodes:500,active:true},{id:'sandbox',kind:'oneTime',environment:'SANDBOX',numberOfCodes:10,active:true,expirationDate:'2030-01-01'}]};
 else if(url.pathname.endsWith('/one-time-code-batches')){
  const body=req.postDataJSON();assert.equal(body.numberOfCodes,500);assert.equal(body.environment,'PRODUCTION');
  batchCalls++;const key=req.headers()['idempotency-key'];assert.ok(key);if(batchKey)assert.equal(key,batchKey);else batchKey=key;
  if(loseBatchResponse){loseBatchResponse=false;status=503;data={error:{code:'operation_unavailable',message:'创建结果暂时无法返回'}};}
  else{stock=500;mode='ready';data={codePool:{id:'batch-500'},deliveryReady:true};}
 }
 else if(url.pathname.endsWith('/personal-deliveries')){
  if(req.method()==='GET')data={deliveries:item?[item]:[],pools:outage||mode==='empty'?[]:[{id:'batch-500',expiration:'2030-01-01',capacity:500,available:mode==='unconfirmed'?0:stock,managed:mode!=='unconfirmed',unconfirmed:mode==='unconfirmed'?500:0,active:true}],inventoryError:outage?'库存暂时不可用':''};
  else{
   const b=req.postDataJSON();assert.equal(req.headers()['x-admin-csrf'],'fixture');assert.equal(b.expiration,undefined);assert.equal(b.numberOfCodes,undefined);
   if(b.action==='create'){
    createCalls++;assert.equal(b.poolID,'batch-500');assert.equal(b.name,'小王');if(mode==='unconfirmed'){assert.equal(b.confirmUnissued,true);mode='ready';stock=500;}
    if(!item){firstKey=b.requestKey;stock--;item={delivery:{id:b.requestKey,name:b.name,poolID:'batch-500',requestID:'request-1',state:'ready',expiration:'2030-01-01'},request:{id:'request-1',status:'assigned',version:1,claimExpiresAt:'2030-01-01T00:00:00Z'}};if(loseResponse){loseResponse=false;status=503;data={error:{code:'delivery_unavailable',message:'结果暂时无法返回'}};}else data={...item,token:'health.request-1.1.signature'};}
    else{assert.equal(b.requestKey,firstKey,'retry must keep original operation');data={...item,token:'health.request-1.1.signature'};failNextList=true;}
   }else{assert.equal(b.action,'resume');data={...item,token:'health.request-1.1.signature'};}
  }
 }else{throw new Error('unexpected request '+req.method()+' '+url.pathname);}
 if(req.method()==='GET'&&url.pathname.endsWith('/personal-deliveries')){if(mode==='unconfirmed')data.pools=[data.pools[0],{...data.pools[0],id:'batch-501'},{...data.pools[0],id:'batch-502'}];if(failNextList){failNextList=false;status=503;data={error:{message:'列表暂时不可用'}};}}
 await route.fulfill({status,contentType:'application/json',body:JSON.stringify(data)});
});
try{
 await page.goto(`http://127.0.0.1:${server.address().port}`,{waitUntil:'networkidle'});
 await page.evaluate(async()=>{state.currentApp='health';state.csrf='fixture';reauthenticate=async()=>{};$('#auth').classList.add('hidden');$('#workspace').classList.remove('hidden');await loadOffers();});
 await page.getByRole('button',{name:'熟人发放',exact:true}).click();
 const name=page.locator('#personal-form [name=name]');await name.pressSequentially('pro');assert.equal(await name.inputValue(),'pro');await name.fill('小王');
 assert.equal(await page.locator('#personal-form input').count(),1);
 assert.equal(await page.locator('#personal-form [type=date]').count(),0);
 await page.getByRole('button',{name:'生成领取链接',exact:true}).click();
 await page.getByRole('button',{name:'重试并获取领取链接',exact:true}).waitFor();assert.equal(stock,499);
 assert.equal(await name.getAttribute('readonly'),'');
 await page.getByRole('button',{name:'重试并获取领取链接',exact:true}).click();
 await page.locator('#personal-link').waitFor();assert.equal(stock,499);assert.equal(createCalls,2);assert.doesNotMatch(await page.locator('#delivery-message').innerText(),/结果暂时无法返回/);
 await page.locator('#delivery-secret-dialog').getByRole('button',{name:'关闭',exact:true}).click();
 assert.match(await page.locator('#personal-list').innerText(),/已分配，尚未领取/);
 assert.match(await page.locator('#personal-list').innerText(),/兑换情况：待核对/);
 item.request.claimedAt=now;item.request.deliveredAt=now;item.request.status='delivered';item.appleReport={redemptions:300,days:14};
 await page.getByRole('button',{name:'刷新',exact:true}).click();await page.getByText(/已领取 ·/).waitFor();
 assert.match(await page.locator('#personal-list').innerText(),/兑换情况：待核对/);
 assert.doesNotMatch(await page.locator('#personal-list').innerText(),/300|已确认.*兑换/);
 item.request.reportedRedeemedAt=now;await page.getByRole('button',{name:'刷新',exact:true}).click();await page.getByText('对方反馈已兑换，尚未核实',{exact:true}).waitFor();
 item.request.verifiedAt=now;await page.getByRole('button',{name:'刷新',exact:true}).click();await page.getByText('已人工关联 Apple 核销证据',{exact:true}).waitFor();
 await fs.mkdir('/tmp/offer-delivery-acceptance',{recursive:true});await page.screenshot({path:'/tmp/offer-delivery-acceptance/personal-one-time.png',fullPage:true});
 stock=0;await page.getByRole('button',{name:'刷新',exact:true}).click();await page.getByText('目前没有可用兑换码，当前后台未开启在 Apple 创建兑换码的权限。',{exact:true}).waitFor();assert.equal(await page.getByRole('button',{name:'生成领取链接',exact:true}).isDisabled(),true);
 outage=true;await page.getByRole('button',{name:'刷新',exact:true}).click();await page.getByText(/暂时无法从 Apple 读取兑换码/).waitFor();assert.match(await page.locator('#personal-list').innerText(),/小王/);
 assert.ok(await page.evaluate(()=>document.documentElement.scrollWidth<=innerWidth+1));
 await page.screenshot({path:'/tmp/offer-delivery-acceptance/personal-simple-outage.png',fullPage:true});
 // Existing Apple batch with zero local inventory: no detour to inventory management.
 outage=false;mode='unconfirmed';item=null;
 await page.evaluate(()=>openPersonalDelivery('friends'));
 await page.getByText('找到已有的兑换码，无需重新创建。',{exact:true}).waitFor();
 assert.equal(await page.getByRole('button',{name:'生成领取链接',exact:true}).isDisabled(),true);
 await page.locator('#personal-form [name=name]').fill('小王');
 assert.equal(await page.locator('#personal-existing option').count(),3);assert.equal(await page.locator('#personal-existing').isVisible(),false);
 await page.locator('#personal-confirm-unissued').check();
 await page.screenshot({path:'/tmp/offer-delivery-acceptance/personal-simple-existing.png',fullPage:true});
 await page.getByRole('button',{name:'生成领取链接',exact:true}).click();await page.locator('#personal-link').waitFor();
 assert.equal(stock,499);assert.equal(batchCalls,0,'existing batch must not create another batch');
 await page.locator('#delivery-secret-dialog').getByRole('button',{name:'关闭',exact:true}).click();
 // New-batch path stays on the same page and preserves the cloud operation key.
 mode='empty';item=null;
 await page.evaluate(()=>{state.offerData.writesEnabled=true;return openPersonalDelivery('friends');});
 await page.getByText(/后台会准备 500 枚一次性码/).waitFor();
 await page.locator('#personal-form [name=name]').fill('小王');
 await page.screenshot({path:'/tmp/offer-delivery-acceptance/personal-simple-new.png',fullPage:true});
 await page.getByRole('button',{name:'准备兑换码并生成链接',exact:true}).click();
 await page.getByRole('button',{name:'重试并获取领取链接',exact:true}).waitFor();
 await page.getByRole('button',{name:'重试并获取领取链接',exact:true}).click();await page.locator('#personal-link').waitFor();
 assert.equal(batchCalls,2);assert.equal(stock,499);
 await page.locator('#delivery-secret-dialog').getByRole('button',{name:'关闭',exact:true}).click();
 await page.emulateMedia({colorScheme:'dark'});await page.screenshot({path:'/tmp/offer-delivery-acceptance/personal-simple-dark.png',fullPage:true});
 assert.deepEqual(errors,[]);
 console.log('PASS simple personal send: ready stock, unimported existing batch, new batch, stable typing, lost-response retries, claim/redemption and outage');
}finally{await browser.close();server.close();}
