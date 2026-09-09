// Offer inventory and named delivery keep input nodes stable during asynchronous reads.
let delivery = null;
let deliveryGeneration = 0;
const deliveryStatus = {requested:'待处理',assigned:'已分配，待发放',delivered:'已发放',cancelled:'已取消',rejected:'已拒绝'};
const deliveryActions = {'request.claim':'领取人通过专属链接领码','request.share_claim':'生成或复制专属领取链接','request.revoke_claim':'撤销专属领取链接','inventory.import_fresh':'新生成的兑换码自动接入','inventory.import':'导入完整码池','inventory.confirm_available':'确认未发放库存','inventory.export':'导出 CSV，移出可分配库存','request.record_external':'补录历史发放','request.personal_create':'为朋友分配一次性码','request.create':'登记申请','request.assign':'分配兑换码','code.reveal':'查看兑换码','request.deliver':'记录实际发放','request.cancel':'取消申请','request.reject':'拒绝申请','request.report_redeemed':'记录领取人反馈','request.link_verified':'人工关联已验证订阅'};
const deliveryPath = d => `/api/v1/apps/${encodeURIComponent(d.app)}/offers/${encodeURIComponent(d.offer.id)}/code-pools/${encodeURIComponent(d.poolID)}/delivery`;
function deliveryMessage(message,error=false){const node=$('#delivery-message');if(node){node.textContent=message;node.className=`notice${error?' error':''}`;}}
function closeOfferDelivery(){deliveryGeneration++;delivery=null;$('#delivery-view').replaceChildren();$('#delivery-view').classList.add('hidden');$('#offer-overview').classList.remove('hidden');document.querySelector('#delivery-secret-dialog')?.remove();}
function deliveryHeader(offer,subtitle){return `<button class="quiet ai-back" id="delivery-back">← 返回</button><div class="section-head"><div><p class="eyebrow">Offer 发放管理</p><h2 tabindex="-1">${escapeHTML(offer.name)}</h2><p class="muted">${escapeHTML(subtitle)}</p></div></div><p id="delivery-message" class="notice hidden" role="status" aria-live="polite"></p>`;}
function mountDelivery(html){$('#offer-overview').classList.add('hidden');const node=$('#delivery-view');node.classList.remove('hidden');node.innerHTML=html;node.querySelector('h2')?.focus();}
async function deliveryRun(task){
 const d=delivery;if(!d||d.busy)return;d.busy=true;
 const controls=new Map([...$('#delivery-view').querySelectorAll('button')].map(b=>[b,b.disabled]));controls.forEach((_,b)=>b.disabled=true);
 try{await task();}catch(e){if(delivery===d)deliveryMessage(e.message,true);}
 finally{d.busy=false;controls.forEach((disabled,b)=>{if(b.isConnected)b.disabled=disabled;});if(delivery===d&&d.data){$("#delivery-first").disabled=!d.cursor;$("#delivery-next").disabled=!d.data.nextCursor;const p=d.data.pool;$("#delivery-request-form button").disabled=!p.active||new Date(p.expiresAt)<=new Date();}}
}
async function deliveryCommand(body){
 const d=delivery;if(!d?.poolID)throw new Error('请重新选择码池');
 const invoke=()=>api(deliveryPath(d),{method:'POST',body,csrfRequired:true});
 try{return await invoke();}catch(e){if(e.code!=='reauthentication_required')throw e;await reauthenticate();if(delivery!==d)throw new Error('页面已切换，请重新操作');return await invoke();}
}
async function openOfferInventory(id,personal=false){
 const offer=state.offerData?.offers?.find(o=>o.id===id);if(!offer)return;
 const d={app:state.currentApp,offer,poolID:null,personalInventory:personal};delivery=d;const generation=++deliveryGeneration;
 mountDelivery(deliveryHeader(offer,'选择码池，查看库存并管理每一位领取人。')+'<div class="actions"><button id="delivery-create-pool" class="secondary">新建码池</button></div><div id="delivery-pools" class="stack"><p class="muted">正在读取 Apple 码池…</p></div>');
 $('#delivery-back').onclick=personal?()=>openPersonalDelivery(id):closeOfferDelivery;
 $('#delivery-create-pool').disabled=!offer.active||!state.offerData.writesEnabled;
 $('#delivery-create-pool').textContent=personal?'新建一次性码批次':'新建码池';
 $('#delivery-create-pool').onclick=async()=>{await openCodes(id);if(personal){const form=$('#codes-form');form.kind.value='oneTime';form.environment.value='PRODUCTION';toggleCodeKind();}};
 try{
  const data=await api(`/api/v1/apps/${encodeURIComponent(d.app)}/offers/${encodeURIComponent(id)}/code-pools`);
  if(delivery!==d||generation!==deliveryGeneration)return;
  const pools=(data.codePools||[]).filter(p=>!personal||(p.kind==='oneTime'&&p.environment==='PRODUCTION'));
  $('#delivery-pools').innerHTML=pools.length?pools.map(p=>`<article class="card delivery-pool"><div><h3>${p.kind==='custom'?'自定义共享码':'Apple 一次性码'}</h3><p>${p.environment==='SANDBOX'?'沙盒测试':'正式环境'} · 生成 ${Number(p.numberOfCodes).toLocaleString()} 次兑换额度</p><p class="muted">${p.active&&offer.active?'启用中':'已停用'} · ${p.expirationDate?'到期 '+escapeHTML(p.expirationDate):'未设置到期日'}</p><small class="muted">码池 ${escapeHTML(p.id)}</small></div><button class="primary" data-delivery-pool="${escapeHTML(p.id)}">管理发放</button></article>`).join(''):'<p class="card muted">还没有码池。创建码池后即可登记领取人。</p>';
  $$('[data-delivery-pool]').forEach(b=>b.onclick=()=>openDeliveryPool(d,b.dataset.deliveryPool));
 }catch(e){if(delivery===d){$('#delivery-pools').textContent='未能读取码池';deliveryMessage(e.message,true);}}
}
async function openDeliveryPool(parent,poolID){
 const d={app:parent.app,offer:parent.offer,poolID,cursor:'',query:'',status:'',requestKey:uuid(),externalKey:uuid(),data:null};delivery=d;deliveryGeneration++;
 mountDelivery(deliveryHeader(d.offer,'库存与领取台账')+`
 <div class="delivery-toolbar"><button id="delivery-refresh" class="secondary">同步 Apple 库存</button><span id="delivery-pool-meta" class="muted">正在读取…</span></div>
 <div id="delivery-summary" class="delivery-metrics"></div><section id="delivery-apple-report" class="card"></section>
 <details class="card delivery-inventory"><summary>库存管理与统计口径</summary><div id="delivery-inventory"></div></details>
 <details class="card" id="delivery-new"><summary>登记领取申请</summary><form id="delivery-request-form" class="delivery-form">
 <label>姓名或昵称<input name="name" required maxlength="80" autocomplete="off" placeholder="用于识别领取人"></label>
 <label>联系方式<input name="contact" maxlength="160" autocomplete="off" placeholder="邮箱、手机号或其他联系方式"></label>
 <label>内部编号<input name="reference" maxlength="80" autocomplete="off" placeholder="可选，例如测试者编号"></label>
 <label>来源渠道<input name="channel" maxlength="80" placeholder="例如朋友邀请、内测活动"></label>
 <label class="delivery-wide">备注<textarea name="note" maxlength="1000" rows="3"></textarea></label>
 <div class="actions delivery-wide"><button class="primary">保存申请</button></div></form></details>
 <div class="section-head subhead"><h3>领取台账</h3><span id="delivery-ledger-count" class="muted"></span></div>
 <form id="delivery-search" class="delivery-search"><label>搜索领取人<input name="query" type="search" maxlength="160" placeholder="姓名、联系方式、编号、渠道或备注"></label><label>处理状态<select name="status"><option value="">全部状态</option>${Object.entries(deliveryStatus).map(([k,v])=>`<option value="${k}">${v}</option>`).join('')}</select></label><button class="secondary">搜索</button></form>
 <div id="delivery-requests" class="stack"></div><div class="actions"><button id="delivery-first" class="quiet">回到第一页</button><button id="delivery-next" class="secondary">下一页</button></div>
 <details class="card subhead"><summary>最近操作记录</summary><div id="delivery-events" class="stack"></div></details>`);
 const historical=document.createElement('details');historical.className='card';historical.id='delivery-external';historical.innerHTML='<summary>补录已经发出的兑换码</summary><p class="muted">用于微信、邮件等渠道已发生的发放。补录不会新增领取申请，也不会分配新码。</p>';
 const historicalForm=$('#delivery-request-form').cloneNode(true);historicalForm.id='delivery-external-form';historicalForm.querySelector('button').textContent='保存历史发放';
 historicalForm.insertAdjacentHTML('afterbegin','<label id="delivery-external-code">已发出的一次性码<input name="code" maxlength="64" autocomplete="off" placeholder="先导入完整码池，再填写已发出的码"></label><label>实际发放时间<input name="deliveredAt" type="datetime-local" required></label>');
 historicalForm.querySelector('[name=deliveredAt]').value=new Date(Date.now()-new Date().getTimezoneOffset()*60000).toISOString().slice(0,16);
 historical.append(historicalForm);$('#delivery-new').after(historical);
 historicalForm.onsubmit=e=>{e.preventDefault();const values=Object.fromEntries(new FormData(historicalForm)),{code,deliveredAt,...recipient}=values;deliveryRun(async()=>{await deliveryCommand({action:'record_external',recipient,code,deliveredAt:new Date(deliveredAt).toISOString(),requestKey:d.externalKey});d.externalKey=uuid();historicalForm.reset();historical.open=false;d.cursor='';await refreshDelivery();deliveryMessage('历史发放已补录，码值不会再次进入可分配库存');});};
 $('#delivery-back').onclick=()=>parent.kind==='personal'?openPersonalDelivery(d.offer.id):openOfferInventory(d.offer.id,!!parent.personalInventory);
 $('#delivery-refresh').onclick=()=>deliveryRun(async()=>{await deliveryCommand({action:'sync'});await refreshDelivery();deliveryMessage('Apple 库存已同步');});
 $('#delivery-request-form').onsubmit=e=>{e.preventDefault();const form=e.currentTarget;const recipient=Object.fromEntries(new FormData(form));deliveryRun(async()=>{await deliveryCommand({action:'request',recipient,requestKey:d.requestKey});d.requestKey=uuid();form.reset();$('#delivery-new').open=false;d.cursor='';await refreshDelivery();deliveryMessage('申请已登记，尚未分配兑换码');});};
 $('#delivery-search').onsubmit=e=>{e.preventDefault();const f=e.currentTarget;d.query=f.query.value;d.status=f.status.value;d.cursor='';deliveryRun(refreshDelivery);};
 $('#delivery-first').onclick=()=>deliveryRun(async()=>{d.cursor='';await refreshDelivery();});
 $('#delivery-next').onclick=()=>deliveryRun(async()=>{d.cursor=d.data.nextCursor;await refreshDelivery();});
 await deliveryRun(async()=>{
  try{await deliveryCommand({action:'sync'});}catch(e){deliveryMessage('Apple 同步失败，尝试读取已有台账：'+e.message,true);}
  await refreshDelivery();
 });
}
async function refreshDelivery(){
 const d=delivery;if(!d?.poolID)return;const gen=++deliveryGeneration;
 const data=await api(deliveryPath(d),{method:'POST',csrfRequired:true,body:{action:'search',query:d.query,status:d.status,cursor:d.cursor}});
 if(delivery!==d||gen!==deliveryGeneration)return;d.data=data;
 renderDeliverySummary(data);renderDeliveryRequests(data);
 $('#delivery-ledger-count').textContent=`${data.summary.applications} 条申请 · ${data.summary.externalDeliveries} 条历史发放`;
 $('#delivery-first').disabled=!d.cursor;$('#delivery-next').disabled=!data.nextCursor;
 $('#delivery-events').innerHTML=(data.events||[]).map(e=>`<div class="delivery-event"><strong>${escapeHTML(deliveryActions[e.action]||e.action)}</strong><small>${formatTime(e.createdAt)} · ${e.quantity} 条 · 操作者 ${escapeHTML(e.actorID)}</small></div>`).join('')||'<p class="muted">暂无操作记录</p>';
}
function renderDeliverySummary(data){
 const p=data.pool,s=data.summary;const usable=p.active&&new Date(p.expiresAt)>new Date();
 $('#delivery-pool-meta').textContent=`${p.environment==='sandbox'?'沙盒测试':'正式环境'} · ${p.kind==='custom'?'自定义共享码':'一次性码'} · ${usable?'可发放':'已停用或过期'} · 同步于 ${formatTime(p.syncedAt)}${data.stale?'（已过期，请刷新）':''}`;
 const metrics=[['生成兑换额度',p.capacity],['领取申请',s.applications],['已发放',s.delivered],['Offer 已验证核销',data.observedOfferSubscriptions]];
 renderDeliveryReport(data);
 $('#delivery-summary').innerHTML=metrics.map(([name,value])=>`<article class="card metric"><span>${name}</span><strong>${value==null?'—':Number(value).toLocaleString()}</strong></article>`).join('')+`<p class="delivery-wide muted">通过链接已领取 ${s.claimed||0} · 待处理 ${s.pending} · 已分配 ${s.assignedRequests} · 反馈已兑换 ${s.reportedRedeemed} · 人工关联已验证订阅 ${s.linkedVerified}<br><small>Offer 核销覆盖同产品、同环境下的全部码池，来自后端收到并验证的 Apple 交易，可能不含全部历史。</small>${data.unknownProductSubscriptions?`<br><small>同参考名称另有 ${Number(data.unknownProductSubscriptions)} 个历史核销未记录产品，不能归入此码池或关联到领取人。</small>`:''}</p>`;
 $('#delivery-request-form').querySelector('button').disabled=!usable;
 $('#delivery-external-code').classList.toggle('hidden',p.kind==='custom');$('#delivery-external-code input').required=p.kind==='oneTime';
 $('#delivery-inventory').innerHTML=`<p class="muted">生成额度、申请、发放和核销是不同阶段，不能相减得到 Apple 的实际剩余次数。领取人反馈不等于 Apple 已验证核销。一次性码无法从 Apple 报表直接确定由谁兑换。</p>`+(p.kind==='oneTime'?`
 <p>已导入 ${s.imported} 枚 · 确认可分配 ${s.available} 枚 · 外部状态待确认 ${s.external} 枚 · 已分配 ${s.assignedCodes} 枚</p>
 <div class="delivery-toolbar"><button id="delivery-import" class="secondary">从 Apple 导入完整码池</button><button id="delivery-export" class="quiet">导出完整 CSV</button></div>
 <details><summary>确认未发放库存</summary><p class="warning">仅在已核对外部发放记录，确认这 ${s.external} 枚待确认码全部没有发出时操作。已经分配的码不会重新入库。如果只确认其中一部分，请先补录其他码的发放记录。</p><form id="delivery-confirm"><label>输入确认未发出的数量<input name="count" type="number" min="1" max="${s.external}" required></label><button class="secondary" ${!usable||!s.external?'disabled':''}>确认加入可分配库存</button></form></details>`:`<p>该码可由多位领取人使用。平台已分配 ${s.assignedRequests} 人；平台外传播和 Apple 的实际核销次数可能不同。</p>`);
 if(p.kind==='oneTime'){
  $('#delivery-import').onclick=()=>deliveryRun(async()=>{await deliveryCommand({action:'import'});await refreshDelivery();deliveryMessage('码池已导入；未确认的历史码不会自动加入可分配库存');});
  $('#delivery-confirm').onsubmit=e=>{e.preventDefault();const count=Number(e.currentTarget.count.value);deliveryRun(async()=>{await deliveryCommand({action:'confirm_inventory',expectedCount:count});await refreshDelivery();deliveryMessage('未发放库存已确认');});};
  $('#delivery-export').onclick=()=>{if(!confirm('导出完整 CSV 会包含已分配的码，并将当前可分配码移出平台库存。请自行保管，勿重复发放。继续导出？'))return;deliveryRun(async()=>{await downloadBatch(delivery.poolID);await refreshDelivery();deliveryMessage('已导出，库存状态已更新');});};
 }
}
function renderDeliveryRequests(data){
 const p=data.pool,usable=p.active&&new Date(p.expiresAt)>new Date();
 $('#delivery-requests').innerHTML=data.requests.length?data.requests.map(r=>{
  const v=r.recipient;let actions='';
  const button=(action,label,disabled=false)=>`<button class="${action==='assign'?'primary':'secondary'}" data-delivery-action="${action}" data-request="${escapeHTML(r.id)}" ${disabled?'disabled':''}>${label}</button>`;
  if(r.status==='requested')actions=button('assign','分配兑换码',!usable)+button('reject','拒绝')+button('cancel','取消');
  if(r.status==='assigned')actions=button('share_claim','专属领取链接')+button('reveal','查看兑换码')+button('deliver','记录已发放')+button('cancel','取消');
  if(r.status==='delivered')actions=button('share_claim','专属领取链接')+button('reveal','查看兑换码')+(!r.reportedRedeemedAt?button('report_redeemed','记录已兑换反馈'):'')+(!r.verifiedAt?button('link_verified','关联核销证据'):'');
  if(r.claimExpiresAt)actions+=button('revoke_claim','撤销领取链接');
  return `<article class="card delivery-recipient"><div class="section-head"><h3>${escapeHTML(v.name)}</h3><span class="chip">${deliveryStatus[r.status]||escapeHTML(r.status)}</span></div><p>${escapeHTML(v.contact||'未填联系方式')}${v.reference?' · '+escapeHTML(v.reference):''}</p><p class="muted">${escapeHTML(v.channel||'未填渠道')} · ${r.source==='external'?'历史发放补录于':r.source==='personal'?'专属码准备于':'申请于'} ${formatTime(r.requestedAt)}</p>${v.note?`<p class="delivery-note">${escapeHTML(v.note)}</p>`:''}<div class="delivery-stages">${r.assignedAt?`<small>分配：${formatTime(r.assignedAt)}</small>`:''}${r.deliveredAt?`<small>发放：${formatTime(r.deliveredAt)}</small>`:''}${r.claimedAt?`<small>通过链接领取：${formatTime(r.claimedAt)}</small>`:''}${r.reportedRedeemedAt?'<small>领取人反馈已兑换（未据此核销）</small>':''}${r.verifiedAt?'<small>管理员已关联 Apple 验证订阅</small>':''}</div><div class="offer-actions">${actions}</div></article>`;
 }).join(''):`<p class="card muted">${data.nextCursor?'本页未找到匹配记录，可继续下一页检索。':'没有匹配的领取记录。'}</p>`;
 $$('[data-delivery-action]').forEach(b=>b.onclick=()=>{
  const r=data.requests.find(r=>r.id===b.dataset.request),action=b.dataset.deliveryAction;
  if(action==='link_verified'){showDeliveryLink(r);return;}
  const messages={revoke_claim:'撤销后，旧链接不能再查看兑换码。已经复制出去的码无法通过此操作收回。确认撤销？',cancel:'取消后，已经分配的码不会重新进入库存。确认取消？',deliver:'确认已经把兑换码发给这位领取人？此操作只记录发放，不表示已兑换。',report_redeemed:'确认已收到这位领取人的兑换反馈？反馈会单独记录，不作为 Apple 核销证据。',reject:'确认拒绝这条领取申请？'};
  if(messages[action]&&!confirm(messages[action]))return;
  deliveryRun(async()=>{const result=await deliveryCommand({action,requestID:r.id,version:r.version});if(action==='reveal'){showDeliverySecret(r,result.code);return;}if(action==='share_claim'){await refreshDelivery();showDeliveryClaimLink(result.token);return;}await refreshDelivery();deliveryMessage('记录已更新');});
 });
}
function deliveryDialog(title,body){
 document.querySelector('#delivery-secret-dialog')?.remove();const dialog=document.createElement('dialog');dialog.id='delivery-secret-dialog';dialog.innerHTML=`<div class="dialog-head"><h2>${escapeHTML(title)}</h2><button class="icon" aria-label="关闭">×</button></div>${body}`;document.body.append(dialog);dialog.querySelector('.icon').onclick=()=>{dialog.close();dialog.remove();};dialog.onclose=()=>dialog.remove();dialog.showModal();return dialog;
}
function showDeliverySecret(r,code){
 const dialog=deliveryDialog('发给 '+r.recipient.name,'<p class="muted">查看或复制不会自动记为发放。实际发出后，请回到台账记录“已发放”。</p><label>兑换码<input readonly autocomplete="off" id="delivery-secret"></label><p id="delivery-copy-status" role="status"></p><button class="primary" id="delivery-copy">复制兑换码</button>');
 dialog.dataset.clearOnHide='true';dialog.querySelector('input').value=code;
 dialog.querySelector('#delivery-copy').onclick=async()=>{try{await navigator.clipboard.writeText(code);dialog.querySelector('#delivery-copy-status').textContent='已复制';}catch{dialog.querySelector('input').select();dialog.querySelector('#delivery-copy-status').textContent='请长按或手动复制';}};
}
function showDeliveryLink(r){
 const data=delivery.data,records=data.verifiedSubscriptions||[];
 const dialog=deliveryDialog('关联已验证订阅',`<p class="warning">这是管理员根据外部核对结果建立的关联。Apple 不提供领取人姓名或具体一次性码，不能仅凭日期或数量猜测归属。</p><form id="delivery-link-form"><label>已核对的订阅凭据<select name="reference" required><option value="">请选择明确核对过的记录</option>${records.map(v=>`<option value="${escapeHTML(v.reference)}" ${v.linkedRequestID?'disabled':''}>${escapeHTML(v.reference.slice(0,12))}… · ${formatTime(v.firstObservedAt)}${v.linkedRequestID?'（已关联）':''}</option>`).join('')}</select></label>${data.moreVerified?'<p class="muted">仅列出最近 100 个订阅。</p>':''}<label class="check"><input type="checkbox" required>我有独立依据确认这条订阅属于该领取人</label><button class="primary">验证并关联</button><p id="delivery-link-error" class="muted" role="alert"></p></form>`);
 const d=delivery;dialog.querySelector('form').onsubmit=async e=>{e.preventDefault();const form=e.currentTarget,button=form.querySelector('button');button.disabled=true;try{if(delivery!==d)return;await deliveryCommand({action:'link_verified',requestID:r.id,version:r.version,reference:form.reference.value});dialog.close();await refreshDelivery();deliveryMessage('已记录管理员关联；领取人身份并非由 Apple 验证');}catch(e){dialog.querySelector('#delivery-link-error').textContent=e.message;}finally{button.disabled=false;}};
}
document.addEventListener('visibilitychange',()=>{if(document.hidden)document.querySelector('#delivery-secret-dialog[data-clear-on-hide]')?.remove();});
function showDeliveryClaimLink(token){
 const dialog=deliveryDialog('发送专属领取链接','<p class="muted">对方点开即可领取，无需填写姓名或邮箱。请只发给这条记录对应的人；链接被转发后无法确认领取者身份。</p><label>领取链接<input id="delivery-claim-link" readonly></label><button id="delivery-copy-claim-link" class="primary">复制链接</button><p id="delivery-link-copy-message" role="status"></p>');
 dialog.dataset.clearOnHide='true';
 const link=location.origin+'/offer-claim#receipt='+encodeURIComponent(token);dialog.querySelector('input').value=link;dialog.querySelector('#delivery-copy-claim-link').onclick=async()=>{try{await navigator.clipboard.writeText(link);dialog.querySelector('#delivery-link-copy-message').textContent='已复制';}catch{dialog.querySelector('input').select();dialog.querySelector('#delivery-link-copy-message').textContent='请手动复制完整链接';}};
}
function renderDeliveryReport(data){
 const r=data.appleReport, sync=data.reportSync||{};
 if(!r){$('#delivery-apple-report').textContent='Apple 核销报表暂不可用';return;}
 const scope=r.scope==='custom_code'?'这枚自定义码':r.scope==='one_time_offer'?'此 Offer 的所有一次性码':'当前码池';
 const state={not_configured:'尚未配置 Apple 报表供应商编号',pending:'等待首次同步',forbidden:'Apple 报表权限不足，请检查密钥的销售与趋势权限',failed:'最近同步未完成，保留上次成功的报表',ready:'定期同步已启用'}[sync.state]||'等待同步';
 $('#delivery-apple-report').innerHTML=`<h3>Apple 报表确认的兑换</h3><p>${r.scope==='unavailable'?'此环境或订阅产品暂不支持报表对账':!r.days?'待确认：尚未取得可用日报':r.redemptions>0?`${scope}已兑换 <strong>${Number(r.redemptions)}</strong> 次`:`已取得的日报中尚未发现${scope}兑换`}</p>${r.days?`<p class="muted">${escapeHTML(r.firstDay)} 至 ${escapeHTML(r.lastDay)}，已取得 ${Number(r.days)} 天日报；缺失日期不算零。更新于 ${formatTime(r.fetchedAt)}</p>`:''}<p class="muted">${state}。报表有延迟，领取和本人反馈不作为核销证据。${r.scope==='custom_code'?'统计覆盖同产品、同 Offer 下使用相同码值的批次；转发后不能确认兑换者本人。':'一次性码报表不能确定具体码或领取人。'}</p>`;
}



const personalDrafts=new Map();
const personalPath=d=>`/api/v1/apps/${encodeURIComponent(d.app)}/offers/${encodeURIComponent(d.offer.id)}/personal-deliveries`;
async function personalAPI(d,path,options){
 try{return await api(path,options);}catch(e){
  if(e.code!=='reauthentication_required')throw e;
  await reauthenticate();if(delivery!==d)throw new Error('页面已切换，请重新操作');
  return await api(path,options);
 }
}
function personalSubmitState(d){
 if(delivery!==d)return;
 const form=$('#personal-form'),button=form.querySelector('[type=submit]');
 const confirm=$('#personal-confirm-unissued');
 button.disabled=d.busy||(!d.pending&&(!d.canSend||(d.needsConfirmation&&!confirm?.checked)));
 button.textContent=d.busy?'正在准备领取链接…':d.pending?'重试并获取领取链接':d.createNew?'准备兑换码并生成链接':'生成领取链接';
 form.elements.name.readOnly=d.busy||!!d.pending;
 $('#personal-refresh').disabled=d.busy;
 $('#personal-new-batch').disabled=d.busy||!!d.pending;
}
async function openPersonalDelivery(offerID){
 const offer=state.offerData?.offers?.find(v=>v.id===offerID);if(!offer)return;
 const draftKey=JSON.stringify([state.currentApp,offerID]);
 const d=personalDrafts.get(draftKey)||{app:state.currentApp,offer,kind:'personal',cursor:'',requestKey:uuid(),busy:false,pending:null,forceNew:false,draftKey};delivery=d;deliveryGeneration++;
 mountDelivery(deliveryHeader(offer,'填一个称呼，把领取链接发给朋友。对方不用填写个人信息。')+`
 <form id="personal-form" class="card personal-send"><label>朋友的称呼<input name="name" required maxlength="80" autocomplete="off" placeholder="例如小王，仅你可见"></label>
 <div id="personal-setup"><p class="muted">正在检查可用兑换码…</p></div>
 <button type="submit" class="primary" disabled>生成领取链接</button></form>
 <div class="actions"><button id="personal-new-batch" class="quiet hidden">使用一批新的兑换码</button></div>
 <p class="muted"><small>领取后会留下记录。兑换反馈单独显示，Apple 不提供单枚一次性码的兑换状态。</small></p>
 <div class="section-head subhead"><h3>发给朋友的记录</h3><button id="personal-refresh" class="secondary">刷新</button></div><div id="personal-list" class="stack"></div><div class="actions"><button id="personal-first" class="quiet">回到第一页</button><button id="personal-next" class="secondary" disabled>下一页</button></div>`);
 $('#delivery-back').onclick=()=>{if(!d.busy)closeOfferDelivery();};
 $('#personal-new-batch').onclick=()=>{d.forceNew=!d.forceNew;renderPersonalSetup(d);};
 $('#personal-refresh').onclick=()=>refreshPersonal(d).catch(e=>deliveryMessage(e.message,true));
 $('#personal-first').onclick=()=>{d.cursor='';refreshPersonal(d).catch(e=>deliveryMessage(e.message,true));};
 $('#personal-next').onclick=()=>{d.cursor=d.nextCursor;refreshPersonal(d).catch(e=>deliveryMessage(e.message,true));};
 const form=$('#personal-form');form.elements.name.value=d.pending?.name||'';
 form.onsubmit=async e=>{
  e.preventDefault();if(d.busy||(!d.pending&&!d.canSend))return;
  if(!d.pending){
   if(d.needsConfirmation&&!$('#personal-confirm-unissued')?.checked)return;
   d.pending={action:'create',requestKey:d.requestKey,name:form.elements.name.value.trim(),poolID:d.selectedPool?.id,confirmUnissued:!!d.needsConfirmation};
   if(d.createNew)d.batchPending={key:uuid(),body:{numberOfCodes:500,environment:'PRODUCTION',expirationDate:new Date(Date.now()+86400000*30).toISOString().slice(0,10)}};
  }
  personalDrafts.set(d.draftKey,d);d.busy=true;personalSubmitState(d);
  try{
   if(delivery!==d)return;
   if(d.batchPending){
    const result=await personalAPI(d,`/api/v1/apps/${encodeURIComponent(d.app)}/offers/${encodeURIComponent(d.offer.id)}/one-time-code-batches`,{method:'POST',csrfRequired:true,idempotencyKey:d.batchPending.key,body:d.batchPending.body});
    if(!result.codePool?.id)throw new Error('暂时无法确认新兑换码，请重试同一次操作');
    d.pending.poolID=result.codePool.id;
    // These codes were just generated for this flow and have not been distributed.
    d.pending.confirmUnissued=!result.deliveryReady;
    d.createdPool=result.codePool;d.batchPending=null;
   }
   const result=await personalAPI(d,personalPath(d),{method:'POST',csrfRequired:true,body:d.pending});
   if(delivery!==d)return;
   personalDrafts.delete(d.draftKey);d.pending=null;d.createdPool=null;d.forceNew=false;d.requestKey=uuid();form.elements.name.value='';d.cursor='';
   // A failed list refresh must not hide a successfully generated claim link.
   showPersonalLink(result);await refreshPersonal(d).catch(()=>deliveryMessage('领取链接已生成，记录暂未刷新，可稍后点刷新查看。'));
  }catch(e){if(delivery===d){
   if(!d.batchPending&&!d.createdPool&&['inventory_unavailable','invalid_delivery','delivery_not_found','reauthentication_required','forbidden'].includes(e.code))d.pending=null;
   deliveryMessage(d.pending?`${e.message}。点击重试会继续同一次操作，不会重复生成或分配。`:e.message,true);
   await refreshPersonal(d).catch(()=>{});
  }}finally{d.busy=false;if(delivery===d){personalSubmitState(d);$('#delivery-back').disabled=false;if(!d.pending)personalDrafts.delete(d.draftKey);}}
 };
 await refreshPersonal(d).catch(e=>{if(delivery===d){$('#personal-setup').textContent='暂时无法读取兑换码，请点刷新重试。';deliveryMessage(e.message,true);}});
}
function renderPersonalSetup(d){
 const data=d.listData;if(!data||delivery!==d)return;
 const pools=(data.pools||[]).filter(p=>p.active).sort((a,b)=>a.expiration.localeCompare(b.expiration)||a.id.localeCompare(b.id));
 const ready=pools.filter(p=>p.available>0),unconfirmed=pools.filter(p=>p.unconfirmed>0||!p.managed);
 const candidates=ready.length?ready:unconfirmed;
 d.selectedPool=candidates.find(p=>p.id===d.selectedPool?.id)||candidates[0];
 const writes=!!state.offerData?.writesEnabled&&d.offer.active;
 d.createNew=!d.pending&&(d.forceNew||!candidates.length)&&writes&&!data.inventoryError;
 d.needsConfirmation=!d.createNew&&!ready.length&&unconfirmed.length>0;
 d.canSend=!data.inventoryError&&(!!d.selectedPool||d.createNew);
 const node=$('#personal-setup');
 if(d.pending){node.innerHTML='<p class="muted">正在核对这一次操作。重试会使用同一个记录。</p>';}
 else if(data.inventoryError){node.textContent='暂时无法从 Apple 读取兑换码，请点刷新重试。已有发放记录仍可查看。';}
 else if(d.createNew){node.innerHTML='<p>后台会准备 500 枚一次性码，这次只给朋友分配 1 枚，其余留待以后使用。</p><p class="muted">Apple 要求每批至少 500 枚，有效期设为 30 天。准备完成后会直接显示领取链接。</p>';}
 else if(ready.length){node.innerHTML=`<p class="muted">已有可用兑换码，后台会自动选一枚。有效期至 ${escapeHTML(d.selectedPool.expiration)}。</p>`;}
 else if(unconfirmed.length){
  node.innerHTML=`<p>找到已有的兑换码，无需重新创建。</p>${unconfirmed.length>1?`<details><summary>换一批兑换码（可选）</summary><label>已有兑换码<select id="personal-existing">${unconfirmed.map((p,i)=>`<option value="${escapeHTML(p.id)}">第 ${i+1} 批 · ${Number(p.capacity)} 枚 · ${escapeHTML(p.expiration)} 到期</option>`).join('')}</select></label></details>`:`<p class="muted">${Number(d.selectedPool.capacity)} 枚 · ${escapeHTML(d.selectedPool.expiration)} 到期</p>`}<label class="personal-confirm"><input id="personal-confirm-unissued" type="checkbox">尚未分配的这些码，没有从其他地方发给别人</label><p class="muted"><small>确认一次，后台会接入并为这位朋友分配一枚。已经分配的码不会再次发出。</small></p>`;
  const select=$('#personal-existing');if(select){select.value=d.selectedPool.id;select.onchange=()=>{d.selectedPool=unconfirmed.find(p=>p.id===select.value);$('#personal-confirm-unissued').checked=false;personalSubmitState(d);};}
  $('#personal-confirm-unissued').onchange=()=>personalSubmitState(d);
 }else{node.textContent=d.offer.active?'目前没有可用兑换码，当前后台未开启在 Apple 创建兑换码的权限。':'这个 Offer 已停用，无法继续发放。';}
 const fresh=$('#personal-new-batch');fresh.classList.toggle('hidden',!writes||!!data.inventoryError||(!unconfirmed.length&&!ready.length));fresh.textContent=d.forceNew?'使用已有兑换码':'使用一批新的兑换码';
 personalSubmitState(d);
}
async function refreshPersonal(d){
 const generation=++deliveryGeneration;
 const data=await api(personalPath(d)+(d.cursor?'?cursor='+encodeURIComponent(d.cursor):''));if(delivery!==d||generation!==deliveryGeneration)return;
 d.listData=data;d.nextCursor=data.nextCursor;$('#personal-next').disabled=!data.nextCursor;$('#personal-first').disabled=!d.cursor;
 renderPersonalSetup(d);
 const rows=data.deliveries||[];
 $('#personal-list').innerHTML=rows.length?rows.map(item=>{
  const v=item.delivery,r=item.request;
  const claim=r?.status==='cancelled'?'已取消':r?.claimedAt?'已领取 · '+formatTime(r.claimedAt):r?.deliveredAt?'已发出，尚未通过链接领取':'已分配，尚未领取';
  const redemption=r?.verifiedAt?'已人工关联 Apple 核销证据':r?.reportedRedeemedAt?'对方反馈已兑换，尚未核实':'兑换情况：待核对';
  const canShare=r&&(r.status==='assigned'||r.status==='delivered')&&r.claimExpiresAt&&new Date(r.claimExpiresAt)>new Date();
  return `<article class="card"><h3>${escapeHTML(v.name)}</h3><p>${claim}</p><p>${redemption}</p><p class="muted">批次有效期至 ${escapeHTML(v.expiration)}。${r?.verifiedAt?'人工关联不代表 Apple 确认此人的姓名或具体码。':''}</p><div class="offer-actions">${canShare?`<button class="secondary" data-personal-link="${escapeHTML(v.id)}">复制领取链接</button>`:''}<button class="quiet" data-personal-pool="${escapeHTML(v.poolID)}">查看发放详情</button></div></article>`;
 }).join(''):'<p class="card empty">还没有熟人发放记录。</p>';
 $$('[data-personal-pool]').forEach(b=>b.onclick=()=>openDeliveryPool(d,b.dataset.personalPool));
 $$('[data-personal-link]').forEach(b=>b.onclick=async()=>{
  if(d.busy)return;d.busy=true;b.disabled=true;
  try{const result=await personalAPI(d,personalPath(d),{method:'POST',csrfRequired:true,body:{action:'resume',requestKey:b.dataset.personalLink}});if(delivery===d)showPersonalLink(result);}
  catch(e){if(delivery===d)deliveryMessage(e.message,true);}
  finally{d.busy=false;if(b.isConnected)b.disabled=false;}
 });
}
function showPersonalLink(result){
 deliveryMessage('一次性码已准备好，可复制领取链接发给朋友。');
 if(!result.token){deliveryMessage('领取链接已撤销或过期，可在发放详情中重新签发。');return;}
 const dialog=deliveryDialog('把领取链接发给这位朋友',`<p>已为这位朋友分配一枚一次性码。对方点击领取后，可前往 Apple 兑换，无需填写姓名或邮箱。</p><label>专属领取链接<input id="personal-link" readonly autocomplete="off"></label><button class="primary" id="personal-copy">复制链接</button>`);
 dialog.dataset.clearOnHide='true';const input=dialog.querySelector('#personal-link');input.value=location.origin+'/offer-claim#receipt='+encodeURIComponent(result.token);
 dialog.querySelector('#personal-copy').onclick=async()=>{try{await navigator.clipboard.writeText(input.value);deliveryMessage('链接已复制，发送后可在发放详情中记录已发出');}catch{input.select();deliveryMessage('请手动复制链接');}};
}
