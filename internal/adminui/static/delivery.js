// Offer inventory and named delivery keep input nodes stable during asynchronous reads.
let delivery = null;
let deliveryGeneration = 0;
const deliveryStatus = {requested:'待处理',assigned:'已分配，待发放',delivered:'已发放',cancelled:'已取消',rejected:'已拒绝'};
const deliveryActions = {'request.claim':'领取人通过专属链接领码','request.share_claim':'生成或复制专属领取链接','request.revoke_claim':'撤销专属领取链接','inventory.import':'导入完整码池','inventory.confirm_available':'确认未发放库存','inventory.export':'导出 CSV，移出可分配库存','request.record_external':'补录历史发放','request.personal_create':'为朋友分配一次性码','request.create':'登记申请','request.assign':'分配兑换码','code.reveal':'查看兑换码','request.deliver':'记录实际发放','request.cancel':'取消申请','request.reject':'拒绝申请','request.report_redeemed':'记录领取人反馈','request.link_verified':'人工关联已验证订阅'};
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
 $('#delivery-back').onclick=()=>parent.kind==='personal'?openPersonalDelivery(d.offer.id):openOfferInventory(d.offer.id);
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



const personalPath=d=>`/api/v1/apps/${encodeURIComponent(d.app)}/offers/${encodeURIComponent(d.offer.id)}/personal-deliveries`;
async function openPersonalDelivery(offerID){
 const offer=state.offerData?.offers?.find(v=>v.id===offerID);if(!offer)return;
 const d={app:state.currentApp,offer,kind:'personal',cursor:'',requestKey:uuid(),busy:false,pending:null};delivery=d;deliveryGeneration++;
 mountDelivery(deliveryHeader(offer,'每位朋友分配一枚 Apple 一次性码，对方无需填写个人信息。')+`
 <form id="personal-form" class="card delivery-form"><label>朋友的称呼<input name="name" required maxlength="80" autocomplete="off" placeholder="例如小王，仅后台可见"></label><label>使用哪批一次性码<select name="poolID" required disabled><option value="">正在读取库存…</option></select></label><p id="personal-stock-note" class="delivery-wide muted"></p><button class="primary" disabled>分配一枚并生成链接</button></form>
 <div class="actions"><button id="personal-inventory" class="secondary">准备一次性码库存</button></div><p class="muted">库存不足时可创建新批次，Apple 每批至少生成 500 枚，每枚各用一次。分配使用该批次的有效期。</p>
 <p class="card muted">领取不等于兑换。Apple 不提供每枚一次性码的兑换状态，个人兑换情况需人工核对。</p>
 <div class="section-head subhead"><h3>熟人发放记录</h3><button id="personal-refresh" class="secondary">刷新</button></div><div id="personal-list" class="stack"></div><div class="actions"><button id="personal-first" class="quiet">回到第一页</button><button id="personal-next" class="secondary" disabled>下一页</button></div>`);
 $('#delivery-back').onclick=closeOfferDelivery;
 $('#personal-inventory').onclick=()=>openOfferInventory(offerID);
 $('#personal-refresh').onclick=()=>refreshPersonal(d).catch(e=>deliveryMessage(e.message,true));
 $('#personal-first').onclick=()=>{d.cursor='';refreshPersonal(d).catch(e=>deliveryMessage(e.message,true));};
 $('#personal-next').onclick=()=>{d.cursor=d.nextCursor;refreshPersonal(d).catch(e=>deliveryMessage(e.message,true));};
 const form=$('#personal-form');
 form.onsubmit=async e=>{
  e.preventDefault();if(d.busy)return;d.busy=true;
  if(!d.pending)d.pending={action:'create',requestKey:d.requestKey,...Object.fromEntries(new FormData(form))};
  const button=form.querySelector('button');button.disabled=true;form.elements.name.readOnly=true;form.elements.poolID.disabled=true;
  try{
   await reauthenticate();if(delivery!==d)return;
   const result=await api(personalPath(d),{method:'POST',csrfRequired:true,body:d.pending});
   if(delivery!==d)return;
   d.pending=null;d.requestKey=uuid();form.elements.name.value='';d.cursor='';await refreshPersonal(d);showPersonalLink(result);
  }catch(e){if(delivery===d){
   if(['inventory_unavailable','invalid_delivery','delivery_not_found','reauthentication_required','forbidden'].includes(e.code)){d.pending=null;}
   deliveryMessage(d.pending?`${e.message}。重试会核对同一次分配，不会再发一枚码。`:e.message,true);
   await refreshPersonal(d).catch(()=>{});
  }}finally{d.busy=false;if(delivery===d){form.elements.name.readOnly=!!d.pending;form.elements.poolID.disabled=!!d.pending||!d.hasStock;button.disabled=!d.pending&&!d.hasStock;button.textContent=d.pending?'重试同一次分配':'分配一枚并生成链接';}}
 };
 await refreshPersonal(d).catch(e=>{if(delivery===d){$('#personal-stock-note').textContent='库存暂不可用，请刷新后重试。';deliveryMessage(e.message,true);}});
}
async function refreshPersonal(d){
 const data=await api(personalPath(d)+(d.cursor?'?cursor='+encodeURIComponent(d.cursor):''));if(delivery!==d)return;
 d.nextCursor=data.nextCursor;$('#personal-next').disabled=!data.nextCursor;$('#personal-first').disabled=!d.cursor;
 const pools=data.pools||[],ready=pools.filter(p=>p.active&&p.available>0);
 d.hasStock=ready.length>0&&!data.inventoryError;
 const form=$('#personal-form'),select=form.elements.poolID,selected=select.value;
 if(!d.pending){
  select.innerHTML=ready.length?ready.map(p=>`<option value="${escapeHTML(p.id)}">可分配 ${Number(p.available)} 枚 · ${escapeHTML(p.expiration)} 到期</option>`).join(''):'<option value="">暂无可分配的一次性码</option>';
  if(ready.some(p=>p.id===selected))select.value=selected;
 }
 select.disabled=!!d.pending||!d.hasStock||d.busy;
 form.querySelector('button').disabled=d.busy||(!d.pending&&!d.hasStock);
 $('#personal-stock-note').textContent=data.inventoryError||(ready.length?'只分配库存中已确认未发出的码。有效期与原批次一致。':pools.some(p=>p.active&&!p.managed)?'已有一次性码批次，请先导入并确认未发放库存。':'暂无可分配库存，请先准备一次性码批次。');
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
  try{await reauthenticate();if(delivery!==d)return;const result=await api(personalPath(d),{method:'POST',csrfRequired:true,body:{action:'resume',requestKey:b.dataset.personalLink}});if(delivery===d)showPersonalLink(result);}
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
