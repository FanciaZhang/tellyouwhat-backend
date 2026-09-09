import assert from 'node:assert/strict';
import fs from 'node:fs/promises';
import http from 'node:http';
import path from 'node:path';
const {chromium}=await import(process.env.PLAYWRIGHT_MODULE||'playwright');
const root=path.resolve('internal/adminui/static');
const server=http.createServer(async(req,res)=>{try{const file=path.join(root,req.url==='/'?'index.html':req.url);res.setHeader('Content-Security-Policy',"default-src 'none'; script-src 'self'; style-src 'self'; connect-src 'self'; img-src 'self'; frame-ancestors 'none'; base-uri 'none'; form-action 'self'");res.setHeader('Content-Type',file.endsWith('.js')?'text/javascript':file.endsWith('.css')?'text/css':'text/html');res.end(await fs.readFile(file));}catch{res.writeHead(404);res.end();}});
await new Promise(resolve=>server.listen(0,'127.0.0.1',resolve));
const browser=await chromium.launch({headless:true,executablePath:process.env.CHROME_EXECUTABLE});
const page=await browser.newPage({viewport:{width:390,height:920}}),errors=[];page.on('pageerror',e=>errors.push(e.message));page.on('dialog',d=>d.accept());

let item=null,createCalls=0,resumeCalls=0;
const now='2026-09-09T10:00:00Z';
await page.route('**/api/**',async route=>{
 const req=route.request(),url=new URL(req.url());let status=200,data={};
 if(url.pathname.endsWith('/session')){status=401;data={error:{message:'login'}};}
 else if(url.pathname.endsWith('/metrics/offers'))data={metrics:[]};
 else if(url.pathname.endsWith('/offers'))data={inventoryScope:'app',offers:[{id:'friends',name:'朋友体验',productID:'health.premium.subscription.monthly',subscriptionID:'456',active:true,productionCodeCount:500,sandboxCodeCount:0}],writesEnabled:true,creationActiveCount:1,activeCount:1,activeLimit:10,syncedAt:now};
 else if(url.pathname.endsWith('/personal-deliveries')){
  if(req.method()==='GET')data={deliveries:item?[item]:[],reportSync:{state:'ready'}};
  else{
   assert.equal(req.headers()['x-admin-csrf'],'fixture');const b=req.postDataJSON();
   if(b.action==='create'){
    createCalls++;item={delivery:{id:b.requestKey,name:b.name,offerID:'friends',state:'creating',createdAt:now,expiration:b.expiration}};
    status=409;data={error:{code:'personal_code_uncertain',message:'Apple 创建结果待确认，请继续核对。'}};
   }else{
    resumeCalls++;assert.equal(b.action,'resume');assert.equal(b.requestKey,item.delivery.id);
    item.delivery.state='ready';item.delivery.poolID='private-pool';item.delivery.requestID='request-1';
    item.request={id:'request-1',status:'assigned',version:2,recipient:{name:item.delivery.name},claimGeneration:1,claimExpiresAt:'2026-12-08T10:00:00Z'};
    item.appleReport={scope:'custom_code',days:0,redemptions:0};data={...item,token:'health.request-1.1.signature'};
   }
  }
 }else{status=404;data={error:{message:'unknown fixture'}};}
 await route.fulfill({status,contentType:'application/json',body:JSON.stringify(data)});
});
try{
 await page.goto(`http://127.0.0.1:${server.address().port}`,{waitUntil:'networkidle'});
 await page.evaluate(async()=>{state.currentApp='health';state.csrf='fixture';reauthenticate=async()=>{};$('#auth').classList.add('hidden');$('#workspace').classList.remove('hidden');await loadOffers();});
 await page.getByRole('button',{name:'熟人发放',exact:true}).click();
 const name=page.locator('#personal-form [name=name]');await name.pressSequentially('pro');assert.equal(await name.inputValue(),'pro');await name.fill('小王');
 assert.equal(await page.locator('#personal-form input').count(),2,'admin needs only nickname and expiry');
 await page.getByRole('button',{name:'创建专属领取链接',exact:true}).click();
 await page.getByRole('button',{name:'继续核对',exact:true}).waitFor();
 assert.equal(createCalls,1);
 await page.getByRole('button',{name:'继续核对',exact:true}).click();
 await page.locator('#personal-link').waitFor();
 assert.match(await page.locator('#personal-link').inputValue(),/offer-claim#receipt=/);
 await page.locator('#delivery-secret-dialog').getByRole('button',{name:'关闭',exact:true}).click();
 assert.equal(createCalls,1);assert.equal(resumeCalls,1);
 assert.match(await page.locator('#personal-list').innerText(),/尚未领取/);
 assert.match(await page.locator('#personal-list').innerText(),/实际兑换：待 Apple 日报确认/);
 assert.doesNotMatch(await page.locator('#personal-list').innerText(),/兑换 0 次/);
 item.request.deliveredAt=now;
 await page.getByRole('button',{name:'刷新记录',exact:true}).click();
 await page.getByText('已发出，尚未通过链接领取',{exact:true}).waitFor();
 item.request.claimedAt=now;item.appleReport={scope:'custom_code',days:1,redemptions:1,lastDay:'2026-09-08'};
 await page.getByRole('button',{name:'刷新记录',exact:true}).click();
 await page.getByText('Apple 已确认：这枚码兑换 1 次',{exact:true}).waitFor();
 assert.match(await page.locator('#personal-list').innerText(),/已领取/);
 assert.ok(await page.evaluate(()=>document.documentElement.scrollWidth<=innerWidth+1));
 await fs.mkdir('/tmp/offer-delivery-acceptance',{recursive:true});await page.screenshot({path:'/tmp/offer-delivery-acceptance/personal.png',fullPage:true});
 assert.deepEqual(errors,[]);
 console.log('PASS personal issuance, stable typing, uncertain creation recovery, link without recipient form, manual delivery / claim / Apple redemption separation');
}finally{await browser.close();server.close();}
