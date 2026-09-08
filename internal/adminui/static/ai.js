const aiLabels = { voice_transcription:"语音转写", meal_photo_capture:"拍照记饮食", hydration_cup_estimate:"水杯估算", meal_text_capture:"文字记饮食", meal_decision:"饮食决策", diet_analysis:"饮食分析", health_nutrition_analysis:"健康营养分析", health_behavior_analysis:"健康行为分析" };
const aiEfforts = {"":"模型默认", minimal:"关闭思考",low:"轻度",medium:"中度",high:"深度"};
const aiStates = {queued:"等待执行",dispatching:"正在提交",watching:"等待云端完成",uncertain:"结果待核对",succeeded:"已完成",cancelled:"已撤销",failed:"失败",conflict:"状态已变化"};
const aiActions = {start:"切换模型",step_back:"回退一个阶段",cancel:"撤销本次灰度",reconcile:"核对云端任务",accept_current:"确认当前模型与费用"};
const ai = {data:{},rows:[],endpoints:new Map(),catalog:null,tab:"functions",screen:"home",generation:0,poll:null,query:"",page:0,endpoint:"",choice:null,versions:null,preview:null,editing:null,dirty:false};
const aiView = () => $("#ai-view");
const aiEscape = escapeHTML;
const aiModel = ep => ep?.ModelReference?.FoundationModel;
const aiModelText = m => m?.Name ? `${m.Name} · ${m.ModelVersion}` : "模型尚未同步";
const aiEndpointID = row => row.endpointID || row.endpoint?.Id;
const aiAffected = id => ai.rows.filter(r=>aiEndpointID(r)===id).map(r=>aiLabels[r.operation]||r.operation).join("、") || "暂无当前绑定";
const aiActive = r => r && r.RollingGray<100 && !(r.Status==="Reverted" && r.RollingGray===0);
function stopAI(){clearTimeout(ai.poll);ai.generation++;}
function aiNavigate(screen){stopAI();ai.screen=screen;aiView().setAttribute("aria-busy","false");}
function aiClick(selector,callback){aiView().querySelectorAll(selector).forEach(el=>el.onclick=()=>callback(el));}
function aiError(error){
  let panel=$("#ai-error");if(!panel){panel=document.createElement("p");panel.id="ai-error";panel.className="notice error";panel.setAttribute("role","alert");aiView().append(panel);}
  panel.textContent=error.message;panel.scrollIntoView({block:"nearest"});
}
async function aiWork(callback){
  const generation=ai.generation;
  aiView().querySelector('#ai-error')?.remove();
  const controls=[...aiView().querySelectorAll('[data-ai-work]')].map(el=>[el,el.disabled]);controls.forEach(([el])=>el.disabled=true);
  aiView().setAttribute("aria-busy","true");
  try{await callback(generation);}catch(error){if(generation===ai.generation)aiError(error);}
  finally{if(generation===ai.generation){aiView().setAttribute("aria-busy","false");controls.forEach(([el,disabled])=>{if(el.isConnected)el.disabled=disabled;});}}
}
function aiFrame(title,subtitle,body,back){
  aiView().innerHTML=`${back?'<button class="quiet ai-back" id="ai-back">← 返回</button>':""}<div class="section-head"><div><h2 tabindex="-1">${aiEscape(title)}</h2><p class="muted">${aiEscape(subtitle)}</p></div></div>${body}`;
  if(back)$("#ai-back").onclick=back;
}
function aiHome(){aiNavigate("home");ai.dirty=false;renderAI();}
async function loadAI(){
  aiNavigate("home");const generation=ai.generation;
  if(!ai.rows.length)aiView().innerHTML='<p class="muted">正在同步功能与实际模型…</p>';
  const data=await api("/api/v1/ai/health");if(generation!==ai.generation)return;
  ai.data=data;ai.rows=data.operations||[];
  ai.rows.forEach(r=>{if(r.endpoint?.Id)ai.endpoints.set(r.endpoint.Id,{...ai.endpoints.get(r.endpoint.Id),endpoint:r.endpoint,syncedAt:r.syncedAt});});
  ai.screen="home";renderAI();
}
function aiOwned(){return [...new Set(ai.data.ownedEndpoints||ai.rows.map(aiEndpointID))].filter(Boolean);}
async function aiLoadEndpoints(generation){
  const ids=aiOwned();let next=0;
  await Promise.all(Array.from({length:Math.min(4,ids.length)},async()=>{while(next<ids.length){const id=ids[next++];const data=await api(`/api/v1/ai/endpoints/${encodeURIComponent(id)}`);if(generation===ai.generation)ai.endpoints.set(id,data);}}));
}
function renderAI(){
  if(ai.screen!=="home")return;
  const tabs=[['functions','功能配置'],['endpoints','模型与接入点'],['history','变更记录']];
  let body=`<div class="ai-tabs" role="tablist" aria-label="AI 管理区域">${tabs.map(([id,label])=>`<button role="tab" id="ai-${id}-tab" aria-controls="ai-content" aria-selected="${ai.tab===id}" tabindex="${ai.tab===id?0:-1}" data-ai-tab="${id}">${label}</button>`).join("")}</div><div id="ai-content" role="tabpanel" aria-labelledby="ai-${ai.tab}-tab">`;
  if(ai.tab==='functions'){
    body+=`<div class="ai-list">${ai.rows.map((row,index)=>`<article class="ai-row" data-ai-row="${index}"><strong>${aiEscape(aiLabels[row.operation]||row.operation)}</strong><div class="ai-modelcell"><span>${aiEscape(aiModelText(aiModel(row.endpoint)))}</span><small>${row.current?`思考：${aiEscape(aiEfforts[row.current.policy.reasoningEffort])} · 联网：${row.current.policy.webSearchEnabled?'开启':'关闭'}`:'参数跟随 App 请求'}${row.syncError?` · ${aiEscape(row.syncError)}`:''}</small></div><div class="ai-actions"><button class="quiet" data-ai-params="${index}">参数</button><button class="secondary" data-ai-switch="${aiEscape(aiEndpointID(row))}">切换模型</button>${row.endpoint?.RollingId?`<button class="quiet" data-ai-progress="${aiEscape(aiEndpointID(row))}">灰度进度</button>`:''}</div></article>`).join("")}</div>`;
  }else if(ai.tab==='endpoints'){
    body+=`<div class="ai-list">${aiOwned().map(id=>{const data=ai.endpoints.get(id),ep=data?.endpoint;return `<article class="ai-row"><div><strong>${aiEscape(ep?.Name||'健康接入点')}</strong><small class="ai-id">${aiEscape(id)}</small></div><div class="ai-modelcell"><span>${aiEscape(aiModelText(aiModel(ep)))}</span><small>使用功能：${aiEscape(aiAffected(id))}</small></div><div class="ai-actions"><button class="secondary" data-ai-switch="${aiEscape(id)}">切换模型</button><button class="quiet" data-ai-progress="${aiEscape(id)}">${ep?.RollingId?'灰度进度':'详情'}</button></div></article>`;}).join("")}</div>`;
  }else{body+=aiHistory();}
  body+='</div><div class="ai-foot"><span class="muted">'+(!ai.data.writesEnabled?'配置发布未启用':'')+(!ai.data.rollingWritesEnabled?' · 模型切换未启用':'')+'</span><button class="quiet" id="ai-refresh">刷新</button></div>';
  aiFrame("AI 管理","查看正在使用的模型，调整参数和切换模型。",body);
  aiClick('[data-ai-tab]',b=>{aiNavigate('home');ai.tab=b.dataset.aiTab;renderAI();if(ai.tab!=='functions')aiWork(async g=>{await aiLoadEndpoints(g);if(g===ai.generation)renderAI();});});
  const tabsDOM=[...aiView().querySelectorAll('[data-ai-tab]')];tabsDOM.forEach((el,i)=>el.onkeydown=e=>{let n;if(e.key==='ArrowRight')n=(i+1)%3;else if(e.key==='ArrowLeft')n=(i+2)%3;else if(e.key==='Home')n=0;else if(e.key==='End')n=2;else return;e.preventDefault();tabsDOM[n].click();aiView().querySelectorAll('[data-ai-tab]')[n].focus();});
  aiClick('[data-ai-params]',b=>openAIParams(Number(b.dataset.aiParams)));
  aiClick('[data-ai-switch]',b=>openAIModels(b.dataset.aiSwitch));
  aiClick('[data-ai-progress]',b=>openAIProgress(b.dataset.aiProgress));
  aiClick('[data-ai-history]',b=>{const row=ai.rows[Number(b.dataset.aiHistory)];openAIParams(Number(b.dataset.aiHistory),row.history.find(r=>r.id===b.dataset.revision)?.policy);});
  aiClick('[data-ai-more]',b=>aiWork(async g=>{const row=ai.rows[Number(b.dataset.aiMore)];const page=await api(`/api/v1/ai/health/${row.operation}/history?cursor=${encodeURIComponent(row.nextCursor)}`);if(g!==ai.generation)return;row.history.push(...page.revisions);row.nextCursor=page.nextCursor;renderAI();}));
  $('#ai-refresh').onclick=()=>aiWork(async()=>{await loadAI();if(ai.tab!=='functions'){await aiLoadEndpoints(ai.generation);renderAI();}});
}
function aiHistory(){
  const records=[];
  ai.rows.forEach((row,index)=>(row.history||[]).forEach(r=>records.push({at:r.publishedAt||r.createdAt,html:`<article class="ai-history-row"><div><strong>${aiEscape(aiLabels[row.operation])} · ${r.publishedAt?'参数已发布':'未发布草稿'}</strong><small>${formatTime(r.publishedAt||r.createdAt)} · ${aiEscape(aiEfforts[r.policy.reasoningEffort])} · 联网${r.policy.webSearchEnabled?'开启':'关闭'}</small></div><button class="quiet" data-ai-history="${index}" data-revision="${aiEscape(r.id)}">载入配置</button></article>`})));
  ai.endpoints.forEach((data,id)=>(data.commands||[]).forEach(c=>records.push({at:c.createdAt,html:`<article class="ai-history-row"><div><strong>${aiEscape(aiActions[c.input.action]||c.input.action)} · ${aiEscape(aiStates[c.state]||c.state)}</strong><small>${formatTime(c.createdAt)} · ${aiEscape(aiAffected(id))}</small><small>${aiEscape(c.detail||'')} ${c.input.target?.Name?aiEscape(aiModelText(c.input.target)):''}</small></div><button class="quiet" data-ai-progress="${aiEscape(id)}">查看详情</button></article>`})));
  records.sort((a,b)=>new Date(b.at)-new Date(a.at));
  return `<div class="ai-list">${records.map(r=>r.html).join('')||'<p class="empty">暂无变更记录</p>'}</div>${ai.rows.map((r,i)=>r.nextCursor?`<button class="quiet" data-ai-more="${i}">${aiEscape(aiLabels[r.operation])} · 更早参数记录</button>`:'').join('')}<p class="muted ai-small">每个接入点显示最近 50 次模型操作。</p>`;
}
function openAIParams(index,restored){
  aiNavigate('params');const row=ai.rows[index];
  ai.editing={index,base:row.current?.id||'',policy:structuredClone(restored||row.current?.policy||{endpoint:aiEndpointID(row),reasoningEffort:'minimal',webSearchEnabled:false,timeoutSeconds:ai.data.timeoutSeconds||90,version:''})};ai.dirty=!!restored;renderAIParams();
}
function renderAIParams(){
  const edit=ai.editing,row=ai.rows[edit.index],p=edit.policy;
  const body=`<form id="ai-params-form" class="ai-panel ai-fields"><p class="muted">当前：${row.current?aiEscape(aiEfforts[row.current.policy.reasoningEffort]):'参数跟随 App 请求'}。发布后应用以下设置。</p><label>思考深度<select name="reasoningEffort">${Object.entries(aiEfforts).map(([value,label])=>`<option value="${value}" ${value===p.reasoningEffort?'selected':''}>${label}</option>`).join('')}</select></label><label class="check"><input name="webSearchEnabled" type="checkbox" ${p.webSearchEnabled?'checked':''} ${row.operation!=='meal_decision'?'disabled':''}>允许联网搜索</label>${row.operation!=='meal_decision'?'<p class="muted ai-small">此功能不使用联网搜索。</p>':''}<details><summary>接入点绑定</summary><label>调用接入点<select name="endpoint">${aiOwned().map(id=>`<option value="${aiEscape(id)}" ${id===p.endpoint?'selected':''}>${aiEscape(ai.endpoints.get(id)?.endpoint?.Name||id)}</option>`).join('')}</select></label><p class="muted ai-small">更改此功能调用的接入点。接入点背后的模型请使用“切换模型”。</p></details><div class="ai-foot"><span class="muted ai-small">先检查并查看差异</span><button class="primary" data-ai-work ${!ai.data.writesEnabled?'disabled':''}>预览变更</button></div><p id="ai-check-status" class="muted" role="status"></p></form>`;
  aiFrame(aiLabels[row.operation],"请求参数",body,()=>{if(!ai.dirty||confirm('放弃尚未发布的参数修改？'))aiHome();});
  $('#ai-params-form').oninput=()=>{ai.dirty=true;};
  $('#ai-params-form').onsubmit=e=>{e.preventDefault();const values=new FormData(e.currentTarget);edit.policy={...p,version:'',endpoint:values.get('endpoint'),reasoningEffort:values.get('reasoningEffort'),webSearchEnabled:values.has('webSearchEnabled')};aiWork(async g=>{
    const revision=await aiWaitCheck(()=>aiCommand('drafts',{operation:row.operation,baseVersion:edit.base,policy:edit.policy}),g);
    if(!revision||g!==ai.generation)return;
    const preview=await api(`/api/v1/ai/health/${row.operation}/revisions/${encodeURIComponent(revision.id)}`);if(g!==ai.generation)return;
    if(!preview.canPublish)throw new Error('配置基线已变化，请返回刷新后重新编辑');
    renderAIParameterPreview(row,preview);
  });};
}
function renderAIParameterPreview(row,preview){
  aiNavigate('parameter-preview');
  const before=preview.before,after=preview.after;
  const diff=(name,old,value)=>`<div class="ai-field"><span>${name}</span><span>${aiEscape(old)} → ${aiEscape(value)}</span></div>`;
  aiFrame('确认参数变更',aiLabels[row.operation],`<div class="ai-panel ai-fields">${diff('思考深度',before?aiEfforts[before.reasoningEffort]:'跟随 App 请求',aiEfforts[after.reasoningEffort])}${diff('联网搜索',before?(before.webSearchEnabled?'开启':'关闭'):'跟随 App 请求',after.webSearchEnabled?'开启':'关闭')}${diff('接入点',before?.endpoint||aiEndpointID(row),after.endpoint)}<p class="muted">对新请求生效。已排队任务保留原参数。</p><button id="ai-publish" class="primary" data-ai-work>通行密钥确认并发布</button><p id="ai-check-status" role="status" class="muted"></p></div>`,()=>{aiNavigate('params');renderAIParams();});
  $('#ai-publish').onclick=()=>aiWork(async g=>{await reauthenticate();if(g!==ai.generation)return;const result=await aiWaitCheck(()=>aiCommand('publish',{operation:row.operation,revision:preview.revision.id,baseVersion:preview.revision.baseVersion,previewToken:preview.previewToken}),g);if(!result||g!==ai.generation)return;ai.dirty=false;notice('配置已发布');await loadAI();});
}
async function aiCommand(action,body){
  const slot=`ai-command:${state.user.id}:${action}:${body.operation}`;
  const identity=JSON.stringify({...body,previewToken:undefined});let previous;try{previous=JSON.parse(sessionStorage.getItem(slot));}catch{}
  const command=previous?.identity===identity?previous:{identity,key:uuid()};sessionStorage.setItem(slot,JSON.stringify(command));
  const result=await api(`/api/v1/ai/health/${action}`,{method:'POST',csrfRequired:true,idempotencyKey:command.key,body});
  if(!result.checking)sessionStorage.removeItem(slot);return result;
}
async function aiWaitCheck(request,generation){
  const started=Date.now();
  while(generation===ai.generation){
    const result=await request();if(generation!==ai.generation)return null;
    if(!result.checking)return result;
    if(Date.now()-started>8*60*1000)throw new Error('检查尚未完成，请稍后重试；线上模型未改变');
    const status=$('#ai-check-status');if(status)status.textContent=`正在用合成内容检查当前功能的模型参数… ${Math.round((Date.now()-started)/1000)} 秒。可返回取消等待。`;
    await new Promise(resolve=>setTimeout(resolve,3000));
  }
  return null;
}
async function openAIModels(id){
  aiNavigate('models');ai.endpoint=id;ai.query='';ai.page=0;ai.preview=null;renderAIModels();
  await aiWork(async g=>{
    const [data,catalog]=await Promise.all([api(`/api/v1/ai/endpoints/${encodeURIComponent(id)}`),ai.catalog?Promise.resolve(ai.catalog):api('/api/v1/ai/models')]);
    if(g!==ai.generation)return;ai.endpoints.set(id,data);ai.catalog=catalog;renderAIModels();
  });
}
function renderAIModels(){
  const data=ai.endpoints.get(ai.endpoint),models=ai.catalog?.models||[],filtered=models.filter(m=>`${m.Name} ${m.DisplayName} ${m.VendorName||''}`.toLowerCase().includes(ai.query.toLowerCase()));
  const pages=Math.max(1,Math.ceil(filtered.length/8));ai.page=Math.min(ai.page,pages-1);
  const ongoing=aiActive(data?.rolling)||(data?.commands||[]).some(c=>['queued','dispatching','watching','uncertain'].includes(c.state));
  aiFrame('选择模型',`当前：${aiModelText(aiModel(data?.endpoint))}`,`<p class="muted ai-small">使用功能：${aiEscape(aiAffected(ai.endpoint))}</p>${ongoing?'<div class="warning">此接入点有尚未完成的操作。可浏览模型，完成后再开始新的切换。 <button class="quiet" id="ai-open-progress">查看进度</button></div>':''}<input id="ai-model-search" type="search" aria-label="搜索全部火山模型" placeholder="搜索名称，例如 Pro、DeepSeek、GLM" value="${aiEscape(ai.query)}"><div class="ai-foot ai-catalog-meta"><span class="muted ai-small">全部模型 · ${filtered.length} 个</span><span class="muted ai-small">选中后检查账号、版本与兼容性</span></div><div class="ai-model-grid">${filtered.slice(ai.page*8,ai.page*8+8).map(m=>`<button class="ai-model-option" data-ai-model="${aiEscape(m.Name)}"><strong>${aiEscape(m.DisplayName||m.Name)}</strong><small>${aiEscape(m.Name)}</small><span>选择版本与检查 →</span></button>`).join('')||`<p class="muted">${ai.catalog?'没有匹配的模型':'正在读取火山完整目录…'}</p>`}</div><div class="ai-foot"><button class="quiet" id="ai-prev" ${ai.page===0?'disabled':''}>上一页</button><span class="muted ai-small">${ai.page+1} / ${pages}</span><button class="quiet" id="ai-next" ${ai.page+1>=pages?'disabled':''}>下一页</button></div><p class="muted ai-small">目录同步：${formatTime(ai.catalog?.syncedAt)}${ai.catalog?.stale?' · 当前为缓存数据，请刷新':''} <button class="quiet" id="ai-catalog-refresh">刷新目录</button></p>`,aiHome);
  $('#ai-model-search').oninput=e=>{ai.query=e.target.value;ai.page=0;renderAIModels();$('#ai-model-search').focus();};
  $('#ai-prev').onclick=()=>{ai.page--;renderAIModels();};$('#ai-next').onclick=()=>{ai.page++;renderAIModels();};
  $('#ai-catalog-refresh').onclick=()=>{ai.catalog=null;openAIModels(ai.endpoint);};
  if($('#ai-open-progress'))$('#ai-open-progress').onclick=()=>openAIProgress(ai.endpoint);
  aiClick('[data-ai-model]',b=>openAIVersions(b.dataset.aiModel));
}
function openAIVersions(name,version){
  aiNavigate('versions');ai.choice=ai.catalog?.models.find(m=>m.Name===name)||{Name:name};ai.versions=null;
  renderAIVersions();aiWork(async g=>{const result=await api(`/api/v1/ai/models/${encodeURIComponent(name)}/versions`);if(g!==ai.generation)return;ai.versions=result;renderAIVersions(version);});
}
function aiPriceDetails(activation){
  const groups=activation?.MultiChargeItems?.length?activation.MultiChargeItems:[{Name:'基础价格',ChargeItems:activation?.ChargeItems||[]}];
  return groups.map(group=>`<div class="ai-price-group"><small>${aiEscape(group.Description||group.Name||'价格分档')}</small>${(group.ChargeItems||[]).filter(c=>['InferencePrompt','InferenceCompletion','AudioPrompt'].includes(c.Type)).map(c=>`<p>${({InferencePrompt:'文本/图片输入',InferenceCompletion:'输出',AudioPrompt:'音频输入'})[c.Type]}：${aiEscape(c.Price)} 元 / ${aiEscape(c.UnitCode)}</p>`).join('')}</div>`).join('');
}
function renderAIVersions(preselected){
  const versions=ai.versions?.versions||[],activation=ai.versions?.activation?.value?.find(a=>a.FoundationModelName===ai.choice.Name),data=ai.endpoints.get(ai.endpoint);
  const pending=(data?.commands||[]).some(c=>['queued','dispatching','watching','uncertain'].includes(c.state))||aiActive(data?.rolling);
  aiFrame('检查切换条件',ai.choice.DisplayName||ai.choice.Name,`<p class="muted ai-small">选择模型 → 检查与确认 → 灰度进度</p><div class="ai-panel"><label>模型版本<select id="ai-version" ${versions.length?'':'disabled'}>${versions.map(v=>`<option value="${aiEscape(v.ModelVersion)}" ${v.ModelVersion===preselected?'selected':''}>${aiEscape(v.ModelVersion)} · ${v.Status==='Published'?'已发布':aiEscape(v.Status||'状态待核对')}</option>`).join('')}</select></label><div class="ai-field"><span>账号开通</span><span>${activation?.State==='Available'?'已开通':aiEscape(activation?.State||'开通状态待查询')}</span></div><div class="ai-field"><span>功能兼容性</span><span>按${aiEscape(aiAffected(ai.endpoint))}检查</span></div><div class="ai-field"><span>接入点原生升级</span><span>等待火山预检</span></div><details><summary>所选模型价格</summary>${aiPriceDetails(activation)}<p class="muted ai-small">价格同步：${formatTime(ai.versions?.activation?.syncedAt)}${ai.versions?.activation?.stale?' · 价格暂未更新，检查时重新查询':''}</p></details><p class="muted ai-small">检查使用合成内容验证协议与当前功能参数，会产生少量模型调用费用。不会修改线上模型。</p>${activation&&activation.State!=='Available'?'<p class="warning">此模型尚未开通。请在火山方舟开通后重新检查。</p>':''}<div class="ai-foot"><button class="quiet" id="ai-versions-refresh">重新读取版本</button><button id="ai-check" class="primary" data-ai-work ${!versions.length||pending||!data?.writesEnabled?'disabled':''}>检查并预览</button></div><p id="ai-check-status" role="status" class="muted">${ai.versions?(versions.length?'':'此模型没有可用版本'):'正在查询版本与价格…'}</p></div>`,()=>{aiNavigate('models');renderAIModels();});
  $('#ai-versions-refresh').onclick=()=>openAIVersions(ai.choice.Name);
  $('#ai-check').onclick=()=>aiWork(async g=>{const input={endpoint:ai.endpoint,action:'start',target:{Name:ai.choice.Name,ModelVersion:$('#ai-version').value}};const preview=await aiWaitCheck(()=>api('/api/v1/ai/rolling/preview',{method:'POST',csrfRequired:true,body:input}),g);if(preview&&g===ai.generation)renderAIRollingPreview(preview);});
}
function aiPriceText(price){return price?`输入 ${price.InputNanosPerMillionTokens/1e9} 元 / 百万 tokens；输出 ${price.OutputNanosPerMillionTokens/1e9} 元 / 百万 tokens`:'价格尚未同步';}
function renderAIRollingPreview(preview){
  aiNavigate('rolling-preview');ai.preview=preview;
  const {input,snapshot}=preview,isStart=input.action==='start';
  const before=aiModel(snapshot.endpoint),after=isStart?input.target:snapshot.rolling?.RollingOut;
  aiFrame(isStart?'确认模型切换':aiActions[input.action],aiAffected(input.endpoint),`<div class="ai-panel"><div class="ai-diff"><div><small class="muted">当前模型</small><strong>${aiEscape(aiModelText(before))}</strong></div><span>→</span><div><small class="muted">${isStart?'目标模型':'原模型'}</small><strong>${aiEscape(aiModelText(after||before))}</strong></div></div><div class="ai-field"><span>影响功能</span><span>${aiEscape(preview.affectedOperations.map(op=>aiLabels[op]||op).join('、')||'暂无当前绑定')}</span></div><div class="ai-field"><span>当前费用预留上限</span><span>${aiEscape(aiPriceText(snapshot.currentPrice))}</span></div>${snapshot.targetPrice?`<div class="ai-field"><span>目标费用预留上限</span><span>${aiEscape(aiPriceText(snapshot.targetPrice))}</span></div>`:""}<div class="ai-field"><span>灰度费用预留上限</span><span>${aiEscape(aiPriceText(snapshot.price))}</span></div><p class="muted ai-small">费用预留包含媒体输入，按账号价、原价及各档位中的最高值计算；这是保守上限。所选模型的实际分档单价可返回上一步查看。</p><div class="ai-field"><span>生效方式</span><span>火山原生灰度 · 自动推进</span></div><p class="muted">${aiEscape(preview.notice)}</p>${input.action==='step_back'?'<p class="warning">请求火山回退一个阶段，具体比例与后续状态以云端返回为准。</p>':''}${input.action==='cancel'?'<p class="warning">撤销整次灰度，让新模型流量归零。</p>':''}<button class="primary" id="ai-start" data-ai-work>通行密钥确认并${isStart?'开始':'提交'}</button><p id="ai-check-status" class="muted" role="status"></p></div>`,()=>isStart?openAIVersions(input.target.Name,input.target.ModelVersion):openAIProgress(input.endpoint));
  $('#ai-start').onclick=()=>aiWork(async g=>{
    await reauthenticate();if(g!==ai.generation)return;
    const slot=`ai-rolling:${state.user.id}:${input.endpoint}`,identity=JSON.stringify(input);let previous;try{previous=JSON.parse(sessionStorage.getItem(slot));}catch{}
    const command=previous?.identity===identity?previous:{identity,key:uuid()};sessionStorage.setItem(slot,JSON.stringify(command));
    const result=await aiWaitCheck(()=>api('/api/v1/ai/rolling/commands',{method:'POST',csrfRequired:true,idempotencyKey:command.key,body:{input,previewToken:preview.previewToken}}),g);
    if(!result||g!==ai.generation)return;sessionStorage.removeItem(slot);notice('操作已保存，后台将自动执行并同步状态');openAIProgress(input.endpoint);
  });
}
function openAIProgress(id){aiNavigate('progress');ai.endpoint=id;renderAIProgress();refreshAIProgress();}
async function refreshAIProgress(){
  const generation=ai.generation,id=ai.endpoint;
  try{const data=await api(`/api/v1/ai/endpoints/${encodeURIComponent(id)}`);if(generation!==ai.generation)return;ai.endpoints.set(id,data);renderAIProgress();}
  catch(error){if(generation===ai.generation)aiError(new Error('状态同步失败，当前显示上次成功结果。'+error.message));}
  finally{if(generation===ai.generation&&!document.hidden)ai.poll=setTimeout(refreshAIProgress,5000);}
}
function renderAIProgress(){
  const focused=document.activeElement;
  const focusID=focused?.id,focusAction=focused?.dataset?.aiNative;
  const detailsOpen=aiView().querySelector('details')?.open;
  const data=ai.endpoints.get(ai.endpoint),ep=data?.endpoint,r=data?.rolling,commands=data?.commands||[],pending=commands.some(c=>['queued','dispatching','uncertain'].includes(c.state)),gray=r?Number(r.RollingGray):null;
  const active=aiActive(r),current=aiModel(ep),completed=r?.RollingGray===100&&current?.Name===r.RollingIn?.Name&&current?.ModelVersion===r.RollingIn?.ModelVersion;
  const prior=commands.find(c=>c.input.action==='start'&&c.state==='succeeded'&&c.input.target.Name===current?.Name&&c.input.target.ModelVersion===current?.ModelVersion);
  const original=completed?r.RollingOut:prior?aiModel(prior.before.endpoint):null;
  const heading=active?'正在切换模型':completed?'模型切换已完成':r?.Status==='Reverted'?'灰度已撤销':pending?'操作等待执行':'模型与灰度';
  aiFrame(heading,aiAffected(ai.endpoint),`<div class="ai-panel">${r?`<div class="ai-diff"><div><small class="muted">原模型 · ${100-gray}%</small><strong>${aiEscape(aiModelText(r.RollingOut))}</strong></div><span>→</span><div><small class="muted">新模型 · ${gray}%</small><strong>${aiEscape(aiModelText(r.RollingIn))}</strong></div></div><div class="ai-meter" role="progressbar" aria-label="新模型流量" aria-valuemin="0" aria-valuemax="100" aria-valuenow="${gray}"><span style="width:${Math.min(100,Math.max(0,gray))}%"></span></div><p class="ai-progress">${gray}%</p><p class="muted">${active?'火山自动推进，无需手动推进阶段。':completed?'已核对接入点实际模型，切换完成。':'云端状态：'+aiEscape(r.Status)}</p>`:`<p>当前实际模型：${aiEscape(aiModelText(current))}</p><p class="muted">没有关联的原生灰度任务。</p>`}<p class="muted ai-small">最近同步：${formatTime(data?.syncedAt)} · 每 5 秒自动更新</p><div class="ai-actions">${active?`<button class="secondary" data-ai-native="step_back" ${pending||r.Status!=='Running'||gray<=0||!data.writesEnabled?'disabled':''}>回退一个阶段</button><button class="quiet" data-ai-native="cancel" ${pending||!['Running','Reverting'].includes(r.Status)||!data.writesEnabled?'disabled':''}>撤销本次灰度</button>`:original?`<button class="secondary" id="ai-switch-back" ${pending||!data.writesEnabled?'disabled':''}>切回原模型</button>`:''}<button class="quiet" id="ai-progress-refresh">立即刷新</button></div>${original&&!active?'<p class="muted ai-small">切回原模型会重新检查并开始新的灰度。</p>':''}${data?.priceState?.blocked?'<p class="warning">模型或费用信息需要核对，AI 请求已暂停。</p><button data-ai-native="accept_current" class="secondary">核对当前模型与费用</button>':''}${r&&commands.some(c=>c.state==='uncertain'&&c.input.action==='start'&&!c.rollingID)?'<button class="secondary" data-ai-native="reconcile">核对并关联云端任务</button>':''}${commands.length?`<div class="ai-command-status"><strong>${aiEscape(aiStates[commands[0].state]||commands[0].state)}</strong><p class="muted">${aiEscape(commands[0].detail||'')} · ${formatTime(commands[0].updatedAt)}</p></div>`:''}<details><summary>接入点与最近请求</summary><p class="ai-id">${aiEscape(ep?.Name||'')}<br>${aiEscape(ai.endpoint)}</p>${(data?.attempts||[]).map(a=>`<p class="ai-small">${aiEscape(a.actualModel||'火山未返回模型名')} · ${formatTime(a.createdAt)} · ${aiEscape(aiLabels[a.operation]||a.operation)}</p>`).join('')}</details></div>`,aiHome);
  if(detailsOpen)aiView().querySelector('details').open=true;
  if(focusID)document.getElementById(focusID)?.focus({preventScroll:true});else if(focusAction)aiView().querySelector(`[data-ai-native="${focusAction}"]`)?.focus({preventScroll:true});
  $('#ai-progress-refresh').onclick=()=>{clearTimeout(ai.poll);refreshAIProgress();};
  if($('#ai-switch-back'))$('#ai-switch-back').onclick=()=>openAIVersions(original.Name,original.ModelVersion);
  aiClick('[data-ai-native]',b=>{clearTimeout(ai.poll);aiWork(async g=>{const preview=await aiWaitCheck(()=>api('/api/v1/ai/rolling/preview',{method:'POST',csrfRequired:true,body:{endpoint:ai.endpoint,action:b.dataset.aiNative,target:{Name:'',ModelVersion:''}}}),g);if(preview&&g===ai.generation)renderAIRollingPreview(preview);});});
}
window.addEventListener('beforeunload',event=>{if(ai.dirty){event.preventDefault();event.returnValue='';}});
document.addEventListener('visibilitychange',()=>{if(document.hidden)clearTimeout(ai.poll);else if(ai.screen==='progress'&&!$('#ai-section').classList.contains('hidden'))refreshAIProgress();});
