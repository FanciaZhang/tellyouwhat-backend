import assert from 'node:assert/strict';
import fs from 'node:fs/promises';
import http from 'node:http';
import os from 'node:os';
import path from 'node:path';

const {chromium}=await import(process.env.PLAYWRIGHT_MODULE||'playwright');
const root=path.resolve('internal/adminui/static'),temporary=await fs.mkdtemp(path.join(os.tmpdir(),'voice-lab-ui-'));
function wav(pcm,rate=16000,channels=1){const out=Buffer.alloc(44+pcm.length);out.write('RIFF');out.writeUInt32LE(out.length-8,4);out.write('WAVEfmt ',8);out.writeUInt32LE(16,16);out.writeUInt16LE(1,20);out.writeUInt16LE(channels,22);out.writeUInt32LE(rate,24);out.writeUInt32LE(rate*channels*2,28);out.writeUInt16LE(channels*2,32);out.writeUInt16LE(16,34);out.write('data',36);out.writeUInt32LE(pcm.length,40);pcm.copy(out,44);return out;}
const river=await fs.readFile('internal/prompteval/fixtures/river.pcm');
const microphone=path.join(temporary,'microphone.wav');await fs.writeFile(microphone,wav(river));
const server=http.createServer(async(req,res)=>{
  res.setHeader('Content-Security-Policy',"default-src 'none'; script-src 'self'; style-src 'self'; connect-src 'self'; img-src 'self'; media-src 'self' blob:; frame-ancestors 'none'; base-uri 'none'; form-action 'self'");
  res.setHeader('Permissions-Policy','camera=(), microphone=(self)');
  try{const file=path.join(root,req.url==='/'?'index.html':req.url);res.setHeader('Content-Type',file.endsWith('.js')?'text/javascript':file.endsWith('.css')?'text/css':'text/html');res.end(await fs.readFile(file));}catch{res.writeHead(404);res.end();}
});
await new Promise(resolve=>server.listen(0,'127.0.0.1',resolve));
const browser=await chromium.launch({headless:true,executablePath:process.env.CHROME_EXECUTABLE,args:['--use-fake-ui-for-media-stream','--use-fake-device-for-media-stream',`--use-file-for-fake-audio-capture=${microphone}`]});
const context=await browser.newContext({viewport:{width:1100,height:1000},permissions:['microphone']});
const page=await context.newPage(),errors=[];page.setDefaultTimeout(20000);page.on('pageerror',error=>errors.push(error.message));page.on('dialog',dialog=>dialog.accept());
const parameters={model:'ep-journal',reasoningEffort:'disabled',temperature:null,maxOutputTokens:8192,timeoutSeconds:90};
const policy={systemPrompt:'',journal:{illustrationSuggestionPrompt:'场景',illustrationGenerationPrompt:'画面',organize:{prompt:'只整理已有事实',lite:{...parameters},pro:{...parameters},liteMaxCharacters:6000,liteMaxBooks:24,liteMaxTags:80},voice:{prompt:'语音编辑规则',polishPrompt:'',removeRepetition:true,parameters:{...parameters},automaticPunctuation:true,normalizeNumbers:true},defaultStyle:'natural',styles:[{id:'natural',name:'自然记录',description:'保留自己的语气',example:'下午到河边散步。',prompt:'理顺口述，保留自己的语气。',enabled:true,order:0},{id:'documentary',name:'严谨纪实',description:'完整书面句',example:'下午到河边散步。',prompt:'保留事实，用完整的书面句。',enabled:true,order:1}]}};
const current={id:'11111111-1111-4111-8111-111111111111',scope:'journal',policy,createdAt:new Date().toISOString(),publishedAt:new Date().toISOString()};
const samples=new Map(),revisions=new Map([[current.id,current]]),runs=new Map(),commands=new Map();
let configuredOnly=false,uncertainRecording=false,operatorMode=false;
let expireStart=false,pauseDrafts=false,draftGates=[];
let sequence=0,failASR=false,failPolish=false,uncertainStart=false,hold=false,paidASR=0,paidPolish=0;
const id=()=>`40000000-0000-4000-8000-${String(++sequence).padStart(12,'0')}`;
function makePlan(input){const selected=input.sampleIDs.map(id=>samples.get(id)),revision=revisions.get(input.candidates[0].revision);return {mode:input.mode,samples:selected,candidates:[{revision,label:'调试草稿'}],maximumCalls:selected.length,reservedNanos:19000000*selected.length,requests:selected.map(s=>s.kind==='voice_recording'?{stage:'recording_transcription',milliseconds:s.recording.bytes/32}:s.kind==='voice_transcription'?{stage:'transcription',milliseconds:Buffer.from(s.audio,'base64').length/32}:{model:parameters.model,instructions:revision.policy.journal.voice.polishPrompt,input:s.voice})};}
function makeRun(input,key){
  if(commands.has(key))return commands.get(key);
  const plan=makePlan(input),run={id:id(),mode:plan.mode,status:hold?'running':'completed',createdAt:new Date().toISOString(),plan,items:[]};
  run.items=plan.samples.map((s,index)=>{
    const asr=s.kind==='voice_transcription'||s.kind==='voice_recording';if(asr)paidASR++;else paidPolish++;
    const failed=s.kind==='voice_recording'&&uncertainRecording||(asr?failASR&&s.name.endsWith('2'):failPolish);
    const number=Number(s.name.match(/(\d+)$/)?.[1]||'1');
    const text=asr?`第${number}段识别原文。`:(plan.candidates[0].revision.policy.journal.voice.polishPrompt.includes('简洁')?'下班后，我和小林去河边散步，买了一杯热茶。':'今天下班后，我和小林去河边散步，买了一杯热茶。');
    const output={candidate:0,configVersion:plan.candidates[0].revision.id,model:asr?'':'doubao-fixture',text:failed?'':text,transcript:asr?text:s.voice.transcript,costNanos:asr?0:1200000,speechCostNanos:asr?800000:0,milliseconds:1420,inputTokens:160,outputTokens:80,request:plan.requests[index],structured:asr?undefined:{paragraphs:[{text,style:'body',targetIDs:s.voice.polish.targets.map(t=>t.id)}]},checks:[],...(failed?{error:s.kind==='voice_recording'?'recording_submission_unconfirmed':'output_contract_failed',...(s.kind==='voice_recording'?{recordingTaskID:id()}:{}),diagnosticOutput:'synthetic invalid output <img src=x onerror=alert(1)>',diagnostics:{Stage:'validate_polish',ValidationCode:'target_coverage'}}:{})};
    return {index,status:hold?'pending':failed?'failed':'completed',result:{outputs:[output],judgment:{status:'not_applicable',scores:[]}}};
  });
  runs.set(run.id,run);commands.set(key,run);return run;
}
await page.route('**/api/**',async route=>{
  const request=route.request(),u=new URL(request.url());let data,status=200;
  if(u.pathname==='/api/v1/session'){status=401;data={error:{message:'login'}};}
  else if(u.pathname==='/api/v1/ai/models')data={models:[{Name:'doubao-text',DisplayName:'豆包文本',VendorName:'火山引擎'}]};
  else if(u.pathname==='/api/v1/ai/models/doubao-text/versions')data={versions:[{ModelId:'ep-alternative',ModelVersion:'1',Status:'Published',Domains:['TextGeneration']}]};
  else if(u.pathname==='/api/v1/ai/health')data={operations:[],writesEnabled:true};
  else if((u.pathname==='/api/v1/ai/prompts'||u.pathname==='/api/v1/ai/voice-debug/config'))data={current,writesEnabled:true,canPublish:!operatorMode,...(configuredOnly?{modelCatalogAvailable:false,configuredModels:['ep-journal']}:{})};
  else if((u.pathname.endsWith('/prompts/drafts')||u.pathname.endsWith('/voice-debug/drafts'))){const input=request.postDataJSON();data={id:id(),scope:'journal',baseVersion:input.baseVersion,policy:input.policy,createdAt:new Date().toISOString()};revisions.set(data.id,data);if(pauseDrafts)await new Promise(resolve=>draftGates.push(resolve));}
  else if(u.pathname.endsWith('/prompts/history'))data={revisions:[...revisions.values()]};
  else if(u.pathname.includes('/prompts/revisions/')){const r=revisions.get(u.pathname.split('/').at(-1));data={revision:r,before:current.policy,after:r.policy,canPublish:false,blockers:['需要完整质量评测']};}
  else if(u.pathname.endsWith('/evaluations/samples')&&request.method()==='POST'){data=request.postDataJSON();if(data.kind==='voice_recording'){data.recording.bytes=data.recording.chunks.reduce((n,id)=>n+Buffer.from(samples.get(id).audio,'base64').length,0);data.recording.sha256='a'.repeat(64);}samples.set(data.id,data);}
  else if(u.pathname.includes('/evaluations/samples/'))data=samples.get(u.pathname.split('/').at(-1));
  else if(u.pathname.endsWith('/evaluations/preview')){const input=request.postDataJSON();assert.equal(input.mode,'voice_debug');assert.equal(input.candidates.length,1);data={plan:makePlan(input),previewToken:'signed-'+id()};}
  else if(u.pathname.includes('/evaluations/commands/')){data=commands.get(u.pathname.split('/').at(-1));if(!data){status=404;data={error:{message:'未找到提交'}};}}
  else if(u.pathname.endsWith('/evaluations/runs')&&request.method()==='POST'){if(expireStart){expireStart=false;await route.fulfill({status:409,contentType:'application/json',body:JSON.stringify({error:{code:'evaluation_preview_changed',message:'预览已过期，请重新预览'}})});return;}data=makeRun(request.postDataJSON(),request.headers()['idempotency-key']);if(uncertainStart){uncertainStart=false;status=503;data={error:{message:'模拟响应丢失'}};}}
  else if(u.pathname.endsWith('/evaluations/runs'))data={runs:[...runs.values()]};
  else if(u.pathname.endsWith('/cancel')){const r=runs.get(u.pathname.split('/').at(-2));r.status='cancelled';r.items.forEach(i=>{if(i.status==='pending')i.status='cancelled';});data={cancelled:true};}
  else if(u.pathname.endsWith('/reconcile-recording')){const r=runs.get(u.pathname.split('/').at(-4));r.items[0].status='completed';r.items[0].result.outputs[0].text='核对后的整段原文。';delete r.items[0].result.outputs[0].error;data={queued:true};}
  else if(u.pathname.includes('/items/')){const parts=u.pathname.split('/');data=runs.get(parts.at(-3)).items[Number(parts.at(-1))];}
  else if(u.pathname.includes('/evaluations/runs/'))data=runs.get(u.pathname.split('/').at(-1));
  else{status=404;data={error:{message:'Unhandled '+u.pathname}};}
  await route.fulfill({status,contentType:'application/json',body:JSON.stringify(data)});
});
async function open(){if(await page.evaluate(()=>typeof voiceLab!=='undefined'&&!!voiceLab.id).catch(()=>false))await page.evaluate(()=>voiceLab.save);await page.goto(`http://127.0.0.1:${server.address().port}`,{waitUntil:'networkidle'});await page.evaluate(async()=>{state.user={id:'fixture',role:'admin'};state.csrf='fixture';$('#auth').classList.add('hidden');$('#workspace').classList.remove('hidden');$('#ai-section').classList.remove('hidden');$('#operations-section').classList.add('hidden');await loadAI();});await page.getByRole('button',{name:'手记 · 语音调试'}).click();await page.locator('#lab-prompt').waitFor();}
const out=process.env.PROMPT_UI_SCREENSHOT_DIR;if(out)await fs.mkdir(out,{recursive:true});
async function screenshot(name){if(!out)return;await page.evaluate(()=>scrollTo(0,0));await page.evaluate(()=>new Promise(resolve=>requestAnimationFrame(()=>requestAnimationFrame(resolve))));const mobile=page.viewportSize().width<720;await page.screenshot({path:path.join(out,name),fullPage:!mobile});if(mobile){await page.locator('#lab-results').scrollIntoViewIfNeeded();await page.screenshot({path:path.join(out,name.replace('.png','-results.png')),fullPage:false});}}
async function begin(stage){await page.locator(stage==='asr'?'#lab-transcribe':'#lab-organize').click();await page.locator('#lab-start').waitFor();await page.locator('#lab-start').click();}
async function idle(){await page.waitForFunction(()=>!voiceLab.busy);}

try {
  await open();assert.ok(await page.evaluate(()=>[...document.querySelectorAll('.lab-config details:not([open])')].every(el=>el.getBoundingClientRect().height<=48)));console.log('STEP: opened lab');assert.ok(await page.locator('#lab-transcribe').isDisabled());
  const prompt=page.locator('#lab-prompt');await prompt.fill('');await prompt.pressSequentially('prompt');await prompt.evaluate(el=>window.promptNode=el);
  const cdp=await context.newCDPSession(page);await cdp.send('Input.imeSetComposition',{text:'中文',selectionStart:2,selectionEnd:2});await cdp.send('Input.insertText',{text:'中文'});
  assert.equal(await prompt.inputValue(),'prompt中文');assert.ok(await prompt.evaluate(el=>el===window.promptNode));
  await page.getByText('口述转写设置',{exact:true}).click();await page.locator('#lab-words').fill('小林');
  await page.locator('#lab-transcript').fill('保留我的旧原文');
  const longPCM=Buffer.alloc(16*32000);for(let i=0;i<longPCM.length;i++)longPCM[i]=river[i%river.length];
  await page.locator('#lab-file').setInputFiles({name:'合成录音.wav',mimeType:'audio/wav',buffer:wav(longPCM)});await page.locator('#lab-audio-preview').waitFor();await idle();await page.locator('#lab-asr-mode').selectOption('dictation');
  assert.equal(await page.evaluate(()=>voiceLab.asset.pcm.length),longPCM.length);
  const bounds=await page.evaluate(()=>{const oldText=voiceLab.transcript,oldPCM=voiceLab.asset.pcm;voiceLab.asset.pcm=new Uint8Array(30*60*32000);const audio=voiceLabSamples('transcription');const audioResult={count:audio.length,maximum:Math.max(...audio.map(s=>atob(s.audio).length)),bytes:audio.reduce((n,s)=>n+atob(s.audio).length,0)};voiceLab.asset.pcm=oldPCM;voiceLab.transcript='今天下午到河边散步，买了一杯热茶。'.repeat(500);const text=voiceLab.transcript;const organized=voiceLabSamples('organization');const preserved=organized.map(s=>s.voice.polish.targets.map(t=>t.turns.map(u=>u.text).join('')).join('')).join('');voiceLab.transcript=oldText;return {...audioResult,lossless:preserved===text,maximumTargets:Math.max(...organized.map(s=>s.voice.polish.targets.length))};});
  assert.equal(bounds.count,120);assert.equal(bounds.maximum,480000);assert.equal(bounds.bytes,30*60*32000);assert.ok(bounds.lossless&&bounds.maximumTargets<=16);
  console.log('STEP: decoded upload');failASR=true;await begin('asr');await page.locator('#lab-retry-failed').waitFor();
  assert.equal(await page.locator('#lab-transcript').inputValue(),'');assert.equal(paidASR,2);
  failASR=false;await page.getByText('口述转写设置',{exact:true}).click();await page.locator('#lab-words').fill('小敏');assert.equal(await page.locator('#lab-retry-failed').count(),1);await page.locator('#lab-retry-failed').click();await page.locator('#lab-start').waitFor();await page.locator('#lab-start').click();await page.waitForFunction(()=>voiceLab.transcript.includes('第2段识别原文'));
  assert.equal(paidASR,3);assert.deepEqual([...samples.values()].at(-1).voice.words,['小敏']);assert.equal(await page.locator('#lab-transcript').inputValue(),'第1段识别原文。\n第2段识别原文。');
  await page.locator('#lab-file').setInputFiles({name:'invalid.mp3',mimeType:'audio/mpeg',buffer:Buffer.from('invalid audio')});await idle();assert.ok((await page.locator('#lab-status').innerText()).includes('无法解码'));assert.equal(await page.evaluate(()=>voiceLab.asset.name),'合成录音.wav');
  await page.locator('#lab-transcript').fill('今天下班了，去河边，和小林散步，买了一杯热茶。');
  await page.getByText('口述转写设置',{exact:true}).click();console.log('STEP: ASR recovery passed');await prompt.fill('按事件分段，保留热茶与同行人。');await begin('polish');await page.waitForFunction(()=>voiceLab.results.length===1);
  assert.equal(paidPolish,1);assert.ok(await page.locator('.lab-prose').getByText('今天下班后，我和小林去河边散步，买了一杯热茶。',{exact:false}).isVisible());
  await prompt.fill('去掉口头重复，句子简洁。');await begin('polish');await page.waitForFunction(()=>voiceLab.results.length===2);
  assert.equal(paidASR,3);assert.equal(paidPolish,2);assert.equal(await page.locator('.lab-result').count(),2);
  await page.locator('#lab-diff').check();assert.ok(await page.locator('.lab-prose ins,.lab-prose del').count()===2);await page.locator('#lab-diff').uncheck();const download=page.waitForEvent('download');await page.locator('[data-lab-export]').last().click();const saved=await download;assert.ok(saved.suggestedFilename().endsWith('.txt'));assert.ok((await fs.readFile(await saved.path(),'utf8')).includes('热茶'));
  console.log('STEP: prompt comparisons passed');await screenshot('voice-lab-desktop.png');
  for(const color of ['light','dark'])for(const width of [390,1100]) {
    await page.emulateMedia({colorScheme:color});await page.setViewportSize({width,height:1000});
    assert.ok(await page.evaluate(()=>document.documentElement.scrollWidth<=innerWidth+1));
    assert.ok(await page.evaluate(()=>{const a=$('.lab-config').getBoundingClientRect(),b=$('.lab-source').getBoundingClientRect();return a.right<=b.left+1||b.right<=a.left+1||a.bottom<=b.top+1||b.bottom<=a.top+1;}));
    await screenshot(`voice-lab-${width}-${color}.png`);
  }
  console.log('STEP: layout passed');failPolish=true;await begin('polish');await page.waitForFunction(()=>voiceLab.results.at(-1).outputs.some(o=>o.error));assert.ok(await page.locator('.lab-result').last().getByText('输出未通过生产协议校验',{exact:true}).isVisible());assert.equal(await page.locator('#ai-view img').count(),0);assert.ok((await page.locator('#lab-transcript').inputValue()).includes('小林'));failPolish=false;
  uncertainStart=true;const prior=paidPolish;await begin('polish');await page.locator('#lab-resume').waitFor({state:'visible'});await page.locator('#lab-resume').click();await idle();assert.equal(paidPolish,prior+1);
  console.log('STEP: recording');await page.locator('#lab-record').click();await page.waitForFunction(()=>voiceLab.capture?.recorder.state==='recording');await page.waitForTimeout(1300);await page.evaluate(()=>window.recordedTracks=voiceLab.capture.stream.getTracks());await page.locator('#lab-record').click();await page.waitForFunction(()=>voiceLab.asset?.name.startsWith('网页录音')&&!voiceLab.busy);assert.ok(await page.evaluate(()=>window.recordedTracks.every(t=>t.readyState==='ended')));assert.ok(await page.evaluate(()=>voiceLab.asset.pcm.byteLength>0));
  await page.locator('#lab-record').click();await page.waitForFunction(()=>voiceLab.capture?.recorder.state==='recording');await page.evaluate(()=>window.leavingTracks=voiceLab.capture.stream.getTracks());await page.locator('#ai-back').click();await page.waitForFunction(()=>!voiceLab.capture);assert.ok(await page.evaluate(()=>window.leavingTracks.every(t=>t.readyState==='ended')));
  await page.getByRole('button',{name:'手记 · 语音调试'}).click();await page.locator('#lab-transcript').waitFor();await page.locator('#lab-transcript').fill('恢复查看的合成原文。');hold=true;await begin('polish');await page.waitForFunction(()=>voiceLab.task.started);await page.locator('#ai-back').click();await page.getByRole('button',{name:'手记 · 语音调试'}).click();await page.locator('#lab-cancel').waitFor({state:'visible'});await page.locator('#lab-cancel').click();await idle();assert.equal([...runs.values()].at(-1).status,'cancelled');hold=false;

  console.log('STEP: persisted workspace and history');
  await page.locator('#lab-transcript').fill('手工校对的原文会保留。');await page.evaluate(()=>voiceLab.save);const preservedID=await page.evaluate(()=>voiceLab.id);await open();assert.equal(await page.locator('#lab-transcript').inputValue(),'手工校对的原文会保留。');
  await page.locator('#lab-history').click();await page.getByRole('heading',{name:'评测运行记录',exact:true}).waitFor();await page.locator('#ai-back').click();await page.locator('#lab-transcript').waitFor();assert.equal(await page.evaluate(()=>voiceLab.id),preservedID);
  await page.locator('#lab-history').click();await page.locator('[data-eval-run]').first().click();await page.locator('#eval-to-lab').waitFor();assert.equal(await page.locator('#eval-draft').count(),0);await page.locator('#ai-back').click();await page.locator('#ai-back').click();await page.locator('#lab-transcript').waitFor();
  await page.locator('#lab-file').setInputFiles({name:'第二个音频.wav',mimeType:'audio/wav',buffer:wav(river)});await page.locator('#lab-audio-preview').waitFor();await idle();assert.equal(await page.locator('#lab-transcript').inputValue(),'');assert.notEqual(await page.evaluate(()=>voiceLab.id),preservedID);await page.locator('#lab-sample').selectOption(preservedID);await page.waitForFunction(()=>voiceLab.transcript==='手工校对的原文会保留。');
  console.log('STEP: expired preview and cancellation ownership');
  await page.locator('#lab-organize').click();await page.locator('#lab-start').waitFor();expireStart=true;await page.locator('#lab-start').click();await page.getByRole('button',{name:'重新预览费用',exact:true}).waitFor();await open();await page.getByRole('button',{name:'重新预览费用',exact:true}).click();await page.getByRole('button',{name:'确认新费用并继续',exact:true}).click();await idle();
  pauseDrafts=true;await page.locator('#lab-organize').click();while(draftGates.length<1)await new Promise(r=>setTimeout(r,10));await page.locator('#lab-cancel').click();await idle();await page.locator('#lab-organize').click();while(draftGates.length<2)await new Promise(r=>setTimeout(r,10));draftGates[0]();await page.waitForTimeout(150);assert.ok(await page.evaluate(()=>voiceLab.busy));assert.ok(await page.locator('#lab-organize').isDisabled());draftGates[1]();pauseDrafts=false;await page.locator('#lab-start').waitFor();await page.locator('#lab-start').click();await idle();
  assert.ok(await page.locator('#lab-reference option').count()>=3);const resultID=await page.evaluate(()=>voiceLab.results.at(-1).id);await page.locator(`[data-lab-name="${resultID}"]`).fill('保留的简洁版本');await page.locator(`[data-lab-keep="${resultID}"]`).click();await page.evaluate(()=>voiceLab.save);await open();assert.ok(await page.getByRole('option',{name:'保留的简洁版本 · 已保留',exact:true}).count()>=1);
  console.log('STEP: recording retained after return');
  await page.locator('#lab-record').click();await page.waitForFunction(()=>voiceLab.capture?.recorder.state==='recording');await page.waitForTimeout(1300);await page.locator('#ai-back').click();await page.getByRole('button',{name:'手记 · 语音调试'}).click();await page.locator('#lab-audio-preview').waitFor();assert.ok(await page.evaluate(()=>voiceLab.asset.pcm.byteLength>0));await page.locator('#lab-transcript').fill('整段录音之前的文字。');
  await page.locator('#lab-file').setInputFiles({name:'整段录音.wav',mimeType:'audio/wav',buffer:wav(river)});await page.locator('#lab-audio-preview').waitFor();await idle();assert.equal(await page.locator('#lab-asr-mode').inputValue(),'recording');await begin('asr');await page.waitForFunction(()=>voiceLab.task.completed);assert.equal([...runs.values()].at(-1).plan.samples.length,1);assert.equal([...runs.values()].at(-1).plan.samples[0].kind,'voice_recording');assert.ok((await page.locator('#lab-transcript').inputValue()).length>0);
  uncertainRecording=true;const beforeReconcile=paidASR;await begin('asr');await page.locator('#lab-recover-recording').waitFor();await open();await page.locator('#lab-recover-recording').click();await idle();assert.equal(await page.locator('#lab-transcript').inputValue(),'核对后的整段原文。');assert.equal(paidASR,beforeReconcile+1);uncertainRecording=false;
  console.log('STEP: audio removal and interrupted recording recovery');
  const audioCase=await page.evaluate(()=>voiceLab.id);await page.locator('#lab-remove-audio').click();await page.waitForFunction(()=>!voiceLab.asset);await page.evaluate(()=>voiceLab.save);await open();assert.equal(await page.evaluate(()=>voiceLab.id),audioCase);assert.equal(await page.locator('#lab-audio-preview').count(),0);assert.ok((await page.locator('#lab-transcript').inputValue()).length>0);
  await page.locator('#lab-record').click();await page.waitForFunction(()=>voiceLab.capture?.recorder.state==='recording');await page.waitForTimeout(1400);await page.evaluate(async()=>{await voiceLab.save;voiceLab.capture.recorder.onstop=null;});await open();assert.ok(await page.evaluate(()=>voiceLab.asset?.file.size>0));assert.equal(await page.locator('.lab-download').count(),1);
  console.log('STEP: native IndexedDB conflicts, quota and scoped orphan cleanup');
  await page.evaluate(async()=>{await voiceDrafts.save({owner:'storage-test',id:'sample',title:'original',recording:{id:'active'}});await voiceDrafts.part('storage-test','sample','active',0,new Blob(['active']));await voiceDrafts.part('storage-test','sample','stale',0,new Blob(['stale']));await voiceDrafts.part('other-author','other','orphan',0,new Blob(['other']));await voiceDrafts.collect('storage-test');});
  assert.deepEqual(await page.evaluate(async()=>[(await voiceDrafts.parts('storage-test','sample','active')).length,(await voiceDrafts.parts('storage-test','sample','stale')).length,(await voiceDrafts.parts('other-author','other','orphan')).length]),[1,0,1]);
  const second=await context.newPage();await second.goto(`http://127.0.0.1:${server.address().port}`,{waitUntil:'networkidle'});await second.evaluate(()=>voiceDrafts.get('storage-test','sample'));await page.evaluate(()=>voiceDrafts.save({owner:'storage-test',id:'sample',title:'first tab',recording:{id:'active'}}));assert.ok(await second.evaluate(async()=>{try{await voiceDrafts.save({owner:'storage-test',id:'sample',title:'stale tab'});return false;}catch{return true;}}));assert.equal(await page.evaluate(async()=>(await voiceDrafts.get('storage-test','sample')).title),'first tab');await second.close();
  assert.ok(await page.evaluate(async()=>{const estimate=navigator.storage.estimate;navigator.storage.estimate=async()=>({quota:10,usage:10});try{await voiceDrafts.space(100);return false;}catch{return true;}finally{navigator.storage.estimate=estimate;}}));await page.evaluate(async()=>{await voiceDrafts.remove('storage-test','sample');await voiceDrafts.remove('other-author','other');});
  await page.getByText('整理模型与参数',{exact:true}).click();await page.locator('#lab-model-catalog').click();await page.getByRole('button',{name:'查看版本',exact:true}).click();await page.getByRole('button',{name:'使用此版本',exact:true}).click();await page.locator('#lab-prompt').waitFor();assert.equal(await page.evaluate(()=>ai.screen),'voice-lab');assert.equal(await page.evaluate(()=>voiceLab.policy.journal.voice.parameters.model),'ep-alternative');await page.locator('#lab-transcript').fill('选择模型后还能调试。');await begin('polish');await idle();
  configuredOnly=true;await page.locator('#ai-back').click();await page.getByRole('button',{name:'手记 · 语音调试'}).click();await page.locator('#lab-prompt').waitFor();await idle();await page.getByText('整理模型与参数',{exact:true}).click();assert.equal(await page.locator('select[name="journal.voice.parameters.model"]').count(),1);assert.equal(await page.locator('[data-prompt-model]').count(),0);assert.equal(await page.locator('select[name="journal.voice.parameters.model"] option').count(),2);
  operatorMode=true;await page.evaluate(async()=>{state.user.role='operator';state.apps=[{id:'journal'}];await loadAI();});await page.locator('#lab-prompt').waitFor();assert.equal(await page.locator('#lab-publish').count(),0);await page.locator('#ai-back').click();assert.ok(await page.locator('#operations-section').isVisible());
  assert.deepEqual(errors,[]);
  console.log('PASS: browser WAV decoding, real MediaRecorder/fake-device capture and track cleanup, partial ASR recovery without duplicate successful calls, original text preservation, stable IME, prompt-only reruns, two-result comparison, output failures/XSS escaping, paid-command idempotent recovery, cancellation, mobile/desktop light/dark geometry. Provider responses are fixtures.');
} catch(error) {
  console.error('Failed UI state:',await page.locator('#lab-status').textContent().catch(()=>null),await page.evaluate(()=>({busy:voiceLab.busy,task:voiceLab.task?.stage,mode:voiceLab.mode,errors:[...document.querySelectorAll(':invalid')].map(e=>({name:e.name,message:e.validationMessage}))})).catch(()=>null));throw error;
} finally {
  draftGates.forEach(resolve=>resolve());await browser.close();await new Promise(resolve=>server.close(resolve));await fs.rm(temporary,{recursive:true,force:true});
}
