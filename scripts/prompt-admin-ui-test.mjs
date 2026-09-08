import assert from 'node:assert/strict';
import fs from 'node:fs/promises';
import http from 'node:http';
import path from 'node:path';
const {chromium}=await import(process.env.PLAYWRIGHT_MODULE||'playwright');
const root=path.resolve('internal/adminui/static');
const server=http.createServer(async(req,res)=>{res.setHeader('Content-Security-Policy',"default-src 'none'; script-src 'self'; style-src 'self'; connect-src 'self'; img-src 'self'; frame-ancestors 'none'; base-uri 'none'; form-action 'self'");try{const file=path.join(root,req.url==='/'?'index.html':req.url);res.setHeader('Content-Type',file.endsWith('.js')?'text/javascript':file.endsWith('.css')?'text/css':'text/html');res.end(await fs.readFile(file));}catch{res.writeHead(404);res.end();}});
await new Promise(resolve=>server.listen(0,'127.0.0.1',resolve));
const browser=await chromium.launch({headless:true,executablePath:process.env.CHROME_EXECUTABLE});
const params={model:'ep-journal',reasoningEffort:'disabled',temperature:null,maxOutputTokens:8192,timeoutSeconds:90};
const policy={systemPrompt:'',journal:{organize:{prompt:'只整理原文明示的事实。',lite:{...params},pro:{...params},liteMaxCharacters:6000,liteMaxBooks:24,liteMaxTags:80},voice:{prompt:'保留手动修改，只有后续口述才能纠正已修改内容。'.repeat(40),removeRepetition:true,parameters:{...params},automaticPunctuation:true,normalizeNumbers:true},defaultStyle:'natural',styles:['自然记录','轻快活泼','严谨纪实','日常流水','散文随笔'].map((name,i)=>({id:i===0?'natural':'style-'+i,name,description:'理顺表达，保留自己的语气。',example:'下班后去了河边散步。',prompt:'保留已有细节。',enabled:true,order:i}))}};
let current={id:'published',scope:'journal',policy,createdAt:new Date().toISOString()},draft;
const context=await browser.newContext({viewport:{width:390,height:844}});const page=await context.newPage();const errors=[];page.on('pageerror',error=>errors.push(error.message));
await page.route('**/api/**',async route=>{const u=new URL(route.request().url());let data,status=200;
 if(u.pathname==='/api/v1/session'){status=401;data={error:{message:'login'}};}
 else if(u.pathname==='/api/v1/ai/health')data={operations:[],writesEnabled:true};
 else if(u.pathname==='/api/v1/ai/prompts')data={current,writesEnabled:true};
 else if(u.pathname.endsWith('/drafts')){const body=route.request().postDataJSON();draft={id:'draft',scope:body.scope,baseVersion:body.baseVersion,policy:body.policy};data=draft;}
 else if(u.pathname.includes('/revisions/'))data={revision:draft,before:current.policy,after:draft.policy,canPublish:true,previewToken:'preview'};
 else if(u.pathname.endsWith('/publish')){current={...draft,id:'next',publishedAt:new Date().toISOString()};data=current;}
 else if(u.pathname==='/api/v1/ai/models')data={models:[{Name:'example-pro',DisplayName:'Example Pro',VendorName:'Fixture'}]};
 else if(u.pathname.endsWith('/versions'))data={versions:[{FoundationModelName:'example-pro',ModelVersion:'1',ModelId:'example-pro-1',Status:'Published',Domains:['LLM']}]};
 else{status=404;data={error:{message:'Unhandled fixture '+u.pathname}};}
 await route.fulfill({status,contentType:'application/json',body:JSON.stringify(data)});
});
try{
 await page.goto(`http://127.0.0.1:${server.address().port}`,{waitUntil:'networkidle'});
 await page.evaluate(async()=>{state.user={id:'fixture',role:'admin'};state.csrf='fixture';window.checks=0;reauthenticate=async()=>window.checks++;$('#auth').classList.add('hidden');$('#workspace').classList.remove('hidden');$('#ai-section').classList.remove('hidden');$('#operations-section').classList.add('hidden');await loadAI();});
 await page.getByRole('button',{name:'手记 · 智能整理',exact:false}).click();
 const input=page.locator('[name="journal.organize.prompt"]');await input.waitFor();await input.fill('');await input.evaluate(el=>window.editNode=el);await input.pressSequentially('pro');assert.equal(await input.inputValue(),'pro');
 await input.press('ArrowLeft');await input.pressSequentially('x');assert.equal(await input.inputValue(),'prxo');assert.equal(await input.evaluate(el=>el===window.editNode&&el.selectionStart===3),true);
 const cdp=await context.newCDPSession(page);await cdp.send('Input.imeSetComposition',{text:'中文输入',selectionStart:4,selectionEnd:4});assert.equal(await input.evaluate(el=>el===window.editNode),true);await cdp.send('Input.insertText',{text:'中文输入'});assert.equal(await input.inputValue(),'prx中文输入o');
 await page.locator('[data-prompt-page="voice"]').click();assert.equal(await page.locator('[name="journal.voice.prompt"]').inputValue(),policy.journal.voice.prompt);
 await page.locator('[data-prompt-page="organize"]').click();assert.equal(await input.inputValue(),'prx中文输入o');
 await page.locator('[data-prompt-model="journal.organize.lite"]').click();const search=page.getByRole('searchbox');await search.pressSequentially('pro');assert.equal(await search.inputValue(),'pro');await page.getByRole('button',{name:'查看版本'}).click();await page.getByRole('button',{name:'使用此版本'}).click();assert.equal(await page.locator('[name="journal.organize.lite.model"]').inputValue(),'example-pro-1');
 await page.locator('[data-prompt-page="styles"]').click();await page.getByRole('button',{name:'＋ 新增风格'}).click();const name=page.locator('[name="journal.styles.5.name"]');await name.fill('克制叙述');await page.locator('[name="journal.styles.5.prompt"]').fill('保留事实，使用简短的完整句。');
 const output=process.env.PROMPT_UI_SCREENSHOT_DIR;if(output)await fs.mkdir(output,{recursive:true});
 for(const color of ['light','dark'])for(const width of [390,1100]){await page.emulateMedia({colorScheme:color});await page.setViewportSize({width,height:900});assert.ok(await page.evaluate(()=>document.documentElement.scrollWidth<=innerWidth+1),'horizontal overflow');if(output)await page.screenshot({path:path.join(output,`styles-${width}-${color}.png`),fullPage:true});}
 await page.getByRole('button',{name:'保存草稿并预览'}).click();await page.getByRole('heading',{name:'确认配置变更'}).waitFor();await page.setViewportSize({width:390,height:844});await page.getByRole('button',{name:'当前发布版',exact:true}).click();assert.equal(await page.locator('.prompt-diff').getAttribute('data-version'),'before');await page.getByRole('button',{name:'候选版本',exact:true}).click();if(output)await page.screenshot({path:path.join(output,'compare-mobile-dark.png'),fullPage:true});
 await page.getByRole('button',{name:'Passkey 确认并发布'}).click();await page.getByRole('heading',{name:'手记 · 写作风格',exact:true}).waitFor();assert.equal(await page.evaluate(()=>window.checks),1);assert.equal(current.policy.journal.styles[5].name,'克制叙述');assert.deepEqual(errors,[]);
 console.log('PASS: stable typing/caret/IME, draft retention, model catalog, styles, mobile/dark layouts, comparison, single Passkey publication');
}finally{await browser.close();await new Promise(resolve=>server.close(resolve));}
