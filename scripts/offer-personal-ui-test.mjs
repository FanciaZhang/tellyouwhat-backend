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
let item=null,createCalls=0,firstKey='',stock=500,outage=false;const now=new Date().toISOString();
await page.route('**/api/**',async route=>{
 const req=route.request(),url=new URL(req.url());let status=200,data={};
 if(url.pathname.endsWith('/session')){status=401;data={error:{message:'login'}};}
 else if(url.pathname.endsWith('/metrics/offers'))data={metrics:[]};
 else if(url.pathname.endsWith('/offers'))data={deliveryAvailable:true,offers:[{id:'friends',name:'朋友体验',productID:'health.premium.subscription.monthly',subscriptionID:'456',active:true,productionCodeCount:500,sandboxCodeCount:0}],writesEnabled:false,creationActiveCount:1,activeCount:1,activeLimit:10,syncedAt:now};
 else if(url.pathname.endsWith('/code-pools'))data={codePools:[{id:'batch-500',kind:'oneTime',environment:'PRODUCTION',numberOfCodes:500,active:true,expirationDate:'2030-01-01'},{id:'shared',kind:'custom',environment:'PRODUCTION',numberOfCodes:500,active:true},{id:'sandbox',kind:'oneTime',environment:'SANDBOX',numberOfCodes:10,active:true,expirationDate:'2030-01-01'}]};
 else if(url.pathname.endsWith('/personal-deliveries')){
  if(req.method()==='GET')data={deliveries:item?[item]:[],pools:outage?[]:[{id:'batch-500',expiration:'2030-01-01',capacity:500,available:stock,managed:true,active:true}],inventoryError:outage?'库存暂时不可用':''};
  else{
   const b=req.postDataJSON();assert.equal(req.headers()['x-admin-csrf'],'fixture');assert.equal(b.expiration,undefined);assert.equal(b.numberOfCodes,undefined);
   if(b.action==='create'){
    createCalls++;assert.equal(b.poolID,'batch-500');assert.equal(b.name,'小王');
    if(!item){firstKey=b.requestKey;stock--;item={delivery:{id:b.requestKey,name:b.name,poolID:'batch-500',requestID:'request-1',state:'ready',expiration:'2030-01-01'},request:{id:'request-1',status:'assigned',version:1,claimExpiresAt:'2030-01-01T00:00:00Z'}};status=503;data={error:{code:'delivery_unavailable',message:'结果暂时无法返回'}};}
    else{assert.equal(b.requestKey,firstKey,'retry must keep original operation');data={...item,token:'health.request-1.1.signature'};}
   }else{assert.equal(b.action,'resume');data={...item,token:'health.request-1.1.signature'};}
  }
 }else{throw new Error('unexpected request '+req.method()+' '+url.pathname);}
 await route.fulfill({status,contentType:'application/json',body:JSON.stringify(data)});
});
try{
 await page.goto(`http://127.0.0.1:${server.address().port}`,{waitUntil:'networkidle'});
 await page.evaluate(async()=>{state.currentApp='health';state.csrf='fixture';reauthenticate=async()=>{};$('#auth').classList.add('hidden');$('#workspace').classList.remove('hidden');await loadOffers();});
 await page.getByRole('button',{name:'熟人发放',exact:true}).click();
 const name=page.locator('#personal-form [name=name]');await name.pressSequentially('pro');assert.equal(await name.inputValue(),'pro');await name.fill('小王');
 assert.equal(await page.locator('#personal-form input').count(),1);
 assert.equal(await page.locator('#personal-form [type=date]').count(),0);
 await page.getByRole('button',{name:'分配一枚并生成链接',exact:true}).click();
 await page.getByRole('button',{name:'重试同一次分配',exact:true}).waitFor();assert.equal(stock,499);
 assert.equal(await name.getAttribute('readonly'),'');
 await page.getByRole('button',{name:'重试同一次分配',exact:true}).click();
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
 stock=0;await page.getByRole('button',{name:'刷新',exact:true}).click();await page.getByText('暂无可分配库存，请先准备一次性码批次。',{exact:true}).waitFor();assert.equal(await page.getByRole('button',{name:'分配一枚并生成链接',exact:true}).isDisabled(),true);
 outage=true;await page.getByRole('button',{name:'刷新',exact:true}).click();await page.getByText('库存暂时不可用',{exact:true}).waitFor();assert.match(await page.locator('#personal-list').innerText(),/小王/);
 assert.ok(await page.evaluate(()=>document.documentElement.scrollWidth<=innerWidth+1));
 await fs.mkdir('/tmp/offer-delivery-acceptance',{recursive:true});await page.screenshot({path:'/tmp/offer-delivery-acceptance/personal-one-time-outage.png',fullPage:true});
 await page.emulateMedia({colorScheme:'dark'});await page.screenshot({path:'/tmp/offer-delivery-acceptance/personal-one-time-dark.png',fullPage:true});
 await page.evaluate(()=>{state.offerData.writesEnabled=true;});
 await page.getByRole('button',{name:'准备一次性码库存',exact:true}).click();
 await page.locator('[data-delivery-pool="batch-500"]').waitFor();
 assert.equal(await page.locator('[data-delivery-pool]').count(),1,'personal inventory must exclude shared and sandbox pools');
 await page.getByRole('button',{name:'新建一次性码批次',exact:true}).click();
 assert.equal(await page.locator('#codes-form [name=kind]').inputValue(),'oneTime');
 assert.equal(await page.locator('#codes-form [name=environment]').inputValue(),'PRODUCTION');
 assert.equal(await page.locator('#codes-form [name=numberOfCodes]').inputValue(),'500');
 assert.deepEqual(errors,[]);
 console.log('PASS one-time stock allocation, stable typing, lost-response retry, claim/redemption distinction, stock exhaustion and outage');
}finally{await browser.close();server.close();}
