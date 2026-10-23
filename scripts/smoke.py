#!/usr/bin/env python3
# 端到端验收：注册/登录 -> 认领节点 -> 报价 -> 提交任务 -> 结算 -> 账单
# 用 Python 而不是 curl：Windows 的 curl 会按本地代码页发送中文请求体。
import json, re, sys, time, urllib.parse, urllib.request, http.cookiejar

PORT = int(sys.argv[1]) if len(sys.argv) > 1 else 8799
API = "http://127.0.0.1:%d" % PORT
NAME = "演示用户"
PWD = "starstack123"
# 重定向到文件时 Windows 的 stdout 会退回 GBK，中文变成乱码
sys.stdout.reconfigure(encoding="utf-8")

jar = http.cookiejar.CookieJar()
op = urllib.request.build_opener(urllib.request.HTTPCookieProcessor(jar))


def call(path, body=None, method=None, raw=None, headers=None):
    data = raw if raw is not None else (json.dumps(body).encode() if body is not None else None)
    h = {"Content-Type": "application/json"}
    h.update(headers or {})
    req = urllib.request.Request(API + path, data=data, headers=h, method=method)
    try:
        with op.open(req, timeout=180) as r:
            t = r.read().decode()
    except urllib.error.HTTPError as e:
        return e.code, json.loads(e.read().decode() or "{}")
    return 200, json.loads(t) if t.startswith(("{", "[")) else t


print("[1] 注册 / 登录")
code, me = call("/api/auth/register", {"name": NAME, "password": PWD})
if code != 200:
    print("   注册不可用（%s），改为登录既有账户" % me.get("error", code))
    code, me = call("/api/auth/login", {"name": NAME, "password": PWD})
assert code == 200, me
print("   %s id=%s 余额=%s" % (me["name"], me["id"], me["credits"]))

print("[2] 用配对码认领 node-a")
code_txt = open("smoke/node-a.log", encoding="utf-8", errors="ignore").read()
m = re.findall(r"配对码 ([A-Z0-9]{6})", code_txt)
assert m, "agent 日志里没有配对码"
code, r = call("/api/pair", {"code": m[-1]})
print("   %s -> %s" % (m[-1], r))
assert code == 200, r

print("[3] 未登录的请求必须被拒绝")
anon = urllib.request.build_opener()
try:
    anon.open(API + "/api/jobs", timeout=10)
    print("   !! 匿名可以读到任务列表")
except urllib.error.HTTPError as e:
    print("   匿名 GET /api/jobs -> %d %s" % (e.code, e.read().decode("utf-8")[:40]))

print("[4] 报价（60s libx264，要求 >=8 核、有 GPU、单价<=1）")
q = urllib.parse.quote(json.dumps({"need_gpu": True, "min_cpus": 8, "max_price": 1.0}, separators=(",", ":")))
code, e = call("/api/estimate?duration=60&vcodec=libx264&req=" + q)
print("   可派发节点 %d 台，预扣 %.2f 点，最省 %.2f 点，余额 %.2f，够不够=%s"
      % (e["eligible"], e["hold"], e["cheapest"], e["balance"], e["affordable"]))
for n in e["nodes"]:
    print("      %s price=%.2f speed=%.2f 空闲=%d 我的机器=%s" % (n["name"], n["price"], n["speed"], n["free_slots"], n["mine"]))

print("[5] 不可能的硬约束应当报价为 0 台可派发")
q2 = urllib.parse.quote(json.dumps({"min_cpus": 9999}, separators=(",", ":")))
_, e2 = call("/api/estimate?duration=60&vcodec=libx264&req=" + q2)
print("   需要 9999 核 -> 可派发 %d 台" % e2["eligible"])

print("[6] 提交任务")
mp = (
    b"--X\r\nContent-Disposition: form-data; name=\"path\"\r\n\r\n./samples/input.mp4\r\n"
    b"--X\r\nContent-Disposition: form-data; name=\"vcodec\"\r\n\r\nlibx264\r\n"
    b"--X\r\nContent-Disposition: form-data; name=\"preset\"\r\n\r\nveryfast\r\n"
    b"--X\r\nContent-Disposition: form-data; name=\"q\"\r\n\r\n28\r\n"
    b"--X\r\nContent-Disposition: form-data; name=\"height\"\r\n\r\n720\r\n"
    b"--X\r\nContent-Disposition: form-data; name=\"segs\"\r\n\r\n4\r\n"
    b"--X\r\nContent-Disposition: form-data; name=\"audio\"\r\n\r\n1\r\n"
    b"--X\r\nContent-Disposition: form-data; name=\"baseline\"\r\n\r\n1\r\n"
    b"--X\r\nContent-Disposition: form-data; name=\"req\"\r\n\r\n"
    + json.dumps({"need_gpu": True, "min_cpus": 8, "max_price": 1.0}, separators=(",", ":")).encode()
    + b"\r\n--X--\r\n"
)
code, j = call("/api/jobs", raw=mp, headers={"Content-Type": "multipart/form-data; boundary=X"})
assert code == 200, j
job = j["id"]
print("   job=%s 分片=%d 预扣=%.2f 点" % (job, j["pieces"], j["hold"]))
print("   note: %s" % j["note"])

print("[7] 等待完成（同时观察基线）")
last = None
for i in range(150):
    _, d = call("/api/jobs")
    mine = [x for x in d["jobs"] if x["id"] == job]
    if not mine:
        print("   !! 任务列表里没有这个 job"); break
    j0 = mine[0]
    st = "%s %d/%d hold=%.2f spent=%.2f" % (j0["status"],
        sum(1 for s in j0["segs"] if s["status"] == "done"), len(j0["segs"]), j0["credits_hold"], j0["credits_spent"])
    if st != last:
        print("   %3ds %s" % (i * 2, st)); last = st
    if j0["status"] in ("done", "failed"):
        break
    time.sleep(2)

print("[8] 分片结算明细")
_, d = call("/api/jobs")
j0 = [x for x in d["jobs"] if x["id"] == job][0]
print("   status=%s wall=%.1fs sum=%.1fs 加速比=%.2fx 实结=%.2f 点"
      % (j0["status"], j0["wall"], j0["sum_wall"], (j0["sum_wall"] / j0["wall"]) if j0["wall"] else 0, j0["credits_spent"]))
for s in j0["segs"]:
    print("      #%d %-9s %-8s dur=%.1fs cost=%.3f verified=%s attempt=%d"
          % (s["index"], s["status"], s["worker"], s["dur"], s["cost"], s["match"], s["attempt"]))
print("   baseline: %s" % j0["baseline"])

print("[9] 账单流水")
_, led = call("/api/ledger?limit=20")
print("   余额=%.2f 我的节点=%d" % (led["balance"], led["nodes"]))
for x in led["spend"]:
    print("      支出 %-8s %+8.2f -> %8.2f  %s" % (x["kind"], x["amount"], x["balance"], (x["note"] or "")[:36]))
for x in led["earn"]:
    print("      收益 %-8s %+8.2f  node=%s %s" % (x["kind"], x["amount"], x["node_id"][:12], (x["note"] or "")[:28]))

print("[10] 节点归属与隐私")
_, st = call("/api/state")
for w in st["workers"]:
    print("   node=%s mine=%s price=%.2f earned=%s billed=%s online=%s"
          % (w["id"], w["mine"], w["price"], w.get("earned"), w.get("seconds_billed"), w["online"]))
leaky = [w["id"] for w in st["workers"] if not w["mine"] and ("hostname" in w or "address" in w)]
print("   非本人节点泄露主机名/地址: %s" % (leaky or "无"))
_, mk = call("/api/market")
for s in mk["slices"][:6]:
    print("   能力 %-12s 节点=%d 空闲槽=%d 价格 %.2f~%.2f 均速 %.2fx 最强卡=%s"
          % (s["encoder"], s["nodes"], s["free_slots"], s["min_price"], s["max_price"], s["avg_speed"], s["best_gpu"]))

print("[11] 改价 + 未授权改价应当被拒绝")
_, r = call("/api/nodes/node-a", {"price": 0.5}, method="PUT")
print("   本人改价 -> %s" % r)
other = urllib.request.build_opener()
try:
    req = urllib.request.Request(API + "/api/nodes/node-a", data=b'{"price":9}',
                                 headers={"Content-Type": "application/json"}, method="PUT")
    other.open(req, timeout=10)
    print("   !! 未登录也能改价")
except urllib.error.HTTPError as e:
    print("   匿名改价 -> %d" % e.code)
