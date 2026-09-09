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

let createCalls=0;
const now='2026-09-09T10:00:00Z';
await page.route('**/api/**',async route=>{
 const req=route.request(),url=new URL(req.url());let status=200,data={};
 if(url.pathname.endsWith('/session')){status=401;data={error:{message:'login'}};}
 else if(url.pathname.endsWith('/metrics/offers'))data={metrics:[]};
 else if(url.pathname.endsWith('/offers'))data={inventoryScope:'app',offers:[{id:'friends',name:'朋友体验',productID:'health.premium.subscription.monthly',subscriptionID:'456',active:true,productionCodeCount:500,sandboxCodeCount:0}],writesEnabled:true,creationActiveCount:1,activeCount:1,activeLimit:10,syncedAt:now};
 else if(url.pathname.endsWith('/personal-deliveries')){
  if(req.method()==='GET')data={deliveries:[],reportSync:{state:'not_configured'}};
  else {createCalls++;status=410;data={error:{code:'personal_code_unsupported',message:'unsupported'}};}
 }else{status=404;data={error:{message:'unknown fixture'}};}
 await route.fulfill({status,contentType:'application/json',body:JSON.stringify(data)});
});
try{
 await page.goto(`http://127.0.0.1:${server.address().port}`,{waitUntil:'networkidle'});
 await page.evaluate(async()=>{state.currentApp='health';state.csrf='fixture';reauthenticate=async()=>{};$('#auth').classList.add('hidden');$('#workspace').classList.remove('hidden');await loadOffers();});
 assert.equal(await page.getByRole('button',{name:'熟人发放',exact:true}).count(),0);
 assert.equal(await page.getByRole('button',{name:'查看码池',exact:true}).count(),1);
 await page.evaluate(()=>openPersonalDelivery('friends'));
 assert.equal(await page.locator('#personal-form').count(),0);
 assert.equal(await page.getByRole('button',{name:'创建专属领取链接',exact:true}).count(),0);
 assert.equal(await page.getByRole('button',{name:'继续核对',exact:true}).count(),0);
 assert.match(await page.locator('body').innerText(),/不支持创建限兑一次/);
 assert.equal(createCalls,0);
 assert.deepEqual(errors,[]);
 console.log('PASS unsupported single-redemption creation is absent from UI and cannot trigger an Apple write');
}finally{await browser.close();server.close();}
