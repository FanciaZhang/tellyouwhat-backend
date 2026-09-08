// Browser acceptance for AI interactions with deterministic management fixtures.
// Cloud protocol and durable command behavior are exercised separately in Go.
import assert from 'node:assert/strict';
import fs from 'node:fs/promises';
import http from 'node:http';
import path from 'node:path';
const {chromium}=await import(process.env.PLAYWRIGHT_MODULE||'playwright');
const root=path.resolve('internal/adminui/static');
const server=http.createServer(async(req,res)=>{try{const file=path.join(root,req.url==='/'?'index.html':req.url);const content=await fs.readFile(file);res.setHeader('Content-Type',file.endsWith('.js')?'text/javascript':file.endsWith('.css')?'text/css':'text/html');res.end(content);}catch{res.writeHead(404);res.end();}});
await new Promise(resolve=>server.listen(0,'127.0.0.1',resolve));
const browser=await chromium.launch({headless:true,executablePath:process.env.CHROME_EXECUTABLE});
const operations=['voice_transcription','meal_photo_capture','hydration_cup_estimate','meal_text_capture','meal_decision','diet_analysis','health_nutrition_analysis','health_behavior_analysis'];
const source={Name:'doubao-seed-2-0-mini',ModelVersion:'260428'},target={Name:'doubao-seed-2-1-pro',ModelVersion:'260628'};
const price={InputNanosPerMillionTokens:800000000,OutputNanosPerMillionTokens:8000000000};
const models=[{Name:target.Name,DisplayName:'Doubao Seed 2.1 Pro'},{Name:'deepseek-v4-pro-ga',DisplayName:'DeepSeek V4 Pro'},{Name:'doubao-seedream-5-0-pro',DisplayName:'Seedream 5 Pro'},...Array.from({length:92},(_,i)=>({Name:`model-${i}`,DisplayName:`Directory model ${i}`}))];
const rows=operations.map((operation,i)=>({operation,endpointID:`ep-${i}`,endpoint:{Id:`ep-${i}`,Name:`接入点 ${i}`,Status:'Running',ModelReference:{FoundationModel:source}},history:[],syncedAt:new Date().toISOString()}));
let rolling=null,commands=[],lastInput=null,checks=0,posts=0,draft;
const context=await browser.newContext({viewport:{width:1280,height:1000}}),page=await context.newPage(),errors=[];
page.on('pageerror',e=>errors.push(e.message));page.on('dialog',d=>d.accept());
await page.route('**/api/**',async route=>{
 const request=route.request(),url=new URL(request.url()),pathname=url.pathname;let data={},status=200;
 if(pathname==='/api/v1/auth/session'){status=401;data={error:{message:'login'}};}
 else if(pathname==='/api/v1/ai/health')data={operations:rows,ownedEndpoints:rows.map(r=>r.endpointID),writesEnabled:true,rollingWritesEnabled:true,timeoutSeconds:90};
 else if(pathname==='/api/v1/ai/models')data={models,syncedAt:new Date().toISOString()};
 else if(pathname.endsWith('/versions')){const name=decodeURIComponent(pathname.split('/')[5]);data={versions:[{FoundationModelName:name,ModelVersion:'260628',Status:'Published'}],activation:{value:[{FoundationModelName:name,State:'Available',ChargeItems:[{Type:'InferencePrompt',Price:'0.001',UnitCode:'千tokens'},{Type:'InferenceCompletion',Price:'0.01',UnitCode:'千tokens'}]}],syncedAt:new Date().toISOString()}};}
 else if(pathname.startsWith('/api/v1/ai/endpoints/'))data={endpoint:rows.find(r=>r.endpointID===pathname.split('/').at(-1)).endpoint,rolling,commands,writesEnabled:true,syncedAt:new Date().toISOString(),attempts:[]};
 else if(pathname.endsWith('/drafts')){const body=request.postDataJSON();draft={id:'draft-1',baseVersion:body.baseVersion,operation:body.operation,policy:body.policy,createdAt:new Date().toISOString()};data=draft;}
 else if(pathname.includes('/revisions/'))data={revision:draft,before:null,after:draft.policy,canPublish:true,previewToken:'parameter-preview'};
 else if(pathname.endsWith('/publish')){const row=rows.find(r=>r.operation===draft.operation);row.current={...draft,publishedAt:new Date().toISOString()};row.history=[row.current];data=row.current;}
 else if(pathname.endsWith('/rolling/preview')){
  const input=request.postDataJSON();lastInput=input;
  if(input.target?.Name?.includes('seedream')){status=422;data={error:{code:'ai_check_failed',message:'此模型用于图像生成，当前功能需要结构化理解结果'}};}
  else if(input.action==='start'&&checks++===0){status=202;data={checking:true};}
  else data={input,previewToken:'rolling-preview',snapshot:{endpoint:rows[3].endpoint,rolling,price,currentPrice:price,targetPrice:price},affectedOperations:['meal_text_capture'],notice:'灰度由火山自动推进；完成后切回会开始新的灰度。'};
 }
 else if(pathname.endsWith('/rolling/commands')){
  posts++;const input=request.postDataJSON().input;
  if(input.action==='start'){rolling={Id:'eprol-fixture',EndpointId:'ep-3',RollingIn:input.target,RollingOut:source,RollingGray:90,Status:'Running'};commands=[{id:'command-1',input,before:{endpoint:structuredClone(rows[3].endpoint)},state:'watching',createdAt:new Date().toISOString()}];}
  if(input.action==='step_back')rolling.RollingGray=88;
  if(input.action==='cancel'){rolling.RollingGray=0;rolling.Status='Reverted';commands[0].state='cancelled';}
  data={id:'command-1',state:'queued'};
 }
 else {status=404;data={error:{message:'unhandled fixture '+pathname}};}
 await route.fulfill({status,contentType:'application/json',body:JSON.stringify(data)});
});
async function waitHeading(text){await page.getByRole('heading',{name:text,exact:true}).waitFor();}
async function settle(){await page.waitForFunction(()=>document.querySelector('#ai-view').getAttribute('aria-busy')!=='true');}
async function overflow(){assert.ok(await page.evaluate(()=>document.documentElement.scrollWidth<=innerWidth+1),'horizontal overflow');}
try{
 await page.goto(`http://127.0.0.1:${server.address().port}`);
 await page.evaluate(async()=>{state.user={id:'fixture'};state.csrf='fixture';window.passkeyChecks=0;reauthenticate=async()=>{window.passkeyChecks++;};$('#auth').classList.add('hidden');$('#workspace').classList.remove('hidden');$('#ai-section').classList.remove('hidden');$('#operations-section').classList.add('hidden');await loadAI();});
 assert.equal(await page.locator('[data-ai-row]').count(),8);
 await page.locator('[data-ai-params="4"]').click();await page.locator('[name=reasoningEffort]').selectOption('high');await page.locator('[name=webSearchEnabled]').check();await page.getByRole('button',{name:'预览变更',exact:true}).click();await waitHeading('确认参数变更');
 assert.equal(await page.evaluate(()=>window.passkeyChecks),0);
 await page.getByRole('button',{name:'通行密钥确认并发布'}).click();await waitHeading('AI 管理');assert.equal(rows[4].current.policy.reasoningEffort,'high');assert.equal(await page.evaluate(()=>window.passkeyChecks),1);
 await page.locator('[data-ai-switch="ep-3"]').click();await page.locator('[data-ai-model]').first().waitFor();await page.getByRole('searchbox').fill('DeepSeek');assert.equal(await page.locator('[data-ai-model]').count(),1);await page.getByRole('searchbox').fill('Seedream');await page.locator('[data-ai-model]').click();await settle();await page.getByRole('button',{name:'检查并预览'}).click();await page.locator('#ai-error').waitFor();assert.equal(posts,0);
 await page.getByRole('button',{name:'← 返回',exact:true}).click();await page.getByRole('searchbox').fill('2.1');await page.locator('[data-ai-model]').click();await settle();await page.getByRole('button',{name:'检查并预览'}).click();await waitHeading('确认模型切换');assert.equal(await page.evaluate(()=>window.passkeyChecks),1);
 await page.getByRole('button',{name:'通行密钥确认并开始'}).dblclick();await waitHeading('正在切换模型');assert.equal(posts,1);assert.equal(await page.evaluate(()=>window.passkeyChecks),2);
 assert.equal(await page.getByRole('progressbar').getAttribute('aria-valuenow'),'90');await page.getByRole('button',{name:'回退一个阶段',exact:true}).click();await waitHeading('回退一个阶段');await page.getByRole('button',{name:'通行密钥确认并提交'}).click();await waitHeading('正在切换模型');assert.equal(await page.getByRole('progressbar').getAttribute('aria-valuenow'),'88');
 rolling.RollingGray=100;rows[3].endpoint.ModelReference.FoundationModel=target;commands[0].state='succeeded';
 await page.getByRole('button',{name:'立即刷新'}).click();await waitHeading('模型切换已完成');assert.equal(await page.getByRole('button',{name:'回退一个阶段',exact:true}).count(),0);await page.getByRole('button',{name:'切回原模型',exact:true}).click();await waitHeading('检查切换条件');assert.ok(await page.locator('#ai-view').innerText().then(t=>t.includes(source.Name)));
 await page.getByRole('button',{name:'← 返回',exact:true}).click();await page.getByRole('button',{name:'← 返回',exact:true}).click();await waitHeading('AI 管理');
 for(const width of [360,390,736,1280]){for(const colorScheme of ['light','dark']){await page.setViewportSize({width,height:1000});await page.emulateMedia({colorScheme});await overflow();if(process.env.AI_UI_SCREENSHOT_DIR){await fs.mkdir(process.env.AI_UI_SCREENSHOT_DIR,{recursive:true});await page.screenshot({path:path.join(process.env.AI_UI_SCREENSHOT_DIR,`overview-${width}-${colorScheme}.png`),fullPage:true});}}}
 await page.getByRole('tab',{name:'模型与接入点',exact:true}).click();await settle();assert.equal(await page.locator('.ai-row').count(),8);await page.getByRole('tab',{name:'变更记录',exact:true}).click();await settle();assert.ok(await page.locator('.ai-history-row').count()>=2);
 assert.deepEqual(errors,[]);console.log('PASS overview, full catalog, incompatibility, one Passkey, publish, duplicate guard, 90% rollback, completion/reverse, history and responsive light/dark layouts');
}finally{await browser.close();server.close();}
