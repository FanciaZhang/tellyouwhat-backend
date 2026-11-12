const voiceLab = { owner:null, config:null, policy:null, style:null, asset:null, transcript:'', results:[], task:null, preview:null, busy:false, capture:null, captureVersion:0, lastRequest:0, id:null, mode:'dictation', title:'文字样本', samples:[], save:Promise.resolve(), savePending:0, saveError:null, recording:null, reference:null };
const voiceLabDuration = seconds => `${Math.floor(seconds/60)}:${String(Math.floor(seconds%60)).padStart(2,'0')}`;
const voiceLabStorage = () => `voice-lab:${state.user.id}`;
const voiceLabActive = () => ai.screen === 'voice-lab';
const voiceLabBack = () => state.user.role==='admin'?aiHome():showSection('operations');

async function voiceLabAPI(path, options) {
  // Long files create many bounded samples; leave room in the shared admin rate limit.
  const delay = Math.max(0, 700-(Date.now()-voiceLab.lastRequest));
  if(delay) await new Promise(resolve=>setTimeout(resolve,delay));
  voiceLab.lastRequest=Date.now();
  return api(path,options);
}
async function openVoiceLab() {
  aiNavigate('voice-lab');const generation=ai.generation;await voiceLabStopCapture();await voiceLab.save;
  aiFrame('手记 · 语音调试','正在读取当前配置…','',voiceLabBack);
  await aiWork(async()=>{
    const config=await api('/api/v1/ai/voice-debug/config');
    if(generation!==ai.generation)return;
    if(voiceLab.owner!==state.user.id) {
      voiceLabReleaseAudio();Object.assign(voiceLab,{owner:state.user.id,id:null,policy:null,transcript:'',results:[],task:null,busy:false,recording:null});
    }
    voiceLab.config=config;await voiceDrafts.collect(voiceLab.owner);
    if(!voiceLab.id)await voiceLabRestore();
    if(generation!==ai.generation)return;
    voiceLabRender();
    if(voiceLab.task?.started&&!voiceLab.task.completed&&voiceLab.task.batches.every(b=>b.run)&&!voiceLab.task.needsPreview)voiceLabPoll();
  });
}
function voiceLabPersist(invalidate=true) {
  if(invalidate){voiceLab.preview=null;$('#lab-preview')?.replaceChildren();}
  const stripAudio=(key,value)=>key==='audio'?undefined:value;
  const task=voiceLab.task?JSON.parse(JSON.stringify(voiceLab.task,(key,value)=>['finishStart','startFinished','polling','outputs'].includes(key)&&value instanceof Map?undefined:stripAudio(key,value))):null;
  if(task)for(const batch of task.batches)delete batch.outputs;
  const row={owner:voiceLab.owner,id:voiceLab.id,title:voiceLab.title,mode:voiceLab.mode,policy:voiceLab.policy,base:voiceLab.base,style:voiceLab.style,words:voiceLab.words,transcript:voiceLab.transcript,results:voiceLab.results,reference:voiceLab.reference,task,recording:voiceLab.recording,file:voiceLab.asset?.file};
  voiceLab.savePending++;
  voiceLab.save=voiceLab.save.catch(()=>{}).then(()=>voiceDrafts.save(row)).then(()=>{voiceLab.saveError=null;}).catch(error=>{voiceLab.saveError=error;voiceLabStatus('本机草稿未保存。请保留本页并导出录音；空间不足或其他标签页的修改可能阻止了保存。',true);}).finally(()=>{voiceLab.savePending--;});
  return voiceLab.save;
}
async function voiceLabRestore(id=sessionStorage.getItem(voiceLabStorage()+':sample'),retainBusy=false) {
  const row=id?await voiceDrafts.get(voiceLab.owner,id):null;
  voiceLabReleaseAudio();
  Object.assign(voiceLab,{id:row?.id||uuid(),title:row?.title||'文字样本',mode:row?.mode||'dictation',policy:structuredClone(row?.policy||voiceLab.config.current.policy),base:row?.base||voiceLab.config.current.id,style:row?.style||voiceLab.config.current.policy.journal.defaultStyle,words:row?.words||'',transcript:row?.transcript||'',results:row?.results||[],reference:row?.reference||null,task:row?.task||null,recording:row?.recording||null,candidate:null,busy:retainBusy,preview:null});
  if(voiceLab.task){voiceLab.task.caseID=voiceLab.id;voiceLab.task.starting=false;for(const b of voiceLab.task.batches)b.outputs=new Map();if(!voiceLab.task.started&&!voiceLab.task.startError)voiceLab.task=null;}
  if(row?.file)try{await voiceLabDecode(row.file);}catch{voiceLab.asset={file:row.file,name:row.file.name,pcm:null,url:URL.createObjectURL(row.file)};}
  if(row?.recording){const parts=await voiceDrafts.parts(voiceLab.owner,voiceLab.id,row.recording.id);if(parts.length){const file=new File(parts,[row.title||'恢复录音',row.recording.mime.includes('mp4')?'m4a':'webm'].join('.'),{type:row.recording.mime});try{await voiceLabDecode(file);voiceLab.recording=null;}catch{voiceLab.asset={file,name:file.name,pcm:null,url:URL.createObjectURL(file)};}}}
  sessionStorage.setItem(voiceLabStorage()+':sample',voiceLab.id);
  await voiceLabPersist(false);voiceLab.samples=await voiceDrafts.list(voiceLab.owner);
}
async function voiceLabSelect(id) {
  await voiceLabStopCapture();await voiceLab.save;if(voiceLab.saveError)return;
  await voiceLabRestore(id);voiceLabRender();if(voiceLab.task?.started&&!voiceLab.task.completed&&voiceLab.task.batches.every(b=>b.run)&&!voiceLab.task.needsPreview)voiceLabPoll();
}
async function voiceLabNew() {
  await voiceLabStopCapture();await voiceLab.save;if(voiceLab.saveError)return;
  sessionStorage.removeItem(voiceLabStorage()+':sample');await voiceLabRestore(null);voiceLabRender();
}
function voiceLabOwns(task){return voiceLabActive()&&voiceLab.task===task&&task.caseID===voiceLab.id;}
function voiceLabStatus(text,error=false) {
  const node=$('#lab-status');if(!node)return;
  node.textContent=text;node.classList.toggle('error',error);
}
function voiceLabPause(){const player=$('#lab-audio-preview');player?.pause();return voiceLabStopCapture();}
function voiceLabLock(busy) {
  voiceLab.busy=busy;
  if(!voiceLabActive())return;
  aiView().querySelectorAll('[data-lab-lock]').forEach(el=>el.disabled=busy);
  $('#lab-transcribe').disabled=busy||!voiceLab.asset?.pcm||voiceLab.config.writesEnabled===false;
  $('#lab-organize').disabled=busy||!voiceLab.transcript.trim()||voiceLab.config.writesEnabled===false;
  $('#lab-cancel').hidden=!busy;
}
function voiceLabRender() {
  const v=voiceLab.policy.journal.voice,styles=voiceLab.policy.journal.styles;
  const modelIDs=[...new Set([voiceLab.policy.journal.organize.lite.model,voiceLab.policy.journal.organize.pro.model,v.parameters.model,...(voiceLab.config.configuredModels||[])])];
  const modelNames=voiceLab.config.modelNames||{[v.parameters.model]:'语音成稿模型'};
  const style=styles.find(s=>s.id===voiceLab.style)||styles[0];voiceLab.style=style.id;
  // Reuse the existing model fields, but keep the lab draft separate from the publisher.
  prompts.edit=voiceLab.policy;
  aiFrame('手记 · 语音调试','录一段或上传音频，检查转写，再反复调整成稿。',`
    <div class="lab-toolbar"><span class="chip">本机草稿 · 未发布</span><div><button class="quiet" id="lab-history" data-lab-lock>运行记录</button>${voiceLab.config.canPublish!==false?'<button class="secondary" id="lab-publish" data-lab-lock>转到发布管理</button>':''}</div></div>
    <div class="lab-samples"><label>调试样本<select id="lab-sample" data-lab-lock>${voiceLab.samples.map(c=>`<option value="${aiEscape(c.id)}" ${c.id===voiceLab.id?'selected':''}>${aiEscape(c.title)}</option>`).join('')}</select></label><button id="lab-new" class="quiet" data-lab-lock>新样本</button><details class="lab-sample-tools"><summary>管理样本</summary><label>样本名称<input id="lab-title" maxlength="80" value="${aiEscape(voiceLab.title)}" data-lab-lock></label><button id="lab-delete" class="quiet" data-lab-lock>删除本机样本</button></details></div>
    <section class="card lab-audio" aria-labelledby="lab-audio-heading">
      <div><h3 id="lab-audio-heading">1. 准备音频</h3><p class="muted">M4A、MP3、WAV 等浏览器可解码的音频，最长 30 分钟、80 MB。</p></div>
      <div class="lab-audio-actions"><button id="lab-record" class="secondary" data-lab-lock>开始录音</button><label class="lab-upload">上传音频<input id="lab-file" type="file" accept="audio/*,.m4a,.mp3,.wav,.aac,.flac,.ogg,.webm" data-lab-lock></label><label class="lab-mode">转写方式<select id="lab-asr-mode" data-lab-lock><option value="dictation" ${voiceLab.mode==='dictation'?'selected':''}>口述转写</option><option value="recording" ${voiceLab.mode==='recording'?'selected':''}>录音文件转写</option></select></label><button id="lab-transcribe" class="primary" data-lab-lock>转写音频</button><span id="lab-record-time" role="status"></span></div>
      <div id="lab-asset"></div>

      <details class="lab-asr-settings"><summary>口述转写设置</summary><label>热词（每行一个）<textarea id="lab-words" rows="2" data-lab-lock placeholder="人名、地名">${aiEscape(voiceLab.words||'')}</textarea></label><label class="prompt-check"><input id="lab-punctuation" type="checkbox" ${v.automaticPunctuation?'checked':''} data-lab-lock>自动标点</label><label class="prompt-check"><input id="lab-numbers" type="checkbox" ${v.normalizeNumbers?'checked':''} data-lab-lock>数字规范化</label></details>
    </section>
    <div class="lab-feedback"><p id="lab-status" role="status" aria-live="polite"></p><button id="lab-cancel" class="quiet" hidden>停止本次调试</button><button id="lab-resume" class="secondary" hidden>恢复查看进度</button></div>
    <div id="lab-preview"></div><div id="lab-failures"></div>
    <div class="lab-grid">
      <section class="card lab-source" aria-labelledby="lab-source-heading"><div class="lab-heading"><h3 id="lab-source-heading">2. 转写原文</h3><span id="lab-count" class="muted"></span></div>
        <label class="lab-source-label" for="lab-transcript">可直接修改，也可粘贴已有转写文本</label>
        <textarea id="lab-transcript" rows="8" data-lab-lock placeholder="转写结果会显示在这里；只调成稿时，也可以直接粘贴文本。">${aiEscape(voiceLab.transcript)}</textarea>
        <div class="lab-actions"><button id="lab-organize" class="primary" data-lab-lock>整理成稿</button></div>
        <p class="muted lab-caption">口述转写按短段处理；录音文件转写保留整段上下文与说话人信息。校对原文后再整理成稿。</p>
      </section>
      <section class="card lab-config" aria-labelledby="lab-config-heading"><h3 id="lab-config-heading">整理规则</h3>
        <form id="lab-config-form" class="prompt-form">
          <label>成稿附加提示词<textarea id="lab-prompt" rows="4" maxlength="16000" data-lab-lock placeholder="例如：去掉无意义的口头重复，按事件分段，保留具体细节。">${aiEscape(v.polishPrompt||'')}</textarea></label>
          <label>写作风格<select id="lab-style" data-lab-lock>${styles.filter(s=>s.enabled||s.id===voiceLab.style).map(s=>`<option value="${aiEscape(s.id)}" ${s.id===style.id?'selected':''}>${aiEscape(s.name)}</option>`).join('')}</select></label>
          <details><summary>风格提示词</summary><label>当前风格的要求<textarea id="lab-style-prompt" rows="6" maxlength="16000" data-lab-lock>${aiEscape(style.prompt)}</textarea></label></details>

          <details><summary>整理模型与参数</summary>${promptParameters('journal.voice.parameters','最终成稿',{...voiceLab.config,modelCatalogAvailable:false,configuredModels:modelIDs,modelNames})}${voiceLab.config.modelCatalogAvailable!==false?'<button type="button" id="lab-model-catalog" data-lab-lock>选择其他可用模型</button>':''}<small class="muted">${aiEscape(v.parameters.model)}</small></details>
        </form>
        <button id="lab-reset" class="quiet" data-lab-lock>载入当前发布配置</button>
      </section>

    </div>
<section id="lab-results" aria-label="成稿对比"></section>
    <p class="muted lab-caption">音频、校对文本与保留版本先存本机。预览费用时上传样本，确认后才调用模型；服务器加密保留 7 天。删除本机样本不会删除服务器记录。</p>
  `,voiceLabBack);
  aiView().querySelectorAll('#lab-config-form input,#lab-config-form select,#lab-config-form textarea,#lab-config-form button').forEach(el=>el.setAttribute('data-lab-lock',''));
  aiView().querySelector('[data-prompt-model]')?.remove();
  $('#lab-config-form').onsubmit=e=>e.preventDefault();
  const updateRules=e=>{
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
  $('#lab-config-form').oninput=updateRules;$('.lab-asr-settings').oninput=updateRules;
  if($('#lab-model-catalog'))$('#lab-model-catalog').onclick=()=>{const back=()=>{aiNavigate('voice-lab');voiceLabRender();};promptModelPicker('journal.voice.parameters',{back,select:(version,model)=>{Object.assign(v.parameters,{model:version.ModelId,foundationModel:model.Name,modelVersion:version.ModelVersion});delete v.parameters.price;voiceLab.config.modelNames ||= {};voiceLab.config.modelNames[version.ModelId]=model.DisplayName||model.Name;voiceLabPersist();back();}});};
  $('#lab-style').onchange=e=>{voiceLab.style=e.target.value;$('#lab-style-prompt').value=styles.find(s=>s.id===voiceLab.style).prompt;voiceLabPersist();};
  $('#lab-transcript').oninput=e=>{voiceLab.transcript=e.target.value;voiceLabPersist();voiceLabCount();voiceLabLock(false);};
  $('#lab-file').onchange=e=>{const file=e.target.files[0];if(file)voiceLabLoad(file);e.target.value='';};
  $('.lab-asr-settings').hidden=voiceLab.mode==='recording';
  $('#lab-asr-mode').onchange=e=>{voiceLab.mode=e.target.value;$('.lab-asr-settings').hidden=voiceLab.mode==='recording';voiceLabPersist();};
  $('#lab-record').onclick=()=>voiceLab.capture?voiceLabStopCapture():voiceLabRecord();
  $('#lab-transcribe').onclick=()=>voiceLabPrepare('transcription');
  $('#lab-organize').onclick=()=>voiceLabPrepare('organization');
  $('#lab-cancel').onclick=voiceLabCancel;
  $('#lab-resume').onclick=()=>voiceLab.task?.cancelled?voiceLabCancel():voiceLabStart();
  $('#lab-title').oninput=e=>{voiceLab.title=e.target.value||'文字样本';const option=$('#lab-sample').selectedOptions[0];if(option)option.textContent=voiceLab.title;voiceLabPersist(false);};$('#lab-sample').onchange=e=>voiceLabSelect(e.target.value);$('#lab-new').onclick=voiceLabNew;$('#lab-delete').onclick=async()=>{if(!confirm('删除这个本机样本及其录音、校对文本和保留版本？'))return;await voiceLab.save;if(voiceLab.saveError)return;await voiceDrafts.remove(voiceLab.owner,voiceLab.id);voiceLab.task=null;await voiceLabRestore(null);voiceLabRender();};
  $('#lab-history').onclick=()=>evaluationRuns('voice-debug');
  $('#ai-back').onclick=async()=>{await voiceLabStopCapture();await voiceLab.save;if(!voiceLab.saveError)voiceLabBack();};
  if($('#lab-publish'))$('#lab-publish').onclick=async()=>{
    if(voiceLab.busy)return;
    sessionStorage.setItem(`prompt-editor:${state.user.id}:journal`,JSON.stringify({base:voiceLab.base,policy:voiceLab.policy}));
    await openPrompts('journal','voice');
  };
  $('#lab-reset').onclick=()=>{voiceLab.policy=structuredClone(voiceLab.config.current.policy);voiceLab.base=voiceLab.config.current.id;voiceLab.style=voiceLab.policy.journal.defaultStyle;voiceLabPersist();voiceLabRender();};
  voiceLabRenderAsset();voiceLabCount();voiceLabRenderResults();voiceLabLock(voiceLab.busy);
  if(voiceLab.task?.started&&!voiceLab.task.completed)$('#lab-resume').hidden=false;
  if(voiceLab.task?.asr?.outputs.some(o=>o?.error))voiceLabRenderFailures(voiceLab.task);
  if(voiceLab.task?.recordingFailure)voiceLabRenderRecordingFailure(voiceLab.task);
  if(voiceLab.task?.needsPreview){$('#lab-resume').hidden=false;$('#lab-resume').textContent='重新预览费用';$('#lab-resume').onclick=()=>voiceLabRepreview(voiceLab.task);}
}
function voiceLabCount(){const el=$('#lab-count');if(el)el.textContent=`${[...voiceLab.transcript].length} 字`;}
function voiceLabReleaseAudio() {
  if(voiceLab.exports){for(const url of voiceLab.exports.values())URL.revokeObjectURL(url);voiceLab.exports.clear();}
  const player=$('#lab-audio-preview');if(player){player.pause();player.removeAttribute('src');player.load();}
  if(voiceLab.asset?.url)URL.revokeObjectURL(voiceLab.asset.url);
  voiceLab.asset=null;
}
function voiceLabRenderAsset() {
  const node=$('#lab-asset');if(!node)return;
  const asset=voiceLab.asset;
  node.innerHTML=asset?`<div class="lab-asset-row"><div><strong>${aiEscape(asset.name)}</strong><small class="muted">${asset.pcm?voiceLabDuration(asset.pcm.byteLength/32000):'录音片段已保留，可先导出'} · ${(asset.file.size/1024/1024).toFixed(1)} MB</small></div><a class="lab-download" href="${aiEscape(asset.url)}" download="${aiEscape(asset.name)}">导出录音</a><button id="lab-remove-audio" class="quiet" data-lab-lock>移除音频</button></div><audio id="lab-audio-preview" controls preload="metadata"></audio>`:'';
  if(asset){$('#lab-audio-preview').src=asset.url;$('#lab-remove-audio').onclick=async()=>{if(!confirm('移除这份本机音频？校对文本与成稿会保留。'))return;voiceLabReleaseAudio();voiceLab.recording=null;voiceLab.preview=null;$('#lab-preview').replaceChildren();await voiceLabPersist(false);if(!voiceLab.saveError)await voiceDrafts.collect(voiceLab.owner);voiceLabRenderAsset();voiceLabLock(false);};}
}
async function voiceLabLoad(file) {
  if(voiceLab.busy)return;
  const version=++voiceLab.captureVersion;voiceLab.loading=true;voiceLabLock(true);voiceLabStatus('正在读取音频…');
  try {
    await voiceDrafts.space(file.size*2);
    if(file.size>80*1024*1024)throw new Error('音频超过 80 MB，请先截取需要调试的部分。');
    const asset=await voiceLabDecodeFile(file);
    if(version!==voiceLab.captureVersion){URL.revokeObjectURL(asset.url);return;}
    if(voiceLab.asset||voiceLab.transcript.trim()||voiceLab.results.length){await voiceLab.save;if(voiceLab.saveError){URL.revokeObjectURL(asset.url);return;}sessionStorage.removeItem(voiceLabStorage()+':sample');await voiceLabRestore(null,true);}
    voiceLabReleaseAudio();voiceLab.asset=asset;voiceLab.title=asset.name;voiceLab.mode='recording';await voiceLabPersist(false);voiceLab.samples=await voiceDrafts.list(voiceLab.owner);
    voiceLab.preview=null;$('#lab-preview')?.replaceChildren();voiceLabRender();voiceLabStatus('音频已准备好。点击“转写音频”开始。');
  }catch(error){if(version===voiceLab.captureVersion)voiceLabStatus(error.message,true);}
  finally{if(version===voiceLab.captureVersion){voiceLab.loading=false;voiceLabLock(false);}}
}
async function voiceLabDecodeFile(file) {
  const context=new OfflineAudioContext(1,1,16000);
  let decoded;try{decoded=await context.decodeAudioData(await file.arrayBuffer());}catch{throw new Error('浏览器无法解码这个音频，请使用 M4A、MP3 或 PCM WAV 文件。');}
  if(!decoded.length||decoded.duration>1801)throw new Error('音频需在 30 分钟以内，请先截取需要调试的部分。');
  const length=Math.min(decoded.length,1800*16000),pcm=new Uint8Array(length*2),view=new DataView(pcm.buffer);
  const channels=Array.from({length:decoded.numberOfChannels},(_,i)=>decoded.getChannelData(i));
  for(let i=0;i<length;i++){let value=0;for(const channel of channels)value+=channel[i]/channels.length;value=Math.max(-1,Math.min(1,value));view.setInt16(i*2,Math.round(value*(value<0?32768:32767)),true);}
  return {file,name:file.name||'网页录音',pcm,url:URL.createObjectURL(file)};
}
async function voiceLabDecode(file){voiceLab.asset=await voiceLabDecodeFile(file);}
async function voiceLabRecord() {
  if(voiceLab.busy)return;
  if(!navigator.mediaDevices?.getUserMedia||typeof MediaRecorder==='undefined'){voiceLabStatus('此浏览器无法录音，请使用 HTTPS 下的 Safari 或 Chrome，或上传音频。',true);return;}
  if(voiceLab.asset||voiceLab.transcript.trim()||voiceLab.results.length)await voiceLabNew();
  if(voiceLab.saveError)return;
  const version=++voiceLab.captureVersion;
  const button=$('#lab-record');button.disabled=true;voiceLabStatus('请允许浏览器使用麦克风。');
  let stream;
  try {
    stream=await navigator.mediaDevices.getUserMedia({audio:{channelCount:1,echoCancellation:true},video:false});
    if(version!==voiceLab.captureVersion||!voiceLabActive()){stream.getTracks().forEach(t=>t.stop());return;}
    const mime=['audio/mp4','audio/webm;codecs=opus','audio/webm'].find(type=>MediaRecorder.isTypeSupported(type));
    await voiceDrafts.space(80*1024*1024);
    const recorder=new MediaRecorder(stream,mime?{mimeType:mime}:undefined),chunks=[];
    const capture={recorder,stream,started:Date.now(),bytes:0,owner:voiceLab.owner,caseID:voiceLab.id,id:uuid(),index:0};capture.done=new Promise(resolve=>capture.finish=resolve);voiceLab.capture=capture;
    voiceLab.mode='dictation';voiceLab.recording={id:capture.id,mime:recorder.mimeType||mime||'audio/webm'};voiceLab.title='网页录音';await voiceLabPersist(false);if(voiceLab.saveError){voiceLab.capture=null;capture.finish();throw Error('recording_storage_unavailable');}
    recorder.ondataavailable=e=>{if(e.data.size){chunks.push(e.data);capture.bytes+=e.data.size;voiceLab.savePending++;voiceLab.save=voiceLab.save.catch(()=>{}).then(()=>voiceDrafts.part(capture.owner,capture.caseID,capture.id,capture.index++,e.data)).catch(error=>{voiceLab.saveError=error;voiceLabStatus('录音片段未能保存，请结束录音并导出。',true);}).finally(()=>voiceLab.savePending--);if(capture.bytes>80*1024*1024)voiceLabStopCapture();}};
    recorder.onerror=()=>{voiceLabStopCapture();voiceLabStatus('录音未能完成，请重试或上传已有音频。',true);};
    recorder.onstop=async()=>{
      clearInterval(capture.timer);stream.getTracks().forEach(t=>t.stop());
      try{
        await voiceLab.save;
        if(voiceLab.capture!==capture)return;
        voiceLab.capture=null;voiceLab.busy=false;
        if(chunks.length){const type=recorder.mimeType||'audio/webm',file=new File(chunks,`网页录音.${type.includes('mp4')?'m4a':'webm'}`,{type});voiceLabReleaseAudio();await voiceLabDecode(file);voiceLab.recording=null;await voiceLabPersist(false);if(!voiceLab.saveError)await voiceDrafts.deleteParts(capture.owner,capture.caseID,capture.id);}
        voiceLab.samples=await voiceDrafts.list(voiceLab.owner);if(voiceLabActive()){voiceLabRender();voiceLabStatus('录音已保存在本机，可以试听或转写。');}
      }catch(error){if(voiceLabActive()){voiceLabLock(false);voiceLabStatus('录音片段已保留，请重新进入此样本恢复并导出。',true);}}
      finally{capture.finish();}
    };
    recorder.start(1000);voiceLabLock(true);button.disabled=false;button.textContent='结束录音';
    $('#lab-file').disabled=true;$('#lab-transcribe').disabled=true;$('#lab-organize').disabled=true;
    voiceLabStatus('正在录音，结束后可试听。');
    capture.timer=setInterval(()=>{const elapsed=(Date.now()-capture.started)/1000;const node=$('#lab-record-time');if(node)node.textContent=voiceLabDuration(elapsed);if(elapsed>=30*60)voiceLabStopCapture();},250);
  }catch(error){stream?.getTracks().forEach(t=>t.stop());voiceLab.capture=null;if(voiceLabActive()){voiceLabStatus(voiceLab.saveError?'本机无法保存录音，请导出旧样本或释放空间后重试。':error.name==='NotAllowedError'?'麦克风未获授权。请在浏览器地址栏允许麦克风，或上传音频。':'无法使用麦克风，请检查设备或上传音频。',true);button.disabled=false;}}
}
function voiceLabStopCapture() {
  voiceLab.captureVersion++;
  const capture=voiceLab.capture;if(!capture)return Promise.resolve();
  clearInterval(capture.timer);
  if(capture.recorder.state!=='inactive')capture.recorder.stop();
  capture.stream.getTracks().forEach(t=>t.stop());return capture.done;
}
window.addEventListener('beforeunload',event=>{if(voiceLab.capture||voiceLab.savePending||voiceLab.saveError){event.preventDefault();event.returnValue='';}});
window.addEventListener('pagehide',()=>{voiceLabStopCapture();});

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
  const sample=(voice,index)=>({id:uuid(),name:`${[...voiceLab.title].slice(0,35).join('')} · ${name} ${index+1}`,kind:stage==='transcription'?'voice_transcription':'voice',voiceDebug:true,voice,expected:[]});
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
  if(!$('#lab-config-form').reportValidity())return;
  voiceLabLock(true);voiceLab.preview=null;$('#lab-preview').replaceChildren();
  let task={caseID:voiceLab.id,mode:retryTask?.mode||voiceLab.mode,stage,samples:[],batches:[],cancelled:false,started:false,source:voiceLab.transcript,policy:structuredClone(voiceLab.policy),base:voiceLab.base,style:voiceLab.style};voiceLab.task=task;
  try {
    const whole=stage==='transcription'&&task.mode==='recording'&&!retryTask;
    const indices=retryTask?retryTask.asr.outputs.map((out,i)=>(!out||out.error)?i:-1).filter(i=>i>=0):null;
    const samples=retryTask?await Promise.all(indices.map(async i=>{let sample=retryTask.asr.samples[i];if(!sample.audio)sample=await voiceLabAPI(evaluationPath+'/samples/'+sample.id);return {...sample,id:uuid(),voice:{...sample.voice,words:voiceLabWords()}};})):voiceLabSamples(stage);
    if(!voiceLabOwns(task)||task.cancelled)return;task.samples=samples;
    if(whole){task.stage='recording';task.inputs=samples;task.samples=[];}
    if(stage==='transcription'&&!whole){task.indices=indices||samples.map((_,i)=>i);task.asr=retryTask?.asr||{samples,outputs:Array(samples.length).fill(null)};}
    voiceLab.task=task;voiceLabPersist(false);
    voiceLabStatus('正在保存调试草稿与样本…');
    const draft=await voiceLabAPI('/api/v1/ai/voice-debug/drafts',{method:'POST',csrfRequired:true,idempotent:true,body:{scope:'journal',baseVersion:task.base,policy:task.policy}});task.draft=draft;
    for(let i=0;i<samples.length;i++) {
      if(task.cancelled||!voiceLabActive()||voiceLab.task!==task)throw new Error('已停止准备。');
      await voiceLabAPI(evaluationPath+'/samples',{method:'POST',csrfRequired:true,body:samples[i]});
      voiceLabStatus(`正在准备样本 ${i+1} / ${samples.length}…`);
    }
    if(whole){const sample={id:uuid(),name:`${[...voiceLab.title].slice(0,40).join('')} · 整段录音`,kind:'voice_recording',voiceDebug:true,recording:{chunks:samples.map(s=>s.id)},expected:[]};const saved=await voiceLabAPI(evaluationPath+'/samples',{method:'POST',csrfRequired:true,body:sample});task.samples=[saved];samples.splice(0,samples.length,saved);voiceLabPersist(false);}
    for(let i=0;i<samples.length;i+=20) {
      if(task.cancelled||!voiceLabActive()||voiceLab.task!==task)throw new Error('已停止准备。');
      const input={mode:'voice_debug',sampleIDs:samples.slice(i,i+20).map(s=>s.id),candidates:[{revision:draft.id,label:'语音调试草稿'}]};
      const preview=await voiceLabAPI(evaluationPath+'/preview',{method:'POST',csrfRequired:true,body:input});
      task.batches.push({input,preview,key:uuid(),offset:i});voiceLabPersist(false);
    }
    if(task.cancelled||!voiceLabActive()||voiceLab.task!==task)return;
    voiceLab.preview=task;
    const cost=task.batches.reduce((n,b)=>n+b.preview.plan.reservedNanos,0),calls=task.batches.reduce((n,b)=>n+b.preview.plan.maximumCalls,0);
    $('#lab-preview').innerHTML=`<section class="card lab-confirm"><div><h3>确认本次${whole?'录音文件转写':stage==='transcription'?'口述转写':'整理'}</h3><p>最多 ${calls} 次调用，预留 ${evaluationMoney(cost)}。按实际用量结算，不额外调用 AI 评分。</p></div><button class="primary" id="lab-start">确认费用并开始</button><details><summary>查看本次实际请求</summary>${task.batches.map(b=>b.preview.plan.requests.map(r=>`<pre>${aiEscape(JSON.stringify(r,null,2))}</pre>`).join('')).join('')}</details></section>`;
    $('#lab-start').onclick=voiceLabStart;voiceLabStatus('样本已上传，确认费用后开始调用。');$('#lab-preview').scrollIntoView({block:'nearest'});
  }catch(error){if(!task||voiceLabOwns(task))voiceLabStatus(error.message,true);}
  finally{if(!task||voiceLabOwns(task))voiceLabLock(false);}
}
async function voiceLabRepreview(task) {
  if(!voiceLabOwns(task)||voiceLab.busy)return;
  voiceLabLock(true);
  try{
    for(const batch of task.batches)if(!batch.run)batch.preview=await voiceLabAPI(evaluationPath+'/preview',{method:'POST',csrfRequired:true,body:batch.input});
    if(!voiceLabOwns(task))return;
    task.needsPreview=false;voiceLab.preview=task;
    $('#lab-preview').innerHTML=`<section class="card lab-confirm"><p>剩余调用最多预留 ${evaluationMoney(task.batches.filter(b=>!b.run).reduce((n,b)=>n+b.preview.plan.reservedNanos,0))}</p><button id="lab-start" class="primary">确认新费用并继续</button></section>`;
    $('#lab-start').onclick=voiceLabStart;voiceLabStatus('费用预览已更新，请确认后继续。');
  }catch(error){if(voiceLabOwns(task))voiceLabStatus(error.message,true);}finally{if(voiceLabOwns(task))voiceLabLock(false);}
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
      if(task.startError){try{batch.run=await voiceLabAPI(evaluationPath+'/commands/'+batch.key);batch.outputs=new Map();task.started=true;voiceLabPersist(false);continue;}catch(error){if(error.status!==404)throw error;}}
      const run=await voiceLabAPI(evaluationPath+'/runs',{method:'POST',csrfRequired:true,idempotencyKey:batch.key,body:{...batch.input,previewToken:batch.preview.previewToken}});
      batch.run=run;batch.outputs=new Map();task.started=true;voiceLabPersist(false);
      if(!voiceLabActive()||generation!==ai.generation)return;
    }
    if(!task.cancelled&&generation===ai.generation)await voiceLabPoll();
  }catch(error){task.startError=true;if(voiceLabOwns(task)&&generation===ai.generation){voiceLabLock(false);if(error.code==='evaluation_preview_changed'){task.needsPreview=true;voiceLabStatus(error.message,true);const button=$('#lab-start')||$('#lab-resume');button.hidden=false;button.textContent='重新预览费用';button.disabled=false;button.onclick=()=>voiceLabRepreview(task);}else{voiceLabStatus(`${error.message}；可恢复核对原提交，不会重复扣费。`,true);$('#lab-resume').hidden=false;}voiceLabPersist(false);}}
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
    voiceLabPersist(false);
    if(active){ai.poll=setTimeout(voiceLabPoll,5000);return;}
    task.completed=true;voiceLabLock(false);$('#lab-preview').replaceChildren();
    if(task.stage==='transcription') {
      for(const b of task.batches)for(let i=0;i<b.input.sampleIDs.length;i++){const item=b.outputs.get(i),output=item?.result?.outputs?.[0];task.asr.outputs[task.indices[b.offset+i]]=item?.status==='completed'&&output&&!output.error?output:{error:output?.error||'cancelled'};}
      const ordered=task.asr.outputs;
      if(ordered.every(out=>out&&!out.error)){voiceLab.transcript=ordered.map(out=>out.text).join('\n');$('#lab-failures').replaceChildren();$('#lab-transcript').value=voiceLab.transcript;voiceLabCount();voiceLabLock(false);voiceLabStatus('转写完成。可以修改原文，再点击整理。');}
      else {voiceLabRenderFailures(task);voiceLabStatus('部分音频转写未完成，原文未被覆盖。下方保留逐段结果，可重试失败段。',true);}
    } else if(task.stage==='recording'){
      const output=task.batches[0].outputs.get(0)?.result?.outputs?.[0];
      if(output&&!output.error){task.recordingFailure=null;$('#lab-failures').replaceChildren();voiceLab.transcript=output.text;$('#lab-transcript').value=voiceLab.transcript;voiceLabCount();voiceLabLock(false);voiceLabStatus('整段录音转写完成，可以校对后整理。');}
      else{task.recordingFailure={error:output?.error||'cancelled',recordingTaskID:output?.recordingTaskID};voiceLabRenderRecordingFailure(task);}
    } else if(!task.collected) {
      task.collected=true;const outputs=[];for(const b of task.batches)for(let i=0;i<b.input.sampleIDs.length;i++)outputs.push(b.outputs.get(i)?.result?.outputs?.[0]||{error:'cancelled'});
      voiceLab.results.push({audio:voiceLab.asset?.name||'文字样本',id:uuid(),name:`成稿 ${voiceLab.results.length+1}`,outputs,source:task.source,policy:task.policy,style:task.style,draft:task.draft.id,at:new Date().toISOString()});voiceLabRenderResults();
      voiceLabStatus(failed?'部分成稿未完成，原文与诊断已保留，可调整规则后重试。':'成稿完成。修改整理规则后，可用同一份原文再试一次。',!!failed);
    }
    voiceLabPersist(false);
  }catch(error){if(voiceLabOwns(task)&&generation===ai.generation){voiceLabLock(false);voiceLabStatus(`${error.message}；任务仍保存在服务器，可恢复查看。`,true);$('#lab-resume').hidden=false;}}finally{if(task.polling===generation)task.polling=null;}
}
function voiceLabRenderRecordingFailure(task){
 const failure=task.recordingFailure;
 $('#lab-failures').innerHTML=`<section class="card"><p>${aiEscape(evaluationError(failure.error))}</p><button id="lab-recover-recording" data-lab-lock>${failure.recordingTaskID?'核对原转写':'重新预览转写'}</button></section>`;
 $('#lab-recover-recording').onclick=()=>failure.recordingTaskID?voiceLabReconcile(task):voiceLabPrepare('transcription');
}
async function voiceLabReconcile(task){
 if(!voiceLabOwns(task)||voiceLab.busy)return;
 voiceLabLock(true);
 try{const batch=task.batches[0];await voiceLabAPI(evaluationPath+`/runs/${batch.run.id}/items/0/reconcile-recording`,{method:'POST',csrfRequired:true});if(!voiceLabOwns(task))return;batch.outputs=new Map();task.completed=false;task.recordingFailure=null;$('#lab-failures').replaceChildren();voiceLabPersist(false);voiceLabStatus('正在查询原录音任务，不会重新提交音频。');await voiceLabPoll();}catch(error){if(voiceLabOwns(task)){voiceLabStatus(error.message,true);voiceLabLock(false);}}
}
async function voiceLabFromRun(run){
 await openVoiceLab();await voiceLabNew();if(voiceLab.saveError)return;
 const id=voiceLab.id,generation=ai.generation;
 const items=await Promise.all(run.items.map(item=>voiceLabAPI(evaluationPath+`/runs/${run.id}/items/${item.index}`)));
 const outputs=items.map(item=>item.result?.outputs?.[0]).filter(Boolean),candidate=run.plan.candidates[0].revision;
 const samples=run.plan.samples[0]?.kind==='voice'?await Promise.all(run.plan.samples.map(sample=>voiceLabAPI(evaluationPath+'/samples/'+sample.id))):null;
 if(!voiceLabActive()||voiceLab.id!==id||generation!==ai.generation)return;
 const style=samples?.[0]?.voice.writingStyle||candidate.policy.journal.defaultStyle;
 const source=samples?samples.map(sample=>sample.voice.transcript).join(''):outputs.map(o=>o.transcript||o.text||'').join('\n');
 Object.assign(voiceLab,{title:run.plan.samples[0]?.name||'历史调试',transcript:source,policy:structuredClone(candidate.policy),base:candidate.baseVersion||voiceLab.config.current.id,style,results:run.plan.samples[0]?.kind==='voice'?[{id:uuid(),name:'历史成稿',outputs,source,policy:candidate.policy,style,at:run.createdAt,draft:candidate.id}]:[]});
 await voiceLabPersist(false);voiceLab.samples=await voiceDrafts.list(voiceLab.owner);voiceLabRender();voiceLabStatus('原文与规则已载入本机新样本；未重新调用模型。');
}
function voiceLabRenderFailures(task) {
  $('#lab-failures').innerHTML=`<section class="card"><h3>逐段转写结果</h3>${task.asr.outputs.map((out,i)=>`<p><strong>第 ${i+1} 段</strong> ${out?.error?aiEscape(evaluationError(out.error)):aiEscape(out?.text??'未完成')}</p>`).join('')}<button id="lab-retry-failed" class="secondary">只重试失败段</button></section>`;
  $('#lab-retry-failed').onclick=()=>voiceLabPrepare('transcription',task);
}
async function voiceLabCancel() {
  if(voiceLab.capture){await voiceLabStopCapture();return;}
  if(voiceLab.loading){voiceLab.captureVersion++;voiceLab.loading=false;voiceLabLock(false);voiceLabStatus('已停止读取，原有音频未改变。');return;}
  const task=voiceLab.task;if(!task)return;task.cancelled=true;clearTimeout(ai.poll);
  voiceLabStatus('正在核对提交并停止…');
  if(task.starting)await task.startFinished;
  let error;
  for(const batch of task.batches)if(!batch.run)try{batch.run=await voiceLabAPI(evaluationPath+'/commands/'+batch.key);batch.outputs=new Map();}catch(e){if(task.startError)error=e;}
  for(const batch of task.batches)if(batch.run)try{await voiceLabAPI(evaluationPath+`/runs/${batch.run.id}/cancel`,{method:'POST',csrfRequired:true});}catch(e){error=e;}
  if(voiceLabOwns(task)){voiceLabLock(false);voiceLabPersist(false);voiceLabStatus(error?'停止请求尚未全部确认，请再次核对，或到运行记录查看。':'已停止；已经发出的调用可能产生费用，已完成结果仍保留。',!!error);$('#lab-resume').hidden=!error;}
}
function voiceLabRenderResults() {
  const node=$('#lab-results');if(!node)return;
  const results=voiceLab.results;
  if(!results.length){node.innerHTML='';return;}
  const latest=results.at(-1),reference=results.find(r=>r.id===voiceLab.reference)||results.at(-2),candidate=results.find(r=>r.id===voiceLab.candidate)||latest;
  const selected=reference&&reference!==candidate?[reference,candidate]:[candidate],sameSource=selected.length===2&&reference.source===candidate.source;
  node.innerHTML=`<div class="lab-heading"><h3>3. 成稿对比</h3><span class="muted">${results.length} 个本机版本</span></div><div class="lab-comparison-controls"><label>对照版本<select id="lab-reference"><option value="">上一次</option>${results.map(r=>`<option value="${r.id}" ${r.id===voiceLab.reference?'selected':''}>${aiEscape(r.name)}${r.kept?' · 已保留':''}</option>`).join('')}</select></label><label>查看版本<select id="lab-candidate">${results.map(r=>`<option value="${r.id}" ${r.id===candidate.id?'selected':''}>${aiEscape(r.name)}${r.kept?' · 已保留':''}</option>`).join('')}</select></label>${sameSource?'<label class="prompt-check"><input id="lab-diff" type="checkbox">标出文字差异</label>':''}</div>${selected.length===2&&!sameSource?'<p class="notice">两个版本的原文不同，请分别检查，不能直接据此判断提示词优劣。</p>':''}<div class="lab-comparison">${selected.map((r,index)=>{
    const text=r.outputs.map(o=>o.text||'').join('\n\n'),errors=r.outputs.filter(o=>o.error),style=r.policy.journal.styles.find(s=>s.id===r.style);
    const cost=r.outputs.reduce((n,o)=>n+(o.costNanos||0),0),ms=r.outputs.reduce((n,o)=>n+(o.milliseconds||0),0);
    return `<article class="card lab-result"><div class="lab-heading"><label>版本名称<input data-lab-name="${r.id}" maxlength="80" value="${aiEscape(r.name)}"></label><span class="chip">${errors.length?'未完整完成':'已完成'}</span></div><p class="muted lab-caption">${aiEscape(r.audio||voiceLab.title)} · ${aiEscape(style?.name||r.style)} · ${(ms/1000).toFixed(1)} 秒 · ${evaluationMoney(cost)}</p>${errors.map(o=>`<p class="notice error">${aiEscape(evaluationError(o.error))}</p>`).join('')}<div class="lab-prose" data-lab-prose="${index}">${text?aiEscape(text):'<span class="muted">没有可应用的成稿</span>'}</div><div class="lab-result-actions"><button data-lab-copy="${r.id}">复制成稿</button><button data-lab-export="${r.id}">导出文本</button><button data-lab-keep="${r.id}" aria-pressed="${!!r.kept}">${r.kept?'已保留':'保留此版本'}</button><button data-lab-use="${r.id}">用这版规则继续调试</button></div><details><summary>提示词、原文与技术详情</summary><h5>成稿附加提示词</h5><pre>${aiEscape(r.policy.journal.voice.polishPrompt||'使用默认成稿规则')}</pre><h5>风格提示词</h5><pre>${aiEscape(style?.prompt||'')}</pre><h5>转写原文</h5><pre>${aiEscape(r.source)}</pre>${r.outputs.map(o=>`<h5>${aiEscape(o.model||'模型未返回')}</h5><pre>${aiEscape(JSON.stringify({request:o.request,structured:o.structured,diagnostics:o.diagnostics,unvalidatedOutput:o.diagnosticOutput},null,2))}</pre>`).join('')}</details></article>`;
  }).join('')}</div>`;
  $('#lab-reference').onchange=e=>{voiceLab.reference=e.target.value;voiceLabPersist(false);voiceLabRenderResults();};
  $('#lab-candidate').onchange=e=>{voiceLab.candidate=e.target.value;voiceLabRenderResults();};
  node.querySelectorAll('[data-lab-name]').forEach(input=>input.oninput=()=>{results.find(r=>r.id===input.dataset.labName).name=input.value;voiceLabPersist(false);});
  node.querySelectorAll('[data-lab-keep]').forEach(button=>button.onclick=()=>{const r=results.find(r=>r.id===button.dataset.labKeep);r.kept=!r.kept;voiceLab.reference=r.id;voiceLabPersist(false);voiceLabRenderResults();});
  node.querySelectorAll('[data-lab-copy]').forEach(button=>button.onclick=async()=>{try{await navigator.clipboard.writeText(results.find(r=>r.id===button.dataset.labCopy).outputs.map(o=>o.text||'').join('\n\n'));voiceLabStatus('已复制成稿。');}catch{voiceLabStatus('无法使用剪贴板，请导出文本。',true);}});
  node.querySelectorAll('[data-lab-export]').forEach(button=>button.onclick=()=>{const r=results.find(r=>r.id===button.dataset.labExport);voiceLab.exports ||= new Map();if(!voiceLab.exports.has(r.id))voiceLab.exports.set(r.id,URL.createObjectURL(new Blob([r.outputs.map(o=>o.text||'').join('\n\n')],{type:'text/plain;charset=utf-8'})));const link=document.createElement('a');link.href=voiceLab.exports.get(r.id);link.download=(r.name||'手记成稿').replace(/[\\/:*?"<>|]/g,'_')+'.txt';link.click();});
  node.querySelectorAll('[data-lab-use]').forEach(button=>button.onclick=()=>{const r=results.find(r=>r.id===button.dataset.labUse);voiceLab.policy=structuredClone(r.policy);voiceLab.style=r.style;voiceLabPersist();voiceLabRender();$('#lab-prompt').focus();});
  if($('#lab-diff'))$('#lab-diff').onchange=e=>{const a=reference.outputs.map(o=>o.text||'').join('\n\n'),b=candidate.outputs.map(o=>o.text||'').join('\n\n');node.querySelector('[data-lab-prose="0"]').innerHTML=e.target.checked?evaluationTextDifference(b,a,'del'):aiEscape(a);node.querySelector('[data-lab-prose="1"]').innerHTML=e.target.checked?evaluationTextDifference(a,b,'ins'):aiEscape(b);};
}
