// Opt-in acceptance against TestLiveAIBrowserServer and disposable Ark resources.
import fs from 'node:fs';
import assert from 'node:assert/strict';
const {chromium} = await import(process.env.PLAYWRIGHT_MODULE || 'playwright');
const ready = process.env.ARK_BROWSER_TEST_READY_FILE;
const fixture = JSON.parse(fs.readFileSync(ready, 'utf8'));
const origin = 'https://admin.e2e.test';
const server = new URL(fixture.serverURL);
const browser = await chromium.launch({headless:true, executablePath:process.env.CHROME_EXECUTABLE,
  args:[`--host-resolver-rules=MAP admin.e2e.test 127.0.0.1:${server.port}`, '--no-proxy-server']});
const context = await browser.newContext({ignoreHTTPSErrors:true,viewport:{width:1280,height:1000}});
const page = await context.newPage();
page.setDefaultTimeout(60000);
const cdp = await context.newCDPSession(page);
await cdp.send('WebAuthn.enable');
await cdp.send('WebAuthn.addVirtualAuthenticator',{options:{protocol:'ctap2',transport:'internal',hasResidentKey:true,hasUserVerification:true,isUserVerified:true,automaticPresenceSimulation:true}});
const errors=[];
page.on('pageerror',error=>errors.push(error.message));
page.on('dialog',dialog=>dialog.accept());
page.on('response',response=>{if(response.status()>=400&&response.url().includes('/api/')) console.log('HTTP',response.status(),new URL(response.url()).pathname);});
async function idle(){await page.waitForFunction(()=>!state.busy);}
async function notice(text){await page.waitForFunction(text=>document.querySelector('#notice').textContent.includes(text),text);await idle();}
async function checkAPI(path){const result=await page.evaluate(async path=>{const response=await fetch(path);return {status:response.status,data:await response.json()};},path);assert.equal(result.status,200);return result.data;}
try {
  await page.goto(fixture.setupURL);
  await page.locator('#display-name').fill('Browser acceptance');
  await page.locator('#setup').click();
  await page.locator('#ai-tab').waitFor({state:'visible'});
  await idle();
  console.log('PASS WebAuthn registration and authenticated session');
  await page.locator('#ai-tab').click();
  await page.locator('[data-ai-row]').first().waitFor();await idle();
  assert.equal(await page.locator('[data-ai-row]').count(),8);
  let config=await checkAPI('/api/v1/ai/health');
  const index=config.operations.findIndex(r=>r.operation==='meal_decision');
  await page.locator(`[data-ai-params="${index}"]`).click();
  await page.locator('[name=reasoningEffort]').selectOption('high');
  await page.locator('[name=webSearchEnabled]').check();
  await page.getByRole('button',{name:'预览变更',exact:true}).click();
  await page.getByRole('heading',{name:'确认参数变更',exact:true}).waitFor();
  await page.getByRole('button',{name:'通行密钥确认并发布'}).click();
  await notice('配置已发布');
  config=await checkAPI('/api/v1/ai/health');
  const row=config.operations.find(r=>r.operation==='meal_decision');
  assert.equal(row.current.policy.reasoningEffort,'high');assert.equal(row.current.policy.webSearchEnabled,true);
  await page.reload();await page.locator('#ai-tab').waitFor({state:'visible'});await idle();await page.locator('#ai-tab').click();await page.locator('[data-ai-row]').first().waitFor();
  assert.match(await page.locator(`[data-ai-row="${index}"]`).innerText(),/深度.*开启/);
  console.log('PASS real WebAuthn, server preview, publication and SQL persistence');
  const textRow=config.operations.find(r=>r.operation==='meal_text_capture');
  const endpoint=textRow.endpointID||textRow.endpoint.Id;
  await page.locator(`[data-ai-switch="${endpoint}"]`).first().click();
  await page.locator('[data-ai-model]').first().waitFor();
  const model=process.env.ARK_BROWSER_TEST_READ_ONLY==='1'?'doubao-seed-2-1-pro':'doubao-seed-2-0-lite';
  await page.getByRole('searchbox').fill(model);
  await page.locator(`[data-ai-model="${model}"]`).click();
  await page.locator('#ai-version option').first().waitFor({state:'attached'});
  await page.locator('#ai-version').selectOption(model.endsWith('pro')?'260628':'260428');
  await page.getByRole('button',{name:'检查并预览'}).click();
  await page.getByRole('heading',{name:'确认模型切换',exact:true}).waitFor({timeout:480000});
  console.log('PASS complete cloud catalog, selected account prices, scoped protocol checks and real native DryRun');
  if(process.env.ARK_BROWSER_TEST_SCREENSHOT)await page.screenshot({path:process.env.ARK_BROWSER_TEST_SCREENSHOT,fullPage:true});
  if(process.env.ARK_BROWSER_TEST_READ_ONLY!=='1'){
    await page.getByRole('button',{name:'通行密钥确认并开始'}).click();
    await notice('操作已保存');
    const deadline=Date.now()+180000;let detail;
    do{detail=await checkAPI('/api/v1/ai/endpoints/'+endpoint);if(detail.rolling?.RollingGray>0)break;await new Promise(resolve=>setTimeout(resolve,2000));}while(Date.now()<deadline);
    assert.ok(detail.rolling?.RollingGray>0,'durable native start did not reach cloud');
    await page.getByRole('button',{name:'立即刷新'}).click();
    await page.getByRole('button',{name:'撤销本次灰度',exact:true}).click();
    await page.getByRole('button',{name:'通行密钥确认并提交'}).click();
    const cancelDeadline=Date.now()+180000;
    do{detail=await checkAPI('/api/v1/ai/endpoints/'+endpoint);if(detail.commands.some(c=>c.input.action==='cancel'&&c.state==='succeeded'))break;await new Promise(resolve=>setTimeout(resolve,2000));}while(Date.now()<cancelDeadline);
    assert.ok(detail.commands.some(c=>c.input.action==='cancel'&&c.state==='succeeded'));
    console.log('PASS durable native start and cancellation');
  }
  assert.deepEqual(errors,[]);
  fs.writeFileSync(ready+'.done','passed\n',{mode:0o600});
} catch(error) {
  console.error('NOTICE',await page.locator('#notice').innerText().catch(()=>''));
  fs.writeFileSync(ready+'.done','failed\n',{mode:0o600});
  console.error('Browser acceptance failed:',error.message.split('Call log:')[0]);process.exitCode=1;
} finally {await browser.close();}
