import assert from 'node:assert/strict';
import fs from 'node:fs/promises';
import http from 'node:http';
import path from 'node:path';
const {chromium}=await import(process.env.PLAYWRIGHT_MODULE||'playwright');
const root=path.resolve('internal/adminui/static');
const server=http.createServer(async(req,res)=>{res.setHeader('Content-Security-Policy',"default-src 'none'; script-src 'self'; style-src 'self'; connect-src 'self'; img-src 'self'; frame-ancestors 'none'; base-uri 'none'; form-action 'self'");try{const file=path.join(root,req.url==='/'?'index.html':req.url);res.setHeader('Content-Type',file.endsWith('.js')?'text/javascript':file.endsWith('.css')?'text/css':'text/html');res.end(await fs.readFile(file));}catch{res.writeHead(404);res.end();}});
await new Promise(resolve=>server.listen(0,'127.0.0.1',resolve));
const browser=await chromium.launch({headless:true,executablePath:process.env.CHROME_EXECUTABLE});
const params={model:'ep-journal',reasoningEffort:'disabled',temperature:null,maxOutputTokens:8192,timeoutSeconds:90};
const policy={systemPrompt:'',journal:{organize:{prompt:'只整理原文明示的事实。',lite:{...params},pro:{...params},liteMaxCharacters:6000,liteMaxBooks:24,liteMaxTags:80},voice:{prompt:'保留手动修改，只有后续口述才能纠正已修改内容。'.repeat(40),removeRepetition:true,parameters:{...params},automaticPunctuation:true,normalizeNumbers:true},defaultStyle:'natural',styles:['自然记录','轻快活泼','严谨纪实','日常流水','散文随笔'].map((name,i)=>({id:i===0?'natural':'style-'+i,name,description:'理顺表达，保留自己的语气。',example:'下班后去了河边散步。',prompt:'保留已有细节。',enabled:true,order:i}))}};
const current={id:'11111111-1111-4111-8111-111111111111',scope:'journal',policy,createdAt:new Date().toISOString(),publishedAt:new Date().toISOString()};const draft={...structuredClone(current),id:'22222222-2222-4222-8222-222222222222',publishedAt:null,baseVersion:current.id};
const samples=[{id:'10000000-0000-4000-8000-000000000001',name:'合成 Lite',kind:'organize_lite',organize:{requestID:'20000000-0000-4000-8000-000000000001',contractVersion:'journal-organize-v1',contentHash:'a'.repeat(64),title:'河边',body:'和小林一起散步。',existingTags:[],rejectedTagNames:[],books:[]},expected:[]},{id:'10000000-0000-4000-8000-000000000002',name:'合成 Pro',kind:'organize_pro',expected:[]},{id:'10000000-0000-4000-8000-000000000003',name:'手改与晚到音频',kind:'voice',voice:{writingStyle:'natural',revision:1,blocks:[{id:'30000000-0000-4000-8000-000000000001',text:'我和小林散步。'}],transcript:'我和小明散步',editedBlockIDs:[],mediaOnlyBlockIDs:[],manualEdits:[],words:[]},expected:[]}];
const plan={samples,candidates:[{label:'当前发布版',revision:current},{label:'候选版本',revision:draft}],judge:params,rubric:'journal-fidelity-2026-09-08',maximumCalls:9,reservedNanos:5e9,requests:Array.from({length:6},()=>({model:'ep-journal',input:'合成文本',instructions:'保留手动编辑。'.repeat(100)}))};
let runs=[],saved=[],startCount=0,cancelled=false;
const run={id:'40000000-0000-4000-8000-000000000001',status:'completed',createdAt:new Date().toISOString(),expiresAt:new Date().toISOString(),plan,items:samples.map((_,index)=>({index,status:'completed'}))};
const outputs=[0,1].map(i=>({candidate:i,configVersion:i?draft.id:current.id,model:'doubao-seed-actual-'+i,inputTokens:500,outputTokens:200,costNanos:500000,milliseconds:1450,request:plan.requests[i],structured:{patches:[]},text:i?'我和小林到河边散步，买了一杯热茶。':'我和小林在河边散步。',checks:[{name:'生产协议与正文约束',passed:true,detail:''}]}));
const context=await browser.newContext({viewport:{width:390,height:844}});const page=await context.newPage();const errors=[];page.on('pageerror',e=>errors.push(e.message));
await page.route('**/api/**',async route=>{const request=route.request(),u=new URL(request.url());let data,status=200;
 if(u.pathname==='/api/v1/session'){status=401;data={error:{message:'login'}};}
 else if(u.pathname==='/api/v1/ai/health')data={operations:[],writesEnabled:true};
 else if(u.pathname==='/api/v1/ai/prompts')data={current,writesEnabled:true};
 else if(u.pathname.endsWith('/prompts/history'))data={revisions:[draft,current]};
 else if(u.pathname.endsWith('/evaluations/samples')&&request.method()==='GET')data={builtins:samples,samples:saved,retentionDays:7};
 else if(u.pathname.endsWith('/evaluations/samples')){data=request.postDataJSON();saved.push(data);}
 else if(u.pathname.includes('/evaluations/samples/'))data=samples.find(s=>s.id===u.pathname.split('/').at(-1));
 else if(u.pathname.endsWith('/evaluations/preview'))data={plan,previewToken:'signed'};
 else if(u.pathname.endsWith('/evaluations/runs')&&request.method()==='POST'){startCount++;runs=[run];data=run;}
 else if(u.pathname.endsWith('/evaluations/runs'))data={runs};
 else if(u.pathname.includes('/items/'))data={index:0,status:'completed',result:{outputs,judgment:{status:'incomplete',model:'judge-pro',rubric:plan.rubric,scores:[],error:'judge_unavailable',inputTokens:0,outputTokens:0,costNanos:0}}};
 else if(u.pathname.endsWith('/cancel')){cancelled=true;run.status='cancelled';data={cancelled:true};}
 else if(u.pathname.includes('/evaluations/runs/'))data=run;
 else{status=404;data={error:{message:'Unhandled '+u.pathname}};}await route.fulfill({status,contentType:'application/json',body:JSON.stringify(data)});
});
try {
 await page.goto(`http://127.0.0.1:${server.address().port}`,{waitUntil:'networkidle'});await page.evaluate(async()=>{state.user={id:'fixture',role:'admin'};state.csrf='fixture';$('#auth').classList.add('hidden');$('#workspace').classList.remove('hidden');$('#ai-section').classList.remove('hidden');$('#operations-section').classList.add('hidden');await loadAI();});
 await page.getByRole('button',{name:'手记 · 效果评测'}).click();await page.getByRole('button',{name:'＋ 添加样例'}).click();const title=page.locator('#eval-editor [name="name"]');await title.fill('');await title.pressSequentially('pro');assert.equal(await title.inputValue(),'pro');await title.evaluate(el=>window.sampleNode=el);const cdp=await context.newCDPSession(page);await cdp.send('Input.imeSetComposition',{text:'中文',selectionStart:2,selectionEnd:2});await cdp.send('Input.insertText',{text:'中文'});assert.equal(await title.inputValue(),'pro中文');assert.ok(await title.evaluate(el=>el===window.sampleNode));await page.locator('[name="organize.body"]').fill('下午去河边散步。');await page.getByRole('button',{name:'保存样例',exact:true}).click();await page.getByRole('heading',{name:'手记 · 效果评测',exact:true}).waitFor();assert.equal(saved.length,1);assert.equal(saved[0].name,'pro中文');
 await page.getByRole('button',{name:'预览请求与费用'}).click();await page.getByRole('heading',{name:'确认本次评测'}).waitFor();assert.ok(await page.getByText('最多 9 次调用',{exact:false}).isVisible());await page.getByText('合成 Lite · 当前发布版',{exact:true}).click();assert.ok(await page.locator('.eval-request').first().isVisible());await page.getByRole('button',{name:'预留费用并开始评测'}).click();await page.getByRole('heading',{name:'评测进度'}).waitFor();assert.equal(startCount,1);await page.locator('[data-eval-item="0"]').click();await page.getByRole('heading',{name:'合成 Lite',exact:true}).waitFor();assert.ok(await page.getByRole('heading',{name:'AI 评分 · 未完成'}).isVisible());await page.locator('[data-eval-output="0"]').click();assert.equal(await page.locator('.eval-output.selected .eval-text').innerText(),'我和小林在河边散步。');await page.locator('[data-eval-output="1"]').click();assert.equal(await page.locator('.eval-output.selected .eval-text').innerText(),'我和小林到河边散步，买了一杯热茶。');
 const out=process.env.PROMPT_UI_SCREENSHOT_DIR;if(out)await fs.mkdir(out,{recursive:true});for(const color of ['light','dark'])for(const width of [390,1100]){await page.emulateMedia({colorScheme:color});await page.setViewportSize({width,height:900});assert.ok(await page.evaluate(()=>document.documentElement.scrollWidth<=innerWidth+1));if(out)await page.screenshot({path:path.join(out,`evaluation-${width}-${color}.png`),fullPage:true});}
 run.status='running';run.items[2].status='pending';await page.locator('#ai-back').click();await page.getByRole('button',{name:'取消剩余评测'}).click();await page.getByRole('button',{name:'返回草稿差异'}).waitFor();assert.ok(cancelled);assert.deepEqual(errors,[]);
 console.log('PASS: explicit sample save, stable typing/IME, request/cost preview, durable run, mobile comparison, incomplete judge, cancellation, CSP and dark/light layout');
}finally{await browser.close();await new Promise(resolve=>server.close(resolve));}
