// Offer inventory and named delivery keep input nodes stable during asynchronous reads.
let delivery = null;
let deliveryGeneration = 0;
const deliveryStatus = {requested:'待处理',assigned:'已分配，待发放',delivered:'已发放',cancelled:'已取消',rejected:'已拒绝'};
const deliveryActions = {'link.create':'创建申请链接','link.revoke':'撤销申请链接','inventory.import':'导入完整码池','inventory.confirm_available':'确认未发放库存','inventory.export':'导出 CSV，移出可分配库存','request.record_external':'补录历史发放','request.create':'登记申请','request.assign':'分配兑换码','request.reveal':'查看兑换码','request.deliver':'记录实际发放','request.cancel':'取消申请','request.reject':'拒绝申请','request.report_redeemed':'记录领取人反馈','request.link_verified':'人工关联已验证订阅'};
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
async function openOfferInventory(id){
 const offer=state.offerData?.offers?.find(o=>o.id===id);if(!offer)return;
 const d={app:state.currentApp,offer,poolID:null};delivery=d;const generation=++deliveryGeneration;
 mountDelivery(deliveryHeader(offer,'选择码池，查看库存并管理每一位领取人。')+'<div class="actions"><button id="delivery-create-pool" class="secondary">新建码池</button></div><div id="delivery-pools" class="stack"><p class="muted">正在读取 Apple 码池…</p></div>');
 $('#delivery-back').onclick=closeOfferDelivery;
 $('#delivery-create-pool').disabled=!offer.active||!state.offerData.writesEnabled;
 $('#delivery-create-pool').onclick=()=>openCodes(id);
 try{
  const data=await api(`/api/v1/apps/${encodeURIComponent(d.app)}/offers/${encodeURIComponent(id)}/code-pools`);
  if(delivery!==d||generation!==deliveryGeneration)return;
  const pools=data.codePools||[];
  $('#delivery-pools').innerHTML=pools.length?pools.map(p=>`<article class="card delivery-pool"><div><h3>${p.kind==='custom'?'自定义共享码':'Apple 一次性码'}</h3><p>${p.environment==='SANDBOX'?'沙盒测试':'正式环境'} · 生成 ${Number(p.numberOfCodes).toLocaleString()} 次兑换额度</p><p class="muted">${p.active&&offer.active?'启用中':'已停用'} · ${p.expirationDate?'到期 '+escapeHTML(p.expirationDate):'未设置到期日'}</p><small class="muted">码池 ${escapeHTML(p.id)}</small></div><button class="primary" data-delivery-pool="${escapeHTML(p.id)}">管理发放</button></article>`).join(''):'<p class="card muted">还没有码池。创建码池后即可登记领取人。</p>';
  $$('[data-delivery-pool]').forEach(b=>b.onclick=()=>openDeliveryPool(d,b.dataset.deliveryPool));
 }catch(e){if(delivery===d){$('#delivery-pools').textContent='未能读取码池';deliveryMessage(e.message,true);}}
}
async function openDeliveryPool(parent,poolID){
 const d={app:parent.app,offer:parent.offer,poolID,cursor:'',query:'',status:'',requestKey:uuid(),externalKey:uuid(),linkKey:uuid(),data:null};delivery=d;deliveryGeneration++;
 mountDelivery(deliveryHeader(d.offer,'库存与领取台账')+`
 <div class="delivery-toolbar"><button id="delivery-refresh" class="secondary">同步 Apple 库存</button><span id="delivery-pool-meta" class="muted">正在读取…</span></div>
 <div id="delivery-summary" class="delivery-metrics"></div>
 <details class="card delivery-inventory"><summary>库存管理与统计口径</summary><div id="delivery-inventory"></div></details>
 <details class="card" id="delivery-link-panel"><summary>申请链接</summary><p class="muted">发给领取人自行登记。通过审批、分配后，对方使用自己的回执领取兑换码。撤销链接仅停止新申请。</p><form id="delivery-create-link" class="delivery-form"><label>活动或渠道名称<input name="label" maxlength="80" required placeholder="例如：九月内测招募"></label><label>最多接收申请数<input name="maxApplications" type="number" min="1" max="10000" value="100" required></label><label>申请截止时间<input name="expiresAt" type="datetime-local" required></label><div class="actions"><button class="primary">创建申请链接</button></div></form><div id="delivery-links" class="stack"></div></details><details class="card" id="delivery-new"><summary>登记领取申请</summary><form id="delivery-request-form" class="delivery-form">
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
 $('#delivery-create-link [name=expiresAt]').value=new Date(Date.now()+7*86400000-new Date().getTimezoneOffset()*60000).toISOString().slice(0,16);
 $('#delivery-create-link').onsubmit=e=>{e.preventDefault();const values=Object.fromEntries(new FormData(e.currentTarget));deliveryRun(async()=>{const result=await deliveryCommand({action:'create_link',label:values.label,maxApplications:Number(values.maxApplications),expiresAt:new Date(values.expiresAt).toISOString(),requestKey:d.linkKey});d.linkKey=uuid();await refreshDelivery();showDeliveryClaimLink(result.token);});};
 $('#delivery-back').onclick=()=>openOfferInventory(d.offer.id);
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
 renderDeliverySummary(data);renderDeliveryRequests(data);renderDeliveryLinks(data);
 $('#delivery-ledger-count').textContent=`${data.summary.applications} 条申请 · ${data.summary.externalDeliveries} 条历史发放`;
 $('#delivery-first').disabled=!d.cursor;$('#delivery-next').disabled=!data.nextCursor;
 $('#delivery-events').innerHTML=(data.events||[]).map(e=>`<div class="delivery-event"><strong>${escapeHTML(deliveryActions[e.action]||e.action)}</strong><small>${formatTime(e.createdAt)} · ${e.quantity} 条 · 操作者 ${escapeHTML(e.actorID)}</small></div>`).join('')||'<p class="muted">暂无操作记录</p>';
}
function renderDeliverySummary(data){
 const p=data.pool,s=data.summary;const usable=p.active&&new Date(p.expiresAt)>new Date();
 $('#delivery-pool-meta').textContent=`${p.environment==='sandbox'?'沙盒测试':'正式环境'} · ${p.kind==='custom'?'自定义共享码':'一次性码'} · ${usable?'可发放':'已停用或过期'} · 同步于 ${formatTime(p.syncedAt)}${data.stale?'（已过期，请刷新）':''}`;
 const metrics=[['生成兑换额度',p.capacity],['领取申请',s.applications],['已发放',s.delivered],['Offer 已验证核销',data.observedOfferSubscriptions]];
 $('#delivery-summary').innerHTML=metrics.map(([name,value])=>`<article class="card metric"><span>${name}</span><strong>${value==null?'—':Number(value).toLocaleString()}</strong></article>`).join('')+`<p class="delivery-wide muted">待处理 ${s.pending} · 已分配 ${s.assignedRequests} · 反馈已兑换 ${s.reportedRedeemed} · 人工关联已验证订阅 ${s.linkedVerified}<br><small>Offer 核销覆盖同一环境下的全部码池，来自后端收到并验证的 Apple 交易，可能不含全部历史。</small></p>`;
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
  if(r.status==='assigned')actions=button('reveal','查看兑换码')+button('deliver','记录已发放')+button('cancel','取消');
  if(r.status==='delivered')actions=button('reveal','查看兑换码')+(!r.reportedRedeemedAt?button('report_redeemed','记录已兑换反馈'):'')+(!r.verifiedAt?button('link_verified','关联核销证据'):'');
  return `<article class="card delivery-recipient"><div class="section-head"><h3>${escapeHTML(v.name)}</h3><span class="chip">${deliveryStatus[r.status]||escapeHTML(r.status)}</span></div><p>${escapeHTML(v.contact||'未填联系方式')}${v.reference?' · '+escapeHTML(v.reference):''}</p><p class="muted">${escapeHTML(v.channel||'未填渠道')} · ${r.source==='external'?'历史发放补录于':'申请于'} ${formatTime(r.requestedAt)}</p>${v.note?`<p class="delivery-note">${escapeHTML(v.note)}</p>`:''}<div class="delivery-stages">${r.assignedAt?`<small>分配：${formatTime(r.assignedAt)}</small>`:''}${r.deliveredAt?`<small>发放：${formatTime(r.deliveredAt)}</small>`:''}${r.reportedRedeemedAt?'<small>领取人反馈已兑换（未据此核销）</small>':''}${r.verifiedAt?'<small>管理员已关联 Apple 验证订阅</small>':''}</div><div class="offer-actions">${actions}</div></article>`;
 }).join(''):`<p class="card muted">${data.nextCursor?'本页未找到匹配记录，可继续下一页检索。':'没有匹配的领取记录。'}</p>`;
 $$('[data-delivery-action]').forEach(b=>b.onclick=()=>{
  const r=data.requests.find(r=>r.id===b.dataset.request),action=b.dataset.deliveryAction;
  if(action==='link_verified'){showDeliveryLink(r);return;}
  const messages={cancel:'取消后，已经分配的码不会重新进入库存。确认取消？',deliver:'确认已经把兑换码发给这位领取人？此操作只记录发放，不表示已兑换。',report_redeemed:'确认已收到这位领取人的兑换反馈？反馈会单独记录，不作为 Apple 核销证据。',reject:'确认拒绝这条领取申请？'};
  if(messages[action]&&!confirm(messages[action]))return;
  deliveryRun(async()=>{const result=await deliveryCommand({action,requestID:r.id,version:r.version});if(action==='reveal'){showDeliverySecret(r,result.code);return;}await refreshDelivery();deliveryMessage('记录已更新');});
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
 const dialog=deliveryDialog('发送领取申请链接','<p class="muted">任何获得此链接的人都可以申请，是否发放由你审批。请通过你选择的渠道发送。</p><label>申请链接<input id="delivery-claim-link" readonly></label><button id="delivery-copy-claim-link" class="primary">复制链接</button><p id="delivery-link-copy-message" role="status"></p>');
 dialog.dataset.clearOnHide='true';
 const link=location.origin+'/offer-claim#link='+encodeURIComponent(token);dialog.querySelector('input').value=link;dialog.querySelector('#delivery-copy-claim-link').onclick=async()=>{try{await navigator.clipboard.writeText(link);dialog.querySelector('#delivery-link-copy-message').textContent='已复制';}catch{dialog.querySelector('input').select();dialog.querySelector('#delivery-link-copy-message').textContent='请手动复制完整链接';}};
}
function renderDeliveryLinks(data){
 const node=$('#delivery-links');node.innerHTML=(data.links||[]).map(l=>`<article class="delivery-event"><strong>${escapeHTML(l.label)}</strong><small>${l.applications} / ${l.maxApplications} 条申请 · 截止 ${formatTime(l.expiresAt)}${l.revokedAt?' · 已撤销':''}</small><div class="delivery-toolbar"><button class="secondary" data-copy-claim="${escapeHTML(l.id)}" ${l.revokedAt?'disabled':''}>复制申请链接</button><button class="quiet" data-revoke-claim="${escapeHTML(l.id)}" ${l.revokedAt?'disabled':''}>停止接收新申请</button></div></article>`).join('')||'<p class="muted">暂无申请链接。</p>';
 node.querySelectorAll('[data-copy-claim]').forEach(b=>b.onclick=()=>deliveryRun(async()=>{const result=await deliveryCommand({action:'copy_link',linkID:b.dataset.copyClaim});showDeliveryClaimLink(result.token);}));
 node.querySelectorAll('[data-revoke-claim]').forEach(b=>b.onclick=()=>{if(!confirm('停止这个链接接收新申请？已有申请和已分配的兑换码会保留。'))return;deliveryRun(async()=>{await deliveryCommand({action:'revoke_link',linkID:b.dataset.revokeClaim});await refreshDelivery();deliveryMessage('申请链接已停止接收新申请');});});
 $('#delivery-create-link button').disabled=!data.pool.active||new Date(data.pool.expiresAt)<=new Date();
}
