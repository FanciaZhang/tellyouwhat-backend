// Deterministic browser regression for offer identity, environments and unavailable metrics.
import assert from 'node:assert/strict';
import fs from 'node:fs/promises';
import http from 'node:http';
import path from 'node:path';
const {chromium}=await import(process.env.PLAYWRIGHT_MODULE||'playwright');
const root=path.resolve('internal/adminui/static');
const server=http.createServer(async(req,res)=>{try{const file=path.join(root,req.url==='/'?'index.html':req.url);res.setHeader('Content-Security-Policy',"default-src 'none'; script-src 'self'; style-src 'self'; connect-src 'self'; img-src 'self'; frame-ancestors 'none'; base-uri 'none'; form-action 'self'");res.setHeader('Content-Type',file.endsWith('.js')?'text/javascript':file.endsWith('.css')?'text/css':'text/html');res.end(await fs.readFile(file));}catch{res.writeHead(404);res.end();}});
await new Promise(resolve=>server.listen(0,'127.0.0.1',resolve));
const browser=await chromium.launch({headless:true,executablePath:process.env.CHROME_EXECUTABLE});
const page=await browser.newPage({viewport:{width:390,height:1000}}),errors=[];
page.on('pageerror',e=>errors.push(e.message));
let failMetrics=false;
await page.route('**/api/**',async route=>{
 const url=new URL(route.request().url());let status=200,data={};
 if(url.pathname.endsWith('/session')){status=401;data={error:{message:'login'}};}
 else if(url.pathname.endsWith('/metrics/offers')){
  status=failMetrics?503:200;
  data=failMetrics?{error:{message:'unavailable'}}:{metrics:[{offerIdentifier:'FRIENDS',environment:'production',redemptions:3,lastRedeemedAt:'2026-09-08T00:00:00Z'},{offerIdentifier:'FRIENDS',environment:'sandbox',redemptions:2}]};
 }else if(url.pathname.endsWith('/offers'))data={offers:[{id:'apple-resource-123',name:'FRIENDS',duration:'ONE_MONTH',customerEligibilities:['NEW'],active:true,productionCodeCount:500,sandboxCodeCount:10},{id:'inactive',name:'OLD',active:false,productionCodeCount:0,sandboxCodeCount:0}],activeCount:1,activeLimit:10,writesEnabled:false,syncedAt:new Date().toISOString()};
 else {status=404;data={error:{message:'missing fixture'}};}
 await route.fulfill({status,contentType:'application/json',body:JSON.stringify(data)});
});
try{
 await page.goto(`http://127.0.0.1:${server.address().port}`,{waitUntil:'networkidle'});
 await page.evaluate(async()=>{state.currentApp='health';$('#auth').classList.add('hidden');$('#workspace').classList.remove('hidden');await loadOffers();});
 assert.equal(await page.locator('#generated-count').innerText(),'500');
 assert.equal(await page.locator('#redemption-count').innerText(),'3');
 assert.match(await page.locator('#offers .offer').first().innerText(),/已验证核销订阅 3 个/);
 assert.equal(await page.getByRole('button',{name:'查看码池',exact:true}).count(),2,'inactive and read-only offers remain inspectable');
 await page.locator('#offer-environment').selectOption('sandbox');
 assert.equal(await page.locator('#generated-count').innerText(),'10');
 assert.equal(await page.locator('#redemption-count').innerText(),'2');
 failMetrics=true;await page.evaluate(()=>loadOffers());
 assert.equal(await page.locator('#redemption-count').innerText(),'暂不可用');
 assert.match(await page.locator('#offers').innerText(),/统计暂不可用/);
 assert.doesNotMatch(await page.locator('#offers').innerText(),/已验证核销订阅 0/);
 assert.ok(await page.evaluate(()=>document.documentElement.scrollWidth<=innerWidth+1));
 assert.deepEqual(errors,[]);
 console.log('PASS reference-name matching, 500 generated / 3 observed, environment split, read-only inventory, unavailable metrics and mobile layout');
}finally{await browser.close();server.close();}
