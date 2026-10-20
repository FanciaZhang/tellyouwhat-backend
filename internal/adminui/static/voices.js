const voices = {data:null,edit:null,proofs:new Map(),query:'',language:'',page:0,audio:null,audioURL:null};
const voiceBase = '/api/v1/journal/read-aloud/catalog';
const voiceStyles = {natural:'自然',calm:'舒缓',storytelling:'讲述'};
const voiceDialects = {beijing:'北京',dongbei:'东北',henan:'河南',shaanxi:'陕西',shanghai:'上海',sichuan:'四川',tianjin:'天津',yue:'粤语'};
const voiceBilling = {unreviewed:'尚未核对',standard:'标准公共音色',premium:'额外收费', 'voice-slot':'复刻槽位',unavailable:'账号不可用'};
function voiceConfiguration(v){
 return {id:v.id,name:v.name,description:v.description||'',languages:[...(v.languages||[])],styles:[...(v.styles||['natural'])],dialects:[...(v.dialects||[])],supportsPitch:!!v.supportsPitch,previewURL:v.previewURL||'',billingClass:v.billingClass||'unreviewed',enabled:!!v.enabled,rank:v.rank??100,probeText:v.probeText||''};
}
function voicePolicy(v){return JSON.stringify([v.id,v.billingClass,v.probeText,v.languages,v.styles,v.dialects,v.supportsPitch]);}
function voiceRememberProof(v){if(v.verification)voices.proofs.set(v.id,{policy:voicePolicy(v),value:v.verification});}
function voiceProof(v){const p=voices.proofs.get(v.id);return p?.policy===voicePolicy(v)?p.value:null;}
function voiceStorage(){return 'voice-editor:'+state.user.id;}
function voicePersist(){
 ai.dirty=true;
 sessionStorage.setItem(voiceStorage(),JSON.stringify({edit:voices.edit,proofs:[...voices.proofs]}));
 const status=$('#voice-save-status');if(status)status.textContent='有未发布的修改 · 已暂存在本机';
}
function voiceStopPreview(){
 if(voices.audio){voices.audio.pause();voices.audio.removeAttribute('src');voices.audio.load();voices.audio=null;}
 if(voices.audioURL){URL.revokeObjectURL(voices.audioURL);voices.audioURL=null;}
}
function voiceBack(){if(!ai.dirty||confirm('修改已暂存在本机，离开音色管理？'))aiHome();}
async function openVoiceCatalog(){
 aiNavigate('voices');
 aiFrame('手记 · 朗读音色','正在读取已发布目录…','',voiceBack);
 await aiWork(async generation=>{
  const data=await api(voiceBase);if(generation!==ai.generation)return;
  voices.data=data;
  let saved;try{saved=JSON.parse(sessionStorage.getItem(voiceStorage()));}catch{}
  voices.proofs=new Map(Array.isArray(saved?.proofs)?saved.proofs.filter(p=>Array.isArray(p)&&p.length===2):[]);
  (data.current?.voices||[]).forEach(voiceRememberProof);
  voices.edit=Array.isArray(saved?.edit?.voices)?saved.edit:{baseVersion:data.current?.version||'',defaultVoiceID:data.current?.defaultVoiceID||'',voices:(data.current?.voices||[]).map(voiceConfiguration)};
  ai.dirty=Array.isArray(saved?.edit?.voices);voices.page=0;voiceList();
 });
}
function voiceControls(){return '<div class="ai-actions"><button id="voice-sync" data-ai-work '+(!voices.data.writesEnabled||!voices.data.syncEnabled?'disabled':'')+'>同步官方候选</button><button id="voice-add" '+(!voices.data.writesEnabled?'disabled':'')+'>添加音色</button><button id="voice-history">历史版本</button><button id="voice-discard" class="quiet">放弃本机草稿并刷新</button></div>';}
function voiceList(){
 aiNavigate('voices');
 const current=voices.data.current,stale=voices.edit.baseVersion!==(current?.version||'');
 aiFrame('手记 · 朗读音色','候选与发布目录分开管理。只有标准计费且验证通过的声音可以开放。',
  '<div class="card voice-catalog">'+(stale?'<p class="notice error">本机草稿基于较早版本，请先合并最新目录。</p><button id="voice-rebase" data-ai-work>合并最新发布版</button>':'')+
  voiceControls()+'<div class="prompt-two voice-filters"><label>搜索音色<input id="voice-search" type="search" value="'+aiEscape(voices.query)+'" placeholder="名称、说明或 ID"></label><label>语言<select id="voice-language"><option value="">全部语言</option>'+[...new Set(voices.edit.voices.flatMap(v=>v.languages))].sort().map(v=>'<option value="'+aiEscape(v)+'" '+(v===voices.language?'selected':'')+'>'+aiEscape(v)+'</option>').join('')+'</select></label></div><p id="voice-count" class="muted"></p><div id="voice-rows" class="ai-list"></div><div id="voice-pages" class="ai-actions"></div><div class="ai-foot"><span class="muted" id="voice-save-status">'+(ai.dirty?'有未发布的修改 · 已暂存在本机':'当前为已发布目录')+'</span><button id="voice-save" class="primary" data-ai-work '+(!voices.data.writesEnabled||stale?'disabled':'')+'>保存草稿并预览</button></div></div>',voiceBack);
 const render=()=>{
  const q=voices.query.toLowerCase(),rows=voices.edit.voices.map((v,i)=>({v,i})).filter(({v})=>(!voices.language||v.languages.includes(voices.language))&&(v.name+' '+v.description+' '+v.id).toLowerCase().includes(q)).sort((a,b)=>a.v.rank-b.v.rank||a.v.name.localeCompare(b.v.name,'zh-CN'));
  const start=voices.page*50;$('#voice-count').textContent='共 '+rows.length+' 个声音 · 已启用 '+voices.edit.voices.filter(v=>v.enabled).length+' 个';
  $('#voice-rows').innerHTML=rows.slice(start,start+50).map(({v,i})=>'<article class="ai-row voice-row"><div><strong>'+aiEscape(v.name)+'</strong><small>'+aiEscape(v.id)+'</small><small>'+aiEscape(v.languages.join('、'))+'</small></div><div class="ai-modelcell"><span>'+aiEscape(v.description)+'</span><small>'+aiEscape(voiceBilling[v.billingClass]||v.billingClass)+' · '+(voiceProof(v)?'已验证':'待验证')+' · '+(v.enabled?'已启用':'未启用')+(voices.edit.defaultVoiceID===v.id?' · 默认声音':'')+'</small></div><button data-voice-edit="'+i+'">管理</button></article>').join('')||'<p class="empty">没有匹配的声音。可以同步官方候选或手动添加。</p>';
  $('#voice-pages').innerHTML=(voices.page?'<button id="voice-prev">上一页</button>':'')+(start+50<rows.length?'<button id="voice-next">下一页</button>':'');
  aiClick('[data-voice-edit]',b=>voiceEditor(Number(b.dataset.voiceEdit)));
  if($('#voice-prev'))$('#voice-prev').onclick=()=>{voices.page--;render();};
  if($('#voice-next'))$('#voice-next').onclick=()=>{voices.page++;render();};
 };
 $('#voice-search').oninput=e=>{if(!e.isComposing){voices.query=e.target.value;voices.page=0;render();}};
 $('#voice-search').oncompositionend=e=>{voices.query=e.target.value;voices.page=0;render();};
 $('#voice-language').onchange=e=>{voices.language=e.target.value;voices.page=0;render();};
 $('#voice-sync').onclick=()=>aiWork(async generation=>{
  const result=await api(voiceBase+'/sync',{method:'POST',csrfRequired:true});if(generation!==ai.generation)return;
  const ids=new Set(voices.edit.voices.map(v=>v.id));let added=0;
  for(const v of result.voices||[])if(!ids.has(v.id)){voices.edit.voices.push(voiceConfiguration(v));ids.add(v.id);added++;}
  voicePersist();voiceList();notice('新增 '+added+' 个未启用候选，已有修改保持不变。'+((result.issues||[]).length?'另有 '+result.issues.length+' 项元数据需要核对。':''));
 });
 $('#voice-add').onclick=()=>{voices.edit.voices.push({id:'',name:'新音色',description:'',languages:['zh-cn'],styles:['natural'],dialects:[],supportsPitch:false,previewURL:'',billingClass:'unreviewed',enabled:false,rank:100,probeText:'今天沿着河边散步，微风吹过树梢。'});voicePersist();voiceEditor(voices.edit.voices.length-1,true);};
 $('#voice-history').onclick=()=>voiceHistory();
 $('#voice-discard').onclick=()=>{if(confirm('放弃本机未发布修改，重新读取最新目录？')){sessionStorage.removeItem(voiceStorage());ai.dirty=false;openVoiceCatalog();}};
 $('#voice-save').onclick=voiceSaveDraft;render();
 if($('#voice-rebase'))$('#voice-rebase').onclick=voiceRebase;
}
function voiceField(v,key,label,type='text',extra=''){
 const value=aiEscape(v[key]??'');
 return '<label>'+label+(type==='textarea'?'<textarea name="'+key+'" rows="3" '+extra+'>'+value+'</textarea>':'<input name="'+key+'" type="'+type+'" value="'+value+'" '+extra+'>')+'</label>';
}
function voiceChecks(v,key,options){return Object.entries(options).map(([value,label])=>'<label class="prompt-check"><input type="checkbox" name="'+key+'" value="'+value+'" '+(v[key].includes(value)?'checked':'')+' '+(key==='styles'&&value==='natural'?'disabled':'')+'>'+label+'</label>').join('');}
function voiceEditor(index,newVoice=false){
 aiNavigate('voice-editor');const v=voices.edit.voices[index],proof=voiceProof(v),writable=voices.data.writesEnabled;
 aiFrame(v.name||'新音色','能力变更需要重新验证。声音说明和排序的修改不会重新消耗合成字符。',
  '<form class="card prompt-form" id="voice-form"><fieldset '+(!writable?'disabled':'')+'><legend>展示与调用</legend>'+
  voiceField(v,'id','稳定音色 ID','text','required pattern="[A-Za-z0-9_]{1,128}" '+(!newVoice&&voices.data.current?.voices.some(old=>old.id===v.id)?'readonly':''))+
  voiceField(v,'name','名称','text','required maxlength="80"')+voiceField(v,'description','说明','textarea','maxlength="500"')+
  '<div class="prompt-two">'+voiceField(v,'rank','排序（小的在前）','number','required min="0" max="10000" step="1"')+'<label>计费类别<select name="billingClass">'+Object.entries(voiceBilling).map(([id,name])=>'<option value="'+id+'" '+(v.billingClass===id?'selected':'')+'>'+name+'</option>').join('')+'</select></label></div>'+
  voiceField({...v,languages:v.languages.join(', ')},'languages','语言代码（逗号分隔，例如 zh-cn, en）','text','required')+
  voiceField(v,'previewURL','官方示例音频 HTTPS 地址','url','maxlength="2048" pattern="https://.+"')+
  voiceField(v,'probeText','验证与个性化试听短句','textarea','required')+'<p class="muted">最多 180 个 UTF-8 字节。使用公开短句，不填私人手记。</p></fieldset>'+
  '<fieldset '+(!writable?'disabled':'')+'><legend>需要验证的能力</legend><div class="voice-checks">'+voiceChecks(v,'styles',voiceStyles)+'</div><div class="voice-checks">'+voiceChecks(v,'dialects',voiceDialects)+'</div><label class="prompt-check"><input type="checkbox" name="supportsPitch" '+(v.supportsPitch?'checked':'')+'>支持音调调整</label></fieldset>'+
  '<p class="muted" id="voice-proof">'+(proof?'验证完成于 '+formatTime(proof.verifiedAt)+'，共 '+proof.cases+' 项。':'尚无匹配当前能力的验证记录。')+'</p>'+
  '<div class="ai-actions"><button type="button" id="voice-official-preview" '+(!v.previewURL?'disabled':'')+'>试听官方示例</button><button type="button" id="voice-probe" data-ai-work '+(!writable||!voices.data.probesEnabled?'disabled':'')+'>验证并试听（消耗字符）</button><button type="button" id="voice-probe-check" class="hidden" data-ai-work>检查上次验证结果</button></div><div id="voice-audio"></div>'+
  '<fieldset '+(!writable?'disabled':'')+'><legend>开放范围</legend><label class="prompt-check"><input type="checkbox" name="enabled" '+(v.enabled?'checked':'')+'>向 App 开放这个声音</label><button type="button" id="voice-default" '+(!v.enabled||v.id===voices.edit.defaultVoiceID?'disabled':'')+'>'+(v.id===voices.edit.defaultVoiceID?'当前默认声音':'设为默认声音')+'</button><p class="muted">发布时会再次校验费用、验证结果和默认声音。</p></fieldset><div class="ai-foot"><button type="submit">返回音色目录</button><button type="button" class="quiet" id="voice-remove" '+(!writable?'disabled':'')+'>从草稿移除</button></div></form>',voiceList);
 const form=$('#voice-form');
 function changed(event){
  const el=event.target;if(!el.name||el.disabled)return;
  v[el.name]=el.name==='languages'?[...new Set(el.value.split(/[,，\s]+/).filter(Boolean))]:el.name==='styles'||el.name==='dialects'?[...form.querySelectorAll('[name="'+el.name+'"]:checked')].map(e=>e.value):el.type==='checkbox'?el.checked:el.type==='number'?Number(el.value):el.value;
  voicePersist();$('#voice-proof').textContent=voiceProof(v)?'当前能力已验证。':'能力待验证，发布前需完成验证。';
  $('#voice-default').disabled=!v.enabled||!v.id||v.id===voices.edit.defaultVoiceID;
  $('#voice-official-preview').disabled=!v.previewURL;
 }
 form.addEventListener('input',changed);form.addEventListener('change',changed);
 form.onsubmit=e=>{e.preventDefault();voiceList();};
 $('#voice-default').onclick=()=>{voices.edit.defaultVoiceID=v.id;voicePersist();voiceEditor(index);};
 $('#voice-remove').onclick=()=>{if(confirm('从未发布草稿移除这个声音？')){voices.edit.voices.splice(index,1);if(voices.edit.defaultVoiceID===v.id)voices.edit.defaultVoiceID='';voicePersist();voiceList();}};
 $('#voice-official-preview').onclick=()=>{try{const u=new URL(v.previewURL);if(u.protocol!=='https:'||u.username||u.password||u.hash)throw new Error('请填写有效的 HTTPS 示例地址');voicePlay(u.href);}catch(error){aiError(error);}};
 const pendingKey='voice-probe:'+state.user.id+':'+v.id;
 let pending;try{pending=JSON.parse(sessionStorage.getItem(pendingKey));}catch{}
 const check=$('#voice-probe-check');check.classList.toggle('hidden',!pending);
 async function probe(replay){
  if(!form.reportValidity())return;
  const body=voiceConfiguration(v);
  if(!replay){
   if(!confirm('验证会对所选语言、风格、方言和音调发起真实合成，消耗测试字符。'+(pending?'上次结果仍需核对；本次将创建新的付费验证。':'')))return;
   pending={key:uuid(),body};sessionStorage.setItem(pendingKey,JSON.stringify(pending));check.classList.remove('hidden');
  }
  if(!pending)return;
  await aiWork(async generation=>{
   await reauthenticate();
   const result=await api(voiceBase+'/probe',{method:'POST',csrfRequired:true,idempotencyKey:pending.key,body:pending.body});
   voiceRememberProof(result.voice);voicePersist();sessionStorage.removeItem(pendingKey);pending=null;
   if(generation!==ai.generation)return;
   voiceEditor(index);
   if(result.audio)voicePlay(voiceWAV(result.audio,result.sampleRate),true);
   notice(result.replayed?'原验证已完成，没有再次合成；可试听官方示例。':'验证成功，请试听样音，确认效果后再启用发布。');
  });
 }
 $('#voice-probe').onclick=()=>probe(false);check.onclick=()=>probe(true);
}
function voiceWAV(encoded,rate){
 const raw=atob(encoded);if(!raw.length||raw.length%2||rate!==24000||raw.length>1048576)throw new Error('试听音频格式不正确');
 const bytes=new Uint8Array(44+raw.length),view=new DataView(bytes.buffer);
 function label(at,text){for(let i=0;i<text.length;i++)bytes[at+i]=text.charCodeAt(i);}
 label(0,'RIFF');view.setUint32(4,36+raw.length,true);label(8,'WAVE');label(12,'fmt ');view.setUint32(16,16,true);view.setUint16(20,1,true);view.setUint16(22,1,true);view.setUint32(24,rate,true);view.setUint32(28,rate*2,true);view.setUint16(32,2,true);view.setUint16(34,16,true);label(36,'data');view.setUint32(40,raw.length,true);
 for(let i=0;i<raw.length;i++)bytes[44+i]=raw.charCodeAt(i);
 return new Blob([bytes],{type:'audio/wav'});
}
function voicePlay(source,blob=false){
 voiceStopPreview();const host=$('#voice-audio');host.replaceChildren();
 const audio=document.createElement('audio');audio.controls=true;audio.preload='none';audio.setAttribute('aria-label','音色试听');audio.referrerPolicy='no-referrer';
 if(blob){voices.audioURL=URL.createObjectURL(source);audio.src=voices.audioURL;}else audio.src=source;
 voices.audio=audio;host.append(audio);audio.onerror=()=>aiError(new Error('示例音频暂不可播放，请检查地址或重新试听。'));
 audio.play().catch(()=>notice('样音已准备好，点击播放即可试听。'));
}
async function voiceCommand(action,body){
 const slot='voice-command:'+state.user.id+':'+action,identity=JSON.stringify(body);let saved;try{saved=JSON.parse(sessionStorage.getItem(slot));}catch{}
 const command=saved?.identity===identity?saved:{identity,key:uuid()};sessionStorage.setItem(slot,JSON.stringify(command));
 const result=await api(voiceBase+'/'+action,{method:'POST',csrfRequired:true,idempotencyKey:command.key,body});sessionStorage.removeItem(slot);return result;
}
function voiceSaveDraft(){return aiWork(async generation=>{const draft=await voiceCommand('drafts',voices.edit);if(generation!==ai.generation)return;draft.voices.forEach(voiceRememberProof);voiceReview(draft);});}
function voiceBlockers(draft){
 const enabled=draft.voices.filter(v=>v.enabled),reasons=[];
 if(!enabled.length)reasons.push('至少启用一个声音');
 if(!enabled.some(v=>v.id===draft.defaultVoiceID))reasons.push('默认声音必须已启用');
 for(const v of enabled)if(v.billingClass!=='standard'||!voiceProof(v))reasons.push(v.name+'：需要标准计费及有效验证');
 return reasons;
}
function voiceReview(draft){
 aiNavigate('voice-review');
 const before=voices.data.current?.voices||[],old=new Map(before.map(v=>[v.id,v])),after=new Map(draft.voices.map(v=>[v.id,v])),changes=[];
 const summaries=v=>v?JSON.stringify(voiceConfiguration(v)):'未配置';
 for(const id of new Set([...old.keys(),...after.keys()]))if(summaries(old.get(id))!==summaries(after.get(id)))changes.push({id,before:old.get(id),after:after.get(id)});
 const blockers=voiceBlockers(draft);
 aiFrame('确认音色目录变更','发布后 App 最多约四小时刷新；服务端立即执行停用限制。',
  '<div class="card"><p>默认声音：'+aiEscape(voices.data.current?.defaultVoiceID||'未设置')+' → '+aiEscape(draft.defaultVoiceID||'未设置')+'</p><p>启用声音：'+before.filter(v=>v.enabled).length+' → '+draft.voices.filter(v=>v.enabled).length+'</p>'+blockers.map(r=>'<p class="notice error">'+aiEscape(r)+'</p>').join('')+'</div><div class="prompt-diff">'+changes.map(c=>'<article class="card"><h3>'+aiEscape(c.after?.name||c.before?.name||c.id)+'</h3><div class="prompt-two"><div><small>当前发布版</small>'+voiceSummary(c.before)+'</div><div><small>候选版本</small>'+voiceSummary(c.after)+'</div></div></article>').join('')+'</div><div class="ai-foot"><button id="voice-edit-again">继续编辑</button><button id="voice-publish" class="primary" data-ai-work '+(blockers.length?'disabled':'')+'>通行密钥确认并发布</button></div>',voiceList);
 $('#voice-edit-again').onclick=voiceList;
 $('#voice-publish').onclick=()=>aiWork(async generation=>{
  await reauthenticate();await voiceCommand('publish',{revision:draft.version,baseVersion:draft.baseVersion});
  sessionStorage.removeItem(voiceStorage());ai.dirty=false;if(generation!==ai.generation)return;
  await openVoiceCatalog();notice('音色目录已发布。');
 });
}
function voiceSummary(v){
 if(!v)return '<p>未配置</p>';
 const rows=[['稳定 ID',v.id],['名称',v.name],['说明',v.description],['语言',v.languages.join('、')],['风格',v.styles.map(s=>voiceStyles[s]||s).join('、')],['方言',v.dialects.map(d=>voiceDialects[d]||d).join('、')||'默认发音'],['音调',v.supportsPitch?'支持调整':'原声'],['计费',voiceBilling[v.billingClass]],['开放',v.enabled?'启用':'停用'],['排序',v.rank],['试听短句',v.probeText],['示例地址',v.previewURL||'未设置']];
 return '<dl class="voice-summary">'+rows.map(([key,value])=>'<dt>'+aiEscape(key)+'</dt><dd>'+aiEscape(value)+'</dd>').join('')+'</dl>';
}
function voiceRebase(){return aiWork(async generation=>{
 const baseVersion=voices.edit.baseVersion;
 const [base,latest]=await Promise.all([baseVersion?api(voiceBase+'/revisions/'+encodeURIComponent(baseVersion)):Promise.resolve({voices:[],defaultVoiceID:''}),api(voiceBase)]);
 if(generation!==ai.generation)return;
 const keyed=v=>({defaultVoiceID:v?.defaultVoiceID||'',voices:Object.fromEntries((v?.voices||[]).map(row=>[row.id,voiceConfiguration(row)]))});
 const merged=promptMergePolicies(keyed(base),keyed(voices.edit),keyed(latest.current));
 aiNavigate('voice-rebase');
 aiFrame('合并最新音色目录','未冲突的修改已合并。冲突项由你选择，之后仍需预览和确认发布。',
  '<div class="card prompt-form">'+merged.conflicts.map((c,i)=>'<label>'+aiEscape(c.path)+'<select data-voice-conflict="'+i+'"><option value="local">保留本机：'+aiEscape(promptDisplay(c.local))+'</option><option value="remote">采用最新：'+aiEscape(promptDisplay(c.remote))+'</option></select></label>').join('')+(merged.conflicts.length?'':'<p>没有字段冲突，可以继续编辑。</p>')+'<button id="voice-merge">保留选择并继续编辑</button></div>',voiceList);
 $('#voice-merge').onclick=()=>{
  aiView().querySelectorAll('[data-voice-conflict]').forEach(el=>{
   if(el.value!=='remote')return;const c=merged.conflicts[Number(el.dataset.voiceConflict)],keys=c.path.split('.'),last=keys.pop(),target=keys.reduce((value,key)=>value[key],merged.policy);
   if(c.remote===undefined)delete target[last];else target[last]=structuredClone(c.remote);
  });
  voices.data=latest;(latest.current?.voices||[]).forEach(voiceRememberProof);
  voices.edit={baseVersion:latest.current?.version||'',defaultVoiceID:merged.policy.defaultVoiceID,voices:Object.values(merged.policy.voices)};
  voicePersist();voiceList();
 };
});}
function voiceHistory(cursor='',previous=[]){
 aiNavigate('voice-history');return aiWork(async generation=>{
  const data=await api(voiceBase+'/history'+(cursor?'?cursor='+encodeURIComponent(cursor):''));if(generation!==ai.generation)return;
  const rows=[...previous,...data.revisions];
  aiFrame('音色目录历史','恢复会生成新草稿，确认发布前当前目录保持有效。','<div class="ai-list">'+rows.map((r,i)=>'<article class="ai-row voice-row"><div><strong>'+(r.version===voices.data.current?.version?'当前发布版':r.publishedAt?'历史发布版':'草稿')+'</strong><small>'+formatTime(r.publishedAt||r.createdAt)+'</small></div><span>'+r.enabledCount+' 个开放声音</span><button data-voice-restore="'+i+'" '+(!voices.data.writesEnabled?'disabled':'')+'>载入为草稿</button></article>').join('')+'</div>'+(data.nextCursor?'<button id="voice-more">更早版本</button>':''),voiceList);
  aiClick('[data-voice-restore]',b=>{
   if(ai.dirty&&!confirm('用历史内容替换本机未发布草稿？'))return;
   aiWork(async generation=>{
    const r=await api(voiceBase+'/revisions/'+encodeURIComponent(rows[Number(b.dataset.voiceRestore)].version));if(generation!==ai.generation)return;
    r.voices.forEach(voiceRememberProof);
    voices.edit={baseVersion:voices.data.current?.version||'',defaultVoiceID:r.defaultVoiceID,voices:r.voices.map(voiceConfiguration)};
    voicePersist();voiceList();notice('历史内容已载入草稿，仍需预览和确认发布。');
   });
  });
  if($('#voice-more'))$('#voice-more').onclick=()=>voiceHistory(data.nextCursor,rows);
 });
}
