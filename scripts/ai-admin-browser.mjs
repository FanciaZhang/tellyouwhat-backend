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
  await page.locator('[data-ai-index]').first().waitFor();await idle();
  assert.equal(await page.locator('[data-ai-index]').count(),8);
  const card=page.locator('#ai-operations article').filter({has:page.locator('input[name=webSearchEnabled]:not([disabled])')});
  await card.locator('select[name=reasoningEffort]').selectOption('high');
  await card.locator('input[name=webSearchEnabled]').check();
  await card.getByRole('button',{name:'验证并保存草稿'}).click();
  await notice('草稿已保存');
  await card.getByRole('button',{name:'验证并发布'}).click();
  await notice('配置已发布');
  const data=await checkAPI('/api/v1/ai/health');
  const row=data.operations.find(r=>r.operation==='meal_decision');
  assert.equal(row.current.policy.reasoningEffort,'high');assert.equal(row.current.policy.webSearchEnabled,true);
  await page.reload();await idle();await page.locator('#ai-tab').click();await idle();
  assert.equal(await card.locator('select[name=reasoningEffort]').inputValue(),'high');
  assert.equal(await card.locator('input[name=webSearchEnabled]').isChecked(),true);
  console.log('PASS draft, fresh Passkey, publish, and persisted configuration after reload');
  await page.locator('#ai-section > details > summary').click();
  await page.locator('#load-ai-models').click();await idle();
  assert.ok(await page.locator('#ai-model-picker option').count()>0);
  await page.locator('#ai-model-picker').selectOption('doubao-seed-2-0-lite');
  await page.locator('#ai-model-versions').click();await idle();
  for(let retry=0;retry<2&&!/输出：[0-9.]+ 元/.test(await page.locator('#ai-model-detail').innerText());retry++){
    await new Promise(resolve=>setTimeout(resolve,6000));await page.locator('#ai-model-versions').click();await idle();
  }
  assert.match(await page.locator('#ai-model-detail').innerText(),/260428/);
  assert.match(await page.locator('#ai-model-detail').innerText(),/账号已开通/);
  assert.match(await page.locator('#ai-model-detail').innerText(),/输出：[0-9.]+ 元/);
  console.log('PASS real model inventory, versions and account pricing');
  await card.getByRole('button',{name:'查看接入点与灰度详情'}).click();await idle();
  await card.locator('[data-rolling-target]').selectOption(JSON.stringify({Name:'doubao-seed-2-0-lite',ModelVersion:'260428'}));
  await card.locator('[data-rolling-action=start]').click();await notice('操作已保存');
  const endpoint=row.endpointID||row.endpoint.Id;
  let detail;
  const deadline=Date.now()+180000;
  do {
    detail=await checkAPI('/api/v1/ai/endpoints/'+endpoint);
    if(detail.rolling?.RollingGray>0)break;
    await new Promise(resolve=>setTimeout(resolve,2000));
  }while(Date.now()<deadline);
  assert.ok(detail.rolling?.RollingGray>0,'durable native start did not reach cloud');
  await card.getByRole('button',{name:'查看接入点与灰度详情'}).click();await idle();
  assert.match(await card.innerText(),/流量 [1-9][0-9]*%/);
  console.log('PASS UI preview, Passkey, SQL command and live native gray',detail.rolling.RollingGray);
  if(process.env.ARK_BROWSER_TEST_SCREENSHOT){await page.screenshot({path:process.env.ARK_BROWSER_TEST_SCREENSHOT,fullPage:true});await card.screenshot({path:process.env.ARK_BROWSER_TEST_SCREENSHOT+'.card.png'});}
  await card.locator('[data-rolling-action=cancel]').click();await notice('操作已保存');
  const cancelDeadline=Date.now()+180000;
  do {
    detail=await checkAPI('/api/v1/ai/endpoints/'+endpoint);
    if(detail.commands.some(c=>c.input.action==='cancel'&&c.state==='succeeded'))break;
    await new Promise(resolve=>setTimeout(resolve,2000));
  }while(Date.now()<cancelDeadline);
  assert.ok(detail.commands.some(c=>c.input.action==='cancel'&&c.state==='succeeded'));
  if(detail.rolling){assert.equal(detail.rolling.Status,'Reverted');assert.equal(detail.rolling.RollingGray,0);}
  await card.getByRole('button',{name:'查看接入点与灰度详情'}).click();await idle();
  assert.match(await card.innerText(),/新模型流量为 0%/);
  assert.deepEqual(errors,[]);
  console.log('PASS UI cancel, durable execution and cloud Reverted/0%');
  fs.writeFileSync(ready+'.done','passed\n',{mode:0o600});
} catch(error) {
  console.error('NOTICE',await page.locator('#notice').innerText().catch(()=>''));
  fs.writeFileSync(ready+'.done','failed\n',{mode:0o600});
  console.error('Browser acceptance failed:',error.message.split('Call log:')[0]);process.exitCode=1;
} finally {await browser.close();}
