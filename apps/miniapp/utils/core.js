var __defProp = Object.defineProperty;
var __defProps = Object.defineProperties;
var __getOwnPropDesc = Object.getOwnPropertyDescriptor;
var __getOwnPropDescs = Object.getOwnPropertyDescriptors;
var __getOwnPropNames = Object.getOwnPropertyNames;
var __getOwnPropSymbols = Object.getOwnPropertySymbols;
var __hasOwnProp = Object.prototype.hasOwnProperty;
var __propIsEnum = Object.prototype.propertyIsEnumerable;
var __defNormalProp = (obj, key, value) => key in obj ? __defProp(obj, key, { enumerable: true, configurable: true, writable: true, value }) : obj[key] = value;
var __spreadValues = (a, b) => {
  for (var prop in b || (b = {}))
    if (__hasOwnProp.call(b, prop))
      __defNormalProp(a, prop, b[prop]);
  if (__getOwnPropSymbols)
    for (var prop of __getOwnPropSymbols(b)) {
      if (__propIsEnum.call(b, prop))
        __defNormalProp(a, prop, b[prop]);
    }
  return a;
};
var __spreadProps = (a, b) => __defProps(a, __getOwnPropDescs(b));
var __export = (target, all) => {
  for (var name in all)
    __defProp(target, name, { get: all[name], enumerable: true });
};
var __copyProps = (to, from, except, desc) => {
  if (from && typeof from === "object" || typeof from === "function") {
    for (let key of __getOwnPropNames(from))
      if (!__hasOwnProp.call(to, key) && key !== except)
        __defProp(to, key, { get: () => from[key], enumerable: !(desc = __getOwnPropDesc(from, key)) || desc.enumerable });
  }
  return to;
};
var __toCommonJS = (mod) => __copyProps(__defProp({}, "__esModule", { value: true }), mod);

// shared/index.js
var index_exports = {};
__export(index_exports, {
  ApiError: () => ApiError,
  CAP_NAMES: () => CAP_NAMES,
  INFLIGHT: () => INFLIGHT,
  JOB_ST: () => JOB_ST,
  LEDGER_KIND: () => LEDGER_KIND,
  QUALITY: () => QUALITY,
  VM_ST: () => VM_ST,
  createApi: () => createApi,
  ctxLabel: () => ctxLabel,
  f1: () => f1,
  f2: () => f2,
  fileSize: () => fileSize,
  hms: () => hms,
  memGB: () => memGB,
  points: () => points,
  portsLabel: () => portsLabel,
  shortId: () => shortId,
  toneOf: () => toneOf,
  ts: () => ts
});
module.exports = __toCommonJS(index_exports);

// shared/api.js
var ApiError = class extends Error {
  constructor(message, status) {
    super(message || "\u8BF7\u6C42\u5931\u8D25");
    this.name = "ApiError";
    this.status = status || 0;
  }
};
var enc = encodeURIComponent;
var trim = (b) => String(b || "").replace(/\/+$/, "");
var qs = (obj) => Object.entries(obj).filter(([, v]) => v !== void 0 && v !== null && v !== "").map(([k, v]) => `${enc(k)}=${enc(v)}`).join("&");
function createApi({ baseUrl, getToken, request, upload }) {
  const root = () => trim(typeof baseUrl === "function" ? baseUrl() : baseUrl);
  const url = (path) => root() + path;
  const auth = () => {
    const t = getToken ? getToken() : "";
    return t ? { Authorization: "Bearer " + t } : {};
  };
  const parse = (res) => {
    let data = null;
    const text = res && typeof res.text === "string" ? res.text : "";
    if (text) {
      try {
        data = JSON.parse(text);
      } catch (e) {
      }
    }
    if (!res || res.status < 200 || res.status >= 300) {
      const msg = data && data.error || text || "HTTP " + (res && res.status || 0);
      throw new ApiError(msg, res ? res.status : 0);
    }
    return data;
  };
  const call = (method, path, body) => request({
    url: url(path),
    method,
    headers: body === void 0 ? auth() : __spreadProps(__spreadValues({}, auth()), { "Content-Type": "application/json" }),
    body: body === void 0 ? void 0 : JSON.stringify(body)
  }).then(parse);
  const get = (p) => call("GET", p);
  const post = (p, b) => call("POST", p, b === void 0 ? {} : b);
  const put = (p, b) => call("PUT", p, b === void 0 ? {} : b);
  const del = (p) => call("DELETE", p, void 0);
  const uploadTo = (p, form) => upload({ url: url(p), method: "POST", headers: auth(), form }).then(parse);
  return {
    baseUrl: root,
    // auth
    login: (name, password) => post("/api/auth/login", { name, password }),
    register: (name, password, invite) => post("/api/auth/register", { name, password, invite }),
    logout: () => post("/api/auth/logout", {}),
    me: () => get("/api/me"),
    state: () => get("/api/state"),
    // jobs
    jobs: () => get("/api/jobs"),
    estimate: ({ duration, kind, vcodec, req }) => get("/api/estimate?" + qs({ duration, kind, vcodec, req: JSON.stringify(req || {}) })),
    submitTranscode: (form) => uploadTo("/api/jobs", form),
    submitRender: (form) => uploadTo("/api/jobs/render", form),
    submitBuild: (form) => uploadTo("/api/jobs/build", form),
    cancelJob: (id) => post("/api/jobs/" + enc(id) + "/cancel", {}),
    jobLogs: (id) => get("/api/jobs/" + enc(id) + "/logs"),
    nodeLogs: (id) => get("/api/nodes/" + enc(id) + "/logs"),
    // billing
    ledger: () => get("/api/ledger"),
    recharge: (amount) => post("/api/me/recharge", { amount }),
    settings: () => get("/api/me/settings"),
    saveSettings: (advanced) => put("/api/me/settings", { advanced }),
    // model gateway
    keys: () => get("/api/keys"),
    createKey: (name) => post("/api/keys", { name }),
    revokeKey: (id) => del("/api/keys/" + enc(id)),
    catalog: () => get("/api/models/catalog"),
    upstreams: () => get("/api/models/upstreams"),
    addUpstream: (body) => post("/api/models/upstreams", body),
    probeUpstream: (base_url, api_key) => post("/api/models/upstreams/probe", { base_url, api_key }),
    setUpstream: (id, enabled) => put("/api/models/upstreams/" + enc(id), { enabled }),
    delUpstream: (id) => del("/api/models/upstreams/" + enc(id)),
    // compute market and provider nodes
    market: () => get("/api/market"),
    launchNode: (body) => post("/api/nodes/launch", body),
    pending: () => get("/api/pending"),
    pair: (code) => post("/api/pair", { code }),
    setNodePrice: (id, price) => put("/api/nodes/" + enc(id), { price }),
    pauseNode: (id, paused) => put("/api/nodes/" + enc(id) + "/pause", { paused }),
    // IPv6 cloud hosts
    vmOffers: () => get("/api/vm/offers"),
    vmInstances: () => get("/api/vm/instances"),
    vmOrder: (body) => post("/api/vm/orders", body),
    vmPublish: (body) => post("/api/vm/offers", body),
    vmSetOffer: (id, enabled) => put("/api/vm/offers/" + enc(id), { enabled }),
    vmDelOffer: (id) => del("/api/vm/offers/" + enc(id)),
    vmStop: (id) => post("/api/vm/instances/" + enc(id) + "/stop", {}),
    vmExtend: (id, hours) => post("/api/vm/instances/" + enc(id) + "/extend", { hours }),
    // community board
    forumPosts: () => get("/api/forum/posts"),
    forumPost: (id) => get("/api/forum/posts/" + enc(id)),
    createPost: (form) => uploadTo("/api/forum/posts", form),
    comment: (id, form) => uploadTo("/api/forum/posts/" + enc(id) + "/comments", form),
    mediaUrl: (id) => url("/api/forum/media/" + enc(id)),
    downloadUrl: (jobId, which) => url("/download/" + enc(jobId) + (which ? "?which=" + enc(which) : "")),
    launchScriptUrl: (path) => url(path)
  };
}

// shared/constants.js
var JOB_ST = {
  pending: "\u5F85\u8C03\u5EA6",
  leased: "\u5DF2\u6D3E\u53D1",
  fetching: "\u53D6\u6E90\u4E2D",
  running: "\u8F6C\u7801\u4E2D",
  uploading: "\u56DE\u4F20\u4E2D",
  merging: "\u5408\u6210\u4E2D",
  queued: "\u6392\u961F\u4E2D",
  done: "\u5B8C\u6210",
  failed: "\u5931\u8D25"
};
var VM_ST = {
  queued: "\u6392\u961F\u4E2D",
  provisioning: "\u542F\u52A8\u4E2D",
  running: "\u8FD0\u884C\u4E2D",
  unreachable: "\u5931\u8054",
  stopping: "\u505C\u673A\u4E2D",
  stopped: "\u5DF2\u505C\u673A",
  failed: "\u5931\u8D25"
};
var LEDGER_KIND = {
  hold: "\u9884\u6263",
  settle: "\u7ED3\u7B97",
  refund: "\u9000\u8FD8",
  earn: "\u6536\u76CA",
  recharge: "\u5145\u503C"
};
var CAP_NAMES = {
  tools: "\u5DE5\u5177\u8C03\u7528",
  json_object: "JSON \u8F93\u51FA",
  json_schema: "JSON Schema",
  regex: "\u6B63\u5219\u7EA6\u675F",
  vision: "\u56FE\u50CF\u8F93\u5165",
  reasoning: "\u63A8\u7406",
  deterministic: "\u786E\u5B9A\u6027",
  prefix_cache: "\u524D\u7F00\u7F13\u5B58"
};
var QUALITY = {
  fast: { vcodec: "libx264", preset: "ultrafast", q: "28" },
  balanced: { vcodec: "libx264", preset: "fast", q: "26" },
  high: { vcodec: "libx264", preset: "slow", q: "20" }
};
var INFLIGHT = ["fetching", "leased", "running", "uploading"];
var toneOf = (status) => status === "done" ? "ok" : status === "failed" ? "bad" : INFLIGHT.includes(status) || status === "running" ? "link" : status === "queued" || status === "pending" || status === "provisioning" ? "vio" : "mute";

// shared/format.js
var f1 = (v) => Number(v || 0).toFixed(1);
var f2 = (v) => Number(v || 0).toFixed(2);
var points = (v) => f2(v) + " \u70B9";
var ts = (sec) => sec ? new Date(sec * 1e3).toLocaleString("zh-CN", { hour12: false }) : "\u2014";
var hms = (sec) => {
  const s = Math.max(0, Math.floor(sec || 0));
  const p = (n) => String(n).padStart(2, "0");
  return `${p(Math.floor(s / 3600))}:${p(Math.floor(s % 3600 / 60))}:${p(s % 60)}`;
};
var ctxLabel = (n) => {
  if (!n) return "32K";
  if (n >= 1048576) return (n / 1048576).toFixed(n % 1048576 ? 2 : 0) + "M";
  if (n >= 1024) return Math.round(n / 1024) + "K";
  return String(n);
};
var memGB = (mb) => mb ? Math.round(mb / 1024 * 10) / 10 + "G" : "\u2014";
function fileSize(bytes) {
  const n = Number(bytes || 0);
  if (n >= 1073741824) return (n / 1073741824).toFixed(2) + " GB";
  if (n >= 1048576) return (n / 1048576).toFixed(1) + " MB";
  if (n >= 1024) return Math.round(n / 1024) + " KB";
  return n + " B";
}
function portsLabel(ports) {
  const list = (ports || []).map((p) => `${p.host_port || "\u81EA\u52A8"}:${p.guest_port}${p.proto === "udp" ? "/udp" : ""}`);
  return list.join(" \xB7 ") || "\u4EC5 SSH";
}
var shortId = (id, n = 10) => String(id || "").slice(0, n);
