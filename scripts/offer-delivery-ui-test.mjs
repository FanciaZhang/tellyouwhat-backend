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
const records=[],commands=[];let imported=0,available=0,external=0;const now='2026-09-09T08:00:00Z';let failSync=false;
await page.route('**/api/**',async route=>{
 const request=route.request(),url=new URL(request.url());let status=200,data={};
 if(url.pathname.endsWith('/session')){status=401;data={error:{message:'login'}};}
 else if(url.pathname.endsWith('/metrics/offers'))data={metrics:[{offerIdentifier:'九月朋友体验',environment:'production',redemptions:3}]};
 else if(url.pathname.endsWith('/offers'))data={offers:[{id:'offer-1',name:'九月朋友体验',duration:'ONE_MONTH',active:true,productionCodeCount:500,sandboxCodeCount:0}],activeCount:1,activeLimit:10,writesEnabled:false,syncedAt:now};
 else if(url.pathname.endsWith('/code-pools'))data={codePools:[{id:'pool-1',kind:'oneTime',environment:'PRODUCTION',active:true,numberOfCodes:500,expirationDate:'2030-01-01'}]};
 else if(url.pathname.endsWith('/delivery')){
  if(request.method()==='POST'){
   const b=request.postDataJSON();commands.push(b);const r=records.find(r=>r.id===b.requestID);
   switch(b.action){
   case 'sync':if(failSync){status=503;data={error:{message:'Apple 暂不可用'}};}break;
   case 'import':imported=500;external=500;break;
   case 'confirm_inventory':assert.equal(b.expectedCount,500);available=500;external=0;break;
   case 'request':data={id:'request-1',poolID:'pool-1',recipient:b.recipient,status:'requested',version:1,requestedAt:now};records.push(data);break;
   case 'assign':r.status='assigned';r.version++;r.assignedAt=now;available--;data=r;break;
   case 'reveal':data={code:'FIXTURE123'};break;
   case 'deliver':r.status='delivered';r.version++;r.deliveredAt=now;data=r;break;
   case 'report_redeemed':r.version++;r.reportedRedeemedAt=now;data=r;break;
   default:throw new Error('missing fixture command '+b.action);
   }
  }else{
   const q=(url.searchParams.get('q')||'').toLowerCase();data={pool:{id:'pool-1',offerID:'offer-1',offerName:'九月朋友体验',kind:'oneTime',capacity:500,active:true,environment:'production',expiresAt:'2030-01-01T00:00:00Z',syncedAt:now},summary:{requests:records.length,applications:records.length,externalDeliveries:0,pending:records.filter(r=>r.status==='requested').length,assignedRequests:records.filter(r=>r.assignedAt).length,assignedCodes:records.filter(r=>r.assignedAt).length,imported,available,external,delivered:records.filter(r=>r.deliveredAt).length,reportedRedeemed:records.filter(r=>r.reportedRedeemedAt).length,linkedVerified:0},observedOfferSubscriptions:3,requests:records.filter(r=>JSON.stringify(r.recipient).toLowerCase().includes(q)),events:[],verifiedSubscriptions:[],stale:failSync};
  }
 }else {status=404;data={error:{message:'missing fixture'}};}
 await route.fulfill({status,contentType:'application/json',body:JSON.stringify(data)});
});
try{
 await page.goto(`http://127.0.0.1:${server.address().port}`,{waitUntil:'networkidle'});
 await page.evaluate(async()=>{state.currentApp='health';state.csrf='fixture';$('#auth').classList.add('hidden');$('#workspace').classList.remove('hidden');await loadOffers();});
 await page.getByRole('button',{name:'查看码池',exact:true}).click();
 await page.getByRole('button',{name:'管理发放',exact:true}).waitFor();
 assert.equal(await page.locator('#delivery-create-pool').isDisabled(),true);
 await page.getByRole('button',{name:'管理发放',exact:true}).click();
 await page.locator('#delivery-ledger-count').filter({hasText:'0 条申请'}).waitFor();
 await page.getByText('库存管理与统计口径',{exact:true}).click();
 await page.getByRole('button',{name:'从 Apple 导入完整码池'}).click();
 await page.locator('#delivery-inventory').filter({hasText:'已导入 500 枚'}).waitFor();
 await page.getByText('确认未发放库存',{exact:true}).click();
 await page.locator('#delivery-confirm input').fill('500');
 await page.getByRole('button',{name:'确认加入可分配库存'}).click();
 await page.locator('#delivery-inventory').filter({hasText:'确认可分配 500 枚'}).waitFor();
 await page.getByText('登记领取申请',{exact:true}).click();
 await page.locator('#delivery-request-form [name=name]').pressSequentially('pro');
 assert.equal(await page.locator('#delivery-request-form [name=name]').inputValue(),'pro','typing must preserve cursor');
 await page.locator('#delivery-request-form [name=name]').fill('小林');
 await page.locator('#delivery-request-form [name=contact]').fill('lin@example.test');
 await page.locator('#delivery-request-form [name=channel]').fill('朋友邀请');
 await page.getByRole('button',{name:'保存申请',exact:true}).click();
 await page.locator('#delivery-requests').filter({hasText:'小林'}).waitFor();
 await page.getByRole('button',{name:'分配兑换码',exact:true}).click();
 await page.getByRole('button',{name:'查看兑换码',exact:true}).click();
 assert.equal(await page.locator('#delivery-secret').inputValue(),'FIXTURE123');
 assert.equal(records[0].status,'assigned','reveal must not mark delivered');
 await page.locator('#delivery-secret-dialog').getByRole('button',{name:'关闭',exact:true}).click();
 assert.equal(await page.locator('#delivery-secret').count(),0,'secret must be removed after closing');
 await page.getByRole('button',{name:'记录已发放',exact:true}).click();
 await page.getByRole('button',{name:'记录已兑换反馈',exact:true}).click();
 await page.locator('#delivery-requests').filter({hasText:'领取人反馈已兑换'}).waitFor();
 assert.equal(await page.locator('#delivery-summary .metric').last().innerText(),'Offer 已验证核销\n3');
 assert.match(await page.locator('#delivery-summary').innerText(),/人工关联已验证订阅 0/);
 const search=page.locator('#delivery-search input');await search.pressSequentially('lin');assert.equal(await search.inputValue(),'lin');
 await page.getByRole('button',{name:'搜索',exact:true}).click();await page.locator('#delivery-requests').filter({hasText:'小林'}).waitFor();assert.equal(await search.inputValue(),'lin');
 await search.fill('不存在');await page.getByRole('button',{name:'搜索',exact:true}).click();await page.getByText('没有匹配的领取记录。',{exact:true}).waitFor();
 await search.fill('');await page.getByRole('button',{name:'搜索',exact:true}).click();await page.locator('#delivery-requests').filter({hasText:'小林'}).waitFor();
 assert.equal(await page.locator('#delivery-next').isDisabled(),true);
 assert.ok(await page.evaluate(()=>document.documentElement.scrollWidth<=innerWidth+1),'mobile horizontal overflow');
 await fs.mkdir('/tmp/offer-delivery-acceptance',{recursive:true});await page.screenshot({path:'/tmp/offer-delivery-acceptance/mobile.png',fullPage:true});
 await page.emulateMedia({colorScheme:'dark'});await page.screenshot({path:'/tmp/offer-delivery-acceptance/mobile-dark.png',fullPage:true});
 assert.deepEqual(errors,[]);console.log('PASS named request, inventory confirmation, allocation, secret lifecycle, delivery/feedback distinction, search cursor, mobile and dark mode');
}finally{await browser.close();server.close();}
