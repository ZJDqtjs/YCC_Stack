// Static + runtime check for the native mini program.
// No WeChat devtools needed: wx.* is stubbed over Node's fetch so the shared
// client, the page modules and the WXML bindings are exercised for real.
const fs = require('fs');
const path = require('path');
const { execFileSync } = require('child_process');

const ROOT = path.join(__dirname, '..', 'miniapp');
const BROKER = process.env.STARSTACK_BROKER || 'http://127.0.0.1:8899';
let failures = 0;
const fail = (msg) => { failures++; console.log('FAIL ' + msg); };
const ok = (msg) => console.log('ok   ' + msg);

// ---------- static: manifests and files ----------
const appJson = JSON.parse(fs.readFileSync(path.join(ROOT, 'app.json'), 'utf8'));
for (const p of appJson.pages) {
  for (const ext of ['js', 'json', 'wxml', 'wxss']) {
    const f = path.join(ROOT, p + '.' + ext);
    if (!fs.existsSync(f)) fail(`missing ${p}.${ext}`);
  }
  JSON.parse(fs.readFileSync(path.join(ROOT, p + '.json'), 'utf8'));
}
ok(`${appJson.pages.length} pages have js/json/wxml/wxss`);

for (const item of appJson.tabBar.list) {
  if (!appJson.pages.includes(item.pagePath)) fail(`tabBar page not registered: ${item.pagePath}`);
}
ok('tabBar pages are registered');

// ---------- static: WXML expressions cannot call methods ----------
for (const p of appJson.pages) {
  const wxml = fs.readFileSync(path.join(ROOT, p + '.wxml'), 'utf8');
  const calls = wxml.match(/\{\{[^}]*\.(toFixed|toUpperCase|toLowerCase|slice|join|map|filter|replace)\s*\(/g);
  if (calls) fail(`${p}.wxml uses unsupported method calls: ${calls.join(', ')}`);
}
ok('WXML bindings are method-free');

// ---------- static: page modules load under a stubbed runtime ----------
const storage = new Map();
global.wx = {
  getStorageSync: (k) => storage.get(k) || '',
  setStorageSync: (k, v) => storage.set(k, v),
  removeStorageSync: (k) => storage.delete(k),
  reLaunch: () => {},
  switchTab: () => {},
  showToast: () => {},
  showModal: () => {},
  stopPullDownRefresh: () => {},
  setClipboardData: () => {},
  request: ({ url, method, header, data, success, fail: onError }) =>
    fetch(url, { method, headers: header, body: data })
      .then(async (r) => success({ statusCode: r.status, data: await r.text() }))
      .catch((e) => onError({ errMsg: e.message })),
  uploadFile: ({ url, filePath, name, header, formData, success, fail: onFail }) => {
    const fd = new FormData();
    for (const [k, v] of Object.entries(formData || {})) fd.append(k, String(v));
    const buf = fs.readFileSync(filePath);
    fd.append(name, new Blob([buf]), path.basename(filePath));
    fetch(url, { method: 'POST', headers: header, body: fd })
      .then(async (r) => success({ statusCode: r.status, data: await r.text() }))
      .catch((e) => onFail(e));
    return { onProgressUpdate: () => {} };
  },
};
global.App = () => {};
global.getApp = () => ({ globalData: { timers: {} } });

for (const p of appJson.pages) {
  try {
    let captured = null;
    global.Page = (opts) => { captured = opts; };
    delete require.cache[require.resolve(path.join(ROOT, p + '.js'))];
    require(path.join(ROOT, p + '.js'));
    if (!captured) fail(`${p}.js did not call Page()`);
  } catch (e) {
    fail(`${p}.js threw: ${e.message}`);
  }
}
ok('all page modules load');

// ---------- runtime: drive the API adapter against a broker ----------
(async () => {
  const api = require(path.join(ROOT, 'utils', 'api.js'));
  storage.set('ss-base', BROKER);
  try {
    const st = await api.api.state();
    if (typeof st.policy !== 'object') fail('state has no policy');
    const user = 'wx' + Date.now().toString().slice(-6);
    const reg = await api.api.register(user, 'pass12345');
    if (!reg.token) fail('register returned no token');
    api.setToken(reg.token);
    const me = await api.api.me();
    if (me.signed_in !== true) fail('bearer token did not authenticate');
    const [jobs, ledger, market] = await Promise.all([api.api.jobs(), api.api.ledger(), api.api.market()]);
    if (!Array.isArray(jobs.jobs)) fail('jobs shape');
    if (!Array.isArray(ledger.spend)) fail('ledger shape');
    if (!Array.isArray(market.slices)) fail('market shape');

    const est = await api.api.estimate({ duration: 2, vcodec: 'libx264', req: {} });
    if (!(est.hold > 0)) fail('estimate returned no hold');

    const sub = await api.api.submitTranscode({
      filePath: process.env.STARSTACK_SAMPLE,
      name: 'file',
      formData: { req: '{}', segs: '4', vcodec: 'libx264', preset: 'fast', q: '26', height: '0', audio: '1', baseline: '0' },
    });
    if (!sub.id) fail('transcode submission returned no id');
    ok(`runtime: ${user} logged in, uploaded ${sub.id}, hold ${sub.hold}`);

    const out = await api.api.logout();
    const after = await api.api.me();
    if (after.signed_in !== false) fail('logout did not revoke the token');
    ok('logout revokes the bearer token');
  } catch (e) {
    fail('runtime: ' + (e.message || e));
  }
  console.log(failures ? `\n${failures} FAILURE(S)` : '\nall checks passed');
  process.exit(failures ? 1 : 0);
})();
