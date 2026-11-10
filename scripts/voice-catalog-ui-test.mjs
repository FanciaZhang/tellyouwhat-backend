// Deterministic UI fixtures only; this does not prove vendor or deployed-admin acceptance.
import assert from 'node:assert/strict';
import fs from 'node:fs/promises';
import http from 'node:http';
import path from 'node:path';
const {chromium}=await import(process.env.PLAYWRIGHT_MODULE||'playwright');
const root=path.resolve('internal/adminui/static');
const csp="default-src 'none'; script-src 'self'; style-src 'self'; connect-src 'self'; img-src 'self'; media-src 'self' blob: https:; frame-ancestors 'none'; base-uri 'none'; form-action 'self'";
const server=http.createServer(async(req,res)=>{try{
 const file=path.join(root,req.url==='/'?'index.html':req.url);
 res.setHeader('Content-Security-Policy',csp);res.setHeader('Content-Type',file.endsWith('.js')?'text/javascript':file.endsWith('.css')?'text/css':'text/html');
 let body=await fs.readFile(file);if(file.endsWith('index.html'))body=Buffer.from(body.toString().replace('Tellyouwhat 管理后台','Tellyouwhat 测试管理后台').replace('<h1>管理后台</h1>','<h1>测试管理后台</h1>').replace('<p class="eyebrow">Tellyouwhat</p>','<p class="eyebrow">Tellyouwhat · 测试环境</p>'));res.end(body);
}catch{res.writeHead(404);res.end();}});
await new Promise(resolve=>server.listen(0,'127.0.0.1',resolve));
const browser=await chromium.launch({headless:true,executablePath:process.env.CHROME_EXECUTABLE});
const context=await browser.newContext({viewport:{width:390,height:844}});
const page=await context.newPage(),errors=[];
page.on('pageerror',error=>errors.push(error.message));page.on('dialog',dialog=>dialog.accept());
const at='2026-10-20T12:00:00Z';
const proof={policySHA256:'fixture-policy',audioSHA256:'fixture-audio',verifiedAt:at,cases:1};
const voice=(id,name,extra={})=>({id,name,description:'适合安静地听一篇手记',languages:['zh-cn'],styles:['natural'],dialects:[],supportsPitch:false,previewURL:'',billingClass:'unreviewed',enabled:false,rank:100,probeText:'今天沿着河边散步。',...extra});
let current={version:'version-one',defaultVoiceID:'voice_one',voices:[voice('voice_one','小何 <script>',{billingClass:'standard',enabled:true,verification:proof}),voice('voice_two','Vivi')],createdAt:at,publishedAt:at};
const initial=structuredClone(current),drafts=[],probes=new Map();
let actualProbeCalls=0,probeHTTPCalls=0,publishCalls=0,lastPublication,firstProbeResponseLost=true;
let heldProbe,releaseProbe,probeArrived;
let isolatedAdmin=true;
const registeredApps=[{id:'health',displayName:'告你健康'},{id:'journal',displayName:'告你手记'},{id:'birthday',displayName:'告你生日'},{id:'albums',displayName:'告你相册'}],offerAppReads=[];
const base='/api/v1/journal/read-aloud/catalog';
await page.route('**/api/**',async route=>{
 const request=route.request(),u=new URL(request.url());let data,status=200;
 if(u.pathname==='/api/v1/session'){if(isolatedAdmin)data={csrfToken:'fixture-csrf',recentlyReauthenticated:true,user:{id:'fixture',displayName:'测试管理员',role:'admin'}};else{status=401;data={error:{message:'fixture login'}};}}
 else if(u.pathname==='/api/v1/apps')data={apps:registeredApps};
 else if(/^\/api\/v1\/apps\/(health|journal|birthday|albums)\/offers$/.test(u.pathname)){offerAppReads.push(u.pathname.split('/')[4]);status=503;data={error:{code:'offer_service_unavailable',message:'此环境未配置 App Store Connect'}};}
 else if(u.pathname.endsWith('/metrics/offers'))data={metrics:[]};
 else if(u.pathname==='/api/v1/ai/health'){if(isolatedAdmin){status=503;data={error:{code:'ai_not_configured',message:'尚未配置火山管理服务身份'}};}else data={operations:[],writesEnabled:true};}
 else if(u.pathname===base)data={current,writesEnabled:true,syncEnabled:true,probesEnabled:true};
 else if(u.pathname===base+'/sync')data={voices:[voice('voice_two','不要覆盖本机修改'),voice('voice_three','云舟')],issues:[],providerCount:2};
 else if(u.pathname===base+'/probe'){
  probeHTTPCalls++;assert.equal(request.headers()['x-admin-csrf'],'fixture-csrf');
  const key=request.headers()['idempotency-key'];assert.ok(key);
  const body=request.postDataJSON();assert.equal(body.verification,undefined);assert.equal(body.resourceID,undefined);
  if(!probes.has(key)){actualProbeCalls++;probes.set(key,{voice:{...body,verification:{...proof,cases:5}},sampleRate:24000});}
  if(body.id==='cancel_voice_1'&&heldProbe){probeArrived();await heldProbe;}
  data={...probes.get(key),replayed:probeHTTPCalls>1};
  if(firstProbeResponseLost||body.id.startsWith('unavailable_voice_')){firstProbeResponseLost=false;status=503;data={error:{message:'fixture: response was lost'}};}
 }
 else if(u.pathname===base+'/drafts'){
  const body=request.postDataJSON();assert.ok(body.voices.every(v=>v.verification===undefined&&v.resourceID===undefined));
  data={...body,version:'draft-'+drafts.length,createdAt:at,voices:body.voices.map(v=>({...v,...((v.id==='voice_one'||v.id==='voice_two')?{verification:proof}:{})}))};drafts.push(structuredClone(data));
 }
 else if(u.pathname===base+'/publish'){publishCalls++;const body=request.postDataJSON();assert.equal(body.baseVersion,current?.version||'');lastPublication=body;const draft=drafts.find(d=>d.version===body.revision);current={...draft,publishedAt:at,voices:draft.voices.map(v=>({...v,...(body.skipSynthesisVerification&&v.enabled&&!v.verification?{approvedPolicySHA256:'fixture-admin-approval'}:{})}))};data=current;}
 else if(u.pathname===base+'/history')data={revisions:[current,initial].map(({version,baseVersion,createdAt,publishedAt,voices})=>({version,baseVersion,createdAt,publishedAt,voiceCount:voices.length,enabledCount:voices.filter(v=>v.enabled).length})),nextCursor:''};
 else if(u.pathname.startsWith(base+'/revisions/')){const id=decodeURIComponent(u.pathname.split('/').at(-1));data=[initial,...drafts,current].find(v=>v.version===id);assert.ok(data,'missing revision fixture');}
 else{status=404;data={error:{message:'Unhandled fixture '+u.pathname}};}
 await route.fulfill({status,contentType:'application/json',body:JSON.stringify(data)});
});
const screenshots=process.env.VOICE_UI_SCREENSHOT_DIR;if(screenshots)await fs.mkdir(screenshots,{recursive:true});
async function capture(name){if(screenshots)await page.screenshot({path:path.join(screenshots,name+'.png')});}
async function captureViewport(name){if(screenshots)await page.screenshot({path:path.join(screenshots,name+'.png')});}
async function geometry(){
 assert.ok(await page.evaluate(()=>document.documentElement.scrollWidth<=innerWidth+1),'horizontal overflow');
 const header=await page.locator('.topbar').evaluate(el=>[...el.children].map(child=>child.getBoundingClientRect()).filter(r=>r.width&&r.height).map(r=>({x:r.x,y:r.y,w:r.width,h:r.height})));
 for(let i=0;i<header.length;i++)for(let j=i+1;j<header.length;j++){const a=header[i],b=header[j];assert.ok(Math.min(a.x+a.w,b.x+b.w)-Math.max(a.x,b.x)<=.5||Math.min(a.y+a.h,b.y+b.h)-Math.max(a.y,b.y)<=.5,'header overlap');}
 if(await page.locator('#app-picker').isVisible()){const picker=await page.locator('#app-picker').boundingBox();assert.ok(picker.height>=44&&picker.x>=0&&picker.x+picker.width<=await page.evaluate(()=>innerWidth),'App selector clipping');}

 const targets=await page.locator('#ai-view button:visible, #ai-view input:visible, #ai-view textarea:visible, #ai-view select:visible').evaluateAll(elements=>elements.map(el=>{
  const target=el.type==='checkbox'?el.closest('label'):el,r=target.getBoundingClientRect();
  return {name:el.name||el.textContent,x:r.x,y:r.y,width:r.width,height:r.height};
 }));
 assert.deepEqual(targets.filter(r=>r.width<43.5||r.height<43.5),[],'small control target');
 const width=await page.evaluate(()=>innerWidth);
 assert.deepEqual(targets.filter(r=>r.x<-.5||r.x+r.width>width+.5),[],'clipped controls');
 for(let i=0;i<targets.length;i++)for(let j=i+1;j<targets.length;j++){
  const a=targets[i],b=targets[j],x=Math.min(a.x+a.width,b.x+b.width)-Math.max(a.x,b.x),y=Math.min(a.y+a.height,b.y+b.height)-Math.max(a.y,b.y);
  assert.ok(x<=.5||y<=.5,'overlapping controls: '+a.name+' / '+b.name);
 }
}
try{
 await page.goto('http://127.0.0.1:'+server.address().port,{waitUntil:'networkidle'});
 await page.getByRole('status').filter({hasText:'Offer 服务尚未启用'}).waitFor();
 assert.equal(await page.locator('#auth').isVisible(),false);assert.equal(await page.locator('#new-offer').isDisabled(),true);
 assert.equal(await page.locator('#offer-service').isVisible(),false);await geometry();
 assert.deepEqual(await page.locator('#app-picker option').evaluateAll(options=>options.map(option=>option.textContent)),registeredApps.map(app=>app.displayName));
 for(const app of registeredApps){
  if((await page.locator('#app-picker').inputValue())!==app.id)await Promise.all([page.waitForResponse(response=>new URL(response.url()).pathname==='/api/v1/apps/'+app.id+'/offers'),page.locator('#app-picker').selectOption(app.id)]);
  assert.equal(await page.locator('#app-picker').inputValue(),app.id);assert.ok(offerAppReads.includes(app.id));await geometry();await capture('unified-admin-'+app.id);
 }
 await capture('isolated-admin-offers-unavailable');
 await page.locator('#ai-tab').click();await page.locator('[data-prompt-open="voices"]').waitFor();
 assert.equal(await page.locator('[data-prompt-open="health"]').count(),1);
 assert.equal(await page.locator('[data-prompt-open="voices"]').count(),1);
 await geometry();await capture('isolated-admin-journal-management');
 await page.locator('[data-prompt-open="voices"]').scrollIntoViewIfNeeded();await capture('isolated-admin-voice-entry');
 await page.locator('[data-prompt-open="voices"]').click();await page.getByRole('heading',{name:'手记 · 朗读音色',exact:true}).waitFor();
 assert.equal(await page.locator('#workspace').isVisible(),true);
 isolatedAdmin=false;
 await page.evaluate(async()=>{state.user={id:'fixture',role:'admin'};state.csrf='fixture-csrf';window.reauthChecks=0;reauthenticate=async()=>window.reauthChecks++;$('#auth').classList.add('hidden');$('#workspace').classList.remove('hidden');$('#ai-section').classList.remove('hidden');$('#operations-section').classList.add('hidden');await loadAI();});
 await page.getByRole('button',{name:'手记 · 朗读音色',exact:false}).click();
 await page.getByRole('heading',{name:'手记 · 朗读音色',exact:true}).waitFor();
 assert.equal(await page.locator('#voice-rows script').count(),0);assert.ok(await page.locator('#voice-rows').textContent().then(t=>t.includes('小何 <script>')));
 await page.locator('#voice-search').fill('Vivi');await page.locator('[data-voice-edit]').click();
 const name=page.locator('#voice-form [name="name"]');await name.fill('');await name.evaluate(el=>window.nameNode=el);await name.pressSequentially('Vivi');
 assert.equal(await name.evaluate(el=>el===window.nameNode&&el.selectionStart===4),true);
 const cdp=await context.newCDPSession(page);await cdp.send('Input.imeSetComposition',{text:'轻声',selectionStart:2,selectionEnd:2});await cdp.send('Input.insertText',{text:'轻声'});assert.equal(await name.inputValue(),'Vivi轻声');
 await page.locator('#voice-form [name="billingClass"]').selectOption('standard');
 await page.locator('#voice-form [name="styles"][value="calm"]').check();await page.locator('#voice-form [name="supportsPitch"]').check();
 await page.locator('#voice-form [name="languages"]').fill('zh-cn, en');
 await page.getByRole('button',{name:'验证并试听（消耗字符）',exact:true}).click();await page.getByRole('alert').filter({hasText:'response was lost'}).waitFor();
 assert.equal(actualProbeCalls,1);await page.getByRole('button',{name:'检查上次验证结果',exact:true}).click();await page.locator('#voice-proof').filter({hasText:'验证完成'}).waitFor();
 assert.equal(actualProbeCalls,1);assert.equal(probeHTTPCalls,2);assert.equal(await page.locator('#voice-form [name="enabled"]').isChecked(),false);
 await page.locator('#voice-form [name="enabled"]').check();await page.getByRole('button',{name:'设为默认声音',exact:true}).click();
 await page.getByRole('button',{name:'返回音色目录',exact:true}).click();
 await page.getByRole('button',{name:'同步官方候选',exact:true}).click();await page.waitForFunction(()=>voices.edit.voices.length===3);await page.locator('#voice-search').fill('');await page.locator('#voice-count').filter({hasText:'共 3 个声音'}).waitFor();
 assert.equal(await page.evaluate(()=>voices.edit.voices.find(v=>v.id==='voice_two').name),'Vivi轻声');
 assert.equal(await page.evaluate(()=>voices.edit.voices.find(v=>v.id==='voice_three').enabled),false);
 await page.reload({waitUntil:'networkidle'});
 await page.evaluate(async()=>{state.user={id:'fixture',role:'admin'};state.csrf='fixture-csrf';window.reauthChecks=0;reauthenticate=async()=>window.reauthChecks++;$('#auth').classList.add('hidden');$('#workspace').classList.remove('hidden');$('#ai-section').classList.remove('hidden');$('#operations-section').classList.add('hidden');await openVoiceCatalog();});
 assert.equal(await page.evaluate(()=>voices.edit.defaultVoiceID),'voice_two');assert.equal(await page.evaluate(()=>voices.edit.voices.length),3);
 assert.equal(await page.evaluate(()=>!!voiceProof(voices.edit.voices.find(v=>v.id==='voice_two'))),true,'reload lost probe result and would invite repeat spending');
 for(const color of ['light','dark'])for(const width of [390,1100]){
  await page.emulateMedia({colorScheme:color});await page.setViewportSize({width,height:900});await page.evaluate(()=>scrollTo(0,0));await geometry();await capture('catalog-'+width+'-'+color);
  await page.locator('#voice-save').scrollIntoViewIfNeeded();await capture('catalog-'+width+'-'+color+'-bottom');
 }
 await page.locator('#voice-search').fill('Vivi');await page.locator('[data-voice-edit]').click();
 await page.setViewportSize({width:390,height:844});await geometry();await captureViewport('editor-mobile-dark-top');
 await page.locator('#voice-form fieldset').nth(1).scrollIntoViewIfNeeded();await captureViewport('editor-mobile-dark-capabilities');
 await page.locator('#voice-default').scrollIntoViewIfNeeded();await captureViewport('editor-mobile-dark-enabling');
 await page.getByRole('button',{name:'返回音色目录',exact:true}).click();
 await page.getByRole('button',{name:'保存草稿并预览',exact:true}).click();await page.getByRole('heading',{name:'确认音色目录变更',exact:true}).waitFor();
 await geometry();await page.evaluate(()=>scrollTo(0,0));await capture('review-mobile-dark');
 await page.locator('#voice-publish').scrollIntoViewIfNeeded();await capture('review-mobile-dark-bottom');
 await page.getByRole('button',{name:'通行密钥确认并发布',exact:true}).click();await page.getByRole('heading',{name:'手记 · 朗读音色',exact:true}).waitFor();
 assert.equal(publishCalls,1);assert.equal(current.defaultVoiceID,'voice_two');assert.equal(await page.evaluate(()=>window.reauthChecks),1);
 await page.getByRole('button',{name:'历史版本',exact:true}).click();await page.getByRole('button',{name:'载入为草稿',exact:true}).nth(1).click();
 await page.getByRole('heading',{name:'手记 · 朗读音色',exact:true}).waitFor();
 assert.equal(await page.evaluate(()=>voices.edit.baseVersion),current.version);assert.equal(await page.evaluate(()=>voices.edit.defaultVoiceID),'voice_one');
 await page.getByRole('button',{name:'保存草稿并预览',exact:true}).click();await page.getByRole('heading',{name:'确认音色目录变更',exact:true}).waitFor();await capture('restore-review-mobile-dark');
 assert.equal(publishCalls,1,'history restore published without review');
 await page.getByRole('button',{name:'继续编辑',exact:true}).click();
 await page.locator('#voice-search').fill('小何');await page.locator('[data-voice-edit]').click();
 await page.locator('#voice-form [name="enabled"]').uncheck();await page.getByRole('button',{name:'返回音色目录',exact:true}).click();
 await page.getByRole('button',{name:'保存草稿并预览',exact:true}).click();await page.getByRole('heading',{name:'确认音色目录变更',exact:true}).waitFor();
 assert.equal(await page.getByRole('button',{name:'通行密钥确认并发布',exact:true}).isDisabled(),true);
 await page.getByRole('button',{name:'继续编辑',exact:true}).click();
 current={...structuredClone(current),version:'concurrent-version',voices:current.voices.map(v=>v.id==='voice_one'?{...v,description:'另一位管理员更新的说明'}:v)};
 await page.evaluate(()=>openVoiceCatalog());await page.getByRole('button',{name:'合并最新发布版',exact:true}).click();await page.getByRole('button',{name:'保留选择并继续编辑',exact:true}).click();
 assert.equal(await page.evaluate(()=>voices.edit.baseVersion),'concurrent-version');
 assert.equal(await page.evaluate(()=>voices.edit.voices.find(v=>v.id==='voice_one').enabled),false);
 assert.equal(await page.evaluate(()=>voices.edit.voices.find(v=>v.id==='voice_one').description),'另一位管理员更新的说明');
 await page.evaluate(()=>{const blob=voiceWAV('AQIDBA==',24000);if(blob.size!==48)throw Error('bad WAV framing');});
 // Import is atomic, preserves existing edits, and cannot import proof or enable voices.
 current={...structuredClone(initial),voices:[structuredClone(initial.voices[0])]};isolatedAdmin=true;
 await page.evaluate(()=>sessionStorage.clear());await page.reload({waitUntil:'networkidle'});
 await page.locator('#ai-tab').click();await page.locator('[data-prompt-open="voices"]').click();
 await page.evaluate(()=>{window.reauthChecks=0;reauthenticate=async()=>window.reauthChecks++;});
 const importCandidates=async voices=>{await page.locator('#voice-import-file').setInputFiles({name:'candidates.json',mimeType:'application/json',buffer:Buffer.from(JSON.stringify({defaultVoiceID:'bulk-voice-0',voices}))});};
 await importCandidates([voice('safe_new','安全候选'),{...voice('forged','伪造证据'),verification:proof}]);
 await page.getByRole('alert').filter({hasText:'夹带验证证据'}).waitFor();
 assert.equal(await page.evaluate(()=>voices.edit.voices.length),1,'partial invalid import changed draft');
 const bulk=Array.from({length:466},(_,i)=>voice('bulk-voice-'+i,'候选 '+i,{enabled:true,billingClass:'standard',rank:i}));
 await importCandidates([voice('voice_one','不得覆盖原名'),...bulk]);
 await page.locator('#voice-count').filter({hasText:'共 467 个声音'}).waitFor();
 assert.equal(await page.evaluate(()=>voices.edit.voices[0].name),initial.voices[0].name);
 assert.equal(await page.evaluate(()=>voices.edit.voices.filter(v=>v.id.startsWith('bulk-')&&(v.enabled||v.billingClass!=='unreviewed')).length),0);
 await geometry();await capture('imported-466-mobile');
 await page.locator('#voice-search').fill('bulk-voice-0');await page.locator('[data-voice-edit]').click();
 const identifier=page.locator('#voice-form [name="id"]');assert.equal(await identifier.evaluate(el=>el.validity.patternMismatch),false,'official hyphen ID rejected by HTML');
 await identifier.fill('bad/path');assert.equal(await identifier.evaluate(el=>el.validity.patternMismatch),true);await identifier.fill('bulk-voice-0');
 await page.getByRole('button',{name:'返回音色目录',exact:true}).click();await page.locator('#voice-search').fill('');
 firstProbeResponseLost=true;const beforeBulk=actualProbeCalls;
 await page.locator('#voice-batch').click();await geometry();await capture('bulk-validation-mobile');
 await page.locator('#voice-batch-start').click();assert.equal(actualProbeCalls,beforeBulk,'price review was not required');
 await page.locator('#voice-batch-price').check();await page.locator('#voice-batch-start').click();
 await page.locator('#voice-batch-status').filter({hasText:'本轮已结束'}).waitFor({timeout:90000});
 assert.equal(actualProbeCalls,beforeBulk+466);assert.equal(await page.locator('#voice-batch-errors li').count(),1);
 await geometry();await capture('bulk-validation-results-mobile');
 assert.equal(await page.evaluate(()=>voices.edit.voices.filter(v=>v.enabled).length),1,'verification enabled voices without review');
 await page.locator('#voice-batch-enable').click();await page.locator('#voice-count').filter({hasText:'已启用 466 个'}).waitFor();
 await page.locator('#voice-batch').click();assert.equal(await page.locator('#voice-batch-start').isDisabled(),true,'unresolved paid probe was scheduled again');
 await page.locator('#voice-batch-back').click();await page.locator('#voice-search').fill('bulk-voice-0');await page.locator('[data-voice-edit]').click();
 await page.getByRole('button',{name:'检查上次验证结果',exact:true}).click();await page.locator('#voice-proof').filter({hasText:'验证完成'}).waitFor();
 assert.equal(actualProbeCalls,beforeBulk+466,'checking uncertain result repeated provider synthesis');
 await page.getByRole('button',{name:'返回音色目录',exact:true}).click();await page.locator('#voice-search').fill('');
 // Stop finishes only the submitted voice, and a service outage stops after three attempts.
 await importCandidates([voice('cancel_voice_1','取消一'),voice('cancel_voice_2','取消二')]);
 await page.getByRole('status').filter({hasText:'已导入 2 个'}).waitFor();
 const arrived=new Promise(resolve=>probeArrived=resolve);heldProbe=new Promise(resolve=>releaseProbe=resolve);
 await page.locator('#voice-batch').click();await page.locator('#voice-batch-price').check();await page.locator('#voice-batch-start').click();await arrived;
 await page.locator('#voice-batch-stop').click();releaseProbe();await page.locator('#voice-batch-status').filter({hasText:'本轮已结束'}).waitFor();
 assert.equal(actualProbeCalls,beforeBulk+467);heldProbe=null;
 await page.locator('#voice-batch-back').click();
 await importCandidates(Array.from({length:4},(_,i)=>voice('unavailable_voice_'+i,'故障 '+i)));
 await page.getByRole('status').filter({hasText:'已导入 4 个'}).waitFor();
 await page.locator('#voice-batch').click();await page.locator('#voice-batch-price').check();await page.locator('#voice-batch-start').click();
 await page.locator('#voice-batch-status').filter({hasText:'连续三项失败'}).waitFor();
 assert.equal(actualProbeCalls,beforeBulk+471,'batch did not stop after three failed voices');
 assert.equal(await page.evaluate(()=>window.reauthChecks),1,'fresh server authentication was not reused for the batch');
 await page.setViewportSize({width:1100,height:900});await geometry();await capture('bulk-validation-desktop');
 // Direct publication is explicit, performs no probe, preserves passkey confirmation and excludes nonstandard billing.
 current=null;await page.evaluate(()=>sessionStorage.clear());await page.evaluate(()=>openVoiceCatalog());
 await importCandidates([voice('direct_voice','直接开放'),voice('paid_voice','额外付费',{billingClass:'premium'}),voice('clone_voice','复刻声音',{billingClass:'voice-slot'})]);
 await page.getByRole('status').filter({hasText:'已导入 3 个'}).waitFor();
 await page.locator('#voice-search').fill('direct_voice');await page.locator('[data-voice-edit]').click();
 await page.locator('#voice-form [name="billingClass"]').selectOption('standard');await page.locator('#voice-form [name="enabled"]').check();
 await page.locator('#voice-default').click();await page.getByRole('button',{name:'返回音色目录',exact:true}).click();
 await page.locator('#voice-save').click();await page.getByRole('heading',{name:'确认音色目录变更',exact:true}).waitFor();
 assert.equal(await page.locator('#voice-publish').isDisabled(),true,'ordinary review skipped proof without administrator approval');
 await page.locator('#voice-edit-again').click();
 const probesBeforeDirect=probeHTTPCalls,reauthBeforeDirect=await page.evaluate(()=>window.reauthChecks);
 await page.locator('#voice-direct').click();await page.locator('#voice-direct-notice').waitFor();
 assert.equal(await page.locator('#voice-publish').isDisabled(),false);assert.equal(probeHTTPCalls,probesBeforeDirect);
 for(const width of [390,1100]){await page.setViewportSize({width,height:900});await geometry();await page.evaluate(()=>scrollTo(0,0));await capture('direct-publication-review-'+width);}
 await page.locator('#voice-publish').click();await page.getByRole('heading',{name:'手记 · 朗读音色',exact:true}).waitFor();
 assert.equal(lastPublication.skipSynthesisVerification,true);assert.equal(await page.evaluate(()=>window.reauthChecks),reauthBeforeDirect+1);
 assert.equal(probeHTTPCalls,probesBeforeDirect,'direct publication purchased synthesis');
 assert.equal(current.defaultVoiceID,'direct_voice');assert.deepEqual(current.voices.filter(v=>v.enabled).map(v=>v.id),['direct_voice']);
 assert.equal(current.voices[0].verification,undefined);assert.ok(current.voices[0].approvedPolicySHA256);
 await page.locator('#voice-search').fill('');assert.ok((await page.locator('#voice-rows').textContent()).includes('管理员已确认 · 未合成验证'));
 await geometry();await capture('direct-publication-result');
 await page.locator('#voice-save').click();await page.getByRole('heading',{name:'确认音色目录变更',exact:true}).waitFor();
 assert.equal(await page.locator('#voice-publish').isDisabled(),false,'existing administrator approval lost on an unchanged draft');
 assert.deepEqual(errors,[]);
 console.log('PASS: explicit direct publication without probe charges or forged evidence, standard billing only, passkey confirmation and persistent approval; atomic 466-voice import, HTML hyphen IDs, reviewed batch validation, uncertain-result replay, cancellation and outage stop, four-App selection and scoped unavailable services, test-environment header geometry, catalog filtering/escaping, caret/IME, persisted draft, sync merge, capability probe replay without repeat charge, default/enable, reviewed publication, history restore, concurrent-edit merge, empty publish blocked, WAV framing, mobile/desktop light/dark geometry');
}finally{await browser.close();await new Promise(resolve=>server.close(resolve));}
