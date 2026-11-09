import assert from 'node:assert/strict';
import fs from 'node:fs/promises';
import http from 'node:http';
import path from 'node:path';
const {chromium}=await import(process.env.PLAYWRIGHT_MODULE||'playwright');
const root=path.resolve('internal/adminui/static');
const server=http.createServer(async(req,res)=>{try{const name=req.url==='/offer-claim'?'offer-claim.html':req.url;const file=path.join(root,name);res.setHeader('Content-Security-Policy',"default-src 'none'; script-src 'self'; style-src 'self'; connect-src 'self'; img-src 'self'; frame-ancestors 'none'; base-uri 'none'; form-action 'self'");res.setHeader('Content-Type',file.endsWith('.js')?'text/javascript':file.endsWith('.css')?'text/css':'text/html');res.end(await fs.readFile(file));}catch{res.writeHead(404);res.end();}});
await new Promise(resolve=>server.listen(0,'127.0.0.1',resolve));
const browser=await chromium.launch({headless:true,executablePath:process.env.CHROME_EXECUTABLE});const page=await browser.newPage({viewport:{width:390,height:850}}),errors=[];page.on('pageerror',e=>errors.push(e.message));
const receipt='health.fixture-receipt.1.signature';let status='assigned',reported=null,claims=0;
await page.context().addCookies([{name:'unrelated_admin_cookie',value:'fixture',domain:'127.0.0.1',path:'/'}]);
await page.route('**/api/v1/offer-claim',async route=>{
 const req=route.request(),inbound=req.postDataJSON();assert.equal(req.headers().cookie,undefined,'public requests must omit admin cookies');assert.equal(new URL(req.url()).search,'','tokens and personal details must not enter query strings');
 let httpStatus=200,data={};
 assert.deepEqual(Object.keys(inbound).sort(),['action','token'],'recipient must not submit personal information');assert.equal(inbound.token,receipt);
 switch(inbound.action){
 case 'status':data={title:'九月朋友体验',status,receiptExpiresAt:'2030-01-01T00:00:00Z',reportedRedeemedAt:reported};break;
 case 'claim':claims++;status='delivered';data={code:'FIXTURE123',status};if(claims===1){httpStatus=503;data={error:{message:'暂时无法确认结果，请重试'}};}break;
 case 'feedback':assert.equal(status,'delivered');reported='2026-09-09T00:00:00Z';data={status,reportedRedeemedAt:reported};break;
 default:throw new Error('unexpected form submission '+inbound.action);
 }
 await route.fulfill({status:httpStatus,contentType:'application/json',body:JSON.stringify(data)});
});
try{
 await page.goto(`http://127.0.0.1:${server.address().port}/offer-claim#receipt=${receipt}`,{waitUntil:'networkidle'});
 assert.equal(await page.locator('form').count(),0,'recipient must not fill out a form');assert.equal(await page.locator('input,textarea').count(),0);
 await page.getByRole('button',{name:'领取并查看兑换码'}).click();await page.locator('#claim-message').filter({hasText:'请重试'}).waitFor();assert.equal(await page.locator('#claim-code').count(),0);
 await page.getByRole('button',{name:'领取并查看兑换码'}).click();assert.equal(await page.locator('#claim-code').inputValue(),'FIXTURE123');assert.equal(claims,2);
 await page.getByRole('button',{name:'我已成功兑换',exact:true}).click();await page.getByText('已记录你的反馈，谢谢。',{exact:true}).waitFor();assert.equal(status,'delivered');
 assert.equal(await page.locator('#claim-code').count(),0);assert.ok(await page.evaluate(()=>document.documentElement.scrollWidth<=innerWidth+1));
 await fs.mkdir('/tmp/offer-delivery-acceptance',{recursive:true});await page.screenshot({path:'/tmp/offer-delivery-acceptance/public-receipt.png',fullPage:true});assert.deepEqual(errors,[]);
 console.log('PASS no personal-information form or submission, direct claim, uncertain-response retry, private code and separate redemption feedback');
}finally{await browser.close();server.close();}
