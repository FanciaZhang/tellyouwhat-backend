const claimView=document.querySelector('#claim-view');
const claimEscape=value=>String(value??'').replace(/[&<>"']/g,c=>({'&':'&amp;','<':'&lt;','>':'&gt;','"':'&quot;',"'":'&#39;'}[c]));
const claimState={busy:false,token:'',kind:''};
function claimNotice(message,error=false){const node=document.querySelector('#claim-message');node.textContent=message;node.className=`notice${error?' error':''}`;}
async function claimAPI(action,extra={}){
 const response=await fetch('/api/v1/offer-claim',{method:'POST',credentials:'omit',headers:{'Content-Type':'application/json',Accept:'application/json'},body:JSON.stringify({action,token:claimState.token,...extra})});
 const data=await response.json().catch(()=>({}));if(!response.ok)throw new Error(data.error?.message||'请求未能完成，请稍后重试。');return data;
}
async function claimRun(task){if(claimState.busy)return;claimState.busy=true;document.querySelector('#claim-message').classList.add('hidden');const controls=[...claimView.querySelectorAll('button')];controls.forEach(b=>b.disabled=true);try{await task();}catch(e){claimNotice(e.message,true);}finally{claimState.busy=false;controls.forEach(b=>{if(b.isConnected)b.disabled=false;});}}
async function loadClaimStatus(){
 const result=await claimAPI('status');const labels={requested:'发放人正在准备兑换码',assigned:'兑换码已准备好',delivered:'兑换码已发放',cancelled:'本次发放已取消',rejected:'本次发放已关闭'};
 claimView.innerHTML=`<h2>${claimEscape(result.title)}</h2><p class="notice">${labels[result.status]||'请联系发放人'}</p><div id="claim-code-area"></div><p class="muted">这是专属领取链接，请勿转发。链接有效期至 ${new Date(result.receiptExpiresAt).toLocaleDateString('zh-CN')}。</p><div class="delivery-toolbar"><button class="secondary" id="claim-copy-receipt">复制领取链接</button><button class="quiet" id="claim-refresh">刷新状态</button></div>`;
 document.querySelector('#claim-copy-receipt').onclick=()=>claimRun(async()=>{try{await navigator.clipboard.writeText(location.href);claimNotice('领取链接已复制。');}catch{claimNotice('请复制浏览器地址栏中的完整链接。');}});
 document.querySelector('#claim-refresh').onclick=()=>claimRun(loadClaimStatus);
 if(result.status==='assigned'||result.status==='delivered'){
  const area=document.querySelector('#claim-code-area');area.innerHTML='<button id="claim-reveal" class="primary">领取并查看兑换码</button>';
  document.querySelector('#claim-reveal').onclick=()=>claimRun(async()=>{const data=await claimAPI('claim');area.innerHTML='<label>你的兑换码<input id="claim-code" readonly autocomplete="off"></label><p class="muted">请在对应 App 的订阅兑换入口使用此码。Apple 会根据你的账号资格确认兑换结果。</p><button class="secondary" id="claim-copy-code">复制兑换码</button>';document.querySelector('#claim-code').value=data.code;document.querySelector('#claim-copy-code').onclick=()=>claimRun(async()=>{try{await navigator.clipboard.writeText(data.code);claimNotice('兑换码已复制');}catch{document.querySelector('#claim-code').select();claimNotice('请长按或手动复制兑换码');}});if(!data.reportedRedeemedAt){area.insertAdjacentHTML('beforeend','<p class="muted">兑换成功后，可以将结果反馈给发放人。</p><button class="quiet" id="claim-feedback">我已成功兑换</button>');document.querySelector('#claim-feedback').onclick=()=>claimRun(async()=>{await claimAPI('feedback');claimNotice('已记录你的反馈，谢谢。');await loadClaimStatus();});}});
 }
}
async function initializeClaim(){
 claimState.token=new URLSearchParams(location.hash.slice(1)).get('receipt')||'';
 if(!claimState.token){claimView.innerHTML='<p>领取链接不完整，请向发放人索取完整链接。</p>';return;}
 await claimRun(loadClaimStatus);
 if(claimView.textContent.includes("正在读取领取信息")){claimView.innerHTML='<p>暂时无法打开领取信息。</p><button class="secondary" id="claim-retry">重新读取</button>';document.querySelector("#claim-retry").onclick=()=>claimRun(loadClaimStatus);}
}
document.addEventListener('visibilitychange',()=>{if(document.hidden){const code=document.querySelector('#claim-code');if(code)document.querySelector('#claim-code-area').innerHTML='<p class="muted">兑换码已隐藏，请刷新状态后重新查看。</p>';}});
initializeClaim();
