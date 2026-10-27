// Queue a ComfyUI UI-format workflow through the local web frontend.
// Usage: node queue_workflow.js <workflow.json> [http://127.0.0.1:8188]
const fs = require('fs');
const path = require('path');
const { chromium } = require('D:/data10086/VScode/Flutter/Yun_Cloud_Computer/.uipreview/node_modules/playwright-core');

const workflowPath = process.argv[2];
const base = process.argv[3] || 'http://127.0.0.1:8188';
if (!workflowPath) {
  console.error('usage: node queue_workflow.js <workflow.json> [base]');
  process.exit(2);
}
const workflow = JSON.parse(fs.readFileSync(workflowPath, 'utf8'));

(async () => {
  const browser = await chromium.launch({
    executablePath: 'C:/Users/29449/AppData/Local/ms-playwright/chromium-1228/chrome-win64/chrome.exe',
    headless: true,
    args: ['--disable-gpu', '--no-sandbox'],
  });
  const page = await browser.newPage({ viewport: { width: 1600, height: 1000 } });
  page.on('console', m => {
    if (m.type() === 'error') console.error('[browser]', m.text());
  });
  await page.goto(base, { waitUntil: 'domcontentloaded', timeout: 120000 });
  await page.waitForFunction(() => window.app && window.app.graph, null, { timeout: 120000 });
  await page.evaluate(async (wf) => {
    await window.app.loadGraphData(wf, true, true);
  }, workflow);
  await page.waitForTimeout(1500);
  const prompt = await page.evaluate(async () => {
    const p = await window.app.graphToPrompt();
    return p.output;
  });
  console.log('api nodes:', Object.keys(prompt).length);
  const res = await page.evaluate(async (p) => {
    const r = await fetch('/prompt', {
      method: 'POST',
      headers: { 'Content-Type': 'application/json' },
      body: JSON.stringify({ prompt: p, client_id: 'starstack-smoke' }),
    });
    return { status: r.status, body: await r.json() };
  }, prompt);
  console.log('queue:', JSON.stringify(res));
  fs.writeFileSync(path.join(path.dirname(workflowPath), 'last_prompt.json'), JSON.stringify(prompt, null, 1));
  await browser.close();
  process.exit(res.status === 200 && res.body.prompt_id ? 0 : 1);
})().catch(e => { console.error(e); process.exit(1); });
