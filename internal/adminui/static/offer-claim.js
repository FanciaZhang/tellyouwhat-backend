const claimView=document.querySelector('#claim-view');
const claimEscape=value=>String(value??'').replace(/[&<>"']/g,c=>({'&':'&amp;','<':'&lt;','>':'&gt;','"':'&quot;',"'":'&#39;'}[c]));
const claimState={busy:false,token:'',kind:'',requestKey:crypto.randomUUID()};
function claimNotice(message,error=false){const node=document.querySelector('#claim-message');node.textContent=message;node.className=`notice${error?' error':''}`;}
async function claimAPI(action,extra={}){
 const response=await fetch('/api/v1/offer-claim',{method:'POST',credentials:'omit',headers:{'Content-Type':'application/json',Accept:'application/json'},body:JSON.stringify({action,token:claimState.token,...extra})});
 const data=await response.json().catch(()=>({}));if(!response.ok)throw new Error(data.error?.message||'请求未能完成，请稍后重试。');return data;
}
async function claimRun(task){if(claimState.busy)return;claimState.busy=true;const controls=[...claimView.querySelectorAll('button')];controls.forEach(b=>b.disabled=true);try{await task();}catch(e){claimNotice(e.message,true);}finally{claimState.busy=false;controls.forEach(b=>{if(b.isConnected)b.disabled=false;});}}
function claimForm(info){
 claimView.innerHTML=`<h2>${claimEscape(info.title)}</h2><p class="muted">${claimEscape(info.channel)} · 截止 ${new Date(info.expiresAt).toLocaleDateString('zh-CN')}</p>`;
 if(!info.accepting){claimView.insertAdjacentHTML('beforeend','<p>本次申请已经结束或名额已满。如果已提交，请使用你保存的查询回执查看结果。</p>');return;}
 claimView.insertAdjacentHTML('beforeend',`<form id="claim-form"><label>姓名或昵称<input name="name" required maxlength="80" autocomplete="name" placeholder="让发放人能认出你"></label><label>联系方式<input name="contact" maxlength="160" autocomplete="off" placeholder="可选，方便发放人与你核对"></label><label>备注<textarea name="note" maxlength="1000" rows="3" placeholder="可选"></textarea></label><label class="check"><input name="consent" type="checkbox" required>我同意发放人保存这些信息，用于审核申请、发放及核对领取状态。</label><p class="muted"><small>提交不保证获批。通过后使用查询回执领取兑换码；无需提供 Apple 密码、付款信息或健康数据。</small></p><button class="primary">提交申请</button></form>`);
 document.querySelector('#claim-form').onsubmit=e=>{e.preventDefault();const values=Object.fromEntries(new FormData(e.currentTarget));claimRun(async()=>{
  const result=await claimAPI('apply',{name:values.name,contact:values.contact,note:values.note,consent:values.consent==='on',requestKey:claimState.requestKey});
  claimState.kind='receipt';claimState.token=result.receiptToken;history.replaceState(null,'','#receipt='+encodeURIComponent(result.receiptToken));
  claimNotice('申请已提交。请保存下面的查询链接，稍后回来领取。');await loadClaimStatus();
 });};
}
async function loadClaimStatus(){
 const result=await claimAPI('status');const labels={requested:'申请已收到，等待审核',assigned:'已通过，可以领取兑换码',delivered:'兑换码已发放',cancelled:'申请已取消',rejected:'申请未通过'};
 claimView.innerHTML=`<h2>${claimEscape(result.title)}</h2><p>${claimEscape(result.name)}</p><p class="notice">${labels[result.status]||'请联系发放人'}</p><p class="muted">这个回执只用于查询你自己的申请，请勿转发。查询期限至 ${new Date(result.receiptExpiresAt).toLocaleDateString('zh-CN')}。</p><div class="delivery-toolbar"><button class="secondary" id="claim-copy-receipt">复制查询链接</button><button class="quiet" id="claim-refresh">刷新状态</button></div><div id="claim-code-area"></div>`;
 document.querySelector('#claim-copy-receipt').onclick=()=>claimRun(async()=>{try{await navigator.clipboard.writeText(location.href);claimNotice('查询链接已复制，请保存在自己的备忘录中。');}catch{claimNotice('请复制浏览器地址栏中的完整链接。');}});
 document.querySelector('#claim-refresh').onclick=()=>claimRun(loadClaimStatus);
 if(result.status==='assigned'||result.status==='delivered'){
  const area=document.querySelector('#claim-code-area');area.innerHTML='<button id="claim-reveal" class="primary">领取并查看兑换码</button>';
  document.querySelector('#claim-reveal').onclick=()=>claimRun(async()=>{const data=await claimAPI('claim');area.innerHTML='<label>你的兑换码<input id="claim-code" readonly autocomplete="off"></label><p class="muted">请在对应 App 的订阅兑换入口使用此码。Apple 会根据你的账号资格确认兑换结果。</p><button class="secondary" id="claim-copy-code">复制兑换码</button>';document.querySelector('#claim-code').value=data.code;document.querySelector('#claim-copy-code').onclick=()=>claimRun(async()=>{try{await navigator.clipboard.writeText(data.code);claimNotice('兑换码已复制');}catch{document.querySelector('#claim-code').select();claimNotice('请长按或手动复制兑换码');}});if(!data.reportedRedeemedAt){area.insertAdjacentHTML('beforeend','<p class="muted">兑换成功后，可以将结果反馈给发放人。</p><button class="quiet" id="claim-feedback">我已成功兑换</button>');document.querySelector('#claim-feedback').onclick=()=>claimRun(async()=>{await claimAPI('feedback');claimNotice('已记录你的反馈，谢谢。');await loadClaimStatus();});}});
 }
}
async function initializeClaim(){
 const params=new URLSearchParams(location.hash.slice(1));claimState.kind=params.has('receipt')?'receipt':'link';claimState.token=params.get(claimState.kind)||'';
 if(!claimState.token){claimView.innerHTML='<p>领取链接不完整，请向发放人索取完整链接。</p>';return;}
 try{const key='offer-claim-request:'+claimState.token;claimState.requestKey=sessionStorage.getItem(key)||claimState.requestKey;if(claimState.kind==='link')sessionStorage.setItem(key,claimState.requestKey);}catch{}
 await claimRun(async()=>{if(claimState.kind==='receipt')await loadClaimStatus();else claimForm(await claimAPI('inspect'));});
}
document.addEventListener('visibilitychange',()=>{if(document.hidden){const code=document.querySelector('#claim-code');if(code)document.querySelector('#claim-code-area').innerHTML='<p class="muted">兑换码已隐藏，请刷新状态后重新查看。</p>';}});
initializeClaim();
