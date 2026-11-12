import assert from 'node:assert/strict';
import fs from 'node:fs/promises';
import path from 'node:path';
const {chromium}=await import(process.env.PLAYWRIGHT_MODULE||'playwright');
const base=new URL(process.env.VOICE_LIVE_URL);assert.equal(base.hostname,'127.0.0.1');assert.equal(base.protocol,'https:');
const browser=await chromium.launch({headless:true,executablePath:process.env.CHROME_EXECUTABLE});
const context=await browser.newContext({ignoreHTTPSErrors:true,viewport:{width:1180,height:900}});const page=await context.newPage();page.setDefaultTimeout(120000);const errors=[];page.on('pageerror',e=>errors.push(e.message));
await context.addCookies([{name:'__Host-tellyouwhat_admin_session',value:process.env.VOICE_LIVE_SESSION,url:base.origin,secure:true,httpOnly:true,sameSite:'Strict'}]);
const evidence=process.env.VOICE_LIVE_EVIDENCE;await fs.mkdir(evidence,{recursive:true});
async function open(){await page.goto(base.href,{waitUntil:'networkidle'});await page.locator('#ai-tab').waitFor({state:'visible'});await page.locator('#ai-tab').click();await page.locator('#lab-transcript').waitFor();}

try{
 await open();assert.equal(await page.locator('#lab-publish').count(),0);
 await page.locator('#lab-file').setInputFiles(process.env.VOICE_LIVE_AUDIO);await page.locator('#lab-audio-preview').waitFor();await page.waitForFunction(()=>!voiceLab.busy);
 assert.equal(await page.locator('#lab-asr-mode').inputValue(),'recording');await page.locator('#lab-transcribe').click();await page.locator('#lab-start').waitFor();await page.locator('#lab-start').click();await page.waitForFunction(()=>voiceLab.task?.completed&&!voiceLab.busy,null,{timeout:150000});
 const transcript=await page.locator('#lab-transcript').inputValue();assert.ok(transcript.includes('河')&&transcript.includes('茶'),await page.locator('#lab-status').innerText());
 await page.locator('#lab-organize').click();await page.locator('#lab-start').waitFor();await page.locator('#lab-start').click();await page.waitForFunction(()=>voiceLab.results.length>0&&!voiceLab.busy,null,{timeout:150000});
 const prose=await page.locator('.lab-prose').innerText();assert.ok(prose.includes('茶')&&prose.includes('河'));assert.equal(await page.locator('.lab-result .error').count(),0);
 await page.evaluate(()=>voiceLab.save);await page.screenshot({path:path.join(evidence,'live-workbench.png'),fullPage:true});await open();assert.equal(await page.locator('#lab-transcript').inputValue(),transcript);assert.equal(await page.locator('.lab-prose').innerText(),prose);
 assert.deepEqual(errors,[]);await fs.writeFile(path.join(evidence,'live-browser-result.json'),JSON.stringify({synthetic:true,transcript,prose,restoredAfterReload:true,operatorCannotPublish:true},null,2));console.log('PASS: actual admin API, whole-recording ASR, finalization, encrypted queue, browser application and IndexedDB reload.');
}finally{await browser.close();}
