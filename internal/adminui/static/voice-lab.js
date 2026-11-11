const voiceLab = { owner:null, config:null, policy:null, style:null, asset:null, transcript:'', results:[], task:null, preview:null, busy:false, capture:null, captureVersion:0, lastRequest:0 };
const voiceLabDuration = seconds => `${Math.floor(seconds/60)}:${String(Math.floor(seconds%60)).padStart(2,'0')}`;
const voiceLabStorage = () => `voice-lab:${state.user.id}`;
const voiceLabActive = () => ai.screen === 'voice-lab';

async function voiceLabAPI(path, options) {
  // Long files create many bounded samples; leave room in the shared admin rate limit.
  const delay = Math.max(0, 700-(Date.now()-voiceLab.lastRequest));
  if(delay) await new Promise(resolve=>setTimeout(resolve,delay));
  voiceLab.lastRequest=Date.now();
  return api(path,options);
}
async function openVoiceLab() {
  aiNavigate('voice-lab');const generation=ai.generation;
  aiFrame('手记 · 语音调试','正在读取当前配置…','',aiHome);
  await aiWork(async()=>{
    const config=await api('/api/v1/ai/prompts?scope=journal');
    if(generation!==ai.generation)return;
    if(voiceLab.owner!==state.user.id) {
      voiceLabReleaseAudio();voiceLab.owner=state.user.id;voiceLab.policy=null;voiceLab.transcript='';voiceLab.results=[];voiceLab.task=null;
    }
    voiceLab.config=config;
    if(!voiceLab.policy) {
      let saved;try{saved=JSON.parse(sessionStorage.getItem(voiceLabStorage()));}catch{}
      voiceLab.policy=structuredClone(saved?.policy||config.current.policy);
      voiceLab.base=saved?.base||config.current.id;
      voiceLab.style=saved?.style||voiceLab.policy.journal.defaultStyle;
      voiceLab.words=saved?.words||'';
    }
    voiceLabRender();
    if(voiceLab.task?.started&&!voiceLab.task.completed)voiceLabPoll();
  });
}
function voiceLabPersist() {
  sessionStorage.setItem(voiceLabStorage(),JSON.stringify({policy:voiceLab.policy,base:voiceLab.base,style:voiceLab.style,words:voiceLab.words}));
  voiceLab.preview=null;
  $('#lab-preview')?.replaceChildren();
}
function voiceLabStatus(text,error=false) {
  const node=$('#lab-status');if(!node)return;
  node.textContent=text;node.classList.toggle('error',error);
}
function voiceLabLock(busy) {
  voiceLab.busy=busy;
  if(!voiceLabActive())return;
  aiView().querySelectorAll('[data-lab-lock]').forEach(el=>el.disabled=busy);
  $('#lab-transcribe').disabled=busy||!voiceLab.asset;
  $('#lab-organize').disabled=busy||!voiceLab.transcript.trim();
  $('#lab-cancel').hidden=!busy;
}
function voiceLabRender() {
  const v=voiceLab.policy.journal.voice,styles=voiceLab.policy.journal.styles;
  const style=styles.find(s=>s.id===voiceLab.style)||styles[0];voiceLab.style=style.id;
  // Reuse the existing model fields, but keep the lab draft separate from the publisher.
  prompts.edit=voiceLab.policy;
  aiFrame('手记 · 语音调试','录一段或上传音频，检查转写，再反复调整成稿。',`
    <div class="lab-toolbar"><span class="chip">调试草稿 · 未发布</span><div><button class="quiet" id="lab-history" data-lab-lock>运行记录</button><button class="secondary" id="lab-publish" data-lab-lock>转到发布管理</button></div></div>
    <section class="card lab-audio" aria-labelledby="lab-audio-heading">
      <div><h3 id="lab-audio-heading">1. 准备音频</h3><p class="muted">M4A、MP3、WAV 等浏览器可解码的音频，最长 30 分钟、80 MB。</p></div>
      <div class="lab-audio-actions"><button id="lab-record" class="secondary" data-lab-lock>开始录音</button><label class="lab-upload">上传音频<input id="lab-file" type="file" accept="audio/*,.m4a,.mp3,.wav,.aac,.flac,.ogg,.webm" data-lab-lock></label><span id="lab-record-time" role="status"></span></div>
      <div id="lab-asset"></div>
    </section>
    <div class="lab-grid">
      <section class="card lab-config" aria-labelledby="lab-config-heading"><h3 id="lab-config-heading">整理规则</h3>
        <form id="lab-config-form" class="prompt-form">
          <label>成稿附加提示词<textarea id="lab-prompt" rows="8" maxlength="16000" data-lab-lock placeholder="例如：去掉无意义的口头重复，按事件分段，保留具体细节。">${aiEscape(v.polishPrompt||'')}</textarea></label>
          <label>写作风格<select id="lab-style" data-lab-lock>${styles.filter(s=>s.enabled||s.id===voiceLab.style).map(s=>`<option value="${aiEscape(s.id)}" ${s.id===style.id?'selected':''}>${aiEscape(s.name)}</option>`).join('')}</select></label>
          <details><summary>风格提示词</summary><label>当前风格的要求<textarea id="lab-style-prompt" rows="6" maxlength="16000" data-lab-lock>${aiEscape(style.prompt)}</textarea></label></details>
          <details><summary>转写设置</summary><label>热词（每行一个）<textarea id="lab-words" rows="3" data-lab-lock placeholder="人名、地名或容易识别错的词">${aiEscape(voiceLab.words||'')}</textarea></label><label class="prompt-check"><input id="lab-punctuation" type="checkbox" ${v.automaticPunctuation?'checked':''} data-lab-lock>自动标点</label><label class="prompt-check"><input id="lab-numbers" type="checkbox" ${v.normalizeNumbers?'checked':''} data-lab-lock>数字规范化</label></details>
          <details><summary>整理模型与参数</summary>${promptParameters('journal.voice.parameters','最终成稿',voiceLab.config)}</details>
        </form>
        <button id="lab-reset" class="quiet" data-lab-lock>载入当前发布配置</button>
      </section>
      <section class="card lab-source" aria-labelledby="lab-source-heading"><div class="lab-heading"><h3 id="lab-source-heading">2. 转写原文</h3><span id="lab-count" class="muted"></span></div>
        <label class="lab-source-label" for="lab-transcript">可直接修改，也可粘贴已有转写文本</label>
        <textarea id="lab-transcript" rows="14" data-lab-lock placeholder="转写结果会显示在这里；只调成稿时，也可以直接粘贴文本。">${aiEscape(voiceLab.transcript)}</textarea>
        <div class="lab-actions"><button id="lab-transcribe" class="secondary" data-lab-lock>预览费用并转写</button><button id="lab-organize" class="primary" data-lab-lock>预览费用并整理</button></div>
        <p class="muted lab-caption">转写使用手机口述的流式识别服务；成稿使用 App 的最终整理规则。附加提示词和风格会进入实际请求。</p>
      </section>
    </div>
    <div class="lab-feedback"><p id="lab-status" role="status" aria-live="polite"></p><button id="lab-cancel" class="quiet" hidden>停止本次调试</button><button id="lab-resume" class="secondary" hidden>恢复查看进度</button></div>
    <div id="lab-preview"></div><section id="lab-results" aria-label="成稿对比"></section>
    <p class="muted lab-caption">仅在点击试跑后上传样本。样本与结果加密保留 7 天，可在运行记录查看；不会读取其他用户的手记。录音开始后不会自动上传。</p>
  `,aiHome);
  aiView().querySelectorAll('#lab-config-form input,#lab-config-form select,#lab-config-form textarea,#lab-config-form button').forEach(el=>el.setAttribute('data-lab-lock',''));
  aiView().querySelector('[data-prompt-model]')?.remove();
  $('#lab-config-form').onsubmit=e=>e.preventDefault();
  $('#lab-config-form').oninput=e=>{
    const el=e.target;
    if(el.id==='lab-prompt')v.polishPrompt=el.value;
    else if(el.id==='lab-style-prompt')voiceLab.policy.journal.styles.find(s=>s.id===voiceLab.style).prompt=el.value;
    else if(el.id==='lab-words')voiceLab.words=el.value;
    else if(el.id==='lab-punctuation')v.automaticPunctuation=el.checked;
    else if(el.id==='lab-numbers')v.normalizeNumbers=el.checked;
    else if(el.name) {
      const keys=el.name.split('.'),last=keys.pop();const target=keys.reduce((o,k)=>o[k],voiceLab.policy);
      target[last]=el.type==='number'?(el.value===''?null:Number(el.value)):el.value;
      if(last==='model'){delete target.foundationModel;delete target.modelVersion;delete target.price;}
    }
    voiceLabPersist();
  };
  $('#lab-style').onchange=e=>{voiceLab.style=e.target.value;$('#lab-style-prompt').value=styles.find(s=>s.id===voiceLab.style).prompt;voiceLabPersist();};
  $('#lab-transcript').oninput=e=>{voiceLab.transcript=e.target.value;voiceLabPersist();voiceLabCount();voiceLabLock(false);};
  $('#lab-file').onchange=e=>{const file=e.target.files[0];if(file)voiceLabLoad(file);e.target.value='';};
  $('#lab-record').onclick=()=>voiceLab.capture?voiceLabStopCapture():voiceLabRecord();
  $('#lab-transcribe').onclick=()=>voiceLabPrepare('transcription');
  $('#lab-organize').onclick=()=>voiceLabPrepare('organization');
  $('#lab-cancel').onclick=voiceLabCancel;
  $('#lab-resume').onclick=()=>voiceLab.task?.cancelled?voiceLabCancel():voiceLabStart();
  $('#lab-history').onclick=evaluationRuns;
  $('#lab-publish').onclick=async()=>{
    if(voiceLab.busy)return;
    sessionStorage.setItem(`prompt-editor:${state.user.id}:journal`,JSON.stringify({base:voiceLab.base,policy:voiceLab.policy}));
    await openPrompts('journal','voice');
  };
  $('#lab-reset').onclick=()=>{voiceLab.policy=structuredClone(voiceLab.config.current.policy);voiceLab.base=voiceLab.config.current.id;voiceLab.style=voiceLab.policy.journal.defaultStyle;voiceLabPersist();voiceLabRender();};
  voiceLabRenderAsset();voiceLabCount();voiceLabRenderResults();voiceLabLock(voiceLab.busy);
  if(voiceLab.task?.started&&!voiceLab.task.completed)$('#lab-resume').hidden=false;
}
function voiceLabCount(){const el=$('#lab-count');if(el)el.textContent=`${[...voiceLab.transcript].length} 字`;}
function voiceLabReleaseAudio() {
  const player=$('#lab-audio-preview');if(player){player.pause();player.removeAttribute('src');player.load();}
  if(voiceLab.asset?.url)URL.revokeObjectURL(voiceLab.asset.url);
  voiceLab.asset=null;
}
function voiceLabRenderAsset() {
  const node=$('#lab-asset');if(!node)return;
  const asset=voiceLab.asset;
  node.innerHTML=asset?`<div class="lab-asset-row"><div><strong>${aiEscape(asset.name)}</strong><small class="muted">${voiceLabDuration(asset.pcm.byteLength/32000)} · ${(asset.file.size/1024/1024).toFixed(1)} MB</small></div><button id="lab-remove-audio" class="quiet" data-lab-lock>移除音频</button></div><audio id="lab-audio-preview" controls preload="metadata"></audio>`:'';
  if(asset){$('#lab-audio-preview').src=asset.url;$('#lab-remove-audio').onclick=()=>{voiceLabReleaseAudio();voiceLab.preview=null;$('#lab-preview').replaceChildren();voiceLabRenderAsset();voiceLabLock(false);};}
}
async function voiceLabLoad(file) {
  if(voiceLab.busy)return;
  const version=++voiceLab.captureVersion;voiceLab.loading=true;voiceLabLock(true);voiceLabStatus('正在读取音频…');
  try {
    if(file.size>80*1024*1024)throw new Error('音频超过 80 MB，请先截取需要调试的部分。');
    const context=new OfflineAudioContext(1,1,16000);
    let decoded;try{decoded=await context.decodeAudioData(await file.arrayBuffer());}catch{throw new Error('浏览器无法解码这个音频，请使用 M4A、MP3 或 PCM WAV 文件。');}
    if(!decoded.length||decoded.duration>30*60)throw new Error('音频需在 30 分钟以内，请先截取需要调试的部分。');
    const pcm=new Uint8Array(decoded.length*2),view=new DataView(pcm.buffer);
    const channels=Array.from({length:decoded.numberOfChannels},(_,i)=>decoded.getChannelData(i));
    for(let i=0;i<decoded.length;i++){
      let value=0;for(const channel of channels)value+=channel[i]/channels.length;
      value=Math.max(-1,Math.min(1,value));view.setInt16(i*2,Math.round(value*(value<0?32768:32767)),true);
    }
    if(version!==voiceLab.captureVersion)return;
    voiceLabReleaseAudio();voiceLab.asset={file,name:file.name||'网页录音',pcm,url:URL.createObjectURL(file)};
    voiceLab.preview=null;$('#lab-preview')?.replaceChildren();voiceLabRenderAsset();voiceLabStatus('音频已准备好。点击“预览费用并转写”开始。');
  }catch(error){if(version===voiceLab.captureVersion)voiceLabStatus(error.message,true);}
  finally{if(version===voiceLab.captureVersion){voiceLab.loading=false;voiceLabLock(false);}}
}
async function voiceLabRecord() {
  if(voiceLab.busy)return;
  if(!navigator.mediaDevices?.getUserMedia||typeof MediaRecorder==='undefined'){voiceLabStatus('此浏览器无法录音，请使用 HTTPS 下的 Safari 或 Chrome，或上传音频。',true);return;}
  const version=++voiceLab.captureVersion;
  const button=$('#lab-record');button.disabled=true;voiceLabStatus('请允许浏览器使用麦克风。');
  let stream;
  try {
    stream=await navigator.mediaDevices.getUserMedia({audio:{channelCount:1,echoCancellation:true},video:false});
    if(version!==voiceLab.captureVersion||!voiceLabActive()){stream.getTracks().forEach(t=>t.stop());return;}
    const mime=['audio/mp4','audio/webm;codecs=opus','audio/webm'].find(type=>MediaRecorder.isTypeSupported(type));
    const recorder=new MediaRecorder(stream,mime?{mimeType:mime}:undefined),chunks=[];
    const capture={recorder,stream,started:Date.now(),bytes:0,discard:false};voiceLab.capture=capture;
    recorder.ondataavailable=e=>{if(e.data.size){chunks.push(e.data);capture.bytes+=e.data.size;if(capture.bytes>80*1024*1024)voiceLabStopCapture();}};
    recorder.onerror=()=>{capture.discard=true;voiceLabStopCapture();voiceLabStatus('录音未能完成，请重试或上传已有音频。',true);};
    recorder.onstop=()=>{
      clearInterval(capture.timer);stream.getTracks().forEach(t=>t.stop());if(voiceLab.capture===capture){voiceLab.capture=null;voiceLab.busy=false;}
      if(voiceLabActive()){$('#lab-record').textContent='开始录音';$('#lab-record-time').textContent='';voiceLabLock(false);}
      if(!capture.discard&&chunks.length){const type=recorder.mimeType||'audio/webm';const file=new File(chunks,`网页录音.${type.includes('mp4')?'m4a':'webm'}`,{type});voiceLabLoad(file);}
    };
    recorder.start(1000);voiceLabLock(true);button.disabled=false;button.textContent='结束录音';
    $('#lab-file').disabled=true;$('#lab-transcribe').disabled=true;$('#lab-organize').disabled=true;
    voiceLabStatus('正在录音，结束后可试听。');
    capture.timer=setInterval(()=>{const elapsed=(Date.now()-capture.started)/1000;const node=$('#lab-record-time');if(node)node.textContent=voiceLabDuration(elapsed);if(elapsed>=30*60)voiceLabStopCapture();},250);
  }catch(error){stream?.getTracks().forEach(t=>t.stop());voiceLab.capture=null;if(voiceLabActive()){voiceLabStatus(error.name==='NotAllowedError'?'麦克风未获授权。请在浏览器地址栏允许麦克风，或上传音频。':'无法使用麦克风，请检查设备或上传音频。',true);button.disabled=false;}}
}
function voiceLabStopCapture(discard=false) {
  voiceLab.captureVersion++;
  if(discard&&voiceLab.loading){voiceLab.loading=false;voiceLab.busy=false;}
  const capture=voiceLab.capture;if(!capture)return;
  capture.discard ||= discard||!voiceLabActive();
  clearInterval(capture.timer);
  if(capture.recorder.state!=='inactive')capture.recorder.stop();
  capture.stream.getTracks().forEach(t=>t.stop());
}
window.addEventListener('pagehide',()=>{voiceLabStopCapture(true);voiceLabReleaseAudio();});

function voiceLabWords() {
  const words=(voiceLab.words||'').split('\n').map(s=>s.trim()).filter(Boolean);
  if(words.length>32||words.some(w=>[...w].length>32)||new TextEncoder().encode(words.join('')).length>96)throw new Error('热词最多 32 个、每个最多 32 字，总计不超过 96 个 UTF-8 字节。请优先填写容易识别错的短词。');
  return words;
}
function voiceLabSnapshot(words) {return {writingStyle:voiceLab.style,revision:0,blocks:[],transcript:'',editedBlockIDs:[],mediaOnlyBlockIDs:[],manualEdits:[],words};}
function voiceLabChunkEnd(characters,start,maximum) {
 const end=Math.min(start+maximum,characters.length);
 if(end===characters.length)return end;
 for(let i=end-1;i>=start+Math.floor(maximum/2);i--)if(/[\s。！？；.!?;]/u.test(characters[i]))return i+1;
 return end;
}
function voiceLabSamples(stage) {
  const words=voiceLabWords(),name=stage==='transcription'?'语音转写调试':'语音成稿调试';
  const sample=(voice,index)=>({id:uuid(),name:`${name} · ${index+1}`,kind:stage==='transcription'?'voice_transcription':'voice',voiceDebug:true,voice,expected:[]});
  if(stage==='transcription') {
    if(!voiceLab.asset)throw new Error('请先录音或上传音频。');
    const samples=[];const pcm=voiceLab.asset.pcm;
    for(let offset=0;offset<pcm.byteLength;offset+=480000){const s=sample(voiceLabSnapshot(words),samples.length);let binary='';const part=pcm.subarray(offset,offset+480000);for(let i=0;i<part.length;i+=8192)binary+=String.fromCharCode(...part.subarray(i,i+8192));s.audio=btoa(binary);samples.push(s);}
    return samples;
  }
  const text=voiceLab.transcript.trim(),characters=[...text];
  if(!text||characters.length>60000)throw new Error('请提供 1 至 60000 字的转写文本。');
  const narrator={personID:uuid(),name:'我'},samples=[];let offset=0;
  while(offset<characters.length) {
    const snapshot=voiceLabSnapshot(words),targets=[];snapshot.dictationMode=true;snapshot.narrator=narrator;
    const end=voiceLabChunkEnd(characters,offset,4000);snapshot.transcript=characters.slice(offset,end).join('');
    for(let start=offset;start<end;) {
      const next=Math.min(voiceLabChunkEnd(characters,start,500),end);
      const part=characters.slice(start,next).join(''),id=uuid(),sourceID=uuid();start=next;
      snapshot.blocks.push({id,text:part,style:'body'});
      targets.push({id,text:part,sourceText:part,style:'body',completeSource:true,sourceIDs:[sourceID],turns:[{id:sourceID,text:part,personID:narrator.personID,person:narrator.name,speaker:'author'}],retainedText:'',sourceKeys:[]});
    }
    const context=offset?[{id:uuid(),text:characters.slice(Math.max(0,offset-800),offset).join(''),style:'body'}]:[];
    snapshot.polish={targets,context,narrator,paused:false};samples.push(sample(snapshot,samples.length));offset=end;
  }
  return samples;
}
async function voiceLabPrepare(stage,retryTask=null) {
  if(voiceLab.busy||voiceLab.capture)return;
  voiceLabLock(true);voiceLab.preview=null;$('#lab-preview').replaceChildren();
  let task;
  try {
    const indices=retryTask?retryTask.asr.outputs.map((out,i)=>(!out||out.error)?i:-1).filter(i=>i>=0):null;
    const samples=retryTask?indices.map(i=>({...retryTask.asr.samples[i],id:uuid()})):voiceLabSamples(stage);
    task={stage,samples,batches:[],cancelled:false,started:false,source:voiceLab.transcript,policy:structuredClone(voiceLab.policy),base:voiceLab.base,style:voiceLab.style};
    if(stage==='transcription'){task.indices=indices||samples.map((_,i)=>i);task.asr=retryTask?.asr||{samples,outputs:Array(samples.length).fill(null)};}
    voiceLab.task=task;
    voiceLabStatus('正在保存调试草稿与样本…');
    const draft=await promptCommand('drafts',{scope:'journal',baseVersion:task.base,policy:task.policy});task.draft=draft;
    for(let i=0;i<samples.length;i++) {
      if(task.cancelled||!voiceLabActive()||voiceLab.task!==task)throw new Error('已停止准备。');
      await voiceLabAPI(evaluationPath+'/samples',{method:'POST',csrfRequired:true,body:samples[i]});
      voiceLabStatus(`正在准备样本 ${i+1} / ${samples.length}…`);
    }
    for(let i=0;i<samples.length;i+=20) {
      if(task.cancelled||!voiceLabActive()||voiceLab.task!==task)throw new Error('已停止准备。');
      const input={mode:'voice_debug',sampleIDs:samples.slice(i,i+20).map(s=>s.id),candidates:[{revision:draft.id,label:'语音调试草稿'}]};
      const preview=await voiceLabAPI(evaluationPath+'/preview',{method:'POST',csrfRequired:true,body:input});
      task.batches.push({input,preview,key:uuid(),offset:i});
    }
    if(task.cancelled||!voiceLabActive()||voiceLab.task!==task)return;
    voiceLab.preview=task;
    const cost=task.batches.reduce((n,b)=>n+b.preview.plan.reservedNanos,0),calls=task.batches.reduce((n,b)=>n+b.preview.plan.maximumCalls,0);
    $('#lab-preview').innerHTML=`<section class="card lab-confirm"><div><h3>确认本次${stage==='transcription'?'转写':'整理'}</h3><p>最多 ${calls} 次调用，预留 ${evaluationMoney(cost)}。按实际用量结算，不额外调用 AI 评分。</p></div><button class="primary" id="lab-start">确认费用并开始</button><details><summary>查看本次实际请求</summary>${task.batches.map(b=>b.preview.plan.requests.map(r=>`<pre>${aiEscape(JSON.stringify(r,null,2))}</pre>`).join('')).join('')}</details></section>`;
    $('#lab-start').onclick=voiceLabStart;voiceLabStatus('样本已准备好，尚未产生模型调用费用。');
  }catch(error){voiceLabStatus(error.message,true);}
  finally{voiceLabLock(false);}
}
async function voiceLabStart() {
  const generation=ai.generation;
  const task=voiceLab.task;if(!task||task.cancelled||voiceLab.busy)return;
  task.starting=true;task.startFinished=new Promise(resolve=>task.finishStart=resolve);
  voiceLabLock(true);$('#lab-resume').hidden=true;$('#lab-start')?.setAttribute('disabled','');
  try {
    for(const batch of task.batches) {
      if(task.cancelled)break;
      if(batch.run)continue;
      if(task.startError){try{batch.run=await voiceLabAPI(evaluationPath+'/commands/'+batch.key);batch.outputs=new Map();task.started=true;continue;}catch(error){if(error.status!==404)throw error;}}
      const run=await voiceLabAPI(evaluationPath+'/runs',{method:'POST',csrfRequired:true,idempotencyKey:batch.key,body:{...batch.input,previewToken:batch.preview.previewToken}});
      batch.run=run;batch.outputs=new Map();task.started=true;
      if(!voiceLabActive()||generation!==ai.generation)return;
    }
    if(!task.cancelled&&generation===ai.generation)await voiceLabPoll();
  }catch(error){task.startError=true;if(voiceLabActive()&&generation===ai.generation){voiceLabLock(false);voiceLabStatus(`${error.message}；可恢复核对原提交，不会重复扣费。`,true);$('#lab-resume').hidden=false;}}
  finally{task.starting=false;task.finishStart();}
}
async function voiceLabPoll() {
  const generation=ai.generation;
  const task=voiceLab.task;if(!task||!voiceLabActive()||task.polling===generation)return;
  task.polling=generation;
  clearTimeout(ai.poll);voiceLabLock(true);
  try {
    let active=false,finished=0,failed=0;
    for(const batch of task.batches) {
      if(!batch.run){active=true;continue;}
      if(!voiceLabActive()||voiceLab.task!==task||generation!==ai.generation)return;
      const run=await voiceLabAPI(evaluationPath+'/runs/'+batch.run.id);batch.run=run;
      for(const item of run.items) {
        if(['pending','running'].includes(item.status)){active=true;continue;}
        finished++;
        if(!batch.outputs.has(item.index))batch.outputs.set(item.index,await voiceLabAPI(evaluationPath+`/runs/${run.id}/items/${item.index}`));
        const value=batch.outputs.get(item.index);if(item.status!=='completed'||value.result?.outputs?.[0]?.error)failed++;
      }
    }
    if(!voiceLabActive()||task!==voiceLab.task||generation!==ai.generation)return;
    voiceLabStatus(`已完成 ${finished} / ${task.samples.length}${failed?` · ${failed} 段未完成`:''}`);
    if(active){ai.poll=setTimeout(voiceLabPoll,5000);return;}
    task.completed=true;voiceLabLock(false);$('#lab-preview').replaceChildren();
    if(task.stage==='transcription') {
      for(const b of task.batches)for(let i=0;i<b.input.sampleIDs.length;i++){const item=b.outputs.get(i),output=item?.result?.outputs?.[0];task.asr.outputs[task.indices[b.offset+i]]=item?.status==='completed'&&output&&!output.error?output:{error:output?.error||'cancelled'};}
      const ordered=task.asr.outputs;
      if(ordered.every(out=>out&&!out.error)){voiceLab.transcript=ordered.map(out=>out.text).join('\n');$('#lab-transcript').value=voiceLab.transcript;voiceLabCount();voiceLabLock(false);voiceLabStatus('转写完成。可以修改原文，再点击整理。');}
      else {voiceLabRenderFailures(task);voiceLabStatus('部分音频转写未完成，原文未被覆盖。下方保留逐段结果，可重试失败段。',true);}
    } else if(!task.collected) {
      task.collected=true;const outputs=[];for(const b of task.batches)for(let i=0;i<b.input.sampleIDs.length;i++)outputs.push(b.outputs.get(i)?.result?.outputs?.[0]||{error:'cancelled'});
      voiceLab.results.push({outputs,source:task.source,policy:task.policy,style:task.style,draft:task.draft.id,at:new Date().toISOString()});voiceLab.results=voiceLab.results.slice(-2);voiceLabRenderResults();
      voiceLabStatus(failed?'部分成稿未完成，保留原文与诊断。可修改 prompt 后再试。':'成稿完成。可修改左侧 prompt，再用同一份原文试一次。',!!failed);
    }
  }catch(error){if(voiceLabActive()&&generation===ai.generation){voiceLabLock(false);voiceLabStatus(`${error.message}；任务仍保存在服务器，可恢复查看。`,true);$('#lab-resume').hidden=false;}}finally{if(task.polling===generation)task.polling=null;}
}
function voiceLabRenderFailures(task) {
  $('#lab-preview').innerHTML=`<section class="card"><h3>逐段转写结果</h3>${task.asr.outputs.map((out,i)=>`<p><strong>第 ${i+1} 段</strong> ${out?.error?aiEscape(evaluationError(out.error)):aiEscape(out?.text??'未完成')}</p>`).join('')}<button id="lab-retry-failed" class="secondary">只重试失败段</button></section>`;
  $('#lab-retry-failed').onclick=()=>voiceLabPrepare('transcription',task);
}
async function voiceLabCancel() {
  if(voiceLab.capture){voiceLabStopCapture(true);return;}
  if(voiceLab.loading){voiceLab.captureVersion++;voiceLab.loading=false;voiceLabLock(false);voiceLabStatus('已停止读取，原有音频未改变。');return;}
  const task=voiceLab.task;if(!task)return;task.cancelled=true;clearTimeout(ai.poll);
  voiceLabStatus('正在核对提交并停止…');
  if(task.starting)await task.startFinished;
  let error;
  for(const batch of task.batches)if(!batch.run)try{batch.run=await voiceLabAPI(evaluationPath+'/commands/'+batch.key);batch.outputs=new Map();}catch(e){if(task.startError)error=e;}
  for(const batch of task.batches)if(batch.run)try{await voiceLabAPI(evaluationPath+`/runs/${batch.run.id}/cancel`,{method:'POST',csrfRequired:true});}catch(e){error=e;}
  if(voiceLabActive()){voiceLabLock(false);voiceLabStatus(error?'停止请求尚未全部确认，请再次核对，或到运行记录查看。':'已停止；已经发出的调用可能产生费用，已完成结果仍保留。',!!error);$('#lab-resume').hidden=!error;}
}
function voiceLabRenderResults() {
  const node=$('#lab-results');if(!node)return;
  node.innerHTML=voiceLab.results.length?`<div class="lab-heading"><h3>3. 成稿对比</h3><span class="muted">保留最近两次</span></div><div class="lab-comparison">${voiceLab.results.map((r,i)=>{
    const text=r.outputs.map(o=>o.text||'').join('\n\n'),errors=r.outputs.filter(o=>o.error),previous=voiceLab.results.length===2&&i===0;
    const style=r.policy.journal.styles.find(s=>s.id===r.style),cost=r.outputs.reduce((n,o)=>n+(o.costNanos||0),0),ms=r.outputs.reduce((n,o)=>n+(o.milliseconds||0),0);
    return `<article class="card lab-result"><div class="lab-heading"><h4>${previous?'上一次':'本次成稿'}</h4><span class="chip">${errors.length?'未完整完成':'已完成'}</span></div><p class="muted lab-caption">${aiEscape(style?.name||r.style)} · ${(ms/1000).toFixed(1)} 秒调用耗时 · ${evaluationMoney(cost)}</p>${errors.map(o=>`<p class="notice error">${aiEscape(evaluationError(o.error))}</p>`).join('')}<div class="lab-prose">${text?aiEscape(text):'<span class="muted">没有可应用的成稿</span>'}</div><details><summary>本次 prompt、请求与诊断</summary><h5>成稿附加提示词</h5><pre>${aiEscape(r.policy.journal.voice.polishPrompt||'使用默认成稿规则')}</pre><h5>风格提示词</h5><pre>${aiEscape(style?.prompt||'')}</pre><h5>转写原文</h5><pre>${aiEscape(r.source)}</pre>${r.outputs.map(o=>`<h5>${aiEscape(o.model||'模型未返回')}</h5><pre>${aiEscape(JSON.stringify({request:o.request,structured:o.structured,diagnostics:o.diagnostics,unvalidatedOutput:o.diagnosticOutput},null,2))}</pre>`).join('')}</details></article>`;
  }).join('')}</div>`:'';
}
